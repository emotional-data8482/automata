package core

import "context"

// PreSendHook fires once per turn, immediately before the provider invocation.
// It receives the messages and tools to be sent and returns (possibly modified)
// versions. Hooks transform a snapshot of the canonical message history into
// a rendered view sent to the provider — they do NOT mutate the canonical
// history maintained by the run loop.
//
// Returning a non-nil error aborts the run.
type PreSendHook func(
	ctx context.Context,
	messages []Message,
	tools []Tool,
) ([]Message, []Tool, error)
