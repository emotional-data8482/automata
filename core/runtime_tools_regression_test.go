package core

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeToolTimeoutRetainsAuthoritativeEffect(t *testing.T) {
	for _, status := range []EffectStatus{EffectApplied, EffectNotApplied, EffectUnknown} {
		t.Run(string(status), func(t *testing.T) {
			runtime := newTestRuntime(t)
			agent := testAgent(&scriptedProvider{turns: []Message{asstTool("write-id", "write", `{}`), asstText("done")}})
			agent.WithToolPolicy(ToolPolicy{Timeout: time.Millisecond})
			report := EffectReport{Status: status, Receipt: "destination-evidence"}
			agent.RegisterTool(effectTool("write", nil, func(ctx context.Context) (ToolResult, error) {
				<-ctx.Done()
				result := TextResult("destination response")
				result.Effect = report
				return result, ctx.Err()
			}, ToolEffectPolicy{Kind: ToolEffectMutating}))
			if _, err := runtime.Register("agent", "v1", agent); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, runErr := runtime.Run(ctx, DefinitionRef{ID: "agent", Revision: "v1"}, "work")
			snapshot, err := runtime.Handle(result.RunID).Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			invocation := snapshot.ToolBatches[0].Invocations[0]
			if invocation.Effect != report {
				t.Fatalf("effect = %+v, want %+v", invocation.Effect, report)
			}
			if status == EffectUnknown {
				if !errors.Is(runErr, ErrRunNeedsAttention) || invocation.State != ToolInvocationUncertain {
					t.Fatalf("unknown outcome = %s, %v", invocation.State, runErr)
				}
				return
			}
			if runErr != nil || result.Output != "done" || invocation.State != ToolInvocationCompleted {
				t.Fatalf("recoverable timeout = %#v, %v", snapshot, runErr)
			}
			results := transcriptToolResults(result.Messages)
			if len(results) != 1 || !results[0].IsError || !strings.Contains(invocation.Result.Text(), "timeout:") {
				t.Fatalf("model-facing timeout lost: %#v", results)
			}
		})
	}
}

func TestRuntimeCancelDuringUncertainToolRemainsTerminal(t *testing.T) {
	runtime := newTestRuntime(t)
	started := make(chan struct{})
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("write-id", "write", `{}`), asstText("must not resume")}})
	var executions atomic.Int32
	agent.RegisterTool(effectTool("write", &executions, func(ctx context.Context) (ToolResult, error) {
		close(started)
		<-ctx.Done()
		return ToolResult{Effect: EffectReport{Status: EffectUnknown}}, ctx.Err()
	}, ToolEffectPolicy{Kind: ToolEffectMutating}))
	if _, err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handle, err := runtime.Submit(ctx, DefinitionRef{ID: "agent", Revision: "v1"}, "work")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := handle.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(ctx)
	if !errors.Is(err, context.Canceled) || result.Status != RunCancelled {
		t.Fatalf("cancelled result = %#v, %v", result, err)
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	invocation := snapshot.ToolBatches[0].Invocations[0]
	if snapshot.State != RuntimeTerminal || invocation.State != ToolInvocationUncertain {
		t.Fatalf("cancelled snapshot = %#v", snapshot)
	}
	resolution := EffectResolution{Result: TextResult("verified write"), Effect: EffectReport{Status: EffectApplied, Receipt: "late-receipt"}}
	for range 2 {
		if err := handle.Reconcile(ctx, invocation.OperationID, resolution); err != nil {
			t.Fatal(err)
		}
		if err := handle.Cancel(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	result, err = handle.Await(ctx)
	if !errors.Is(err, context.Canceled) || result.Status != RunCancelled || result.Output == "must not resume" {
		t.Fatalf("reconciled cancellation = %#v, %v", result, err)
	}
	snapshot, err = handle.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ToolBatches[0].Invocations[0].Effect != resolution.Effect || executions.Load() != 1 {
		t.Fatalf("late evidence lost or tool replayed: %#v, executions=%d", snapshot, executions.Load())
	}
}

func TestRuntimeRecoveryRetainsFatalBatchOutcome(t *testing.T) {
	// Both sides of the batch-commit boundary must preserve the fatal result:
	// the batch commit itself, and execution finalization after it.
	for _, boundary := range []struct {
		name  string
		match func(string, []byte) bool
	}{
		{"batch_commit", committingTransition("batch_committed")},
		{"finalization", enteringState(RuntimeFinalizing)},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			base := NewMemoryStore().(*memoryStore)
			store := &writeFaultStore{Store: noCloseStore{Store: base}, match: boundary.match}
			runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			var executions atomic.Int32
			agent := testAgent(&scriptedProvider{turns: []Message{asstTool("write-id", "write", `{}`), asstText("must not resume")}})
			agent.RegisterTool(effectTool("write", &executions, func(context.Context) (ToolResult, error) {
				return ToolResult{Effect: EffectReport{Status: EffectApplied, Receipt: "write-1"}}, errors.New("fatal write error")
			}, ToolEffectPolicy{Kind: ToolEffectMutating}))
			if _, err := runtime.Register("agent", "v1", agent); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := runtime.Run(ctx, DefinitionRef{ID: "agent", Revision: "v1"}, "work")
			if err == nil {
				t.Fatal("injected persistence failure was invisible")
			}
			record := getRecord(t, base, result.RunID)
			if boundary.name == "finalization" && (record.LastTransition != "batch_committed" || record.State != RuntimeRunning) {
				t.Fatalf("wrong fault boundary: %#v", record)
			}
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: base}})
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			if _, err := recovered.Register("agent", "v1", agent); err != nil {
				t.Fatal(err)
			}
			if err := recovered.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			result, err = recovered.Handle(result.RunID).Await(ctx)
			if err == nil || !strings.Contains(err.Error(), "fatal write error") || result.Status != RunFailed {
				t.Fatalf("fatal batch resumed: %#v, %v", result, err)
			}
			if executions.Load() != 1 || result.Turns != 1 || len(transcriptToolResults(result.Messages)) != 1 {
				t.Fatalf("recovery repeated work or lost transcript: %#v, executions=%d", result, executions.Load())
			}
			if len(result.Diagnostics) == 0 || !strings.Contains(result.Diagnostics[0].Message, "fatal write error") {
				t.Fatalf("fatal diagnostic lost: %#v", result.Diagnostics)
			}
		})
	}
}
