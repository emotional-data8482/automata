package core

import "testing"

func TestStreamAccumulatorFoldsEvents(t *testing.T) {
	call := ToolCall{ID: "c1", Type: "function", Function: FunctionCall{Name: "search", Arguments: `{"q":"x"}`}}

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
	if tc := top.ToolCalls[0]; !tc.Done || tc.Result != "ok" || tc.Err != nil || tc.Call != call {
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
	call := ToolCall{ID: "c9", Type: "function", Function: FunctionCall{Name: "late", Arguments: "{}"}}

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
	call := ToolCall{ID: "c1", Type: "function", Function: FunctionCall{Name: "echo", Arguments: "{}"}}

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

	if current, _ := acc.View(""); current.Text != "before after" || !current.ToolCalls[0].Done {
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
	if _, ok := acc.View("ghost"); ok {
		t.Error("View(ghost) reported ok for an unseen agent")
	}
}
