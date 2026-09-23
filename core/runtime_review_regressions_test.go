// Regressions for the defects found by the independent T08 review.

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

// Each tool result fits MaxPayloadBytes, but the committed batch chunk (all
// results together) does not. The run needs attention, Reconcile cannot
// help (no invocation is uncertain), and every Recover reruns the worker into
// the same failure.
func TestRuntimeOversizedBatchIsNotRerunByRecovery(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), MaxPayloadBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	var calls atomic.Int32
	agent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("c-1", "big", `{}`), toolUse("c-2", "big", `{}`)),
		asstText("done"),
	}})
	agent.RegisterTool(Func("big", "big result", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return strings.Repeat("y", 2500), nil
	}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "go", SubmitOptions{})
	t.Logf("first run: err=%v", err)
	if !errors.Is(err, ErrRunNeedsAttention) {
		t.Fatalf("want attention, got %v", err)
	}
	snap, _ := runtime.Handle(result.RunID).Snapshot(context.Background())
	for _, b := range snap.ToolBatches {
		for _, inv := range b.Invocations {
			t.Logf("invocation %s state=%s", inv.OperationID, inv.State)
		}
	}
	before := len(allEvents(t, runtime.Handle(result.RunID)))
	for i := 0; i < 3; i++ {
		if err := runtime.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Handle(result.RunID).Await(context.Background()); !errors.Is(err, ErrRunNeedsAttention) {
			t.Fatalf("after Recover %d: %v, want attention", i+1, err)
		}
	}
	// Recovery leaves the payload-blocked run alone until the limit covers it:
	// no rerun, no new commits, and no tool executed again.
	if after := len(allEvents(t, runtime.Handle(result.RunID))); after != before || calls.Load() != 2 {
		t.Fatalf("events %d -> %d, tool executions %d; want no rerun", before, after, calls.Load())
	}
}

// An acknowledged cancellation whose transcript is damaged: recovery turns it
// into execution attention, and the durable cancel receipt makes every later
// Cancel a no-op, so the run can never become terminal.
func TestRuntimeDamagedCancelRequestedRunFinishesCancellation(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	seedInterruptedRun(t, base, "canceling", "batch_committed", afterBatchMessages)
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, "canceling")
		if err != nil {
			return err
		}
		record.State = RuntimeCancelRequested
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		data, _ := json.Marshal(cancelReceipt{Version: runtimeEncodingVersion, RunID: "canceling", Generation: record.Generation, ObservedState: RuntimeRunning})
		return tx.Put(runtimeReceiptsBucket, cancelReceiptKey("canceling"), data)
	}); err != nil {
		t.Fatal(err)
	}
	alterTranscriptChunk(t, base, "canceling", 0, "go", "forged")

	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, _ := runtimeRecord(runtime, "canceling")
	t.Logf("after Recover: state=%s kind=%s", record.State, record.AttentionKind)
	err = runtime.Handle("canceling").Cancel(context.Background())
	record, _ = runtimeRecord(runtime, "canceling")
	t.Logf("Cancel = %v; state=%s", err, record.State)
	if err == nil && record.State != RuntimeTerminal && record.State != RuntimeFinalizing {
		t.Fatalf("Cancel reported success but the run stays %s", record.State)
	}
}

// A child's final answer fits in its own transcript chunk, but the parent's
// invocation record that carries it (plus the call and metadata) does not.
func TestRuntimeOversizedChildProjectionNeedsAttentionWithoutStrandingRecovery(t *testing.T) {
	base := NewMemoryStore()
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}, MaxPayloadBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"`+strings.Repeat("t", 300)+`"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	if err := runtime.Register("child", "v1", testAgent(&scriptedProvider{turns: []Message{asstText(strings.Repeat("z", 3700))}})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, awaitErr := handle.Await(ctx); !errors.Is(awaitErr, ErrRunNeedsAttention) || !strings.Contains(awaitErr.Error(), ErrPayloadTooLarge.Error()) {
		t.Fatalf("parent Await = %v, want payload attention", awaitErr)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatalf("Recover = %v, want the oversized projection isolated on its run", err)
	}
}

