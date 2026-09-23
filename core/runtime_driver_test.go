package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shortRepairDelay(t *testing.T) {
	t.Helper()
	previous := runtimeRepairDelay
	runtimeRepairDelay = 10 * time.Millisecond
	t.Cleanup(func() { runtimeRepairDelay = previous })
}

func awaitWithin(t *testing.T, handle *RunHandle, within time.Duration) (RunResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	result, err := handle.Await(ctx)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		t.Fatalf("Await did not return within %s", within)
	}
	return result, err
}

// Without any host Recover call, a waiting run whose logical deadline passes
// is finalized by the driver, and Await on it returns.
func TestRuntimeDriverFinalizesWaitingDeadlineWithoutRecover(t *testing.T) {
	runtime := newTestRuntime(t)
	provider := &scriptedProvider{turns: []Message{asstTool("q-1", "ask_user", `{"prompt":"region?"}`)}}
	if err := runtime.Register("asker", "v1", questionTestAgent(t, provider)); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "asker", "v1", "help", SubmitOptions{Deadline: time.Now().Add(50 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := awaitWithin(t, handle, 2*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("await past deadline = %v, want DeadlineExceeded", err)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeTerminal || snapshot.Waits[0].State != WaitCancelled {
		t.Fatalf("finalized run = %#v, %v", snapshot, err)
	}
}

func expiringApprovalAgent(t *testing.T, calls *atomic.Int64, expiresAfter time.Duration) *Agent {
	t.Helper()
	provider := &scriptedProvider{turns: []Message{asstTool("write-1", "write", `{}`), asstText("expired safely")}}
	tool := FuncResult("write", "write", func(context.Context, struct{}) (ToolResult, error) {
		calls.Add(1)
		return ToolResult{Blocks: Blocks{TextBlock{Text: "written"}}, Effect: EffectReport{Status: EffectApplied}}, nil
	})
	tool = WithToolEffectPolicy(tool, ToolEffectPolicy{Kind: ToolEffectMutating})
	tool = WithDurableWait(tool, DurableWaitPolicy{Kind: WaitApproval, ExpiresAfter: expiresAfter, Target: func(json.RawMessage) (string, error) { return "record-1", nil }})
	agent, err := New(provider, AgentConfig{Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// Without any host Recover call, an unanswered approval expires on time: the
// driver denies it, the run continues without dispatching the mutation, and a
// late answer is rejected as expired rather than as a conflict.
func TestRuntimeDriverExpiresWaitWithoutRecover(t *testing.T) {
	var calls atomic.Int64
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Authorizer: ApprovalAuthorizerFunc(func(context.Context, ApprovalAuthorization) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("writer", "v1", expiringApprovalAgent(t, &calls, 30*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "writer", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wait := waitForRuntimeWait(t, handle)
	result, err := awaitWithin(t, handle, 2*time.Second)
	if err != nil || result.Output != "expired safely" || calls.Load() != 0 {
		t.Fatalf("result = %#v, %v, dispatches = %d", result, err, calls.Load())
	}
	resolution := WaitResolution{Decision: Allow, ActionDigest: wait.ActionDigest}
	for range 2 {
		if err := handle.ResolveWait(context.Background(), wait.ID, resolution); !errors.Is(err, ErrWaitExpired) {
			t.Fatalf("late answer to a runtime-expired wait = %v, want ErrWaitExpired", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("expired approval dispatched %d times", calls.Load())
	}
}

// failChildConsumeStore fails, once, the transaction that consumes a child
// wait: the post-commit parent wake after a child terminalizes is lost.
type failChildConsumeStore struct {
	Store
	armed atomic.Bool
}

type failChildConsumeTx struct {
	StoreTransaction
	store *failChildConsumeStore
}

func (tx failChildConsumeTx) Put(bucket, key string, value []byte) error {
	if bucket == runtimeWaitsBucket && strings.Contains(string(value), `"state":"consumed"`) && tx.store.armed.CompareAndSwap(true, false) {
		return errors.New("injected lost parent wake")
	}
	return tx.StoreTransaction.Put(bucket, key, value)
}

func (s *failChildConsumeStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	return s.Store.Transaction(ctx, writable, func(tx StoreTransaction) error {
		if writable {
			tx = failChildConsumeTx{StoreTransaction: tx, store: s}
		}
		return fn(tx)
	})
}

// Without any host Recover call, a child completion whose parent wake was
// lost is consumed by the driver: the parent continues and the child is not
// rerun.
func TestRuntimeDriverRepairsLostChildWakeWithoutRecover(t *testing.T) {
	shortRepairDelay(t)
	store := &failChildConsumeStore{Store: NewMemoryStore()}
	store.armed.Store(true)
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	childProvider := &repeatingChildProvider{}
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	if err := runtime.Register("child", "v1", testAgent(childProvider)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := awaitWithin(t, handle, 2*time.Second)
	if err != nil || result.Output != "parent done" {
		t.Fatalf("parent after lost wake = %#v, %v", result, err)
	}
	if store.armed.Load() {
		t.Fatal("the parent wake fault never fired")
	}
	if got := childProvider.calls.Load(); got != 1 {
		t.Fatalf("child provider calls = %d, want 1", got)
	}
}

// Without any host Recover call, a lost wake from a grandchild that settles
// beneath a canceled (terminal) child is repaired through that child: the
// driver repeats the wake's climb to the root, which stops needing child
// attention and continues.
func TestRuntimeDriverRepairsLostWakeThroughTerminalChild(t *testing.T) {
	shortRepairDelay(t)
	grandchildProvider := newIgnoringCancelProvider()
	t.Cleanup(grandchildProvider.unblock)
	child := testAgent(&scriptedProvider{turns: []Message{
		asstTool("g1", "delegate2", `{"topic":"tea"}`),
		asstText("child done"),
	}})
	child.RegisterTool(DurableChildTool(childTestDefinition("delegate2"), DurableChildPolicy{DefinitionID: "grandchild", Revision: "v1"}))
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	store := &failChildConsumeStore{Store: NewMemoryStore()}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	for id, agent := range map[string]*Agent{"grandchild": testAgent(grandchildProvider), "child": child, "parent": parent} {
		if err := runtime.Register(id, "v1", agent); err != nil {
			t.Fatal(err)
		}
	}
	handle, err := runtime.Submit(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, grandchildProvider.started, "grandchild provider")
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childID := snapshot.ToolBatches[0].Invocations[0].ChildRunID
	if err := runtime.Handle(childID).Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	blocked := waitForRunState(t, runtime, handle.ID(), RuntimeNeedsAttention)
	if blocked.Attention == nil || blocked.Attention.Kind != AttentionChild || blocked.Attention.BlockingRunID != childID {
		t.Fatalf("parent attention = %#v, want child %s while its descendant settles", blocked.Attention, childID)
	}
	// Let the cancellation's own repair checks pass before losing the wake.
	time.Sleep(50 * time.Millisecond)
	store.armed.Store(true)
	grandchildProvider.unblock()
	waitForRunState(t, runtime, handle.ID(), RuntimeTerminal)
	result, err := handle.Await(context.Background())
	if err != nil || result.Output != "parent done" {
		t.Fatalf("parent after lost deep wake = %#v, %v", result, err)
	}
	if store.armed.Load() {
		t.Fatal("the parent wake fault never fired")
	}
	final, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if final.Accounting.Tree.Unsettled != 0 || final.Attention != nil {
		t.Fatalf("parent after settlement: attention %#v, unsettled descendants %d", final.Attention, final.Accounting.Tree.Unsettled)
	}
}

// A slow committed-run hook on one run the driver finalized must not hold
// the driver: another run's logical deadline still finalizes on time.
func TestRuntimeDriverSlowHookDoesNotDelayOtherDeadlines(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Hooks: []CommittedRunHook{{
		Name: "slow", Timeout: 5 * time.Second,
		Handle: func(ctx context.Context, _ RunSnapshot) error {
			if calls.Add(1) == 1 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	defer close(release)
	for _, name := range []string{"first", "second"} {
		provider := &scriptedProvider{turns: []Message{asstTool("q-1", "ask_user", `{"prompt":"region?"}`)}}
		if err := runtime.Register(name, "v1", questionTestAgent(t, provider)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := runtime.Submit(context.Background(), "first", "v1", "help", SubmitOptions{Deadline: time.Now().Add(100 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	waitForRuntimeWait(t, first)
	second, err := runtime.Submit(context.Background(), "second", "v1", "help", SubmitOptions{Deadline: time.Now().Add(200 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	waitForRuntimeWait(t, second)
	waitForSignal(t, entered, "first run's hook")
	if _, err := awaitWithin(t, second, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second run past its deadline = %v, want DeadlineExceeded while the first run's hook is running", err)
	}
}
