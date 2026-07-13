package claude

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

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

// redactedThinkingData is the shape core stores in a RawBlock for Anthropic's
// redacted_thinking block, so it round-trips without core modeling it.
type redactedThinkingData struct {
	Data string `json:"data"`
}

// convertMessages translates core's block-based messages into Anthropic's
// content-block model:
//   - role:"system" messages are extracted into the System parameter.
//   - role:"tool" messages are coalesced into a single user message of
//     tool_result blocks (Anthropic requires alternating user/assistant turns).
//   - role:"assistant" messages carry text, thinking (with signature),
//     redacted_thinking (from a RawBlock), and tool_use blocks.
//   - role:"user" messages carry text and image blocks.
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
			if text := m.Text(); text != "" {
				system = append(system, anthropic.TextBlockParam{Text: text})
			}

		case "user":
			flush()
			blocks, err := userBlocks(m.Blocks)
			if err != nil {
				return nil, nil, fmt.Errorf("user message at index %d: %w", i, err)
			}
			out = append(out, anthropic.NewUserMessage(blocks...))

		case "assistant":
			flush()
			blocks, err := assistantBlocks(m.Blocks)
			if err != nil {
				return nil, nil, fmt.Errorf("assistant message at index %d: %w", i, err)
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewAssistantMessage(blocks...))
			}

		case "tool":
			for _, blk := range m.Blocks {
				tr, ok := blk.(core.ToolResultBlock)
				if !ok {
					return nil, nil, fmt.Errorf("tool message at index %d has non-tool_result block %T", i, blk)
				}
				block, err := toolResultBlock(tr)
				if err != nil {
					return nil, nil, fmt.Errorf("tool message at index %d: %w", i, err)
				}
				pending = append(pending, block)
			}

		default:
			return nil, nil, fmt.Errorf("unsupported message role %q at index %d", m.Role, i)
		}
	}
	flush()

	return system, out, nil
}

// userBlocks converts a user turn's blocks (text, image) into Anthropic content.
func userBlocks(blocks core.Blocks) ([]anthropic.ContentBlockParamUnion, error) {
	out := make([]anthropic.ContentBlockParamUnion, 0, len(blocks))
	for _, blk := range blocks {
		switch b := blk.(type) {
		case core.TextBlock:
			out = append(out, anthropic.NewTextBlock(b.Text))
		case core.ImageBlock:
			out = append(out, imageBlock(b))
		default:
			return nil, fmt.Errorf("unsupported user block %T", blk)
		}
	}
	return out, nil
}

// assistantBlocks converts an assistant turn's blocks into Anthropic content,
// preserving thinking (with its signature) and redacted_thinking so a tool loop
// replays them verbatim, which the API requires.
func assistantBlocks(blocks core.Blocks) ([]anthropic.ContentBlockParamUnion, error) {
	out := make([]anthropic.ContentBlockParamUnion, 0, len(blocks))
	for _, blk := range blocks {
		switch b := blk.(type) {
		case core.TextBlock:
			if b.Text != "" {
				out = append(out, anthropic.NewTextBlock(b.Text))
			}
		case core.ThinkingBlock:
			out = append(out, anthropic.NewThinkingBlock(b.Signature, b.Thinking))
		case core.ToolUseBlock:
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			out = append(out, anthropic.NewToolUseBlock(b.ID, json.RawMessage(input), b.Name))
		case core.RawBlock:
			if b.Provider == "anthropic" && b.Type == "redacted_thinking" {
				var d redactedThinkingData
				if err := json.Unmarshal(b.Data, &d); err != nil {
					return nil, fmt.Errorf("decode redacted_thinking: %w", err)
				}
				out = append(out, anthropic.ContentBlockParamUnion{
					OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{Data: d.Data},
				})
				continue
			}
			return nil, fmt.Errorf("unsupported raw block provider=%q type=%q", b.Provider, b.Type)
		default:
			return nil, fmt.Errorf("unsupported assistant block %T", blk)
		}
	}
	return out, nil
}

// toolResultBlock converts a core ToolResultBlock into an Anthropic tool_result
// block, mapping IsError onto the native is_error flag. Text-only content uses
// the simple constructor; content with images builds the richer param.
func toolResultBlock(tr core.ToolResultBlock) (anthropic.ContentBlockParamUnion, error) {
	hasImage := false
	var text strings.Builder
	for _, blk := range tr.Content {
		switch b := blk.(type) {
		case core.TextBlock:
			text.WriteString(b.Text)
		case core.ImageBlock:
			hasImage = true
		default:
			return anthropic.ContentBlockParamUnion{}, fmt.Errorf("unsupported tool_result content block %T", blk)
		}
	}
	if !hasImage {
		return anthropic.NewToolResultBlock(tr.ToolUseID, text.String(), tr.IsError), nil
	}

	content := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(tr.Content))
	for _, blk := range tr.Content {
		switch b := blk.(type) {
		case core.TextBlock:
			content = append(content, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: b.Text},
			})
		case core.ImageBlock:
			img := imageBlock(b)
			content = append(content, anthropic.ToolResultBlockParamContentUnion{OfImage: img.OfImage})
		}
	}
	trp := &anthropic.ToolResultBlockParam{ToolUseID: tr.ToolUseID, Content: content}
	if tr.IsError {
		trp.IsError = anthropic.Bool(true)
	}
	return anthropic.ContentBlockParamUnion{OfToolResult: trp}, nil
}

// imageBlock converts a core ImageBlock into an Anthropic image content block,
// preferring inline base64 data and falling back to a URL source.
func imageBlock(b core.ImageBlock) anthropic.ContentBlockParamUnion {
	if len(b.Data) > 0 {
		return anthropic.NewImageBlockBase64(b.MediaType, base64.StdEncoding.EncodeToString(b.Data))
	}
	return anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: b.URL})
}

// convertResponse translates an Anthropic Message back into core's block-based
// assistant message. Text, thinking (with signature), and tool_use blocks map
// to their core counterparts; redacted_thinking is preserved verbatim in a
// RawBlock so it survives a round-trip back to the API.
func convertResponse(resp *anthropic.Message) core.Message {
	var blocks core.Blocks

	for _, block := range resp.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			blocks = append(blocks, core.TextBlock{Text: v.Text})
		case anthropic.ThinkingBlock:
			blocks = append(blocks, core.ThinkingBlock{Thinking: v.Thinking, Signature: v.Signature})
		case anthropic.RedactedThinkingBlock:
			data, _ := json.Marshal(redactedThinkingData{Data: v.Data})
			blocks = append(blocks, core.RawBlock{
				Provider: "anthropic",
				Type:     "redacted_thinking",
				Data:     data,
			})
		case anthropic.ToolUseBlock:
			args := v.JSON.Input.Raw()
			if args == "" {
				args = "{}"
			}
			blocks = append(blocks, core.ToolUseBlock{
				ID:    v.ID,
				Name:  v.Name,
				Input: json.RawMessage(args),
			})
		}
	}

	msg := core.Message{Role: "assistant", Blocks: blocks}
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
