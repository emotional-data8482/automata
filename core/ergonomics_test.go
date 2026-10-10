package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestJSONResult(t *testing.T) {
	result, err := JSONResult(map[string]int{"n": 2})
	if err != nil || result.Text() != `{"n":2}` || result.IsError {
		t.Fatalf("JSONResult = %#v, %v", result, err)
	}
	if _, err := JSONResult(make(chan int)); err == nil {
		t.Fatal("JSONResult encoded a channel")
	}
}

func TestStructuredOutputToolNameIsExported(t *testing.T) {
	if StructuredOutputToolName != "automata_structured_output" {
		t.Fatalf("StructuredOutputToolName = %q", StructuredOutputToolName)
	}
}

type verdictInput struct {
	Verdict  string   `json:"verdict" enum:"approve,reject"`
	Severity *int     `json:"severity,omitempty" enum:"1,2,3"`
	Tags     []string `json:"tags,omitempty" enum:"style,bug"`
}

// An enum tag is advertised to the model and enforced on typed payloads.
func TestEnumTagIsAdvertisedAndEnforced(t *testing.T) {
	definition := FuncResult("review", "review", func(context.Context, verdictInput) (ToolResult, error) {
		return TextResult("ok"), nil
	}).Definition()
	var schema struct {
		Properties map[string]struct {
			Enum  []any `json:"enum"`
			Items struct {
				Enum []any `json:"enum"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties["verdict"].Enum; !slices.Equal(got, []any{"approve", "reject"}) {
		t.Fatalf("verdict enum = %v", got)
	}
	if got := schema.Properties["severity"].Enum; len(got) != 4 || got[0] != float64(1) || got[3] != nil {
		t.Fatalf("severity enum = %v; want 1,2,3 and null", got)
	}
	if got := schema.Properties["tags"].Items.Enum; !slices.Equal(got, []any{"style", "bug"}) {
		t.Fatalf("tags item enum = %v", got)
	}
	if err := validateToolArguments(definition.InputSchema, json.RawMessage(`{"verdict":"maybe"}`)); err == nil {
		t.Fatal("tool schema accepted a value outside the enum")
	}

	for payload, valid := range map[string]bool{
		`{"verdict":"approve"}`:                       true,
		`{"verdict":"approve","severity":2}`:          true,
		`{"verdict":"approve","severity":null}`:       true,
		`{"verdict":"approve","tags":["bug"]}`:        true,
		`{"verdict":"maybe"}`:                         false,
		`{"verdict":"reject","severity":4}`:           false,
		`{"verdict":"reject","tags":["docs"]}`:        false,
		`{"verdict":"approve","severity":2,"x":1}`:    true,
		`{"verdict":"approve","tags":["bug","x"]}`:    false,
		`{"verdict":"approve","severity":3.0}`:        true,
		`{"verdict":"reject","severity":1,"tags":[]}`: true,
	} {
		violations := validateTypedPayload[verdictInput](json.RawMessage(payload))
		if (len(violations) == 0) != valid {
			t.Errorf("%s: violations = %v, want valid=%v", payload, violations, valid)
		}
	}
}

// A host reaches a child run's result and a tool's receipts from the parent
// without walking its tool batches.
func TestSnapshotToolCallLookups(t *testing.T) {
	runtime := newTestRuntime(t)
	child := testAgent(&scriptedProvider{turns: []Message{asstText("child verdict")}})
	parent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("pay-1", "pay", `{}`), toolUse("ask-1", "reviewer", `{"topic":"tea"}`)),
		asstText("parent done"),
	}})
	parent.RegisterTool(effectTool("pay", nil, func(context.Context) (ToolResult, error) {
		result := TextResult("paid")
		result.Effect = EffectReport{Status: EffectApplied, Receipt: "charge-7"}
		return result, nil
	}, ToolEffectPolicy{Kind: ToolEffectMutating}))
	parent.RegisterTool(ChildTool[struct {
		Topic string `json:"topic"`
	}]("reviewer", "review", DefinitionRef{ID: "child", Revision: "v1"}))
	for id, agent := range map[string]*Agent{"child": child, "parent": parent} {
		if _, err := runtime.Register(id, "v1", agent); err != nil {
			t.Fatal(err)
		}
	}
	result, err := runtime.Run(context.Background(), DefinitionRef{ID: "parent", Revision: "v1"}, "go")
	if err != nil || result.Output != "parent done" {
		t.Fatalf("parent = %+v, %v", result, err)
	}
	handle := runtime.Handle(result.RunID)
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if invocation, ok := snapshot.ToolCall("pay-1"); !ok || invocation.Effect.Receipt != "charge-7" {
		t.Fatalf("ToolCall(pay-1) = %#v, %v", invocation, ok)
	}
	if _, ok := snapshot.ToolCall("missing"); ok {
		t.Fatal("ToolCall found a missing call")
	}
	if effects := snapshot.AppliedEffects("pay"); len(effects) != 1 || effects[0].Receipt != "charge-7" {
		t.Fatalf("AppliedEffects(pay) = %#v", effects)
	}
	if effects := snapshot.AppliedEffects("reviewer"); len(effects) != 0 {
		t.Fatalf("AppliedEffects(reviewer) = %#v", effects)
	}
	childHandle, err := handle.Child(context.Background(), "ask-1")
	if err != nil {
		t.Fatal(err)
	}
	childResult, err := childHandle.Await(context.Background())
	if err != nil || childResult.Output != "child verdict" {
		t.Fatalf("child = %+v, %v", childResult, err)
	}
	if _, err := handle.Child(context.Background(), "pay-1"); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("Child(pay-1) = %v, want ErrOperationNotFound", err)
	}
}

func TestCurrentTimeStampsLatestUserMessageStably(t *testing.T) {
	clock := time.Date(2026, 10, 10, 9, 30, 0, 0, time.UTC)
	hook := CurrentTime(func() time.Time { return clock })
	messages := []Message{UserMessage("earlier"), asstText("ok"), UserMessage("what day is it?")}
	ctx := context.WithValue(context.Background(), currentTimeContextKey{}, currentTimeContext{times: &currentTimeStamps{}, message: 2})
	req, err := hook(ctx, Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Messages[2].Text(); !strings.HasPrefix(got, "Current time: 2026-10-10T09:30:00Z") || !strings.HasSuffix(got, "what day is it?") {
		t.Fatalf("stamped message = %q", got)
	}
	if req.Messages[0].Text() != "earlier" || messages[2].Text() != "what day is it?" {
		t.Fatal("hook changed an earlier message or the canonical transcript")
	}
	// A later tool turn of the same run keeps the first stamp.
	clock = clock.Add(time.Minute)
	later := append(slices.Clone(messages), asstTool("c1", "lookup", `{}`), ToolResultBlockMessage("c1", Blocks{TextBlock{Text: "r"}}, false))
	req, err = hook(ctx, Request{Messages: later})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Messages[2].Text(); !strings.HasPrefix(got, "Current time: 2026-10-10T09:30:00Z") {
		t.Fatalf("restamped message = %q", got)
	}
}
