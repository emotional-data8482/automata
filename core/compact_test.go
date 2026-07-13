package core

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// countingSummarizer is a Provider that returns a fixed summary and counts how
// many times it was invoked.
type countingSummarizer struct {
	mu    sync.Mutex
	calls int
	reply string
}

func (p *countingSummarizer) Invoke(_ context.Context, _ Request) (Response, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return Response{Message: asstText(p.reply)}, nil
}

func (p *countingSummarizer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// bigText returns a string long enough to push the char-based estimate over a
// threshold.
func bigText(n int) string { return strings.Repeat("x", n) }

// TestCompactorBelowThresholdIsNoOp pins that under the trigger the hook returns
// the messages unchanged and never calls the summarizer.
func TestCompactorBelowThresholdIsNoOp(t *testing.T) {
	sum := &countingSummarizer{reply: "SUMMARY"}
	hook := Compactor(sum, CompactorConfig{TriggerTokens: 1_000_000, KeepRecent: 2})

	msgs := []Message{SystemMessage("sys"), UserMessage("hi"), asstText("hello")}
	out, _, err := hook(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if len(out) != len(msgs) {
		t.Errorf("out len = %d, want unchanged %d", len(out), len(msgs))
	}
	if sum.count() != 0 {
		t.Errorf("summarizer called %d times below threshold, want 0", sum.count())
	}
}

// TestCompactorSummarizesOlderTurns pins the happy path: over the threshold, the
// hook keeps the system prompt and recent turns and replaces the middle with a
// summary.
func TestCompactorSummarizesOlderTurns(t *testing.T) {
	sum := &countingSummarizer{reply: "SUMMARY-TEXT"}
	hook := Compactor(sum, CompactorConfig{TriggerTokens: 1, KeepRecent: 2})

	msgs := []Message{
		SystemMessage("sys"),
		UserMessage("q1"),
		asstText("a1"),
		UserMessage("q2"),
		asstText("a2"),
		UserMessage("q3"),
		asstText("a3"),
	}
	out, _, err := hook(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if sum.count() != 1 {
		t.Fatalf("summarizer called %d times, want 1", sum.count())
	}
	// System prompt preserved at the front.
	if out[0].Role != "system" || out[0].Text() != "sys" {
		t.Errorf("out[0] = %+v, want the system message", out[0])
	}
	// A summary appears, carrying the summarizer's text.
	joined := ""
	for _, m := range out {
		joined += m.Role + ":" + m.Text() + "|"
	}
	if !strings.Contains(joined, "SUMMARY-TEXT") {
		t.Errorf("compacted view missing summary text: %s", joined)
	}
	// The recent window is preserved: the last message verbatim, and q3's text
	// survives (folded into the summary-carrying user message to keep roles
	// alternating).
	if out[len(out)-1].Text() != "a3" {
		t.Errorf("last message = %q, want a3", out[len(out)-1].Text())
	}
	if !strings.Contains(out[len(out)-2].Text(), "q3") {
		t.Errorf("q3 not preserved in the compacted view: %q", out[len(out)-2].Text())
	}
	// Roles must alternate for the non-system messages (no two users in a row).
	for i := systemPrefixLen(out); i < len(out)-1; i++ {
		if out[i].Role == "user" && out[i+1].Role == "user" {
			t.Errorf("adjacent user messages at %d: %+v", i, out[i:i+2])
		}
	}
}

// TestCompactorNeverSplitsToolPair pins the invariant: the summarized/kept
// boundary never separates an assistant tool_use from its tool_result.
func TestCompactorNeverSplitsToolPair(t *testing.T) {
	sum := &countingSummarizer{reply: "S"}
	// KeepRecent lands the cut in the middle of a tool_use/tool_result pair; the
	// hook must advance it so the pair stays together.
	hook := Compactor(sum, CompactorConfig{TriggerTokens: 1, KeepRecent: 1})

	msgs := []Message{
		SystemMessage("sys"),
		UserMessage("go"),
		AssistantMessage(toolUse("t1", "search", "{}")),
		ToolResultMessage("t1", "found", false), // KeepRecent=1 would start the suffix here — a split
	}
	out, _, err := hook(context.Background(), msgs, nil)
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	// No tool-result message may appear without its tool_use earlier in the view.
	seenToolUse := map[string]bool{}
	for _, m := range out {
		for _, tu := range m.ToolUses() {
			seenToolUse[tu.ID] = true
		}
		for _, blk := range m.Blocks {
			if tr, ok := blk.(ToolResultBlock); ok && !seenToolUse[tr.ToolUseID] {
				t.Errorf("tool_result %q appears before its tool_use in the compacted view", tr.ToolUseID)
			}
		}
	}
}

// TestCompactorMemoizesSummary pins that the summary is reused across hook calls
// within the recompute margin, so the summarizer is not called every turn.
func TestCompactorMemoizesSummary(t *testing.T) {
	sum := &countingSummarizer{reply: "S"}
	hook := Compactor(sum, CompactorConfig{TriggerTokens: 1, KeepRecent: 2, MinRecompute: 100})

	base := []Message{
		SystemMessage("sys"),
		UserMessage("q1"),
		asstText("a1"),
		UserMessage("q2"),
		asstText("a2"),
	}
	// First call summarizes.
	if _, _, err := hook(context.Background(), base, nil); err != nil {
		t.Fatalf("hook 1: %v", err)
	}
	// A couple more turns accrue, still within MinRecompute.
	grown := append(append([]Message(nil), base...), UserMessage("q3"), asstText("a3"))
	if _, _, err := hook(context.Background(), grown, nil); err != nil {
		t.Fatalf("hook 2: %v", err)
	}
	if sum.count() != 1 {
		t.Errorf("summarizer called %d times, want 1 (memoized within margin)", sum.count())
	}
}

// TestCompactorConcurrentSafe runs the hook from many goroutines to catch data
// races on the shared cache (run under -race).
func TestCompactorConcurrentSafe(t *testing.T) {
	sum := &countingSummarizer{reply: "S"}
	hook := Compactor(sum, CompactorConfig{TriggerTokens: 1, KeepRecent: 2})

	msgs := []Message{
		SystemMessage("sys"),
		UserMessage(bigText(50)),
		asstText("a1"),
		UserMessage("q2"),
		asstText("a2"),
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := hook(context.Background(), msgs, nil); err != nil {
				t.Errorf("hook: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestEstimateTokensPrefersUsage pins that the estimate uses reported usage when
// present, and the char heuristic otherwise.
func TestEstimateTokensPrefersUsage(t *testing.T) {
	withU := []Message{withUsage(asstText("hi"), &Usage{InputTokens: 100, OutputTokens: 20})}
	if got := estimateTokens(withU); got != 120 {
		t.Errorf("estimate with usage = %d, want 120", got)
	}
	noU := []Message{UserMessage(bigText(400))}
	if got := estimateTokens(noU); got < 50 {
		t.Errorf("char estimate = %d, want a nontrivial count", got)
	}
}

func systemPrefixLen(messages []Message) int {
	n := 0
	for n < len(messages) && messages[n].Role == "system" {
		n++
	}
	return n
}
