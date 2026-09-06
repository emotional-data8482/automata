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

func (p *capturingProvider) Invoke(_ context.Context, req Request) (Response, error) {
	p.received = append(p.received, append([]Message(nil), req.Messages...))
	if p.calls >= len(p.turns) {
		return Response{}, fmt.Errorf("no script for turn %d", p.calls)
	}
	m := p.turns[p.calls]
	p.calls++
	return Response{Message: m}, nil
}

// lastUserMessage returns the content of the most recent role:"user" message
// seen by the provider.
func (p *capturingProvider) lastUserMessage(t *testing.T) string {
	t.Helper()
	for i := len(p.received) - 1; i >= 0; i-- {
		for j := len(p.received[i]) - 1; j >= 0; j-- {
			if m := p.received[i][j]; m.Role == "user" {
				return m.Text()
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
	subProvider := &capturingProvider{turns: []Message{asstText("notes")}}
	sub := New(subProvider)

	final := "final"
	orch := New(&scriptedProvider{turns: []Message{
		asstTool("r1", "researcher", `{"topic":"GLP-1","questions":["cost","supply"]}`),
		asstText(final),
	}})
	orch.RegisterTool(AsToolFunc(sub, "researcher", "delegate research", renderResearch))

	out, err := orch.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Output != final {
		t.Errorf("output = %q, want %q", out.Output, final)
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
		asstTool("r1", "researcher", `{"topic":`),
		asstText(final),
	}})
	orch.RegisterTool(AsToolFunc(sub, "researcher", "delegate research", renderResearch))

	var result string
	var isError bool
	out, err := orch.RunStream(context.Background(), "go", func(ev StreamEvent) {
		if ev.Kind == StreamToolResult {
			result = ev.Result
			isError = ev.IsError
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out.Output != final {
		t.Errorf("output = %q, want %q", out.Output, final)
	}
	// The core result carries the raw error text (no "error: " prefix) with the
	// IsError flag set.
	if !strings.HasPrefix(result, "invalid args") || !isError {
		t.Errorf("tool result = %q (isError %v), want prefix %q with isError", result, isError, "invalid args")
	}
	if subProvider.calls != 0 {
		t.Errorf("sub-agent was invoked %d times on invalid args, want 0", subProvider.calls)
	}
}

// Missing required arguments are rejected before the renderer or child runs.
func TestAsToolFuncMissingRequiredArgs(t *testing.T) {
	subProvider := &capturingProvider{turns: []Message{asstText("done")}}
	sub := New(subProvider)

	final := "final"
	orch := New(&scriptedProvider{turns: []Message{
		asstTool("r1", "researcher", "{}"),
		asstText(final),
	}})
	orch.RegisterTool(AsToolFunc(sub, "researcher", "delegate research", renderResearch))

	if _, err := orch.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if subProvider.calls != 0 {
		t.Error("child executed with missing required arguments")
	}
}

// TestAsToolFuncSchemaFromP pins that the schema advertised to the model is
// still derived from P, exactly as with AsTool.
func TestAsToolFuncSchemaFromP(t *testing.T) {
	tool := AsToolFunc(New(&scriptedProvider{}), "researcher", "delegate research", renderResearch)

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(tool.Definition().InputSchema, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if tool.Definition().Name != "researcher" || tool.Definition().Description != "delegate research" {
		t.Errorf("schema name/description = %q/%q", tool.Definition().Name, tool.Definition().Description)
	}
	for _, prop := range []string{"topic", "questions"} {
		if _, ok := schema.Properties[prop]; !ok {
			t.Errorf("schema missing property %q (have %v)", prop, schema.Properties)
		}
	}
	if len(schema.Required) != 1 || schema.Required[0] != "topic" {
		t.Errorf("required = %v, want [topic]", schema.Required)
	}
}

// TestAsToolFuncStreamsTagged verifies the renderer variant inherits AsTool's
// streaming behavior: sub-agent events arrive tagged with the tool name.
func TestAsToolFuncStreamsTagged(t *testing.T) {
	subText := "sub result"
	subProvider := &recordingProvider{turns: []Message{asstText(subText)}}
	sub := New(subProvider)

	final := "done"
	orch := New(&recordingProvider{turns: []Message{
		asstTool("r1", "researcher", `{"topic":"x"}`),
		asstText(final),
	}})
	orch.RegisterTool(AsToolFunc(sub, "researcher", "delegate research", renderResearch))

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
