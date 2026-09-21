package core

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/tracing"
)

func TestDurableWaitAndEffectWrappersComposeInEitherOrder(t *testing.T) {
	base := FuncResult("write", "write", func(context.Context, struct{}) (ToolResult, error) {
		return ToolResult{}, nil
	})
	waitPolicy := DurableWaitPolicy{Kind: WaitApproval, Target: func(json.RawMessage) (string, error) { return "target", nil }}
	for _, tool := range []Tool{
		WithDurableWait(WithToolEffectPolicy(base, ToolEffectPolicy{Kind: ToolEffectMutating}), waitPolicy),
		WithToolEffectPolicy(WithDurableWait(base, waitPolicy), ToolEffectPolicy{Kind: ToolEffectMutating}),
	} {
		agent, err := New(&scriptedProvider{}, AgentConfig{Tools: []Tool{tool}})
		if err != nil {
			t.Fatal(err)
		}
		stored := agent.tools[0]
		wait, configured, err := waitPolicyFor(stored)
		if err != nil || !configured || wait.Kind != WaitApproval {
			t.Fatalf("wait policy = %#v, %v, %v", wait, configured, err)
		}
		effect, err := effectPolicyFor(stored)
		if err != nil || effect.Kind != ToolEffectMutating {
			t.Fatalf("effect policy = %#v, %v", effect, err)
		}
	}
}

