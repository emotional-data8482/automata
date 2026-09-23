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
