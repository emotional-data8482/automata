// Command durable_typed demonstrates validated domain output through Runtime
// with a temporary SQLite store and credential-free fake providers.
//
// First, the publisher performs a write, then returns an invalid structured
// payload. Runtime corrects the payload in the same run without re-dispatching
// the already accepted write.
//
// Second, a lead agent delegates to a reviewer through a durable child tool.
// The reviewer's typed verdict is kept on its own child run, and the lead
// continues the same conversation in a second, serialized turn.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/sqlite"
)

type PublishInput struct {
	Path string `json:"path" desc:"artifact path"`
	Body string `json:"body" desc:"artifact body"`
}

type PublishBrief struct {
	Title       string `json:"title"`
	Accepted    bool   `json:"accepted"`
	Receipt     string `json:"receipt"`
	Corrections int    `json:"corrections"`
}

type ReviewVerdict struct {
	Verdict string `json:"verdict"`
	Notes   string `json:"notes"`
}

type fakeProvider struct {
	turns []core.Message
	calls int
}

func (p *fakeProvider) Invoke(_ context.Context, req core.Request) (core.Response, error) {
	if p.calls >= len(p.turns) {
		return core.Response{}, fmt.Errorf("fake provider exhausted after %d calls", p.calls)
	}
	message := p.turns[p.calls]
	p.calls++

	reason := core.StopEndTurn
	if len(message.ToolUses()) > 0 {
		reason = core.StopToolUse
	}
	return core.Response{Message: message, StopReason: reason}, nil
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir, err := os.MkdirTemp("", "automata-durable-typed-*")
	if err != nil {
		exitf("create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	store, err := sqlite.Open(ctx, filepath.Join(dir, "runtime.db"))
	if err != nil {
		exitf("open sqlite store: %v", err)
	}
	runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
	if err != nil {
		exitf("construct runtime: %v", err)
	}
	defer runtime.Close()

	publishWithCorrection(ctx, runtime)
	reviewThread(ctx, runtime)
}

// publishWithCorrection shows one accepted write surviving an in-run
// structured-output correction.
func publishWithCorrection(ctx context.Context, runtime *core.Runtime) {
	var writes atomic.Int64
	publish := core.FuncResult("publish_report", "Publish a report artifact", func(_ context.Context, in PublishInput) (core.ToolResult, error) {
		count := writes.Add(1)
		receipt := fmt.Sprintf("%s#%d", in.Path, count)
		result := core.TextResult("published " + receipt)
		result.Effect = core.EffectReport{Status: core.EffectApplied, Receipt: receipt}
		return result, nil
	})
	publish = core.WithToolEffectPolicy(publish, core.ToolEffectPolicy{
		Kind:  core.ToolEffectMutating,
		Scope: "durable-typed-example",
		SemanticKey: func(raw json.RawMessage) (string, error) {
			var in PublishInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			return in.Path, nil
		},
	})

	provider := &fakeProvider{turns: []core.Message{
		core.AssistantMessage(core.ToolUseBlock{ID: "write-1", Name: "publish_report", Input: json.RawMessage(`{"path":"report.md","body":"ready"}`)}),
		// Invalid on purpose: title must be a string. Runtime records the
		// violation and asks for a correction without replaying publish_report.
		core.AssistantMessage(core.ToolUseBlock{ID: "typed-1", Name: "automata_structured_output", Input: json.RawMessage(`{"title":42,"accepted":true,"receipt":"report.md#1","corrections":0}`)}),
		core.AssistantMessage(core.ToolUseBlock{ID: "typed-2", Name: "automata_structured_output", Input: json.RawMessage(`{"title":"Quarterly report published","accepted":true,"receipt":"report.md#1","corrections":1}`)}),
	}}

	agent, err := core.New(provider, core.AgentConfig{
		Tools:    []core.Tool{publish},
		MaxTurns: 6,
		StructuredOutput: &core.StructuredOutputConfig{
			Schema: json.RawMessage(`{
				"type":"object",
				"properties":{
					"title":{"type":"string"},
					"accepted":{"type":"boolean"},
					"receipt":{"type":"string"},
					"corrections":{"type":"integer","minimum":0}
				},
				"required":["title","accepted","receipt","corrections"],
				"additionalProperties":false
			}`),
			MaxCorrections: 1,
		},
	})
	if err != nil {
		exitf("construct agent: %v", err)
	}

	if err := runtime.Register("publisher", "demo-v1", agent); err != nil {
		exitf("register agent: %v", err)
	}
	handle, err := runtime.Submit(ctx, "publisher", "demo-v1", "publish the report and return the domain brief", core.SubmitOptions{
		Scope: "demo", Key: "report-001",
	})
	if err != nil {
		exitf("submit run: %v", err)
	}

	result, err := handle.Await(ctx)
	if err != nil {
		exitf("run failed after %d turns with partial structured output %q: %v", result.Turns, result.StructuredOutput, err)
	}
	var brief PublishBrief
	if err := json.Unmarshal(result.StructuredOutput, &brief); err != nil {
		exitf("decode structured output: %v", err)
	}
	if writes.Load() != 1 {
		exitf("expected one write, saw %d", writes.Load())
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		exitf("snapshot run: %v", err)
	}
	effectReceipt := ""
	for _, batch := range snapshot.ToolBatches {
		for _, invocation := range batch.Invocations {
			if invocation.Call.Name == "publish_report" && invocation.Effect.Status == core.EffectApplied {
				effectReceipt = invocation.Effect.Receipt
			}
		}
	}

	fmt.Printf("domain output: %+v\n", brief)
	fmt.Printf("provider turns: %d; external writes: %d\n", provider.calls, writes.Load())
	fmt.Printf("durable effect receipt: %s\n", effectReceipt)
	fmt.Println("correction reused the accepted write receipt; publish_report was not dispatched again")
}

// reviewThread delegates to a reviewer as a durable child run, then continues
// the lead's conversation in a second turn from its committed history.
func reviewThread(ctx context.Context, runtime *core.Runtime) {
	reviewer, err := core.New(&fakeProvider{turns: []core.Message{
		withUsage(core.AssistantMessage(core.ToolUseBlock{ID: "verdict-1", Name: "automata_structured_output", Input: json.RawMessage(`{"verdict":"approve","notes":"receipt report.md#1 matches the brief"}`)}), 40),
	}}, core.AgentConfig{
		MaxTurns: 3,
		StructuredOutput: &core.StructuredOutputConfig{
			Schema: json.RawMessage(`{
				"type":"object",
				"properties":{"verdict":{"type":"string","enum":["approve","reject"]},"notes":{"type":"string"}},
				"required":["verdict","notes"],
				"additionalProperties":false
			}`),
		},
	})
	if err != nil {
		exitf("construct reviewer: %v", err)
	}
	if err := runtime.Register("reviewer", "demo-v1", reviewer); err != nil {
		exitf("register reviewer: %v", err)
	}

	// The child tool pins the reviewer's registered definition and revision;
	// each call is admitted as its own durable run linked to the lead's call.
	review := core.DurableChildTool(core.ToolDefinition{
		Name:        "review",
		Description: "Ask the reviewer for a verdict on one artifact.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"artifact":{"type":"string"}},"required":["artifact"]}`),
	}, core.DurableChildPolicy{DefinitionID: "reviewer", Revision: "demo-v1"})
	lead, err := core.New(&fakeProvider{turns: []core.Message{
		withUsage(core.AssistantMessage(core.ToolUseBlock{ID: "review-1", Name: "review", Input: json.RawMessage(`{"artifact":"report.md"}`)}), 30),
		withUsage(core.AssistantMessage(core.TextBlock{Text: "The reviewer approved report.md."}), 60),
		withUsage(core.AssistantMessage(core.TextBlock{Text: "Team update: report.md is published and approved."}), 80),
	}}, core.AgentConfig{
		Tools:      []core.Tool{review},
		MaxTurns:   4,
		ToolPolicy: core.ToolPolicy{MaxCalls: 4},
	})
	if err != nil {
		exitf("construct lead: %v", err)
	}
	if err := runtime.Register("lead", "demo-v1", lead); err != nil {
		exitf("register lead: %v", err)
	}

	thread := core.ConversationOptions{Scope: "demo", ID: "report-review"}
	first, err := runtime.Run(ctx, "lead", "demo-v1", "Get report.md reviewed.", core.SubmitOptions{Conversation: thread})
	if err != nil {
		exitf("first turn failed after %d turns with output %q: %v", first.Turns, first.Output, err)
	}
	// The second turn names the head it continues; a stale or competing turn
	// would be rejected instead of merged.
	thread.ExpectedHead = first.RunID
	second, err := runtime.Run(ctx, "lead", "demo-v1", "Summarize that for the team.", core.SubmitOptions{Conversation: thread})
	if err != nil {
		exitf("second turn failed after %d turns with output %q: %v", second.Turns, second.Output, err)
	}

	snapshot, err := runtime.Handle(first.RunID).Snapshot(ctx)
	if err != nil {
		exitf("snapshot first turn: %v", err)
	}
	childID := snapshot.ToolBatches[0].Invocations[0].ChildRunID
	child, err := runtime.Handle(childID).Snapshot(ctx)
	if err != nil {
		exitf("snapshot child run: %v", err)
	}
	var verdict ReviewVerdict
	if err := json.Unmarshal(child.Result.StructuredOutput, &verdict); err != nil {
		exitf("decode child verdict: %v", err)
	}
	conversation, err := runtime.Conversation(ctx, thread.Scope, thread.ID)
	if err != nil {
		exitf("inspect conversation: %v", err)
	}

	fmt.Printf("child verdict: %+v (child run linked to parent: %t)\n", verdict, child.ParentRunID == first.RunID)
	fmt.Printf("turn 1 input tokens: %d local, %d across %d runs\n", first.Usage.InputTokens, snapshot.Tree.Usage.InputTokens, snapshot.Tree.Runs)
	fmt.Printf("turn 2 continued %d committed messages: %s\n", len(second.Messages)-2, second.Output)
	fmt.Printf("conversation turns: %d; head is turn 2: %t\n", conversation.Turns, conversation.Head == second.RunID)
}

func withUsage(message core.Message, inputTokens int) core.Message {
	message.Usage = &core.Usage{InputTokens: inputTokens}
	return message
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
