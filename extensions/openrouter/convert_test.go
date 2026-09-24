package openrouter

import (
	"encoding/json"
	"testing"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"

	orsdk "github.com/OpenRouterTeam/go-sdk"

	"github.com/emotional-data8482/automata/core"
)

func textMsg(role, text string) core.Message {
	return core.Message{Role: role, Blocks: core.Blocks{core.TextBlock{Text: text}}}
}

func TestConvertMessagesRolesAndToolFlow(t *testing.T) {
	msgs := []core.Message{
		textMsg("system", "be brief"),
		textMsg("user", "hello"),
		{Role: "assistant", Blocks: core.Blocks{
			core.TextBlock{Text: "checking"},
			core.ToolUseBlock{ID: "call_1", Name: "echo", Input: json.RawMessage(`{"q":"hi"}`)},
		}},
		{Role: "tool", Blocks: core.Blocks{
			core.ToolResultBlock{ToolUseID: "call_1", Content: core.Blocks{core.TextBlock{Text: "echo: hi"}}},
		}},
	}
	out, err := convertMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4", len(out))
	}
	if out[0].Type != components.ChatMessagesTypeSystem || out[0].ChatSystemMessage == nil {
		t.Errorf("system message = %#v", out[0])
	}
	if got := *out[0].ChatSystemMessage.Content.Str; got != "be brief" {
		t.Errorf("system content = %q", got)
	}
	if out[1].Type != components.ChatMessagesTypeUser {
		t.Errorf("user message = %#v", out[1])
	}
	if got := *out[1].ChatUserMessage.Content.Str; got != "hello" {
		t.Errorf("user content = %q", got)
	}
	am := out[2].ChatAssistantMessage
	if am == nil || len(am.ToolCalls) != 1 {
		t.Fatalf("assistant message = %#v", out[2])
	}
	tc := am.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "echo" || tc.Function.Arguments != `{"q":"hi"}` {
		t.Errorf("tool call = %#v", tc)
	}
	tm := out[3].ChatToolMessage
	if tm == nil || tm.ToolCallID != "call_1" || *tm.Content.Str != "echo: hi" {
		t.Errorf("tool message = %#v", out[3])
	}

	// The wire form must keep role discriminants and tool-call IDs.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"role":"system"`, `"role":"user"`, `"role":"assistant"`, `"role":"tool"`, `"tool_call_id":"call_1"`, `"id":"call_1"`} {
		if !contains(string(raw), want) {
			t.Errorf("wire messages missing %s: %s", want, raw)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestConvertMessagesToolErrorPrefix(t *testing.T) {
	msgs := []core.Message{{Role: "tool", Blocks: core.Blocks{
		core.ToolResultBlock{ToolUseID: "c1", IsError: true, Content: core.Blocks{core.TextBlock{Text: "boom"}}},
	}}}
	out, err := convertMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if got := *out[0].ChatToolMessage.Content.Str; got != "error: boom" {
		t.Errorf("tool error content = %q, want error-prefixed", got)
	}
}

func TestConvertMessagesRejectsUnknownRole(t *testing.T) {
	if _, err := convertMessages([]core.Message{{Role: "robot", Blocks: core.Blocks{core.TextBlock{Text: "x"}}}}); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestUserContentImages(t *testing.T) {
	msgs := []core.Message{{Role: "user", Blocks: core.Blocks{
		core.TextBlock{Text: "look"},
		core.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}},
		core.ImageBlock{URL: "https://example.com/pic.jpg"},
	}}}
	out, err := convertMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	um := out[0].ChatUserMessage
	if um == nil || um.Content.ArrayOfChatContentItems == nil {
		t.Fatalf("user message = %#v", out[0])
	}
	items := um.Content.ArrayOfChatContentItems
	if len(items) != 3 {
		t.Fatalf("content parts = %d, want 3", len(items))
	}
	if items[0].ChatContentText == nil || items[0].ChatContentText.Text != "look" {
		t.Errorf("text part = %#v", items[0])
	}
	img1 := items[1].ChatContentImage
	if img1 == nil || img1.ImageURL.URL != "data:image/png;base64,AQID" {
		t.Errorf("inline image = %#v", items[1])
	}
	img2 := items[2].ChatContentImage
	if img2 == nil || img2.ImageURL.URL != "https://example.com/pic.jpg" {
		t.Errorf("url image = %#v", items[2])
	}
}

func TestToolResultTextDegradesNonText(t *testing.T) {
	tr := core.ToolResultBlock{ToolUseID: "c1", Content: core.Blocks{
		core.TextBlock{Text: "ok: "},
		core.ImageBlock{MediaType: "image/png"},
		core.RawBlock{Provider: "openrouter", Type: "weird"},
	}}
	got := toolResultText(tr)
	want := "ok: [non-text tool result block: image/png][non-text tool result block omitted]"
	if got != want {
		t.Errorf("toolResultText = %q, want %q", got, want)
	}
}

