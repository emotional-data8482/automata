package core

import "testing"

func TestStreamAccumulatorFoldsEvents(t *testing.T) {
	call := ToolUseBlock{ID: "c1", Name: "search", Input: []byte(`{"q":"x"}`)}

	var acc StreamAccumulator
	for _, ev := range []StreamEvent{
		{Kind: StreamText, Text: "Hello"},
		{Kind: StreamUsage, Usage: &Usage{InputTokens: 10, OutputTokens: 5}},
		{Kind: StreamToolCall, ToolCall: call},
		{Kind: StreamText, Agent: "sub", Text: "working"},
		{Kind: StreamUsage, Agent: "sub", Usage: &Usage{InputTokens: 3, OutputTokens: 2}},
		{Kind: StreamToolResult, ToolCall: call, Result: "ok"},
		{Kind: StreamText, Text: " world"},
	} {
		acc.Add(ev)
	}

	views := acc.Views()
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2: %+v", len(views), views)
	}

	top := views[0]
	if top.Agent != "" || top.Text != "Hello world" {
		t.Errorf("top view = %+v, want Agent \"\" Text \"Hello world\"", top)
	}
	if len(top.ToolCalls) != 1 {
		t.Fatalf("top has %d tool calls, want 1", len(top.ToolCalls))
	}
	if tc := top.ToolCalls[0]; !tc.Done || tc.Result != "ok" || tc.Err != nil || !toolCallEqual(tc.Call, call) {
		t.Errorf("tool call view = %+v, want paired Done result %q", tc, "ok")
	}
	if top.Usage != (Usage{InputTokens: 10, OutputTokens: 5}) {
		t.Errorf("top usage = %+v", top.Usage)
	}

	sub := views[1]
	if sub.Agent != "sub" || sub.Text != "working" || sub.Usage != (Usage{InputTokens: 3, OutputTokens: 2}) {
		t.Errorf("sub view = %+v", sub)
	}

	if totals := acc.Totals(); totals != (Usage{InputTokens: 13, OutputTokens: 7}) {
		t.Errorf("totals = %+v, want {13 7}", totals)
	}
}

// TestStreamAccumulatorSumsUsage pins that per-turn usage is summed across
// turns — not folded with Usage.Merge's max semantics, which would report 10/5
// here instead of 17/8.
func TestStreamAccumulatorSumsUsage(t *testing.T) {
	var acc StreamAccumulator
	acc.Add(StreamEvent{Kind: StreamUsage, Usage: &Usage{InputTokens: 10, OutputTokens: 5}})
	acc.Add(StreamEvent{Kind: StreamUsage, Usage: &Usage{InputTokens: 7, OutputTokens: 3}})

	if totals := acc.Totals(); totals != (Usage{InputTokens: 17, OutputTokens: 8}) {
		t.Errorf("totals = %+v, want {17 8}", totals)
	}
}

// TestStreamAccumulatorUnmatchedResult covers a consumer that attached mid-run:
// a StreamToolResult with no announced call is recorded, not dropped.
func TestStreamAccumulatorUnmatchedResult(t *testing.T) {
	call := ToolUseBlock{ID: "c9", Name: "late", Input: []byte("{}")}

	var acc StreamAccumulator
	acc.Add(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: "late-ok"})

	views := acc.Views()
	if len(views) != 1 || len(views[0].ToolCalls) != 1 {
		t.Fatalf("views = %+v, want one view with one tool call", views)
	}
	if tc := views[0].ToolCalls[0]; !tc.Done || tc.Result != "late-ok" {
		t.Errorf("tool call view = %+v, want Done with result %q", tc, "late-ok")
	}
}

