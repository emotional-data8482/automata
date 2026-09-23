package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/retry"
)

// childTestDefinition is a minimal valid input schema for a declared child
// tool; DurableChildTool rejects unsupported or non-object schemas eagerly.
func childTestDefinition(name string) ToolDefinition {
	return ToolDefinition{
		Name:        name,
		Description: "delegate to a durable child",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
	}
}

// childTestInvocation builds a complete, non-placeholder invocation identity
// for admission boundary tests.
func childTestInvocation(runID, batchID string, ordinal int) storedToolInvocation {
	return storedToolInvocation{
		Version:     runtimeEncodingVersion,
		RunID:       runID,
		BatchID:     batchID,
		OperationID: runID + ":" + batchID + ":" + itoa(ordinal),
		Ordinal:     ordinal,
		Call:        ToolUseBlock{ID: "c1", Name: "delegate", Input: json.RawMessage(`{"topic":"tea"}`)},
		State:       ToolInvocationReserved,
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0000000000000000"
	}
	return "0000000000000001"
}

// admitInTx runs admitChildRun inside its own writable transaction so tests
// exercise the same atomic boundary the batch integration will share.
func admitInTx(t *testing.T, runtime *Runtime, parent storedRuntimeRun, invocation storedToolInvocation, policy DurableChildPolicy, task string) (childAdmissionReceipt, error) {
	t.Helper()
	var receipt childAdmissionReceipt
	err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		var txErr error
		receipt, txErr = runtime.admitChildRun(tx, parent, invocation, policy, task)
		return txErr
	})
	return receipt, err
}

func childTestParent() storedRuntimeRun {
	deadline := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: "parent-1", DefinitionID: "agent",
		DefinitionRevision: "v1", Task: "parent task", Deadline: deadline,
		State: RuntimeRunning, Generation: 2, Result: RunResult{RunID: "parent-1"},
	}
}

func countChildRuns(t *testing.T, runtime *Runtime) int {
	t.Helper()
	count := 0
	err := runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		return tx.Scan(runtimeRunsBucket, "", func(_ string, raw []byte) error {
			record, err := decodeRuntimeRun(raw)
			if err != nil {
				return err
			}
			if record.ParentRunID != "" {
				count++
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func countLinks(t *testing.T, runtime *Runtime) int {
	t.Helper()
	count := 0
	err := runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		return tx.Scan(runtimeChildLinksBucket, "", func(string, []byte) error {
			count++
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// --- admission identity and idempotency --------------------------------------

func TestChildAdmissionIsIdempotentByParentOperation(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("parent")}})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("child-def", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("child")}})); err != nil {
		t.Fatal(err)
	}
	parent := childTestParent()
	invocation := childTestInvocation(parent.RunID, "0000000000000000", 0)
	policy := DurableChildPolicy{DefinitionID: "child-def", Revision: "v1"}

	first, err := admitInTx(t, runtime, parent, invocation, policy, "research tea")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created {
		t.Fatal("first admission was not created")
	}
	child, err := runtime.load(context.Background(), first.ChildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentRunID != parent.RunID || child.ParentOperationID != invocation.OperationID {
		t.Fatalf("child parentage = %q/%q", child.ParentRunID, child.ParentOperationID)
	}
	if !child.Deadline.Equal(parent.Deadline) {
		t.Fatalf("child deadline = %v, want inherited %v", child.Deadline, parent.Deadline)
	}
	if child.State != RuntimeReady || child.DefinitionID != "child-def" {
		t.Fatalf("child record = %#v", child)
	}

	// A lost acknowledgement replaying the exact admission resolves the
	// original child; no duplicate or orphan is created.
	second, err := admitInTx(t, runtime, parent, invocation, policy, "research tea")
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.ChildRunID != first.ChildRunID || second.WaitID != first.WaitID {
		t.Fatalf("replayed admission = %#v, want original %#v", second, first)
	}
	if got := countChildRuns(t, runtime); got != 1 {
		t.Fatalf("child runs after replay = %d, want 1", got)
	}
	if got := countLinks(t, runtime); got != 1 {
		t.Fatalf("child links after replay = %d, want 1", got)
	}
	var wait storedWait
	err = runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		raw, err := tx.Get(runtimeWaitsBucket, waitStorageKey(parent.RunID, first.WaitID))
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &wait)
	})
	if err != nil {
		t.Fatal(err)
	}
	if wait.Kind != WaitChild || wait.State != WaitPending || wait.ChildRunID != first.ChildRunID ||
		wait.OperationID != invocation.OperationID || wait.BatchID != invocation.BatchID || wait.Ordinal != invocation.Ordinal {
		t.Fatalf("child wait = %#v", wait)
	}
}

func TestChildAdmissionDistinctOperationsCreateDistinctChildren(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("child-def", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	parent := childTestParent()
	policy := DurableChildPolicy{DefinitionID: "child-def", Revision: "v1"}

	// Two operations that carry the same model call ID are distinct durable
	// invocations: identity is the parent operation, not the call ID.
	firstCall := childTestInvocation(parent.RunID, "0000000000000000", 0)
	secondCall := childTestInvocation(parent.RunID, "0000000000000001", 0)
	secondCall.Call.ID = firstCall.Call.ID

	first, err := admitInTx(t, runtime, parent, firstCall, policy, "first task")
	if err != nil {
		t.Fatal(err)
	}
	second, err := admitInTx(t, runtime, parent, secondCall, policy, "first task")
	if err != nil {
		t.Fatal(err)
	}
	if first.ChildRunID == second.ChildRunID || first.WaitID == second.WaitID {
		t.Fatalf("distinct operations resolved the same child: %#v vs %#v", first, second)
	}
	if got := countChildRuns(t, runtime); got != 2 {
		t.Fatalf("child runs = %d, want 2", got)
	}
	if got := countLinks(t, runtime); got != 2 {
		t.Fatalf("child links = %d, want 2", got)
	}
}

func TestChildAdmissionConflictOnChangedPayload(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("child-def", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	parent := childTestParent()
	invocation := childTestInvocation(parent.RunID, "0000000000000000", 0)
	policy := DurableChildPolicy{DefinitionID: "child-def", Revision: "v1"}
	if _, err := admitInTx(t, runtime, parent, invocation, policy, "research tea"); err != nil {
		t.Fatal(err)
	}
	if _, err := admitInTx(t, runtime, parent, invocation, DurableChildPolicy{DefinitionID: "other-def", Revision: "v1"}, "research tea"); !errors.Is(err, errChildAdmissionConflict) {
		t.Fatalf("changed definition = %v", err)
	}
	if _, err := admitInTx(t, runtime, parent, invocation, policy, "research coffee"); !errors.Is(err, errChildAdmissionConflict) {
		t.Fatalf("changed task = %v", err)
	}
	if got := countChildRuns(t, runtime); got != 1 {
		t.Fatalf("conflicting admission created child runs = %d, want 1", got)
	}
	if got := countLinks(t, runtime); got != 1 {
		t.Fatalf("conflicting admission created links = %d, want 1", got)
	}
}

// --- fail-closed boundaries --------------------------------------------------

func TestChildAdmissionRequiresRegisteredBinding(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	parent := childTestParent()
	invocation := childTestInvocation(parent.RunID, "0000000000000000", 0)
	if _, err := admitInTx(t, runtime, parent, invocation, DurableChildPolicy{DefinitionID: "missing", Revision: "v1"}, "task"); !errors.Is(err, ErrDefinitionNotRegistered) {
		t.Fatalf("missing binding = %v", err)
	}
	if _, err := admitInTx(t, runtime, parent, invocation, DurableChildPolicy{DefinitionID: "agent", Revision: "unregistered"}, "task"); !errors.Is(err, ErrDefinitionNotRegistered) {
		t.Fatalf("unregistered revision = %v", err)
	}
	if got := countChildRuns(t, runtime); got != 0 {
		t.Fatalf("failed admission created child runs = %d", got)
	}
	if got := countLinks(t, runtime); got != 0 {
		t.Fatalf("failed admission created links = %d", got)
	}
}

func TestChildAdmissionRequiresCompleteIdentityAndTask(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("child-def", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	parent := childTestParent()
	policy := DurableChildPolicy{DefinitionID: "child-def", Revision: "v1"}

	noBatch := childTestInvocation(parent.RunID, "", 0)
	if _, err := admitInTx(t, runtime, parent, noBatch, policy, "task"); err == nil {
		t.Fatal("admission without a batch identity was accepted")
	}
	badOrdinal := childTestInvocation(parent.RunID, "0000000000000000", -1)
	if _, err := admitInTx(t, runtime, parent, badOrdinal, policy, "task"); err == nil {
		t.Fatal("admission with a negative ordinal was accepted")
	}
	noOperation := childTestInvocation(parent.RunID, "0000000000000000", 0)
	noOperation.OperationID = ""
	if _, err := admitInTx(t, runtime, parent, noOperation, policy, "task"); err == nil {
		t.Fatal("admission without an operation identity was accepted")
	}
	complete := childTestInvocation(parent.RunID, "0000000000000000", 0)
	if _, err := admitInTx(t, runtime, parent, complete, policy, ""); err == nil {
		t.Fatal("admission without a projected task was accepted")
	}
	if got := countChildRuns(t, runtime) + countLinks(t, runtime); got != 0 {
		t.Fatalf("failed admissions persisted records = %d", got)
	}
}

func TestChildAdmissionRollbackPersistsNothing(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("child-def", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	parent := childTestParent()
	invocation := childTestInvocation(parent.RunID, "0000000000000000", 0)
	policy := DurableChildPolicy{DefinitionID: "child-def", Revision: "v1"}

	err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		if _, err := runtime.admitChildRun(tx, parent, invocation, policy, "research tea"); err != nil {
			return err
		}
		// The transaction is forced to fail after every admission write, so an
		// atomic store must discard all of them.
		return errors.New("injected rollback")
	})
	if err == nil || !strings.Contains(err.Error(), "injected rollback") {
		t.Fatalf("rollback = %v", err)
	}
	if got := countChildRuns(t, runtime) + countLinks(t, runtime); got != 0 {
		t.Fatalf("rolled-back admission persisted records = %d", got)
	}
	var pending int
	err = runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		return tx.Scan(runtimeWaitsBucket, parent.RunID+"/", func(string, []byte) error {
			pending++
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("rolled-back admission persisted waits = %d", pending)
	}

	// Nothing partial survived: the next admission creates a fresh child.
	receipt, err := admitInTx(t, runtime, parent, invocation, policy, "research tea")
	if err != nil || !receipt.Created {
		t.Fatalf("post-rollback admission = %#v, %v", receipt, err)
	}
}

// --- host-facing boundary ----------------------------------------------------

func TestChildWaitCannotBeResolvedByHost(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("child-def", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}
	parent := childTestParent()
	invocation := childTestInvocation(parent.RunID, "0000000000000000", 0)
	receipt, err := admitInTx(t, runtime, parent, invocation, DurableChildPolicy{DefinitionID: "child-def", Revision: "v1"}, "research tea")
	if err != nil {
		t.Fatal(err)
	}
	handle := runtime.Handle(parent.RunID)
	if err := handle.ResolveWait(context.Background(), receipt.WaitID, WaitResolution{Decision: Allow}); !errors.Is(err, errChildWaitHostResolution) {
		t.Fatalf("host child-wait resolution = %v", err)
	}
	var wait storedWait
	err = runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		raw, err := tx.Get(runtimeWaitsBucket, waitStorageKey(parent.RunID, receipt.WaitID))
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &wait)
	})
	if err != nil {
		t.Fatal(err)
	}
	if wait.State != WaitPending {
		t.Fatalf("child wait state = %q, want pending", wait.State)
	}
}

func TestRuntimeRejectsVersion6Store(t *testing.T) {
	base := NewMemoryStore()
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		data, _ := json.Marshal(6)
		return tx.Put(runtimeMetaBucket, "encoding", data)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}}); err == nil ||
		!strings.Contains(err.Error(), "unsupported runtime storage version 6") {
		t.Fatalf("version 6 store = %v", err)
	}
}

