package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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

func TestFrozenConstructionAndRunOptionSnapshots(t *testing.T) {
	temp := 0.4
	schema := json.RawMessage(`{"type":"object"}`)
	tool := &definitionTool{definition: ToolDefinition{Name: "frozen", InputSchema: schema}}
	config := AgentConfig{Tools: []Tool{tool}, DefaultCallOptions: CallOptions{Temperature: &temp, StopSequences: []string{"stop"}, ThinkingBudget: 10}, ToolPolicy: ToolPolicy{PerTool: map[string]ToolLimits{"frozen": {MaxCalls: 1}}}}
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
	config.DefaultCallOptions.StopSequences[0] = "changed"
	config.ToolPolicy.PerTool["frozen"] = ToolLimits{MaxCalls: 99}
	tool.definition.Name = "changed"
	schema[0] = '!'
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, e := a.Run(context.Background(), "go"); e != nil {
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

func TestCallOptionsPatchAllFields(t *testing.T) {
	temp := 0.5
	zero := 0.0
	base := CallOptions{Temperature: &temp, MaxTokens: 12, ThinkingBudget: 15, StopSequences: []string{"stop"}, ToolChoice: &ToolChoice{Mode: ToolChoiceTool, Name: "tool"}, OutputSchema: json.RawMessage(`{"type":"object"}`)}
	cases := []struct {
		name  string
		patch CallOptionsPatch
		check func(CallOptions) bool
	}{
		{"inherit", CallOptionsPatch{}, func(o CallOptions) bool { return reflect.DeepEqual(o, base) }},
		{"temperature zero", CallOptionsPatch{Temperature: Setting[*float64]{true, &zero}}, func(o CallOptions) bool { return o.Temperature != nil && *o.Temperature == 0 }},
		{"temperature clear", CallOptionsPatch{Temperature: Setting[*float64]{Set: true}}, func(o CallOptions) bool { return o.Temperature == nil }},
		{"tokens zero", CallOptionsPatch{MaxTokens: Setting[int]{Set: true}}, func(o CallOptions) bool { return o.MaxTokens == 0 }},
		{"tokens set", CallOptionsPatch{MaxTokens: Setting[int]{true, 24}}, func(o CallOptions) bool { return o.MaxTokens == 24 }},
		{"thinking zero", CallOptionsPatch{ThinkingBudget: Setting[int]{Set: true}}, func(o CallOptions) bool { return o.ThinkingBudget == 0 }},
		{"thinking set", CallOptionsPatch{ThinkingBudget: Setting[int]{true, 20}}, func(o CallOptions) bool { return o.ThinkingBudget == 20 }},
		{"stops nil", CallOptionsPatch{StopSequences: Setting[[]string]{Set: true}}, func(o CallOptions) bool { return len(o.StopSequences) == 0 }},
		{"stops empty", CallOptionsPatch{StopSequences: Setting[[]string]{true, []string{}}}, func(o CallOptions) bool { return len(o.StopSequences) == 0 }},
		{"stops set", CallOptionsPatch{StopSequences: Setting[[]string]{true, []string{"new"}}}, func(o CallOptions) bool { return o.StopSequences[0] == "new" }},
		{"choice clear", CallOptionsPatch{ToolChoice: Setting[*ToolChoice]{Set: true}}, func(o CallOptions) bool { return o.ToolChoice == nil }},
		{"choice set", CallOptionsPatch{ToolChoice: Setting[*ToolChoice]{true, &ToolChoice{Mode: ToolChoiceNone}}}, func(o CallOptions) bool { return o.ToolChoice.Mode == ToolChoiceNone }},
		{"schema clear", CallOptionsPatch{OutputSchema: Setting[json.RawMessage]{Set: true}}, func(o CallOptions) bool { return o.OutputSchema == nil }},
		{"schema set", CallOptionsPatch{OutputSchema: Setting[json.RawMessage]{true, json.RawMessage(`{"type":"string"}`)}}, func(o CallOptions) bool { return string(o.OutputSchema) == `{"type":"string"}` }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := runConfig{options: cloneCallOptions(base)}
			WithCallOptions(tc.patch)(&cfg)
			if !tc.check(cfg.options) {
				t.Fatalf("options %+v", cfg.options)
			}
		})
	}
	stops := []string{"captured"}
	choice := &ToolChoice{Mode: ToolChoiceNone}
	raw := json.RawMessage(`{"type":"object"}`)
	option := WithCallOptions(CallOptionsPatch{StopSequences: Setting[[]string]{true, stops}, ToolChoice: Setting[*ToolChoice]{true, choice}, OutputSchema: Setting[json.RawMessage]{true, raw}})
	stops[0] = "bad"
	choice.Mode = ToolChoiceAny
	raw[0] = '!'
	for range 2 {
		var cfg runConfig
		option(&cfg)
		if cfg.options.StopSequences[0] != "captured" || cfg.options.ToolChoice.Mode != ToolChoiceNone || !json.Valid(cfg.options.OutputSchema) {
			t.Fatal("capture alias")
		}
		cfg.options.StopSequences[0] = "bad"
		cfg.options.OutputSchema[0] = '!'
	}
}

func TestInvalidConstructionAndAdmittedOptions(t *testing.T) {
	p := invokeFunc(func(context.Context, Request) (Response, error) { t.Fatal("provider invoked"); return Response{}, nil })
	tool := Func("tool", "", func(context.Context, struct{}) (string, error) { return "", nil })
	for _, cfg := range []AgentConfig{{MaxTurns: -1}, {Tools: []Tool{nil}}, {Tools: []Tool{tool, tool}}, {Tools: []Tool{Func(structuredOutputToolName, "", func(context.Context, struct{}) (string, error) { return "", nil })}}, {ToolPolicy: ToolPolicy{MaxCalls: -1}}, {DefaultCallOptions: CallOptions{MaxTokens: -1}}} {
		if _, e := New(p, cfg); e == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
	if _, e := New(nil, AgentConfig{}); e == nil {
		t.Fatal("nil provider accepted")
	}
	var kinds []RunEventKind
	a := newTestAgent(t, p, AgentConfig{})
	r, e := a.Run(context.Background(), "go", WithMaxTurns(0), WithObserver(func(_ context.Context, e RunEvent) { kinds = append(kinds, e.Kind) }))
	if e == nil || r.Status != RunFailed || r.RunID == "" || r.Turns != 0 || len(r.Messages) != 0 || !reflect.DeepEqual(kinds, []RunEventKind{RunStarted, RunFinished}) {
		t.Fatalf("%+v %v %v", r, e, kinds)
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
			var opts []RunOption
			if native {
				opts = append(opts, WithNativeStructuredOutput())
			}
			_, r, e := RunTyped[personResult](context.Background(), a, "go", opts...)
			if e != nil || r.Turns != 2 || r.ProviderAttempts != 2 || r.Usage.InputTokens != 18 {
				t.Fatalf("%+v %v", r, e)
			}
		})
	}
}

