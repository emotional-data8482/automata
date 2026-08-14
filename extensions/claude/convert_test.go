package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/emotional-data8482/automata/core"
)

// TestConvertMessagesHoistsSystem pins that system messages become the System
// parameter and do not appear as conversation turns.
func TestConvertMessagesHoistsSystem(t *testing.T) {
	system, msgs, err := convertMessages([]core.Message{
		core.SystemMessage("be terse"),
		core.UserMessage("hi"),
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	if len(system) != 1 || system[0].Text != "be terse" {
		t.Errorf("system = %+v, want one block 'be terse'", system)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (user only)", len(msgs))
	}
}

// TestConvertMessagesThinkingRoundTrip pins the headline fix: an assistant
// message carrying a thinking block (with signature) and a tool_use block
// converts to Anthropic thinking + tool_use params, preserving the signature.
func TestConvertMessagesThinkingRoundTrip(t *testing.T) {
	_, msgs, err := convertMessages([]core.Message{
		{Role: "assistant", Blocks: core.Blocks{
			core.ThinkingBlock{Thinking: "reason", Signature: "sig-xyz"},
			core.ToolUseBlock{ID: "t1", Name: "lookup", Input: []byte(`{"q":"x"}`)},
		}},
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	blob, _ := json.Marshal(msgs)
	s := string(blob)
	for _, want := range []string{`"type":"thinking"`, `"signature":"sig-xyz"`, `"thinking":"reason"`, `"type":"tool_use"`, `"name":"lookup"`} {
		if !strings.Contains(s, want) {
			t.Errorf("marshaled assistant message missing %q\n got: %s", want, s)
		}
	}
}

// TestConvertMessagesToolResultIsError pins that ToolResultBlock.IsError maps
// onto Anthropic's native is_error flag.
func TestConvertMessagesToolResultIsError(t *testing.T) {
	_, msgs, err := convertMessages([]core.Message{
		core.ToolResultMessage("t1", "boom", true),
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	blob, _ := json.Marshal(msgs)
	s := string(blob)
	if !strings.Contains(s, `"is_error":true`) {
		t.Errorf("tool result missing is_error:true\n got: %s", s)
	}
	if !strings.Contains(s, "boom") {
		t.Errorf("tool result missing content\n got: %s", s)
	}
}

// TestConvertMessagesUserImage pins that an image block becomes an Anthropic
// image content block (base64 source).
func TestConvertMessagesUserImage(t *testing.T) {
	_, msgs, err := convertMessages([]core.Message{
		{Role: "user", Blocks: core.Blocks{
			core.TextBlock{Text: "look:"},
			core.ImageBlock{MediaType: "image/png", Data: []byte{0x89, 0x50}},
		}},
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	blob, _ := json.Marshal(msgs)
	s := string(blob)
	if !strings.Contains(s, `"type":"image"`) || !strings.Contains(s, "base64") {
		t.Errorf("user image not converted to a base64 image block\n got: %s", s)
	}
}

// TestConvertMessagesRedactedThinkingRoundTrip pins that a RawBlock holding
// Anthropic's redacted_thinking converts back to the redacted_thinking param.
func TestConvertMessagesRedactedThinkingRoundTrip(t *testing.T) {
	_, msgs, err := convertMessages([]core.Message{
		{Role: "assistant", Blocks: core.Blocks{
			core.RawBlock{Provider: "anthropic", Type: "redacted_thinking", Data: []byte(`{"data":"ENCRYPTED"}`)},
		}},
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	blob, _ := json.Marshal(msgs)
	s := string(blob)
	if !strings.Contains(s, `"type":"redacted_thinking"`) || !strings.Contains(s, "ENCRYPTED") {
		t.Errorf("redacted_thinking not reconstructed\n got: %s", s)
	}
}

// TestConvertResponse pins the receive direction: an Anthropic Message with
// thinking, redacted_thinking, text, and tool_use blocks decodes into the
// matching core blocks (and preserves the thinking signature and redacted data).
func TestConvertResponse(t *testing.T) {
	// Build a realistic API response JSON and let the SDK unmarshal it, since
	// the response block types are populated from JSON, not constructed.
	raw := `{
		"id": "msg_1",
		"type": "message",
		"role": "assistant",
		"model": "claude",
		"stop_reason": "tool_use",
		"content": [
			{"type": "thinking", "thinking": "hmm", "signature": "sig-1"},
			{"type": "redacted_thinking", "data": "ENC"},
			{"type": "text", "text": "here we go"},
			{"type": "tool_use", "id": "t1", "name": "lookup", "input": {"q": "x"}}
		],
		"usage": {"input_tokens": 10, "output_tokens": 4}
	}`
	var m anthropic.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal api response: %v", err)
	}

	msg := convertResponse(&m)
	if msg.Role != "assistant" {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	if len(msg.Blocks) != 4 {
		t.Fatalf("got %d blocks, want 4: %#v", len(msg.Blocks), msg.Blocks)
	}
	think, ok := msg.Blocks[0].(core.ThinkingBlock)
	if !ok || think.Thinking != "hmm" || think.Signature != "sig-1" {
		t.Errorf("block 0 = %#v, want thinking with signature", msg.Blocks[0])
	}
	raw0, ok := msg.Blocks[1].(core.RawBlock)
	if !ok || raw0.Provider != "anthropic" || raw0.Type != "redacted_thinking" || !strings.Contains(string(raw0.Data), "ENC") {
		t.Errorf("block 1 = %#v, want redacted_thinking raw block", msg.Blocks[1])
	}
	if txt, ok := msg.Blocks[2].(core.TextBlock); !ok || txt.Text != "here we go" {
		t.Errorf("block 2 = %#v, want text", msg.Blocks[2])
	}
	tu, ok := msg.Blocks[3].(core.ToolUseBlock)
	if !ok || tu.ID != "t1" || tu.Name != "lookup" || !strings.Contains(string(tu.Input), `"q"`) {
		t.Errorf("block 3 = %#v, want tool_use", msg.Blocks[3])
	}
	if msg.Usage == nil || msg.Usage.InputTokens != 10 || msg.Usage.OutputTokens != 4 {
		t.Errorf("usage = %+v, want {10 4}", msg.Usage)
	}
}

// TestConvertMessagesCoalescesToolResults pins that consecutive tool messages
// collapse into a single user turn (Anthropic requires alternating roles).
func TestConvertMessagesCoalescesToolResults(t *testing.T) {
	_, msgs, err := convertMessages([]core.Message{
		core.UserMessage("go"),
		{Role: "assistant", Blocks: core.Blocks{
			core.ToolUseBlock{ID: "a", Name: "x", Input: []byte("{}")},
			core.ToolUseBlock{ID: "b", Name: "y", Input: []byte("{}")},
		}},
		core.ToolResultMessage("a", "ra", false),
		core.ToolResultMessage("b", "rb", false),
	})
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	// user, assistant, then ONE coalesced user turn of tool_results.
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3 (tool results coalesced)", len(msgs))
	}
}
