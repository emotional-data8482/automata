package core

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeRecoveryPreservesEffectiveToolSelection(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &failWritableTransactionStore{Store: noCloseStore{Store: base}, failAt: 6}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int32
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("hidden", "write", `{}`), asstText("done")}})
	agent.RegisterTool(FuncResult("write", "write", func(context.Context, struct{}) (ToolResult, error) {
		executions.Add(1)
		return TextResult("wrote"), nil
	}))
	agent.preSendHooks = []PreSendHook{func(_ context.Context, request Request) (Request, error) {
		request.Tools = nil
		return request, nil
	}}
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if !errors.Is(err, ErrRunNeedsAttention) || executions.Load() != 0 {
		t.Fatalf("initial run = %#v, %v; executions=%d", result, err, executions.Load())
	}
	_ = runtime.Close()

	reopened, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err = reopened.Handle(result.RunID).Await(ctx)
	if err != nil || result.Output != "done" || executions.Load() != 0 {
		t.Fatalf("recovered run = %#v, %v; executions=%d", result, err, executions.Load())
	}
	results := transcriptToolResults(result.Messages)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("unavailable tool result = %#v", results)
	}
}

func TestRuntimeRecoveryNeverRepeatsUncertainCommittedHook(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	var deliveries atomic.Int32
	hooks := []CommittedRunHook{{Name: "effect", Handle: func(context.Context, RunSnapshot) error {
		deliveries.Add(1)
		return nil
	}}}
	// Write 7 commits the hook-delivery marker; write 8 records the delivered
	// outcome and is the fault that leaves delivery uncertain.
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{
		Store: &failWritableTransactionStore{Store: noCloseStore{Store: base}, failAt: 8}, Hooks: hooks,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := testAgent(&countingRuntimeProvider{})
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err == nil {
		t.Fatal("expected hook-outcome persistence fault")
	}
	_ = runtime.Close()

	reopened, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := reopened.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := reopened.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if deliveries.Load() != 1 || snapshot.State != RuntimeNeedsAttention || snapshot.Result.Output != "done" {
		t.Fatalf("hook deliveries=%d snapshot=%#v", deliveries.Load(), snapshot)
	}
}

func TestRuntimeCancelResolvesReservedMutationAsNotApplied(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: &failWritableTransactionStore{Store: noCloseStore{Store: base}, failAt: 7}})
	if err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int32
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("first", "write", `{}`), asstTool("next", "write", `{}`), asstText("done")}})
	agent.RegisterTool(effectTool("write", &executions, func(context.Context) (ToolResult, error) {
		return ToolResult{Effect: EffectReport{Status: EffectApplied}}, nil
	}, ToolEffectPolicy{Kind: ToolEffectMutating, Scope: "scope", SemanticKey: func(json.RawMessage) (string, error) { return "resource", nil }}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	first, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if !errors.Is(err, ErrRunNeedsAttention) {
		t.Fatalf("first run error = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle := reopened.Handle(first.RunID)
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if invocation.State != ToolInvocationCompleted || invocation.Effect.Status != EffectNotApplied {
		t.Fatalf("cancelled reservation = %#v", invocation)
	}
	if _, err := reopened.Run(context.Background(), "agent", "v1", "work2", SubmitOptions{}); err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions=%d, want 1", executions.Load())
	}
}

func TestRuntimeToolDeadlineIdentitySurvivesInvocationStorage(t *testing.T) {
	runtime := newTestRuntime(t)
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("read", "read", `{}`)}})
	agent.RegisterTool(effectTool("read", nil, func(ctx context.Context) (ToolResult, error) {
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	}, ToolEffectPolicy{Kind: ToolEffectReadOnly}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{Deadline: time.Now().Add(20 * time.Millisecond)})
	if !errors.Is(err, context.DeadlineExceeded) || result.Status != RunCancelled {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestLegacyErrorAdapterPreservesEffectEvidence(t *testing.T) {
	for _, report := range []EffectReport{
		{Status: EffectApplied, Receipt: "applied"},
		{Status: EffectNotApplied, Receipt: "not-applied"},
		{Status: EffectUnknown, Receipt: "unknown"},
	} {
		t.Run(string(report.Status), func(t *testing.T) {
			tool := WithLegacyToolErrors(effectTool("write", nil, func(context.Context) (ToolResult, error) {
				return ToolResult{Effect: report}, errors.New("post-write failure")
			}, ToolEffectPolicy{Kind: ToolEffectMutating}))
			result, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil || result.Effect != report || !result.IsError {
				t.Fatalf("adapted result=%#v err=%v", result, err)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := EffectReport{Status: EffectApplied, Receipt: "before-cancel"}
	tool := WithLegacyToolErrors(effectTool("write", nil, func(context.Context) (ToolResult, error) {
		return ToolResult{Effect: report}, context.Canceled
	}, ToolEffectPolicy{Kind: ToolEffectMutating}))
	result, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if !errors.Is(err, context.Canceled) || result.Effect != report {
		t.Fatalf("fatal adaptation result=%#v err=%v", result, err)
	}
}

func TestRuntimeLegacyAdapterKeepsAppliedEffectCompleted(t *testing.T) {
	runtime := newTestRuntime(t)
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("write", "write", `{}`), asstText("done")}})
	tool := effectTool("write", nil, func(context.Context) (ToolResult, error) {
		return ToolResult{Effect: EffectReport{Status: EffectApplied, Receipt: "receipt"}}, errors.New("post-write failure")
	}, ToolEffectPolicy{Kind: ToolEffectMutating})
	agent.RegisterTool(WithLegacyToolErrors(tool))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil || result.Output != "done" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if invocation.State != ToolInvocationCompleted || invocation.Effect.Status != EffectApplied || invocation.Effect.Receipt != "receipt" {
		t.Fatalf("invocation=%#v", invocation)
	}
}

func TestRuntimeFatalToolEmitsOneResultEvent(t *testing.T) {
	runtime := newTestRuntime(t)
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("fail", "fail", `{}`)}})
	agent.RegisterTool(FuncResult("fail", "fail", func(context.Context, struct{}) (ToolResult, error) {
		return ToolResult{}, errors.New("fail")
	}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	var events []StreamEvent
	_, err := runtime.RunStream(context.Background(), "agent", "v1", "work", func(event StreamEvent) {
		if event.Kind == StreamToolResult {
			events = append(events, event)
		}
	}, SubmitOptions{})
	if err == nil || len(events) != 1 || events[0].ToolCall.ID != "fail" || events[0].Err == nil || !events[0].IsError {
		t.Fatalf("events=%#v err=%v", events, err)
	}
}

func TestRuntimeSemanticGuardRejectionEmitsResult(t *testing.T) {
	runtime := newTestRuntime(t)
	agent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("first", "write", `{}`), toolUse("duplicate", "write", `{}`)),
		asstText("done"),
	}})
	var executions atomic.Int32
	agent.RegisterTool(effectTool("write", &executions, func(context.Context) (ToolResult, error) {
		return ToolResult{Blocks: Blocks{TextBlock{Text: "wrote"}}, Effect: EffectReport{Status: EffectApplied}}, nil
	}, ToolEffectPolicy{Kind: ToolEffectMutating, Scope: "scope", SemanticKey: func(json.RawMessage) (string, error) { return "same", nil }}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	resultIDs := make(map[string]int)
	result, err := runtime.RunStream(context.Background(), "agent", "v1", "work", func(event StreamEvent) {
		if event.Kind == StreamToolResult {
			resultIDs[event.ToolCall.ID]++
		}
	}, SubmitOptions{})
	if err != nil || result.Output != "done" || executions.Load() != 1 || resultIDs["first"] != 1 || resultIDs["duplicate"] != 1 {
		t.Fatalf("result=%#v IDs=%v executions=%d err=%v", result, resultIDs, executions.Load(), err)
	}
}

func TestRuntimeFatalBatchEmitsQueuedCancellationResult(t *testing.T) {
	runtime := newTestRuntime(t)
	agent := testAgent(&scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("fatal", "fatal", `{}`), toolUse("queued", "queued", `{}`)),
	}})
	agent.WithToolPolicy(ToolPolicy{MaxParallel: 1})
	agent.RegisterTool(FuncResult("fatal", "fatal", func(context.Context, struct{}) (ToolResult, error) {
		return ToolResult{}, errors.New("fatal")
	}))
	var queuedExecutions atomic.Int32
	agent.RegisterTool(FuncResult("queued", "queued", func(context.Context, struct{}) (ToolResult, error) {
		queuedExecutions.Add(1)
		return TextResult("unexpected"), nil
	}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	var resultIDs []string
	_, err := runtime.RunStream(context.Background(), "agent", "v1", "work", func(event StreamEvent) {
		if event.Kind == StreamToolResult {
			resultIDs = append(resultIDs, event.ToolCall.ID)
		}
	}, SubmitOptions{})
	if err == nil || queuedExecutions.Load() != 0 || len(resultIDs) != 2 || resultIDs[0] != "fatal" || resultIDs[1] != "queued" {
		t.Fatalf("result IDs=%v queued executions=%d err=%v", resultIDs, queuedExecutions.Load(), err)
	}
}

// A fault on the hook-delivery marker leaves no hook invoked, so recovery
// delivers each hook exactly once and commits the terminal result.
func TestRuntimeRecoveryDeliversHookNeverStarted(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	var deliveries atomic.Int32
	hooks := []CommittedRunHook{{Name: "effect", Handle: func(context.Context, RunSnapshot) error {
		deliveries.Add(1)
		return nil
	}}}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{
		Store: &failWritableTransactionStore{Store: noCloseStore{Store: base}, failAt: 7}, Hooks: hooks,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := testAgent(&countingRuntimeProvider{})
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err == nil || deliveries.Load() != 0 {
		t.Fatalf("marker fault = %v, deliveries %d", err, deliveries.Load())
	}
	_ = runtime.Close()

	reopened, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := reopened.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := reopened.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if deliveries.Load() != 1 || snapshot.State != RuntimeTerminal || snapshot.Result.Output != "done" || len(snapshot.HookResults) != 1 {
		t.Fatalf("hook deliveries=%d snapshot=%#v", deliveries.Load(), snapshot)
	}
}