// --- registration boundary ---------------------------------------------------

func TestRegisterValidatesDurableChildTool(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{})); err != nil {
		t.Fatal(err)
	}

	agent := testAgent(&scriptedProvider{}).WithToolPolicy(ToolPolicy{MaxCalls: 2})
	agent.RegisterTool(DurableChildTool(childTestDefinition("delegate"), DurableChildPolicy{DefinitionID: "agent", Revision: "v1"}))
	if err := runtime.Register("parent", "v1", agent); err != nil {
		t.Fatalf("valid durable child rejected: %v", err)
	}

	// legacyError adaptation preserves child metadata by delegation.
	legacy := testAgent(&scriptedProvider{})
	legacy.RegisterTool(WithLegacyToolErrors(DurableChildTool(childTestDefinition("delegate"), DurableChildPolicy{DefinitionID: "agent", Revision: "v1"})))
	if err := runtime.Register("legacy-parent", "v1", legacy); err != nil {
		t.Fatalf("legacyError-wrapped durable child rejected: %v", err)
	}

	invalidSchema := testAgent(&scriptedProvider{})
	invalidSchema.RegisterTool(DurableChildTool(ToolDefinition{Name: "bad", InputSchema: json.RawMessage(`{"type":"array"}`)}, DurableChildPolicy{DefinitionID: "agent", Revision: "v1"}))
	if err := runtime.Register("bad-schema", "v1", invalidSchema); err == nil || !strings.Contains(err.Error(), "input schema must be an object schema") {
		t.Fatalf("invalid schema = %v", err)
	}
	unparsableSchema := testAgent(&scriptedProvider{})
	unparsableSchema.RegisterTool(DurableChildTool(ToolDefinition{Name: "bad", InputSchema: json.RawMessage(`{`)}, DurableChildPolicy{DefinitionID: "agent", Revision: "v1"}))
	if err := runtime.Register("bad-json", "v1", unparsableSchema); err == nil {
		t.Fatal("unparsable schema was accepted")
	}
	missingPolicy := testAgent(&scriptedProvider{})
	missingPolicy.RegisterTool(DurableChildTool(childTestDefinition("bad"), DurableChildPolicy{}))
	if err := runtime.Register("bad-policy", "v1", missingPolicy); err == nil || !strings.Contains(err.Error(), "definition id and revision") {
		t.Fatalf("missing policy identity = %v", err)
	}
}

func TestRegisterRejectsUnsafeDurableChildWrappers(t *testing.T) {
	runtime := newTestRuntime(t)
	build := func(wrap func(Tool) Tool) *Agent {
		agent := testAgent(&scriptedProvider{})
		agent.RegisterTool(wrap(DurableChildTool(childTestDefinition("delegate"), DurableChildPolicy{DefinitionID: "agent", Revision: "v1"})))
		return agent
	}
	cases := []struct {
		name  string
		agent *Agent
		want  string
	}{
		{"retry would repeat the child run", build(func(t Tool) Tool { return WithToolRetry(t, retry.Config{MaxAttempts: 2}) }), "cannot be retried"},
		{"durable wait would double-suspend", build(func(t Tool) Tool { return WithDurableWait(t, DurableWaitPolicy{Kind: WaitQuestion}) }), "durable wait"},
		{"effect policy bypasses child-run ownership", build(func(t Tool) Tool { return WithToolEffectPolicy(t, ToolEffectPolicy{Kind: ToolEffectMutating}) }), "effect policy"},
		{"wrapper order cannot hide the child", build(func(t Tool) Tool { return WithLegacyToolErrors(WithToolRetry(t, retry.Config{MaxAttempts: 2})) }), "cannot be retried"},
	}
	for _, tc := range cases {
		if err := runtime.Register(tc.name, "v1", tc.agent); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

func TestInspectChildToolDetectsTransientAdaptersThroughWrappers(t *testing.T) {
	sub := testAgent(&scriptedProvider{})
	direct := AsTool[struct{}](sub, "sub", "delegate")
	rendered := AsToolFunc[struct{}](sub, "sub", "delegate", func(struct{}) string { return "task" })
	wrapped := WithLegacyToolErrors(AsTool[struct{}](sub, "sub", "delegate"))
	retryWrapped := WithToolRetry(WithLegacyToolErrors(direct), retry.Config{MaxAttempts: 2})
	plain := Func("echo", "echo", func(context.Context, struct{}) (string, error) { return "ok", nil })
	child := DurableChildTool(childTestDefinition("delegate"), DurableChildPolicy{DefinitionID: "child-def", Revision: "v1"})

	for _, tool := range []Tool{direct, rendered, wrapped, retryWrapped} {
		inspection, err := inspectChildTool(tool)
		if err != nil {
			t.Fatalf("%T: %v", tool, err)
		}
		if !inspection.transientAdapter || inspection.hasChild {
			t.Fatalf("%T inspection = %#v", tool, inspection)
		}
	}
	inspection, err := inspectChildTool(plain)
	if err != nil || inspection.transientAdapter || inspection.hasChild {
		t.Fatalf("plain tool inspection = %#v, %v", inspection, err)
	}
	inspection, err = inspectChildTool(child)
	if err != nil || inspection.transientAdapter || !inspection.hasChild ||
		inspection.child.DefinitionID != "child-def" || inspection.child.Revision != "v1" {
		t.Fatalf("durable child inspection = %#v, %v", inspection, err)
	}
}

// --- process-local boundary --------------------------------------------------

func TestDurableChildToolExecuteFailsOutsideRuntime(t *testing.T) {
	child := DurableChildTool(childTestDefinition("delegate"), DurableChildPolicy{DefinitionID: "child-def", Revision: "v1"})
	if _, err := child.Execute(context.Background(), json.RawMessage(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "only through Runtime admission") {
		t.Fatalf("execute outside runtime = %v", err)
	}
}

// --- slice 2: execution, completion, wake, and recovery ----------------------

// childBarrierProvider blocks until its barrier releases, honoring worker
// cancellation so tests can close a runtime deterministically.
type childBarrierProvider struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	calls       atomic.Int32
}

func (p *childBarrierProvider) Invoke(ctx context.Context, _ Request) (Response, error) {
	p.calls.Add(1)
	p.startedOnce.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return fixtureResponse(asstText("child done")), nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// stageWaitingParentWithTerminalChild writes the exact torn recovery state:
// a waiting parent holding one pending child wait whose linked child run is
// already durably terminal, with the child wake lost. Recover must consume
// the completion exactly once and continue the parent.
func stageWaitingParentWithTerminalChild(t *testing.T, runtime *Runtime, parentID string, childID string) string {
	t.Helper()
	batchID := "0000000000000000"
	operationID := parentID + ":" + batchID + ":0"
	call := ToolUseBlock{ID: "c1", Name: "delegate", Input: json.RawMessage(`{"topic":"tea"}`)}
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		parent := storedRuntimeRun{
			Version: runtimeEncodingVersion, RunID: parentID, DefinitionID: "parent", DefinitionRevision: "v1",
			Task: "go", State: RuntimeWaiting, Generation: 1, Result: RunResult{RunID: parentID},
			PendingBatchID: batchID, NextBatchOrdinal: 1,
		}
		if err := putRuntimeRun(tx, parent); err != nil {
			return err
		}
		batch := storedToolBatch{Version: runtimeEncodingVersion, RunID: parentID, BatchID: batchID, Ordinal: 0, Count: 1}
		if err := putStoredJSON(tx, runtimeBatchesBucket, batchStorageKey(parentID, batchID), batch); err != nil {
			return err
		}
		invocation := storedToolInvocation{
			Version: runtimeEncodingVersion, RunID: parentID, BatchID: batchID,
			OperationID: parentID + ":" + batchID + ":0", Ordinal: 0,
			Call: call, State: ToolInvocationReserved,
			WaitID:     childWaitIDFor(operationID),
			ChildRunID: childID,
		}
		if err := putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(parentID, batchID, 0), invocation); err != nil {
			return err
		}
		link := storedChildLink{
			Version: runtimeEncodingVersion, ParentRunID: parentID,
			OperationID: invocation.OperationID, ChildRunID: childID,
			DefinitionID: "child", DefinitionRevision: "v1",
			TaskDigest: childAdmissionDigest(childAdmissionPayload{DefinitionID: "child", Revision: "v1", Task: string(call.Input)}),
			CreatedAt:  time.Now().UTC(),
		}
		if err := putStoredJSON(tx, runtimeChildLinksBucket, childLinkKey(parentID, invocation.OperationID), link); err != nil {
			return err
		}
		wait := storedWait{
			Version: runtimeEncodingVersion, RunID: parentID, ID: childWaitIDFor(invocation.OperationID),
			Kind: WaitChild, State: WaitPending, OperationID: invocation.OperationID,
			BatchID: batchID, Ordinal: 0, Tool: call.Name, Arguments: append(json.RawMessage(nil), call.Input...),
			DefinitionID: "child", DefinitionRevision: "v1", ChildRunID: childID, CreatedAt: time.Now().UTC(),
		}
		if err := putStoredWait(tx, wait); err != nil {
			return err
		}
		// The seeded child ran as an ordinary standalone run; make it
		// self-identifying as this parent's admitted child so recovery sees a
		// consistent child/link/wait triple.
		childRecord, err := getRuntimeRun(tx, childID)
		if err != nil {
			return err
		}
		childRecord.ParentRunID = parentID
		childRecord.ParentOperationID = invocation.OperationID
		return putRuntimeRun(tx, childRecord)
	}); err != nil {
		t.Fatal(err)
	}
	return childID
}
func newSharedChildDefinition(parent *Agent) {
	parent.RegisterTool(DurableChildTool(childTestDefinition("delegate"), DurableChildPolicy{DefinitionID: "child", Revision: "v1"}))
}

// A mixed batch suspends entirely on the child wait; after the child settles
// the remaining ordinary sibling dispatches and the batch commits in model
// order with the child projection first.
func TestRuntimeDurableChildMixedBatchKeepsSiblingOrder(t *testing.T) {
	var extraCalls atomic.Int32
	extra := Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) {
		extraCalls.Add(1)
		return "extra ok", nil
	})
	parent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("c1", "delegate", `{"topic":"tea"}`), toolUse("t1", "extra", `{}`)),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	parent.RegisterTool(extra)

	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(&repeatingChildProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent done" {
		t.Errorf("output = %q", result.Output)
	}
	if got := extraCalls.Load(); got != 1 {
		t.Fatalf("extra calls = %d, want 1", got)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	batch := snapshot.ToolBatches[0]
	if batch.Invocations[0].ChildRunID == "" {
		t.Fatalf("child invocation = %#v", batch.Invocations[0])
	}
	// Model order: child projection first, ordinary sibling second.
	if batch.Invocations[0].Result.Text() != "child done" || batch.Invocations[1].Result.Text() != "extra ok" {
		t.Fatalf("batch results = %q, %q", batch.Invocations[0].Result.Text(), batch.Invocations[1].Result.Text())
	}
	if got := snapshot.Result.Messages[3].Blocks[0].(ToolResultBlock).ToolUseID; got != "t1" {
		t.Fatalf("second transcript result = %q", got)
	}
}

