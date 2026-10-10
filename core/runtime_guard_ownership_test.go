package core

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
)

// guardedCreateTool is a mutating tool whose semantic guard is keyed by the
// "resource" argument, so every run on the store shares one guard per resource.
func guardedCreateTool(executions *atomic.Int32) Tool {
	tool := FuncResult("create", "create", func(context.Context, struct {
		Resource string `json:"resource"`
	}) (ToolResult, error) {
		executions.Add(1)
		result := TextResult("created")
		result.Effect = EffectReport{Status: EffectApplied, Receipt: "charge"}
		return result, nil
	})
	return WithToolEffectPolicy(tool, ToolEffectPolicy{
		Kind: ToolEffectMutating, Scope: "cards",
		SemanticKey: func(raw json.RawMessage) (string, error) {
			var value struct {
				Resource string `json:"resource"`
			}
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", err
			}
			return value.Resource, nil
		},
	})
}

// seedGuardedDispatch stores run runID with one dispatched "create" call that
// owns the semantic guard for resource "card".
func seedGuardedDispatch(t *testing.T, store Store, runID string) (storedToolInvocation, string) {
	t.Helper()
	seedPendingRuntimeBatch(t, store, runID, []ToolUseBlock{toolUse("charge-id", "create", `{"resource":"card"}`)}, []ToolInvocationState{ToolInvocationDispatched})
	guardKey := effectGuardStorageKey("cards", "create", "card")
	var invocation storedToolInvocation
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		key := invocationStorageKey(runID, "0000000000000000", 0)
		raw, err := tx.Get(runtimeInvocationsBucket, key)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &invocation); err != nil {
			return err
		}
		invocation.GuardKey = guardKey
		invocation.GuardDisplay = "cards:card"
		if err := putStoredJSON(tx, runtimeInvocationsBucket, key, invocation); err != nil {
			return err
		}
		return putStoredJSON(tx, runtimeEffectGuardsBucket, guardKey, storedEffectGuard{OperationID: invocation.OperationID, Status: EffectUnknown})
	}); err != nil {
		t.Fatal(err)
	}
	return invocation, guardKey
}

func putGuard(t *testing.T, store Store, key string, guard storedEffectGuard) {
	t.Helper()
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		return putStoredJSON(tx, runtimeEffectGuardsBucket, key, guard)
	}); err != nil {
		t.Fatal(err)
	}
}

func readGuard(t *testing.T, store Store, key string) storedEffectGuard {
	t.Helper()
	var guard storedEffectGuard
	if err := store.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		raw, err := tx.Get(runtimeEffectGuardsBucket, key)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &guard)
	}); err != nil {
		t.Fatal(err)
	}
	return guard
}

// Run A's charge is lost (uncertain), run B later owns the guard and applies
// the charge, then an operator reconciles A as not applied. Run C must still
// be denied by B's applied guard.
func TestRuntimeStaleReconcileKeepsNewerGuardOwner(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	invocationA, guardKey := seedGuardedDispatch(t, base, "run-a")

	var executions atomic.Int32
	provider := &scriptedProvider{turns: []Message{
		asstText("a continues"),
		asstTool("charge-c", "create", `{"resource":"card"}`),
		asstText("c done"),
	}}
	agent := testAgent(provider)
	agent.RegisterTool(guardedCreateTool(&executions))
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	handleA := runtime.Handle("run-a")
	snapshot, err := handleA.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeNeedsAttention || snapshot.ToolBatches[0].Invocations[0].State != ToolInvocationUncertain {
		t.Fatalf("run A = %#v", snapshot)
	}

	// Run B owns the guard and has applied the charge.
	applied := storedEffectGuard{OperationID: "run-b:0000000000000000:0", Status: EffectApplied}
	putGuard(t, base, guardKey, applied)

	resolution := EffectResolution{Result: TextResult("charge was not taken"), Effect: EffectReport{Status: EffectNotApplied}}
	if err := handleA.Reconcile(context.Background(), invocationA.OperationID, resolution); err != nil {
		t.Fatal(err)
	}
	if got := readGuard(t, base, guardKey); got != applied {
		t.Fatalf("guard after stale reconcile = %#v, want %#v", got, applied)
	}
	if result, err := handleA.Await(context.Background()); err != nil || result.Output != "a continues" {
		t.Fatalf("run A = %#v, %v", result, err)
	}
	if err := handleA.Reconcile(context.Background(), invocationA.OperationID, resolution); err != nil {
		t.Fatalf("exact reconcile retry = %v", err)
	}
	changed := resolution
	changed.Effect = EffectReport{Status: EffectApplied}
	if err := handleA.Reconcile(context.Background(), invocationA.OperationID, changed); !errors.Is(err, ErrReconciliationConflict) {
		t.Fatalf("conflicting reconcile = %v", err)
	}

	result, err := runtime.Run(context.Background(), DefinitionRef{ID: "agent", Revision: "v1"}, "charge again")
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 0 {
		t.Fatalf("run C charged the card %d times; want the guard to deny it", executions.Load())
	}
	results := transcriptToolResults(result.Messages)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("run C results = %#v", results)
	}
}

