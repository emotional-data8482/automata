package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/core/coretest"
)

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Docs.Example.com":                "docs.example.com",
		" docs.example.com. ":             "docs.example.com",
		"https://docs.example.com:8443/a": "docs.example.com",
		"localhost:3000":                  "localhost",
		"127.0.0.1":                       "127.0.0.1",
	} {
		if got, err := normalizeHost(in); err != nil || got != want {
			t.Errorf("normalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "*.example.com", "*", "user@evil", "a b.com"} {
		if got, err := normalizeHost(bad); err == nil {
			t.Errorf("normalizeHost(%q) = %q, want an error", bad, got)
		}
	}
}

// accessSession is a Session without a browser: enough for the access tool
// and the allowed set.
func accessSession(cfg Config) *Session {
	return &Session{cfg: cfg, hosts: cfg.AllowedHosts, refs: newRefRegistry()}
}

func execAccess(t *testing.T, s *Session, host, reason string) core.ToolResult {
	t.Helper()
	raw, _ := json.Marshal(accessParams{Host: host, Reason: reason})
	r, err := s.requestAccess().Execute(t.Context(), raw)
	if err != nil {
		t.Fatalf("request access: fatal error %v", err)
	}
	return r
}

func TestRequestAccessCallback(t *testing.T) {
	var asked []string
	s := accessSession(Config{AskAccess: func(_ context.Context, host, reason string) (bool, error) {
		asked = append(asked, host+": "+reason)
		switch host {
		case "docs.example.com":
			return true, nil
		case "broken.example.com":
			return false, errors.New("prompt closed")
		}
		return false, nil
	}})

	if r := execAccess(t, s, "https://Docs.Example.com/guide", "read the guide"); r.IsError {
		t.Fatalf("allowed host: %s", r.Text())
	}
	if err := s.checkURL("https://docs.example.com/x"); err != nil {
		t.Fatalf("approved host must be allowed: %v", err)
	}
	if r := execAccess(t, s, "evil.example.com", "why not"); !r.IsError || r.Effect.Status != core.EffectNotApplied || !strings.Contains(r.Text(), "denied") {
		t.Fatalf("denied host: %+v", r)
	}
	if r := execAccess(t, s, "broken.example.com", "x"); !r.IsError || !strings.Contains(r.Text(), "prompt closed") {
		t.Fatalf("callback error: %+v", r)
	}
	if r := execAccess(t, s, "*.example.com", "everything"); !r.IsError || !strings.Contains(r.Text(), "wildcards") {
		t.Fatalf("wildcard: %+v", r)
	}
	for _, host := range []string{"evil.example.com", "broken.example.com"} {
		if err := s.checkURL("https://" + host + "/"); err == nil {
			t.Errorf("%s must stay blocked", host)
		}
	}
	want := []string{"docs.example.com: read the guide", "evil.example.com: why not", "broken.example.com: x"}
	if fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Errorf("asked %q, want %q", asked, want)
	}
}

func TestRequestAccessCallbackSerialized(t *testing.T) {
	var active, peak atomic.Int32
	var asked []string // application-owned callback state, not synchronized
	s := accessSession(Config{AskAccess: func(_ context.Context, host, _ string) (bool, error) {
		n := active.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		asked = append(asked, host)
		time.Sleep(time.Millisecond)
		active.Add(-1)
		return true, nil
	}})
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			<-start
			r := execAccess(t, s, fmt.Sprintf("h%d.test", i), "read")
			if r.IsError {
				t.Errorf("access request: %s", r.Text())
			}
		})
	}
	close(start)
	wg.Wait()
	if peak.Load() != 1 || len(asked) != 16 {
		t.Fatalf("concurrent callbacks = %d, calls = %d; want 1 and 16", peak.Load(), len(asked))
	}
}

func TestRequestAccessClosedOrCanceled(t *testing.T) {
	for _, name := range []string{"closed", "canceled", "canceled during callback"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			asked := false
			s := accessSession(Config{AskAccess: func(context.Context, string, string) (bool, error) {
				asked = true
				cancel()
				return true, nil
			}})
			s.closed = name == "closed"
			if name == "canceled" {
				cancel()
			}
			raw, _ := json.Marshal(accessParams{Host: "example.com"})
			r, err := s.requestAccess().Execute(ctx, raw)
			if name == "closed" {
				if err != nil || !r.IsError || r.Effect.Status != core.EffectNotApplied || !strings.Contains(r.Text(), "closed") {
					t.Fatalf("closed session: result=%+v error=%v", r, err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error = %v", err)
			}
			if asked != (name == "canceled during callback") {
				t.Fatalf("asked = %v for %s", asked, name)
			}
			if hostAllowed(s.hosts, "example.com") {
				t.Fatal("closed or canceled request granted access")
			}
		})
	}
}

// Outside Runtime and without AskAccess nobody approved the call, so the
// tool must refuse instead of granting itself access.
func TestRequestAccessWithoutDecider(t *testing.T) {
	s := accessSession(Config{})
	r := execAccess(t, s, "docs.example.com", "read")
	if !r.IsError || r.Effect.Status != core.EffectNotApplied || !strings.Contains(r.Text(), "core.Runtime") || !strings.Contains(r.Text(), "AskAccess") {
		t.Fatalf("no decider: %+v", r)
	}
	if err := s.checkURL("https://docs.example.com/"); err == nil {
		t.Fatal("host must not be allowed without a decision")
	}
}

