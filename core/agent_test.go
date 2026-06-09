package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// capturingProvider replays scripted Messages and records the message history
// it receives on each turn, so tests can assert what task a sub-agent was
// handed.
type capturingProvider struct {
	turns    []Message
	calls    int
	received [][]Message
}

func (p *capturingProvider) Invoke(_ context.Context, messages []Message, _ []Tool) (Message, error) {
	p.received = append(p.received, append([]Message(nil), messages...))
	if p.calls >= len(p.turns) {
		return Message{}, fmt.Errorf("no script for turn %d", p.calls)
	}
	m := p.turns[p.calls]
	p.calls++
	return m, nil
}

// lastUserMessage returns the content of the most recent role:"user" message
// seen by the provider.
func (p *capturingProvider) lastUserMessage(t *testing.T) string {
	t.Helper()
	for i := len(p.received) - 1; i >= 0; i-- {
		for j := len(p.received[i]) - 1; j >= 0; j-- {
			if m := p.received[i][j]; m.Role == "user" && m.Content != nil {
				return *m.Content
			}
		}
	}
	t.Fatal("provider saw no user message")
	return ""
}

type researchArgs struct {
	Topic     string   `json:"topic"`
	Questions []string `json:"questions,omitempty"`
}

func renderResearch(p researchArgs) string {
	return fmt.Sprintf("Research: %s\nQuestions:\n- %s", p.Topic, strings.Join(p.Questions, "\n- "))
}

// TestAsToolFuncRendersTask pins feature: the sub-agent receives the rendered
// natural-language task, not the model's raw JSON arguments.
func TestAsToolFuncRendersTask(t *testing.T) {
	notes := "notes"
	subProvider := &capturingProvider{turns: []Message{{Role: "assistant", Content: &notes}}}
	sub := New(subProvider)

	final := "final"
	orch := New(&scriptedProvider{turns: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "r1", Type: "function",
			Function: FunctionCall{Name: "researcher", Arguments: `{"topic":"GLP-1","questions":["cost","supply"]}`},
		}}},
		{Role: "assistant", Content: &final},
	}})
	orch.RegisterTool(AsToolFunc[researchArgs](sub, "researcher", "delegate research", renderResearch))

	out, err := orch.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != final {
		t.Errorf("output = %q, want %q", out, final)
	}

	want := "Research: GLP-1\nQuestions:\n- cost\n- supply"
	if got := subProvider.lastUserMessage(t); got != want {
		t.Errorf("sub-agent task = %q, want %q", got, want)
	}
}

// TestAsToolFuncInvalidArgs pins the error path: arguments that don't decode
// into P are fed back to the model as a recoverable tool error, and the
// sub-agent is never invoked.
func TestAsToolFuncInvalidArgs(t *testing.T) {
	subProvider := &capturingProvider{}
	sub := New(subProvider)

	final := "recovered"
	orch := New(&scriptedProvider{turns: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "r1", Type: "function",
			Function: FunctionCall{Name: "researcher", Arguments: `{"topic":`},
		}}},
		{Role: "assistant", Content: &final},
	}})
	orch.RegisterTool(AsToolFunc[researchArgs](sub, "researcher", "delegate research", renderResearch))

	var result string
	out, err := orch.RunStream(context.Background(), "go", func(ev StreamEvent) {
		if ev.Kind == StreamToolResult {
			result = ev.Result
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out != final {
		t.Errorf("output = %q, want %q", out, final)
	}
	if !strings.HasPrefix(result, "error: invalid args") {
		t.Errorf("tool result = %q, want prefix %q", result, "error: invalid args")
	}
	if subProvider.calls != 0 {
		t.Errorf("sub-agent was invoked %d times on invalid args, want 0", subProvider.calls)
	}
}

// TestAsToolFuncEmptyArgsRenderZero pins parity with Func: "", "null" and "{}"
// render the zero value of P instead of failing.
func TestAsToolFuncEmptyArgsRenderZero(t *testing.T) {
	done := "done"
	subProvider := &capturingProvider{turns: []Message{{Role: "assistant", Content: &done}}}
	sub := New(subProvider)

	final := "final"
	orch := New(&scriptedProvider{turns: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "r1", Type: "function",
			Function: FunctionCall{Name: "researcher", Arguments: "{}"},
		}}},
		{Role: "assistant", Content: &final},
	}})
	orch.RegisterTool(AsToolFunc[researchArgs](sub, "researcher", "delegate research", renderResearch))

	if _, err := orch.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := subProvider.lastUserMessage(t), renderResearch(researchArgs{}); got != want {
		t.Errorf("sub-agent task = %q, want zero-value render %q", got, want)
	}
}

// TestAsToolFuncSchemaFromP pins that the schema advertised to the model is
// still derived from P, exactly as with AsTool.
func TestAsToolFuncSchemaFromP(t *testing.T) {
	tool := AsToolFunc[researchArgs](New(&scriptedProvider{}), "researcher", "delegate research", renderResearch)

	var schema struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if schema.Name != "researcher" || schema.Description != "delegate research" {
		t.Errorf("schema name/description = %q/%q", schema.Name, schema.Description)
	}
	for _, prop := range []string{"topic", "questions"} {
		if _, ok := schema.Parameters.Properties[prop]; !ok {
			t.Errorf("schema missing property %q (have %v)", prop, schema.Parameters.Properties)
		}
	}
	if len(schema.Parameters.Required) != 1 || schema.Parameters.Required[0] != "topic" {
		t.Errorf("required = %v, want [topic]", schema.Parameters.Required)
	}
}

// TestAsToolFuncStreamsTagged verifies the renderer variant inherits AsTool's
// streaming behavior: sub-agent events arrive tagged with the tool name.
func TestAsToolFuncStreamsTagged(t *testing.T) {
	subText := "sub result"
	subProvider := &recordingProvider{turns: []Message{{Role: "assistant", Content: &subText}}}
	sub := New(subProvider)

	final := "done"
	orch := New(&recordingProvider{turns: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "r1", Type: "function",
			Function: FunctionCall{Name: "researcher", Arguments: `{"topic":"x"}`},
		}}},
		{Role: "assistant", Content: &final},
	}})
	orch.RegisterTool(AsToolFunc[researchArgs](sub, "researcher", "delegate research", renderResearch))

	var tagged []string
	if _, err := orch.RunStream(context.Background(), "go", func(ev StreamEvent) {
		if ev.Kind == StreamText && ev.Agent == "researcher" {
			tagged = append(tagged, ev.Text)
		}
	}); err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if len(tagged) != 1 || tagged[0] != subText {
		t.Errorf("tagged sub-agent text = %v, want [%q]", tagged, subText)
	}
	if subProvider.streamCalls != 1 {
		t.Errorf("sub-agent streamCalls = %d, want 1", subProvider.streamCalls)
	}
}
