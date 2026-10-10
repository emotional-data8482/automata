package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeCanceledUnreadableHistoryTerminalizes(t *testing.T) {
	for _, withHooks := range []bool{false, true} {
		for _, deadline := range []bool{false, true} {
			t.Run(testBoolName("hooks", withHooks)+"/"+testBoolName("deadline", deadline), func(t *testing.T) {
				var calls atomic.Int32
				config := RuntimeConfig{Store: NewMemoryStore()}
				if withHooks {
					config.Hooks = []CommittedRunHook{{Name: "audit", Handle: func(context.Context, RunSnapshot) error {
						calls.Add(1)
						return nil
					}}}
				}
				runtime, err := NewRuntime(context.Background(), config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = runtime.Close() })
				handle, _ := submitWaitingQuestion(t, runtime)
				waitForWorkerExit(t, runtime, handle.ID())
				if err := runtime.store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
					return deletePrefix(tx, runtimeFactsBucket, handle.ID()+"/")
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := handle.Snapshot(context.Background()); !errors.Is(err, ErrPayloadUnavailable) {
					t.Fatalf("Snapshot = %v, want unavailable history", err)
				}
				if deadline {
					if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
						record, err := getRuntimeRun(tx, handle.ID())
						if err != nil {
							return err
						}
						record.Deadline = time.Now().Add(-time.Second)
						return putRuntimeRun(tx, record)
					}); err != nil {
						t.Fatal(err)
					}
					finalized, _, err := runtime.expireWaitingDeadline(context.Background(), handle.ID())
					if err != nil || !finalized {
						t.Fatalf("expire = %v, %v", finalized, err)
					}
					if err := runtime.completeRunHooks(handle.ID()); err != nil {
						t.Fatal(err)
					}
				} else if err := handle.Cancel(context.Background()); err != nil {
					t.Fatal(err)
				}
				record, err := runtime.compactRecord(context.Background(), handle.ID())
				if err != nil {
					t.Fatal(err)
				}
				want := context.Canceled
				if deadline {
					want = context.DeadlineExceeded
				}
				failure := snapshotError(snapshotFromRecord(record))
				if record.State != RuntimeTerminal || record.Result.Status != RunCancelled || !errors.Is(failure, want) {
					t.Fatalf("canceled record = %s %s %v", record.State, record.Result.Status, failure)
				}
				if calls.Load() != 0 {
					t.Fatal("hooks observed unreadable history")
				}
				if err := handle.Cancel(context.Background()); err != nil {
					t.Fatalf("retry = %v", err)
				}
			})
		}
	}
}

func testBoolName(name string, yes bool) string {
	if yes {
		return name
	}
	return "no_" + name
}

func TestRuntimeHookRepairWaitsForCanceledDescendants(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(testBoolName("deadline", deadline), func(t *testing.T) {
			shortRepairDelay(t)
			childStarted := make(chan struct{})
			childRelease := make(chan struct{})
			parentObserved := make(chan int, 1)
			var release sync.Once
			runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Hooks: []CommittedRunHook{{Name: "audit", Handle: func(ctx context.Context, snapshot RunSnapshot) error {
				if snapshot.Parent != nil {
					close(childStarted)
					select {
					case <-childRelease:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				parentObserved <- snapshot.Accounting.Tree.Unsettled
				return nil
			}}}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { release.Do(func() { close(childRelease) }); _ = runtime.Close() })
			parent := testAgent(&scriptedProvider{turns: []Message{asstTool("c1", "delegate", `{"topic":"tea"}`), asstText("done")}})
			newSharedChildDefinition(parent)
			for id, agent := range map[string]*Agent{"child": questionChild(), "parent": parent} {
				if _, err := runtime.Register(id, "v1", agent); err != nil {
					t.Fatal(err)
				}
			}
			handle, err := runtime.Submit(context.Background(), DefinitionRef{ID: "parent", Revision: "v1"}, "go")
			if err != nil {
				t.Fatal(err)
			}
			snapshot := waitForRunState(t, runtime, handle.ID(), RuntimeWaiting)
			childID := snapshot.ToolBatches[0].Invocations[0].ChildRunID
			waitForRunState(t, runtime, childID, RuntimeWaiting)
			waitForWorkerExit(t, runtime, childID)
			waitForWorkerExit(t, runtime, handle.ID())
			done := make(chan error, 1)
			if deadline {
				if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
					record, err := getRuntimeRun(tx, handle.ID())
					if err != nil {
						return err
					}
					record.Deadline = time.Now().Add(-time.Second)
					return putRuntimeRun(tx, record)
				}); err != nil {
					t.Fatal(err)
				}
				go func() {
					record, err := runtime.compactRecord(context.Background(), handle.ID())
					if err == nil {
						err = runtime.maintainSuspended(context.Background(), record, runtime.goCompleteRunHooks)
					}
					done <- err
				}()
			} else {
				go func() { done <- handle.Cancel(context.Background()) }()
			}
			select {
			case <-childStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("child hook did not start")
			}
			// Explicit repair plus multiple timer firings must not overtake the child.
			runtime.maintainRun(context.Background(), handle.ID())
			select {
			case unsettled := <-parentObserved:
				t.Fatalf("parent hook overtook child: unsettled=%d", unsettled)
			case <-time.After(50 * time.Millisecond):
			}
			release.Do(func() { close(childRelease) })
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("finalizer did not return")
			}
			select {
			case unsettled := <-parentObserved:
				if unsettled != 0 {
					t.Fatalf("parent saw %d unsettled runs", unsettled)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("parent hook did not finish")
			}
			_, _ = awaitWithin(t, handle, 2*time.Second)
		})
	}
}

func TestRuntimeCloseWaitsForCancellationHooks(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Hooks: []CommittedRunHook{{Name: "audit", Handle: func(ctx context.Context, _ RunSnapshot) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { once.Do(func() { close(release) }); _ = runtime.Close() })
	handle, _ := submitWaitingQuestion(t, runtime)
	waitForWorkerExit(t, runtime, handle.ID())
	canceled := make(chan error, 1)
	go func() { canceled <- handle.Cancel(context.Background()) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("hook did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- runtime.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close overtook cancellation hook: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish")
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("Cancel did not finish")
	}
}
