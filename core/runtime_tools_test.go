package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func effectTool(name string, calls *atomic.Int32, handler func(context.Context) (ToolResult, error), policy ToolEffectPolicy) Tool {
	tool := FuncResult(name, name, func(ctx context.Context, _ struct{}) (ToolResult, error) {
		if calls != nil {
			calls.Add(1)
		}
		return handler(ctx)
	})
	return WithToolEffectPolicy(tool, policy)
}

func seedPendingRuntimeBatch(t *testing.T, store Store, runID string, calls []ToolUseBlock, states []ToolInvocationState) {
	t.Helper()
	messages := []Message{UserMessage("work"), AssistantMessage(func() []Block {
		blocks := make([]Block, len(calls))
		for i, call := range calls {
			blocks[i] = call
		}
		return blocks
	}()...)}
	effectiveTools := make([]string, 0, len(calls))
	seenTools := make(map[string]bool, len(calls))
	for _, call := range calls {
		if !seenTools[call.Name] {
			effectiveTools = append(effectiveTools, call.Name)
			seenTools[call.Name] = true
		}
	}
	record := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: runID, DefinitionID: "agent", DefinitionRevision: "v1",
		Task: "work", State: RuntimeRunning, Generation: 2,
		Result:         RunResult{RunID: runID, Turns: 1, Steps: 1, FinalMessage: messages[1]},
		LastTransition: "provider_accepted", EffectiveTools: effectiveTools,
		PendingBatchID: "0000000000000000", NextBatchOrdinal: 1,
	}
	batch := storedToolBatch{Version: runtimeEncodingVersion, RunID: runID, BatchID: record.PendingBatchID, Count: len(calls)}
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		if err := appendTranscript(tx, runID, &record, messages); err != nil {
			return err
		}
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		if err := putStoredJSON(tx, runtimeBatchesBucket, batchStorageKey(runID, batch.BatchID), batch); err != nil {
			return err
		}
		for i, call := range calls {
			invocation := storedToolInvocation{
				Version: runtimeEncodingVersion, RunID: runID, BatchID: batch.BatchID,
				OperationID: runID + ":0000000000000000:" + string(rune('0'+i)),
				Ordinal:     i, Call: call, State: states[i], EffectKind: ToolEffectMutating,
			}
			if states[i] == ToolInvocationCompleted {
				invocation.Result = TextResult("already complete")
				invocation.Effect = EffectReport{Status: EffectApplied, Receipt: "receipt-existing"}
			}
			if err := putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(runID, batch.BatchID, i), invocation); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRecoveryDoesNotRerunCompletedBatchSibling(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	calls := []ToolUseBlock{toolUse("first-id", "first", `{}`), toolUse("second-id", "second", `{}`)}
	seedPendingRuntimeBatch(t, base, "resume-batch", calls, []ToolInvocationState{ToolInvocationCompleted, ToolInvocationReserved})

	var firstCalls, secondCalls atomic.Int32
	agent := testAgent(&scriptedProvider{turns: []Message{asstText("done")}})
	agent.RegisterTool(effectTool("first", &firstCalls, func(context.Context) (ToolResult, error) {
		result := TextResult("unexpected")
		result.Effect = EffectReport{Status: EffectApplied}
		return result, nil
	}, ToolEffectPolicy{Kind: ToolEffectMutating}))
	agent.RegisterTool(effectTool("second", &secondCalls, func(context.Context) (ToolResult, error) {
		result := TextResult("second complete")
		result.Effect = EffectReport{Status: EffectApplied, Receipt: "receipt-second"}
		return result, nil
	}, ToolEffectPolicy{Kind: ToolEffectMutating}))

	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Handle("resume-batch").Await(context.Background())
	if err != nil || result.Output != "done" {
		t.Fatalf("resumed result = %#v, %v", result, err)
	}
	if firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		t.Fatalf("executions = first %d second %d; want 0/1", firstCalls.Load(), secondCalls.Load())
	}
	results := transcriptToolResults(result.Messages)
	if len(results) != 2 || results[0].ToolUseID != "first-id" || results[1].ToolUseID != "second-id" {
		t.Fatalf("canonical results = %#v", results)
	}
}