func TestReplayReconciliationAndResume(t *testing.T) {
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
			a := newTestAgent(t, fixedResponseProvider{Response{Message: msg, StopReason: tc.stop}}, AgentConfig{Tools: []Tool{Func("tool", "", func(context.Context, struct{}) (string, error) { t.Error("executed incomplete call"); return "", nil })}})
			r, e := a.Run(context.Background(), "go")
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
			next := newTestAgent(t, invokeFunc(func(_ context.Context, req Request) (Response, error) {
				if e := validateHistory(req.Messages); e != nil {
					t.Error(e)
				}
				return fixtureResponse(asstText("continued")), nil
			}), AgentConfig{})
			s, e := next.ResumeSession(rr.Messages)
			if e != nil {
				t.Fatal(e)
			}
			r.Messages[1].Blocks[0] = TextBlock{Text: "changed"}
			snap := s.Messages()
			snap[1].Blocks[0] = TextBlock{Text: "changed again"}
			if _, e = s.Run(context.Background(), "next"); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestResumeValidationLocations(t *testing.T) {
	a := newTestAgent(t, &scriptedProvider{}, AgentConfig{})
	for _, messages := range [][]Message{{asstTool("a", "tool", `{}`)}, {ToolResultMessage("missing", "ok", false)}, {asstTool("a", "tool", `{"x":`)}} {
		if _, e := a.ResumeSession(messages); e == nil || !strings.Contains(e.Error(), "message 0") {
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
	r, e := a.Run(context.Background(), "go")
	if e != nil || r.Turns != 1 || r.ProviderAttempts != 2 || r.Usage.InputTokens != 3 {
		t.Fatalf("%+v %v", r, e)
	}
	a = newTestAgent(t, &scriptedProvider{turns: []Message{asstText("earlier")}}, AgentConfig{})
	s := a.NewSession()
	_, _ = s.Run(context.Background(), "first")
	r, e = s.Run(context.Background(), "second")
	if e == nil || r.Output != "" || r.FinalMessage.Role != "" || r.ProviderStopReason != "" {
		t.Fatalf("stale output %+v %v", r, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a = newTestAgent(t, invokeFunc(func(context.Context, Request) (Response, error) {
		cancel()
		return fixtureResponse(asstText("partial")), nil
	}), AgentConfig{})
	r, e = a.Run(ctx, "go")
	if !errors.Is(e, context.Canceled) || r.Status != RunCancelled || r.Output != "partial" {
		t.Fatalf("%+v %v", r, e)
	}
}

type retryableError struct{}

func (*retryableError) Error() string   { return "retry" }
func (*retryableError) Retryable() bool { return true }
