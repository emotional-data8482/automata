// Command durable_typed demonstrates validated domain output through Runtime
// with a temporary SQLite store and a credential-free fake provider. The
// provider first performs a write, then returns an invalid structured payload.
// Runtime corrects the payload in the same run without re-dispatching the
// already accepted write.
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

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
