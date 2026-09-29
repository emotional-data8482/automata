package core

import (
	"context"
	"sync"
)

// ToolProgress is a provisional progress report from an executing tool.
type ToolProgress struct {
	// Progress should increase as work advances, in units the tool chooses.
	Progress float64
	// Total is the value Progress reaches on completion, or zero if unknown.
	Total float64
	// Message optionally describes the current step.
	Message string
}

// ReportToolProgress publishes progress for the tool call executing under ctx
// as a [StreamToolProgress] event on the run's live views, including ancestor
// views of a child run. It is safe for concurrent use and never waits for an
// observer. Outside a Runtime tool execution, or once that tool's Execute has
// returned, it does nothing.
//
// Progress is provisional: it is never persisted, and a bounded view may drop
// it. Report at a rate a person could follow, not per byte of work.
func ReportToolProgress(ctx context.Context, progress ToolProgress) {
	if reporter, ok := ctx.Value(toolProgressKey{}).(*toolProgressReporter); ok {
		reporter.report(progress)
	}
}

type toolProgressKey struct{}

// toolProgressReporter publishes one call's progress until the call returns.
// Holding mu across emit orders every published report before close, and so
// before the call's result event.
type toolProgressReporter struct {
	mu   sync.Mutex
	emit func(StreamEvent)
	call ToolUseBlock // ID and Name only
	done bool
}

func (r *toolProgressReporter) report(progress ToolProgress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.done {
		r.emit(StreamEvent{Kind: StreamToolProgress, ToolCall: r.call, Progress: &progress})
	}
}

func (r *toolProgressReporter) close() {
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
}
