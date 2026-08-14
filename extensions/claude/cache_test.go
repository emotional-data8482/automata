package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

// TestConversationCacheStampsLastMessage pins that WithConversationCache places
// a cache_control breakpoint on the last message's content.
func TestConversationCacheStampsLastMessage(t *testing.T) {
	p := New("claude-x", "key").WithConversationCache()
	params, err := p.buildParams(core.Request{Messages: []core.Message{
		core.UserMessage("first"),
		core.AssistantMessage(core.TextBlock{Text: "reply"}),
		core.UserMessage("follow up"),
	}})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	last := params.Messages[len(params.Messages)-1]
	blob, _ := json.Marshal(last)
	if !strings.Contains(string(blob), "cache_control") {
		t.Errorf("last message missing cache_control: %s", blob)
	}
}

// TestConversationCacheWalksPastThinking pins that the breakpoint lands on a
// cacheable block, skipping a trailing thinking block (which rejects
// cache_control).
func TestConversationCacheWalksPastThinking(t *testing.T) {
	p := New("claude-x", "key").WithConversationCache()
	// Artificial ordering: a thinking block AFTER the text block, so the cache
	// must walk back past it to the text block.
	params, err := p.buildParams(core.Request{Messages: []core.Message{
		core.UserMessage("go"),
		{Role: "assistant", Blocks: core.Blocks{
			core.TextBlock{Text: "the answer"},
			core.ThinkingBlock{Thinking: "reasoning", Signature: "sig"},
		}},
	}})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	last := params.Messages[len(params.Messages)-1]
	for _, block := range last.Content {
		blob, _ := json.Marshal(block)
		s := string(blob)
		if strings.Contains(s, `"type":"thinking"`) && strings.Contains(s, "cache_control") {
			t.Errorf("cache_control landed on a thinking block: %s", s)
		}
		if strings.Contains(s, `"type":"text"`) && !strings.Contains(s, "cache_control") {
			t.Errorf("cache_control did not land on the text block: %s", s)
		}
	}
}

// TestNoConversationCacheByDefault pins that the breakpoint is absent unless
// WithConversationCache is set.
func TestNoConversationCacheByDefault(t *testing.T) {
	p := New("claude-x", "key")
	params, err := p.buildParams(core.Request{Messages: []core.Message{
		core.UserMessage("hello"),
	}})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	blob, _ := json.Marshal(params.Messages)
	if strings.Contains(string(blob), "cache_control") {
		t.Errorf("cache_control present without WithConversationCache: %s", blob)
	}
}

// TestCallOptionsMapped pins that CallOptions reach the Anthropic params.
func TestCallOptionsMapped(t *testing.T) {
	p := New("claude-x", "key")
	temp := 0.3
	params, err := p.buildParams(core.Request{
		Messages: []core.Message{core.UserMessage("go")},
		Options: core.CallOptions{
			Temperature:    &temp,
			MaxTokens:      2048,
			StopSequences:  []string{"STOP"},
			ThinkingBudget: 4096,
			ToolChoice:     &core.ToolChoice{Mode: core.ToolChoiceTool, Name: "answer"},
		},
	})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if params.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %d, want 2048", params.MaxTokens)
	}
	blob, _ := json.Marshal(params)
	s := string(blob)
	for _, want := range []string{`"temperature":0.3`, "STOP", `"budget_tokens":4096`, `"name":"answer"`} {
		if !strings.Contains(s, want) {
			t.Errorf("params missing %q\n got: %s", want, s)
		}
	}
}