// The restart reuses the persisted reservations: the charged subtree cap
// survives restart, the replayed batch does not re-charge it, and the denied
// sibling stays not-executed.
func TestRuntimeDurableChildSiblingsNotRerunAfterRestart(t *testing.T) {
	childProvider := &repeatingChildProvider{}
	var deniedCalls atomic.Int32
	denied := Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) {
		deniedCalls.Add(1)
		return "unexpected", nil
	})
	parent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("c1", "delegate", `{"topic":"tea"}`), toolUse("t1", "extra", `{}`)),
		asstText("parent done"),
	}}).WithToolPolicy(ToolPolicy{MaxCalls: 1})
	newSharedChildDefinition(parent)
	parent.RegisterTool(denied)

	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(childProvider)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	first, err := runtime.Run(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range 2 {
		if err := runtime.Recover(context.Background()); err != nil {
			t.Fatalf("Recover: %v", err)
		}
	}
	snapshot, err := runtime.Handle(first.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeTerminal {
		t.Fatalf("parent state = %s", snapshot.State)
	}
	batch := snapshot.ToolBatches[0]
	if !batch.Committed {
		t.Fatalf("batch not committed: %#v", batch)
	}
	if got := childProvider.calls.Load(); got != 1 {
		t.Fatalf("child provider calls = %d, want 1 (child recreated?)", got)
	}
	if got := deniedCalls.Load(); got != 0 {
		t.Fatalf("denied sibling executed = %d, want 0", got)
	}
	if batch.Invocations[0].ChildRunID == "" || batch.Invocations[1].Effect.Status != EffectNotApplied {
		t.Fatalf("sibling records = %#v", batch.Invocations)
	}
	// Wait consumed across restart; exactly one link.
	var childLinkCount, runCount int
	if err := runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		runCount = 0
		if err := tx.Scan(runtimeRunsBucket, "", func(string, []byte) error { runCount++; return nil }); err != nil {
			return err
		}
		return tx.Scan(runtimeChildLinksBucket, "", func(string, []byte) error { childLinkCount++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if runCount != 2 || childLinkCount != 1 {
		t.Fatalf("runs = %d links = %d, want 2/1", runCount, childLinkCount)
	}
	parentRecord, err := runtimeRecord(runtime, first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	// Restart must neither reset the charged cap nor re-charge it on replay.
	if parentRecord.ToolBudget.TotalCap != 1 || parentRecord.ToolBudget.Total != 1 {
		t.Fatalf("parent tool budget after restart = %#v, want cap 1 used 1", parentRecord.ToolBudget)
	}
}

// A crash that loses the child completion wake is reconstructed by recovery:
// the terminal child is consumed exactly once and the parent continues.
func TestRuntimeDurableChildLostWakeRecovered(t *testing.T) {
	childProvider := &repeatingChildProvider{}
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)

	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(childProvider)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	childResult, err := runtime.Run(context.Background(), "child", "v1", "seed", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if childProvider.calls.Load() != 1 {
		t.Fatalf("seed child calls = %d", childProvider.calls.Load())
	}
	parentID := "staged-parent"
	stageWaitingParentWithTerminalChild(t, runtime, parentID, childResult.RunID)

	for range 2 {
		if err := runtime.Recover(context.Background()); err != nil {
			t.Fatalf("Recover: %v", err)
		}
	}
	result, err := runtime.Handle(parentID).Await(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent done" || result.RunID != parentID {
		t.Fatalf("staged parent = %q %q", result.RunID, result.Output)
	}
	if childProvider.calls.Load() != 1 {
		t.Fatalf("child provider calls = %d, want 1", childProvider.calls.Load())
	}
	snapshot, err := runtime.Handle(parentID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.ToolBatches[0].Invocations[0].Result.Text(); got != "child done" {
		t.Fatalf("child projection = %q", got)
	}
	var consumed bool
	if err := runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		wait, err := getStoredWait(tx, parentID, childWaitIDFor(parentID+":0000000000000000:0"))
		consumed = wait.State == WaitConsumed && wait.ChildRunID == childResult.RunID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !consumed {
		t.Fatal("child wait was not consumed")
	}
}

// Required child attention blocks ordinary parent continuation, stays visible
// from Snapshot/Await, and a later clean child settlement unblocks the parent
// through the recovery recheck.
func TestRuntimeDurableChildAttentionBlocksParentAndLaterUnblocks(t *testing.T) {
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	childProvider := &repeatingChildProvider{}
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(childProvider)); err != nil {
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
	// Inject child-side attention: the linked child is not consumable until
	// its blocked state settles.
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, childResult.RunID)
		if err != nil {
			return err
		}
		record.State = RuntimeNeedsAttention
		// A hooks-kind attention is never auto-resumable, matching the
		// conservatively blocked child a real crashed hook delivery leaves.
		record.AttentionKind = "hooks"
		record.AttentionReason = "previous owner stopped while delivering committed-run hooks; delivery outcome is unknown"
		record.Generation++
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Handle(parentID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeNeedsAttention || snapshot.Attention == nil || snapshot.Attention.Kind != AttentionChild ||
		snapshot.Attention.BlockingRunID != childResult.RunID {
		t.Fatalf("parent snapshot = %s attention %#v", snapshot.State, snapshot.Attention)
	}
	if _, err := runtime.Handle(parentID).Await(context.Background()); !errors.Is(err, ErrRunNeedsAttention) {
		t.Fatalf("Await = %v, want ErrRunNeedsAttention", err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	stillBlocked, err := runtime.Handle(parentID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stillBlocked.Attention == nil || stillBlocked.Attention.BlockingRunID != childResult.RunID {
		t.Fatalf("recovered parent attention = %#v, want child %s", stillBlocked.Attention, childResult.RunID)
	}
	// The host resolves the child authoritatively; the child settles cleanly.
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, childResult.RunID)
		if err != nil {
			return err
		}
		record.State = RuntimeTerminal
		record.Result.Status = RunCompleted
		record.Result.FinalMessage = asstText("child done")
		record.AttentionReason = ""
		record.AttentionKind = ""
		record.Generation++
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	final, err := runtime.Handle(parentID).Await(context.Background())
	if err != nil {
		t.Fatalf("Await after child settlement: %v", err)
	}
	if final, _ := runtime.Handle(parentID).Snapshot(context.Background()); final.State != RuntimeTerminal ||
		final.Attention != nil || final.ToolBatches[0].Invocations[0].Result.Text() != "child done" {
		t.Fatalf("parent final = %#v", final)
	}
	_ = final
	if childProvider.calls.Load() != 1 {
		t.Fatalf("child provider calls = %d, want 1", childProvider.calls.Load())
	}
}

// Canceling a waiting parent cancels its running required child in the same
// commit. The parent terminalizes as canceled, keeps the link and its
// not-applied invocation, and the child settles as canceled too.
func TestRuntimeDurableChildCancellationPropagatesToRunningChild(t *testing.T) {
	barrier := &childBarrierProvider{started: make(chan struct{}), release: make(chan struct{})}
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(barrier)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-barrier.started
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeTerminal || snapshot.Result.Status != RunCancelled {
		t.Fatalf("canceled parent = %s %s", snapshot.State, snapshot.Result.Status)
	}
	canceled := snapshot.ToolBatches[0].Invocations[0]
	if canceled.ChildRunID == "" || !canceled.Result.IsError || canceled.Effect.Status != EffectNone ||
		!strings.Contains(canceled.Result.Text(), "outcome of child run "+canceled.ChildRunID) {
		t.Fatalf("canceled invocation = %#v", canceled)
	}
	childResult, err := awaitRun(t, runtime, canceled.ChildRunID)
	if !errors.Is(err, context.Canceled) || childResult.Status != RunCancelled {
		t.Fatalf("child = %s, %v; want canceled", childResult.Status, err)
	}
	if got := barrier.calls.Load(); got != 1 {
		t.Fatalf("child provider calls = %d, want 1", got)
	}
	final, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if final.State != RuntimeTerminal || final.Result.Status != RunCancelled || final.Accounting.Tree.Unsettled != 0 {
		t.Fatalf("parent after child settled = %s %s unsettled %d", final.State, final.Result.Status, final.Accounting.Tree.Unsettled)
	}
}

// ignoringCancelProvider is a non-cooperative child: its first turn ignores
// cancellation until released, then requests a tool call.
type ignoringCancelProvider struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
	calls       atomic.Int32
}

func newIgnoringCancelProvider() *ignoringCancelProvider {
	return &ignoringCancelProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *ignoringCancelProvider) Invoke(context.Context, Request) (Response, error) {
	if p.calls.Add(1) == 1 {
		p.startedOnce.Do(func() { close(p.started) })
		<-p.release
		return fixtureResponse(AssistantMessage(toolUse("e1", "extra", `{}`))), nil
	}
	return fixtureResponse(asstText("child done")), nil
}

func (p *ignoringCancelProvider) unblock() { p.releaseOnce.Do(func() { close(p.release) }) }

// A canceled parent terminalizes before a non-cooperative child settles. The
// child is durably cancel-requested, so its late response dispatches nothing,
// and the late settlement never resurrects the parent.
func TestRuntimeDurableCanceledParentBlocksNoncooperativeChildDispatch(t *testing.T) {
	var extraCalls atomic.Int32
	extra := Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) {
		extraCalls.Add(1)
		return "extra ok", nil
	})
	childProvider := newIgnoringCancelProvider()
	child := testAgent(childProvider)
	child.RegisterTool(extra)
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	runtime := newTestRuntime(t)
	t.Cleanup(childProvider.unblock)
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
	waitForSignal(t, childProvider.started, "child provider")
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeTerminal || snapshot.Result.Status != RunCancelled || snapshot.Accounting.Tree.Unsettled != 1 {
		t.Fatalf("parent = %s %s unsettled %d; want canceled with one unsettled child", snapshot.State, snapshot.Result.Status, snapshot.Accounting.Tree.Unsettled)
	}
	childID := snapshot.ToolBatches[0].Invocations[0].ChildRunID
	childRecord, err := runtimeRecord(runtime, childID)
	if err != nil {
		t.Fatal(err)
	}
	if childRecord.State != RuntimeCancelRequested {
		t.Fatalf("child state = %s, want durable cancel request", childRecord.State)
	}

	childProvider.unblock()
	childResult, err := awaitRun(t, runtime, childID)
	if !errors.Is(err, context.Canceled) || childResult.Status != RunCancelled {
		t.Fatalf("child = %s, %v; want canceled", childResult.Status, err)
	}
	if got := extraCalls.Load(); got != 0 {
		t.Fatalf("tool dispatched under a canceled ancestor %d times", got)
	}
	final, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if final.State != RuntimeTerminal || final.Result.Status != RunCancelled || final.Accounting.Tree.Unsettled != 0 {
		t.Fatalf("parent after child settled = %s %s unsettled %d", final.State, final.Result.Status, final.Accounting.Tree.Unsettled)
	}
}

// questionChild builds a child definition that suspends on a durable question.
func questionChild() *Agent {
	child := testAgent(&scriptedProvider{turns: []Message{asstTool("q1", "ask", `{}`), asstText("child done")}})
	child.RegisterTool(WithDurableWait(Func("ask", "ask a human", func(context.Context, struct{}) (string, error) {
		return "unused", nil
	}), DurableWaitPolicy{Kind: WaitQuestion}))
	return child
}

// Cancellation reaches every non-terminal descendant in the canceling commit:
// a suspended grandchild terminalizes immediately, and a running child whose
// owner stopped is left cancel-requested, so recovery finalizes it without any
// new provider or tool dispatch.
func TestRuntimeDurableCancellationPersistsForSuspendedAndStoppedDescendants(t *testing.T) {
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
	runtime := newTestRuntime(t)
	for id, agent := range map[string]*Agent{"grandchild": questionChild(), "child": child, "parent": parent} {
		if err := runtime.Register(id, "v1", agent); err != nil {
			t.Fatal(err)
		}
	}
	handle, err := runtime.Submit(context.Background(), "parent", "v1", "go", SubmitOptions{})
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

	// Simulate the child's owner having stopped mid-execution: its record says
	// running, but no worker in this process owns it.
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, childID)
		if err != nil {
			return err
		}
		record.State = RuntimeRunning
		record.Generation++
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	for runID, want := range map[string]RuntimeState{handle.ID(): RuntimeTerminal, childID: RuntimeCancelRequested, grandchildID: RuntimeTerminal} {
		record, err := runtimeRecord(runtime, runID)
		if err != nil {
			t.Fatal(err)
		}
		if record.State != want {
			t.Fatalf("run %s state = %s, want %s", runID, record.State, want)
		}
	}
	grandchild, err := runtime.Handle(grandchildID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if grandchild.Result.Status != RunCancelled || grandchild.Waits[0].State != WaitCancelled {
		t.Fatalf("grandchild = %s wait %s", grandchild.Result.Status, grandchild.Waits[0].State)
	}

	// A new owner finishes the persisted cancellation; nothing is re-dispatched.
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	childResult, err := awaitRun(t, runtime, childID)
	if !errors.Is(err, context.Canceled) || childResult.Status != RunCancelled {
		t.Fatalf("child = %s, %v; want canceled", childResult.Status, err)
	}
	if err := runtime.Handle(grandchildID).ResolveWait(context.Background(), grandchild.Waits[0].ID, WaitResolution{Answer: json.RawMessage(`"late"`), Decision: Allow}); err == nil {
		t.Fatal("a late answer resumed a descendant of a canceled run")
	}
	final, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if final.Accounting.Tree.Unsettled != 0 || final.Result.Status != RunCancelled {
		t.Fatalf("parent = %s unsettled %d", final.Result.Status, final.Accounting.Tree.Unsettled)
	}
}

// Canceling a middle child whose own descendant does not cooperate blocks the
// waiting parent with child attention until the descendant settles; the late
// settlement then climbs the canceled chain and the parent consumes the
// canceled child outcome once and continues.
func TestRuntimeDurableCanceledChildWakesParentAfterDescendantSettles(t *testing.T) {
	grandchildProvider := newIgnoringCancelProvider()
	grandchild := testAgent(grandchildProvider)
	grandchild.RegisterTool(Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) { return "extra ok", nil }))
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
	runtime := newTestRuntime(t)
	t.Cleanup(grandchildProvider.unblock)
	for id, agent := range map[string]*Agent{"grandchild": grandchild, "child": child, "parent": parent} {
		if err := runtime.Register(id, "v1", agent); err != nil {
			t.Fatal(err)
		}
	}
	handle, err := runtime.Submit(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, grandchildProvider.started, "grandchild provider")
	parentSnapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childID := parentSnapshot.ToolBatches[0].Invocations[0].ChildRunID
	if err := runtime.Handle(childID).Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	blocked, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if blocked.State != RuntimeNeedsAttention || blocked.Attention == nil || blocked.Attention.Kind != AttentionChild ||
		blocked.Attention.BlockingRunID != childID {
		t.Fatalf("parent = %s %#v, want child %s attention while descendant settles", blocked.State, blocked.Attention, childID)
	}
	settlingChild, err := runtime.Handle(childID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settlingChild.State != RuntimeTerminal || settlingChild.Accounting.Tree.Unsettled == 0 {
		t.Fatalf("canceled child = %s accounting %#v, want terminal with unsettled descendant", settlingChild.State, settlingChild.Accounting)
	}

	grandchildProvider.unblock()
	waitForRunState(t, runtime, handle.ID(), RuntimeTerminal)
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	if result.Output != "parent done" {
		t.Fatalf("parent output = %q", result.Output)
	}
	final, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	projection := final.ToolBatches[0].Invocations[0].Result
	if !projection.IsError || !strings.Contains(projection.Text(), "did not complete") {
		t.Fatalf("canceled child projection = %#v", projection)
	}
	if final.Accounting.Tree.Unsettled != 0 || final.Attention != nil {
		t.Fatalf("parent after settlement: attention %#v, unsettled descendants %d", final.Attention, final.Accounting.Tree.Unsettled)
	}
}

// Reconciling a terminal child's uncertain effect lets the blocked parent
// consume the child outcome and continue without re-running the child.
func TestRuntimeDurableReconcilingTerminalChildWakesParent(t *testing.T) {
	childProvider := &repeatingChildProvider{}
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(childProvider)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	childResult, err := runtime.Run(context.Background(), "child", "v1", "seed", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	childID := childResult.RunID
	operationID := childID + ":b0:0"
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		batch := storedToolBatch{Version: runtimeEncodingVersion, RunID: childID, BatchID: "b0", Count: 1, Committed: true}
		if err := putStoredJSON(tx, runtimeBatchesBucket, batchStorageKey(childID, "b0"), batch); err != nil {
			return err
		}
		invocation := storedToolInvocation{
			Version: runtimeEncodingVersion, RunID: childID, BatchID: "b0", OperationID: operationID,
			Call: toolUse("w1", "write", `{}`), State: ToolInvocationUncertain,
			Effect: EffectReport{Status: EffectUnknown}, EffectKind: ToolEffectMutating,
		}
		return putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(childID, "b0", 0), invocation)
	}); err != nil {
		t.Fatal(err)
	}
	parentID := "staged-parent"
	stageWaitingParentWithTerminalChild(t, runtime, parentID, childID)
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	blocked, err := runtime.Handle(parentID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if blocked.State != RuntimeNeedsAttention || blocked.Attention == nil || blocked.Attention.Kind != AttentionChild {
		t.Fatalf("parent = %s %#v, want child attention for the uncertain effect", blocked.State, blocked.Attention)
	}
	if err := runtime.Handle(childID).Reconcile(context.Background(), operationID, EffectResolution{
		Result: TextResult("verified"), Effect: EffectReport{Status: EffectApplied, Receipt: "write-1"},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Handle(parentID).Await(context.Background())
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	if result.Output != "parent done" {
		t.Fatalf("parent output = %q", result.Output)
	}
	if got := childProvider.calls.Load(); got != 1 {
		t.Fatalf("child provider calls = %d, want 1", got)
	}
}

// A waiting parent's logical deadline finalizes its suspended descendants in
// the same commit. Children inherit the ancestor deadline at admission.
func TestRuntimeDurableWaitingDeadlinePropagatesToSuspendedDescendants(t *testing.T) {
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", questionChild()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(300 * time.Millisecond).UTC()
	handle, err := runtime.Submit(context.Background(), "parent", "v1", "go", SubmitOptions{Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	waitForRunState(t, runtime, handle.ID(), RuntimeWaiting)
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childID := snapshot.ToolBatches[0].Invocations[0].ChildRunID
	waitForRunState(t, runtime, childID, RuntimeWaiting)
	childRecord, err := runtimeRecord(runtime, childID)
	if err != nil {
		t.Fatal(err)
	}
	if !childRecord.Deadline.Equal(deadline) {
		t.Fatalf("child deadline = %v, want inherited %v", childRecord.Deadline, deadline)
	}
	time.Sleep(time.Until(deadline) + 10*time.Millisecond)
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Await(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent = %v, want deadline", err)
	}
	child, err := runtime.Handle(childID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if child.State != RuntimeTerminal || child.Failure == nil || child.Failure.Kind != FailureDeadline || child.Waits[0].State != WaitCancelled {
		t.Fatalf("child = %s %#v wait %s, want deadline-finalized", child.State, child.Failure, child.Waits[0].State)
	}
}

// Accepted structured child values are preserved on the child run and
// projected as the model-facing JSON text of the parent invocation.
func TestRuntimeDurableChildStructuredOutputProjection(t *testing.T) {
	childAgent, err := New(&scriptedProvider{turns: []Message{asstText(`{"summary":"child done"}`)}}, AgentConfig{
		StructuredOutput: &StructuredOutputConfig{
			Schema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
			Native: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)

	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", childAgent); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent done" {
		t.Fatalf("output = %q", result.Output)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childRecord, err := runtime.load(context.Background(), snapshot.ToolBatches[0].Invocations[0].ChildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if string(childRecord.Result.StructuredOutput) != `{"summary":"child done"}` {
		t.Fatalf("child structured output = %q", childRecord.Result.StructuredOutput)
	}
	projection := snapshot.ToolBatches[0].Invocations[0].Result
	if projection.Text() != `{"summary":"child done"}` || len(projection.Blocks) != 1 {
		t.Fatalf("structured projection = %#v", projection.Blocks)
	}
	for _, block := range projection.Blocks {
		if _, ok := block.(ToolUseBlock); ok {
			t.Fatalf("projection copied a hidden tool call: %#v", block)
		}
	}
}

// The child's native rich blocks carry through to the parent projection
// verbatim while the child record retains its full native transcript.
func TestRuntimeDurableChildRichBlocksRetained(t *testing.T) {
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(TextBlock{Text: "see attached"}, RawBlock{Type: "custom_widget", Data: json.RawMessage(`{"n":1}`)}),
	}})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent done" {
		t.Fatalf("output = %q", result.Output)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	blocks := snapshot.ToolBatches[0].Invocations[0].Result.Blocks
	if len(blocks) != 2 {
		t.Fatalf("rich projection = %#v", blocks)
	}
	if _, ok := blocks[1].(RawBlock); !ok {
		t.Fatalf("rich projection dropped RawBlock: %#v", blocks)
	}
	childRecord, err := runtime.load(context.Background(), snapshot.ToolBatches[0].Invocations[0].ChildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := childRecord.Result.FinalMessage.Blocks[1].(RawBlock); !ok {
		t.Fatalf("child record rich blocks = %#v", childRecord.Result.FinalMessage.Blocks)
	}
}

type failingChildProvider struct{ calls atomic.Int32 }

func (p *failingChildProvider) Invoke(context.Context, Request) (Response, error) {
	p.calls.Add(1)
	return Response{}, errors.New("child provider unavailable")
}

// An authoritative child failure becomes a model-visible error projection; the
// parent may react and complete. The child record keeps its own failure
// evidence and the run identity link.
func TestRuntimeDurableChildFailureModelVisible(t *testing.T) {
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent handled child failure"),
	}})
	newSharedChildDefinition(parent)
	childProvider := &failingChildProvider{}
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(childProvider)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent handled child failure" {
		t.Fatalf("output = %q", result.Output)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if !invocation.Result.IsError || invocation.Effect.Status != EffectNone {
		t.Fatalf("child projection = %#v effect %#v", invocation.Result, invocation.Effect)
	}
	childRecord, err := runtime.load(context.Background(), invocation.ChildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if childRecord.State != RuntimeTerminal || childRecord.Result.Status != RunFailed || childRecord.Error == "" {
		t.Fatalf("child record = %s %s %q", childRecord.State, childRecord.Result.Status, childRecord.Error)
	}
}

func TestRegisterRejectsTransientChildAdapters(t *testing.T) {
	child := testAgent(&scriptedProvider{turns: []Message{asstText("x")}})
	cases := []struct {
		name string
		tool Tool
	}{
		{"asTool", AsTool[struct{}](child, "delegate", "process-local child")},
		{"asToolFunc", AsToolFunc(child, "delegate", "process-local child", func(struct{}) string { return "task" })},
		{"legacyWrapped", WithLegacyToolErrors(AsTool[struct{}](child, "delegate", "child"))},
		{"retryWrapped", WithToolRetry(AsTool[struct{}](child, "delegate", "child"), retry.Config{MaxAttempts: 2})},
	}
	for _, tc := range cases {
		runtime := newTestRuntime(t)
		parent := testAgent(nil)
		parent.RegisterTool(tc.tool)
		err := runtime.Register("parent", "v1", parent)
		if err == nil || !strings.Contains(err.Error(), "process-local child adapters") {
			t.Fatalf("%s: registered transient adapter = %v", tc.name, err)
		}
	}
}

func TestRegisterRejectsDurableChildWithProcessLocalAuthority(t *testing.T) {
	tool := DurableChildTool(childTestDefinition("delegate"), DurableChildPolicy{DefinitionID: "child-def", Revision: "v1"})
	cases := []struct {
		name  string
		agent *Agent
	}{
		{"approver", func() *Agent {
			agent, err := New(&scriptedProvider{}, AgentConfig{
				Tools: []Tool{tool},
				Approver: ApproverFunc(func(context.Context, ToolUseBlock, []Message) (Decision, error) {
					return Decision{Outcome: Allow}, nil
				}),
			})
			if err != nil {
				panic(err)
			}
			return agent
		}()},
		{"policyTimeout", func() *Agent {
			agent, err := New(&scriptedProvider{}, AgentConfig{Tools: []Tool{tool}, ToolPolicy: ToolPolicy{Timeout: time.Second}})
			if err != nil {
				panic(err)
			}
			return agent
		}()},
		{"perToolTimeout", func() *Agent {
			agent, err := New(&scriptedProvider{}, AgentConfig{Tools: []Tool{tool}, ToolPolicy: ToolPolicy{PerTool: map[string]ToolLimits{"delegate": {Timeout: time.Second}}}})
			if err != nil {
				panic(err)
			}
			return agent
		}()},
	}
	for _, tc := range cases {
		runtime := newTestRuntime(t)
		err := runtime.Register("parent", "v1", tc.agent)
		if err == nil || !strings.Contains(err.Error(), "durable child tools do not support") {
			t.Fatalf("%s: = %v", tc.name, err)
		}
	}
	// Plain leaf tools keep working under the same controls.
	plain, err := New(&scriptedProvider{}, AgentConfig{Tools: []Tool{
		Func("leaf", "leaf", func(context.Context, struct{}) (string, error) { return "ok", nil }),
	}, ToolPolicy: ToolPolicy{Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t)
	if err := runtime.Register("leafy", "v1", plain); err != nil {
		t.Fatalf("plain leaf with timeout = %v", err)
	}
}

// --- durable subtree cap reservations ---------------------------------------

// A descendant's known tool invocations reserve every applicable ancestor
// subtree total atomically with batch creation: the grandchild's ordinary tool
// call is denied because the root parent's cap was fully consumed by the two
// child invocations above it. Zero caps stay unlimited.
func TestRuntimeDurableSubtreeCapDescendantsReserveAncestorTotals(t *testing.T) {
	var extraCalls atomic.Int32
	extra := Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) {
		extraCalls.Add(1)
		return "extra ok", nil
	})
	grandchild := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("e1", "extra", `{}`)),
		asstText("grandchild done"),
	}})
	grandchild.RegisterTool(extra)
	child := testAgent(&scriptedProvider{turns: []Message{
		asstTool("g1", "delegate2", `{"topic":"tea"}`),
		asstText("child done"),
	}})
	child.RegisterTool(DurableChildTool(childTestDefinition("delegate2"), DurableChildPolicy{DefinitionID: "grandchild", Revision: "v1"}))
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}}).WithToolPolicy(ToolPolicy{MaxCalls: 2})
	newSharedChildDefinition(parent)

	runtime := newTestRuntime(t)
	if err := runtime.Register("grandchild", "v1", grandchild); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("child", "v1", child); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent done" {
		t.Fatalf("output = %q", result.Output)
	}
	// The grandchild's tool call was denied at the exhausted ancestor total and
	// never executed.
	if got := extraCalls.Load(); got != 0 {
		t.Fatalf("extra calls = %d, want 0", got)
	}
	parentSnapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childID := parentSnapshot.ToolBatches[0].Invocations[0].ChildRunID
	childSnapshot, err := runtime.Handle(childID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	grandchildID := childSnapshot.ToolBatches[0].Invocations[0].ChildRunID
	grandchildSnapshot, err := runtime.Handle(grandchildID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	denied := grandchildSnapshot.ToolBatches[0].Invocations[0]
	if denied.Call.Name != "extra" || !denied.Result.IsError || denied.Effect.Status != EffectNotApplied {
		t.Fatalf("grandchild invocation = %#v, want denied budget result", denied)
	}
	if grandchildSnapshot.State != RuntimeTerminal || grandchildSnapshot.Result.Output != "grandchild done" {
		t.Fatalf("grandchild run = %s %q", grandchildSnapshot.State, grandchildSnapshot.Result.Output)
	}
	// The two child invocations charged the root subtree cap; the child and
	// grandchild definitions pin no caps of their own.
	parentRecord, err := runtimeRecord(runtime, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if parentRecord.ToolBudget.TotalCap != 2 || parentRecord.ToolBudget.Total != 2 {
		t.Fatalf("parent tool budget = %#v, want cap 2 used 2", parentRecord.ToolBudget)
	}
	childRecord, err := runtimeRecord(runtime, childID)
	if err != nil {
		t.Fatal(err)
	}
	grandchildRecord, err := runtimeRecord(runtime, grandchildID)
	if err != nil {
		t.Fatal(err)
	}
	if childRecord.ToolBudget.TotalCap != 0 || childRecord.ToolBudget.Total != 0 ||
		grandchildRecord.ToolBudget.TotalCap != 0 || grandchildRecord.ToolBudget.Total != 0 {
		t.Fatalf("descendant budgets = %#v / %#v, want uncapped", childRecord.ToolBudget, grandchildRecord.ToolBudget)
	}
}

// A child definition's own MaxCalls stays stricter than any ancestor cap and
// applies only to its own calls: the second same-batch call is denied locally
// while the first also charged the (uncapped) ancestor chain.
func TestRuntimeDurableChildLocalCapStricterThanAncestor(t *testing.T) {
	var extraCalls atomic.Int32
	extra := Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) {
		extraCalls.Add(1)
		return "extra ok", nil
	})
	child := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("e1", "extra", `{}`), toolUse("e2", "extra", `{}`)),
		asstText("child done"),
	}}).WithToolPolicy(ToolPolicy{MaxCalls: 1})
	child.RegisterTool(extra)
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)

	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", child); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent done" {
		t.Fatalf("output = %q", result.Output)
	}
	if got := extraCalls.Load(); got != 1 {
		t.Fatalf("extra calls = %d, want 1", got)
	}
	parentSnapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childID := parentSnapshot.ToolBatches[0].Invocations[0].ChildRunID
	childSnapshot, err := runtime.Handle(childID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	batch := childSnapshot.ToolBatches[0]
	if batch.Invocations[0].Result.Text() != "extra ok" || batch.Invocations[0].Effect.Status == EffectNotApplied {
		t.Fatalf("first sibling = %#v", batch.Invocations[0])
	}
	denied := batch.Invocations[1]
	if !denied.Result.IsError || denied.Effect.Status != EffectNotApplied {
		t.Fatalf("second sibling = %#v, want denied budget result", denied)
	}
	if !strings.Contains(denied.Result.Text(), "max calls 1") {
		t.Fatalf("denial = %q, want local max calls 1", denied.Result.Text())
	}
	childRecord, err := runtimeRecord(runtime, childID)
	if err != nil {
		t.Fatal(err)
	}
	if childRecord.ToolBudget.TotalCap != 1 || childRecord.ToolBudget.Total != 1 {
		t.Fatalf("child tool budget = %#v, want cap 1 used 1", childRecord.ToolBudget)
	}
	parentRecord, err := runtimeRecord(runtime, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	// The child call itself charged the uncapped parent chain once.
	if parentRecord.ToolBudget.TotalCap != 0 || parentRecord.ToolBudget.Total != 0 {
		t.Fatalf("parent tool budget = %#v, want uncapped", parentRecord.ToolBudget)
	}
}

// siblingBarrierProvider serves each of two concurrent sibling child runs a
// first turn that issues one known tool call, blocking until both runs reached
// the provider so their batch reservations contend, then a closing text turn.
// The turn is derived from the request transcript so one provider instance can
// serve both runs.
type siblingBarrierProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *siblingBarrierProvider) Invoke(ctx context.Context, req Request) (Response, error) {
	p.started <- struct{}{}
	select {
	case <-p.release:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	turn := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			turn++
		}
	}
	if turn == 0 {
		return fixtureResponse(AssistantMessage(toolUse("e1", "extra", `{}`))), nil
	}
	return fixtureResponse(asstText("child done")), nil
}

