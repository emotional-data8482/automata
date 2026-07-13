package core

import (
	"context"
	"strings"
	"testing"
)

type personResult struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

// TestRunTypedHappyPath pins the common case: the model calls the injected
// structured_output tool, and RunTyped decodes its arguments into T.
func TestRunTypedHappyPath(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := New(provider)

	got, res, err := RunTyped[personResult](context.Background(), agent, "who?")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v, want {Ada 36}", got)
	}
	if res.StopReason != StopEndTurn {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopEndTurn)
	}
}

// TestRunTypedInjectsSchemaTool pins that the injected tool's JSON schema is
// derived from T and advertised to the model.
func TestRunTypedInjectsSchemaTool(t *testing.T) {
	capt := &toolCapturingProvider{reply: asstTool("s1", structuredOutputToolName, `{"name":"x","age":1}`)}
	agent := New(capt)

	if _, _, err := RunTyped[personResult](context.Background(), agent, "go"); err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if capt.toolNames[structuredOutputToolName] == "" {
		t.Fatalf("structured_output tool not advertised; saw %v", capt.toolNames)
	}
	// The schema should mention the struct's fields.
	schema := capt.toolNames[structuredOutputToolName]
	for _, field := range []string{"name", "age"} {
		if !strings.Contains(schema, field) {
			t.Errorf("schema missing field %q: %s", field, schema)
		}
	}
}

// TestRunTypedForcesToolOnTextFallback pins the fallback: when the model answers
// with prose, RunTyped issues a second turn forcing the tool with thinking
// disabled, and decodes that.
func TestRunTypedForcesToolOnTextFallback(t *testing.T) {
	capt := &forcingProvider{
		replies: []Message{
			asstText("Ada is 36 years old."),                                    // turn 1: prose, no tool call
			asstTool("s2", structuredOutputToolName, `{"name":"Ada","age":36}`), // forced turn
		},
	}
	agent := New(capt).WithDefaultCallOptions(CallOptions{ThinkingBudget: 2048})

	got, _, err := RunTyped[personResult](context.Background(), agent, "who?")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" || got.Age != 36 {
		t.Errorf("decoded = %+v, want {Ada 36}", got)
	}
	// The second (forced) turn must set tool_choice=structured_output and
	// disable thinking, even though the agent default enabled it.
	if len(capt.seen) != 2 {
		t.Fatalf("provider saw %d turns, want 2", len(capt.seen))
	}
	forced := capt.seen[1]
	if forced.ToolChoice == nil || forced.ToolChoice.Mode != ToolChoiceTool || forced.ToolChoice.Name != structuredOutputToolName {
		t.Errorf("forced turn tool choice = %+v, want forced structured_output", forced.ToolChoice)
	}
	if forced.ThinkingBudget != 0 {
		t.Errorf("forced turn ThinkingBudget = %d, want 0 (thinking must be off)", forced.ThinkingBudget)
	}
}

// TestRunTypedDecodeError pins that malformed arguments surface as a decode
// error rather than a silent zero value.
func TestRunTypedDecodeError(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("s1", structuredOutputToolName, `{"name":"x","age":"not-a-number"}`),
	}}
	agent := New(provider)

	_, _, err := RunTyped[personResult](context.Background(), agent, "go")
	if err == nil || !strings.Contains(err.Error(), "decode structured output") {
		t.Errorf("err = %v, want a decode error", err)
	}
}

// TestRunTypedCoexistsWithRealTools pins that RunTyped works when the agent also
// has ordinary tools: the model may call a real tool first, then the terminal.
func TestRunTypedCoexistsWithRealTools(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("c1", "lookup", `{"q":"Ada"}`),
		asstTool("s1", structuredOutputToolName, `{"name":"Ada","age":36}`),
	}}
	agent := New(provider)
	agent.RegisterTool(Func("lookup", "look up a person", func(_ context.Context, _ struct {
		Q string `json:"q"`
	}) (string, error) {
		return "found", nil
	}))

	got, _, err := RunTyped[personResult](context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("RunTyped: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("decoded = %+v, want name Ada", got)
	}
}

// --- test providers -------------------------------------------------------

// toolCapturingProvider records the tool schemas it was sent, then replies once.
type toolCapturingProvider struct {
	reply     Message
	toolNames map[string]string // name -> schema JSON
	calls     int
}

func (p *toolCapturingProvider) Invoke(_ context.Context, req Request) (Response, error) {
	if p.toolNames == nil {
		p.toolNames = map[string]string{}
	}
	for _, tl := range req.Tools {
		p.toolNames[tl.Name()] = string(tl.Schema())
	}
	p.calls++
	return Response{Message: p.reply}, nil
}

// forcingProvider replies with a scripted sequence and records the CallOptions
// of each turn, so a test can assert the forced-turn options.
type forcingProvider struct {
	replies []Message
	calls   int
	seen    []CallOptions
}

func (p *forcingProvider) Invoke(_ context.Context, req Request) (Response, error) {
	p.seen = append(p.seen, req.Options)
	m := p.replies[min(p.calls, len(p.replies)-1)]
	p.calls++
	return Response{Message: m}, nil
}
