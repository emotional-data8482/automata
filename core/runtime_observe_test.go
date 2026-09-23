package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// questionTestAgent registers a durable question tool that suspends the run
// until the host answers it.
func questionTestAgent(t testing.TB, provider Provider) *Agent {
	t.Helper()
	question := Func("ask_user", "ask the user", func(context.Context, struct {
		Prompt string `json:"prompt"`
	}) (string, error) {
		return "must not execute", nil
	})
	question = WithDurableWait(question, DurableWaitPolicy{Kind: WaitQuestion})
	agent, err := New(provider, AgentConfig{Tools: []Tool{question}})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// submitWaitingQuestion admits a run that suspends on a question and returns
// once its worker has left.
func submitWaitingQuestion(t *testing.T, runtime *Runtime, turns ...Message) (*RunHandle, WaitSnapshot) {
	t.Helper()
	provider := &scriptedProvider{turns: append([]Message{asstTool("q-1", "ask_user", `{"prompt":"region?"}`)}, turns...)}
	if _, err := runtime.Register("asker", "v1", questionTestAgent(t, provider)); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), DefinitionRef{ID: "asker", Revision: "v1"}, "help")
	if err != nil {
		t.Fatal(err)
	}
	wait := waitForRuntimeWait(t, handle)
	waitForWorkerExit(t, runtime, handle.ID())
	return handle, wait
}

func waitForWorkerExit(t *testing.T, runtime *Runtime, runID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.mu.Lock()
		_, live := runtime.live[runID]
		runtime.mu.Unlock()
		if !live {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("worker did not exit")
}

func allEvents(t *testing.T, handle *RunHandle) []CommittedEvent {
	t.Helper()
	page, err := handle.Events(context.Background(), 0, maxEventPageLimit)
	if err != nil {
		t.Fatal(err)
	}
	if page.Next != page.Head {
		t.Fatalf("one page did not reach the head: next=%d head=%d", page.Next, page.Head)
	}
	return page.Events
}

// Committed events replay the whole lifecycle: contiguous sequences, the
// admission and every state change, invocation progress in model order, and
// message events that reassemble exactly the committed transcript.
func TestRuntimeCommittedEventsReplayRunLifecycle(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(toolUse("c-1", "echo", `{"text":"a"}`), toolUse("c-2", "echo", `{"text":"b"}`)),
		asstText("done"),
	}}
	echo := Func("echo", "echo", func(_ context.Context, in struct {
		Text string `json:"text"`
	}) (string, error) {
		return in.Text, nil
	})
	agent, err := New(provider, AgentConfig{Tools: []Tool{WithToolEffectPolicy(echo, ToolEffectPolicy{Kind: ToolEffectReadOnly})}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t)
	if _, err := runtime.Register("agent", "v1", agent); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), DefinitionRef{ID: "agent", Revision: "v1"}, "go")
	if err != nil {
		t.Fatal(err)
	}
	handle := runtime.Handle(result.RunID)
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events := allEvents(t, handle)
	if len(events) == 0 || snapshot.EventSequence != uint64(len(events)) {
		t.Fatalf("snapshot event sequence = %d with %d events", snapshot.EventSequence, len(events))
	}
	var transcript []Message
	var states []RuntimeState
	var invocations []string
	for i, event := range events {
		if event.Sequence != uint64(i+1) || event.RunID != result.RunID || event.Time.IsZero() {
			t.Fatalf("event %d = %#v, want contiguous sequence %d of run %s", i, event, i+1, result.RunID)
		}
		switch event.Kind {
		case CommittedRunState:
			if len(states) > 0 && event.PreviousState != states[len(states)-1] {
				t.Fatalf("state event %d previous = %q, want %q", i, event.PreviousState, states[len(states)-1])
			}
			states = append(states, event.State)
		case CommittedMessages:
			if event.MessageIndex != len(transcript) || len(event.Messages) != event.MessageCount {
				t.Fatalf("messages event %d covers [%d,+%d) with %d messages; transcript has %d", i, event.MessageIndex, event.MessageCount, len(event.Messages), len(transcript))
			}
			transcript = append(transcript, event.Messages...)
		case CommittedInvocation:
			invocations = append(invocations, event.Tool+":"+string(event.InvocationState))
		}
	}
	if !reflect.DeepEqual(transcript, snapshot.Result.Messages) {
		t.Fatalf("replayed transcript = %#v\nwant %#v", transcript, snapshot.Result.Messages)
	}
	if states[0] != RuntimeReady || events[0].PreviousState != "" || states[len(states)-1] != RuntimeTerminal {
		t.Fatalf("states = %v, want ready ... terminal", states)
	}
	if last := events[len(events)-1]; last.Kind != CommittedRunState || last.State != RuntimeTerminal {
		t.Fatalf("last event = %#v, want the terminal state", last)
	}
	want := []string{"echo:reserved", "echo:reserved", "echo:dispatched", "echo:completed", "echo:dispatched", "echo:completed"}
	if len(invocations) < 4 || invocations[0] != want[0] || invocations[len(invocations)-1] != "echo:completed" {
		t.Fatalf("invocation events = %v", invocations)
	}
	completed := 0
	for _, invocation := range invocations {
		if invocation == "echo:completed" {
			completed++
		}
	}
	if completed != 2 {
		t.Fatalf("invocation events = %v, want two completions", invocations)
	}
}

