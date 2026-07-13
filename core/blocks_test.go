package core

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestBlocksRoundTrip pins that every block type survives a marshal/unmarshal
// cycle through the type-tagged envelope with its fields intact.
func TestBlocksRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		block Block
	}{
		{"text", TextBlock{Text: "hello world"}},
		{"thinking", ThinkingBlock{Thinking: "let me reason", Signature: "sig-abc"}},
		{"thinking no signature", ThinkingBlock{Thinking: "reason only"}},
		{"tool_use", ToolUseBlock{ID: "t1", Name: "search", Input: json.RawMessage(`{"q":"x"}`)}},
		{"tool_result", ToolResultBlock{ToolUseID: "t1", Content: Blocks{TextBlock{Text: "42"}}}},
		{"tool_result error", ToolResultBlock{ToolUseID: "t1", Content: Blocks{TextBlock{Text: "boom"}}, IsError: true}},
		{"image inline", ImageBlock{MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}}},
		{"image url", ImageBlock{URL: "https://example.com/x.png"}},
		{"raw", RawBlock{Provider: "anthropic", Type: "redacted_thinking", Data: json.RawMessage(`{"data":"enc"}`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := Blocks{c.block}
			data, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var out Blocks
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("unmarshal: %v (json: %s)", err, data)
			}
			if !reflect.DeepEqual(in, out) {
				t.Errorf("round-trip mismatch:\n in: %#v\nout: %#v\njson: %s", in, out, data)
			}
		})
	}
}

// TestBlockEnvelopeShape pins the wire shape: an inline "type" discriminant as
// the first field.
func TestBlockEnvelopeShape(t *testing.T) {
	data, err := json.Marshal(Blocks{TextBlock{Text: "hi"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(data), `[{"type":"text","text":"hi"}]`; got != want {
		t.Errorf("envelope = %s, want %s", got, want)
	}
}

// TestBlocksUnknownTypeBecomesRaw pins forward compatibility: a block with an
// unrecognized type tag decodes into a RawBlock preserving its bytes, rather
// than erroring.
func TestBlocksUnknownTypeBecomesRaw(t *testing.T) {
	data := []byte(`[{"type":"crystal_ball","vision":"the future"}]`)
	var out Blocks
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d blocks, want 1", len(out))
	}
	raw, ok := out[0].(RawBlock)
	if !ok {
		t.Fatalf("block type = %T, want RawBlock", out[0])
	}
	if raw.Type != "crystal_ball" {
		t.Errorf("raw.Type = %q, want crystal_ball", raw.Type)
	}
	// The original bytes are preserved verbatim in Data.
	var got map[string]any
	if err := json.Unmarshal(raw.Data, &got); err != nil {
		t.Fatalf("raw data not valid json: %v", err)
	}
	if got["vision"] != "the future" {
		t.Errorf("preserved data = %v, want vision:the future", got)
	}
}

// TestBlocksNestedToolResult pins that ToolResultBlock content (itself Blocks)
// gets the envelope recursively.
func TestBlocksNestedToolResult(t *testing.T) {
	in := Blocks{ToolResultBlock{
		ToolUseID: "t1",
		Content:   Blocks{TextBlock{Text: "see image"}, ImageBlock{MediaType: "image/jpeg", URL: "u"}},
	}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Blocks
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("nested round-trip mismatch:\n in: %#v\nout: %#v\njson: %s", in, out, data)
	}
}

// TestBlocksNilAndEmpty pins the boundary cases of the envelope.
func TestBlocksNilAndEmpty(t *testing.T) {
	// nil marshals to null and reloads as nil.
	data, err := json.Marshal(Blocks(nil))
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	if string(data) != "null" {
		t.Errorf("nil blocks = %s, want null", data)
	}
	var out Blocks
	if err := json.Unmarshal([]byte("null"), &out); err != nil {
		t.Fatalf("unmarshal null: %v", err)
	}
	if out != nil {
		t.Errorf("unmarshaled null = %#v, want nil", out)
	}
	// empty slice marshals to [] and reloads as a non-nil empty slice.
	data, err = json.Marshal(Blocks{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if string(data) != "[]" {
		t.Errorf("empty blocks = %s, want []", data)
	}
}
