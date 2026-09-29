package core

import (
	"context"
	"sync"
	"testing"
)

type progressArgs struct{}

// progressTool reports each step before returning, and keeps its context so
// a test can report after Execute has returned.
func progressTool(name string, steps []ToolProgress, kept *context.Context) Tool {
	return Func(name, "reports progress", func(ctx context.Context, _ progressArgs) (string, error) {
		for _, step := range steps {
			ReportToolProgress(ctx, step)
		}
		if kept != nil {
			*kept = ctx
		}
		return "finished", nil
	})
}

func TestRunStreamEmitsToolProgressBeforeResult(t *testing.T) {
	provider := &recordingProvider{turns: []Message{asstTool("c1", "work", `{}`), asstText("done")}}
	steps := []ToolProgress{{Progress: 1, Total: 2, Message: "half"}, {Progress: 2, Total: 2}}
	var kept context.Context
	agent := testAgent(provider).WithTools(progressTool("work", steps, &kept))

	var mu sync.Mutex
	var got []StreamEvent
	if _, err := runAgentStream(t, agent, "go", func(ev StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Kind == StreamToolCall || ev.Kind == StreamToolProgress || ev.Kind == StreamToolResult {
			got = append(got, ev)
		}
	}); err != nil {
		t.Fatal(err)
	}
	// Reports after Execute returned are dropped without effect.
	ReportToolProgress(kept, ToolProgress{Progress: 99})

	mu.Lock()
	defer mu.Unlock()
	kinds := []StreamEventKind{StreamToolCall, StreamToolProgress, StreamToolProgress, StreamToolResult}
	if len(got) != len(kinds) {
		t.Fatalf("got %d tool events, want %d: %+v", len(got), len(kinds), got)
	}
	for i, kind := range kinds {
		if got[i].Kind != kind || got[i].ToolCall.ID != "c1" || got[i].ToolCall.Name != "work" {
			t.Fatalf("event %d = %+v, want kind %d for call c1", i, got[i], kind)
		}
	}
	for i, step := range steps {
		event := got[1+i]
		if event.Progress == nil || *event.Progress != step {
			t.Fatalf("progress %d = %+v, want %+v", i, event.Progress, step)
		}
		if len(event.ToolCall.Input) != 0 {
			t.Fatalf("progress %d carries input %s", i, event.ToolCall.Input)
		}
	}
}

func TestReportToolProgressOutsideRuntimeIsNoOp(t *testing.T) {
	ReportToolProgress(context.Background(), ToolProgress{Progress: 1})
	result, err := progressTool("work", []ToolProgress{{Progress: 1}}, nil).Execute(context.Background(), nil)
	if err != nil || result.Text() != "finished" {
		t.Fatalf("direct Execute = %+v, %v", result, err)
	}
}

// TestWithToolProgressStop pins the ordering guarantee: once stop returns,
// no receive is running and none follows, even from goroutines a tool leaked.
func TestWithToolProgressStop(t *testing.T) {
	var mu sync.Mutex
	received, active := 0, 0
	ctx, stop := WithToolProgress(context.Background(), func(ToolProgress) {
		mu.Lock()
		active++
		received++
		mu.Unlock()
		mu.Lock()
		active--
		mu.Unlock()
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				ReportToolProgress(ctx, ToolProgress{Progress: 1})
			}
		})
	}
	stop()
	mu.Lock()
	atStop, running := received, active
	mu.Unlock()
	wg.Wait()
	stop() // idempotent
	mu.Lock()
	defer mu.Unlock()
	if running != 0 {
		t.Fatalf("%d receives still running after stop returned", running)
	}
	if received != atStop {
		t.Fatalf("%d reports received after stop", received-atStop)
	}
}