// Pages are bounded by the requested limit, resume exactly from Next, and a
// cursor outside the retained sequence is an explicit gap, never a silent
// skip.
func TestRuntimeEventPagesAreBoundedAndResumable(t *testing.T) {
	runtime := newTestRuntime(t)
	if _, err := runtime.Register("agent", "v1", testAgent(&scriptedProvider{turns: []Message{asstText("done")}})); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), DefinitionRef{ID: "agent", Revision: "v1"}, "go")
	if err != nil {
		t.Fatal(err)
	}
	handle := runtime.Handle(result.RunID)
	all := allEvents(t, handle)
	if len(all) < 4 {
		t.Fatalf("events = %d, want at least admission, running, messages, terminal", len(all))
	}
	var paged []CommittedEvent
	cursor := uint64(0)
	for pages := 0; ; pages++ {
		page, err := handle.Events(context.Background(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) > 2 || page.Head != uint64(len(all)) {
			t.Fatalf("page = %d events, head %d", len(page.Events), page.Head)
		}
		if len(page.Events) == 0 {
			if page.Next != cursor {
				t.Fatalf("empty page moved the cursor from %d to %d", cursor, page.Next)
			}
			break
		}
		paged = append(paged, page.Events...)
		cursor = page.Next
		if pages > len(all) {
			t.Fatal("paging did not terminate")
		}
	}
	if !reflect.DeepEqual(paged, all) {
		t.Fatalf("paged events differ from one read:\n%#v\n%#v", paged, all)
	}
	if _, err := handle.Events(context.Background(), uint64(len(all))+1, 10); !errors.Is(err, ErrEventGap) {
		t.Fatalf("cursor ahead of head = %v, want ErrEventGap", err)
	}
	if _, err := runtime.Handle("missing").Events(context.Background(), 0, 10); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("unknown run = %v, want ErrRunNotFound", err)
	}
}

