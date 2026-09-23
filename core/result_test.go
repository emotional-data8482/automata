package core

import (
	"context"
	"errors"
	"strings"
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
	agent := testAgent(provider)
	agent.RegisterTool(Func("echo", "echoes", func(_ context.Context, a echoArgs) (string, error) {
		return "echoed:" + a.Msg, nil
	}))

	res, err := runAgent(t, agent, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "all done" {
		t.Errorf("Output = %q, want %q", res.Output, "all done")
	}
	if res.FinalMessage.Text() != "all done" {
		t.Errorf("FinalMessage.Text() = %q, want %q", res.FinalMessage.Text(), "all done")
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
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

// TestRunResultMaxTurnsReturnsPartials pins that hitting the turn budget returns
// the error AND a populated result (partial transcript, usage, StopMaxTurns).
func TestRunResultMaxTurnsReturnsPartials(t *testing.T) {
	provider := &optionsProvider{turns: []Message{
		withUsage(asstTool("c1", "echo", "{}"), &Usage{InputTokens: 4, OutputTokens: 2}),
	}}
	agent := testAgent(provider).WithMaxTurns(1)
	agent.RegisterTool(Func("echo", "echoes", func(_ context.Context, _ echoArgs) (string, error) {
		return "ok", nil
	}))

	res, err := runAgent(t, agent, "go")
	if !errors.Is(err, ErrMaxTurnsExceeded) || !strings.Contains(err.Error(), "exceeded max turns (1)") {
		t.Fatalf("err = %v, want ErrMaxTurnsExceeded with effective limit 1", err)
	}
	if res.StopReason != StopMaxTurns {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopMaxTurns)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
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
			res, err := runAgent(t, testAgent(provider), "go")
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
			if res.Turns != 1 || len(res.Messages) == 0 || res.Messages[len(res.Messages)-1].Role != "assistant" {
				t.Errorf("partial result lost progress: Turns=%d Messages=%+v", res.Turns, res.Messages)
			}
		})
	}
}

func TestUnrecognizedResponseReasonDoesNotBecomeSuccess(t *testing.T) {
	provider := fixedResponseProvider{response: Response{
		Message:    asstText("looks complete"),
		StopReason: StopReason("brand_new_reason"),
	}}
	res, err := runAgent(t, testAgent(provider), "go")
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
	agent := testAgent(provider).WithCallOptions(CallOptions{
		Temperature: floatPtr(0.2),
		MaxTokens:   1024,
	})

	if _, err := runAgent(t, agent, "go"); err != nil {
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