// A worker that finishes after its invocation lost the guard must leave the
// new owner's guard unchanged.
func TestRuntimeLateCompletionKeepsNewerGuardOwner(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	invocation, guardKey := seedGuardedDispatch(t, base, "run-late")
	owner := storedEffectGuard{OperationID: "run-b:0000000000000000:0", Status: EffectUnknown}
	putGuard(t, base, guardKey, owner)

	returned := ErrorResult("declined")
	returned.Effect = EffectReport{Status: EffectNotApplied}
	if err := runtime.completeToolInvocation(context.Background(), invocation, returned, nil, returned, false); err != nil {
		t.Fatal(err)
	}
	if got := readGuard(t, base, guardKey); got != owner {
		t.Fatalf("guard after late completion = %#v, want %#v", got, owner)
	}
	stored := readInvocation(t, base, invocation)
	if stored.State != ToolInvocationCompleted || stored.Effect.Status != EffectNotApplied {
		t.Fatalf("late completion did not record its own invocation: %#v", stored)
	}
}

// A worker that finishes after its invocation was settled elsewhere (here by
// Reconcile) must not replace that outcome or the guard.
func TestRuntimeLateCompletionAfterReconcileIsIgnored(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	invocation, guardKey := seedGuardedDispatch(t, base, "run-settled")
	settled := invocation
	settled.State = ToolInvocationCompleted
	settled.Result = TextResult("verified applied")
	settled.Effect = EffectReport{Status: EffectApplied, Receipt: "verified"}
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		return putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(settled.RunID, settled.BatchID, settled.Ordinal), settled)
	}); err != nil {
		t.Fatal(err)
	}
	owner := storedEffectGuard{OperationID: invocation.OperationID, Status: EffectApplied}
	putGuard(t, base, guardKey, owner)

	returned := ErrorResult("timed out")
	returned.Effect = EffectReport{Status: EffectNotApplied}
	if err := runtime.completeToolInvocation(context.Background(), invocation, returned, nil, returned, false); err != nil {
		t.Fatal(err)
	}
	if got := readGuard(t, base, guardKey); got != owner {
		t.Fatalf("guard after late completion = %#v, want %#v", got, owner)
	}
	if stored := readInvocation(t, base, invocation); stored.Effect.Receipt != "verified" {
		t.Fatalf("late completion replaced the settled outcome: %#v", stored)
	}
}

func readInvocation(t *testing.T, store Store, invocation storedToolInvocation) storedToolInvocation {
	t.Helper()
	var stored storedToolInvocation
	if err := store.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		raw, err := tx.Get(runtimeInvocationsBucket, invocationStorageKey(invocation.RunID, invocation.BatchID, invocation.Ordinal))
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &stored)
	}); err != nil {
		t.Fatal(err)
	}
	return stored
}
