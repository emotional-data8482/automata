package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestTypedNativeLimitPreservesValidationCause(t *testing.T) {
	p := &nativeCapturingProvider{turns: []Message{asstText(`{"name":"Ada"}`)}}
	a := newTestAgent(t, p, AgentConfig{MaxTurns: 1})
	_, r, err := runTyped[personResult](t, a, "go", withNative())
	if !errors.Is(err, ErrMaxTurnsExceeded) || !errors.Is(err, ErrInvalidStructuredOutput) || r.Status != RunLimitReached || r.ProviderAttempts != 1 {
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
		v, r, err := runTyped[personResult](t, a, "go")
		if err != nil || v.Age != 36 || calls != 1 || r.Turns != 4 || r.ProviderAttempts != 4 {
			t.Fatalf("value %+v, result %+v, calls %d, error %v", v, r, calls, err)
		}
		results := transcriptToolResults(r.Messages[len(r.Messages)-2 : len(r.Messages)-1])
		if len(results) != 1 || !results[0].IsError {
			t.Fatalf("correction must retain budget denial: %+v", results)
		}
	}
}

func TestStreamRejectedCallsRemainReplayable(t *testing.T) {
	for _, tc := range []struct {
		name, id, input string
		cause, want     error
		kept            int
	}{
		{"complete interrupted", "b", `{}`, io.ErrUnexpectedEOF, ErrIncompleteResponse, 2},
		{"malformed sibling", "b", `{"x":`, io.ErrUnexpectedEOF, ErrIncompleteResponse, 0},
		{"missing id", "", `{}`, io.ErrUnexpectedEOF, ErrIncompleteResponse, 0},
		{"duplicate id", "a", `{}`, io.ErrUnexpectedEOF, ErrIncompleteResponse, 0},
		{"cancelled fragment", "b", `{"x":`, context.Canceled, context.Canceled, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &scriptedStreamProvider{turns: [][]StreamChunk{{
				{Deltas: []BlockDelta{{Index: 0, Type: "text", Text: "partial"}, {Index: 1, Type: "tool_use", ID: "a", Name: "work", PartialJSON: `{}`}, {Index: 2, Type: "tool_use", ID: tc.id, Name: "work", PartialJSON: tc.input}}},
				{Usage: &Usage{InputTokens: 7}, Err: tc.cause},
			}}}
			provider := &continuingStreamProvider{first: p, t: t}
			a := newTestAgent(t, provider, AgentConfig{Tools: []Tool{Func("work", "", func(context.Context, struct{}) (string, error) { t.Error("executed interrupted call"); return "", nil })}})
			runtime := newTestRuntime(t)
			ref, err := runtime.Register("agent", "v1", a)
			if err != nil {
				t.Fatal(err)
			}
			thread := ConversationRef{ID: "stream"}
			r, err := runtime.RunStream(context.Background(), ref, "go", func(StreamEvent) {}, WithConversation(thread, ""))
			if !errors.Is(err, tc.want) || r.Output != "partial" || r.Usage.InputTokens != 7 || len(r.FinalMessage.ToolUses()) != tc.kept {
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
			if err := validateHistory(restored.Messages); err != nil {
				t.Fatalf("restored history: %v", err)
			}
			next, err := runtime.Run(context.Background(), ref, "next", WithConversation(thread, r.RunID))
			if err != nil || next.Output != "continued" {
				t.Fatalf("continuation = %+v, %v", next, err)
			}
		})
	}
}

// continuingStreamProvider streams its first turn from a scripted provider
// and answers every later turn with "continued" after validating the
// history it was sent.
type continuingStreamProvider struct {
	first *scriptedStreamProvider
	calls int
	t     *testing.T
}

func (p *continuingStreamProvider) Invoke(context.Context, Request) (Response, error) {
	return Response{}, errors.New("Invoke should not be used on the streaming path")
}

func (p *continuingStreamProvider) InvokeStream(ctx context.Context, req Request) (<-chan StreamChunk, error) {
	p.calls++
	if p.calls == 1 {
		return p.first.InvokeStream(ctx, req)
	}
	if err := validateHistory(req.Messages); err != nil {
		p.t.Error(err)
	}
	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		streamMessage(ch, asstText("continued"))
	}()
	return ch, nil
}
