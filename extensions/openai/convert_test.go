package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

// TestConvertMessagesToolResultTextOnly pins the existing text-only behavior:
// a string tool result maps 1:1 to a role:"tool" message, with IsError
// rendered as an "error:" content prefix.
func TestConvertMessagesToolResultTextOnly(t *testing.T) {
	msgs, err := convertMessages([]core.Message{
		core.ToolResultMessage("t1", "boom", true),
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d wire messages, want 1", len(msgs))
	}
	wm := msgs[0]
	if wm.Role != "tool" || wm.ToolCallID != "t1" || wm.Content != "error: boom" {
		t.Errorf("wire message = %+v", wm)
	}
}

// TestConvertMessagesToolResultRichText pins that a multi-text-block rich tool
// result flattens to the concatenation of its text, in order.
func TestConvertMessagesToolResultRichText(t *testing.T) {
	msgs, err := convertMessages([]core.Message{
		core.ToolResultBlockMessage("t1", core.Blocks{
			core.TextBlock{Text: "part one. "},
			core.TextBlock{Text: "part two"},
		}, false),
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if msgs[0].Content != "part one. part two" {
		t.Errorf("content = %v, want %q", msgs[0].Content, "part one. part two")
	}
}

// TestConvertMessagesToolResultImagePlaceholder pins the documented OpenAI
// degradation: Chat Completions has no tool-result image content, so image
// blocks become a deterministic placeholder instead of being silently dropped
// (which would leave the model an empty tool result).
func TestConvertMessagesToolResultImageOnly(t *testing.T) {
	msgs, err := convertMessages([]core.Message{
		core.ToolResultBlockMessage("t1", core.Blocks{
			core.ImageBlock{MediaType: "image/png", Data: []byte{0x89, 0x50}},
		}, false),
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	content, _ := msgs[0].Content.(string)
	if content != "[non-text tool result block: image/png]" {
		t.Errorf("content = %q, want placeholder", content)
	}
}

// TestConvertMessagesToolResultMixed pins mixed text/image results: text is
// preserved in order and each image becomes a placeholder line.
func TestConvertMessagesToolResultMixed(t *testing.T) {
	msgs, err := convertMessages([]core.Message{
		core.ToolResultBlockMessage("t1", core.Blocks{
			core.TextBlock{Text: "screenshot:"},
			core.ImageBlock{MediaType: "image/jpeg", Data: []byte{1}},
			core.ImageBlock{URL: "https://example.com/x.png"},
		}, false),
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	content, _ := msgs[0].Content.(string)
	want := "screenshot:[non-text tool result block: image/jpeg][non-text tool result block: image]"
	if content != want {
		t.Errorf("content = %q, want %q", content, want)
	}
}

// TestConvertMessagesToolResultErrorPrefix pins that an error rich result keeps
// the "error:" prefix in front of the (possibly placeholder-bearing) content.
func TestConvertMessagesToolResultErrorPrefix(t *testing.T) {
	msgs, err := convertMessages([]core.Message{
		core.ToolResultBlockMessage("t1", core.Blocks{core.ImageBlock{MediaType: "image/png", Data: []byte{1}}}, true),
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	content, _ := msgs[0].Content.(string)
	if !strings.HasPrefix(content, "error: [non-text tool result block: image/png]") {
		t.Errorf("content = %q, want error prefix before placeholder", content)
	}
}

// TestConvertMessagesToolResultWireShape pins the full JSON shape of a
// text-only tool result so a wire-level regression (extra/renamed fields)
// surfaces here.
func TestConvertMessagesToolResultWireShape(t *testing.T) {
	msgs, err := convertMessages([]core.Message{
		core.ToolResultMessage("t1", "hello", false),
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	blob, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"role":"tool","content":"hello","tool_call_id":"t1"}]`
	if string(blob) != want {
		t.Errorf("wire JSON = %s, want %s", blob, want)
	}
}
