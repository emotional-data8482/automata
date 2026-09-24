package openrouter

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	orsdk "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"

	"github.com/emotional-data8482/automata/core"
)

// convertTools maps core tool definitions to OpenRouter function tools. core's
// schema shape is JSON Schema, which OpenRouter forwards as-is in the
// function's parameters.
func convertTools(tools []core.ToolDefinition) ([]components.ChatFunctionTool, error) {
	out := make([]components.ChatFunctionTool, 0, len(tools))
	for _, t := range tools {
		var params map[string]any
		if len(t.InputSchema) > 0 {
			if err := json.Unmarshal(t.InputSchema, &params); err != nil {
				return nil, fmt.Errorf("tool %q parameters: %w", t.Name, err)
			}
		}
		fn := components.ChatFunctionToolFunctionFunction{Name: t.Name, Parameters: params}
		if t.Description != "" {
			fn.Description = orsdk.Pointer(t.Description)
		}
		out = append(out, components.CreateChatFunctionToolChatFunctionToolFunction(components.ChatFunctionToolFunction{
			Type:     components.ChatFunctionToolTypeFunction,
			Function: fn,
		}))
	}
	return out, nil
}

// toolResultText flattens a tool result's blocks into the text a Chat
// Completions tool message can carry. Text blocks concatenate in order;
// non-text blocks become a documented placeholder line so a rich result never
// reaches the model as an empty string. OpenRouter has no native tool-result
// image content, so degradation is by placeholder rather than a conversion
// error.
func toolResultText(tr core.ToolResultBlock) string {
	var b strings.Builder
	for _, blk := range tr.Content {
		switch t := blk.(type) {
		case core.TextBlock:
			b.WriteString(t.Text)
		case core.ImageBlock:
			kind := t.MediaType
			if kind == "" {
				kind = "image"
			}
			b.WriteString("[non-text tool result block: ")
			b.WriteString(kind)
			b.WriteString("]")
		default:
			b.WriteString("[non-text tool result block omitted]")
		}
	}
	return b.String()
}

// convertMessages maps core block messages to OpenRouter wire messages.
// Thinking blocks are dropped on send (Chat Completions-style input has no
// thinking channel, and OpenRouter cannot preserve thinking signatures across
// its normalization); raw blocks are dropped too — core keeps them for the
// provider that produced them. Tool results map to role:"tool" messages with
// an "error:" content prefix, since there is no native error flag.
func convertMessages(msgs []core.Message) ([]components.ChatMessages, error) {
	out := make([]components.ChatMessages, 0, len(msgs))
	for i, m := range msgs {
		switch m.Role {
		case "system":
			out = append(out, components.CreateChatMessagesSystem(components.ChatSystemMessage{
				Content: components.CreateChatSystemMessageContentStr(m.Text()),
			}))

		case "user":
			content, err := userContent(m.Blocks)
			if err != nil {
				return nil, fmt.Errorf("user message at index %d: %w", i, err)
			}
			out = append(out, components.CreateChatMessagesUser(components.ChatUserMessage{
				Content: content,
			}))

		case "assistant":
			am := components.ChatAssistantMessage{}
			if text := m.Text(); text != "" {
				am.Content = optionalnullable.From(orsdk.Pointer(components.CreateChatAssistantMessageContentStr(text)))
			}
			for _, tu := range m.ToolUses() {
				args := string(tu.Input)
				if args == "" {
					args = "{}"
				}
				am.ToolCalls = append(am.ToolCalls, components.ChatToolCall{
					ID:   tu.ID,
					Type: components.ChatToolCallTypeFunction,
					Function: components.ChatToolCallFunction{
						Name:      tu.Name,
						Arguments: args,
					},
				})
			}
			out = append(out, components.CreateChatMessagesAssistant(am))

		case "tool":
			for _, blk := range m.Blocks {
				tr, ok := blk.(core.ToolResultBlock)
				if !ok {
					return nil, fmt.Errorf("tool message at index %d has non-tool_result block %T", i, blk)
				}
				content := toolResultText(tr)
				if tr.IsError {
					content = "error: " + content
				}
				out = append(out, components.CreateChatMessagesTool(components.ChatToolMessage{
					Content:    components.CreateChatToolMessageContentStr(content),
					ToolCallID: tr.ToolUseID,
				}))
			}

		default:
			return nil, fmt.Errorf("unsupported message role %q at index %d", m.Role, i)
		}
	}
	return out, nil
}

