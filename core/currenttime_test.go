package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCurrentTimeIsRunLocalAndStableAcrossToolTurns(t *testing.T) {
	base := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	var clocks atomic.Int64
	var requests []string
	turn := 0
	provider := invokeFunc(func(_ context.Context, req Request) (Response, error) {
		for _, message := range req.Messages {
			if message.Role == "user" {
				requests = append(requests, message.Text())
			}
		}
		turn++
		if turn%2 == 1 {
			return Response{Message: asstTool("call", "read", `{}`), StopReason: StopToolUse}, nil
		}
		return Response{Message: asstText("done"), StopReason: StopEndTurn}, nil
	})
	agent, err := New(provider, AgentConfig{
		Tools: []Tool{Func("read", "read", func(context.Context, struct{}) (string, error) { return "ok", nil })},
		PreSendHooks: []PreSendHook{
			func(_ context.Context, req Request) (Request, error) {
				// A transform can change the rendered user-message index without
				// changing its canonical identity (as compaction does).
				if len(req.Messages) > 1 {
					req.Messages = append([]Message{SystemMessage("rendered prefix")}, req.Messages...)
				}
				return req, nil
			},
			CurrentTime(func() time.Time { return base.Add(time.Duration(clocks.Add(1)-1) * time.Hour) }),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t)
	ref, err := runtime.Register("clock", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := runtime.Run(context.Background(), ref, "What time is it?")
		if err != nil {
			t.Fatal(err)
		}
		if result.Messages[0].Text() != "What time is it?" {
			t.Fatalf("hook changed canonical history: %+v", result.Messages)
		}
	}
	if clocks.Load() != 2 || len(requests) != 4 {
		t.Fatalf("clock calls=%d, requests=%q; want one stamp per run", clocks.Load(), requests)
	}
	for i, request := range requests {
		want := "Current time: " + base.Add(time.Duration(i/2)*time.Hour).Format(time.RFC3339)
		if !strings.HasPrefix(request, want) {
			t.Fatalf("request %d = %q; want prefix %q", i, request, want)
		}
	}
}

func TestCurrentTimeDoesNotEvictAnotherWorker(t *testing.T) {
	var calls atomic.Int64
	hook := CurrentTime(func() time.Time { return time.Unix(calls.Add(1), 0) })
	victim := context.WithValue(context.Background(), currentTimeContextKey{}, currentTimeContext{times: &currentTimeStamps{}, message: 0})
	req := Request{Messages: []Message{UserMessage("victim")}}
	first, err := hook(victim, req)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1025 {
		ctx := context.WithValue(context.Background(), currentTimeContextKey{}, currentTimeContext{times: &currentTimeStamps{}, message: 0})
		if _, err := hook(ctx, Request{Messages: []Message{UserMessage(fmt.Sprint(i))}}); err != nil {
			t.Fatal(err)
		}
	}
	later, err := hook(victim, req)
	if err != nil || later.Messages[0].Text() != first.Messages[0].Text() {
		t.Fatalf("active worker stamp changed: %q -> %q, %v", first.Messages[0].Text(), later.Messages[0].Text(), err)
	}
	// A repeated canonical user message at a new position is a new turn.
	next := context.WithValue(victim, currentTimeContextKey{}, currentTimeContext{times: victim.Value(currentTimeContextKey{}).(currentTimeContext).times, message: 2})
	newTurn, err := hook(next, req)
	if err != nil || newTurn.Messages[0].Text() == first.Messages[0].Text() {
		t.Fatalf("new user turn reused old stamp: %q, %v", newTurn.Messages[0].Text(), err)
	}
}

func TestCurrentTimeDirectCallsGetFreshStamps(t *testing.T) {
	var calls atomic.Int64
	hook := CurrentTime(func() time.Time { return time.Unix(calls.Add(1), 0) })
	req := Request{Messages: []Message{UserMessage("same task")}}
	first, err := hook(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hook(context.Background(), req)
	if err != nil || first.Messages[0].Text() == second.Messages[0].Text() {
		t.Fatalf("direct calls reused stamp: %q, %q, %v", first.Messages[0].Text(), second.Messages[0].Text(), err)
	}
}

func TestCurrentTimeRefreshesAfterRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int64
	var stamps []string
	provider := invokeFunc(func(_ context.Context, req Request) (Response, error) {
		for _, message := range req.Messages {
			if message.Role == "user" {
				stamps = append(stamps, message.Text())
			}
		}
		if len(stamps) == 1 {
			return Response{Message: asstTool("ask", "ask_user", `{}`), StopReason: StopToolUse}, nil
		}
		return Response{Message: asstText("done"), StopReason: StopEndTurn}, nil
	})
	question := WithDurableWait(Func("ask_user", "ask", func(context.Context, struct{}) (string, error) {
		return "must not execute", nil
	}), DurableWaitPolicy{Kind: WaitQuestion, Prompt: func(json.RawMessage) (string, error) { return "Which region?", nil }})
	agent, err := New(provider, AgentConfig{Tools: []Tool{question}, PreSendHooks: []PreSendHook{
		CurrentTime(func() time.Time { return time.Unix(calls.Add(1), 0) }),
	}})
	if err != nil {
		t.Fatal(err)
	}
	store := noCloseStore{Store: NewMemoryStore()}
	first, err := NewRuntime(ctx, RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ref, err := first.Register("clock", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := first.Submit(ctx, ref, "same task")
	if err != nil {
		t.Fatal(err)
	}
	wait := waitForRuntimeWait(t, handle)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewRuntime(ctx, RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Register("clock", "v1", agent); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	handle = reopened.Handle(handle.ID())
	if err := handle.ResolveWait(ctx, wait.ID, WaitResolution{Answer: json.RawMessage(`"us-east"`)}); err != nil {
		t.Fatal(err)
	}
	result, err := handle.Await(ctx)
	if err != nil || result.Output != "done" {
		t.Fatalf("recovered run = %+v, %v", result, err)
	}
	if calls.Load() != 2 || len(stamps) != 2 || stamps[0] == stamps[1] {
		t.Fatalf("recovered segment did not refresh: clock calls=%d, stamps=%q", calls.Load(), stamps)
	}
	if result.Messages[0].Text() != "same task" {
		t.Fatal("clock hint became canonical history")
	}
}
