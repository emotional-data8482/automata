package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// conversationProvider records every request transcript and replays one
// scripted turn per call; a scripted nil Message returns an error instead.
type conversationProvider struct {
	mu       sync.Mutex
	turns    []Message
	requests [][]Message
}

func (p *conversationProvider) Invoke(_ context.Context, request Request) (Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, cloneMessages(request.Messages))
	if len(p.requests) > len(p.turns) {
		return Response{}, errors.New("no script for turn")
	}
	turn := p.turns[len(p.requests)-1]
	if turn.Role == "" {
		return Response{}, errors.New("scripted provider failure")
	}
	return fixtureResponse(turn), nil
}

func (p *conversationProvider) request(i int) []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[i]
}

func conversationRuntime(t *testing.T, provider Provider) *Runtime {
	t.Helper()
	agent, err := New(provider, AgentConfig{SystemPrompt: "be brief"})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t)
	if err := runtime.Register("chat", "v1", agent); err != nil {
		t.Fatal(err)
	}
	return runtime
}

func turnOptions(expectedHead string) SubmitOptions {
	return SubmitOptions{Conversation: ConversationOptions{Scope: "tenant", ID: "c1", ExpectedHead: expectedHead}}
}

// The next turn is seeded from the head's canonical committed history with
// the new task appended exactly once; provider-native blocks survive, usage
// stays local, and the head advances in the terminal commit.
func TestRuntimeConversationContinuesCommittedHistory(t *testing.T) {
	native := RawBlock{Provider: "fake", Type: "citation", Data: json.RawMessage(`{"n":1}`)}
	provider := &conversationProvider{turns: []Message{
		withUsage(AssistantMessage(TextBlock{Text: "hi"}, native), &Usage{InputTokens: 10}),
		withUsage(asstText("again done"), &Usage{InputTokens: 20}),
	}}
	runtime := conversationRuntime(t, provider)
	ctx := context.Background()
	first, err := runtime.Run(ctx, "chat", "v1", "hello", turnOptions(""))
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.Run(ctx, "chat", "v1", "again", turnOptions(first.RunID))
	if err != nil {
		t.Fatal(err)
	}
	if second.Output != "again done" || second.RunID == first.RunID {
		t.Fatalf("second turn = %#v", second)
	}
	want := append(cloneMessages(first.Messages), UserMessage("again"))
	if got := provider.request(1); !reflect.DeepEqual(got, want) {
		t.Fatalf("second request = %#v\nwant %#v", got, want)
	}
	if len(second.Messages) != len(first.Messages)+2 {
		t.Fatalf("second transcript has %d messages, want %d", len(second.Messages), len(first.Messages)+2)
	}
	tasks := 0
	for _, message := range second.Messages {
		if message.Role == "user" && message.Text() == "again" {
			tasks++
		}
	}
	if tasks != 1 {
		t.Fatalf("task appended %d times", tasks)
	}
	if got := second.Messages[2].Blocks[1]; !reflect.DeepEqual(got, native) {
		t.Fatalf("native block = %#v, want %#v", got, native)
	}
	if second.Usage != (Usage{InputTokens: 20}) {
		t.Fatalf("second turn usage = %#v, want local only", second.Usage)
	}
	conversation, err := runtime.Conversation(ctx, "tenant", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Head != second.RunID || conversation.ActiveRunID != "" || conversation.Turns != 2 ||
		conversation.DefinitionID != "chat" || conversation.DefinitionRevision != "v1" {
		t.Fatalf("conversation = %#v", conversation)
	}
	for _, runID := range []string{first.RunID, second.RunID} {
		snapshot, err := runtime.Handle(runID).Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Definition != (DefinitionRef{ID: "chat", Revision: "v1"}) ||
			snapshot.Conversation == nil || *snapshot.Conversation != (ConversationRef{Scope: "tenant", ID: "c1"}) ||
			snapshot.Parent != nil || snapshot.Failure != nil || snapshot.Attention != nil {
			t.Fatalf("run %s snapshot groups = %#v", runID, snapshot)
		}
	}
}

// Stale heads, competing turns, and a different definition are rejected; a
// rejected admission changes nothing.
func TestRuntimeConversationRejectsStaleCompetingAndMismatchedTurns(t *testing.T) {
	provider := &countingBarrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime := newTestRuntime(t)
	t.Cleanup(provider.unblock)
	if err := runtime.Register("chat", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("chat", "v2", testAgent(&countingRuntimeProvider{})); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := runtime.Submit(ctx, "chat", "v1", "hello", turnOptions("missing")); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("unknown head = %v, want conflict", err)
	}
	if _, err := runtime.Submit(ctx, "chat", "v1", "hello", SubmitOptions{Conversation: ConversationOptions{Scope: "tenant"}}); err == nil {
		t.Fatal("conversation scope without id was accepted")
	}
	first, err := runtime.Submit(ctx, "chat", "v1", "hello", turnOptions(""))
	if err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, provider.started, "first turn")
	if _, err := runtime.Submit(ctx, "chat", "v1", "competing", turnOptions("")); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("competing turn = %v, want busy", err)
	}
	provider.unblock()
	if _, err := first.Await(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Submit(ctx, "chat", "v1", "stale", turnOptions("")); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("stale head = %v, want conflict", err)
	}
	if _, err := runtime.Submit(ctx, "chat", "v2", "other definition", turnOptions(first.ID())); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("different definition = %v, want conflict", err)
	}
	conversation, err := runtime.Conversation(ctx, "tenant", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Head != first.ID() || conversation.ActiveRunID != "" || conversation.Turns != 1 {
		t.Fatalf("conversation = %#v", conversation)
	}
	if _, err := runtime.Conversation(ctx, "tenant", "missing"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("missing conversation = %v", err)
	}
}

