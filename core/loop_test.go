package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestUnknownToolIsRecoverable pins that a hallucinated tool name does not
// abort the run: the model gets a "tool not found" result naming the available
// tools, sibling calls in the same batch still execute, and the stream carries
// the failure as a StreamToolResult with ErrToolNotFound.
func TestUnknownToolIsRecoverable(t *testing.T) {
	final := "recovered"
	provider := &capturingProvider{turns: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "g1", Type: "function", Function: FunctionCall{Name: "ghost", Arguments: "{}"}},
			{ID: "e1", Type: "function", Function: FunctionCall{Name: "echo", Arguments: `{"msg":"hi"}`}},
		}},
		{Role: "assistant", Content: &final},
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
	if out != final {
		t.Errorf("output = %q, want %q", out, final)
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
		if m.Role == "tool" && m.Content != nil {
			results[m.ToolCallID] = *m.Content
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