func TestToolsOfferRequestAccessOnlyWhenEnabled(t *testing.T) {
	names := func(s *Session) string {
		var out []string
		for _, tool := range s.Tools() {
			out = append(out, tool.Definition().Name)
		}
		return strings.Join(out, ",")
	}
	ask := func(context.Context, string, string) (bool, error) { return true, nil }
	for _, c := range []struct {
		cfg  Config
		want bool
	}{{Config{}, false}, {Config{RequestAccess: true}, true}, {Config{AskAccess: ask}, true}} {
		if got := strings.Contains(names(accessSession(c.cfg)), requestAccessTool); got != c.want {
			t.Errorf("RequestAccess=%v AskAccess=%v: offered=%v, want %v", c.cfg.RequestAccess, c.cfg.AskAccess != nil, got, c.want)
		}
	}
}

func TestAllowedSetConcurrent(t *testing.T) {
	s := accessSession(Config{AllowedHosts: []string{"base.test"}})
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			host := fmt.Sprintf("h%d.test", i%8)
			s.allowHost(host)
			_ = s.checkURL("https://base.test/")
			s.recordBlocked("https://blocked.test/")
			_ = s.takeBlocked()
			if err := s.checkURL("https://" + host + "/"); err != nil {
				t.Errorf("%s: %v", host, err)
			}
		})
	}
	wg.Wait()
	if n := len(s.hosts); n != 9 {
		t.Fatalf("allowed set has %d hosts, want 9 (no duplicates): %v", n, s.hosts)
	}
}

// approvalRun drives a Runtime run whose model asks for access to the
// localhost fixture, answers the approval with decision, and then tries to
// navigate there. It returns the pending wait, the run output, and the
// transcript the model saw last.
func approvalRun(t *testing.T, decision core.Decision) (core.WaitSnapshot, string, string, *fixtures) {
	t.Helper()
	other := newFixtures(t, "")
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	s := newSession(t, Config{AllowedHosts: []string{"127.0.0.1"}, RequestAccess: true})

	provider := coretest.NewProvider(
		coretest.ToolCall("1", "browser_navigate", map[string]any{"url": otherURL + "/secret"}),
		coretest.ToolCall("2", "browser_request_access", map[string]any{"host": "localhost", "reason": "read the secret page"}),
		coretest.ToolCall("3", "browser_navigate", map[string]any{"url": otherURL + "/secret"}),
		coretest.Text("done"),
	)
	runtime := coretest.NewRuntime(t, core.RuntimeConfig{
		Authorizer: core.ApprovalAuthorizerFunc(func(context.Context, core.ApprovalAuthorization) error { return nil }),
	})
	agent := coretest.NewAgent(t, provider, core.AgentConfig{Tools: s.Tools(), MaxTurns: 6})
	def := coretest.Register(t, runtime, "browser", "v1", agent)
	handle, err := runtime.Submit(t.Context(), def, "read the secret page")
	if err != nil {
		t.Fatal(err)
	}

	wait := pendingWait(t, handle)
	err = handle.ResolveWait(t.Context(), wait.ID, core.WaitResolution{
		Decision: decision, Reason: "not today", Actor: "taylor", ActionDigest: wait.ActionDigest,
	})
	if err != nil {
		t.Fatalf("resolve wait: %v", err)
	}
	result, err := handle.Await(t.Context())
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	requests := provider.Requests()
	return wait, result.Output, fmt.Sprint(requests[len(requests)-1].Messages), other
}

func pendingWait(t *testing.T, handle *core.RunHandle) core.WaitSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var cursor uint64
	for {
		snapshot, err := handle.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, wait := range snapshot.Waits {
			if wait.State == core.WaitPending {
				return wait
			}
		}
		page, err := handle.WaitEvents(ctx, cursor, 64)
		if err != nil {
			t.Fatalf("no pending approval: %v", err)
		}
		cursor = page.Next
	}
}

func TestRequestAccessApprovedThroughRuntime(t *testing.T) {
	wait, output, transcript, other := approvalRun(t, core.Allow)
	if wait.Tool != "browser_request_access" || wait.Target != "localhost" ||
		wait.Prompt != "Allow the browser to visit localhost? Reason: read the secret page" {
		t.Fatalf("wait = tool %q target %q prompt %q", wait.Tool, wait.Target, wait.Prompt)
	}
	if output != "done" {
		t.Fatalf("output %q", output)
	}
	for _, want := range []string{`host "localhost" is not allowed; call browser_request_access`, "localhost is now allowed for this session.", "title: Secret"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript missing %q:\n%s", want, transcript)
		}
	}
	if n := other.hits.Load(); n != 1 {
		t.Fatalf("approved host received %d request(s), want 1", n)
	}
}

func TestRequestAccessDeniedThroughRuntime(t *testing.T) {
	_, output, transcript, other := approvalRun(t, core.Deny)
	if output != "done" {
		t.Fatalf("output %q", output)
	}
	if !strings.Contains(transcript, "denied: not today") || strings.Contains(transcript, "now allowed") {
		t.Errorf("deny should reach the model as an error:\n%s", transcript)
	}
	if n := other.hits.Load(); n != 0 {
		t.Fatalf("denied host received %d request(s)", n)
	}
}