func TestDurableWaitEncodingIsStable(t *testing.T) {
	wait := storedWait{
		Version: runtimeEncodingVersion, RunID: "run-1", ID: "wait-1", Kind: WaitApproval,
		State: WaitPending, OperationID: "op-1", BatchID: "batch-1", Ordinal: 2,
		Tool: "write", Arguments: json.RawMessage(`{"path":"a"}`), Target: "a",
		ActionDigest: "digest", DefinitionID: "agent", DefinitionRevision: "v1",
		PolicyContext: "policy-v2", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	data, err := json.Marshal(wait)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"version":5,"run_id":"run-1","id":"wait-1","kind":"approval","state":"pending","operation_id":"op-1","batch_id":"batch-1","ordinal":2,"tool":"write","arguments":{"path":"a"},"target":"a","action_digest":"digest","definition_id":"agent","definition_revision":"v1","policy_context":"policy-v2","created_at":"2026-01-02T03:04:05Z"}`
	if string(data) != want {
		t.Fatalf("wait encoding drifted:\n got %s\nwant %s", data, want)
	}
	var decoded storedWait
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Version != runtimeEncodingVersion || decoded.ActionDigest != wait.ActionDigest {
		t.Fatalf("decoded wait = %#v", decoded)
	}
}

func waitForRuntimeWait(t *testing.T, handle *RunHandle) WaitSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := handle.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.State == RuntimeWaiting && len(snapshot.Waits) > 0 {
			return snapshot.Waits[len(snapshot.Waits)-1]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("run did not enter durable wait")
	return WaitSnapshot{}
}

func TestRuntimeQuestionWaitSurvivesRestartAndResolutionRetry(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("question-1", "ask_user", `{"prompt":"Which region?"}`),
		asstText("continued"),
	}}
	var executed atomic.Int64
	question := Func("ask_user", "ask the user", func(context.Context, struct {
		Prompt string `json:"prompt"`
	}) (string, error) {
		executed.Add(1)
		return "must not execute", nil
	})
	question = WithDurableWait(question, DurableWaitPolicy{
		Kind: WaitQuestion,
		Prompt: func(args json.RawMessage) (string, error) {
			var input struct {
				Prompt string `json:"prompt"`
			}
			if err := json.Unmarshal(args, &input); err != nil {
				return "", err
			}
			return input.Prompt, nil
		},
	})
	agent, err := New(provider, AgentConfig{Tools: []Tool{question}})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("assistant", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "assistant", "v1", "help", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wait := waitForRuntimeWait(t, handle)
	deadline := time.Now().Add(time.Second)
	for {
		runtime.mu.Lock()
		_, live := runtime.live[handle.ID()]
		runtime.mu.Unlock()
		if !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiting run retained a worker")
		}
		time.Sleep(time.Millisecond)
	}
	if wait.Kind != WaitQuestion || wait.Prompt != "Which region?" || wait.State != WaitPending {
		t.Fatalf("wait = %#v", wait)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Register("assistant", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	handle = reopened.Handle(handle.ID())
	answer := WaitResolution{Answer: json.RawMessage(`"us-east"`)}
	if err := handle.ResolveWait(context.Background(), wait.ID, answer); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "continued" || executed.Load() != 0 {
		t.Fatalf("result = %#v, question executions = %d", result, executed.Load())
	}
	// Simulate a lost command acknowledgement after later run transitions.
	if err := handle.ResolveWait(context.Background(), wait.ID, answer); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	if err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Answer: json.RawMessage(`"eu-west"`)}); !errors.Is(err, ErrWaitConflict) {
		t.Fatalf("conflicting retry = %v", err)
	}
}

func TestRuntimeApprovalBindsActionAndRevalidatesAuthority(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		asstTool("write-1", "write", `{"path":"report.txt"}`),
		asstText("done"),
	}}
	var writes atomic.Int64
	write := FuncResult("write", "write a file", func(context.Context, struct {
		Path string `json:"path"`
	}) (ToolResult, error) {
		writes.Add(1)
		result := TextResult("written")
		result.Effect = EffectReport{Status: EffectApplied, Receipt: "receipt-1"}
		return result, nil
	})
	write = WithToolEffectPolicy(write, ToolEffectPolicy{Kind: ToolEffectMutating})
	write = WithDurableWait(write, DurableWaitPolicy{
		Kind: WaitApproval, PolicyContext: "policy-v1", ExpiresAfter: time.Minute,
		Target: func(args json.RawMessage) (string, error) {
			var input struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &input); err != nil {
				return "", err
			}
			return input.Path, nil
		},
	})
	agent, err := New(provider, AgentConfig{Tools: []Tool{write}})
	if err != nil {
		t.Fatal(err)
	}
	var authorized atomic.Bool
	authorized.Store(true)
	authorizer := ApprovalAuthorizerFunc(func(_ context.Context, request ApprovalAuthorization) error {
		if request.Actor != "user-7" || request.Target != "report.txt" || request.PolicyContext != "policy-v1" {
			return errors.New("wrong authority context")
		}
		if !authorized.Load() {
			return errors.New("revoked")
		}
		return nil
	})
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Authorizer: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Register("writer", "v3", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "writer", "v3", "write", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wait := waitForRuntimeWait(t, handle)
	if wait.Target != "report.txt" || wait.ActionDigest == "" || wait.DefinitionRevision != "v3" {
		t.Fatalf("approval wait = %#v", wait)
	}
	wrong := WaitResolution{Decision: Allow, Actor: "user-7", ActionDigest: "stale"}
	if err := handle.ResolveWait(context.Background(), wait.ID, wrong); !errors.Is(err, ErrApprovalActionMismatch) {
		t.Fatalf("modified/stale action = %v", err)
	}
	resolution := WaitResolution{Decision: Allow, Actor: "user-7", ActionDigest: wait.ActionDigest}
	if err := handle.ResolveWait(context.Background(), wait.ID, resolution); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "done" || writes.Load() != 1 {
		t.Fatalf("result = %#v, writes = %d", result, writes.Load())
	}
	if err := handle.ResolveWait(context.Background(), wait.ID, resolution); err != nil {
		t.Fatalf("lost-response retry: %v", err)
	}
}

func TestDurableApprovalDoesNotAuthorizeNestedToolCalls(t *testing.T) {
	childProvider := &scriptedProvider{turns: []Message{asstTool("leaf-1", "leaf", `{}`), asstText("child adapted")}}
	var leafCalls atomic.Int64
	var childApprovals atomic.Int64
	leaf := Func("leaf", "leaf", func(context.Context, struct{}) (string, error) {
		leafCalls.Add(1)
		return "leaf", nil
	})
	child, err := New(childProvider, AgentConfig{
		Tools: []Tool{leaf},
		Approver: ApproverFunc(func(context.Context, ToolUseBlock, []Message) (Decision, error) {
			childApprovals.Add(1)
			return Decision{Outcome: Deny, Reason: "child denied"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	delegate := Func("delegate", "delegate", func(ctx context.Context, _ struct{}) (string, error) {
		result, err := child.Run(ctx, "child")
		return result.Output, err
	})
	delegate = WithDurableWait(delegate, DurableWaitPolicy{Kind: WaitApproval, Target: func(json.RawMessage) (string, error) { return "child-run", nil }})
	parent, err := New(&scriptedProvider{turns: []Message{asstTool("delegate-1", "delegate", `{}`), asstText("parent done")}}, AgentConfig{Tools: []Tool{delegate}})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Authorizer: ApprovalAuthorizerFunc(func(context.Context, ApprovalAuthorization) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Register("parent", "v1", parent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "parent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wait := waitForRuntimeWait(t, handle)
	if err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Decision: Allow, ActionDigest: wait.ActionDigest}); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Await(context.Background()); err != nil {
		t.Fatal(err)
	}
	if childApprovals.Load() != 1 || leafCalls.Load() != 0 {
		t.Fatalf("child approvals = %d, leaf calls = %d", childApprovals.Load(), leafCalls.Load())
	}
}

func TestRuntimeApprovalIsRevalidatedImmediatelyBeforeDispatch(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{asstTool("write-1", "write", `{}`), asstText("denied safely")}}
	var calls atomic.Int64
	tool := FuncResult("write", "write", func(context.Context, struct{}) (ToolResult, error) {
		calls.Add(1)
		return ToolResult{Blocks: Blocks{TextBlock{Text: "written"}}, Effect: EffectReport{Status: EffectApplied}}, nil
	})
	tool = WithToolEffectPolicy(tool, ToolEffectPolicy{Kind: ToolEffectMutating})
	tool = WithDurableWait(tool, DurableWaitPolicy{Kind: WaitApproval, Target: func(json.RawMessage) (string, error) { return "record-1", nil }})
	agent, err := New(provider, AgentConfig{Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	var checks atomic.Int64
	authorizer := ApprovalAuthorizerFunc(func(context.Context, ApprovalAuthorization) error {
		if checks.Add(1) > 1 {
			return errors.New("authority revoked")
		}
		return nil
	})
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Authorizer: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Register("writer", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "writer", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wait := waitForRuntimeWait(t, handle)
	if err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Decision: Allow, ActionDigest: wait.ActionDigest}); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "denied safely" || calls.Load() != 0 || checks.Load() != 2 {
		t.Fatalf("result = %#v, calls = %d, authority checks = %d", result, calls.Load(), checks.Load())
	}
}

func TestRuntimeExpiredApprovalCannotDispatchAndRetryIsStable(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{asstTool("write-1", "write", `{}`), asstText("expired safely")}}
	var calls atomic.Int64
	tool := FuncResult("write", "write", func(context.Context, struct{}) (ToolResult, error) {
		calls.Add(1)
		return ToolResult{Blocks: Blocks{TextBlock{Text: "written"}}, Effect: EffectReport{Status: EffectApplied}}, nil
	})
	tool = WithToolEffectPolicy(tool, ToolEffectPolicy{Kind: ToolEffectMutating})
	tool = WithDurableWait(tool, DurableWaitPolicy{Kind: WaitApproval, ExpiresAfter: time.Millisecond, Target: func(json.RawMessage) (string, error) { return "record-1", nil }})
	agent, err := New(provider, AgentConfig{Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Authorizer: ApprovalAuthorizerFunc(func(context.Context, ApprovalAuthorization) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Register("writer", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "writer", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wait := waitForRuntimeWait(t, handle)
	time.Sleep(2 * time.Millisecond)
	resolution := WaitResolution{Decision: Allow, ActionDigest: wait.ActionDigest}
	if err := handle.ResolveWait(context.Background(), wait.ID, resolution); !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("expired resolution = %v", err)
	}
	if err := handle.ResolveWait(context.Background(), wait.ID, resolution); !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("expired retry = %v", err)
	}
	result, err := handle.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "expired safely" || calls.Load() != 0 {
		t.Fatalf("result = %#v, calls = %d", result, calls.Load())
	}
}

func TestRuntimeCancellationAfterAcceptedApprovalPreservesReceipt(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{asstTool("write-1", "write", `{}`)}}
	var calls atomic.Int64
	tool := FuncResult("write", "write", func(context.Context, struct{}) (ToolResult, error) {
		calls.Add(1)
		return ToolResult{Blocks: Blocks{TextBlock{Text: "written"}}, Effect: EffectReport{Status: EffectApplied}}, nil
	})
	tool = WithToolEffectPolicy(tool, ToolEffectPolicy{Kind: ToolEffectMutating})
	tool = WithDurableWait(tool, DurableWaitPolicy{Kind: WaitApproval, Target: func(json.RawMessage) (string, error) { return "record-1", nil }})
	agent, err := New(provider, AgentConfig{Tools: []Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	secondCheck := make(chan struct{})
	release := make(chan struct{})
	var checks atomic.Int64
	authorizer := ApprovalAuthorizerFunc(func(context.Context, ApprovalAuthorization) error {
		if checks.Add(1) == 2 {
			close(secondCheck)
			<-release
		}
		return nil
	})
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Authorizer: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if err := runtime.Register("writer", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "writer", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wait := waitForRuntimeWait(t, handle)
	resolution := WaitResolution{Decision: Allow, ActionDigest: wait.ActionDigest}
	if err := handle.ResolveWait(context.Background(), wait.ID, resolution); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondCheck:
	case <-time.After(time.Second):
		t.Fatal("pre-dispatch authority check did not start")
	}
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if _, err := handle.Await(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("await = %v", err)
	}
	if err := handle.ResolveWait(context.Background(), wait.ID, resolution); err != nil {
		t.Fatalf("accepted resolution receipt was lost: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("cancelled approval dispatched %d times", calls.Load())
	}
}

type waitBoundaryStore struct {
	Store
	entered chan struct{}
	release chan struct{}
	paused  atomic.Bool
}

type waitBoundaryTx struct {
	StoreTransaction
	batchReady *bool
}

func (tx waitBoundaryTx) Put(bucket, key string, value []byte) error {
	if bucket == runtimeRunsBucket {
		var record storedRuntimeRun
		if json.Unmarshal(value, &record) == nil && record.LastTransition == "batch_ready" && record.PendingBatchID == "" {
			*tx.batchReady = true
		}
	}
	return tx.StoreTransaction.Put(bucket, key, value)
}

func (s *waitBoundaryStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	batchReady := false
	err := s.Store.Transaction(ctx, writable, func(tx StoreTransaction) error {
		return fn(waitBoundaryTx{StoreTransaction: tx, batchReady: &batchReady})
	})
	if err == nil && batchReady && s.paused.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return err
}

type waitAckStore struct {
	Store
	fail atomic.Bool
}

func (s *waitAckStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	err := s.Store.Transaction(ctx, writable, fn)
	if err == nil && writable && s.fail.CompareAndSwap(true, false) {
		return errors.New("committed but acknowledgement lost")
	}
	return err
}

type waitTestLimiter func(context.Context) error

func (f waitTestLimiter) Wait(ctx context.Context) error { return f(ctx) }

type waitEndTracer struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type waitEndSpan struct {
	tracing.Span
	tracer *waitEndTracer
}

func (t *waitEndTracer) Start(ctx context.Context, name string, attrs ...tracing.Attr) (context.Context, tracing.Span) {
	ctx, span := tracing.Noop.Start(ctx, name, attrs...)
	if name == "agent.run" {
		return ctx, &waitEndSpan{Span: span, tracer: t}
	}
	return ctx, span
}

func (s *waitEndSpan) End() {
	s.tracer.once.Do(func() {
		close(s.tracer.entered)
		<-s.tracer.release
	})
	s.Span.End()
}

func waitForRuntimeIdle(t *testing.T, runtime *Runtime, handle *RunHandle) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.mu.Lock()
		_, live := runtime.live[handle.ID()]
		runtime.mu.Unlock()
		if !live {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runtime worker did not retire")
}

func approvalWaitTool(calls *atomic.Int64) Tool {
	tool := FuncResult("work", "work", func(context.Context, struct{}) (ToolResult, error) {
		calls.Add(1)
		return ToolResult{Blocks: Blocks{TextBlock{Text: "ok"}}, Effect: EffectReport{Status: EffectApplied}}, nil
	})
	return WithDurableWait(
		WithToolEffectPolicy(tool, ToolEffectPolicy{Kind: ToolEffectMutating}),
		DurableWaitPolicy{Kind: WaitApproval, Target: func(json.RawMessage) (string, error) { return "target", nil }},
	)
}

func TestRuntimeCancellationCannotBeReversedByWaitCreation(t *testing.T) {
	var calls atomic.Int64
	agent, err := New(&scriptedProvider{turns: []Message{asstTool("call-1", "work", `{}`)}}, AgentConfig{Tools: []Tool{approvalWaitTool(&calls)}})
	if err != nil {
		t.Fatal(err)
	}
	store := &waitBoundaryStore{Store: NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store, Authorizer: ApprovalAuthorizerFunc(func(context.Context, ApprovalAuthorization) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	var release sync.Once
	defer release.Do(func() { close(store.release) })
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not reach the batch-ready boundary")
	}
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(store.release) })
	result, err := handle.Await(context.Background())
	if !errors.Is(err, context.Canceled) || result.Status != RunCancelled || calls.Load() != 0 {
		t.Fatalf("result = %#v, err = %v, calls = %d", result, err, calls.Load())
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeTerminal || len(snapshot.Waits) != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatalf("cancel receipt retry: %v", err)
	}
}

func TestRuntimeApprovalRevalidatesAfterRateLimitWait(t *testing.T) {
	var calls atomic.Int64
	var revoked atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	authorizer := ApprovalAuthorizerFunc(func(context.Context, ApprovalAuthorization) error {
		if revoked.Load() {
			return errors.New("revoked")
		}
		return nil
	})
	agent, err := New(&scriptedProvider{turns: []Message{asstTool("call-1", "work", `{}`), asstText("done")}}, AgentConfig{
		Tools: []Tool{approvalWaitTool(&calls)},
		ToolPolicy: ToolPolicy{RateLimiter: waitTestLimiter(func(ctx context.Context) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Authorizer: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wait := waitForRuntimeWait(t, handle)
	waitForRuntimeIdle(t, runtime, handle)
	resolution := WaitResolution{Decision: Allow, ActionDigest: wait.ActionDigest}
	if err := handle.ResolveWait(context.Background(), wait.ID, resolution); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("rate limiter was not entered")
	}
	revoked.Store(true)
	close(release)
	if _, err := handle.Await(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("revoked approval dispatched %d times", calls.Load())
	}
	if err := handle.ResolveWait(context.Background(), wait.ID, resolution); err != nil {
		t.Fatalf("historical resolution after revocation: %v", err)
	}
}

func TestRuntimeWaitResolutionHandoffAndLostAcknowledgement(t *testing.T) {
	for _, test := range []struct {
		name   string
		tracer tracing.Tracer
		store  Store
		arm    func()
	}{
		{name: "retiring worker", tracer: &waitEndTracer{entered: make(chan struct{}), release: make(chan struct{})}, store: NewMemoryStore()},
		{name: "lost commit acknowledgement", store: &waitAckStore{Store: NewMemoryStore()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			question := WithDurableWait(Func("work", "question", func(context.Context, struct{}) (string, error) { return "", nil }), DurableWaitPolicy{Kind: WaitQuestion})
			agent, err := New(&scriptedProvider{turns: []Message{asstTool("call-1", "work", `{}`), asstText("done")}}, AgentConfig{Tools: []Tool{question}, Tracer: test.tracer})
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: test.store})
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			if err := runtime.Register("agent", "v1", agent); err != nil {
				t.Fatal(err)
			}
			handle, err := runtime.Submit(context.Background(), "agent", "v1", "go", SubmitOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var releaseTracer sync.Once
			if tracer, ok := test.tracer.(*waitEndTracer); ok {
				select {
				case <-tracer.entered:
				case <-time.After(time.Second):
					t.Fatal("worker did not begin retirement")
				}
				defer releaseTracer.Do(func() { close(tracer.release) })
			}
			wait := waitForRuntimeWait(t, handle)
			if store, ok := test.store.(*waitAckStore); ok {
				waitForRuntimeIdle(t, runtime, handle)
				store.fail.Store(true)
			}
			resolution := WaitResolution{Answer: json.RawMessage(`"answer"`)}
			resolveErr := handle.ResolveWait(context.Background(), wait.ID, resolution)
			if _, ok := test.store.(*waitAckStore); ok {
				if resolveErr == nil {
					t.Fatal("expected lost acknowledgement")
				}
				if err := handle.ResolveWait(context.Background(), wait.ID, resolution); err != nil {
					t.Fatal(err)
				}
			} else if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			if tracer, ok := test.tracer.(*waitEndTracer); ok {
				releaseTracer.Do(func() { close(tracer.release) })
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := handle.Await(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeRecoverAppliesDeadlineToSuspendedWait(t *testing.T) {
	question := WithDurableWait(Func("work", "question", func(context.Context, struct{}) (string, error) { return "", nil }), DurableWaitPolicy{Kind: WaitQuestion})
	agent, err := New(&scriptedProvider{turns: []Message{asstTool("call-1", "work", `{}`)}}, AgentConfig{Tools: []Tool{question}})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "go", SubmitOptions{Deadline: time.Now().Add(20 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	waitForRuntimeWait(t, handle)
	waitForRuntimeIdle(t, runtime, handle)
	time.Sleep(25 * time.Millisecond)
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || result.Status != RunCancelled {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Waits[0].State == WaitPending {
		t.Fatalf("deadline left wait pending: %#v", snapshot.Waits[0])
	}
}

func TestRuntimeObserverRemainsAttachedAcrossWait(t *testing.T) {
	tracer := &waitEndTracer{entered: make(chan struct{}), release: make(chan struct{})}
	question := WithDurableWait(Func("work", "question", func(context.Context, struct{}) (string, error) { return "", nil }), DurableWaitPolicy{Kind: WaitQuestion})
	agent, err := New(&scriptedProvider{turns: []Message{asstTool("call-1", "work", `{}`), asstText("done")}}, AgentConfig{Tools: []Tool{question}, Tracer: tracer})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	var releaseTracer sync.Once
	defer releaseTracer.Do(func() { close(tracer.release) })
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "go", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-tracer.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not begin retirement")
	}
	wait := waitForRuntimeWait(t, handle)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var events atomic.Int64
	go func() { done <- handle.Observe(ctx, func(StreamEvent) { events.Add(1) }) }()
	deadline := time.Now().Add(time.Second)
	for {
		runtime.mu.Lock()
		subscribed := len(runtime.subscribers[handle.ID()]) > 0
		runtime.mu.Unlock()
		if subscribed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("observer did not subscribe")
		}
		time.Sleep(time.Millisecond)
	}
	releaseTracer.Do(func() { close(tracer.release) })
	select {
	case err := <-done:
		t.Fatalf("observer ended at suspension: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Answer: json.RawMessage(`"answer"`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("observer did not receive terminal completion")
	}
	if events.Load() == 0 {
		t.Fatal("observer received no continuation events")
	}
}

func TestRuntimeRunStreamContinuesAcrossWait(t *testing.T) {
	tracer := &waitEndTracer{entered: make(chan struct{}), release: make(chan struct{})}
	question := WithDurableWait(Func("work", "question", func(context.Context, struct{}) (string, error) { return "", nil }), DurableWaitPolicy{Kind: WaitQuestion})
	agent, err := New(&scriptedProvider{turns: []Message{asstTool("call-1", "work", `{}`), asstText("done")}}, AgentConfig{Tools: []Tool{question}, Tracer: tracer})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	var releaseTracer sync.Once
	defer releaseTracer.Do(func() { close(tracer.release) })
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	type streamOutcome struct {
		result RunResult
		err    error
	}
	done := make(chan streamOutcome, 1)
	var events atomic.Int64
	go func() {
		result, err := runtime.RunStream(context.Background(), "agent", "v1", "go", func(StreamEvent) { events.Add(1) }, SubmitOptions{})
		done <- streamOutcome{result: result, err: err}
	}()
	select {
	case <-tracer.entered:
	case <-time.After(time.Second):
		t.Fatal("stream worker did not begin retirement")
	}
	var runID string
	if err := runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
		return tx.Scan(runtimeRunsBucket, "", func(key string, _ []byte) error {
			runID = key
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("admitted run not found")
	}
	handle := runtime.Handle(runID)
	wait := waitForRuntimeWait(t, handle)
	releaseTracer.Do(func() { close(tracer.release) })
	select {
	case outcome := <-done:
		t.Fatalf("stream ended at suspension: %#v", outcome)
	case <-time.After(30 * time.Millisecond):
	}
	if err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Answer: json.RawMessage(`"answer"`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.result.Output != "done" {
			t.Fatalf("stream outcome = %#v", outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not complete after wait resolution")
	}
	if events.Load() == 0 {
		t.Fatal("stream received no continuation events")
	}
}

func TestRuntimeApprovalRevocationAndCancellationPreventDispatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		action func(*RunHandle, WaitSnapshot)
	}{
		{name: "revoked", action: func(handle *RunHandle, wait WaitSnapshot) {
			err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Decision: Allow, Actor: "revoked", ActionDigest: wait.ActionDigest})
			if !errors.Is(err, ErrApprovalUnauthorized) {
				t.Errorf("resolution = %v", err)
			}
		}},
		{name: "cancelled", action: func(handle *RunHandle, _ WaitSnapshot) {
			if err := handle.Cancel(context.Background()); err != nil {
				t.Errorf("cancel: %v", err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &scriptedProvider{turns: []Message{asstTool("mutate-1", "mutate", `{}`)}}
			var calls atomic.Int64
			tool := FuncResult("mutate", "mutate", func(context.Context, struct{}) (ToolResult, error) {
				calls.Add(1)
				return ToolResult{Blocks: Blocks{TextBlock{Text: "ok"}}, Effect: EffectReport{Status: EffectApplied}}, nil
			})
			tool = WithToolEffectPolicy(tool, ToolEffectPolicy{Kind: ToolEffectMutating})
			tool = WithDurableWait(tool, DurableWaitPolicy{Kind: WaitApproval, Target: func(json.RawMessage) (string, error) { return "resource", nil }})
			agent, err := New(provider, AgentConfig{Tools: []Tool{tool}})
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore(), Authorizer: ApprovalAuthorizerFunc(func(context.Context, ApprovalAuthorization) error { return errors.New("revoked") })})
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			if err := runtime.Register("agent", "v1", agent); err != nil {
				t.Fatal(err)
			}
			handle, err := runtime.Submit(context.Background(), "agent", "v1", "go", SubmitOptions{})
			if err != nil {
				t.Fatal(err)
			}
			wait := waitForRuntimeWait(t, handle)
			test.action(handle, wait)
			if calls.Load() != 0 {
				t.Fatalf("mutation dispatched %d times", calls.Load())
			}
			if test.name == "cancelled" {
				if err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Decision: Deny, ActionDigest: wait.ActionDigest}); !errors.Is(err, ErrWaitConflict) {
					t.Fatalf("post-cancel response = %v", err)
				}
			}
		})
	}
}