// Concurrent sibling child runs reserve the shared ancestor subtree cap
// atomically: with one slot left, exactly one sibling's tool call is admitted
// and executed and the other is denied without executing, regardless of which
// wins the race.
func TestRuntimeDurableConcurrentSiblingRunsReserveAncestorAtomically(t *testing.T) {
	var extraCalls atomic.Int32
	extra := Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) {
		extraCalls.Add(1)
		return "extra ok", nil
	})
	barrier := &siblingBarrierProvider{started: make(chan struct{}, 2), release: make(chan struct{})}
	child := testAgent(barrier)
	child.RegisterTool(extra)
	parent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("c1", "delegate", `{"topic":"a"}`), toolUse("c2", "delegate", `{"topic":"b"}`)),
		asstText("parent done"),
	}}).WithToolPolicy(ToolPolicy{MaxCalls: 3})
	newSharedChildDefinition(parent)

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
	<-barrier.started
	<-barrier.started
	close(barrier.release)
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent done" {
		t.Fatalf("output = %q", result.Output)
	}
	// Exactly one sibling executed its tool call; the other was denied.
	if got := extraCalls.Load(); got != 1 {
		t.Fatalf("extra calls = %d, want 1", got)
	}
	parentSnapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	executed, denied := 0, 0
	for _, invocation := range parentSnapshot.ToolBatches[0].Invocations {
		childID := invocation.ChildRunID
		if childID == "" {
			t.Fatalf("child invocation = %#v", invocation)
		}
		childSnapshot, err := runtime.Handle(childID).Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if childSnapshot.State != RuntimeTerminal || childSnapshot.Result.Output != "child done" {
			t.Fatalf("child %s = %s %q", childID, childSnapshot.State, childSnapshot.Result.Output)
		}
		tool := childSnapshot.ToolBatches[0].Invocations[0]
		if tool.Result.IsError {
			if tool.Effect.Status != EffectNotApplied {
				t.Fatalf("denied invocation = %#v, want not applied", tool)
			}
			if !strings.Contains(tool.Result.Text(), ErrToolBudgetExhausted.Error()) {
				t.Fatalf("denied invocation = %q", tool.Result.Text())
			}
			denied++
		} else {
			if tool.Result.Text() != "extra ok" {
				t.Fatalf("executed invocation = %#v", tool)
			}
			executed++
		}
	}
	if executed != 1 || denied != 1 {
		t.Fatalf("sibling outcomes = %d executed, %d denied, want 1/1", executed, denied)
	}
	parentRecord, err := runtimeRecord(runtime, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if parentRecord.ToolBudget.TotalCap != 3 || parentRecord.ToolBudget.Total != 3 {
		t.Fatalf("parent tool budget = %#v, want cap 3 used 3", parentRecord.ToolBudget)
	}
}

