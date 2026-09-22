package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// repeatingChildProvider answers every provider turn with a fixed child
// response, so several concurrent durable child runs of one definition can
// each complete independently.
type repeatingChildProvider struct {
	calls atomic.Int32
}

func (p *repeatingChildProvider) Invoke(context.Context, Request) (Response, error) {
	p.calls.Add(1)
	return fixtureResponse(asstText("child done")), nil
}

func TestRuntimeRequiresExplicitStore(t *testing.T) {
	if _, err := NewRuntime(context.Background(), RuntimeConfig{}); err == nil {
		t.Fatal("nil store silently selected ephemeral execution")
	}
	runtime, err := NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(context.Background(), RuntimeConfig{Store: failingRuntimeStore{}}); err == nil {
		t.Fatal("unavailable persistent store silently fell back")
	}
}

func TestRuntimeUsesExistingLoopAndStableRunIdentity(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{asstText("done")}}
	agent := testAgent(provider)
	runtime := newTestRuntime(t)
	if err := runtime.Register("answerer", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "answerer", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" || result.Output != "done" || result.Status != RunCompleted {
		t.Fatalf("result = %#v", result)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RunID != result.RunID || snapshot.Result.RunID != result.RunID || snapshot.State != RuntimeTerminal {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestRuntimeAdmissionIsIdempotent(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("ok")}})); err != nil {
		t.Fatal(err)
	}
	options := SubmitOptions{Scope: "tenant", Key: "task-1"}
	a, err := runtime.Submit(context.Background(), "agent", "v1", "same", options)
	if err != nil {
		t.Fatal(err)
	}
	b, err := runtime.Submit(context.Background(), "agent", "v1", "same", options)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() {
		t.Fatalf("duplicate admission IDs = %q, %q", a.ID(), b.ID())
	}
	if _, err := runtime.Submit(context.Background(), "agent", "v1", "changed", options); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("changed duplicate = %v", err)
	}
}

func TestRuntimeCompletedAdmissionRetryReturnsStoredResult(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("ok")}})); err != nil {
		t.Fatal(err)
	}
	options := SubmitOptions{Scope: "tenant", Key: "completed-task"}
	first, err := runtime.Run(context.Background(), "agent", "v1", "same", options)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := runtime.Submit(context.Background(), "agent", "v1", "same", options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := retried.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID != first.RunID || result.Output != first.Output || result.Status != RunCompleted {
		t.Fatalf("retried result = %#v, want %#v", result, first)
	}
}

// Durable children are ordinary durable runs: the parent suspends on a
// persisted child wait while its children execute, and consumes each
// completion exactly once. The parent's subtree cap is pinned at admission and
// each child invocation reserves against it atomically with batch creation.
func TestRuntimeDurableChildConcurrentSiblingsComplete(t *testing.T) {
	childProvider := &repeatingChildProvider{}
	child := testAgent(childProvider)
	childDefinition := ToolDefinition{
		Name:        "child",
		Description: "delegate to a durable child",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
	}
	parent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("sub-1", "child", `{"topic":"a"}`), toolUse("sub-2", "child", `{"topic":"b"}`)),
		asstText("parent done"),
	}})
	parent.RegisterTool(DurableChildTool(childDefinition, DurableChildPolicy{DefinitionID: "child", Revision: "v1"}))
	parent.WithToolPolicy(ToolPolicy{MaxCalls: 2})

	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", child); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent done" {
		t.Errorf("output = %q", result.Output)
	}
	if got := childProvider.calls.Load(); got != 2 {
		t.Fatalf("child provider calls = %d, want 2", got)
	}
	// The two concurrent sibling child invocations reserved the parent's
	// pinned subtree cap atomically with batch creation.
	parentRecord, err := runtimeRecord(runtime, handle.ID())
	if err != nil {
		t.Fatal(err)
	}
	if parentRecord.ToolBudget.TotalCap != 2 || parentRecord.ToolBudget.Total != 2 {
		t.Fatalf("parent tool budget = %#v, want cap 2 used 2", parentRecord.ToolBudget)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ToolBatches) != 1 || len(snapshot.ToolBatches[0].Invocations) != 2 {
		t.Fatalf("parent batch = %#v", snapshot.ToolBatches)
	}
	childIDs := map[string]bool{}
	for i, invocation := range snapshot.ToolBatches[0].Invocations {
		if invocation.ChildRunID == "" {
			t.Fatalf("invocation %d has no child run id", i)
		}
		if childIDs[invocation.ChildRunID] {
			t.Fatalf("duplicate child run id %q across siblings", invocation.ChildRunID)
		}
		childIDs[invocation.ChildRunID] = true
		child, err := runtime.Handle(invocation.ChildRunID).Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if child.State != RuntimeTerminal || child.Result.Output != "child done" {
			t.Fatalf("child %s = %s %q", invocation.ChildRunID, child.State, child.Result.Output)
		}
		if child.ParentRunID != handle.ID() || child.ParentOperationID != invocation.OperationID {
			t.Fatalf("child %s parentage = %q/%q", invocation.ChildRunID, child.ParentRunID, child.ParentOperationID)
		}
	}
	// Canonical transcript keeps the child projections in model order.
	results := snapshot.Result.Messages
	if len(results) < 4 {
		t.Fatalf("parent transcript = %#v", results)
	}
	for i, callID := range []string{"sub-1", "sub-2"} {
		blocks := results[2+i].Blocks
		if len(blocks) != 1 {
			t.Fatalf("tool result %d = %#v", i, blocks)
		}
		resultBlock, ok := blocks[0].(ToolResultBlock)
		if !ok || resultBlock.ToolUseID != callID {
			t.Fatalf("transcript result %d = %#v", i, blocks)
		}
		if resultBlock.IsError || resultBlock.Content[0].(TextBlock).Text != "child done" {
			t.Fatalf("child projection %d = %#v", i, blocks)
		}
	}
}

