package core

import (
	"context"
	"errors"
	"testing"
)

// Recover acts on records it scanned earlier. A cancel-requested run that its
// worker finishes before Recover reaches it must stay terminal (bug review L1).
func TestRecoverDoesNotRewriteRunFinishedAfterScan(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	messages := []Message{UserMessage("work"), asstText("partial")}
	scanned := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: "cancel-race", DefinitionID: "agent", DefinitionRevision: "v1",
		Task: "work", State: RuntimeCancelRequested, Generation: 2,
		Result:         RunResult{RunID: "cancel-race", Turns: 1, FinalMessage: messages[1]},
		LastTransition: transitionProviderAccepted,
	}
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		if err := appendTranscript(tx, scanned.RunID, &scanned, messages); err != nil {
			return err
		}
		return putRuntimeRun(tx, scanned)
	}); err != nil {
		t.Fatal(err)
	}
	if got := classifyRecovery(scanned, recoveryFacts{bindingRegistered: true, payloadFits: true}); got != recoveryCancel {
		t.Fatalf("scanned record classifies as %v, want cancel", got)
	}

	// The worker finishes the run after Recover scanned it.
	finished := scanned
	finished.State = RuntimeTerminal
	finished.Generation = 7
	finished.Result.Status = RunCancelled
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		return putRuntimeRun(tx, finished)
	}); err != nil {
		t.Fatal(err)
	}

	if err := runtime.recoverRecord(context.Background(), scanned); err != nil {
		t.Fatal(err)
	}
	var current storedRuntimeRun
	active := false
	if err := base.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		var err error
		current, err = getRuntimeRun(tx, scanned.RunID)
		if err != nil {
			return err
		}
		if _, err := tx.Get(runtimeActiveBucket, scanned.RunID); err == nil {
			active = true
		} else if !errors.Is(err, ErrStoreKeyNotFound) {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if current.State != RuntimeTerminal || current.Generation != 7 || active {
		t.Fatalf("finished run became %s gen %d (active=%v); want terminal gen 7", current.State, current.Generation, active)
	}
}
