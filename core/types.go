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
