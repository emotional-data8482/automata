package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/emotional-data8482/automata/core"
)

// crashAfterWriteEnv makes the publish tool exit the process right after its
// external write, before Runtime can commit the outcome. The demo sets it to
// exercise the dispatch window.
const crashAfterWriteEnv = "DURABLE_HOST_CRASH_AFTER_WRITE"

// PublishInput is the publish tool's arguments.
type PublishInput struct {
	Path string `json:"path" desc:"artifact file name"`
	Body string `json:"body" desc:"artifact content"`
}

// ReviewRequest is the reviewer child's input.
type ReviewRequest struct {
	Artifact string `json:"artifact" desc:"artifact file name to review"`
}

// ReviewVerdict is the reviewer child's validated output.
type ReviewVerdict struct {
	Verdict string `json:"verdict" desc:"approve or reject"`
	Notes   string `json:"notes"`
}

// ledgerEntry is one line of the external write ledger: the destination's own
// record of what it applied, keyed by the idempotency key the tool sent.
type ledgerEntry struct {
	IdempotencyKey string `json:"idempotency_key"`
	Path           string `json:"path"`
}

// registerDefinitions registers the same immutable revisions in every
// process. A store's runs pin these references, so a process that changed an
// agent would register a new revision instead.
func registerDefinitions(runtime *core.Runtime, dir string) (core.DefinitionRef, error) {
	reviewer, err := core.New(reviewerModel{}, core.AgentConfig{
		SystemPrompt: "Review the named artifact and answer with a verdict.",
		MaxTurns:     3,
		StructuredOutput: &core.StructuredOutputConfig{
			Schema:         core.OutputSchema[ReviewVerdict](),
			MaxCorrections: 1,
		},
	})
	if err != nil {
		return core.DefinitionRef{}, err
	}
	reviewerRef, err := runtime.Register("reviewer", "v1", reviewer)
	if err != nil {
		return core.DefinitionRef{}, err
	}

	publisher, err := core.New(publisherModel{}, core.AgentConfig{
		SystemPrompt: "Publish the requested artifact, then have it reviewed.",
		MaxTurns:     6,
		ToolPolicy:   core.ToolPolicy{MaxCalls: 4},
		Tools: []core.Tool{
			publishTool(dir),
			core.ChildTool[ReviewRequest]("review", "Ask the reviewer for a verdict on one artifact.", reviewerRef),
		},
	})
	if err != nil {
		return core.DefinitionRef{}, err
	}
	return runtime.Register("publisher", "v1", publisher)
}

// publishTool writes an artifact under dir/artifacts and appends the write to
// dir/effects.log. It is mutating, approval-gated, and guarded against a
// semantic duplicate write of the same path.
func publishTool(dir string) core.Tool {
	publish := core.FuncResult("publish_artifact", "Publish an artifact file.",
		func(ctx context.Context, in PublishInput) (core.ToolResult, error) {
			op, ok := core.ToolOperationFromContext(ctx)
			if !ok {
				return core.ToolResult{}, errors.New("publish requires a durable tool operation")
			}
			if err := publishArtifact(dir, op.IdempotencyKey, in); err != nil {
				// The destination may have applied part of the write.
				result := core.ErrorResult("publish failed: " + err.Error())
				result.Effect = core.EffectReport{Status: core.EffectUnknown}
				return result, nil
			}
			if os.Getenv(crashAfterWriteEnv) == "1" {
				// Simulate the process dying after the external write but
				// before Runtime commits the outcome.
				os.Exit(3)
			}
			result := core.TextResult("published " + in.Path)
			result.Effect = core.EffectReport{Status: core.EffectApplied, Receipt: in.Path}
			return result, nil
		})
	publish = core.WithToolEffectPolicy(publish, core.ToolEffectPolicy{
		Kind:  core.ToolEffectMutating,
		Scope: "artifacts",
		SemanticKey: func(raw json.RawMessage) (string, error) {
			var in PublishInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			return in.Path, nil
		},
	})
	return core.WithDurableWait(publish, core.DurableWaitPolicy{
		Kind:          core.WaitApproval,
		PolicyContext: "artifacts-v1",
		Prompt: func(raw json.RawMessage) (string, error) {
			var in PublishInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			return fmt.Sprintf("Publish %s (%d bytes)?", in.Path, len(in.Body)), nil
		},
		Target: func(raw json.RawMessage) (string, error) {
			var in PublishInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			return in.Path, nil
		},
	})
}