func TestAssistantThinkingDroppedOnSend(t *testing.T) {
	msgs := []core.Message{{Role: "assistant", Blocks: core.Blocks{
		core.ThinkingBlock{Thinking: "secret", Signature: "sig"},
		core.TextBlock{Text: "answer"},
	}}}
	out, err := convertMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(raw), "secret") || contains(string(raw), "sig") {
		t.Errorf("thinking leaked to wire: %s", raw)
	}
}

func TestConvertResponse(t *testing.T) {
	msg := components.ChatAssistantMessage{
		Content:   optionalnullable.From(orsdk.Pointer(components.CreateChatAssistantMessageContentStr("hi there"))),
		Reasoning: optionalnullable.From(orsdk.Pointer("pondering")),
		ToolCalls: []components.ChatToolCall{{
			ID:   "call_9",
			Type: components.ChatToolCallTypeFunction,
			Function: components.ChatToolCallFunction{
				Name:      "echo",
				Arguments: `{"q":"x"}`,
			},
		}},
	}
	got := convertResponse(msg)
	if got.Role != "assistant" || len(got.Blocks) != 3 {
		t.Fatalf("message = %#v", got)
	}
	think, ok := got.Blocks[0].(core.ThinkingBlock)
	if !ok || think.Thinking != "pondering" {
		t.Errorf("thinking block = %#v", got.Blocks[0])
	}
	if _, ok := got.Blocks[1].(core.TextBlock); !ok {
		t.Errorf("text block = %#v", got.Blocks[1])
	}
	tu, ok := got.Blocks[2].(core.ToolUseBlock)
	if !ok || tu.ID != "call_9" || tu.Name != "echo" || string(tu.Input) != `{"q":"x"}` {
		t.Errorf("tool use block = %#v", got.Blocks[2])
	}
}

func TestConvertResponseRefusal(t *testing.T) {
	msg := components.ChatAssistantMessage{
		Refusal: optionalnullable.From(orsdk.Pointer("cannot help")),
	}
	got := convertResponse(msg)
	if len(got.Blocks) != 1 {
		t.Fatalf("blocks = %#v", got.Blocks)
	}
	if tb, ok := got.Blocks[0].(core.TextBlock); !ok || tb.Text != "cannot help" {
		t.Errorf("refusal block = %#v", got.Blocks[0])
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := map[string]core.StopReason{
		"stop":           core.StopEndTurn,
		"tool_calls":     core.StopToolUse,
		"function_call":  core.StopToolUse,
		"length":         core.StopTokenLimit,
		"content_filter": core.StopContentFilter,
		"error":          core.StopUnknown,
		"":               core.StopUnknown,
		"mystery":        core.StopUnknown,
	}
	for raw, want := range cases {
		if got := mapFinishReason(raw); got != want {
			t.Errorf("mapFinishReason(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestUsageToCore(t *testing.T) {
	if got := usageToCore(nil); got != nil {
		t.Errorf("nil usage = %+v, want nil", got)
	}
	zero := &components.ChatUsage{}
	if got := usageToCore(zero); got != nil {
		t.Errorf("zero usage = %+v, want nil", got)
	}
	u := &components.ChatUsage{
		PromptTokens:     11,
		CompletionTokens: 7,
		TotalTokens:      18,
		PromptTokensDetails: optionalnullable.From(&components.ChatUsagePromptTokensDetails{
			CachedTokens:     orsdk.Pointer(int64(4)),
			CacheWriteTokens: orsdk.Pointer(int64(2)),
		}),
	}
	got := usageToCore(u)
	if got == nil {
		t.Fatal("usage = nil")
	}
	want := core.Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 4, CacheCreationTokens: 2}
	if *got != want {
		t.Errorf("usage = %+v, want %+v", *got, want)
	}
}

func TestConvertTools(t *testing.T) {
	tools, err := convertTools([]core.ToolDefinition{{
		Name:        "echo",
		Description: "echo the query",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("len = %d", len(tools))
	}
	fn := tools[0].ChatFunctionToolFunction
	if fn == nil {
		t.Fatalf("tool union = %#v", tools[0])
	}
	if fn.Function.Name != "echo" || *fn.Function.Description != "echo the query" {
		t.Errorf("function = %#v", fn.Function)
	}
	if fn.Function.Parameters["type"] != "object" {
		t.Errorf("parameters = %#v", fn.Function.Parameters)
	}

	if _, err := convertTools([]core.ToolDefinition{{Name: "bad", InputSchema: json.RawMessage(`{not json`)}}); err == nil {
		t.Error("expected error for malformed schema")
	}
}