// An exact retry of a turn whose acknowledgement was lost resolves the
// original run from its receipt even after that turn moved the head.
func TestRuntimeConversationExactRetryResolvesBeforeHeadCheck(t *testing.T) {
	runtime := conversationRuntime(t, &repeatingChildProvider{})
	ctx := context.Background()
	first, err := runtime.Run(ctx, "chat", "v1", "hello", turnOptions(""))
	if err != nil {
		t.Fatal(err)
	}
	options := turnOptions(first.RunID)
	options.Scope, options.Key = "client", "turn-2"
	second, err := runtime.Run(ctx, "chat", "v1", "again", options)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := runtime.Submit(ctx, "chat", "v1", "again", options)
	if err != nil || retried.ID() != second.RunID {
		t.Fatalf("exact retry = %v, %v; want run %s", retried, err, second.RunID)
	}
	if _, err := runtime.Submit(ctx, "chat", "v1", "changed", options); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("changed payload = %v, want admission conflict", err)
	}
	if _, err := runtime.Submit(ctx, "chat", "v1", "again", turnOptions(first.RunID)); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("unkeyed stale turn = %v, want conflict", err)
	}
	if got := storedRunCount(t, runtime.store); got != 2 {
		t.Fatalf("stored runs = %d, want 2", got)
	}
}

// Concurrent admissions for the same head reserve exactly one active run.
func TestRuntimeConversationConcurrentAdmissionReservesOneRun(t *testing.T) {
	provider := &countingBarrierRuntimeProvider{started: make(chan struct{}), release: make(chan struct{})}
	runtime := newTestRuntime(t)
	t.Cleanup(provider.unblock)
	if err := runtime.Register("chat", "v1", testAgent(provider)); err != nil {
		t.Fatal(err)
	}
	const competitors = 8
	var admitted atomic.Int32
	errs := make(chan error, competitors)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for range competitors {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if _, err := runtime.Submit(context.Background(), "chat", "v1", "hello", turnOptions("")); err != nil {
				errs <- err
				return
			}
			admitted.Add(1)
		}()
	}
	start.Done()
	done.Wait()
	close(errs)
	if got := admitted.Load(); got != 1 {
		t.Fatalf("admitted turns = %d, want 1", got)
	}
	for err := range errs {
		if !errors.Is(err, ErrConversationBusy) {
			t.Fatalf("competing admission = %v, want busy", err)
		}
	}
}

