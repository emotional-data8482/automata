package core

import (
	"context"
	"encoding/json"
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

type Message struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Usage      *Usage         `json:"usage,omitempty"`
	Meta       map[string]any `json:"-"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func UserMessage(content string) Message {
	return Message{Role: "user", Content: &content}
}

func SystemMessage(content string) Message {
	return Message{Role: "system", Content: &content}
}

func ToolResultMessage(toolCallID, content string) Message {
	return Message{Role: "tool", Content: &content, ToolCallID: toolCallID}
}

func GoalMessage(content string) Message {
	return Message{Role: "goal", Content: &content}
}

type Tool interface {
	Name() string
	Schema() json.RawMessage
	Execute(ctx context.Context, args string) (string, error)
}

type StreamChunk struct {
	ContentDelta string
	ToolCalls    []StreamToolCallFragment
	FinishReason string
	Usage        *Usage
	Err          error
}

type StreamToolCallFragment struct {
	Index     int
	ID        string
	Type      string
	Name      string
	Arguments string
}

// StreamEventKind discriminates the StreamEvent variants delivered to a
// RunStream callback. Switch on it to decide which fields are populated.
type StreamEventKind int

const (
	// StreamText carries an assistant content delta in Text.
	StreamText StreamEventKind = iota
	// StreamToolCall reports a tool call the model requested. ToolCall holds
	// the assembled call (name + arguments) before it executes.
	StreamToolCall
	// StreamToolResult reports a finished tool call. ToolCall identifies the
	// call; Result holds the string fed back to the model and Err is non-nil
	// if the tool returned an error.
	StreamToolResult
	// StreamUsage reports the token usage for a completed provider turn. Usage
	// holds the assembled per-turn counts; it fires once per turn, after that
	// turn's text deltas and before its tool calls.
	StreamUsage
)

// StreamEvent is a single item in the event stream delivered to the RunStream
// callback. Unlike StreamChunk (the provider-facing wire fragment), a
// StreamEvent is a fully assembled, run-level observation: a content delta, a
// requested tool call, or a tool result.
type StreamEvent struct {
	Kind StreamEventKind
	// Agent names the sub-agent the event originated from, set to the sub-agent
	// tool's name when an [AsTool] sub-agent streams into a parent run. It is
	// empty for events from the top-level agent being streamed. Nested
	// sub-agents keep the innermost tag (a wrapper only stamps when empty).
	Agent    string
	Text     string   // StreamText: the content delta
	ToolCall ToolCall // StreamToolCall / StreamToolResult: the call
	Result   string   // StreamToolResult: the string returned to the model
	Usage    *Usage   // StreamUsage: the completed turn's token usage
	Err      error    // StreamToolResult: non-nil if the tool returned an error
}
