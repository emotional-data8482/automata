package core

import (
	"context"
	"sync"
	"testing"
)

// Canceling a suspended run reaches committed-run hooks for it and for every
// suspended descendant the cancellation finalizes, deepest first, and each
// run's event log records the finalizing step before the terminal one.
func TestRuntimeCancelOfSuspendedRunsDeliversHooks(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	statuses := map[string]RunStatus{}
	hooks := []CommittedRunHook{{Name: "audit", Handle: func(_ context.Context, snapshot RunSnapshot) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, snapshot.RunID)
		statuses[snapshot.RunID] = snapshot.Result.Status
		return nil
	}}}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	child := testAgent(&scriptedProvider{turns: []Message{
		asstTool("g1", "delegate2", `{"topic":"tea"}`),
		asstText("child done"),
	}})
	child.RegisterTool(NewChildTool(childTestDefinition("delegate2"), DefinitionRef{ID: "grandchild", Revision: "v1"}))
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	for id, agent := range map[string]*Agent{"grandchild": questionChild(), "child": child, "parent": parent} {
		if _, err := runtime.Register(id, "v1", agent); err != nil {
			t.Fatal(err)
		}
	}
	handle, err := runtime.Submit(context.Background(), DefinitionRef{ID: "parent", Revision: "v1"}, "go")
	if err != nil {
		t.Fatal(err)
	}
	waitForRunState(t, runtime, handle.ID(), RuntimeWaiting)
	parentSnapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childID := parentSnapshot.ToolBatches[0].Invocations[0].ChildRunID
	childSnapshot := waitForRunState(t, runtime, childID, RuntimeWaiting)
	grandchildID := childSnapshot.ToolBatches[0].Invocations[0].ChildRunID
	waitForRunState(t, runtime, grandchildID, RuntimeWaiting)
	waitForWorkerExit(t, runtime, grandchildID)

	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	want := []string{grandchildID, childID, handle.ID()}
	if len(got) != len(want) {
		t.Fatalf("hooks saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hooks saw %v, want deepest first %v", got, want)
		}
		if statuses[want[i]] != RunCancelled {
			t.Fatalf("hook saw run %s as %s", want[i], statuses[want[i]])
		}
	}
	for _, runID := range want {
		snapshot, err := runtime.Handle(runID).Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State != RuntimeTerminal || len(snapshot.Hooks) != 1 || snapshot.Hooks[0].Error != "" {
			t.Fatalf("run %s = %s hooks %+v", runID, snapshot.State, snapshot.Hooks)
		}
		page, err := runtime.Handle(runID).Events(context.Background(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var states []RuntimeState
		for _, event := range page.Events {
			if event.Kind == CommittedRunState {
				states = append(states, event.State)
			}
		}
		if n := len(states); n < 2 || states[n-2] != RuntimeFinalizing || states[n-1] != RuntimeTerminal {
			t.Fatalf("run %s state events = %v, want ... finalizing, terminal", runID, states)
		}
	}
}

// A canceled suspended run whose hook delivery never started (its canceler
// stopped after the commit) is finished by Recover without losing the hooks.
func TestRuntimeRecoverFinishesCanceledSuspendedRunHooks(t *testing.T) {
	store := NewMemoryStore()
	var calls int
	var mu sync.Mutex
	hooks := []CommittedRunHook{{Name: "audit", Handle: func(context.Context, RunSnapshot) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil
	}}}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	handle, _ := submitWaitingQuestion(t, runtime)
	// Commit the cancellation exactly as Cancel does, without delivering.
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, handle.ID())
		if err != nil {
			return err
		}
		if err := cancelRunWaits(tx, record.RunID); err != nil {
			return err
		}
		setRuntimeError(&record, context.Canceled)
		_, err = finalizeCanceledTx(tx, &record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := NewRuntime(context.Background(), RuntimeConfig{Store: store, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	if err := recovered.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := recovered.Handle(handle.ID()).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if snapshot.State != RuntimeTerminal || snapshot.Result.Status != RunCancelled || calls != 1 {
		t.Fatalf("recovered = %s %s, hook calls %d", snapshot.State, snapshot.Result.Status, calls)
	}
}