// A restarted parent restores its charged subtree counter from the record: the
// replayed child batch does not re-charge it, and the continuation batch
// reserves the restored counter so the second call in the batch is denied.
func TestRuntimeDurableSubtreeCapCounterSurvivesRestart(t *testing.T) {
	childProvider := &repeatingChildProvider{}
	var extraCalls atomic.Int32
	extra := Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) {
		extraCalls.Add(1)
		return "extra ok", nil
	})
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		AssistantMessage(toolUse("t2", "extra", `{}`), toolUse("t3", "extra", `{}`)),
		asstText("parent done"),
	}}).WithToolPolicy(ToolPolicy{MaxCalls: 2})
	newSharedChildDefinition(parent)
	parent.RegisterTool(extra)

	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(childProvider)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	childResult, err := runtime.Run(context.Background(), "child", "v1", "seed", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if childProvider.calls.Load() != 1 {
		t.Fatalf("seed child calls = %d", childProvider.calls.Load())
	}
	parentID := "staged-cap-parent"
	stageWaitingParentWithTerminalChild(t, runtime, parentID, childResult.RunID)
	// The staged parent already reserved one child invocation against its
	// cap of 2 before the (simulated) restart.
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, parentID)
		if err != nil {
			return err
		}
		record.ToolBudget = storedToolBudget{TotalCap: 2, Total: 1}
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := runtime.Recover(context.Background()); err != nil {
			t.Fatalf("Recover: %v", err)
		}
	}
	result, err := runtime.Handle(parentID).Await(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Output != "parent done" || result.RunID != parentID {
		t.Fatalf("staged parent = %q %q", result.RunID, result.Output)
	}
	// The completed sibling child was not recreated.
	if got := childProvider.calls.Load(); got != 1 {
		t.Fatalf("child provider calls = %d, want 1", got)
	}
	snapshot, err := runtime.Handle(parentID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ToolBatches) != 2 {
		t.Fatalf("parent batches = %d, want 2", len(snapshot.ToolBatches))
	}
	for i, invocation := range snapshot.ToolBatches[1].Invocations {
		if i == 0 && invocation.Result.IsError {
			t.Fatalf("first continuation call = %#v, want executed", invocation)
		}
	}
	denied := snapshot.ToolBatches[1].Invocations[1]
	if !denied.Result.IsError || denied.Effect.Status != EffectNotApplied {
		t.Fatalf("second continuation call = %#v, want denied budget result", denied)
	}
	if !strings.Contains(denied.Result.Text(), "max calls 2") {
		t.Fatalf("denial = %q, want restored cap 2", denied.Result.Text())
	}
	if got := extraCalls.Load(); got != 1 {
		t.Fatalf("extra calls = %d, want 1", got)
	}
	// The restored counter persisted: the replay did not re-charge it and the
	// continuation batch charged it exactly once more.
	parentRecord, err := runtimeRecord(runtime, parentID)
	if err != nil {
		t.Fatal(err)
	}
	if parentRecord.ToolBudget.TotalCap != 2 || parentRecord.ToolBudget.Total != 2 {
		t.Fatalf("parent tool budget = %#v, want cap 2 used 2", parentRecord.ToolBudget)
	}
}