func TestWithToolProgressNesting(t *testing.T) {
	var outer, inner []ToolProgress
	outerCtx, stopOuter := WithToolProgress(context.Background(), func(p ToolProgress) { outer = append(outer, p) })
	defer stopOuter()

	// The innermost receiver wins; it can pass reports on explicitly.
	innerCtx, stopInner := WithToolProgress(outerCtx, func(p ToolProgress) {
		inner = append(inner, p)
		p.Message = "forwarded " + p.Message
		ReportToolProgress(outerCtx, p)
	})
	ReportToolProgress(innerCtx, ToolProgress{Progress: 1, Message: "step"})
	stopInner()
	ReportToolProgress(innerCtx, ToolProgress{Progress: 2})

	// A nil receiver discards reports instead of reaching the outer one.
	quietCtx, stopQuiet := WithToolProgress(outerCtx, nil)
	defer stopQuiet()
	ReportToolProgress(quietCtx, ToolProgress{Progress: 3})

	if len(inner) != 1 || inner[0].Message != "step" {
		t.Fatalf("inner = %+v", inner)
	}
	if len(outer) != 1 || outer[0] != (ToolProgress{Progress: 1, Message: "forwarded step"}) {
		t.Fatalf("outer = %+v", outer)
	}
}

func TestChildToolProgressReachesAncestorViews(t *testing.T) {
	runtime := newTestRuntime(t)
	leafProvider := &recordingProvider{turns: []Message{asstTool("l1", "work", `{}`), asstText("leaf done")}}
	leaf, err := runtime.Register("leaf", "v1", testAgent(leafProvider).WithTools(
		progressTool("work", []ToolProgress{{Progress: 3, Message: "indexing"}}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rootProvider := &recordingProvider{turns: []Message{asstTool("r1", "leaf", `{"topic":"x"}`), asstText("root done")}}
	root, err := runtime.Register("root", "v1", testAgent(rootProvider).WithTools(ChildTool[researchArgs]("leaf", "leaf work", leaf)))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var progress []StreamEvent
	if _, err := runtime.RunStream(context.Background(), root, "go", func(ev StreamEvent) {
		if ev.Kind == StreamToolProgress {
			mu.Lock()
			progress = append(progress, ev)
			mu.Unlock()
		}
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(progress) != 1 {
		t.Fatalf("progress events = %+v, want one", progress)
	}
	ev := progress[0]
	if ev.Agent != "leaf" || ev.InvocationID != "r1" || ev.ToolCall.ID != "l1" || ev.Progress.Message != "indexing" {
		t.Fatalf("progress event = %+v, want the leaf's call l1 tagged leaf/r1", ev)
	}
}

func TestStreamAccumulatorTracksToolProgress(t *testing.T) {
	call := ToolUseBlock{ID: "c1", Name: "work", Input: []byte(`{"n":1}`)}
	progress := func(p float64) StreamEvent {
		return StreamEvent{Kind: StreamToolProgress, ToolCall: ToolUseBlock{ID: "c1", Name: "work"}, Progress: &ToolProgress{Progress: p, Total: 4}}
	}

	var acc StreamAccumulator
	acc.Add(StreamEvent{Kind: StreamToolCall, ToolCall: call})
	acc.Add(progress(1))
	snapshot := acc.Views()
	acc.Add(progress(3))
	acc.Add(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: "ok"})

	if p := snapshot[0].ToolCalls[0].Progress; p == nil || p.Progress != 1 {
		t.Fatalf("snapshot progress = %+v, want the report it was taken at", p)
	}
	current, _ := acc.View("", "")
	if len(current.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", current.ToolCalls)
	}
	if tc := current.ToolCalls[0]; !tc.Done || tc.Result != "ok" || tc.Progress == nil || tc.Progress.Progress != 3 {
		t.Fatalf("tool call = %+v, want done with the last progress kept", tc)
	}

	// A view attached mid-call first sees progress; the result still pairs
	// with it and supplies the call's input.
	var late StreamAccumulator
	late.Add(progress(2))
	late.Add(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: "ok"})
	view, _ := late.View("", "")
	if len(view.ToolCalls) != 1 {
		t.Fatalf("late tool calls = %+v", view.ToolCalls)
	}
	if tc := view.ToolCalls[0]; !tc.Done || string(tc.Call.Input) != `{"n":1}` || tc.Progress.Progress != 2 {
		t.Fatalf("late tool call = %+v", tc)
	}
	// A progress event without a report is ignored.
	late.Add(StreamEvent{Kind: StreamToolProgress, ToolCall: call})
	if view, _ := late.View("", ""); len(view.ToolCalls) != 1 {
		t.Fatalf("nil progress added a call: %+v", view.ToolCalls)
	}
}