// A failed turn with structurally valid partial history may continue; a turn
// canceled with an unanswered tool call blocks the conversation instead of
// inventing a result for it.
func TestRuntimeConversationContinuesFailedHistoryButBlocksIncomplete(t *testing.T) {
	provider := &conversationProvider{turns: []Message{{}, asstText("recovered")}}
	runtime := conversationRuntime(t, provider)
	ctx := context.Background()
	failed, err := runtime.Run(ctx, "chat", "v1", "hello", turnOptions(""))
	if err == nil || failed.Status != RunFailed {
		t.Fatalf("first turn = %s, %v; want failure", failed.Status, err)
	}
	next, err := runtime.Run(ctx, "chat", "v1", "try again", turnOptions(failed.RunID))
	if err != nil || next.Output != "recovered" {
		t.Fatalf("continuation after failure = %q, %v", next.Output, err)
	}
	if got := provider.request(1); len(got) != 3 || got[1].Text() != "hello" || got[2].Text() != "try again" {
		t.Fatalf("continuation request = %#v", got)
	}

	asker := testAgent(&scriptedProvider{turns: []Message{asstTool("q1", "ask", `{}`)}})
	asker.RegisterTool(WithDurableWait(Func("ask", "ask a human", func(context.Context, struct{}) (string, error) {
		return "unused", nil
	}), DurableWaitPolicy{Kind: WaitQuestion}))
	if err := runtime.Register("asker", "v1", asker); err != nil {
		t.Fatal(err)
	}
	options := SubmitOptions{Conversation: ConversationOptions{ID: "questions"}}
	waiting, err := runtime.Submit(ctx, "asker", "v1", "hello", options)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunState(t, runtime, waiting.ID(), RuntimeWaiting)
	if err := waiting.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	options.Conversation.ExpectedHead = waiting.ID()
	if _, err := runtime.Submit(ctx, "asker", "v1", "continue", options); !errors.Is(err, ErrConversationBlocked) {
		t.Fatalf("continuation of incomplete history = %v, want blocked", err)
	}
	conversation, err := runtime.Conversation(ctx, "", "questions")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Head != waiting.ID() || conversation.ActiveRunID != "" {
		t.Fatalf("conversation = %#v", conversation)
	}
}