// TestStreamAccumulatorSnapshotIsolation pins that Views returns copies: later
// Add calls must not mutate a snapshot the caller already holds.
func TestStreamAccumulatorSnapshotIsolation(t *testing.T) {
	call := ToolUseBlock{ID: "c1", Name: "echo", Input: []byte("{}")}

	var acc StreamAccumulator
	acc.Add(StreamEvent{Kind: StreamText, Text: "before"})
	acc.Add(StreamEvent{Kind: StreamToolCall, ToolCall: call})

	snapshot := acc.Views()

	acc.Add(StreamEvent{Kind: StreamText, Text: " after"})
	acc.Add(StreamEvent{Kind: StreamToolResult, ToolCall: call, Result: "ok"})

	if snapshot[0].Text != "before" {
		t.Errorf("snapshot text = %q, want %q", snapshot[0].Text, "before")
	}
	if snapshot[0].ToolCalls[0].Done {
		t.Error("snapshot tool call became Done after a later Add")
	}

	if current, _ := acc.View("", ""); current.Text != "before after" || !current.ToolCalls[0].Done {
		t.Errorf("current view = %+v, want updated text and Done call", current)
	}
}

// TestStreamAccumulatorTopLevelFirst pins Views ordering when a sub-agent
// produces events before the top-level agent does.
func TestStreamAccumulatorTopLevelFirst(t *testing.T) {
	var acc StreamAccumulator
	acc.Add(StreamEvent{Kind: StreamText, Agent: "sub", Text: "first"})
	acc.Add(StreamEvent{Kind: StreamText, Text: "second"})

	views := acc.Views()
	if len(views) != 2 || views[0].Agent != "" || views[1].Agent != "sub" {
		t.Errorf("view order = %+v, want top-level first", views)
	}
}

func TestStreamAccumulatorViewMissing(t *testing.T) {
	var acc StreamAccumulator
	if _, ok := acc.View("ghost", ""); ok {
		t.Error("View(ghost) reported ok for an unseen agent")
	}
}

// TestStreamAccumulatorSeparatesParallelInvocations pins the InvocationID fix:
// two concurrent calls to the same sub-agent tool share an Agent name but must
// land in separate lanes keyed by InvocationID, not interleave into one view.
func TestStreamAccumulatorSeparatesParallelInvocations(t *testing.T) {
	var acc StreamAccumulator
	// Two "researcher" invocations, r1 and r2, interleaved as they would be if
	// the orchestrator called the tool twice in one turn.
	acc.Add(StreamEvent{Kind: StreamText, Agent: "researcher", InvocationID: "r1", Text: "alpha "})
	acc.Add(StreamEvent{Kind: StreamText, Agent: "researcher", InvocationID: "r2", Text: "beta "})
	acc.Add(StreamEvent{Kind: StreamText, Agent: "researcher", InvocationID: "r1", Text: "one"})
	acc.Add(StreamEvent{Kind: StreamText, Agent: "researcher", InvocationID: "r2", Text: "two"})
	acc.Add(StreamEvent{Kind: StreamUsage, Agent: "researcher", InvocationID: "r1", Usage: &Usage{InputTokens: 5}})
	acc.Add(StreamEvent{Kind: StreamUsage, Agent: "researcher", InvocationID: "r2", Usage: &Usage{InputTokens: 9}})

	v1, ok := acc.View("researcher", "r1")
	if !ok || v1.Text != "alpha one" || v1.Usage.InputTokens != 5 {
		t.Errorf("r1 view = %+v, want text %q usage 5", v1, "alpha one")
	}
	v2, ok := acc.View("researcher", "r2")
	if !ok || v2.Text != "beta two" || v2.Usage.InputTokens != 9 {
		t.Errorf("r2 view = %+v, want text %q usage 9", v2, "beta two")
	}

	// ViewsFor groups both invocations under the shared name, in first-seen order.
	lanes := acc.ViewsFor("researcher")
	if len(lanes) != 2 || lanes[0].InvocationID != "r1" || lanes[1].InvocationID != "r2" {
		t.Errorf("ViewsFor(researcher) = %+v, want r1 then r2", lanes)
	}
	// Totals still sum across both.
	if totals := acc.Totals(); totals.InputTokens != 14 {
		t.Errorf("totals.InputTokens = %d, want 14", totals.InputTokens)
	}
}
