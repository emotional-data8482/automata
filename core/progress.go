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

// ReportToolProgress sends progress to the receiver ctx carries (see
// [WithToolProgress]) and returns once the receiver has taken it. It is safe
// for concurrent use. Without a receiver, or once the receiver is stopped, it
// does nothing.
//
// Under a Runtime, the receiver publishes a [StreamToolProgress] event on the
// run's live views, including ancestor views of a child run, and never waits
// for a view. Such progress is provisional: it is never persisted, and a
// bounded view may drop it. Report at a rate a person could follow, not per
// byte of work.
func ReportToolProgress(ctx context.Context, progress ToolProgress) {
	if receiver, ok := ctx.Value(toolProgressKey{}).(*toolProgressReceiver); ok {
		receiver.report(progress)
	}
}

// WithToolProgress returns a copy of ctx whose [ReportToolProgress] calls go
// to receive, and a stop function. Calls to receive are serialized, so
// receive must not report on the context it was installed on. After stop
// returns, receive is running nowhere and is never called again; stop is
// idempotent. A nil receive discards reports.
//
// The receiver replaces any receiver ctx already carries. To pass reports on,
// call ReportToolProgress on the outer context from receive.
//
// Runtime installs a receiver around every tool execution and stops it before
// publishing the call's result. Install one yourself to execute a tool
// elsewhere, such as serving it over another protocol or observing its
// progress in a test, and stop it before reporting the tool's result.
func WithToolProgress(ctx context.Context, receive func(ToolProgress)) (context.Context, func()) {
	receiver := &toolProgressReceiver{receive: receive}
	return context.WithValue(ctx, toolProgressKey{}, receiver), receiver.stop
}

type toolProgressKey struct{}

// toolProgressReceiver delivers one execution's progress until stopped.
// Holding mu across receive orders every delivered report before stop.
type toolProgressReceiver struct {
	mu      sync.Mutex
	receive func(ToolProgress)
	stopped bool
}

func (r *toolProgressReceiver) report(progress ToolProgress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.stopped && r.receive != nil {
		r.receive(progress)
	}
}

func (r *toolProgressReceiver) stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
}
