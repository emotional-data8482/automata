package core

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

const pruneNow = time.Nanosecond

// Each retention class applies independently: events leave an explicit gap,
// history leaves the accepted outcome and effect evidence, and deleting runs
// leaves tombstones so exact retries resolve to ErrRunPruned instead of new
// work. A repeated pass finds nothing left to prune.
func TestRuntimePruneAppliesEachClassIndependently(t *testing.T) {
	var executions atomic.Int32
	write := effectTool("write", &executions, func(context.Context) (ToolResult, error) {
		result := TextResult("wrote")
		result.Effect = EffectReport{Status: EffectApplied, Receipt: "receipt-1"}
		return result, nil
	}, ToolEffectPolicy{Kind: ToolEffectMutating})
	agent, err := New(&scriptedProvider{turns: []Message{asstTool("w1", "write", `{}`), asstText("done")}}, AgentConfig{Tools: []Tool{write}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	options := SubmitOptions{Scope: "tenant", Key: "job-1"}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", options)
	if err != nil {
		t.Fatal(err)
	}
	handle := runtime.Handle(result.RunID)
	ctx := context.Background()

	report, err := runtime.Prune(ctx, RetentionPolicy{Events: pruneNow})
	if err != nil || report.Events != 1 || report.History != 0 || report.Runs != 0 {
		t.Fatalf("events pass = %#v, %v", report, err)
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil || len(snapshot.Result.Messages) == 0 || snapshot.EventSequence == 0 {
		t.Fatalf("snapshot after events pass = %#v, %v", snapshot, err)
	}
	if _, err := handle.Events(ctx, 0, 10); !errors.Is(err, ErrEventGap) {
		t.Fatalf("cursor inside pruned events = %v, want ErrEventGap", err)
	}
	if page, err := handle.Events(ctx, snapshot.EventSequence, 10); err != nil || len(page.Events) != 0 {
		t.Fatalf("resynchronized cursor = %#v, %v", page, err)
	}

	report, err = runtime.Prune(ctx, RetentionPolicy{History: pruneNow})
	if err != nil || report.History != 1 {
		t.Fatalf("history pass = %#v, %v", report, err)
	}
	snapshot, err = handle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if !snapshot.HistoryPruned || len(snapshot.Result.Messages) != 0 || snapshot.Result.Output != "done" ||
		!invocation.ResultPruned || len(invocation.Result.Blocks) != 0 || invocation.Effect.Receipt != "receipt-1" {
		t.Fatalf("snapshot after history pass = %#v", snapshot)
	}

	report, err = runtime.Prune(ctx, RetentionPolicy{Runs: pruneNow})
	if err != nil || report.Runs != 1 {
		t.Fatalf("runs pass = %#v, %v", report, err)
	}
	if _, err := handle.Snapshot(ctx); !errors.Is(err, ErrRunPruned) {
		t.Fatalf("snapshot of pruned run = %v, want ErrRunPruned", err)
	}
	if _, err := handle.Events(ctx, 0, 10); !errors.Is(err, ErrRunPruned) {
		t.Fatalf("events of pruned run = %v, want ErrRunPruned", err)
	}
	if err := handle.Cancel(ctx); !errors.Is(err, ErrRunPruned) {
		t.Fatalf("cancel of pruned run = %v, want ErrRunPruned", err)
	}
	if _, err := runtime.Submit(ctx, "agent", "v1", "work", options); !errors.Is(err, ErrRunPruned) {
		t.Fatalf("exact admission retry = %v, want ErrRunPruned", err)
	}
	if _, err := runtime.Submit(ctx, "agent", "v1", "other work", options); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("changed admission under a pruned identity = %v, want ErrAdmissionConflict", err)
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("tool executions = %d, want 1", got)
	}

	report, err = runtime.Prune(ctx, RetentionPolicy{Events: pruneNow, History: pruneNow, Runs: pruneNow})
	if err != nil || report != (PruneReport{}) {
		t.Fatalf("repeated pass = %#v, %v; want nothing left", report, err)
	}
}

// A terminal run with an unresolved effect is not settled, so no retention
// class touches it until the effect is reconciled.
func TestRuntimePruneWaitsForUnresolvedEffects(t *testing.T) {
	runtime := newTestRuntime(t)
	started := make(chan struct{})
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("write-id", "write", `{}`)}})
	agent.RegisterTool(effectTool("write", nil, func(ctx context.Context) (ToolResult, error) {
		close(started)
		<-ctx.Done()
		return ToolResult{Effect: EffectReport{Status: EffectUnknown}}, ctx.Err()
	}, ToolEffectPolicy{Kind: ToolEffectMutating}))
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	handle, err := runtime.Submit(ctx, "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, started, "tool dispatch")
	if err := handle.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Await(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run = %v", err)
	}
	all := RetentionPolicy{Events: pruneNow, History: pruneNow, Runs: pruneNow}
	if report, err := runtime.Prune(ctx, all); err != nil || report != (PruneReport{}) {
		t.Fatalf("prune with an uncertain effect = %#v, %v; want nothing pruned", report, err)
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if err := handle.Reconcile(ctx, invocation.OperationID, EffectResolution{
		Result: TextResult("verified"), Effect: EffectReport{Status: EffectApplied, Receipt: "late"},
	}); err != nil {
		t.Fatal(err)
	}
	if report, err := runtime.Prune(ctx, all); err != nil || report.Events != 1 || report.History != 1 || report.Runs != 1 {
		t.Fatalf("prune after reconciliation = %#v, %v", report, err)
	}
}

// Deleting runs keeps effect guards, so a semantic duplicate of a pruned
// run's applied mutation is still rejected; conversation turns are never
// history-pruned or deleted because later turns reference them.
func TestRuntimePruneKeepsGuardsAndConversations(t *testing.T) {
	var executions atomic.Int32
	guarded := effectTool("write", &executions, func(context.Context) (ToolResult, error) {
		result := TextResult("wrote")
		result.Effect = EffectReport{Status: EffectApplied, Receipt: "receipt"}
		return result, nil
	}, ToolEffectPolicy{Kind: ToolEffectMutating, Scope: "docs", SemanticKey: func(json.RawMessage) (string, error) { return "report.md", nil }})
	provider := &scriptedProvider{turns: []Message{
		asstTool("w1", "write", `{}`), asstText("first done"),
		asstTool("w2", "write", `{}`), asstText("second done"),
	}}
	agent, err := New(provider, AgentConfig{Tools: []Tool{guarded}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := runtime.Run(ctx, "agent", "v1", "first", SubmitOptions{}); err != nil {
		t.Fatal(err)
	}
	if report, err := runtime.Prune(ctx, RetentionPolicy{Runs: pruneNow}); err != nil || report.Runs != 1 {
		t.Fatalf("runs pass = %#v, %v", report, err)
	}
	second, err := runtime.Run(ctx, "agent", "v1", "second", SubmitOptions{})
	if err != nil || second.Output != "second done" {
		t.Fatalf("second run = %#v, %v", second, err)
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("guarded mutation executed %d times after its run was pruned, want 1", got)
	}

	chat := newTestRuntime(t)
	if err := chat.Register("chat", "v1", testAgent(&repeatingChildProvider{})); err != nil {
		t.Fatal(err)
	}
	first, err := chat.Run(ctx, "chat", "v1", "hello", turnOptions(""))
	if err != nil {
		t.Fatal(err)
	}
	if report, err := chat.Prune(ctx, RetentionPolicy{History: pruneNow, Runs: pruneNow}); err != nil || report.History != 0 || report.Runs != 0 {
		t.Fatalf("conversation prune = %#v, %v; want turns kept", report, err)
	}
	next, err := chat.Run(ctx, "chat", "v1", "again", turnOptions(first.RunID))
	if err != nil || len(next.Messages) != 4 {
		t.Fatalf("turn after prune = %#v, %v", next, err)
	}
}

// Each class prunes at most Limit runs per call and reports More while runs
// are due, so hosts can bound the work of one call.
func TestRuntimePruneLimitBoundsOneCall(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&repeatingChildProvider{})); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 3 {
		if _, err := runtime.Run(ctx, "agent", "v1", "work", SubmitOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	policy := RetentionPolicy{Runs: pruneNow, Limit: 2}
	if report, err := runtime.Prune(ctx, policy); err != nil || report.Runs != 2 || !report.More {
		t.Fatalf("first bounded pass = %#v, %v", report, err)
	}
	if report, err := runtime.Prune(ctx, policy); err != nil || report.Runs != 1 || report.More {
		t.Fatalf("second bounded pass = %#v, %v", report, err)
	}
	if _, err := runtime.Prune(ctx, RetentionPolicy{Runs: -1}); err == nil {
		t.Fatal("negative retention age accepted")
	}
}

// A root run is deleted with its durable child subtree; the child is never
// deleted on its own while its root is retained.
func TestRuntimePruneDeletesRootWithChildren(t *testing.T) {
	parent := testAgent(&scriptedProvider{turns: []Message{
		asstTool("c1", "delegate", `{"topic":"tea"}`),
		asstText("parent done"),
	}})
	newSharedChildDefinition(parent)
	runtime := newTestRuntime(t)
	if err := runtime.Register("child", "v1", testAgent(&repeatingChildProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	result, err := runtime.Run(ctx, "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	childID := snapshot.ToolBatches[0].Invocations[0].ChildRunID
	if childID == "" {
		t.Fatal("parent has no linked child")
	}
	if report, err := runtime.Prune(ctx, RetentionPolicy{Runs: pruneNow}); err != nil || report.Runs != 1 {
		t.Fatalf("runs pass = %#v, %v; want one root pruned", report, err)
	}
	for _, runID := range []string{result.RunID, childID} {
		if _, err := runtime.Handle(runID).Snapshot(ctx); !errors.Is(err, ErrRunPruned) {
			t.Fatalf("run %s after pruning its root = %v, want ErrRunPruned", runID, err)
		}
	}
}
