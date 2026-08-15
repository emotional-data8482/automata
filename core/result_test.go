package core

import (
	"context"
	"errors"
	"testing"
)

// optionsProvider records the CallOptions it was invoked with and replies with
// a scripted message, so tests can assert option merging and inspect requests.
type optionsProvider struct {
	turns []Message
	calls int
	seen  []CallOptions
}

type fixedResponseProvider struct{ response Response }

func (p fixedResponseProvider) Invoke(context.Context, Request) (Response, error) {
	return p.response, nil
}

func (p *optionsProvider) Invoke(_ context.Context, req Request) (Response, error) {
	p.seen = append(p.seen, req.Options)
	if p.calls >= len(p.turns) {
		return Response{}, errors.New("no script")
	}
	m := p.turns[p.calls]
	p.calls++
	reason := StopEndTurn
	if len(m.ToolUses()) > 0 {
		reason = StopToolUse
	}
	return Response{Message: m, StopReason: reason}, nil
}

func floatPtr(f float64) *float64 { return &f }

// TestRunResultFields pins the RunResult of a normal multi-turn run: output,
// final message, summed usage, step count, and StopEndTurn.
func TestRunResultFields(t *testing.T) {
	provider := &optionsProvider{turns: []Message{
		withUsage(asstTool("c1", "echo", `{"msg":"hi"}`), &Usage{InputTokens: 10, OutputTokens: 5}),
		withUsage(asstText("all done"), &Usage{InputTokens: 8, OutputTokens: 3}),
	}}
	agent := New(provider)
	agent.RegisterTool(Func("echo", "echoes", func(_ context.Context, a echoArgs) (string, error) {
		return "echoed:" + a.Msg, nil
	}))

	res, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "all done" {
		t.Errorf("Output = %q, want %q", res.Output, "all done")
	}
	if res.FinalMessage.Text() != "all done" {
		t.Errorf("FinalMessage.Text() = %q, want %q", res.FinalMessage.Text(), "all done")
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d, want 2", res.Steps)
	}
	if res.Usage != (Usage{InputTokens: 18, OutputTokens: 8}) {
		t.Errorf("Usage = %+v, want summed {18 8}", res.Usage)
	}
	if res.StopReason != StopEndTurn {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopEndTurn)
	}
	// The transcript is present and ends at the final assistant reply.
	if len(res.Messages) == 0 || res.Messages[len(res.Messages)-1].Role != "assistant" {
		t.Errorf("Messages = %+v, want a transcript ending in an assistant turn", res.Messages)
	}
}

// TestRunResultMaxStepsReturnsPartials pins that hitting the step budget returns
// the error AND a populated result (partial transcript, usage, StopMaxSteps).
func TestRunResultMaxStepsReturnsPartials(t *testing.T) {
	provider := &optionsProvider{turns: []Message{
		withUsage(asstTool("c1", "echo", "{}"), &Usage{InputTokens: 4, OutputTokens: 2}),
	}}
	agent := New(provider).WithMaxSteps(1)
	agent.RegisterTool(Func("echo", "echoes", func(_ context.Context, _ echoArgs) (string, error) {
		return "ok", nil
	}))

	res, err := agent.Run(context.Background(), "go")
	if !errors.Is(err, ErrMaxStepsExceeded) {
		t.Fatalf("err = %v, want ErrMaxStepsExceeded", err)
	}
	if res.StopReason != StopMaxSteps {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopMaxSteps)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1", res.Steps)
	}
	if res.Usage != (Usage{InputTokens: 4, OutputTokens: 2}) {
		t.Errorf("Usage = %+v, want partial {4 2}", res.Usage)
	}
	// Partial transcript survives: task, assistant tool call, tool result.
	if got := roles(res.Messages); len(got) != 3 {
		t.Errorf("partial transcript roles = %v, want 3 entries", got)
	}
}

