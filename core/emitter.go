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

// toolCallIDKey is the context key under which a streaming run stashes the ID
// of the tool call it is about to execute, so a sub-agent tool (see [AsTool])
// can stamp that ID as the [StreamEvent.InvocationID] on the events it
// forwards. Two parallel calls to the same sub-agent tool share a name but not
// an ID, so the ID is what keeps their event streams distinguishable.
type toolCallIDKey struct{}

// withToolCallID returns a child context carrying id as the active tool-call
// ID. [Loop.executeTool] installs it (alongside the emitter) before running a
// tool in a streaming run.
func withToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

// toolCallIDFrom returns the tool-call ID installed by the enclosing streaming
// run, or "" if none is present.
func toolCallIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(toolCallIDKey{}).(string)
	return id
}