// A consumer that waits for events holds no reads while nothing commits and
// wakes on the commit that resolves the wait.
func TestRuntimeWaitEventsWakesOnCommittedTransition(t *testing.T) {
	runtime := newTestRuntime(t)
	handle, wait := submitWaitingQuestion(t, runtime, asstText("answered"))
	snapshot, err := handle.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	idle, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	page, err := handle.WaitEvents(idle, snapshot.EventSequence, 10)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || len(page.Events) != 0 {
		t.Fatalf("idle wait = %d events, %v; want deadline with no events", len(page.Events), err)
	}
	got := make(chan EventPage, 1)
	go func() {
		page, err := handle.WaitEvents(context.Background(), snapshot.EventSequence, 10)
		if err != nil {
			t.Error(err)
		}
		got <- page
	}()
	if err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Answer: json.RawMessage(`"eu"`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case page := <-got:
		if len(page.Events) == 0 || page.Events[0].Sequence != snapshot.EventSequence+1 {
			t.Fatalf("woken page = %#v", page)
		}
		found := false
		for _, event := range page.Events {
			if event.Kind == CommittedWait && event.WaitID == wait.ID && event.WaitState == WaitResolved {
				found = true
			}
		}
		if !found {
			t.Fatalf("woken page lacks the wait resolution: %#v", page.Events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitEvents did not wake on the resolution commit")
	}
	if _, err := handle.Await(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type countingTransactionsStore struct {
	Store
	reads, writes atomic.Int64
}

func (s *countingTransactionsStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	if writable {
		s.writes.Add(1)
	} else {
		s.reads.Add(1)
	}
	return s.Store.Transaction(ctx, writable, fn)
}

// An idle Await reads the compact record once and then waits for commits; it
// no longer re-reads the full run (transcript included) every 10ms.
func TestRuntimeIdleAwaitDoesNotPollStorage(t *testing.T) {
	store := &countingTransactionsStore{Store: NewMemoryStore()}
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	handle, wait := submitWaitingQuestion(t, runtime, asstText("answered"))
	before := store.reads.Load()
	idle, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	_, err = handle.Await(idle)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle await = %v", err)
	}
	if reads := store.reads.Load() - before; reads > 1 {
		t.Fatalf("idle await issued %d reads in 150ms, want 1", reads)
	}
	done := make(chan error, 1)
	go func() {
		_, err := handle.Await(context.Background())
		done <- err
	}()
	if err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Answer: json.RawMessage(`"eu"`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not wake when the run completed")
	}
}

// Canceling or closing while a view waits on a suspended run (which has no
// worker to publish a terminal item) still wakes the view promptly.
func TestRuntimeViewsWakeWhenSuspendedRunIsCanceledOrClosed(t *testing.T) {
	runtime := newTestRuntime(t)
	handle, _ := submitWaitingQuestion(t, runtime)
	awaited := make(chan error, 1)
	observed := make(chan error, 1)
	go func() {
		_, err := handle.Await(context.Background())
		awaited <- err
	}()
	go func() { observed <- handle.Observe(context.Background(), nil) }()
	time.Sleep(10 * time.Millisecond)
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]chan error{"Await": awaited, "Observe": observed} {
		select {
		case err := <-ch:
			if name == "Await" && !errors.Is(err, context.Canceled) {
				t.Fatalf("Await after cancel = %v", err)
			}
			if name == "Observe" && err != nil {
				t.Fatalf("Observe after cancel = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not wake on cancellation", name)
		}
	}

	closing, err := NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	waiting, _ := submitWaitingQuestion(t, closing)
	go func() {
		_, err := waiting.Await(context.Background())
		awaited <- err
	}()
	time.Sleep(10 * time.Millisecond)
	if err := closing.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-awaited:
		if err == nil {
			t.Fatal("Await on a closed runtime returned no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await did not wake on Close")
	}
}

type streamingDeltasProvider struct{ deltas int }

func (p *streamingDeltasProvider) Invoke(context.Context, Request) (Response, error) {
	return Response{}, errors.New("stream only")
}

func (p *streamingDeltasProvider) InvokeStream(ctx context.Context, _ Request) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		ch <- StreamChunk{Deltas: []BlockDelta{{Index: 0, Type: "text"}}}
		for range p.deltas {
			select {
			case ch <- StreamChunk{Deltas: []BlockDelta{{Index: 0, Text: "x"}}}:
			case <-ctx.Done():
				return
			}
		}
		ch <- StreamChunk{StopReason: StopEndTurn}
	}()
	return ch, nil
}

// A stalled live observer neither blocks durable commits nor buffers without
// bound: provisional deltas beyond its bounded queue are dropped, the run
// completes, and committed events still replay everything that committed.
func TestRuntimeStalledObserverDoesNotBlockCommits(t *testing.T) {
	runtime := newTestRuntime(t)
	if _, err := runtime.Register("agent", "v1", testAgent(&streamingDeltasProvider{deltas: 2000})); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.submit(context.Background(), DefinitionRef{ID: "agent", Revision: "v1"}, "go", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	stall := make(chan struct{})
	defer close(stall)
	var delivered atomic.Int64
	observing := make(chan error, 1)
	go func() {
		observing <- handle.Observe(context.Background(), func(StreamEvent) {
			delivered.Add(1)
			<-stall
		})
	}()
	time.Sleep(10 * time.Millisecond)
	runtime.start(handle.ID())
	awaitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := handle.Await(awaitCtx)
	if err != nil || len(result.Output) != 2000 {
		t.Fatalf("run behind a stalled observer = %d bytes, %v", len(result.Output), err)
	}
	runtime.mu.Lock()
	for _, ch := range runtime.subscribers[handle.ID()] {
		if cap(ch) > 64 {
			t.Fatalf("observer queue capacity = %d, want bounded", cap(ch))
		}
	}
	runtime.mu.Unlock()
	if got := delivered.Load(); got != 1 {
		t.Fatalf("stalled observer received %d callbacks, want 1", got)
	}
	events := allEvents(t, runtime.Handle(handle.ID()))
	if last := events[len(events)-1]; last.State != RuntimeTerminal {
		t.Fatalf("last committed event = %#v", last)
	}
}

// A streaming view whose run suspends and is then canceled returns promptly.
// Before committed wakeups it waited for a terminal item that only a worker
// publishes, and a canceled suspended run has no worker.
func TestRuntimeRunStreamReturnsWhenSuspendedRunIsCanceled(t *testing.T) {
	runtime := newTestRuntime(t)
	provider := &scriptedProvider{turns: []Message{asstTool("q-1", "ask_user", `{"prompt":"region?"}`)}}
	if _, err := runtime.Register("asker", "v1", questionTestAgent(t, provider)); err != nil {
		t.Fatal(err)
	}
	streamed := make(chan error, 1)
	options := WithIdempotencyKey("tenant", "stream-cancel")
	go func() {
		_, err := runtime.RunStream(context.Background(), DefinitionRef{ID: "asker", Revision: "v1"}, "help", nil, options)
		streamed <- err
	}()
	var handle *RunHandle
	deadline := time.Now().Add(2 * time.Second)
	for handle == nil && time.Now().Before(deadline) {
		if h, err := runtime.Submit(context.Background(), DefinitionRef{ID: "asker", Revision: "v1"}, "help", options); err == nil {
			handle = h
		}
	}
	waitForRuntimeWait(t, handle)
	waitForWorkerExit(t, runtime, handle.ID())
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-streamed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunStream after cancel = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunStream did not return after its suspended run was canceled")
	}
}

// A stale scheduling attempt on a suspended run is benign. It used to fail
// the claim with "invalid state waiting" and surface as a worker failure to
// every view of a run that was only waiting for an answer.
func TestRuntimeStaleStartOfWaitingRunIsBenign(t *testing.T) {
	runtime := newTestRuntime(t)
	handle, wait := submitWaitingQuestion(t, runtime, asstText("answered"))
	runtime.start(handle.ID())
	waitForWorkerExit(t, runtime, handle.ID())
	if err := runtime.failure(handle.ID()); err != nil {
		t.Fatalf("stale start recorded a failure: %v", err)
	}
	if err := handle.ResolveWait(context.Background(), wait.ID, WaitResolution{Answer: json.RawMessage(`"eu"`)}); err != nil {
		t.Fatal(err)
	}
	if result, err := handle.Await(context.Background()); err != nil || result.Output != "answered" {
		t.Fatalf("result after stale start = %#v, %v", result, err)
	}
}
