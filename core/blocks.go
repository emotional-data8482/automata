package core

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Block is one piece of a [Message]'s content. The set of concrete block types
// is closed — [TextBlock], [ThinkingBlock], [ToolUseBlock], [ToolResultBlock],
// [ImageBlock] — with [RawBlock] as the escape hatch for provider-specific
// blocks that core does not model. The interface method is unexported so only
// this package can define blocks, which keeps the JSON envelope (see [Blocks])
// exhaustive.
type Block interface {
	blockType() string
}

// TextBlock is a run of assistant or user text.
type TextBlock struct {
	Text string `json:"text"`
}

func (TextBlock) blockType() string { return "text" }

// ThinkingBlock is a model reasoning block. Signature is the provider's
// integrity token for the thinking content; Anthropic requires it to be sent
// back verbatim when the thinking block is replayed in a later turn, so it must
// round-trip. Providers without extended thinking ignore these blocks.
type ThinkingBlock struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"signature,omitempty"`
}

func (ThinkingBlock) blockType() string { return "thinking" }

// ToolUseBlock is a tool call the model requested. Input is the raw JSON object
// of arguments (kept as [json.RawMessage] so transcripts render real JSON and
// an [Approver] can rewrite it without a decode/encode round-trip).
type ToolUseBlock struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (ToolUseBlock) blockType() string { return "tool_use" }

// ToolResultBlock carries the outcome of a [ToolUseBlock], matched by
// ToolUseID. Content is nested blocks (typically a [TextBlock], optionally an
// [ImageBlock]); IsError marks the tool as having failed, which providers with
// a native error flag map onto it.
type ToolResultBlock struct {
	ToolUseID string `json:"tool_use_id"`
	Content   Blocks `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

func (ToolResultBlock) blockType() string { return "tool_result" }

// ImageBlock is an image, supplied either inline (MediaType + Data) or by
// reference (URL). Data is base64-encoded by [encoding/json] when marshaled.
type ImageBlock struct {
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

func (ImageBlock) blockType() string { return "image" }

// RawBlock is the escape hatch for a provider-specific block core does not
// model (e.g. Anthropic's redacted_thinking). Provider names the provider that
// owns it, Type is the provider's block type, and Data is the provider block
// verbatim, so it survives round-trips and can be handed back to that provider
// untouched. Blocks with an unrecognized type tag also decode into a RawBlock,
// so a transcript written by a newer core never fails to load in an older one.
type RawBlock struct {
	Provider string          `json:"provider,omitempty"`
	Type     string          `json:"block_type"`
	Data     json.RawMessage `json:"data,omitempty"`
}

func (RawBlock) blockType() string { return "raw" }

// Blocks is a slice of [Block] with a type-tagged JSON representation: each
// block marshals to a JSON object with an inline "type" discriminant
// (`{"type":"text","text":"hi"}`), and unmarshaling dispatches on it. Because
// the envelope lives on this named slice type, [Message] needs no custom
// marshaler and a transcript persists and reloads as plain data.
type Blocks []Block

// MarshalJSON renders the slice as a JSON array of type-tagged block objects.
func (bs Blocks) MarshalJSON() ([]byte, error) {
	if bs == nil {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, b := range bs {
		if i > 0 {
			buf.WriteByte(',')
		}
		raw, err := marshalBlock(b)
		if err != nil {
			return nil, err
		}
		buf.Write(raw)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// UnmarshalJSON decodes a JSON array of type-tagged block objects, dispatching
// each on its "type" field. An unrecognized type is preserved as a [RawBlock]
// rather than failing the whole decode.
func (bs *Blocks) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*bs = nil
		return nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return err
	}
	out := make(Blocks, 0, len(raws))
	for _, raw := range raws {
		b, err := unmarshalBlock(raw)
		if err != nil {
			return err
		}
		out = append(out, b)
	}
	*bs = out
	return nil
}

// marshalBlock marshals one block and splices in its "type" discriminant as the
// first field of the resulting object.
func marshalBlock(b Block) ([]byte, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || raw[0] != '{' {
		return nil, fmt.Errorf("block %T did not marshal to a JSON object", b)
	}
	prefix := `{"type":"` + b.blockType() + `"`
	if bytes.Equal(raw, []byte("{}")) {
		return []byte(prefix + "}"), nil
	}
	out := make([]byte, 0, len(prefix)+len(raw))
	out = append(out, prefix...)
	out = append(out, ',')
	out = append(out, raw[1:]...) // drop the leading '{'
	return out, nil
}

// unmarshalBlock decodes one type-tagged block object into its concrete type.
func unmarshalBlock(raw json.RawMessage) (Block, error) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	switch envelope.Type {
	case "text":
		return decodeBlock[TextBlock](raw)
	case "thinking":
		return decodeBlock[ThinkingBlock](raw)
	case "tool_use":
		return decodeBlock[ToolUseBlock](raw)
	case "tool_result":
		return decodeBlock[ToolResultBlock](raw)
	case "image":
		return decodeBlock[ImageBlock](raw)
	case "raw":
		return decodeBlock[RawBlock](raw)
	default:
		// Unknown type (e.g. written by a newer core): preserve the whole block
		// verbatim so it round-trips instead of failing the decode.
		return RawBlock{Type: envelope.Type, Data: raw}, nil
	}
}

func decodeBlock[T Block](raw json.RawMessage) (Block, error) {
	var b T
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return b, nil
}
