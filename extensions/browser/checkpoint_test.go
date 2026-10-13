package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/core/coretest"
)

func execCheckpoint(t *testing.T, s *Session, note string) core.ToolResult {
	t.Helper()
	raw, _ := json.Marshal(checkpointParams{Note: note})
	r, err := s.checkpoint().Execute(t.Context(), raw)
	if err != nil {
		t.Fatalf("checkpoint: fatal error %v", err)
	}
	return r
}

// Outside Runtime no user decision exists, so the tool must refuse rather
// than claim the user dealt with the challenge.
func TestCheckpointWithoutDecider(t *testing.T) {
	s := accessSession(Config{HumanCheckpoint: true})
	r := execCheckpoint(t, s, "image-grid CAPTCHA")
	if !r.IsError || r.Effect.Status != core.EffectNotApplied || !strings.Contains(r.Text(), "core.Runtime") {
		t.Fatalf("no decider: %+v", r)
	}
}

func TestCheckpointClosedOrCanceled(t *testing.T) {
	s := accessSession(Config{HumanCheckpoint: true})
	s.closed = true
	if r := execCheckpoint(t, s, "x"); !r.IsError || !strings.Contains(r.Text(), "closed") {
		t.Fatalf("closed session: %+v", r)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	raw, _ := json.Marshal(checkpointParams{Note: "x"})
	if _, err := s.checkpoint().Execute(ctx, raw); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestToolsOfferCheckpointOnlyWhenEnabled(t *testing.T) {
	var names []string
	for _, tool := range accessSession(Config{HumanCheckpoint: true}).Tools() {
		names = append(names, tool.Definition().Name)
	}
	if !strings.Contains(strings.Join(names, ","), checkpointTool) {
		t.Errorf("HumanCheckpoint set but %s not offered: %v", checkpointTool, names)
	}
	names = nil
	for _, tool := range accessSession(Config{}).Tools() {
		names = append(names, tool.Definition().Name)
	}
	if strings.Contains(strings.Join(names, ","), checkpointTool) {
		t.Errorf("HumanCheckpoint unset but %s offered: %v", checkpointTool, names)
	}
}

func TestNormalizeRejectsHeadlessCheckpoint(t *testing.T) {
	if _, err := normalize(Config{HumanCheckpoint: true}); err == nil {
		t.Fatal("headless HumanCheckpoint must be rejected")
	}
	if _, err := normalize(Config{HumanCheckpoint: true, Headed: true}); err != nil {
		t.Fatalf("headed HumanCheckpoint: %v", err)
	}
}

// checkpointRun drives a Runtime run whose model pauses at a challenge,
// answers the approval with decision, and then reads the page again.
func checkpointRun(t *testing.T, decision core.Decision) (core.WaitSnapshot, string, string) {
	t.Helper()
	fix := newFixtures(t, "")
	s := newSession(t, Config{AllowedHosts: []string{"127.0.0.1"}, HumanCheckpoint: true, Headed: true})

	provider := coretest.NewProvider(
		coretest.ToolCall("1", "browser_navigate", map[string]any{"url": fix.URL + "/form"}),
		coretest.ToolCall("2", "browser_checkpoint", map[string]any{"note": "image-grid CAPTCHA on the form"}),
		coretest.ToolCall("3", "browser_snapshot", map[string]any{}),
		coretest.Text("done"),
	)
	runtime := coretest.NewRuntime(t, core.RuntimeConfig{
		Authorizer: core.ApprovalAuthorizerFunc(func(context.Context, core.ApprovalAuthorization) error { return nil }),
	})
	agent := coretest.NewAgent(t, provider, core.AgentConfig{Tools: s.Tools(), MaxTurns: 6})
	def := coretest.Register(t, runtime, "browser", "v1", agent)
	handle, err := runtime.Submit(t.Context(), def, "fill the form")
	if err != nil {
		t.Fatal(err)
	}

	wait := pendingWait(t, handle)
	err = handle.ResolveWait(t.Context(), wait.ID, core.WaitResolution{
		Decision: decision, Reason: "not now", Actor: "taylor", ActionDigest: wait.ActionDigest,
	})
	if err != nil {
		t.Fatalf("resolve wait: %v", err)
	}
	result, err := handle.Await(t.Context())
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	requests := provider.Requests()
	return wait, result.Output, fmt.Sprint(requests[len(requests)-1].Messages)
}

func TestCheckpointApprovedThroughRuntime(t *testing.T) {
	wait, output, transcript := checkpointRun(t, core.Allow)
	if wait.Tool != checkpointTool || wait.Target != "image-grid CAPTCHA on the form" ||
		wait.Prompt != "The browser hit image-grid CAPTCHA on the form. Solve it in the visible browser window, then approve to continue." {
		t.Fatalf("wait = tool %q target %q prompt %q", wait.Tool, wait.Target, wait.Prompt)
	}
	if output != "done" {
		t.Fatalf("output %q", output)
	}
	if !strings.Contains(transcript, "The user says the challenge is dealt with. Take a fresh browser_snapshot") {
		t.Errorf("transcript missing the post-checkpoint advice:\n%s", transcript)
	}
}

func TestCheckpointDeniedThroughRuntime(t *testing.T) {
	_, output, transcript := checkpointRun(t, core.Deny)
	if output != "done" {
		t.Fatalf("output %q", output)
	}
	if !strings.Contains(transcript, "denied: not now") || strings.Contains(transcript, "challenge is dealt with") {
		t.Errorf("deny should reach the model as an error:\n%s", transcript)
	}
}