// publishArtifact is the external destination: it applies a write once per
// idempotency key and records it in the ledger.
func publishArtifact(dir, key string, in PublishInput) error {
	if applied, err := ledgerHas(dir, key); err != nil || applied {
		return err
	}
	artifacts := filepath.Join(dir, "artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(artifacts, filepath.Base(in.Path)), []byte(in.Body), 0o644); err != nil {
		return err
	}
	line, _ := json.Marshal(ledgerEntry{IdempotencyKey: key, Path: in.Path})
	f, err := os.OpenFile(filepath.Join(dir, "effects.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// readLedger returns every write the destination applied.
func readLedger(dir string) ([]ledgerEntry, error) {
	f, err := os.Open(filepath.Join(dir, "effects.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []ledgerEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry ledgerEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}

func ledgerHas(dir, key string) (bool, error) {
	entries, err := readLedger(dir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.IdempotencyKey == key {
			return true, nil
		}
	}
	return false, nil
}

// publisherModel is a deterministic stand-in for a model. It decides from the
// committed transcript alone, so a restarted process continues identically:
// publish, then request a review, then summarize.
type publisherModel struct{}

func (publisherModel) Invoke(_ context.Context, req core.Request) (core.Response, error) {
	task, results := "", map[string]core.ToolResultBlock{}
	for _, message := range req.Messages {
		if message.Role == "user" && task == "" {
			task = message.Text()
		}
		for _, block := range message.Blocks {
			if result, ok := block.(core.ToolResultBlock); ok {
				results[result.ToolUseID] = result
			}
		}
	}
	path := slug(task) + ".md"
	published, publishedDone := results["publish-1"]
	reviewed, reviewedDone := results["review-1"]
	switch {
	case !publishedDone:
		input, _ := json.Marshal(PublishInput{Path: path, Body: "# " + task + "\n"})
		return toolCall("publish-1", "publish_artifact", input), nil
	case published.IsError:
		return answer("Not published: " + resultText(published)), nil
	case !reviewedDone:
		input, _ := json.Marshal(ReviewRequest{Artifact: path})
		return toolCall("review-1", "review", input), nil
	default:
		return answer(fmt.Sprintf("Published %s. Review: %s", path, resultText(reviewed))), nil
	}
}

// reviewerModel answers every review with a JSON verdict in plain text; the
// declared output contract extracts and validates it.
type reviewerModel struct{}

func (reviewerModel) Invoke(_ context.Context, req core.Request) (core.Response, error) {
	var in ReviewRequest
	for _, message := range req.Messages {
		if message.Role == "user" {
			_ = json.Unmarshal([]byte(message.Text()), &in)
		}
	}
	verdict, _ := json.Marshal(ReviewVerdict{Verdict: "approve", Notes: "checked " + in.Artifact})
	return answer(string(verdict)), nil
}

func toolCall(id, name string, input json.RawMessage) core.Response {
	return core.Response{
		Message:    core.AssistantMessage(core.ToolUseBlock{ID: id, Name: name, Input: input}),
		StopReason: core.StopToolUse,
	}
}

func answer(text string) core.Response {
	return core.Response{Message: core.AssistantMessage(core.TextBlock{Text: text}), StopReason: core.StopEndTurn}
}

func resultText(result core.ToolResultBlock) string {
	return core.Message{Blocks: result.Content}.Text()
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "artifact"
	}
	return s
}