func TestCompletionFailuresReturnPartialResults(t *testing.T) {
	tests := []struct {
		name       string
		reason     StopReason
		raw        string
		text       string
		wantIs     error
		wantOutput string
	}{
		{
			name:       "token limit",
			reason:     StopTokenLimit,
			raw:        "length",
			text:       "partial answer",
			wantIs:     ErrTokenLimit,
			wantOutput: "partial answer",
		},
		{
			name:   "filtered empty response",
			reason: StopContentFilter,
			raw:    "content_filter",
			wantIs: ErrContentFiltered,
		},
		{
			name:       "unknown provider reason",
			reason:     StopUnknown,
			raw:        "future_reason",
			text:       "not known to be final",
			wantIs:     ErrUnknownStopReason,
			wantOutput: "not known to be final",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := fixedResponseProvider{response: Response{
				Message:       asstText(tt.text),
				StopReason:    tt.reason,
				RawStopReason: tt.raw,
			}}
			res, err := New(provider).Run(context.Background(), "go")
			if !errors.Is(err, tt.wantIs) {
				t.Fatalf("err = %v, want errors.Is(_, %v)", err, tt.wantIs)
			}
			var completionErr *CompletionError
			if !errors.As(err, &completionErr) {
				t.Fatalf("err type = %T, want *CompletionError", err)
			}
			if completionErr.Reason != tt.reason || completionErr.RawReason != tt.raw {
				t.Errorf("CompletionError = %+v, want reason %q / raw %q", completionErr, tt.reason, tt.raw)
			}
			if res.Output != tt.wantOutput {
				t.Errorf("Output = %q, want %q", res.Output, tt.wantOutput)
			}
			if res.StopReason != tt.reason || res.RawStopReason != tt.raw {
				t.Errorf("result reasons = %q / %q, want %q / %q", res.StopReason, res.RawStopReason, tt.reason, tt.raw)
			}
			if res.Steps != 1 || len(res.Messages) == 0 || res.Messages[len(res.Messages)-1].Role != "assistant" {
				t.Errorf("partial result lost progress: Steps=%d Messages=%+v", res.Steps, res.Messages)
			}
		})
	}
}

func TestUnrecognizedResponseReasonDoesNotBecomeSuccess(t *testing.T) {
	provider := fixedResponseProvider{response: Response{
		Message:    asstText("looks complete"),
		StopReason: StopReason("brand_new_reason"),
	}}
	res, err := New(provider).Run(context.Background(), "go")
	if !errors.Is(err, ErrUnknownStopReason) {
		t.Fatalf("err = %v, want ErrUnknownStopReason", err)
	}
	if res.StopReason != StopUnknown || res.RawStopReason != "brand_new_reason" {
		t.Errorf("result reasons = %q / %q, want unknown / brand_new_reason", res.StopReason, res.RawStopReason)
	}
}

// TestDefaultCallOptionsSent pins that agent default CallOptions reach the
// provider on every turn.
func TestDefaultCallOptionsSent(t *testing.T) {
	provider := &optionsProvider{turns: []Message{asstText("done")}}
	agent := New(provider).WithDefaultCallOptions(CallOptions{
		Temperature: floatPtr(0.2),
		MaxTokens:   1024,
	})

	if _, err := agent.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.seen) != 1 {
		t.Fatalf("provider saw %d turns, want 1", len(provider.seen))
	}
	got := provider.seen[0]
	if got.Temperature == nil || *got.Temperature != 0.2 || got.MaxTokens != 1024 {
		t.Errorf("sent options = %+v, want temp 0.2 / max 1024", got)
	}
}

// TestWithCallOptionsMergesOverDefault pins that a per-run override merges over
// the agent default field-by-field: overridden fields change, others persist.
func TestWithCallOptionsMergesOverDefault(t *testing.T) {
	provider := &optionsProvider{turns: []Message{asstText("done")}}
	agent := New(provider).WithDefaultCallOptions(CallOptions{
		Temperature: floatPtr(0.2),
		MaxTokens:   1024,
	})

	if _, err := agent.Run(context.Background(), "go",
		WithCallOptions(CallOptions{MaxTokens: 4096}), // override only MaxTokens
	); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := provider.seen[0]
	if got.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want overridden 4096", got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Errorf("Temperature = %v, want default 0.2 preserved", got.Temperature)
	}
}

// TestCallOptionsMerge unit-tests the field-wise merge directly.
func TestCallOptionsMerge(t *testing.T) {
	base := CallOptions{
		Temperature:    floatPtr(0.5),
		MaxTokens:      100,
		StopSequences:  []string{"a"},
		ThinkingBudget: 2048,
	}
	over := CallOptions{MaxTokens: 200, ThinkingBudget: 4096}
	got := base.merge(over)

	if got.MaxTokens != 200 || got.ThinkingBudget != 4096 {
		t.Errorf("override fields not applied: %+v", got)
	}
	if got.Temperature == nil || *got.Temperature != 0.5 || len(got.StopSequences) != 1 {
		t.Errorf("untouched fields not preserved: %+v", got)
	}
	// The base value is not mutated.
	if base.MaxTokens != 100 {
		t.Errorf("merge mutated the receiver: %+v", base)
	}
}
