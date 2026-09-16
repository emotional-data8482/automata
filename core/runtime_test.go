package core

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeRequiresExplicitStore(t *testing.T) {
	if _, err := NewRuntime(context.Background(), RuntimeConfig{}); err == nil {
		t.Fatal("nil store silently selected ephemeral execution")
	}
	runtime, err := NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(context.Background(), RuntimeConfig{Store: failingRuntimeStore{}}); err == nil {
		t.Fatal("unavailable persistent store silently fell back")
	}
}

func TestRuntimeUsesExistingLoopAndStableRunIdentity(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{asstText("done")}}
	agent := testAgent(provider)
	runtime := newTestRuntime(t)
	if err := runtime.Register("answerer", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "answerer", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" || result.Output != "done" || result.Status != RunCompleted {
		t.Fatalf("result = %#v", result)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RunID != result.RunID || snapshot.Result.RunID != result.RunID || snapshot.State != RuntimeTerminal {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestRuntimeAdmissionIsIdempotent(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("ok")}})); err != nil {
		t.Fatal(err)
	}
	options := SubmitOptions{Scope: "tenant", Key: "task-1"}
	a, err := runtime.Submit(context.Background(), "agent", "v1", "same", options)
	if err != nil {
		t.Fatal(err)
	}
	b, err := runtime.Submit(context.Background(), "agent", "v1", "same", options)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() {
		t.Fatalf("duplicate admission IDs = %q, %q", a.ID(), b.ID())
	}
	if _, err := runtime.Submit(context.Background(), "agent", "v1", "changed", options); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("changed duplicate = %v", err)
	}
}

func TestRuntimeCompletedAdmissionRetryReturnsStoredResult(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("ok")}})); err != nil {
		t.Fatal(err)
	}
	options := SubmitOptions{Scope: "tenant", Key: "completed-task"}
	first, err := runtime.Run(context.Background(), "agent", "v1", "same", options)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := runtime.Submit(context.Background(), "agent", "v1", "same", options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := retried.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID != first.RunID || result.Output != first.Output || result.Status != RunCompleted {
		t.Fatalf("retried result = %#v, want %#v", result, first)
	}
}

func TestRuntimeViewCancellationDoesNotCancelRun(t *testing.T) {
	provider := &barrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-provider.started
	view, cancel := context.WithCancel(context.Background())
	cancel()
	partial, err := handle.Await(view)
	if !errors.Is(err, context.Canceled) || partial.RunID != handle.ID() {
		t.Fatalf("disconnected view = %#v, %v", partial, err)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeRunning {
		t.Fatalf("logical run after disconnect = %#v, %v", snapshot, err)
	}
	close(provider.release)
	result, err := handle.Await(context.Background())
	if err != nil || result.Output != "done" {
		t.Fatalf("continued result = %#v, %v", result, err)
	}
}

func TestRuntimeSyncHelperDisconnectCanReattachByAdmissionIdentity(t *testing.T) {
	provider := &barrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	view, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	options := SubmitOptions{Scope: "tenant", Key: "reattach"}
	go func() {
		_, err := runtime.Run(view, "agent", "v1", "work", options)
		finished <- err
	}()
	<-provider.started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("sync view disconnect = %v", err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", options)
	if err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	result, err := handle.Await(context.Background())
	if err != nil || result.Output != "done" || result.RunID != handle.ID() {
		t.Fatalf("reattached result = %#v, %v", result, err)
	}
}

func TestRuntimeStreamIsAViewOfSameRun(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("streamed")}})); err != nil {
		t.Fatal(err)
	}
	var text string
	result, err := runtime.RunStream(context.Background(), "agent", "v1", "work", func(event StreamEvent) {
		if event.Kind == StreamText {
			text += event.Text
		}
	}, SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" || result.Output != "streamed" || text != "streamed" {
		t.Fatalf("stream result = %#v, text=%q", result, text)
	}
}

func TestRuntimeExplicitCancelOwnsLogicalCancellation(t *testing.T) {
	provider := &cancelRuntimeProvider{started: make(chan struct{})}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-provider.started
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if !errors.Is(err, context.Canceled) || result.Status != RunCancelled {
		t.Fatalf("cancel result = %#v, %v", result, err)
	}
}

