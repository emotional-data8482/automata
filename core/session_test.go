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
		asstText(four),
		asstText(eight),
	}}
	agent := New(provider).WithSystemPrompt("You are a calculator.")

	s := agent.NewSession()
	if out, err := s.Run(context.Background(), "2+2?"); err != nil || out.Output != four {
		t.Fatalf("first run = %q, %v", out.Output, err)
	}
	if out, err := s.Run(context.Background(), "double it"); err != nil || out.Output != eight {
		t.Fatalf("second run = %q, %v", out.Output, err)
	}

	want := []string{"system", "user", "assistant", "user"}
	got := roles(provider.received[1])
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("second turn saw roles %v, want %v", got, want)
	}
	if provider.received[1][2].Text() != four {
		t.Errorf("second turn is missing the first assistant reply")
	}
}

// TestSessionTranscriptIncludesToolTurns verifies Messages() exposes the full
// conversation: system prompt, task, assistant tool calls, and tool results.
func TestSessionTranscriptIncludesToolTurns(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("c1", "echo", `{"msg":"hi"}`),
		asstText("done"),
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
		asstTool("c1", "echo", `{"msg":"x"}`),
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

// TestSessionResumesAfterParallelToolAbort proves a fatal tool in a parallel
// batch commits a structurally complete transcript. A second run with a fresh
// context can send that history back to the provider and continue normally.
func TestSessionResumesAfterParallelToolAbort(t *testing.T) {
	blockedStarted := make(chan struct{})
	provider := &capturingProvider{turns: []Message{
		AssistantMessage(
			toolUse("a1", "abort", `{}`),
			toolUse("b1", "blocked", `{}`),
		),
		asstText("resumed"),
	}}
	agent := New(provider)
	agent.RegisterTool(Func("abort", "aborts the batch", func(_ context.Context, _ struct{}) (string, error) {
		<-blockedStarted
		return "", context.Canceled
	}))
	agent.RegisterTool(Func("blocked", "waits for its sibling", func(ctx context.Context, _ struct{}) (string, error) {
		close(blockedStarted)
		<-ctx.Done()
		return "", ctx.Err()
	}))

	session := agent.NewSession()
	first, err := session.Run(context.Background(), "start")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first Run err = %v, want context.Canceled", err)
	}
	firstResults := transcriptToolResults(first.Messages)
	if len(firstResults) != 2 || firstResults[0].ToolUseID != "a1" || firstResults[1].ToolUseID != "b1" {
		t.Fatalf("first run results = %+v, want ordered results for a1 and b1", firstResults)
	}
	for _, result := range firstResults {
		if !result.IsError {
			t.Errorf("result for %q IsError=false, want true", result.ToolUseID)
		}
	}

	second, err := session.Run(context.Background(), "continue")
	if err != nil || second.Output != "resumed" {
		t.Fatalf("resumed Run = %q, %v", second.Output, err)
	}
	if len(provider.received) != 2 {
		t.Fatalf("provider received %d requests, want 2", len(provider.received))
	}

	// The resumed request contains one result for every prior assistant call,
	// followed by the new user turn. Providers can therefore accept the history.
	resumedHistory := provider.received[1]
	results := transcriptToolResults(resumedHistory)
	if len(results) != 2 || results[0].ToolUseID != "a1" || results[1].ToolUseID != "b1" {
		t.Errorf("resumed history results = %+v, want ordered a1/b1 results", results)
	}
	wantRoles := []string{"user", "assistant", "tool", "tool", "user"}
	if got := roles(resumedHistory); strings.Join(got, ",") != strings.Join(wantRoles, ",") {
		t.Errorf("resumed history roles = %v, want %v", got, wantRoles)
	}

	committed := session.Messages()
	if got := roles(committed); len(got) != 6 || got[len(got)-1] != "assistant" {
		t.Errorf("committed resumed transcript roles = %v, want six entries ending in assistant", got)
	}
}

// TestSessionResumeRoundTrip persists a transcript through JSON and resumes it
// on a fresh Session: the next run sees the full history, and the system
// prompt is not duplicated.
func TestSessionResumeRoundTrip(t *testing.T) {
	first, second := "first", "second"
	provider := &capturingProvider{turns: []Message{
		asstText(first),
		asstText(second),
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
	if out, err := resumed.Run(context.Background(), "two"); err != nil || out.Output != second {
		t.Fatalf("resumed run = %q, %v", out.Output, err)
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

// TestSessionPersistsEveryBlockType pins the compatibility surface: a
// transcript containing every block type — thinking (with signature), tool_use,
// tool_result (with an image), and a provider RawBlock — survives a JSON
// round-trip through ResumeSession with its blocks intact.
func TestSessionPersistsEveryBlockType(t *testing.T) {
	transcript := []Message{
		SystemMessage("sys"),
		UserMessage("do it"),
		{Role: "assistant", Blocks: Blocks{
			ThinkingBlock{Thinking: "reasoning", Signature: "sig-1"},
			RawBlock{Provider: "anthropic", Type: "redacted_thinking", Data: []byte(`{"data":"enc"}`)},
			TextBlock{Text: "calling the tool"},
			ToolUseBlock{ID: "t1", Name: "lookup", Input: []byte(`{"q":"x"}`)},
		}},
		{Role: "tool", Blocks: Blocks{ToolResultBlock{
			ToolUseID: "t1",
			Content:   Blocks{TextBlock{Text: "see figure"}, ImageBlock{MediaType: "image/png", URL: "u"}},
		}}},
	}

	agent := New(&capturingProvider{turns: []Message{asstText("final")}})
	blob, err := json.Marshal(agent.ResumeSession(transcript).Messages())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded []Message
	if err := json.Unmarshal(blob, &reloaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Re-marshal both and compare bytes: proves the blocks round-trip exactly.
	orig, _ := json.Marshal(transcript)
	round, _ := json.Marshal(reloaded)
	if string(orig) != string(round) {
		t.Errorf("transcript did not round-trip:\n orig:  %s\n round: %s", orig, round)
	}

	// Spot-check the resumed session still runs and continues the history.
	resumed := agent.ResumeSession(reloaded)
	if out, err := resumed.Run(context.Background(), "next"); err != nil || out.Output != "final" {
		t.Fatalf("resumed run = %q, %v", out.Output, err)
	}
}

// TestSessionRunStream verifies the streaming variant continues history and
// delivers events like Agent.RunStream.
func TestSessionRunStream(t *testing.T) {
	hello, again := "hello", "again"
	provider := &capturingProvider{turns: []Message{
		asstText(hello),
		asstText(again),
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