// userContent returns a plain string for text-only messages, or an array of
// content parts when images are present (data: URLs for inline bytes).
func userContent(blocks core.Blocks) (components.ChatUserMessageContent, error) {
	hasImage := false
	for _, blk := range blocks {
		if _, ok := blk.(core.ImageBlock); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return components.CreateChatUserMessageContentStr(core.Message{Blocks: blocks}.Text()), nil
	}

	parts := make([]components.ChatContentItems, 0, len(blocks))
	for _, blk := range blocks {
		switch b := blk.(type) {
		case core.TextBlock:
			parts = append(parts, components.CreateChatContentItemsText(components.ChatContentText{Text: b.Text}))
		case core.ImageBlock:
			parts = append(parts, components.CreateChatContentItemsImageURL(components.ChatContentImage{
				ImageURL: components.ChatContentImageImageURL{URL: imageDataURL(b)},
			}))
		default:
			return components.ChatUserMessageContent{}, fmt.Errorf("unsupported user block %T", blk)
		}
	}
	return components.CreateChatUserMessageContentArrayOfChatContentItems(parts), nil
}

// imageDataURL renders a core ImageBlock as an image_url value: a data: URL
// for inline bytes, or the URL as-is.
func imageDataURL(b core.ImageBlock) string {
	if len(b.Data) > 0 {
		mt := b.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(b.Data)
	}
	return b.URL
}

// convertResponse maps an OpenRouter assistant message back into a core
// assistant message: reasoning becomes a ThinkingBlock (OpenRouter normalizes
// thinking and cannot return provider signatures, so the block carries text
// only), content becomes TextBlocks, and tool calls become ToolUseBlocks.
func convertResponse(msg components.ChatAssistantMessage) core.Message {
	var blocks core.Blocks
	if r, ok := msg.Reasoning.Get(); ok && r != nil && *r != "" {
		blocks = append(blocks, core.ThinkingBlock{Thinking: *r})
	}
	if content, ok := msg.Content.Get(); ok && content != nil {
		switch {
		case content.Str != nil && *content.Str != "":
			blocks = append(blocks, core.TextBlock{Text: *content.Str})
		case content.ArrayOfChatContentItems != nil:
			for _, item := range content.ArrayOfChatContentItems {
				if item.ChatContentText != nil {
					blocks = append(blocks, core.TextBlock{Text: item.ChatContentText.Text})
				} else {
					blocks = append(blocks, core.TextBlock{Text: "[non-text assistant content part omitted]"})
				}
			}
		}
	}
	if refusal, ok := msg.Refusal.Get(); ok && refusal != nil && *refusal != "" {
		blocks = append(blocks, core.TextBlock{Text: *refusal})
	}
	for _, tc := range msg.ToolCalls {
		blocks = append(blocks, core.ToolUseBlock{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}
	return core.Message{Role: "assistant", Blocks: blocks}
}

// mapFinishReason translates OpenRouter finish_reason values into core's
// provider-neutral stop vocabulary. "error" (an upstream provider failure
// surfaced mid-generation) and unknown values are deliberately not treated as
// successful completions; Response.RawStopReason retains them.
func mapFinishReason(raw string) core.StopReason {
	switch raw {
	case "stop":
		return core.StopEndTurn
	case "tool_calls", "function_call":
		return core.StopToolUse
	case "length":
		return core.StopTokenLimit
	case "content_filter", "refusal":
		return core.StopContentFilter
	case "cancelled", "canceled":
		return core.StopCancelled
	default:
		return core.StopUnknown
	}
}

// usageToCore maps OpenRouter usage into core's accounting. Cached prompt
// tokens map to CacheReadTokens and cache writes to CacheCreationTokens;
// OpenRouter's cost fields have no core equivalent and are dropped.
func usageToCore(u *components.ChatUsage) *core.Usage {
	if u == nil {
		return nil
	}
	usage := &core.Usage{
		InputTokens:  int(u.PromptTokens),
		OutputTokens: int(u.CompletionTokens),
	}
	if d, ok := u.PromptTokensDetails.Get(); ok && d != nil {
		if d.CachedTokens != nil {
			usage.CacheReadTokens = int(*d.CachedTokens)
		}
		if d.CacheWriteTokens != nil {
			usage.CacheCreationTokens = int(*d.CacheWriteTokens)
		}
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0 {
		return nil
	}
	return usage
}