// A failed child admission rolls the whole batch transaction back: no child
// run or link exists, no reservation was persisted against the cap, and the
// failure is inspectable on the parent instead of silently swallowed.
func TestRuntimeDurableChildAdmissionRollbackDoesNotCharge(t *testing.T) {
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}}).WithToolPolicy(ToolPolicy{MaxCalls: 2})
	newSharedChildDefinition(parent)

	runtime := newTestRuntime(t)
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	// The pinned child definition is never registered, so the admission fails
	// closed inside the batch-creation transaction.
	_, err := runtime.Run(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err == nil {
		t.Fatal("failed child admission was invisible")
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
	if runCount != 1 || linkCount != 0 {
		t.Fatalf("runs = %d links = %d, want 1/0", runCount, linkCount)
	}
	record, err := runtimeRecord(runtime, onlyRunIDOfRuntime(t, runtime))
	if err != nil {
		t.Fatal(err)
	}
	if record.State != RuntimeNeedsAttention {
		t.Fatalf("parent state = %s, want needs attention", record.State)
	}
	// The rolled-back reservation left no persisted charge against the cap.
	if record.ToolBudget.TotalCap != 2 || record.ToolBudget.Total != 0 {
		t.Fatalf("parent tool budget = %#v, want cap 2 used 0", record.ToolBudget)
	}
}

func onlyRunIDOfRuntime(t *testing.T, runtime *Runtime) string {
	t.Helper()
	var id string
	if err := runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		return tx.Scan(runtimeRunsBucket, "", func(runID string, _ []byte) error {
			id = runID
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// failRunGetStore fails one writable-transaction read of an armed run record,
// simulating a storage fault while a descendant loads that ancestor for its
// durable reservation.
type failRunGetStore struct {
	Store
	target atomic.Value // string
	armed  atomic.Bool
}

type failRunGetTx struct {
	StoreTransaction
	store *failRunGetStore
}

func (tx failRunGetTx) Get(bucket, key string) ([]byte, error) {
	if target, _ := tx.store.target.Load().(string); bucket == runtimeRunsBucket && key == target && tx.store.armed.CompareAndSwap(true, false) {
		return nil, errors.New("injected ancestor read failure")
	}
	return tx.StoreTransaction.Get(bucket, key)
}

func (s *failRunGetStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	return s.Store.Transaction(ctx, writable, func(tx StoreTransaction) error {
		if writable {
			tx = failRunGetTx{StoreTransaction: tx, store: s}
		}
		return fn(tx)
	})
}

// gatedToolProvider blocks its first turn until released, then requests one
// known tool call; later turns finish.
type gatedToolProvider struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (p *gatedToolProvider) Invoke(ctx context.Context, _ Request) (Response, error) {
	if p.calls.Add(1) == 1 {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
		return fixtureResponse(AssistantMessage(toolUse("e1", "extra", `{}`))), nil
	}
	return fixtureResponse(asstText("child done")), nil
}

// A storage failure while a descendant loads an ancestor for its durable
// reservation aborts batch creation and requires attention. It is never
// committed as a model-visible budget denial, and nothing is charged or run.
func TestRuntimeDurableReservationStoreErrorIsNotBudgetDenial(t *testing.T) {
	var extraCalls atomic.Int32
	extra := Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) {
		extraCalls.Add(1)
		return "extra ok", nil
	})
	childProvider := &gatedToolProvider{started: make(chan struct{}), release: make(chan struct{})}
	child := testAgent(childProvider)
	child.RegisterTool(extra)
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}}).WithToolPolicy(ToolPolicy{MaxCalls: 5})
	newSharedChildDefinition(parent)

	store := &failRunGetStore{Store: newEphemeralStore()}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
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
	waitForSignal(t, childProvider.started, "child provider")
	store.target.Store(handle.ID())
	store.armed.Store(true)
	close(childProvider.release)

	parentSnapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childID := parentSnapshot.ToolBatches[0].Invocations[0].ChildRunID
	childSnapshot := waitForRunState(t, runtime, childID, RuntimeNeedsAttention)
	if store.armed.Load() {
		t.Fatal("injected ancestor read failure never fired")
	}
	if len(childSnapshot.ToolBatches) != 0 {
		t.Fatalf("child committed a batch despite the storage failure: %#v", childSnapshot.ToolBatches)
	}
	if childSnapshot.Attention == nil || !strings.Contains(childSnapshot.Attention.Reason, "injected ancestor read failure") {
		t.Fatalf("child attention = %#v", childSnapshot.Attention)
	}
	if got := extraCalls.Load(); got != 0 {
		t.Fatalf("extra calls = %d, want 0", got)
	}
	parentRecord, err := runtimeRecord(runtime, handle.ID())
	if err != nil {
		t.Fatal(err)
	}
	if parentRecord.ToolBudget.Total != 1 {
		t.Fatalf("parent tool budget = %#v, want only the child invocation charged", parentRecord.ToolBudget)
	}
}

// waitForRunState polls a run snapshot until it reaches state.
func waitForRunState(t *testing.T, runtime *Runtime, runID string, state RuntimeState) RunSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, err := runtime.Handle(runID).Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State == state {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s state = %s, want %s", runID, snapshot.State, state)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- tree accounting ----------------------------------------------------------

// Tree accounting derives from each linked run's persisted local values: every
// run counts once, local RunResult.Usage never includes descendants, and
// duplicate child completion or repeated recovery never adds usage again.
func TestRuntimeDurableTreeAccountingCountsEachRunOnce(t *testing.T) {
	grandchild := testAgent(&scriptedProvider{turns: []Message{withUsage(asstText("grandchild done"), &Usage{InputTokens: 100, OutputTokens: 1})}})
	child := testAgent(&scriptedProvider{turns: []Message{
		withUsage(asstTool("g1", "delegate2", `{"topic":"tea"}`), &Usage{InputTokens: 20, OutputTokens: 2}),
		withUsage(asstText("child done"), &Usage{InputTokens: 20, OutputTokens: 2}),
	}})
	child.RegisterTool(DurableChildTool(childTestDefinition("delegate2"), DurableChildPolicy{DefinitionID: "grandchild", Revision: "v1"}))
	parent := testAgent(&scriptedProvider{turns: []Message{
		withUsage(asstTool("c1", "delegate", `{"topic":"tea"}`), &Usage{InputTokens: 3, OutputTokens: 3}),
		withUsage(asstText("parent done"), &Usage{InputTokens: 3, OutputTokens: 3}),
	}})
	newSharedChildDefinition(parent)

	runtime := newTestRuntime(t)
	for id, agent := range map[string]*Agent{"grandchild": grandchild, "child": child, "parent": parent} {
		if err := runtime.Register(id, "v1", agent); err != nil {
			t.Fatal(err)
		}
	}
	result, err := runtime.Run(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Usage != (Usage{InputTokens: 6, OutputTokens: 6}) {
		t.Fatalf("parent local usage = %#v, want only its own turns", result.Usage)
	}
	want := TreeAccounting{Runs: 3, Usage: Usage{InputTokens: 146, OutputTokens: 11}, ProviderAttempts: 5}
	assertTree := func(label string) {
		t.Helper()
		snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Accounting.Tree != want {
			t.Fatalf("%s: parent tree = %#v, want %#v", label, snapshot.Accounting.Tree, want)
		}
		if snapshot.Result.Usage != result.Usage {
			t.Fatalf("%s: parent local usage changed to %#v", label, snapshot.Result.Usage)
		}
	}
	assertTree("completed")

	parentSnapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childID := parentSnapshot.ToolBatches[0].Invocations[0].ChildRunID
	childSnapshot, err := runtime.Handle(childID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if childSnapshot.Accounting.Tree != (TreeAccounting{Runs: 2, Usage: Usage{InputTokens: 140, OutputTokens: 5}, ProviderAttempts: 3}) {
		t.Fatalf("child tree = %#v", childSnapshot.Accounting.Tree)
	}
	if childSnapshot.Result.Usage != (Usage{InputTokens: 40, OutputTokens: 4}) {
		t.Fatalf("child local usage = %#v", childSnapshot.Result.Usage)
	}

	// Duplicate completion notifications and repeated recovery are no-ops.
	for range 2 {
		if err := runtime.wakeParentFromChild(context.Background(), childID); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	assertTree("after duplicate wake and recovery")
}

// seedInterruptedRun commits a running run whose owner stopped right after
// committing lastTransition.
func seedInterruptedRun(t *testing.T, store Store, runID, lastTransition string, messages []Message) {
	t.Helper()
	record := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: runID, DefinitionID: "agent", DefinitionRevision: "v1",
		Task: "go", State: RuntimeRunning, Generation: 3,
		Result:         RunResult{RunID: runID, Turns: 1, ProviderAttempts: 1, Usage: Usage{InputTokens: 5}},
		LastTransition: lastTransition,
		EffectiveTools: []string{"extra"},
	}
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		if err := appendTranscript(tx, runID, &record, messages); err != nil {
			return err
		}
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}
}

// afterBatchMessages is a committed transcript whose next step is a provider
// turn.
var afterBatchMessages = []Message{
	UserMessage("go"),
	asstTool("t1", "extra", `{}`),
	ToolResultBlockMessage("t1", Blocks{TextBlock{Text: "ok"}}, false),
}

// Recovery records unknown provider-attempt evidence exactly once, and only
// when an attempt record is open: the provider may have received that
// request. A run that stopped before its next attempt record never reached
// the provider and resumes; one with an open record needs attention instead
// of silently sending the request again.
func TestRuntimeRecoveryRecordsUnknownProviderAttemptOnce(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	seedInterruptedRun(t, base, "after-batch", "batch_committed", afterBatchMessages)
	seedInterruptedRun(t, base, "after-accept", "provider_accepted", []Message{UserMessage("go"), asstText("final")})
	seedInterruptedRun(t, base, "in-flight", transitionProviderAttemptStarted, afterBatchMessages)
	// Claimed, then stopped before its first attempt record committed.
	seedInterruptedRun(t, base, "claimed", "", nil)

	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	provider := &repeatingChildProvider{}
	agent := testAgent(provider)
	agent.RegisterTool(Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) { return "ok", nil }))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	// A repeated pass over the same interrupted generation counts it once.
	scanned, err := runtimeRecord(runtime, "in-flight")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.recordInterruptedAttempt(context.Background(), scanned); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for runID, wantUnknown := range map[string]int{"after-batch": 0, "after-accept": 0, "claimed": 0} {
		result, err := runtime.Handle(runID).Await(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", runID, err)
		}
		snapshot, err := runtime.Handle(runID).Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Accounting.UnknownAttempts != wantUnknown || snapshot.Accounting.Tree.UnknownAttempts != wantUnknown {
			t.Fatalf("%s: unknown attempts = %d (tree %d), want %d", runID, snapshot.Accounting.UnknownAttempts, snapshot.Accounting.Tree.UnknownAttempts, wantUnknown)
		}
		if result.Usage.InputTokens != 5 {
			t.Fatalf("%s: usage = %#v, want only recorded usage", runID, result.Usage)
		}
	}
	// Only the resumed after-batch and claimed runs called the provider.
	if got := provider.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (after-batch and claimed)", got)
	}
	inFlight := runtime.Handle("in-flight")
	if _, err := inFlight.Await(context.Background()); !errors.Is(err, ErrRunNeedsAttention) {
		t.Fatalf("interrupted attempt = %v, want ErrRunNeedsAttention", err)
	}
	snapshot, err := inFlight.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Attention == nil || snapshot.Attention.Kind != AttentionProvider || snapshot.Accounting.UnknownAttempts != 1 || snapshot.Accounting.FreshAttempts != 0 ||
		snapshot.Result.Usage.InputTokens != 5 || snapshot.Result.ProviderAttempts != 2 {
		t.Fatalf("interrupted attempt snapshot = attention %#v unknown %d fresh %d usage %#v attempts %d",
			snapshot.Attention, snapshot.Accounting.UnknownAttempts, snapshot.Accounting.FreshAttempts, snapshot.Result.Usage, snapshot.Result.ProviderAttempts)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, err := runtimeRecord(runtime, "in-flight")
	if err != nil {
		t.Fatal(err)
	}
	if record.UnknownAttempts != 1 || record.State != RuntimeNeedsAttention || provider.calls.Load() != 2 {
		t.Fatalf("after another recovery: unknown %d, state %s, provider calls %d", record.UnknownAttempts, record.State, provider.calls.Load())
	}
}

