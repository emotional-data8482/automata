package core

import (
	"context"
	"encoding/json"
	"strings"
)

type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

// Merge folds another Usage into u taking the field-wise maximum. Providers
// report cumulative usage as a stream progresses, so within a single turn the
// latest (largest) value of each counter is the truth. To total usage across
// turns or agents, use [Usage.Add] instead — Merge would under-count.
func (u *Usage) Merge(other *Usage) {
	if u == nil || other == nil {
		return
	}
	if other.InputTokens > u.InputTokens {
		u.InputTokens = other.InputTokens
	}
	if other.OutputTokens > u.OutputTokens {
		u.OutputTokens = other.OutputTokens
	}
	if other.CacheCreationTokens > u.CacheCreationTokens {
		u.CacheCreationTokens = other.CacheCreationTokens
	}
	if other.CacheReadTokens > u.CacheReadTokens {
		u.CacheReadTokens = other.CacheReadTokens
	}
}

// Add sums another Usage into u field-wise. Use it to total per-turn usage
// (e.g. from [StreamUsage] events) across turns or agents; within one turn's
// cumulative stream chunks, use [Usage.Merge].
func (u *Usage) Add(other *Usage) {
	if u == nil || other == nil {
		return
	}
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheCreationTokens += other.CacheCreationTokens
	u.CacheReadTokens += other.CacheReadTokens
}

// Message is one turn in a conversation: a role and its content as a list of
// typed [Block]s. The block model is a superset of what any single provider
// exposes, so nothing is lost in translation — thinking blocks (with their
// signatures), tool calls, tool results (with an error flag), and images all
// survive round-trips through a transcript and back to the provider.
//
// Tool calls are not a separate field: they are [ToolUseBlock]s in Blocks, read
// via [Message.ToolUses]. Blocks is the single source of truth.
type Message struct {
	Role   string `json:"role"` // "system" | "user" | "assistant" | "tool"
	Blocks Blocks `json:"blocks,omitempty"`
	Usage  *Usage `json:"usage,omitempty"`
}

// Text returns the concatenated text of all [TextBlock]s in the message.
func (m Message) Text() string {
	var b strings.Builder
	for _, blk := range m.Blocks {
		if t, ok := blk.(TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// ToolUses returns the [ToolUseBlock]s in the message, in block order. This is
// how the run loop and an [Approver] read the tool calls a model requested.
func (m Message) ToolUses() []ToolUseBlock {
	var out []ToolUseBlock
	for _, blk := range m.Blocks {
		if t, ok := blk.(ToolUseBlock); ok {
			out = append(out, t)
		}
	}
	return out
}

// Thinking returns the concatenated text of all [ThinkingBlock]s in the message.
func (m Message) Thinking() string {
	var b strings.Builder
	for _, blk := range m.Blocks {
		if t, ok := blk.(ThinkingBlock); ok {
			b.WriteString(t.Thinking)
		}
	}
	return b.String()
}

// UserMessage builds a user turn from plain text.
func UserMessage(content string) Message {
	return Message{Role: "user", Blocks: Blocks{TextBlock{Text: content}}}
}

// SystemMessage builds a system turn from plain text.
func SystemMessage(content string) Message {
	return Message{Role: "system", Blocks: Blocks{TextBlock{Text: content}}}
}

// AssistantMessage builds an assistant turn from the given blocks (text,
// thinking, tool_use, …).
func AssistantMessage(blocks ...Block) Message {
	return Message{Role: "assistant", Blocks: blocks}
}

// ToolResultMessage builds a tool-result turn answering the call toolUseID.
// content is the string fed back to the model; isError marks the tool as
// failed, which providers with a native error flag map onto.
func ToolResultMessage(toolUseID, content string, isError bool) Message {
	return Message{
		Role: "tool",
		Blocks: Blocks{ToolResultBlock{
			ToolUseID: toolUseID,
			Content:   Blocks{TextBlock{Text: content}},
			IsError:   isError,
		}},
	}
}

type Tool interface {
	Name() string
	Schema() json.RawMessage
	Execute(ctx context.Context, args string) (string, error)
}

// StreamChunk is the provider-facing wire fragment of a streaming response: a
// batch of block deltas plus optional finish reason and usage. A
// [StreamProvider] emits these; [consumeStream] assembles them into a [Message].
type StreamChunk struct {
	Deltas       []BlockDelta
	FinishReason string
	Usage        *Usage
	Err          error
}

// BlockDelta is an incremental update to one content block, addressed by its
// provider-assigned Index. The first delta for a block carries its Type (and,
// for tool_use, its ID/Name); later deltas carry only the growing content
// (Text for text/thinking blocks, PartialJSON for tool_use input, Signature for
// a thinking block's trailing signature).
type BlockDelta struct {
	Index int    // provider content-block index
	Type  string // "text" | "thinking" | "tool_use"; set on the block's first delta

	// tool_use start fields
	ID   string
	Name string

	// continuation fields
	Text        string // text or thinking content delta
	PartialJSON string // tool_use input fragment
	Signature   string // thinking signature (arrives at block end on Anthropic)
}

// StreamEventKind discriminates the StreamEvent variants delivered to a
// RunStream callback. Switch on it to decide which fields are populated.
type StreamEventKind int

const (
	// StreamText carries an assistant content delta in Text.
	StreamText StreamEventKind = iota
	// StreamThinking carries a model reasoning delta in Text. It fires only for
	// providers with extended thinking enabled.
	StreamThinking
	// StreamToolCall reports a tool call the model requested. ToolCall holds
	// the assembled call (name + input) before it executes.
	StreamToolCall
	// StreamToolResult reports a finished tool call. ToolCall identifies the
	// call; Result holds the string fed back to the model, IsError marks a tool
	// failure, and Err is non-nil if the tool returned an error.
	StreamToolResult
	// StreamUsage reports the token usage for a completed provider turn. Usage
	// holds the assembled per-turn counts; it fires once per turn, after that
	// turn's text deltas and before its tool calls.
	StreamUsage
)

// StreamEvent is a single item in the event stream delivered to the RunStream
// callback. Unlike StreamChunk (the provider-facing wire fragment), a
// StreamEvent is a fully assembled, run-level observation: a content or
// thinking delta, a requested tool call, a tool result, or usage.
type StreamEvent struct {
	Kind StreamEventKind
	// Agent names the sub-agent the event originated from, set to the sub-agent
	// tool's name when an [AsTool] sub-agent streams into a parent run. It is
	// empty for events from the top-level agent being streamed. Nested
	// sub-agents keep the innermost tag (a wrapper only stamps when empty).
	Agent string
	// InvocationID identifies the specific sub-agent invocation the event came
	// from: the ID of the tool call that started it. Two parallel calls to the
	// same sub-agent tool share an Agent name but have distinct InvocationIDs,
	// so (Agent, InvocationID) is what keeps their event streams separable. It
	// is empty for top-level events, and — like Agent — keeps the innermost
	// value on nested sub-agents.
	InvocationID string
	Text         string       // StreamText / StreamThinking: the content delta
	ToolCall     ToolUseBlock // StreamToolCall / StreamToolResult: the call
	Result       string       // StreamToolResult: the string returned to the model
	IsError      bool         // StreamToolResult: true if the tool failed
	Usage        *Usage       // StreamUsage: the completed turn's token usage
	Err          error        // StreamToolResult: non-nil if the tool returned an error
}