func TestRuntimeUsesRequestTransformsButNotDirectRunObservers(t *testing.T) {
	var transforms atomic.Int32
	var legacyObservations atomic.Int32
	agent, err := New(&scriptedProvider{turns: []Message{asstText("done")}}, AgentConfig{
		Observers: []RunObserver{func(context.Context, RunEvent) {
			legacyObservations.Add(1)
		}},
		PreSendHooks: []PreSendHook{func(_ context.Context, request Request) (Request, error) {
			transforms.Add(1)
			return request, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil || result.Status != RunCompleted {
		t.Fatalf("durable result = %#v, %v", result, err)
	}
	if transforms.Load() != 1 || legacyObservations.Load() != 0 {
		t.Fatalf("request transforms=%d legacy observations=%d", transforms.Load(), legacyObservations.Load())
	}
}

func TestRuntimeCommittedHooksAreBoundedVisibleAndDoNotRewriteResult(t *testing.T) {
	hookErr := errors.New("notification unavailable")
	var runtime *Runtime
	var order []string
	hooks := []CommittedRunHook{
		{
			Name: "audit",
			Handle: func(_ context.Context, snapshot RunSnapshot) error {
				order = append(order, "audit")
				persisted, err := runtime.Handle(snapshot.RunID).Snapshot(context.Background())
				if err != nil {
					return err
				}
				if persisted.State != RuntimeFinalizing || persisted.Result.Output != "done" {
					t.Errorf("hook observed %#v", persisted)
				}
				snapshot.Result.Output = "mutated"
				return hookErr
			},
		},
		{
			Name:    "bounded",
			Timeout: 10 * time.Millisecond,
			Handle: func(ctx context.Context, snapshot RunSnapshot) error {
				order = append(order, "bounded")
				if snapshot.Result.Output != "done" {
					t.Errorf("hook snapshots alias: %#v", snapshot.Result)
				}
				<-ctx.Done()
				return ctx.Err()
			},
		},
		{
			Name: "panic",
			Handle: func(context.Context, RunSnapshot) error {
				order = append(order, "panic")
				panic("boom")
			},
		},
	}
	var err error
	runtime, err = NewEphemeralRuntime(hooks...)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("done")}})); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil || result.Output != "done" || result.Status != RunCompleted {
		t.Fatalf("execution result = %#v, %v", result, err)
	}
	snapshot, err := runtime.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != RuntimeTerminal || len(snapshot.HookResults) != 3 {
		t.Fatalf("hook snapshot = %#v", snapshot)
	}
	if snapshot.HookResults[0].Error != hookErr.Error() {
		t.Fatalf("first hook result = %#v", snapshot.HookResults[0])
	}
	if snapshot.HookResults[1].Error != context.DeadlineExceeded.Error() || snapshot.HookResults[2].Error != "panic: boom" {
		t.Fatalf("hook results = %#v", snapshot.HookResults)
	}
	if want := []string{"audit", "bounded", "panic"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("hook order = %v, want %v", order, want)
	}
}

func TestRuntimeRejectsInvalidCommittedHooks(t *testing.T) {
	for _, hooks := range [][]CommittedRunHook{
		{{Handle: func(context.Context, RunSnapshot) error { return nil }}},
		{{Name: "missing"}},
		{{Name: "negative", Timeout: -time.Second, Handle: func(context.Context, RunSnapshot) error { return nil }}},
		{{Name: "duplicate", Handle: func(context.Context, RunSnapshot) error { return nil }}, {Name: "duplicate", Handle: func(context.Context, RunSnapshot) error { return nil }}},
	} {
		if _, err := NewEphemeralRuntime(hooks...); err == nil {
			t.Fatalf("accepted hooks %#v", hooks)
		}
	}
}

func TestRuntimeRecoverUsesPinnedBindingAndConservativeAttention(t *testing.T) {
	store := &memoryStore{buckets: make(map[string]map[string][]byte)}
	first, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	agent := testAgent(&scriptedProvider{turns: []Message{asstText("recovered")}})
	if err := first.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := first.submit(context.Background(), "agent", "v1", "work", SubmitOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	first.cancel()
	first.mu.Lock()
	first.closed = true
	first.mu.Unlock()

	reopened, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
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
	result, err := reopened.Handle(handle.ID()).Await(context.Background())
	if err != nil || result.Output != "recovered" || result.RunID != handle.ID() {
		t.Fatalf("recovered result = %#v, %v", result, err)
	}

	missing, err := NewRuntime(context.Background(), RuntimeConfig{Store: noCloseStore{Store: store}})
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Close()
	ready := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: "missing-binding", DefinitionID: "agent",
		DefinitionRevision: "v2", Task: "work", State: RuntimeReady,
		Generation: 1, Result: RunResult{RunID: "missing-binding"},
	}
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		return putRuntimeRun(tx, ready)
	}); err != nil {
		t.Fatal(err)
	}
	if err := missing.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := missing.Handle("missing-binding").Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeNeedsAttention || snapshot.AttentionReason == "" {
		t.Fatalf("missing binding snapshot = %#v, %v", snapshot, err)
	}
	if err := missing.Register("agent", "v2", testAgent(&scriptedProvider{turns: []Message{asstText("late binding")}})); err != nil {
		t.Fatal(err)
	}
	if err := missing.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err = missing.Handle("missing-binding").Await(context.Background())
	if err != nil || result.Output != "late binding" {
		t.Fatalf("late-bound result = %#v, %v", result, err)
	}
	finalizing := storedRuntimeRun{
		Version: runtimeEncodingVersion, RunID: "hook-uncertain", DefinitionID: "agent",
		DefinitionRevision: "v2", Task: "work", State: RuntimeFinalizing,
		Generation: 1, Result: RunResult{RunID: "hook-uncertain", Status: RunCompleted, Output: "done"},
	}
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		return putRuntimeRun(tx, finalizing)
	}); err != nil {
		t.Fatal(err)
	}
	if err := missing.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err = missing.Handle("hook-uncertain").Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeNeedsAttention || snapshot.Result.Output != "done" {
		t.Fatalf("uncertain hook recovery = %#v, %v", snapshot, err)
	}
}

