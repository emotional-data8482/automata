package openai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/emotional-data8482/automata/core"
)

// --- wire types (OpenAI Chat Completions) ---------------------------------

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"` // string, or []contentPart for images
	Refusal    string         `json:"refusal,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type wireToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"` // "function"
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type wireTool struct {
	Type     string          `json:"type"` // "function"
	Function json.RawMessage `json:"function"`
}

// convertTools maps core tools to OpenAI function tools. core's schema shape
// ({name, description, parameters}) is exactly OpenAI's function shape, so it
// passes through as the function payload.
func convertTools(tools []core.ToolDefinition) []wireTool {
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		raw, _ := json.Marshal(struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters"`
		}{t.Name, t.Description, t.InputSchema})
		out = append(out, wireTool{Type: "function", Function: raw})
	}
	return out
}

// toolResultText flattens a tool result's blocks into the text-only content a
// Chat Completions tool message can represent. Text blocks concatenate in
// order; non-text blocks (images, …) become a documented placeholder line so a
// rich result never arrives at the model as an empty string:
//
//	[non-text tool result block: image/png]
//
// Chat Completions has no native tool-result image/structured content, so
// degradation is by placeholder rather than a conversion error. Callers that
// need the model to see images should use a provider with native support
// (Anthropic) or route the image through a user message.
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

// convertMessages maps core's block messages to OpenAI wire messages. Thinking
// and provider-raw blocks are dropped on send (Chat Completions has no input for
// them). Tool results map 1:1 to role:"tool" messages; IsError is rendered as an
// "error:" content prefix since OpenAI has no native error flag.
func convertMessages(msgs []core.Message) ([]wireMessage, error) {
	out := make([]wireMessage, 0, len(msgs))
	for i, m := range msgs {
		switch m.Role {
		case "system":
			out = append(out, wireMessage{Role: "system", Content: m.Text()})

		case "user":
			content, err := userContent(m.Blocks)
			if err != nil {
				return nil, fmt.Errorf("user message at index %d: %w", i, err)
			}
			out = append(out, wireMessage{Role: "user", Content: content})

		case "assistant":
			wm := wireMessage{Role: "assistant"}
			if text := m.Text(); text != "" {
				wm.Content = text
			}
			for _, tu := range m.ToolUses() {
				args := string(tu.Input)
				if args == "" {
					args = "{}"
				}
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
					ID:       tu.ID,
					Type:     "function",
					Function: wireFunction{Name: tu.Name, Arguments: args},
				})
			}
			out = append(out, wm)

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
				out = append(out, wireMessage{
					Role:       "tool",
					ToolCallID: tr.ToolUseID,
					Content:    content,
				})
			}

		default:
			return nil, fmt.Errorf("unsupported message role %q at index %d", m.Role, i)
		}
	}
	return out, nil
}

// userContent returns either a plain string (text-only) or an array of content
// parts (when images are present).
func userContent(blocks core.Blocks) (any, error) {
	hasImage := false
	for _, blk := range blocks {
		if _, ok := blk.(core.ImageBlock); ok {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return core.Message{Blocks: blocks}.Text(), nil
	}

	parts := make([]contentPart, 0, len(blocks))
	for _, blk := range blocks {
		switch b := blk.(type) {
		case core.TextBlock:
			parts = append(parts, contentPart{Type: "text", Text: b.Text})
		case core.ImageBlock:
			parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: imageDataURL(b)}})
		default:
			return nil, fmt.Errorf("unsupported user block %T", blk)
		}
	}
	return parts, nil
}

// imageDataURL renders a core ImageBlock as a value OpenAI accepts for
// image_url: a data: URL for inline bytes, or the URL as-is.
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

// convertResponse maps an OpenAI choice message back into a core assistant
// message: content becomes a TextBlock, tool_calls become ToolUseBlocks.
func convertResponse(msg wireMessage) core.Message {
	var blocks core.Blocks
	if s, ok := msg.Content.(string); ok && s != "" {
		blocks = append(blocks, core.TextBlock{Text: s})
	}
	if msg.Refusal != "" {
		blocks = append(blocks, core.TextBlock{Text: msg.Refusal})
	}
	for _, tc := range msg.ToolCalls {
		args := tc.Function.Arguments
		blocks = append(blocks, core.ToolUseBlock{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(args),
		})
	}
	return core.Message{Role: "assistant", Blocks: blocks}
}
