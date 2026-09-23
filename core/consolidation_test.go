package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/emotional-data8482/automata/retry"
)

type invokeFunc func(context.Context, Request) (Response, error)

func (f invokeFunc) Invoke(c context.Context, r Request) (Response, error) { return f(c, r) }
func newTestAgent(t *testing.T, p Provider, c AgentConfig) *Agent {
	t.Helper()
	a, e := New(p, c)
	if e != nil {
		t.Fatal(e)
	}
	return a
}

func TestFrozenConstruction(t *testing.T) {
	temp := 0.4
	schema := json.RawMessage(`{"type":"object"}`)
	tool := &definitionTool{definition: ToolDefinition{Name: "frozen", InputSchema: schema}}
	config := AgentConfig{Tools: []Tool{tool}, CallOptions: CallOptions{Temperature: &temp, StopSequences: []string{"stop"}, ThinkingBudget: 10}, ToolPolicy: ToolPolicy{PerTool: map[string]ToolLimits{"frozen": {MaxCalls: 1}}}}
	a := newTestAgent(t, invokeFunc(func(_ context.Context, r Request) (Response, error) {
		if *r.Options.Temperature != 0.4 || r.Options.StopSequences[0] != "stop" || r.Tools[0].Name != "frozen" {
			t.Errorf("mutated request: %+v", r)
		}
		r.Tools[0].InputSchema[0] = '!'
		r.Options.StopSequences[0] = "changed"
		return fixtureResponse(asstText("ok")), nil
	}), config)
	temp = 1
	config.Tools[0] = nil
	config.CallOptions.StopSequences[0] = "changed"
	config.ToolPolicy.PerTool["frozen"] = ToolLimits{MaxCalls: 99}
	tool.definition.Name = "changed"
	schema[0] = '!'
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, e := runAgent(t, a, "go"); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if a.toolPolicy.PerTool["frozen"].MaxCalls != 1 {
		t.Fatal("policy alias")
	}
}

type definitionTool struct{ definition ToolDefinition }

func (t *definitionTool) Definition() ToolDefinition { return t.definition }
func (t *definitionTool) Execute(context.Context, json.RawMessage) (ToolResult, error) {
	return TextResult("ok"), nil
}

func TestInvalidConstruction(t *testing.T) {
	p := invokeFunc(func(context.Context, Request) (Response, error) { t.Fatal("provider invoked"); return Response{}, nil })
	tool := Func("tool", "", func(context.Context, struct{}) (string, error) { return "", nil })
	for _, cfg := range []AgentConfig{{MaxTurns: -1}, {Tools: []Tool{nil}}, {Tools: []Tool{tool, tool}}, {Tools: []Tool{Func(structuredOutputToolName, "", func(context.Context, struct{}) (string, error) { return "", nil })}}, {ToolPolicy: ToolPolicy{MaxCalls: -1}}, {CallOptions: CallOptions{MaxTokens: -1}}} {
		if _, e := New(p, cfg); e == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	if _, e := New(nil, AgentConfig{}); e == nil {
		t.Fatal("nil provider accepted")
	}
}

func TestTypedPublicAccountingAndFinalization(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "tool", true: "native"}[native], func(t *testing.T) {
			p := &nativeCapturingProvider{turns: []Message{withUsage(asstTool("a", structuredOutputToolName, `{"name":"Ada"}`), &Usage{InputTokens: 10}), withUsage(asstTool("b", structuredOutputToolName, `{"name":"Ada","age":36}`), &Usage{InputTokens: 8})}}
			if native {
				p.turns[0] = withUsage(asstText("unusable"), &Usage{InputTokens: 10})
				p.turns[1] = withUsage(asstText(`{"name":"Ada","age":36}`), &Usage{InputTokens: 8})
			}
			a := newTestAgent(t, p, AgentConfig{})
			var opts []typedOption
			if native {
				opts = append(opts, withNative())
			}
			_, r, e := runTyped[personResult](t, a, "go", opts...)
			if e != nil || r.Turns != 2 || r.ProviderAttempts != 2 || r.Usage.InputTokens != 18 {
				t.Fatalf("%+v %v", r, e)
			}
		})
	}
}

