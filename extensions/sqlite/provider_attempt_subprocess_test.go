package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// attemptProvider asks for one lookup, then answers. In the killed owner its
// second call signals inFlight and never returns.
type attemptProvider struct {
	calls    atomic.Int32
	inOwner  bool
	inFlight chan struct{}
}

func (p *attemptProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	call := p.calls.Add(1)
	if p.inOwner && call == 1 {
		return core.Response{
			Message: withAttemptUsage(core.AssistantMessage(core.ToolUseBlock{
				ID: "lookup-1", Name: "lookup", Input: json.RawMessage(`{}`),
			}), 7),
			StopReason: core.StopToolUse,
		}, nil
	}
	if p.inOwner {
		close(p.inFlight)
		for {
			time.Sleep(time.Hour) // Killed while holding the request.
		}
	}
	return core.Response{Message: withAttemptUsage(core.AssistantMessage(core.TextBlock{Text: "answered"}), 3), StopReason: core.StopEndTurn}, nil
}

func withAttemptUsage(message core.Message, tokens int) core.Message {
	message.Usage = &core.Usage{InputTokens: tokens}
	return message
}

func attemptAgent(provider core.Provider) (*core.Agent, error) {
	lookup := core.WithToolEffectPolicy(core.Func("lookup", "look something up", func(context.Context, struct{}) (string, error) {
		return "found", nil
	}), core.ToolEffectPolicy{Kind: core.ToolEffectReadOnly})
	return core.New(provider, core.AgentConfig{Tools: []core.Tool{lookup}})
}

func runProviderAttemptOwner() {
	ctx := context.Background()
	store, err := Open(ctx, os.Getenv("AUTOMATA_SQLITE_TEST_PATH"))
	if err != nil {
		fmt.Println("open-error: " + err.Error())
		os.Exit(3)
	}
	runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
	if err != nil {
		fmt.Println("runtime-error: " + err.Error())
		os.Exit(3)
	}
	provider := &attemptProvider{inOwner: true, inFlight: make(chan struct{})}
	agent, err := attemptAgent(provider)
	if err == nil {
		err = runtime.Register("agent", "v1", agent)
	}
	if err != nil {
		fmt.Println("agent-error: " + err.Error())
		os.Exit(3)
	}
	handle, err := runtime.Submit(ctx, "agent", "v1", "look it up", core.SubmitOptions{Scope: "subprocess", Key: "attempt"})
	if err != nil {
		fmt.Println("submit-error: " + err.Error())
		os.Exit(3)
	}
	select {
	case <-provider.inFlight:
	case <-time.After(10 * time.Second):
		fmt.Println("never-in-flight")
		os.Exit(3)
	}
	fmt.Println("in-flight " + handle.ID())
	for {
		time.Sleep(time.Hour) // Killed while the second provider attempt is in flight.
	}
}

// Killing the owner while a provider attempt is in flight, after a committed
// tool batch, never sends that request again without an explicit policy: the
// reopened run needs provider attention with one unknown attempt and only its
// recorded usage. With a fresh-attempt bound, recovery starts exactly one
// recorded fresh attempt, and usage still counts only responses that
// arrived.
func TestInterruptedProviderAttemptIsNeverSilentlyRepeated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock-based ownership is unsupported on windows")
	}
	path := filepath.Join(t.TempDir(), "runtime.sqlite")
	cmd, scanner := startSubprocess(t, "provider-attempt-owner", path)
	runID := waitSubprocessLine(t, scanner, "in-flight ")[len("in-flight "):]
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	ctx := context.Background()
	reopen := func(maxFresh int) (*core.Runtime, *attemptProvider) {
		t.Helper()
		store, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		reopened, err := core.NewRuntime(ctx, core.RuntimeConfig{
			Store: store, ProviderRecovery: core.ProviderRecoveryPolicy{MaxFreshAttempts: maxFresh},
		})
		if err != nil {
			t.Fatal(err)
		}
		provider := &attemptProvider{}
		agent, err := attemptAgent(provider)
		if err != nil {
			t.Fatal(err)
		}
		if err := reopened.Register("agent", "v1", agent); err != nil {
			t.Fatal(err)
		}
		if err := reopened.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		return reopened, provider
	}

	conservative, provider := reopen(0)
	handle := conservative.Handle(runID)
	if _, err := handle.Await(ctx); !errors.Is(err, core.ErrRunNeedsAttention) {
		t.Fatalf("await without policy = %v, want ErrRunNeedsAttention", err)
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Attention == nil || snapshot.Attention.Kind != core.AttentionProvider || snapshot.Accounting.UnknownAttempts != 1 || snapshot.Accounting.FreshAttempts != 0 ||
		snapshot.Result.Usage.InputTokens != 7 || provider.calls.Load() != 0 {
		t.Fatalf("without policy: attention %#v unknown %d fresh %d usage %d provider calls %d",
			snapshot.Attention, snapshot.Accounting.UnknownAttempts, snapshot.Accounting.FreshAttempts, snapshot.Result.Usage.InputTokens, provider.calls.Load())
	}
	if err := conservative.Close(); err != nil {
		t.Fatal(err)
	}

	authorized, provider := reopen(1)
	t.Cleanup(func() { _ = authorized.Close() })
	result, err := authorized.Handle(runID).Await(ctx)
	if err != nil || result.Output != "answered" {
		t.Fatalf("fresh attempt = %#v, %v", result, err)
	}
	snapshot, err = authorized.Handle(runID).Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 1 || snapshot.Accounting.FreshAttempts != 1 || snapshot.Accounting.UnknownAttempts != 1 || result.Usage.InputTokens != 10 {
		t.Fatalf("with policy: provider calls %d fresh %d unknown %d usage %d; want 1, 1, 1, 10",
			provider.calls.Load(), snapshot.Accounting.FreshAttempts, snapshot.Accounting.UnknownAttempts, result.Usage.InputTokens)
	}
}
