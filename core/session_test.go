package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func roles(messages []Message) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
		out[i] = m.Role
	}
	return out
}

// TestSessionMultiTurn pins the core promise: the second run continues the
// first run's conversation, system prompt included.
func TestSessionMultiTurn(t *testing.T) {
	four, eight := "4", "8"
	provider := &capturingProvider{turns: []Message{
		{Role: "assistant", Content: &four},
		{Role: "assistant", Content: &eight},
	}}
	agent := New(provider).WithSystemPrompt("You are a calculator.")

	s := agent.NewSession()
	if out, err := s.Run(context.Background(), "2+2?"); err != nil || out != four {
		t.Fatalf("first run = %q, %v", out, err)
	}
	if out, err := s.Run(context.Background(), "double it"); err != nil || out != eight {
		t.Fatalf("second run = %q, %v", out, err)
	}

	want := []string{"system", "user", "assistant", "user"}
	got := roles(provider.received[1])
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("second turn saw roles %v, want %v", got, want)
	}
	if c := provider.received[1][2].Content; c == nil || *c != four {
		t.Errorf("second turn is missing the first assistant reply")
	}
}

// TestSessionTranscriptIncludesToolTurns verifies Messages() exposes the full
// conversation: system prompt, task, assistant tool calls, and tool results.
func TestSessionTranscriptIncludesToolTurns(t *testing.T) {
	done := "done"
	provider := &capturingProvider{turns: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Type: "function", Function: FunctionCall{Name: "echo", Arguments: `{"msg":"hi"}`}},
		}},
		{Role: "assistant", Content: &done},
	}}
	agent := New(provider).WithSystemPrompt("sys")
	agent.RegisterTool(Func("echo", "echoes msg", func(_ context.Context, a echoArgs) (string, error) {
		return "echoed:" + a.Msg, nil
	}))

	s := agent.NewSession()
	if _, err := s.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{"system", "user", "assistant", "tool", "assistant"}
	got := roles(s.Messages())
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("transcript roles = %v, want %v", got, want)
	}
}

// TestSessionTranscriptSurvivesError pins the audit story: a failed run still
// records everything up to the failure.
func TestSessionTranscriptSurvivesError(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Type: "function", Function: FunctionCall{Name: "echo", Arguments: `{"msg":"x"}`}},
		}},
	}}
	agent := New(provider).WithMaxSteps(1)
	agent.RegisterTool(Func("echo", "echoes msg", func(_ context.Context, a echoArgs) (string, error) {
		return "echoed:" + a.Msg, nil
	}))

	s := agent.NewSession()
	_, err := s.Run(context.Background(), "go")
	if !errors.Is(err, ErrMaxStepsExceeded) {
		t.Fatalf("err = %v, want ErrMaxStepsExceeded", err)
	}

	want := []string{"user", "assistant", "tool"}
	got := roles(s.Messages())
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("transcript after failure = %v, want %v", got, want)
	}
}

// TestSessionResumeRoundTrip persists a transcript through JSON and resumes it
// on a fresh Session: the next run sees the full history, and the system
// prompt is not duplicated.
func TestSessionResumeRoundTrip(t *testing.T) {
	first, second := "first", "second"
	provider := &capturingProvider{turns: []Message{
		{Role: "assistant", Content: &first},
		{Role: "assistant", Content: &second},
	}}
	agent := New(provider).WithSystemPrompt("sys")

	s := agent.NewSession()
	if _, err := s.Run(context.Background(), "one"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	blob, err := json.Marshal(s.Messages())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var transcript []Message
	if err := json.Unmarshal(blob, &transcript); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	resumed := agent.ResumeSession(transcript)
	if out, err := resumed.Run(context.Background(), "two"); err != nil || out != second {
		t.Fatalf("resumed run = %q, %v", out, err)
	}

	seen := provider.received[1]
	systems := 0
	for _, m := range seen {
		if m.Role == "system" {
			systems++
		}
	}
	if systems != 1 {
		t.Errorf("resumed run saw %d system messages, want 1", systems)
	}
	want := []string{"system", "user", "assistant", "user"}
	if got := roles(seen); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("resumed run saw roles %v, want %v", got, want)
	}
}

// TestSessionRunStream verifies the streaming variant continues history and
// delivers events like Agent.RunStream.
func TestSessionRunStream(t *testing.T) {
	hello, again := "hello", "again"
	provider := &capturingProvider{turns: []Message{
		{Role: "assistant", Content: &hello},
		{Role: "assistant", Content: &again},
	}}
	agent := New(provider)

	s := agent.NewSession()
	var texts []string
	if _, err := s.RunStream(context.Background(), "hi", func(ev StreamEvent) {
		if ev.Kind == StreamText {
			texts = append(texts, ev.Text)
		}
	}); err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if len(texts) != 1 || texts[0] != hello {
		t.Errorf("stream texts = %v, want [%q]", texts, hello)
	}

	if _, err := s.RunStream(context.Background(), "more", nil); err != nil {
		t.Fatalf("second RunStream: %v", err)
	}
	want := []string{"user", "assistant", "user", "assistant"}
	if got := roles(s.Messages()); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("transcript roles = %v, want %v", got, want)
	}
}