// A provider turn rejected as incomplete keeps its evidence, and the failed
// turn's committed history is valid for the next conversation turn.
func TestReplayReconciliationAndContinuation(t *testing.T) {
	valid := ToolUseBlock{ID: "a", Name: "tool", Input: json.RawMessage(`{}`)}
	for _, tc := range []struct {
		name  string
		calls Blocks
		stop  StopReason
		kept  int
	}{
		{"incomplete", Blocks{valid}, StopIncomplete, 1},
		{"missing stop", Blocks{valid}, "", 1},
		{"shape disagreement", Blocks{valid}, StopEndTurn, 1},
		{"mixed malformed", Blocks{valid, ToolUseBlock{ID: "b", Name: "tool", Input: json.RawMessage(`{"x":`)}}, StopToolUse, 0},
		{"duplicate", Blocks{valid, valid}, StopToolUse, 0},
		{"missing id", Blocks{ToolUseBlock{Name: "tool", Input: json.RawMessage(`{}`)}}, StopToolUse, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := Message{Role: "assistant", Blocks: append(Blocks{TextBlock{Text: "partial"}}, tc.calls...), Usage: &Usage{InputTokens: 7}}
			calls := 0
			provider := invokeFunc(func(_ context.Context, req Request) (Response, error) {
				calls++
				if calls == 1 {
					return Response{Message: msg, StopReason: tc.stop}, nil
				}
				if e := validateHistory(req.Messages); e != nil {
					t.Error(e)
				}
				return fixtureResponse(asstText("continued")), nil
			})
			a := newTestAgent(t, provider, AgentConfig{Tools: []Tool{Func("tool", "", func(context.Context, struct{}) (string, error) { t.Error("executed incomplete call"); return "", nil })}})
			runtime := newTestRuntime(t)
			ref, e := runtime.Register("agent", "v1", a)
			if e != nil {
				t.Fatal(e)
			}
			thread := ConversationRef{ID: "replay"}
			r, e := runtime.Run(context.Background(), ref, "go", WithConversation(thread, ""))
			if e == nil || r.Output != "partial" || r.Usage.InputTokens != 7 || len(r.FinalMessage.ToolUses()) != tc.kept {
				t.Fatalf("%+v %v", r, e)
			}
			data, e := json.Marshal(r)
			if e != nil {
				t.Fatal(e)
			}
			var rr RunResult
			if e = json.Unmarshal(data, &rr); e != nil {
				t.Fatal(e)
			}
			if tc.kept == 0 && len(rr.Diagnostics) == 0 {
				t.Fatal("no rejected evidence")
			}
			next, e := runtime.Run(context.Background(), ref, "next", WithConversation(thread, r.RunID))
			if e != nil || next.Output != "continued" {
				t.Fatalf("continuation = %+v, %v", next, e)
			}
		})
	}
}

func TestHistoryValidationLocations(t *testing.T) {
	for _, messages := range [][]Message{{asstTool("a", "tool", `{}`)}, {ToolResultMessage("missing", "ok", false)}, {asstTool("a", "tool", `{"x":`)}} {
		if e := validateHistory(messages); e == nil || !strings.Contains(e.Error(), "message 0") {
			t.Fatalf("bad location: %v", e)
		}
	}
}

func TestAttemptsCancellationAndNoStaleOutput(t *testing.T) {
	attempts := 0
	p := invokeFunc(func(_ context.Context, r Request) (Response, error) {
		attempts++
		if attempts == 1 {
			r.Messages[0].Blocks[0] = TextBlock{Text: "mutated"}
			return Response{}, &retryableError{}
		}
		if r.Messages[0].Text() != "go" {
			t.Error("retry request alias")
		}
		return fixtureResponse(withUsage(asstText("done"), &Usage{InputTokens: 3})), nil
	})
	cfg := retry.Config{MaxAttempts: 2}
	a := newTestAgent(t, p, AgentConfig{Retry: &cfg})
	r, e := runAgent(t, a, "go")
	if e != nil || r.Turns != 1 || r.ProviderAttempts != 2 || r.Usage.InputTokens != 3 {
		t.Fatalf("%+v %v", r, e)
	}
	// A failed next turn must not report the previous turn's answer.
	runtime := newTestRuntime(t)
	ref, e := runtime.Register("chat", "v1", newTestAgent(t, &scriptedProvider{turns: []Message{asstText("earlier")}}, AgentConfig{}))
	if e != nil {
		t.Fatal(e)
	}
	thread := ConversationRef{ID: "stale"}
	first, e := runtime.Run(context.Background(), ref, "first", WithConversation(thread, ""))
	if e != nil {
		t.Fatal(e)
	}
	r, e = runtime.Run(context.Background(), ref, "second", WithConversation(thread, first.RunID))
	if e == nil || r.Output != "" || r.FinalMessage.Role != "" || r.ProviderStopReason != "" {
		t.Fatalf("stale output %+v %v", r, e)
	}
	// Logical cancellation during a provider call keeps the partial answer.
	handles := make(chan *RunHandle, 1)
	ref, e = runtime.Register("canceled", "v1", newTestAgent(t, invokeFunc(func(context.Context, Request) (Response, error) {
		h := <-handles
		if err := h.Cancel(context.Background()); err != nil {
			t.Error(err)
		}
		return fixtureResponse(asstText("partial")), nil
	}), AgentConfig{}))
	if e != nil {
		t.Fatal(e)
	}
	h, e := runtime.Submit(context.Background(), ref, "go")
	if e != nil {
		t.Fatal(e)
	}
	handles <- h
	r, e = h.Await(context.Background())
	if !errors.Is(e, context.Canceled) || r.Status != RunCancelled || r.Output != "partial" {
		t.Fatalf("%+v %v", r, e)
	}
}

type retryableError struct{}

func (*retryableError) Error() string   { return "retry" }
func (*retryableError) Retryable() bool { return true }