// An explicit ProviderRecovery bound authorizes fresh attempts in place of
// interrupted ones. Each is recorded, bounded per run, and distinguishable
// from the unknown attempt it replaces; raising the bound later lets a run
// already in provider attention continue on the next Recover.
func TestRuntimeProviderRecoveryPolicyAuthorizesBoundedFreshAttempts(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	seedInterruptedRun(t, base, "fresh", transitionProviderAttemptStarted, afterBatchMessages)
	seedInterruptedRun(t, base, "exhausted", transitionProviderAttemptStarted, afterBatchMessages)
	if err := base.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, "exhausted")
		if err != nil {
			return err
		}
		record.FreshAttempts = 1
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}
	open := func(maxFresh int) (*Runtime, *repeatingChildProvider) {
		runtime, err := NewRuntime(context.Background(), RuntimeConfig{
			Store: noCloseStore{Store: base}, ProviderRecovery: ProviderRecoveryPolicy{MaxFreshAttempts: maxFresh},
		})
		if err != nil {
			t.Fatal(err)
		}
		provider := &repeatingChildProvider{}
		agent := testAgent(provider)
		agent.RegisterTool(Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) { return "ok", nil }))
		if err := runtime.Register("agent", "v1", agent); err != nil {
			t.Fatal(err)
		}
		if err := runtime.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		return runtime, provider
	}
	if _, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), ProviderRecovery: ProviderRecoveryPolicy{MaxFreshAttempts: -1}}); err == nil {
		t.Fatal("negative MaxFreshAttempts accepted")
	}

	runtime, provider := open(1)
	result, err := runtime.Handle("fresh").Await(context.Background())
	if err != nil || result.Output != "child done" {
		t.Fatalf("fresh attempt = %#v, %v", result, err)
	}
	fresh, err := runtime.Handle("fresh").Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The fresh attempt is a new turn and attempt: one attempt was recorded
	// before the interruption, the interrupted one counts, and the fresh one
	// adds a third.
	if fresh.Accounting.FreshAttempts != 1 || fresh.Accounting.UnknownAttempts != 1 || result.Turns != 2 || result.ProviderAttempts != 3 || provider.calls.Load() != 1 {
		t.Fatalf("fresh attempt accounting: fresh %d unknown %d turns %d attempts %d calls %d",
			fresh.Accounting.FreshAttempts, fresh.Accounting.UnknownAttempts, result.Turns, result.ProviderAttempts, provider.calls.Load())
	}
	if _, err := runtime.Handle("exhausted").Await(context.Background()); !errors.Is(err, ErrRunNeedsAttention) {
		t.Fatalf("exhausted bound = %v, want ErrRunNeedsAttention", err)
	}
	_ = runtime.Close()

	runtime, provider = open(2)
	defer runtime.Close()
	result, err = runtime.Handle("exhausted").Await(context.Background())
	if err != nil || result.Output != "child done" || provider.calls.Load() != 1 {
		t.Fatalf("raised bound = %#v, %v, calls %d", result, err, provider.calls.Load())
	}
	if record, _ := runtimeRecord(runtime, "exhausted"); record.FreshAttempts != 2 || record.UnknownAttempts != 1 {
		t.Fatalf("raised bound accounting: fresh %d unknown %d", record.FreshAttempts, record.UnknownAttempts)
	}
}

// Stopping the worker while a provider call is in flight is the same
// uncertainty as a crash: the run needs provider attention with one unknown
// attempt, and the recovery policy decides whether a fresh attempt follows.
func TestRuntimeWorkerStopDuringProviderAttemptNeedsProviderAttention(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	provider := &cancelRuntimeProvider{started: make(chan struct{})}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
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
	waitForSignal(t, provider.started, "provider start")
	// Closing cancels the worker while the provider call is in flight.
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	record := getRecord(t, base, handle.ID())
	if record.State != RuntimeNeedsAttention || record.AttentionKind != "provider" || record.UnknownAttempts != 1 {
		t.Fatalf("stopped attempt = %s kind %q unknown %d", record.State, record.AttentionKind, record.UnknownAttempts)
	}

	fresh := &countingRuntimeProvider{}
	reopened, err := NewRuntime(context.Background(), RuntimeConfig{
		Store: noCloseStore{Store: base}, ProviderRecovery: ProviderRecoveryPolicy{MaxFreshAttempts: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Register("agent", "v1", testAgent(fresh)); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := reopened.Handle(handle.ID()).Await(context.Background())
	if err != nil || result.Output != "done" || fresh.calls.Load() != 1 {
		t.Fatalf("fresh attempt after stop = %#v, %v, calls %d", result, err, fresh.calls.Load())
	}
}

// awaitRun bounds Await so a lost wake or missing cancellation fails the test
// instead of hanging it.
func awaitRun(t *testing.T, runtime *Runtime, runID string) (RunResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runtime.Handle(runID).Await(ctx)
}

// --- review regressions -------------------------------------------------------

// A process-local agent nested inside a Runtime run's tool charges the run's
// persisted subtree cap, exactly as direct nested runs share the total budget.
func TestRuntimeNestedProcessLocalAgentChargesPersistedCap(t *testing.T) {
	var leafCalls atomic.Int32
	inner := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("l1", "leaf", `{}`), toolUse("l2", "leaf", `{}`)),
		asstText("inner done"),
	}})
	inner.RegisterTool(Func("leaf", "leaf work", func(context.Context, struct{}) (string, error) {
		leafCalls.Add(1)
		return "leaf ok", nil
	}))
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("o1", "outer", `{}`),
		asstText("parent done"),
	}}).WithToolPolicy(ToolPolicy{MaxCalls: 2})
	parent.RegisterTool(Func("outer", "run a nested agent", func(ctx context.Context, _ struct{}) (string, error) {
		result, err := inner.Run(ctx, "go")
		return result.Output, err
	}))
	runtime := newTestRuntime(t)
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil || result.Output != "parent done" {
		t.Fatalf("parent = %q, %v", result.Output, err)
	}
	if got := leafCalls.Load(); got != 1 {
		t.Fatalf("nested leaf calls = %d, want 1 under the shared cap of 2", got)
	}
	record, err := runtimeRecord(runtime, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.ToolBudget.TotalCap != 2 || record.ToolBudget.Total != 2 {
		t.Fatalf("parent tool budget = %#v, want the nested charge persisted", record.ToolBudget)
	}
}

// deadlineChildProvider blocks until its context ends, so a child sharing
// the parent's deadline fails exactly at that deadline.
type deadlineChildProvider struct{}

func (deadlineChildProvider) Invoke(ctx context.Context, _ Request) (Response, error) {
	<-ctx.Done()
	return Response{}, ctx.Err()
}

