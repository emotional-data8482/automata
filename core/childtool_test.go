package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// capturingProvider replays scripted Messages and records the message history
// it receives on each turn, so tests can assert what task a child was handed.
type capturingProvider struct {
	mu       sync.Mutex
	turns    []Message
	calls    int
	received [][]Message
}

func (p *capturingProvider) Invoke(_ context.Context, req Request) (Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.received = append(p.received, append([]Message(nil), req.Messages...))
	if p.calls >= len(p.turns) {
		return Response{}, fmt.Errorf("no script for turn %d", p.calls)
	}
	m := p.turns[p.calls]
	p.calls++
	return fixtureResponse(m), nil
}

// lastUserMessage returns the content of the most recent role:"user" message
// seen by the provider.
func (p *capturingProvider) lastUserMessage(t *testing.T) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
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

func TestChildToolDerivesSchemaFromP(t *testing.T) {
	tool := ChildTool[researchArgs]("researcher", "delegate research", DefinitionRef{ID: "researcher", Revision: "v1"})

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

func TestChildToolRejectsIncompleteReference(t *testing.T) {
	runtime := newTestRuntime(t)
	parent := testAgent(nil).WithTools(ChildTool[researchArgs]("researcher", "delegate research", DefinitionRef{ID: "researcher"}))
	if _, err := runtime.Register("parent", "v1", parent); err == nil {
		t.Fatal("Register accepted a child tool without a revision")
	}
}

// The child's task is the model's validated arguments as JSON, and its
// accepted answer becomes the parent's tool result.
func TestChildToolHandsArgumentsToChildRun(t *testing.T) {
	runtime := newTestRuntime(t)
	childProvider := &capturingProvider{turns: []Message{asstText("found it")}}
	researcher, err := runtime.Register("researcher", "v1", testAgent(childProvider))
	if err != nil {
		t.Fatal(err)
	}
	parentProvider := &capturingProvider{turns: []Message{
		asstTool("r1", "researcher", `{"topic":"tides"}`),
		asstText("done"),
	}}
	lead, err := runtime.Register("lead", "v1", testAgent(parentProvider).WithTools(ChildTool[researchArgs]("researcher", "delegate research", researcher)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), lead, "go")
	if err != nil {
		t.Fatal(err)
	}
	if got := childProvider.lastUserMessage(t); got != `{"topic":"tides"}` {
		t.Fatalf("child task = %q", got)
	}
	toolResult := result.Messages[len(result.Messages)-2]
	if toolResult.Role != "tool" || toolResult.Blocks[0].(ToolResultBlock).Content[0].(TextBlock).Text != "found it" {
		t.Fatalf("parent tool result = %#v", toolResult)
	}
}

// A child run's provisional events reach the parent's live view, tagged with
// the child tool name and the call ID that started it. A grandchild keeps the
// innermost tag.
func TestChildRunEventsStreamIntoAncestorViews(t *testing.T) {
	runtime := newTestRuntime(t)
	leafProvider := &recordingProvider{turns: []Message{asstText("leaf text")}}
	leaf, err := runtime.Register("leaf", "v1", testAgent(leafProvider))
	if err != nil {
		t.Fatal(err)
	}
	middleProvider := &recordingProvider{turns: []Message{
		asstTool("m1", "leaf", `{"topic":"x"}`),
		asstText("middle text"),
	}}
	middle, err := runtime.Register("middle", "v1", testAgent(middleProvider).WithTools(ChildTool[researchArgs]("leaf", "leaf work", leaf)))
	if err != nil {
		t.Fatal(err)
	}
	rootProvider := &recordingProvider{turns: []Message{
		asstTool("r1", "middle", `{"topic":"x"}`),
		asstText("root text"),
	}}
	root, err := runtime.Register("root", "v1", testAgent(rootProvider).WithTools(ChildTool[researchArgs]("middle", "middle work", middle)))
	if err != nil {
		t.Fatal(err)
	}

	type tag struct{ agent, invocation, text string }
	var texts []tag
	if _, err := runtime.RunStream(context.Background(), root, "go", func(ev StreamEvent) {
		if ev.Kind == StreamText {
			texts = append(texts, tag{ev.Agent, ev.InvocationID, ev.Text})
		}
	}); err != nil {
		t.Fatal(err)
	}
	want := map[tag]bool{
		{"", "", "root text"}:           true,
		{"middle", "r1", "middle text"}: true,
		{"leaf", "m1", "leaf text"}:     true,
	}
	if len(texts) != len(want) {
		t.Fatalf("streamed texts = %+v, want %v", texts, want)
	}
	for _, got := range texts {
		if !want[got] {
			t.Fatalf("unexpected streamed text %+v (all: %+v)", got, texts)
		}
	}
}
