package claude

import (
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/emotional-data/automata/core"
)

// core's buildSchema (core/tools.go) emits this exact shape.
type coreToolSchema struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	} `json:"parameters"`
}

func convertTools(tools []core.Tool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		var s coreToolSchema
		if err := json.Unmarshal(t.Schema(), &s); err != nil {
			// Schema came from core's own marshaller; a parse failure means the
			// tool is malformed. Fall back to name + empty schema so the request
			// doesn't 400 on a missing name.
			s.Name = t.Name()
		}
		if s.Name == "" {
			s.Name = t.Name()
		}
		tp := anthropic.ToolParam{
			Name: s.Name,
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: s.Parameters.Properties,
				Required:   s.Parameters.Required,
			},
		}
		if s.Description != "" {
			tp.Description = anthropic.String(s.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return out
}

// convertMessages translates core's flat OpenAI-shaped messages into
// Anthropic's content-block model:
//   - role:"system" messages are extracted into the System parameter.
//   - role:"tool" messages are coalesced into a single user message of
//     tool_result blocks (Anthropic requires alternating user/assistant turns).
//   - role:"assistant" messages with tool_calls become an assistant message
//     containing optional text plus one tool_use block per call.
func convertMessages(msgs []core.Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam, error) {
	var system []anthropic.TextBlockParam
	var out []anthropic.MessageParam
	var pending []anthropic.ContentBlockParamUnion

	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, anthropic.NewUserMessage(pending...))
		pending = nil
	}

	for i, m := range msgs {
		switch m.Role {
		case "system":
			if m.Content == nil {
				continue
			}
			system = append(system, anthropic.TextBlockParam{Text: *m.Content})

		case "user":
			flush()
			if m.Content == nil {
				return nil, nil, fmt.Errorf("user message at index %d has nil content", i)
			}
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(*m.Content)))

		case "assistant":
			flush()
			var blocks []anthropic.ContentBlockParamUnion
			if m.Content != nil && *m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(*m.Content))
			}
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				if args == "" {
					args = "{}"
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: json.RawMessage(args),
					},
				})
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewAssistantMessage(blocks...))
			}

		case "tool":
			content := ""
			if m.Content != nil {
				content = *m.Content
			}
			pending = append(pending, anthropic.NewToolResultBlock(m.ToolCallID, content, false))

		default:
			return nil, nil, fmt.Errorf("unsupported message role %q at index %d", m.Role, i)
		}
	}
	flush()

	return system, out, nil
}

// convertResponse translates an Anthropic Message back into core's flat
// assistant message. Text blocks are concatenated; tool_use blocks become
// core.ToolCall entries with Arguments set to the raw JSON of the input.
// Content stays nil when the model returned only tool calls — core's run loop
// uses that to distinguish "tools to run" from "empty response" in
// core/loop.go.
func convertResponse(resp *anthropic.Message) core.Message {
	var text string
	var toolCalls []core.ToolCall

	for _, block := range resp.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			text += v.Text
		case anthropic.ToolUseBlock:
			args := v.JSON.Input.Raw()
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, core.ToolCall{
				ID:   v.ID,
				Type: "function",
				Function: core.FunctionCall{
					Name:      v.Name,
					Arguments: args,
				},
			})
		}
	}

	msg := core.Message{
		Role:      "assistant",
		ToolCalls: toolCalls,
	}
	if text != "" {
		msg.Content = &text
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 ||
		resp.Usage.CacheCreationInputTokens > 0 || resp.Usage.CacheReadInputTokens > 0 {
		msg.Usage = &core.Usage{
			InputTokens:         int(resp.Usage.InputTokens),
			OutputTokens:        int(resp.Usage.OutputTokens),
			CacheCreationTokens: int(resp.Usage.CacheCreationInputTokens),
			CacheReadTokens:     int(resp.Usage.CacheReadInputTokens),
		}
	}
	return msg
}
