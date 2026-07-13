package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// retryableToolErr is a tool error that reports itself retryable, so a test can
// prove the loop does NOT retry tool execution on its own.
type retryableToolErr struct{ msg string }

func (e *retryableToolErr) Error() string   { return e.msg }
func (e *retryableToolErr) Retryable() bool { return true }

// countingTool records how many times Execute is called and always fails with a
// retryable error.
type countingTool struct{ calls atomic.Int32 }

func (t *countingTool) Name() string            { return "counter" }
func (t *countingTool) Schema() json.RawMessage { return json.RawMessage(`{"name":"counter"}`) }
func (t *countingTool) Execute(context.Context, string) (string, error) {
	t.calls.Add(1)
	return "", &retryableToolErr{msg: "transient"}
}

// TestToolExecuteNotRetriedByLoop is the regression test for the double-retry
// bug: even when a tool returns a retryable error, the loop must call Execute
// exactly once. Retrying there would replay a whole sub-agent run, re-emitting
// its stream events and double-counting usage. The error is still fed back to
// the model as a recoverable tool result.
func TestToolExecuteNotRetriedByLoop(t *testing.T) {
	final := "recovered"
	provider := &capturingProvider{turns: []Message{
		asstTool("c1", "counter", "{}"),
		asstText(final),
	}}

	tool := &countingTool{}
	agent := New(provider)
	agent.RegisterTool(tool)

	out, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Output != final {
		t.Errorf("output = %q, want %q", out.Output, final)
	}
	if got := tool.calls.Load(); got != 1 {
		t.Errorf("Execute called %d times, want 1 (loop must not retry tools)", got)
	}
}

// TestUnknownToolIsRecoverable pins that a hallucinated tool name does not
// abort the run: the model gets a "tool not found" result naming the available
// tools, sibling calls in the same batch still execute, and the stream carries
// the failure as a StreamToolResult with ErrToolNotFound.
func TestUnknownToolIsRecoverable(t *testing.T) {
	final := "recovered"
	provider := &capturingProvider{turns: []Message{
		AssistantMessage(toolUse("g1", "ghost", "{}"), toolUse("e1", "echo", `{"msg":"hi"}`)),
		asstText(final),
	}}

	agent := New(provider)
	agent.RegisterTool(Func("echo", "echoes msg", func(_ context.Context, a echoArgs) (string, error) {
		return "echoed:" + a.Msg, nil
	}))

	var ghostEvent *StreamEvent
	out, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		if ev.Kind == StreamToolResult && ev.ToolCall.ID == "g1" {
			e := ev
			ghostEvent = &e
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out.Output != final {
		t.Errorf("output = %q, want %q", out.Output, final)
	}

	if ghostEvent == nil {
		t.Fatal("no StreamToolResult event for the unknown tool")
	}
	if !errors.Is(ghostEvent.Err, ErrToolNotFound) {
		t.Errorf("event Err = %v, want ErrToolNotFound", ghostEvent.Err)
	}

	// The model should see both results on the next turn: the not-found error
	// (naming the real tools) and the sibling call's normal output.
	if len(provider.received) < 2 {
		t.Fatalf("provider saw %d turns, want 2", len(provider.received))
	}
	results := map[string]string{}
	for _, m := range provider.received[1] {
		if m.Role != "tool" {
			continue
		}
		for _, blk := range m.Blocks {
			if tr, ok := blk.(ToolResultBlock); ok {
				results[tr.ToolUseID] = Message{Blocks: tr.Content}.Text()
			}
		}
	}
	ghost := results["g1"]
	if !strings.Contains(ghost, "tool not found") || !strings.Contains(ghost, `"ghost"`) {
		t.Errorf("ghost result = %q, want a tool-not-found error naming the call", ghost)
	}
	if !strings.Contains(ghost, "echo") {
		t.Errorf("ghost result = %q, want it to list the available tools", ghost)
	}
	if results["e1"] != "echoed:hi" {
		t.Errorf("sibling tool result = %q, want %q", results["e1"], "echoed:hi")
	}
}
