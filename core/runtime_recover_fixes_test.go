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

// A graceful Close after the provider answered leaves the run in execution
// attention at provider_accepted. Recovery must finish it from the saved
// answer, as it does after a hard crash at the same point (bug review L3).
func TestRecoverFinishesAcceptedAnswerAfterGracefulClose(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	messages := []Message{UserMessage("work"), asstText("the answer")}
	record := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: "closed-after-answer", DefinitionID: "agent", DefinitionRevision: "v1",
		Task: "work", State: RuntimeRunning, Generation: 3,
		Result:         RunResult{RunID: "closed-after-answer", Turns: 1, ProviderAttempts: 1, FinalMessage: messages[1]},
		LastTransition: transitionProviderAccepted,
	}
	setRunAttention(&record, AttentionExecution, "worker stopped during execution")
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		if err := appendTranscript(tx, record.RunID, &record, messages); err != nil {
			return err
		}
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}

	provider := &scriptedProvider{}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Handle(record.RunID).Await(context.Background())
	if err != nil || result.Output != "the answer" || result.Status != RunCompleted {
		t.Fatalf("recovered result = %#v, %v", result, err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider called %d times; the accepted answer must not be requested again", provider.calls)
	}
}
