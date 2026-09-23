package core

import (
	"context"
	"encoding/json"
)

// Request is a single provider invocation: the conversation so far, the tools
// available this turn, and the per-call [CallOptions]. Providers ignore any
// option they cannot honor (documented per provider) rather than erroring, so a
// caller can set ThinkingBudget unconditionally and let providers without
// extended thinking skip it.
type Request struct {
	Messages []Message
	Tools    []ToolDefinition
	Options  CallOptions
}

// CallOptions are the per-call knobs applied to a provider invocation. The zero
// value means "provider defaults for everything". A definition sets them with
// [AgentConfig].CallOptions.
type CallOptions struct {
	// Temperature, when non-nil, sets the sampling temperature.
	Temperature *float64
	// MaxTokens caps the response length; 0 means the provider default.
	MaxTokens int
	// StopSequences are strings that halt generation when produced.
	StopSequences []string
	// ToolChoice constrains which tool (if any) the model must call.
	ToolChoice *ToolChoice
	// ThinkingBudget, when > 0, enables extended thinking with this token
	// budget on providers that support it.
	ThinkingBudget int
	// OutputSchema, when non-nil, asks providers with native structured-output
	// support to enforce this JSON schema on the response (see
	// [StructuredOutputProvider] and [StructuredOutputConfig].Native). Providers
	// without support ignore it, like any other option they cannot honor.
	OutputSchema json.RawMessage
}

// ToolChoiceMode selects how the model may use tools on a turn.
type ToolChoiceMode int

const (
	// ToolChoiceAuto lets the model decide whether to call a tool (the default).
	ToolChoiceAuto ToolChoiceMode = iota
	// ToolChoiceNone forbids tool calls this turn.
	ToolChoiceNone
	// ToolChoiceAny requires the model to call some tool.
	ToolChoiceAny
	// ToolChoiceTool requires the model to call the specific tool named in
	// ToolChoice.Name.
	ToolChoiceTool
)

// ToolChoice constrains tool use for a turn. Name is required only for
// [ToolChoiceTool].
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
}

// Response is a single provider turn: the assistant message, a provider-neutral
// stop reason, and the provider's original reason for diagnostics.
//
// Providers should always set StopReason. RawStopReason should be copied
// verbatim from the provider (for example, OpenAI's "length" or Anthropic's
// "max_tokens"). The loop tolerates an empty StopReason for compatibility with
// older custom providers, inferring end-turn versus tool-use from Message, but
// a nonempty unrecognized reason is treated as [StopUnknown], never success.
type Response struct {
	Message       Message
	StopReason    StopReason
	RawStopReason string

	// completionErr is set by core's streaming adapter when a response ended
	// after partial data because of cancellation or a transport failure. It is
	// folded into the CompletionError returned by the loop.
	completionErr error
}

// Provider performs a non-streaming provider invocation.
type Provider interface {
	Invoke(ctx context.Context, req Request) (Response, error)
}

// StreamProvider adds a streaming invocation that emits [StreamChunk]s.
type StreamProvider interface {
	Provider
	InvokeStream(ctx context.Context, req Request) (<-chan StreamChunk, error)
}

// StructuredOutputProvider is an optional interface a Provider implements to
// advertise native, schema-enforced structured output. When a definition's
// [StructuredOutputConfig] sets Native and its provider implements this
// interface and reports support, runs send the declared schema via
// [CallOptions.OutputSchema] instead of injecting the hidden
// structured-output tool, and the provider's response text is parsed
// and validated like any other structured payload. Supported native invalid
// payloads correct within the run's structured-output budget. Providers only
// implementing [Provider], or reporting no support, use the hidden-tool path.
//
// Support is advertised per provider, not per model: if a specific model
// rejects schema enforcement, the provider surfaces that as an invocation
// error.
type StructuredOutputProvider interface {
	Provider

	// SupportsNativeStructuredOutput reports whether the provider honors
	// [CallOptions.OutputSchema] by enforcing the schema on its responses.
	SupportsNativeStructuredOutput() bool
}