// The denied sibling keeps its identity as a recoverable, not-applied result:
// no second child run is admitted and the parent's pinned cap is charged only
// by the admitted sibling.
func TestRuntimeDurableChildBudgetDeniedSiblingKeepsIdentity(t *testing.T) {
	childProvider := &repeatingChildProvider{}
	child := testAgent(childProvider)
	parent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("sub-1", "child", `{"topic":"a"}`), toolUse("sub-2", "child", `{"topic":"b"}`)),
		asstText("parent done"),
	}}).WithToolPolicy(ToolPolicy{MaxCalls: 1})
	parent.RegisterTool(DurableChildTool(childTestDefinition("child"), DurableChildPolicy{DefinitionID: "child", Revision: "v1"}))

	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", child); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	var budgetEvent StreamEvent
	result, err := runtime.RunStream(context.Background(), "parent", "v1", "go", func(event StreamEvent) {
		if event.Kind == StreamToolResult && errors.Is(event.Err, ErrToolBudgetExhausted) {
			budgetEvent = event
		}
	}, SubmitOptions{})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if result.Output != "parent done" {
		t.Errorf("output = %q", result.Output)
	}
	if got := childProvider.calls.Load(); got != 1 {
		t.Fatalf("child provider calls = %d, want 1", got)
	}
	if budgetEvent.ToolCall.Name != "child" || budgetEvent.ToolCall.ID != "sub-2" {
		t.Errorf("budget event = %#v", budgetEvent)
	}
	handle := runtime.Handle(result.RunID)
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	batch := snapshot.ToolBatches[0]
	if batch.Invocations[1].Effect.Status != EffectNotApplied || batch.Invocations[1].State != ToolInvocationCompleted {
		t.Fatalf("denied sibling = %#v", batch.Invocations[1])
	}
	childID := batch.Invocations[0].ChildRunID
	if batch.Invocations[1].ChildRunID != "" {
		t.Fatalf("denied sibling admitted a child run: %#v", batch.Invocations[1])
	}
	childSnapshot, err := runtime.Handle(childID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if childSnapshot.ParentRunID != result.RunID || childSnapshot.State != RuntimeTerminal {
		t.Fatalf("child %s = %s parent %q", childID, childSnapshot.State, childSnapshot.ParentRunID)
	}
	// Exactly one child run and link exist; the denied reservation produced no
	// second admission, and the parent's cap records one used reservation.
	parentRecord, err := runtimeRecord(runtime, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if parentRecord.ToolBudget.TotalCap != 1 || parentRecord.ToolBudget.Total != 1 {
		t.Fatalf("parent tool budget = %#v, want cap 1 used 1", parentRecord.ToolBudget)
	}
	var runCount, linkCount int
	if err := runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		if err := tx.Scan(runtimeRunsBucket, "", func(string, []byte) error { runCount++; return nil }); err != nil {
			return err
		}
		return tx.Scan(runtimeChildLinksBucket, "", func(string, []byte) error { linkCount++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if runCount != 2 || linkCount != 1 {
		t.Fatalf("runs = %d links = %d, want 2/1", runCount, linkCount)
	}
}

func TestRuntimeRunStreamStartsAdmittedWorkAfterViewCancellation(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	view, cancel := context.WithCancel(context.Background())
	store := &afterAdmissionStore{Store: base, after: cancel}
	provider := &countingRuntimeProvider{}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}

	store.armed.Store(true)
	result, err := runtime.RunStream(view, "agent", "v1", "work", nil, SubmitOptions{})
	runID := onlyStoredRunID(t, base)
	if !errors.Is(err, context.Canceled) || result.RunID != runID {
		t.Errorf("detached stream = %#v, %v; want run %q and context cancellation", result, err, runID)
	}

	awaitCtx, awaitCancel := context.WithTimeout(context.Background(), time.Second)
	defer awaitCancel()
	completed, err := runtime.Handle(runID).Await(awaitCtx)
	if err != nil || completed.Output != "done" || completed.RunID != runID {
		t.Fatalf("admitted run did not complete = %#v, %v", completed, err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestRuntimeRunStreamStartsAdmittedWorkAfterSnapshotFailure(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	readErr := errors.New("injected attachment read failure")
	store := &failReadAfterAdmissionStore{Store: base, err: readErr}
	provider := &countingRuntimeProvider{}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}

	store.armed.Store(true)
	result, err := runtime.RunStream(context.Background(), "agent", "v1", "work", nil, SubmitOptions{})
	runID := onlyStoredRunID(t, base)
	if !errors.Is(err, readErr) || result.RunID != runID {
		t.Errorf("failed attachment = %#v, %v; want run %q and read error", result, err, runID)
	}

	awaitCtx, awaitCancel := context.WithTimeout(context.Background(), time.Second)
	defer awaitCancel()
	completed, err := runtime.Handle(runID).Await(awaitCtx)
	if err != nil || completed.Output != "done" || completed.RunID != runID {
		t.Fatalf("admitted run did not complete = %#v, %v", completed, err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestRuntimeRunStreamDisconnectRetainsIdentityWithoutDetachedRead(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &failRuntimeReadsStore{Store: base, err: errors.New("injected detached read failure")}
	provider := &countingBarrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(provider.unblock)

	view, cancel := context.WithCancel(context.Background())
	finished := make(chan BackgroundResult, 1)
	go func() {
		result, err := runtime.RunStream(view, "agent", "v1", "work", nil, SubmitOptions{})
		finished <- BackgroundResult{Result: result, Err: err}
	}()
	waitForSignal(t, provider.started, "provider start")
	store.failReads.Store(true)
	cancel()

	detached := waitForBackgroundResult(t, finished)
	if !errors.Is(detached.Err, context.Canceled) || detached.Result.RunID == "" {
		t.Fatalf("detached stream = %#v, %v; want admitted identity and context cancellation", detached.Result, detached.Err)
	}
	if got := store.failedReads.Load(); got != 0 {
		t.Fatalf("detached storage reads = %d, want 0", got)
	}

	store.failReads.Store(false)
	provider.unblock()
	awaitCtx, awaitCancel := context.WithTimeout(context.Background(), time.Second)
	defer awaitCancel()
	completed, err := runtime.Handle(detached.Result.RunID).Await(awaitCtx)
	if err != nil || completed.Output != "done" || completed.RunID != detached.Result.RunID {
		t.Fatalf("admitted run did not complete = %#v, %v", completed, err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestRuntimeRunStreamDoesNotStartUnknownAdmissionOutcome(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	commitErr := errors.New("admission commit outcome unknown")
	store := &unknownAdmissionStore{Store: base, err: commitErr}
	provider := &countingRuntimeProvider{}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}

	store.armed.Store(true)
	result, err := runtime.RunStream(context.Background(), "agent", "v1", "work", nil, SubmitOptions{})
	if !errors.Is(err, commitErr) || result.RunID != "" {
		t.Fatalf("unknown admission result = %#v, %v", result, err)
	}
	runID := onlyStoredRunID(t, base)
	snapshot, err := runtime.Handle(runID).Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeReady {
		t.Fatalf("unknown admission snapshot = %#v, %v", snapshot, err)
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
}

func TestRuntimeRunStreamDoesNotStartCanceledAdmission(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	provider := &countingRuntimeProvider{}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	view, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := runtime.RunStream(view, "agent", "v1", "work", nil, SubmitOptions{})
	if !errors.Is(err, context.Canceled) || result.RunID != "" {
		t.Fatalf("canceled admission = %#v, %v", result, err)
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	if got := storedRunCount(t, base); got != 0 {
		t.Fatalf("stored runs = %d, want 0", got)
	}
}

func TestRuntimeStaleStreamSchedulingDoesNotPoisonCompletedRun(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &pauseReadStore{
		Store:   base,
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
	provider := &countingBarrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(provider.unblock)
	t.Cleanup(store.unblock)
	options := SubmitOptions{Scope: "tenant", Key: "stale-stream"}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", options)
	if err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, provider.started, "provider start")

	terminal, unsubscribe := runtime.subscribe(handle.ID())
	defer unsubscribe()
	store.pause.Store(true)
	attached := make(chan BackgroundResult, 1)
	go func() {
		result, err := runtime.RunStream(context.Background(), "agent", "v1", "work", nil, options)
		attached <- BackgroundResult{Result: result, Err: err}
	}()
	waitForSignal(t, store.reached, "stream snapshot")

	provider.unblock()
	waitForRuntimeTerminal(t, terminal)
	first, err := handle.Await(context.Background())
	if err != nil || first.Output != "done" {
		t.Fatalf("original result = %#v, %v", first, err)
	}
	staleAttempt, unsubscribeStale := runtime.subscribe(handle.ID())
	defer unsubscribeStale()
	store.unblock()
	reattached := waitForBackgroundResult(t, attached)
	if reattached.Err != nil || reattached.Result.RunID != first.RunID || reattached.Result.Output != first.Output {
		t.Fatalf("reattached result = %#v, %v; want %#v", reattached.Result, reattached.Err, first)
	}
	waitForRuntimeTerminal(t, staleAttempt)

	again, err := handle.Await(context.Background())
	if err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("later await = %#v, %v; want %#v", again, err, first)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeTerminal || snapshot.Result.Output != "done" {
		t.Fatalf("terminal snapshot = %#v, %v", snapshot, err)
	}
}

func TestRuntimeDuplicateSchedulingWhileLiveIsHarmless(t *testing.T) {
	provider := &countingBarrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(provider.unblock)
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, provider.started, "provider start")
	runtime.start(handle.ID())
	runtime.start(handle.ID())
	provider.unblock()
	awaitCtx, awaitCancel := context.WithTimeout(context.Background(), time.Second)
	defer awaitCancel()
	result, err := handle.Await(awaitCtx)
	if err != nil || result.Output != "done" {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestRuntimeClaimFailurePreservesRunIdentity(t *testing.T) {
	claimErr := errors.New("injected claim failure")
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &failWritableTransactionStore{Store: base, failAt: 3, err: claimErr}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(&countingRuntimeProvider{})); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	awaitCtx, awaitCancel := context.WithTimeout(context.Background(), time.Second)
	defer awaitCancel()
	result, err := handle.Await(awaitCtx)
	if !errors.Is(err, claimErr) || result.RunID != handle.ID() {
		t.Fatalf("claim failure = %#v, %v", result, err)
	}
}

func TestRuntimeStaleSchedulingPreservesGenuineWorkerFailure(t *testing.T) {
	finishErr := errors.New("injected finalization failure")
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &failWritableTransactionStore{Store: base, failAt: 6, err: finishErr}
	provider := &countingRuntimeProvider{}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	options := SubmitOptions{Scope: "tenant", Key: "failed-finalization"}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", options)
	if err != nil {
		t.Fatal(err)
	}
	firstCtx, firstCancel := context.WithTimeout(context.Background(), time.Second)
	defer firstCancel()
	first, err := handle.Await(firstCtx)
	if !errors.Is(err, finishErr) || first.RunID != handle.ID() || first.Output != "done" {
		t.Fatalf("initial worker failure = %#v, %v", first, err)
	}

	view, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	attached, err := runtime.RunStream(view, "agent", "v1", "work", nil, options)
	if !errors.Is(err, finishErr) || attached.RunID != first.RunID || attached.Output != first.Output {
		t.Fatalf("reattached worker failure = %#v, %v; want %#v", attached, err, first)
	}
	againCtx, againCancel := context.WithTimeout(context.Background(), time.Second)
	defer againCancel()
	again, err := handle.Await(againCtx)
	if !errors.Is(err, finishErr) || again.RunID != first.RunID || again.Output != first.Output {
		t.Fatalf("later worker failure = %#v, %v; want %#v", again, err, first)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestRuntimeInvalidPersistedStateIsAClaimFailure(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: base})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(&countingRuntimeProvider{})); err != nil {
		t.Fatal(err)
	}
	record := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: "invalid-state", DefinitionID: "agent",
		DefinitionRevision: "v1", Task: "work", State: RuntimeState("corrupt"),
		Generation: 1, Result: RunResult{RunID: "invalid-state"},
	}
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}

	runtime.start(record.RunID)
	awaitCtx, awaitCancel := context.WithTimeout(context.Background(), time.Second)
	defer awaitCancel()
	result, err := runtime.Handle(record.RunID).Await(awaitCtx)
	if err == nil || result.RunID != record.RunID || !strings.Contains(err.Error(), "invalid state") {
		t.Fatalf("invalid state claim = %#v, %v", result, err)
	}
}

func TestRuntimeViewCancellationDoesNotCancelRun(t *testing.T) {
	provider := &barrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-provider.started
	view, cancel := context.WithCancel(context.Background())
	cancel()
	partial, err := handle.Await(view)
	if !errors.Is(err, context.Canceled) || partial.RunID != handle.ID() {
		t.Fatalf("disconnected view = %#v, %v", partial, err)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeRunning {
		t.Fatalf("logical run after disconnect = %#v, %v", snapshot, err)
	}
	close(provider.release)
	result, err := handle.Await(context.Background())
	if err != nil || result.Output != "done" {
		t.Fatalf("continued result = %#v, %v", result, err)
	}
}

func TestRuntimeAwaitCancellationRetainsIdentityWithoutDetachedRead(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &failRuntimeReadsStore{Store: base, err: errors.New("injected detached read failure")}
	provider := &countingBarrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(provider.unblock)
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, provider.started, "provider start")

	view, cancel := context.WithCancel(context.Background())
	cancel()
	store.failReads.Store(true)
	partial, err := handle.Await(view)
	if !errors.Is(err, context.Canceled) || partial.RunID != handle.ID() {
		t.Fatalf("detached await = %#v, %v; want admitted identity and context cancellation", partial, err)
	}
	if got := store.failedReads.Load(); got != 0 {
		t.Fatalf("detached storage reads = %d, want 0", got)
	}

	store.failReads.Store(false)
	provider.unblock()
	awaitCtx, awaitCancel := context.WithTimeout(context.Background(), time.Second)
	defer awaitCancel()
	completed, err := handle.Await(awaitCtx)
	if err != nil || completed.Output != "done" || completed.RunID != handle.ID() {
		t.Fatalf("admitted run did not complete = %#v, %v", completed, err)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestRuntimeSyncHelperDisconnectCanReattachByAdmissionIdentity(t *testing.T) {
	provider := &barrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	view, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	options := SubmitOptions{Scope: "tenant", Key: "reattach"}
	go func() {
		_, err := runtime.Run(view, "agent", "v1", "work", options)
		finished <- err
	}()
	<-provider.started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("sync view disconnect = %v", err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", options)
	if err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	result, err := handle.Await(context.Background())
	if err != nil || result.Output != "done" || result.RunID != handle.ID() {
		t.Fatalf("reattached result = %#v, %v", result, err)
	}
}

func TestRuntimeStreamIsAViewOfSameRun(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("streamed")}})); err != nil {
		t.Fatal(err)
	}
	var text string
	result, err := runtime.RunStream(context.Background(), "agent", "v1", "work", func(event StreamEvent) {
		if event.Kind == StreamText {
			text += event.Text
		}
	}, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" || result.Output != "streamed" || text != "streamed" {
		t.Fatalf("stream result = %#v, text=%q", result, text)
	}
}

func TestRuntimeExplicitCancelOwnsLogicalCancellation(t *testing.T) {
	provider := &cancelRuntimeProvider{started: make(chan struct{})}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-provider.started
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if !errors.Is(err, context.Canceled) || result.Status != RunCancelled {
		t.Fatalf("cancel result = %#v, %v", result, err)
	}
}

func TestRuntimeUsesRequestTransformsButNotDirectRunObservers(t *testing.T) {
	var transforms atomic.Int32
	var legacyObservations atomic.Int32
	agent, err := New(&scriptedProvider{turns: []Message{asstText("done")}}, AgentConfig{
		Observers: []RunObserver{func(context.Context, RunEvent) {
			legacyObservations.Add(1)
		}},
		PreSendHooks: []PreSendHook{func(_ context.Context, request Request) (Request, error) {
			transforms.Add(1)
			return request, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil || result.Status != RunCompleted {
		t.Fatalf("durable result = %#v, %v", result, err)
	}
	if transforms.Load() != 1 || legacyObservations.Load() != 0 {
		t.Fatalf("request transforms=%d legacy observations=%d", transforms.Load(), legacyObservations.Load())
	}
}

func TestRuntimeCommittedHooksAreBoundedVisibleAndDoNotRewriteResult(t *testing.T) {
	hookErr := errors.New("notification unavailable")
	var runtime *Runtime
	var order []string
	hooks := []CommittedRunHook{
		{
			Name: "audit",
			Handle: func(_ context.Context, snapshot RunSnapshot) error {
				order = append(order, "audit")
				persisted, err := runtime.Handle(snapshot.RunID).Snapshot(context.Background())
				if err != nil {
					return err
				}
				if persisted.State != RuntimeFinalizing || persisted.Result.Output != "done" {
					t.Errorf("hook observed %#v", persisted)
				}
				snapshot.Result.Output = "mutated"
				return hookErr
			},
		},
		{
			Name:    "bounded",
			Timeout: 10 * time.Millisecond,
			Handle: func(ctx context.Context, snapshot RunSnapshot) error {
				order = append(order, "bounded")
				if snapshot.Result.Output != "done" {
					t.Errorf("hook snapshots alias: %#v", snapshot.Result)
				}
				<-ctx.Done()
				return ctx.Err()
			},
		},
		{
			Name: "panic",
			Handle: func(context.Context, RunSnapshot) error {
				order = append(order, "panic")
				panic("boom")
			},
		},
	}
	var err error
	runtime, err = NewEphemeralRuntime(hooks...)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("done")}})); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil || result.Output != "done" || result.Status != RunCompleted {
		t.Fatalf("execution result = %#v, %v", result, err)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeTerminal || len(snapshot.HookResults) != 3 {
		t.Fatalf("hook snapshot = %#v", snapshot)
	}
	if snapshot.HookResults[0].Error != hookErr.Error() {
		t.Fatalf("first hook result = %#v", snapshot.HookResults[0])
	}
	if snapshot.HookResults[1].Error != context.DeadlineExceeded.Error() || snapshot.HookResults[2].Error != "panic: boom" {
		t.Fatalf("hook results = %#v", snapshot.HookResults)
	}
	if want := []string{"audit", "bounded", "panic"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("hook order = %v, want %v", order, want)
	}
}

func TestRuntimeRejectsInvalidCommittedHooks(t *testing.T) {
	for _, hooks := range [][]CommittedRunHook{
		{{Handle: func(context.Context, RunSnapshot) error { return nil }}},
		{{Name: "missing"}},
		{{Name: "negative", Timeout: -time.Second, Handle: func(context.Context, RunSnapshot) error { return nil }}},
		{{Name: "duplicate", Handle: func(context.Context, RunSnapshot) error { return nil }}, {Name: "duplicate", Handle: func(context.Context, RunSnapshot) error { return nil }}},
	} {
		if _, err := NewEphemeralRuntime(hooks...); err == nil {
			t.Fatalf("accepted hooks %#v", hooks)
		}
	}
}

func TestRuntimeRecoverUsesPinnedBindingAndConservativeAttention(t *testing.T) {
	store := &memoryStore{buckets: make(map[string]map[string][]byte)}
	first, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	agent := testAgent(&scriptedProvider{turns: []Message{asstText("recovered")}})
	if err := first.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := first.submit(context.Background(), "agent", "v1", "work", SubmitOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	first.cancel()
	first.mu.Lock()
	first.closed = true
	first.mu.Unlock()

	reopened, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := reopened.Handle(handle.ID()).Await(context.Background())
	if err != nil || result.Output != "recovered" || result.RunID != handle.ID() {
		t.Fatalf("recovered result = %#v, %v", result, err)
	}

	missing, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Close()
	ready := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: "missing-binding", DefinitionID: "agent",
		DefinitionRevision: "v2", Task: "work", State: RuntimeReady,
		Generation: 1, Result: RunResult{RunID: "missing-binding"},
	}
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		return putRuntimeRun(tx, ready)
	}); err != nil {
		t.Fatal(err)
	}
	if err := missing.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := missing.Handle("missing-binding").Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeNeedsAttention || snapshot.AttentionReason == "" {
		t.Fatalf("missing binding snapshot = %#v, %v", snapshot, err)
	}
	if err := missing.Register("agent", "v2", testAgent(&scriptedProvider{turns: []Message{asstText("late binding")}})); err != nil {
		t.Fatal(err)
	}
	if err := missing.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err = missing.Handle("missing-binding").Await(context.Background())
	if err != nil || result.Output != "late binding" {
		t.Fatalf("late-bound result = %#v, %v", result, err)
	}
	// Hook delivery had started, so its outcome is unknown: attention. A
	// committed result whose delivery never started finishes instead.
	for _, finalizing := range []storedRuntimeRun{
		{
			Version: runtimeEncodingVersion, RunID: "hook-uncertain", DefinitionID: "agent",
			DefinitionRevision: "v2", Task: "work", State: RuntimeFinalizing, HookDelivery: []string{"audit"},
			Generation: 1, Result: RunResult{RunID: "hook-uncertain", Status: RunCompleted, Output: "done"},
		},
		{
			Version: runtimeEncodingVersion, RunID: "hook-unstarted", DefinitionID: "agent",
			DefinitionRevision: "v2", Task: "work", State: RuntimeFinalizing,
			Generation: 1, Result: RunResult{RunID: "hook-unstarted", Status: RunCompleted, Output: "done"},
		},
	} {
		if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
			return putRuntimeRun(tx, finalizing)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := missing.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err = missing.Handle("hook-uncertain").Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeNeedsAttention || snapshot.Result.Output != "done" {
		t.Fatalf("uncertain hook recovery = %#v, %v", snapshot, err)
	}
	snapshot, err = missing.Handle("hook-unstarted").Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeTerminal || snapshot.Result.Status != RunCompleted || snapshot.Result.Output != "done" {
		t.Fatalf("unstarted hook recovery = %#v, %v", snapshot, err)
	}
}

func TestRuntimeRecoverDoesNotReclassifyLiveRun(t *testing.T) {
	provider := &barrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-provider.started
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeRunning {
		t.Fatalf("live run after recovery = %#v, %v", snapshot, err)
	}
	close(provider.release)
	if _, err := handle.Await(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePersistsAcceptedProviderTurnBeforeToolDispatch(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &failWritableTransactionStore{Store: base, failAt: 4}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	tool := &countingTool{}
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("c1", "counter", `{}`)}})
	agent.RegisterTool(tool)
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if !errors.Is(err, ErrRunNeedsAttention) {
		t.Fatalf("transition failure = %#v, %v", result, err)
	}
	if tool.calls.Load() != 0 {
		t.Fatalf("tool dispatched %d times before durable provider transition", tool.calls.Load())
	}
	if len(result.Messages) < 2 || len(result.Messages[len(result.Messages)-1].ToolUses()) != 1 {
		t.Fatalf("accepted provider evidence was not retained: %#v", result.Messages)
	}
}

func TestRuntimeRestoresDeadlineErrorClassification(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&cancelRuntimeProvider{started: make(chan struct{})})); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{Deadline: time.Now().Add(20 * time.Millisecond)})
	if !errors.Is(err, context.DeadlineExceeded) || result.Status != RunCancelled {
		t.Fatalf("deadline result = %#v, %v", result, err)
	}
}

func TestRuntimeDefinitionRegistrationIsImmutable(t *testing.T) {
	runtime := newTestRuntime(t)
	provider := &capturingProvider{turns: []Message{asstText("a")}}
	a := testAgent(provider)
	a.systemPrompt = "original"
	b := testAgent(&scriptedProvider{turns: []Message{asstText("b")}})
	if err := runtime.Register("agent", "v1", a); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("agent", "v1", a); err != nil {
		t.Fatalf("idempotent registration = %v", err)
	}
	if err := runtime.Register("agent", "v1", b); !errors.Is(err, ErrDefinitionConflict) {
		t.Fatalf("replacement registration = %v", err)
	}
	a.systemPrompt = "mutated after registration"
	if _, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := provider.received[0][0].Text(); got != "original" {
		t.Fatalf("registered definition drifted to %q", got)
	}
}

func TestRuntimeCloseYieldsInterruptedWorkerToAttention(t *testing.T) {
	provider := &barrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	store := newEphemeralStore()
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-provider.started
	closed := make(chan error, 1)
	go func() { closed <- runtime.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned while provider was active: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(provider.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if handle.ID() == "" {
		t.Fatal("admitted run lost its identity")
	}
}

// runtimeRecord loads a stored run record through the runtime's store so tests
// can assert on durable bookkeeping that is not part of the public snapshot.
func runtimeRecord(runtime *Runtime, runID string) (storedRuntimeRun, error) {
	var record storedRuntimeRun
	err := runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		var err error
		record, err = getRuntimeRun(tx, runID)
		return err
	})
	return record, err
}

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	runtime, err := NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

type barrierRuntimeProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *barrierRuntimeProvider) Invoke(context.Context, Request) (Response, error) {
	close(p.started)
	<-p.release
	return Response{Message: asstText("done"), StopReason: StopEndTurn}, nil
}

type countingRuntimeProvider struct{ calls atomic.Int32 }

func (p *countingRuntimeProvider) Invoke(context.Context, Request) (Response, error) {
	p.calls.Add(1)
	return Response{Message: asstText("done"), StopReason: StopEndTurn}, nil
}

type countingBarrierRuntimeProvider struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
	calls       atomic.Int32
}

func (p *countingBarrierRuntimeProvider) Invoke(context.Context, Request) (Response, error) {
	p.calls.Add(1)
	p.startedOnce.Do(func() { close(p.started) })
	<-p.release
	return Response{Message: asstText("done"), StopReason: StopEndTurn}, nil
}

func (p *countingBarrierRuntimeProvider) unblock() {
	p.releaseOnce.Do(func() { close(p.release) })
}

type cancelRuntimeProvider struct{ started chan struct{} }

func (p *cancelRuntimeProvider) Invoke(ctx context.Context, _ Request) (Response, error) {
	close(p.started)
	<-ctx.Done()
	return Response{}, ctx.Err()
}

type noCloseStore struct{ Store }

func (noCloseStore) Close() error { return nil }

type failingRuntimeStore struct{}

func (failingRuntimeStore) Transaction(context.Context, bool, func(StoreTransaction) error) error {
	return errors.New("storage unavailable")
}
func (failingRuntimeStore) Close() error { return nil }

type failWritableTransactionStore struct {
	Store
	writes atomic.Int32
	failAt int32
	err    error
}

func (s *failWritableTransactionStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	if writable && s.writes.Add(1) == s.failAt {
		if s.err != nil {
			return s.err
		}
		return errors.New("injected durable transition failure")
	}
	return s.Store.Transaction(ctx, writable, fn)
}

type afterAdmissionStore struct {
	Store
	armed atomic.Bool
	after func()
}

func (s *afterAdmissionStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	err := s.Store.Transaction(ctx, writable, fn)
	if err == nil && writable && s.armed.CompareAndSwap(true, false) {
		s.after()
	}
	return err
}

type failReadAfterAdmissionStore struct {
	Store
	armed    atomic.Bool
	failRead atomic.Bool
	err      error
}

func (s *failReadAfterAdmissionStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	if !writable && s.failRead.CompareAndSwap(true, false) {
		return s.err
	}
	err := s.Store.Transaction(ctx, writable, fn)
	if err == nil && writable && s.armed.CompareAndSwap(true, false) {
		s.failRead.Store(true)
	}
	return err
}

type failRuntimeReadsStore struct {
	Store
	failReads   atomic.Bool
	failedReads atomic.Int32
	err         error
}

func (s *failRuntimeReadsStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	if !writable && s.failReads.Load() {
		s.failedReads.Add(1)
		return s.err
	}
	return s.Store.Transaction(ctx, writable, fn)
}

type unknownAdmissionStore struct {
	Store
	armed atomic.Bool
	err   error
}

func (s *unknownAdmissionStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	err := s.Store.Transaction(ctx, writable, fn)
	if err == nil && writable && s.armed.CompareAndSwap(true, false) {
		return s.err
	}
	return err
}

type pauseReadStore struct {
	Store
	pause       atomic.Bool
	reached     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func (s *pauseReadStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	err := s.Store.Transaction(ctx, writable, fn)
	if err == nil && !writable && s.pause.CompareAndSwap(true, false) {
		close(s.reached)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *pauseReadStore) unblock() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func onlyStoredRunID(t *testing.T, store Store) string {
	t.Helper()
	var ids []string
	if err := store.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		return tx.Scan(runtimeRunsBucket, "", func(id string, _ []byte) error {
			ids = append(ids, id)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("stored run IDs = %v, want exactly one", ids)
	}
	return ids[0]
}

func storedRunCount(t *testing.T, store Store) int {
	t.Helper()
	count := 0
	if err := store.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		return tx.Scan(runtimeRunsBucket, "", func(_ string, _ []byte) error {
			count++
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitForRuntimeTerminal(t *testing.T, events <-chan runtimeStreamItem) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case item := <-events:
			if item.terminal {
				return
			}
		case <-timer.C:
			t.Fatal("timed out waiting for runtime completion")
		}
	}
}

func waitForBackgroundResult(t *testing.T, result <-chan BackgroundResult) BackgroundResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for background result")
		return BackgroundResult{}
	}
}
