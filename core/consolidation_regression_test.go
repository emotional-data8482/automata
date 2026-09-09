package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
)

func TestTypedNativeLimitPreservesValidationCause(t *testing.T) {
	p := &nativeCapturingProvider{turns: []Message{asstText(`{"name":"Ada"}`)}}
	a := newTestAgent(t, p, AgentConfig{})
	_, r, err := RunTyped[personResult](context.Background(), a, "go", nil, WithNativeStructuredOutput(), WithMaxTurns(1))
	if !errors.Is(err, ErrMaxStepsExceeded) || !errors.Is(err, ErrInvalidStructuredOutput) || r.Status != RunLimitReached || r.ProviderAttempts != 1 {
		t.Fatalf("result %+v, error %v", r, err)
	}
}

func TestTypedCorrectionSharesToolBudgets(t *testing.T) {
	for _, policy := range []ToolPolicy{{MaxCalls: 1}, {PerTool: map[string]ToolLimits{"work": {MaxCalls: 1}}}} {
		calls := 0
		p := &scriptedProvider{turns: []Message{
			asstTool("a", "work", `{}`),
			asstTool("b", structuredOutputToolName, `{"name":"Ada"}`),
			asstTool("c", "work", `{}`),
			asstText(`{"name":"Ada","age":36}`),
		}}
		a := newTestAgent(t, p, AgentConfig{ToolPolicy: policy, Tools: []Tool{Func("work", "", func(context.Context, struct{}) (string, error) { calls++; return "ok", nil })}})
		v, r, err := RunTyped[personResult](context.Background(), a, "go")
		if err != nil || v.Age != 36 || calls != 1 || r.Turns != 4 || r.ProviderAttempts != 4 {
			t.Fatalf("value %+v, result %+v, calls %d, error %v", v, r, calls, err)
		}
		results := transcriptToolResults(r.Messages[len(r.Messages)-2 : len(r.Messages)-1])
		if len(results) != 1 || !results[0].IsError {
			t.Fatalf("correction must retain budget denial: %+v", results)
		}
	}
}

func TestCapturedOptionsConcurrentReuse(t *testing.T) {
	temp := 0.4
	tool := &definitionTool{definition: ToolDefinition{Name: "frozen", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	opts := []RunOption{WithTools(tool), WithCallOptions(CallOptionsPatch{Temperature: Setting[*float64]{Set: true, Value: &temp}, StopSequences: Setting[[]string]{Set: true, Value: []string{"stop"}}})}
	temp = 9
	tool.definition.InputSchema[0] = '!'
	tool.definition.Name = "changed"
	a := newTestAgent(t, invokeFunc(func(_ context.Context, req Request) (Response, error) {
		if *req.Options.Temperature != 0.4 || req.Options.StopSequences[0] != "stop" || req.Tools[0].Name != "frozen" || !json.Valid(req.Tools[0].InputSchema) {
			t.Errorf("mutated captured option: %+v", req)
		}
		*req.Options.Temperature = 99
		req.Options.StopSequences[0] = "changed"
		req.Tools[0].InputSchema[0] = '!'
		return fixtureResponse(asstText("done")), nil
	}), AgentConfig{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := a.Run(context.Background(), "go", opts...); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestStreamRejectedCallsRemainReplayable(t *testing.T) {
	for _, tc := range []struct {
		name, id, input string
		cause           error
		kept            int
	}{
		{"complete interrupted", "b", `{}`, io.ErrUnexpectedEOF, 2},
		{"malformed sibling", "b", `{"x":`, io.ErrUnexpectedEOF, 0},
		{"missing id", "", `{}`, io.ErrUnexpectedEOF, 0},
		{"duplicate id", "a", `{}`, io.ErrUnexpectedEOF, 0},
		{"cancelled fragment", "b", `{"x":`, context.Canceled, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &scriptedStreamProvider{turns: [][]StreamChunk{{
				{Deltas: []BlockDelta{{Index: 0, Type: "text", Text: "partial"}, {Index: 1, Type: "tool_use", ID: "a", Name: "work", PartialJSON: `{}`}, {Index: 2, Type: "tool_use", ID: tc.id, Name: "work", PartialJSON: tc.input}}},
				{Usage: &Usage{InputTokens: 7}, Err: tc.cause},
			}}}
			a := newTestAgent(t, p, AgentConfig{Tools: []Tool{Func("work", "", func(context.Context, struct{}) (string, error) { t.Error("executed interrupted call"); return "", nil })}})
			r, err := a.RunStream(context.Background(), "go", nil)
			if !errors.Is(err, tc.cause) || r.Output != "partial" || r.Usage.InputTokens != 7 || len(r.FinalMessage.ToolUses()) != tc.kept {
				t.Fatalf("result %+v, error %v", r, err)
			}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var restored RunResult
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			next := newTestAgent(t, invokeFunc(func(_ context.Context, req Request) (Response, error) {
				if err := validateHistory(req.Messages); err != nil {
					t.Error(err)
				}
				return fixtureResponse(asstText("continued")), nil
			}), AgentConfig{})
			s, err := next.ResumeSession(restored.Messages)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Run(context.Background(), "next"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckpointObserversDetachNestedResultsAndUsage(t *testing.T) {
	p := &scriptedProvider{turns: []Message{withUsage(asstTool("a", "image", `{}`), &Usage{InputTokens: 7}), asstText("done")}}
	mutate := func(messages []Message) {
		for _, m := range messages {
			if m.Usage != nil {
				m.Usage.InputTokens = 99
			}
			for _, r := range transcriptToolResults([]Message{m}) {
				r.Content[0].(ImageBlock).Data[0] = 99
			}
		}
	}
	check := func(messages []Message) {
		for _, m := range messages {
			if m.Usage != nil && m.Usage.InputTokens != 7 {
				t.Error("usage alias")
			}
			for _, r := range transcriptToolResults([]Message{m}) {
				if r.Content[0].(ImageBlock).Data[0] != 1 {
					t.Error("image alias")
				}
			}
		}
	}
	a := newTestAgent(t, p, AgentConfig{Tools: []Tool{FuncResult("image", "", func(context.Context, struct{}) (ToolResult, error) {
		return ToolResult{Blocks: Blocks{ImageBlock{MediaType: "image/png", Data: []byte{1}}}}, nil
	})}, Observers: []RunObserver{
		func(_ context.Context, e RunEvent) {
			if p, ok := e.Payload.(CheckpointCommittedPayload); ok {
				mutate(p.Checkpoint.Messages)
			}
		},
		func(_ context.Context, e RunEvent) {
			if p, ok := e.Payload.(CheckpointCommittedPayload); ok {
				check(p.Checkpoint.Messages)
			}
		},
	}})
	s := a.NewSession()
	r, err := s.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	mutate(r.Messages)
	check(s.Messages())
}