// A Store that stays usable after Runtime.Close (as noCloseStore, used to
// share one store across runtimes): a commit after Close on a run that still
// has a hub subscriber closes an already-closed channel.
func TestRuntimeCommitAfterCloseDoesNotPanic(t *testing.T) {
	base := NewMemoryStore()
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	handle, _ := submitWaitingQuestion(t, runtime)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() { _, _ = handle.Await(ctx) }()
	time.Sleep(20 * time.Millisecond)
	_ = runtime.Close()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Cancel after Close panicked: %v", p)
		}
	}()
	err = handle.Cancel(context.Background())
	t.Logf("Cancel after Close = %v", err)
}

type countingStore struct {
	Store
	txs atomic.Int64
}

func (s *countingStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	s.txs.Add(1)
	return s.Store.Transaction(ctx, writable, fn)
}

// A parent blocked on child attention that also holds a pending, expired
// question wait: the driver rearms the elapsed expiry forever.
func TestRuntimeDriverDoesNotSpinOnChildAttentionWithExpiredWait(t *testing.T) {
	store := &countingStore{Store: NewMemoryStore()}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	parent := testAgent(&scriptedProvider{turns: []Message{asstTool("c1", "delegate", `{"topic":"tea"}`), asstText("parent done")}})
	newSharedChildDefinition(parent)
	if err := runtime.Register("child", "v1", testAgent(&repeatingChildProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	childResult, err := runtime.Run(context.Background(), "child", "v1", "seed", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parentID := "staged-parent"
	stageWaitingParentWithTerminalChild(t, runtime, parentID, childResult.RunID)
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, childResult.RunID)
		if err != nil {
			return err
		}
		record.State = RuntimeNeedsAttention
		record.AttentionKind = "hooks"
		record.AttentionReason = "blocked"
		record.Generation++
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		// A second, ordinary pending wait on the parent whose expiry elapsed.
		return putStoredWait(tx, storedWait{
			Version: runtimeEncodingVersion, RunID: parentID, ID: "question-wait",
			Kind: WaitQuestion, State: WaitPending, OperationID: "op-q", Tool: "ask_user",
			CreatedAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(100 * time.Millisecond),
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := runtime.Handle(parentID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("parent state=%s attention=%+v", snap.State, snap.Attention)
	time.Sleep(150 * time.Millisecond)
	before := store.txs.Load()
	time.Sleep(200 * time.Millisecond)
	delta := store.txs.Load() - before
	t.Logf("store transactions in 200ms of idle: %d", delta)
	if delta > 20 {
		t.Fatalf("driver hot loop: %d transactions in 200ms with no work to do", delta)
	}
}

type countingTextProvider struct{ calls atomic.Int32 }

func (p *countingTextProvider) Invoke(context.Context, Request) (Response, error) {
	p.calls.Add(1)
	return fixtureResponse(asstText("fresh answer")), nil
}

// Recover before Register (the documented order for admitted work) with an
// authorized fresh attempt: the fresh-attempt budget is spent on a worker that
// cannot bind, and after registration the run never resumes.
func TestRuntimeRecoveryBeforeRegistrationResumesAfterIt(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	seedInterruptedRun(t, base, "in-flight", transitionProviderAttemptStarted, afterBatchMessages)
	seedInterruptedRun(t, base, "no-attempt-yet", "", []Message{UserMessage("go")})

	runtime, err := NewRuntime(context.Background(), RuntimeConfig{
		Store: noCloseStore{Store: base}, ProviderRecovery: ProviderRecoveryPolicy{MaxFreshAttempts: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForWorkerExit(t, runtime, "in-flight")
	waitForWorkerExit(t, runtime, "no-attempt-yet")
	provider := &countingTextProvider{}
	agent := testAgent(provider)
	agent.RegisterTool(Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) { return "ok", nil }))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"in-flight", "no-attempt-yet"} {
		waitForWorkerExit(t, runtime, id)
		record, err := runtimeRecord(runtime, id)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: state=%s kind=%s reason=%q last=%s fresh=%d unknown=%d", id, record.State, record.AttentionKind, record.AttentionReason, record.LastTransition, record.FreshAttempts, record.UnknownAttempts)
		if record.State != RuntimeTerminal {
			t.Errorf("%s did not resume after registration + Recover", id)
		}
	}
	t.Logf("provider calls = %d", provider.calls.Load())
}

// Two commits on one run whose post-commit notifications arrive in reverse
// order (they run on different goroutines after the store serialized them):
// the older, non-settling state overwrites the newer settling one, and a
// waiter that wakes afterwards skips the read that would end its wait.
func TestRuntimeOutOfOrderNotifyCannotHideSettledState(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	handle, _ := submitWaitingQuestion(t, runtime)
	events := allEvents(t, handle)
	head := events[len(events)-1].Sequence

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := handle.Await(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // Await has read Waiting and is parked

	// Durable change to a settling state (NeedsAttention) at head+1 ...
	if err := runtime.store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, handle.ID())
		if err != nil {
			return err
		}
		record.State = RuntimeNeedsAttention
		record.AttentionKind, record.AttentionReason = "execution", "review"
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}
	// ... whose wake is delivered before the delayed wake of an older
	// commit (head) that recorded a non-settling state.
	runtime.hub.notify([]runCommit{
		{runID: handle.ID(), head: head + 1, state: RuntimeNeedsAttention},
		{runID: handle.ID(), head: head, state: RuntimeWaiting},
	})
	err = <-done
	if !errors.Is(err, ErrRunNeedsAttention) {
		t.Fatalf("Await = %v, want ErrRunNeedsAttention (durable state is needs_attention)", err)
	}
}

// One terminal run whose tool invocation fails its digest blocks every later
// Prune call for every run: the oldest-first pass stops at the damaged run.
func TestRuntimePruneSkipsDamagedRunAndContinues(t *testing.T) {
	base := NewMemoryStore()
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("e1", "extra", `{}`), asstText("done"), asstText("second")}})
	agent.RegisterTool(Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) { return "original", nil }))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	damaged, err := runtime.Run(context.Background(), "agent", "v1", "one", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := runtime.Run(context.Background(), "agent", "v1", "two", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Bit rot in the first run's invocation result (digest left unchanged).
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		var key string
		var raw []byte
		if err := tx.Scan(runtimeInvocationsBucket, damaged.RunID+"/", func(k string, v []byte) error {
			key, raw = k, v
			return nil
		}); err != nil {
			return err
		}
		return tx.Put(runtimeInvocationsBucket, key, []byte(strings.Replace(string(raw), "original", "forged!!", 1)))
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	for i := 0; i < 2; i++ {
		if _, err := runtime.Prune(context.Background(), RetentionPolicy{Events: time.Nanosecond}); err != nil {
			t.Fatalf("Prune #%d = %v", i+1, err)
		}
	}
	page, err := runtime.Handle(healthy.RunID).Events(context.Background(), 0, 10)
	t.Logf("healthy run still has %d events (err %v)", len(page.Events), err)
	if len(page.Events) > 0 {
		t.Fatal("a damaged run blocked retention of an unrelated healthy run")
	}
}

// WaitEvents blocked on a quiet run when Runtime.Close runs.
func TestRuntimeWaitEventsReturnsWhenRuntimeCloses(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	handle, _ := submitWaitingQuestion(t, runtime)
	head := allEvents(t, handle)
	after := head[len(head)-1].Sequence

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	type res struct {
		err  error
		took time.Duration
	}
	done := make(chan res, 1)
	go func() {
		start := time.Now()
		_, err := handle.WaitEvents(ctx, after, 10)
		done <- res{err, time.Since(start)}
	}()
	time.Sleep(50 * time.Millisecond)
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	t.Logf("WaitEvents returned err=%v after %v", r.err, r.took)
	if r.err == context.DeadlineExceeded {
		t.Fatalf("WaitEvents did not observe Close; it only returned at ctx deadline (%v)", r.took)
	}
}