func TestRuntimeRecoverDoesNotReclassifyLiveRun(t *testing.T) {
	provider := &barrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-provider.started
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil || snapshot.State != RuntimeRunning {
		t.Fatalf("live run after recovery = %#v, %v", snapshot, err)
	}
	close(provider.release)
	if _, err := handle.Await(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePersistsAcceptedProviderTurnBeforeToolDispatch(t *testing.T) {
	base := &memoryStore{buckets: make(map[string]map[string][]byte)}
	store := &failWritableTransactionStore{Store: base, failAt: 4}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	tool := &countingTool{}
	agent := testAgent(&scriptedProvider{turns: []Message{asstTool("c1", "counter", `{}`)}})
	agent.RegisterTool(tool)
	if err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), "agent", "v1", "work", SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(context.Background())
	if !errors.Is(err, ErrRunNeedsAttention) {
		t.Fatalf("transition failure = %#v, %v", result, err)
	}
	if tool.calls.Load() != 0 {
		t.Fatalf("tool dispatched %d times before durable provider transition", tool.calls.Load())
	}
	if len(result.Messages) < 2 || len(result.Messages[len(result.Messages)-1].ToolUses()) != 1 {
		t.Fatalf("accepted provider evidence was not retained: %#v", result.Messages)
	}
}

func TestRuntimeRestoresDeadlineErrorClassification(t *testing.T) {
	runtime := newTestRuntime(t)
	if err := runtime.Register("agent", "v1", testAgent(&cancelRuntimeProvider{started: make(chan struct{})})); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{Deadline: time.Now().Add(20 * time.Millisecond)})
	if !errors.Is(err, context.DeadlineExceeded) || result.Status != RunCancelled {
		t.Fatalf("deadline result = %#v, %v", result, err)
	}
}

func TestRuntimeDefinitionRegistrationIsImmutable(t *testing.T) {
	runtime := newTestRuntime(t)
	provider := &capturingProvider{turns: []Message{asstText("a")}}
	a := testAgent(provider)
	a.systemPrompt = "original"
	b := testAgent(&scriptedProvider{turns: []Message{asstText("b")}})
	if err := runtime.Register("agent", "v1", a); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("agent", "v1", a); err != nil {
		t.Fatalf("idempotent registration = %v", err)
	}
	if err := runtime.Register("agent", "v1", b); !errors.Is(err, ErrDefinitionConflict) {
		t.Fatalf("replacement registration = %v", err)
	}
	a.systemPrompt = "mutated after registration"
	if _, err := runtime.Run(context.Background(), "agent", "v1", "work", SubmitOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := provider.received[0][0].Text(); got != "original" {
		t.Fatalf("registered definition drifted to %q", got)
	}
}

func TestRuntimeCloseYieldsInterruptedWorkerToAttention(t *testing.T) {
	provider := &barrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	store := newEphemeralStore()
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
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
	<-provider.started
	closed := make(chan error, 1)
	go func() { closed <- runtime.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned while provider was active: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(provider.release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if handle.ID() == "" {
		t.Fatal("admitted run lost its identity")
	}
}

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	runtime, err := NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

type barrierRuntimeProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *barrierRuntimeProvider) Invoke(context.Context, Request) (Response, error) {
	close(p.started)
	<-p.release
	return Response{Message: asstText("done"), StopReason: StopEndTurn}, nil
}

type cancelRuntimeProvider struct{ started chan struct{} }

func (p *cancelRuntimeProvider) Invoke(ctx context.Context, _ Request) (Response, error) {
	close(p.started)
	<-ctx.Done()
	return Response{}, ctx.Err()
}

type noCloseStore struct{ Store }

func (noCloseStore) Close() error { return nil }

type failingRuntimeStore struct{}

func (failingRuntimeStore) Transaction(context.Context, bool, func(StoreTransaction) error) error {
	return errors.New("storage unavailable")
}
func (failingRuntimeStore) Close() error { return nil }

type failWritableTransactionStore struct {
	Store
	writes atomic.Int32
	failAt int32
}

func (s *failWritableTransactionStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	if writable && s.writes.Add(1) == s.failAt {
		return errors.New("injected durable transition failure")
	}
	return s.Store.Transaction(ctx, writable, fn)
}