func TestRuntimeUncertainEffectRequiresAuthoritativeReconciliation(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	call := toolUse("write-id", "write", `{}`)
	seedPendingRuntimeBatch(t, base, "uncertain-run", []ToolUseBlock{call}, []ToolInvocationState{ToolInvocationDispatched})

	var executions atomic.Int32
	agent := testAgent(&scriptedProvider{turns: []Message{asstText("accepted")}})
	agent.RegisterTool(effectTool("write", &executions, func(context.Context) (ToolResult, error) {
		return ToolResult{}, errors.New("must not execute")
	}, ToolEffectPolicy{Kind: ToolEffectMutating}))
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	handle := runtime.Handle("uncertain-run")
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeNeedsAttention || len(snapshot.ToolBatches) != 1 || len(snapshot.ToolBatches[0].Invocations) != 1 {
		t.Fatalf("uncertain snapshot = %#v", snapshot)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if invocation.State != ToolInvocationUncertain || invocation.Effect.Status != EffectUnknown {
		t.Fatalf("uncertain invocation = %#v", invocation)
	}
	if len(transcriptToolResults(snapshot.Result.Messages)) != 0 {
		t.Fatal("partial batch leaked into canonical transcript")
	}
	resolution := EffectResolution{Result: TextResult("verified existing write"), Effect: EffectReport{Status: EffectApplied, Receipt: "external-42"}}
	if err := handle.Reconcile(context.Background(), invocation.OperationID, resolution); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err != nil || result.Output != "accepted" || executions.Load() != 0 {
		t.Fatalf("reconciled result = %#v, %v; executions=%d", result, err, executions.Load())
	}
	if err := handle.Reconcile(context.Background(), invocation.OperationID, resolution); err != nil {
		t.Fatalf("exact reconcile retry = %v", err)
	}
	changed := resolution
	changed.Effect.Receipt = "different"
	if err := handle.Reconcile(context.Background(), invocation.OperationID, changed); !errors.Is(err, ErrReconciliationConflict) {
		t.Fatalf("changed reconcile = %v", err)
	}
}

func TestRuntimeCancellationRetainsLateReconciliationEvidence(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	initializer, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	_ = initializer.Close()
	seedPendingRuntimeBatch(t, base, "cancel-reconcile", []ToolUseBlock{toolUse("write-id", "write", `{}`)}, []ToolInvocationState{ToolInvocationDispatched})

	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("unused")}})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	handle := runtime.Handle("cancel-reconcile")
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	operationID := snapshot.ToolBatches[0].Invocations[0].OperationID
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := handle.Reconcile(context.Background(), operationID, EffectResolution{
		Result: TextResult("late verified write"), Effect: EffectReport{Status: EffectApplied, Receipt: "late-receipt"},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if snapshot.State != RuntimeTerminal || snapshot.Result.Status != RunCancelled || invocation.State != ToolInvocationCompleted || invocation.Effect.Receipt != "late-receipt" {
		t.Fatalf("cancelled reconciled snapshot = %#v", snapshot)
	}
}

func TestRuntimeRetainsEffectReturnedWithError(t *testing.T) {
	runtime := newTestRuntime(t)
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("write-id", "write", `{}`)}})
	agent.RegisterTool(effectTool("write", nil, func(context.Context) (ToolResult, error) {
		result := TextResult("destination accepted write")
		result.Effect = EffectReport{Status: EffectApplied, Receipt: "write-7"}
		return result, errors.New("response decoding failed")
	}, ToolEffectPolicy{Kind: ToolEffectMutating}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err == nil || !strings.Contains(err.Error(), "response decoding failed") {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	snapshot, snapErr := runtime.Handle(result.RunID).Snapshot(context.Background())
	if snapErr != nil {
		t.Fatal(snapErr)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if invocation.State != ToolInvocationCompleted || invocation.Effect.Status != EffectApplied || invocation.Effect.Receipt != "write-7" {
		t.Fatalf("stored effect = %#v", invocation)
	}
}

func TestRuntimeReturnedErrorIsNotAutomaticallyUncertain(t *testing.T) {
	runtime := newTestRuntime(t)
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("legacy-id", "legacy", `{}`)}})
	agent.RegisterTool(FuncResult("legacy", "legacy", func(context.Context, struct{}) (ToolResult, error) {
		return ToolResult{}, errors.New("ordinary failure")
	}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err == nil {
		t.Fatal("expected ordinary tool error")
	}
	snapshot, snapErr := runtime.Handle(result.RunID).Snapshot(context.Background())
	if snapErr != nil {
		t.Fatal(snapErr)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if invocation.State != ToolInvocationCompleted || invocation.Effect.Status != EffectUnreported || snapshot.State != RuntimeTerminal {
		t.Fatalf("ordinary error misclassified = %#v in %s", invocation, snapshot.State)
	}
}

func TestRuntimeMutatingToolMustReportEffect(t *testing.T) {
	runtime := newTestRuntime(t)
	tool := effectTool("write", nil, func(context.Context) (ToolResult, error) {
		return TextResult("ambiguous"), nil
	}, ToolEffectPolicy{Kind: ToolEffectMutating})
	agent, err := New(&scriptedProvider{turns: []Message{asstTool("write-id", "write", `{}`)}}, AgentConfig{Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if !errors.Is(err, ErrRunNeedsAttention) {
		t.Fatalf("missing report result = %#v, %v", result, err)
	}
	snapshot, snapErr := runtime.Handle(result.RunID).Snapshot(context.Background())
	if snapErr != nil {
		t.Fatal(snapErr)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if invocation.State != ToolInvocationUncertain || invocation.Effect.Status != EffectUnknown {
		t.Fatalf("missing report invocation = %#v", invocation)
	}
}

func TestRuntimeDurableBatchCommitsReverseCompletionInModelOrder(t *testing.T) {
	runtime := newTestRuntime(t)
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("slow-id", "slow", `{}`), toolUse("fast-id", "fast", `{}`)),
		asstText("done"),
	}}
	fastDone := make(chan struct{})
	agent := testAgent(provider)
	agent.RegisterTool(effectTool("slow", nil, func(context.Context) (ToolResult, error) {
		<-fastDone
		return TextResult("slow result"), nil
	}, ToolEffectPolicy{Kind: ToolEffectReadOnly}))
	agent.RegisterTool(effectTool("fast", nil, func(context.Context) (ToolResult, error) {
		close(fastDone)
		return TextResult("fast result"), nil
	}, ToolEffectPolicy{Kind: ToolEffectReadOnly}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	results := transcriptToolResults(result.Messages)
	if len(results) != 2 || results[0].ToolUseID != "slow-id" || results[1].ToolUseID != "fast-id" {
		t.Fatalf("model-order results = %#v", results)
	}
}

func TestRuntimeToolOperationIdentityAndPanicEvidence(t *testing.T) {
	t.Run("operation identity", func(t *testing.T) {
		runtime := newTestRuntime(t)
		agent := testAgent(&scriptedProvider{turns: []Message{asstTool("write-id", "write", `{}`), asstText("done")}})
		agent.RegisterTool(effectTool("write", nil, func(ctx context.Context) (ToolResult, error) {
			op, ok := ToolOperationFromContext(ctx)
			if !ok || op.ID == "" || op.IdempotencyKey != op.ID {
				return ToolResult{}, errors.New("missing operation identity")
			}
			result := TextResult("wrote")
			result.Effect = EffectReport{Status: EffectApplied, Receipt: op.ID}
			return result, nil
		}, ToolEffectPolicy{Kind: ToolEffectMutating}))
		if err := runtime.Register("agent", "v1", agent); err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		invocation := snapshot.ToolBatches[0].Invocations[0]
		if invocation.Effect.Receipt != invocation.OperationID {
			t.Fatalf("operation = %#v", invocation)
		}
	})

	t.Run("panic diagnostic", func(t *testing.T) {
		runtime := newTestRuntime(t)
		agent := testAgent(&scriptedProvider{turns: []Message{asstTool("panic-id", "panic", `{}`)}})
		agent.RegisterTool(FuncResult("panic", "panic", func(context.Context, struct{}) (ToolResult, error) {
			panic("boom")
		}))
		if err := runtime.Register("agent", "v1", agent); err != nil {
			t.Fatal(err)
		}
		result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
		if err == nil || !strings.Contains(err.Error(), "panicked: boom") {
			t.Fatalf("panic result = %#v, %v", result, err)
		}
		if len(result.Diagnostics) != 1 || result.Diagnostics[0].Kind != "tool_execution_error" || !strings.Contains(result.Diagnostics[0].Message, "panicked: boom") {
			t.Fatalf("panic diagnostics = %#v", result.Diagnostics)
		}
	})
}

func TestRuntimeInvalidMutatingArgumentsAreKnownNotApplied(t *testing.T) {
	runtime := newTestRuntime(t)
	provider := &scriptedProvider{turns: []Message{asstTool("bad", "create", `{"resource":42}`), asstText("recovered")}}
	var executions atomic.Int32
	tool := FuncResult("create", "create", func(context.Context, struct {
		Resource string `json:"resource"`
	}) (ToolResult, error) {
		executions.Add(1)
		return ToolResult{}, errors.New("must not execute")
	})
	tool = WithToolEffectPolicy(tool, ToolEffectPolicy{
		Kind: ToolEffectMutating, Scope: "invalid-test",
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
	agent, err := New(provider, AgentConfig{Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil || result.Output != "recovered" || executions.Load() != 0 {
		t.Fatalf("invalid mutation = %#v, %v; executions=%d", result, err, executions.Load())
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if invocation.State != ToolInvocationCompleted || invocation.Effect.Status != EffectNotApplied {
		t.Fatalf("invalid argument effect = %#v", invocation)
	}
}

func TestRuntimeSemanticGuardRejectsDuplicateMutationInModelOrder(t *testing.T) {
	runtime := newTestRuntime(t)
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("first", "create", `{"resource":"same"}`), toolUse("second", "create", `{"resource":"same"}`)),
		asstText("done"),
	}}
	var executions atomic.Int32
	tool := FuncResult("create", "create", func(context.Context, struct {
		Resource string `json:"resource"`
	}) (ToolResult, error) {
		executions.Add(1)
		result := TextResult("created")
		result.Effect = EffectReport{Status: EffectApplied, Receipt: "created-same"}
		return result, nil
	})
	tool = WithToolEffectPolicy(tool, ToolEffectPolicy{
		Kind: ToolEffectMutating, Scope: "test-resources",
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
	agent := testAgent(provider)
	agent.RegisterTool(tool)
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 {
		t.Fatalf("mutations = %d, want 1", executions.Load())
	}
	results := transcriptToolResults(result.Messages)
	if len(results) != 2 || results[0].ToolUseID != "first" || results[0].IsError || results[1].ToolUseID != "second" || !results[1].IsError {
		t.Fatalf("guarded results = %#v", results)
	}
}
