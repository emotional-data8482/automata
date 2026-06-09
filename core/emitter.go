package core

import "context"

// emitterKey is the context key under which a streaming run stashes its event
// sink so that sub-agent tools (see [AsTool]) can forward their own stream
// events into the parent run.
type emitterKey struct{}

// withEmitter returns a child context carrying emit as the active stream sink.
// A [Loop] driving a RunStream installs its sink here before executing tools
// (see Loop.executeTool); a plain Run never installs one.
func withEmitter(ctx context.Context, emit func(StreamEvent)) context.Context {
	return context.WithValue(ctx, emitterKey{}, emit)
}

// emitterFrom returns the active stream sink installed by an enclosing
// streaming run, or nil if the run is not streaming. A nil result tells a
// sub-agent tool to fall back to non-streaming [Agent.Run].
func emitterFrom(ctx context.Context) func(StreamEvent) {
	emit, _ := ctx.Value(emitterKey{}).(func(StreamEvent))
	return emit
}