// A turn whose owner stopped after its result committed but before any hook
// was delivered is finished by recovery, which advances the head and frees
// the conversation for the next turn.
func TestRuntimeConversationRecoveryReleasesFinalizedTurn(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	// Fault the terminal hook-outcome commit of a plain turn.
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: &writeFaultStore{Store: noCloseStore{Store: base}, match: enteringState(RuntimeTerminal)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("chat", "v1", testAgent(&repeatingChildProvider{})); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := runtime.Run(ctx, "chat", "v1", "hello", turnOptions(""))
	if err == nil {
		t.Fatal("injected terminal-commit fault was invisible")
	}
	_ = runtime.Close()

	reopened, err := NewRuntime(ctx, RuntimeConfig{Store: noCloseStore{Store: base}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Register("chat", "v1", testAgent(&repeatingChildProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	conversation, err := reopened.Conversation(ctx, "tenant", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Head != first.RunID || conversation.ActiveRunID != "" {
		t.Fatalf("conversation = %#v, want head %s released", conversation, first.RunID)
	}
	next, err := reopened.Run(ctx, "chat", "v1", "again", turnOptions(first.RunID))
	if err != nil || next.Output != "child done" {
		t.Fatalf("next turn = %q, %v", next.Output, err)
	}
}

// A turn left in hook attention keeps the conversation busy until the host
// acknowledges the interrupted delivery; the acknowledgement advances the
// head with the turn's completed result and never re-invokes the hook.
func TestRuntimeConversationAcknowledgedHooksReleaseTurn(t *testing.T) {
	base := NewMemoryStore().(*memoryStore)
	var deliveries atomic.Int32
	hooks := []CommittedRunHook{{Name: "audit", Handle: func(context.Context, RunSnapshot) error {
		deliveries.Add(1)
		return nil
	}}}
	// The terminal commit records the delivered hook outcome; faulting it
	// leaves the delivery interrupted after the marker.
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: &writeFaultStore{Store: noCloseStore{Store: base}, match: enteringState(RuntimeTerminal)}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Register("chat", "v1", testAgent(&repeatingChildProvider{})); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := runtime.Run(ctx, "chat", "v1", "hello", turnOptions(""))
	if err == nil {
		t.Fatal("injected hook-outcome fault was invisible")
	}
	_ = runtime.Close()

	reopened, err := NewRuntime(ctx, RuntimeConfig{Store: noCloseStore{Store: base}, Hooks: hooks})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Register("chat", "v1", testAgent(&repeatingChildProvider{})); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Submit(ctx, "chat", "v1", "again", turnOptions(first.RunID)); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("turn during hook attention = %v, want busy", err)
	}
	if err := reopened.Handle(first.RunID).AcknowledgeHooks(ctx); err != nil {
		t.Fatal(err)
	}
	acknowledged, err := reopened.Handle(first.RunID).Await(ctx)
	if err != nil || acknowledged.Status != RunCompleted || acknowledged.Output != "child done" {
		t.Fatalf("acknowledged turn = %s %q, %v", acknowledged.Status, acknowledged.Output, err)
	}
	next, err := reopened.Run(ctx, "chat", "v1", "again", turnOptions(first.RunID))
	if err != nil || next.Output != "child done" {
		t.Fatalf("next turn = %q, %v", next.Output, err)
	}
	// One delivery before the interruption, one for the new turn; none repeated.
	if got := deliveries.Load(); got != 2 {
		t.Fatalf("hook deliveries = %d, want 2", got)
	}
}

// fixedAnswerProvider answers every turn with the same large text.
type fixedAnswerProvider struct{ answer string }

func (p fixedAnswerProvider) Invoke(context.Context, Request) (Response, error) {
	return fixtureResponse(asstText(p.answer)), nil
}

// Conversation turns reference the head's committed transcript instead of
// copying it: each turn stores only its own messages, so storage grows
// linearly with the conversation rather than quadratically, while every turn
// still reads the whole conversation and its events cover only what it added.
func TestRuntimeConversationTurnsReferenceHistoryWithoutCopying(t *testing.T) {
	runtime := newTestRuntime(t)
	answer := strings.Repeat("a", 2048)
	if err := runtime.Register("chat", "v1", testAgent(fixedAnswerProvider{answer: answer})); err != nil {
		t.Fatal(err)
	}
	const turns = 8
	head := ""
	var ownBytes []int
	var last RunResult
	for turn := range turns {
		result, err := runtime.Run(context.Background(), "chat", "v1", fmt.Sprintf("question %d", turn), turnOptions(head))
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Messages) != 2*(turn+1) || result.Messages[len(result.Messages)-1].Text() != answer {
			t.Fatalf("turn %d transcript has %d messages, want the whole conversation", turn, len(result.Messages))
		}
		size := 0
		if err := runtime.transaction(context.Background(), false, func(tx StoreTransaction) error {
			return tx.Scan(runtimeFactsBucket, result.RunID+"/", func(_ string, raw []byte) error {
				size += len(raw)
				return nil
			})
		}); err != nil {
			t.Fatal(err)
		}
		ownBytes = append(ownBytes, size)
		head, last = result.RunID, result
	}
	if ownBytes[turns-1] > ownBytes[0]+64 {
		t.Fatalf("per-turn transcript bytes grew with history: %v", ownBytes)
	}
	var covered int
	for _, event := range allEvents(t, runtime.Handle(last.RunID)) {
		if event.Kind != CommittedMessages {
			continue
		}
		if event.MessageIndex < 2*(turns-1) {
			t.Fatalf("last turn's messages event starts at %d, inside the referenced history", event.MessageIndex)
		}
		covered += event.MessageCount
	}
	if covered != 2 {
		t.Fatalf("last turn's events cover %d messages, want its own task and answer", covered)
	}
}