// A parent woken by a child that failed at their shared deadline is
// finalized by that deadline; it is never left running without a worker.
func TestRuntimeDurableSharedDeadlineFinalizesWokenParent(t *testing.T) {
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(deadlineChildProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "parent", "v1", "go", SubmitOptions{Deadline: time.Now().Add(150 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := waitForRunState(t, runtime, handle.ID(), RuntimeTerminal)
	if snapshot.Failure == nil || snapshot.Failure.Kind != FailureDeadline || snapshot.Result.Status != RunCancelled {
		t.Fatalf("parent = %s failure %#v", snapshot.Result.Status, snapshot.Failure)
	}
	child, err := runtime.Handle(snapshot.ToolBatches[0].Invocations[0].ChildRunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if child.State != RuntimeTerminal || child.Failure == nil || child.Failure.Kind != FailureDeadline {
		t.Fatalf("child = %s failure %#v", child.State, child.Failure)
	}
}

// A parent blocked on child attention is still suspended: recovery enforces
// its logical deadline and finalizes the blocking descendant with it.
func TestRuntimeDurableChildAttentionParentHonorsDeadline(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(&repeatingChildProvider{})); err != nil {
		t.Fatal(err)
	}
	childResult, err := runtime.Run(context.Background(), "child", "v1", "seed", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parentID := "staged-parent"
	stageWaitingParentWithTerminalChild(t, runtime, parentID, childResult.RunID)
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		parent, err := getRuntimeRun(tx, parentID)
		if err != nil {
			return err
		}
		parent.State, parent.AttentionKind, parent.AttentionReason = RuntimeNeedsAttention, "child", "blocked"
		parent.Deadline = time.Now().Add(-time.Second).UTC()
		if err := putRuntimeRun(tx, parent); err != nil {
			return err
		}
		child, err := getRuntimeRun(tx, childResult.RunID)
		if err != nil {
			return err
		}
		child.State, child.AttentionKind, child.AttentionReason = RuntimeNeedsAttention, "execution", "stuck"
		return putRuntimeRun(tx, child)
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for runID, kind := range map[string]string{parentID: "deadline", childResult.RunID: "deadline"} {
		snapshot, err := runtime.Handle(runID).Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State != RuntimeTerminal || snapshot.Failure == nil || snapshot.Failure.Kind != FailureKind(kind) {
			t.Fatalf("run %s = %s failure %#v, want terminal %s", runID, snapshot.State, snapshot.Failure, kind)
		}
	}
}

// A child whose owner stopped after its result committed but before any hook
// was delivered finishes on recovery, and the parent consumes its real
// completed outcome instead of being blocked behind hook attention.
func TestRuntimeDurableFinalizingChildWithoutHookDeliveryCompletesParent(t *testing.T) {
	childProvider := &repeatingChildProvider{}
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(childProvider)); err != nil {
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
		child, err := getRuntimeRun(tx, childResult.RunID)
		if err != nil {
			return err
		}
		child.State = RuntimeFinalizing
		return putRuntimeRun(tx, child)
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForRunState(t, runtime, parentID, RuntimeTerminal)
	snapshot, err := runtime.Handle(parentID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Result.Output != "parent done" || snapshot.ToolBatches[0].Invocations[0].Result.Text() != "child done" {
		t.Fatalf("parent = %q projection %q", snapshot.Result.Output, snapshot.ToolBatches[0].Invocations[0].Result.Text())
	}
	if got := childProvider.calls.Load(); got != 1 {
		t.Fatalf("child provider calls = %d, want 1", got)
	}
}

// Child attention follows the blocking sibling. Once both attention children
// are canceled and consumed, the parent waits for its remaining sibling and
// later completes without stale attention.
func TestRuntimeDurableChildAttentionClearsWhenSiblingsRemainPending(t *testing.T) {
	parent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("c1", "delegate", `{"topic":"a"}`), toolUse("c2", "delegate", `{"topic":"b"}`), toolUse("c3", "delegate", `{"topic":"c"}`)),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	child := testAgent(&repeatingQuestionProvider{})
	child.RegisterTool(WithDurableWait(Func("ask", "ask a human", func(context.Context, struct{}) (string, error) {
		return "unused", nil
	}), DurableWaitPolicy{Kind: WaitQuestion}))
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
	waitForRunState(t, runtime, handle.ID(), RuntimeWaiting)
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first, second, third := snapshot.ToolBatches[0].Invocations[0].ChildRunID,
		snapshot.ToolBatches[0].Invocations[1].ChildRunID, snapshot.ToolBatches[0].Invocations[2].ChildRunID
	waitForRunState(t, runtime, first, RuntimeWaiting)
	waitForRunState(t, runtime, second, RuntimeWaiting)
	thirdSnapshot := waitForRunState(t, runtime, third, RuntimeWaiting)
	firstRecord, err := runtimeRecord(runtime, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.markAttention(context.Background(), first, firstRecord.Generation, "stuck"); err != nil {
		t.Fatal(err)
	}
	blocked := waitForRunState(t, runtime, handle.ID(), RuntimeNeedsAttention)
	if blocked.Attention == nil || blocked.Attention.Kind != AttentionChild || blocked.Attention.BlockingRunID != first {
		t.Fatalf("parent attention = %#v, want first child %s", blocked.Attention, first)
	}
	secondRecord, err := runtimeRecord(runtime, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.markAttention(context.Background(), second, secondRecord.Generation, "also stuck"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Handle(first).Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	blocked = waitForRunState(t, runtime, handle.ID(), RuntimeNeedsAttention)
	if blocked.Attention == nil || blocked.Attention.Kind != AttentionChild || blocked.Attention.BlockingRunID != second {
		t.Fatalf("parent attention after first child settled = %#v, want second child %s", blocked.Attention, second)
	}
	if err := runtime.Handle(second).Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	waiting := waitForRunState(t, runtime, handle.ID(), RuntimeWaiting)
	if waiting.Attention != nil {
		t.Fatalf("parent waiting on third child retains attention %#v", waiting.Attention)
	}
	if err := runtime.Handle(third).ResolveWait(context.Background(), thirdSnapshot.Waits[0].ID, WaitResolution{Answer: json.RawMessage(`"yes"`), Decision: Allow}); err != nil {
		t.Fatal(err)
	}
	result, err := awaitRun(t, runtime, handle.ID())
	if err != nil || result.Output != "parent done" {
		t.Fatalf("parent = %q, %v", result.Output, err)
	}
	final, err := handle.Snapshot(context.Background())
	if err != nil || final.State != RuntimeTerminal || final.Attention != nil {
		t.Fatalf("terminal parent = %#v, %v", final, err)
	}
}

// repeatingQuestionProvider asks once per run, then finishes after the answer.
type repeatingQuestionProvider struct{}

func (repeatingQuestionProvider) Invoke(_ context.Context, request Request) (Response, error) {
	for _, message := range request.Messages {
		if message.Role == "assistant" {
			return fixtureResponse(asstText("child done")), nil
		}
	}
	return fixtureResponse(asstTool("q1", "ask", `{}`)), nil
}

// A run restarted while its pending batch still holds a pending wait (for
// example after a storage fault forced execution attention) suspends again
// instead of being left running without a worker.
func TestRuntimeReplayedBatchWithPendingWaitSuspends(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("asker", "v1", questionChild()); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "asker", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waiting := waitForRunState(t, runtime, handle.ID(), RuntimeWaiting)
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, handle.ID())
		if err != nil {
			return err
		}
		record.State = RuntimeReady
		record.Generation++
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}
	runtime.start(handle.ID())
	waitForRunState(t, runtime, handle.ID(), RuntimeWaiting)
	if err := handle.ResolveWait(context.Background(), waiting.Waits[0].ID, WaitResolution{Answer: json.RawMessage(`"yes"`), Decision: Allow}); err != nil {
		t.Fatal(err)
	}
	result, err := awaitRun(t, runtime, handle.ID())
	if err != nil || result.Output != "child done" {
		t.Fatalf("resumed run = %q, %v", result.Output, err)
	}
}

// A nested charge that cannot be recorded fails the nested run instead of
// showing its model a budget denial; nothing runs. A run that is no longer
// running admits no nested charge at all.
func TestRuntimeNestedReservationFailureIsNotBudgetDenial(t *testing.T) {
	var leafCalls atomic.Int32
	inner := testAgent(&scriptedProvider{turns: []Message{asstTool("l1", "leaf", `{}`), asstText("inner done")}})
	inner.RegisterTool(Func("leaf", "leaf work", func(context.Context, struct{}) (string, error) {
		leafCalls.Add(1)
		return "leaf ok", nil
	}))
	store := &failRunGetStore{Store: newEphemeralStore()}
	var innerErr error
	parent := testAgent(&scriptedProvider{turns: []Message{asstTool("o1", "outer", `{}`), asstText("parent done")}})
	parent.RegisterTool(Func("outer", "run a nested agent", func(ctx context.Context, _ struct{}) (string, error) {
		store.armed.Store(true)
		result, err := inner.Run(ctx, "go")
		innerErr = err
		return result.Output, err
	}))
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.submit(context.Background(), "parent", "v1", "go", SubmitOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	store.target.Store(handle.ID())
	runtime.start(handle.ID())
	// The outer tool returns the nested failure as a Go error, which aborts
	// the parent as any tool execution error does.
	if _, err := awaitRun(t, runtime, handle.ID()); err == nil || errors.Is(err, ErrToolBudgetExhausted) {
		t.Fatalf("parent = %v, want the propagated storage failure", err)
	}
	if innerErr == nil || errors.Is(innerErr, ErrToolBudgetExhausted) || !strings.Contains(innerErr.Error(), "injected ancestor read failure") {
		t.Fatalf("nested run error = %v, want the storage failure", innerErr)
	}
	if got := leafCalls.Load(); got != 0 {
		t.Fatalf("leaf calls = %d, want 0", got)
	}

	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, handle.ID())
		if err != nil {
			return err
		}
		record.State = RuntimeCancelRequested
		record.ToolBudget = storedToolBudget{TotalCap: 5}
		return putRuntimeRun(tx, record)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.reserveNestedCall(handle.ID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("nested charge on a cancel-requested run = %v", err)
	}
	record, err := runtimeRecord(runtime, handle.ID())
	if err != nil {
		t.Fatal(err)
	}
	if record.ToolBudget.Total != 0 {
		t.Fatalf("refused nested charge persisted: %#v", record.ToolBudget)
	}
}

// afterWaitingStore runs hook once, right after the transaction that commits
// a run as waiting on a pending batch, before the worker continues.
type afterWaitingStore struct {
	Store
	hook  func()
	fired atomic.Bool
}

type afterWaitingTx struct {
	StoreTransaction
	waiting *bool
}

func (tx afterWaitingTx) Put(bucket, key string, value []byte) error {
	if bucket == runtimeRunsBucket {
		var record storedRuntimeRun
		if json.Unmarshal(value, &record) == nil && record.State == RuntimeWaiting && record.PendingBatchID != "" {
			*tx.waiting = true
		}
	}
	return tx.StoreTransaction.Put(bucket, key, value)
}

func (s *afterWaitingStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	waiting := false
	err := s.Store.Transaction(ctx, writable, func(tx StoreTransaction) error {
		return fn(afterWaitingTx{StoreTransaction: tx, waiting: &waiting})
	})
	if err == nil && waiting && s.fired.CompareAndSwap(false, true) {
		s.hook()
	}
	return err
}

// A wait resolved between the batch commit that suspends the run and the
// worker's own wait check does not let that worker dispatch siblings: it
// suspends, and the resolution's resumption runs the batch exactly once.
func TestRuntimeSuspendedBatchDoesNotDispatchAfterRacingResolution(t *testing.T) {
	var extraCalls atomic.Int32
	agent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("q1", "ask", `{}`), toolUse("e1", "extra", `{}`)),
		asstText("done"),
	}})
	agent.RegisterTool(WithDurableWait(Func("ask", "ask a human", func(context.Context, struct{}) (string, error) {
		return "unused", nil
	}), DurableWaitPolicy{Kind: WaitQuestion}))
	agent.RegisterTool(Func("extra", "ordinary work", func(context.Context, struct{}) (string, error) {
		extraCalls.Add(1)
		return "extra ok", nil
	}))
	store := &afterWaitingStore{Store: newEphemeralStore()}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.submit(context.Background(), "agent", "v1", "go", SubmitOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	var resolveErr error
	store.hook = func() {
		snapshot, err := handle.Snapshot(context.Background())
		if err != nil {
			resolveErr = err
			return
		}
		resolveErr = handle.ResolveWait(context.Background(), snapshot.Waits[0].ID, WaitResolution{Answer: json.RawMessage(`"yes"`), Decision: Allow})
	}
	runtime.start(handle.ID())
	result, err := awaitRun(t, runtime, handle.ID())
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if err != nil || result.Output != "done" {
		t.Fatalf("run = %q, %v", result.Output, err)
	}
	if got := extraCalls.Load(); got != 1 {
		t.Fatalf("sibling dispatched %d times, want 1", got)
	}
}

// Acknowledging interrupted hook delivery terminalizes a child with its
// committed result, records the hook as unknown, and lets the blocked parent
// consume the real outcome. It is idempotent and refuses other runs.
func TestRuntimeAcknowledgeHooksCompletesChildAndWakesParent(t *testing.T) {
	childProvider := &repeatingChildProvider{}
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(childProvider)); err != nil {
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
	// The child's owner stopped after hook delivery started.
	if err := runtime.transaction(context.Background(), true, func(tx StoreTransaction) error {
		child, err := getRuntimeRun(tx, childResult.RunID)
		if err != nil {
			return err
		}
		child.State = RuntimeFinalizing
		child.HookResults = nil
		child.HookDelivery = []string{"audit"}
		return putRuntimeRun(tx, child)
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if child := waitForRunState(t, runtime, childResult.RunID, RuntimeNeedsAttention); child.Attention == nil || child.Attention.Kind != AttentionHooks {
		t.Fatalf("child attention = %#v", child.Attention)
	}
	if blocked := waitForRunState(t, runtime, parentID, RuntimeNeedsAttention); blocked.Attention == nil || blocked.Attention.Kind != AttentionChild {
		t.Fatalf("parent attention = %#v", blocked.Attention)
	}
	if err := runtime.Handle(parentID).AcknowledgeHooks(context.Background()); err == nil {
		t.Fatal("acknowledged hooks on a run that is not awaiting them")
	}

	childHandle := runtime.Handle(childResult.RunID)
	for range 2 {
		if err := childHandle.AcknowledgeHooks(context.Background()); err != nil {
			t.Fatalf("AcknowledgeHooks: %v", err)
		}
	}
	child, err := childHandle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if child.State != RuntimeTerminal || child.Result.Status != RunCompleted || child.Result.Output != "child done" {
		t.Fatalf("acknowledged child = %s %s %q", child.State, child.Result.Status, child.Result.Output)
	}
	if len(child.Hooks) != 1 || child.Hooks[0].Name != "audit" || !child.Hooks[0].Unknown || child.Hooks[0].Error == "" {
		t.Fatalf("hook results = %#v, want one unknown audit delivery", child.Hooks)
	}
	waitForRunState(t, runtime, parentID, RuntimeTerminal)
	parentSnapshot, err := runtime.Handle(parentID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if parentSnapshot.Result.Output != "parent done" || parentSnapshot.ToolBatches[0].Invocations[0].Result.Text() != "child done" {
		t.Fatalf("parent = %q projection %q", parentSnapshot.Result.Output, parentSnapshot.ToolBatches[0].Invocations[0].Result.Text())
	}
	if got := childProvider.calls.Load(); got != 1 {
		t.Fatalf("child provider calls = %d, want 1", got)
	}
}
