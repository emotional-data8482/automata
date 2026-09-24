package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

// minimalResult is the smallest valid non-streaming ChatResult.
const minimalResult = `{
  "id": "resp-1",
  "object": "chat.completion",
  "created": 1730000000,
  "model": "vendor/model",
  "choices": [{
    "index": 0,
    "finish_reason": "stop",
    "message": {"role": "assistant", "content": "hi"}
  }]
}`

// newTestProvider serves handler at the SDK's base URL, so tests never touch
// the live API.
func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return New("vendor/model", WithAPIKey("test-key"), WithServerURL(ts.URL))
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, body)
}

func TestInvokeConvertsResponse(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{
		  "id": "resp-1",
		  "object": "chat.completion",
		  "created": 1730000000,
		  "model": "vendor/model",
		  "choices": [{
		    "index": 0,
		    "finish_reason": "tool_calls",
		    "message": {
		      "role": "assistant",
		      "content": "Let me check.",
		      "reasoning": "pondering",
		      "tool_calls": [
		        {"id": "call_1", "type": "function",
		         "function": {"name": "echo", "arguments": "{\"q\":\"hi\"}"}}
		      ]
		    }
		  }],
		  "usage": {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18,
		            "prompt_tokens_details": {"cached_tokens": 4, "cache_write_tokens": 2}}
		}`)
	})

	resp, err := p.Invoke(context.Background(), core.Request{
		Messages: []core.Message{textMsg("user", "check")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != core.StopToolUse || resp.RawStopReason != "tool_calls" {
		t.Errorf("stop = %v/%q, want tool_use/tool_calls", resp.StopReason, resp.RawStopReason)
	}
	if len(resp.Message.Blocks) != 3 {
		t.Fatalf("blocks = %#v", resp.Message.Blocks)
	}
	if _, ok := resp.Message.Blocks[0].(core.ThinkingBlock); !ok {
		t.Errorf("block 0 = %#v, want thinking", resp.Message.Blocks[0])
	}
	tu, ok := resp.Message.Blocks[2].(core.ToolUseBlock)
	if !ok || tu.ID != "call_1" || tu.Name != "echo" || string(tu.Input) != `{"q":"hi"}` {
		t.Errorf("tool use = %#v", resp.Message.Blocks[2])
	}
	wantUsage := core.Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 4, CacheCreationTokens: 2}
	if resp.Message.Usage == nil || *resp.Message.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", resp.Message.Usage, wantUsage)
	}
}

func TestInvokeRefusalMapsToContentFilter(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{
		  "id": "resp-1", "object": "chat.completion", "created": 1, "model": "vendor/model",
		  "choices": [{
		    "index": 0, "finish_reason": "stop",
		    "message": {"role": "assistant", "refusal": "cannot help"}
		  }]
		}`)
	})
	resp, err := p.Invoke(context.Background(), core.Request{Messages: []core.Message{textMsg("user", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != core.StopContentFilter {
		t.Errorf("stop = %v, want content_filter", resp.StopReason)
	}
	if text := resp.Message.Text(); text != "cannot help" {
		t.Errorf("text = %q", text)
	}
}

func TestInvokeNoChoices(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"id": "r", "object": "chat.completion", "created": 1,
		  "model": "vendor/model", "choices": []}`)
	})
	_, err := p.Invoke(context.Background(), core.Request{Messages: []core.Message{textMsg("user", "x")}})
	if err == nil || !contains(err.Error(), "no choices") {
		t.Fatalf("err = %v, want no-choices error", err)
	}
}

func TestInvokeMapsAPIErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		contentTyp string
		body       string
		wantCode   int
		retryable  bool
	}{
		// The typed JSON shape OpenRouter returns for 429s.
		{"rate limit json", 429, "application/json",
			`{"error": {"code": 429, "message": "rate limited"}}`, 429, true},
		// Untyped bodies fall back to the SDK's generic APIError.
		{"bad request plain", 400, "text/plain", "bad input", 400, false},
		{"server error json", 500, "application/json",
			`{"error": {"code": 500, "message": "boom"}}`, 500, true},
		{"unknown status plain", 418, "text/plain", "teapot", 418, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentTyp)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			_, err := p.Invoke(context.Background(), core.Request{Messages: []core.Message{textMsg("user", "x")}})
			if err == nil {
				t.Fatal("expected error")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v (%T), want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.wantCode {
				t.Errorf("status = %d, want %d", apiErr.StatusCode, tc.wantCode)
			}
			if apiErr.Retryable() != tc.retryable {
				t.Errorf("retryable = %v, want %v", apiErr.Retryable(), tc.retryable)
			}
		})
	}
}

func TestRequestShape(t *testing.T) {
	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q", got)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &captured); err != nil {
			t.Fatalf("request body: %v", err)
		}
		writeJSON(w, minimalResult)
	})

	temp := 0.5
	_, err := p.Invoke(context.Background(), core.Request{
		Messages: []core.Message{
			textMsg("system", "be brief"),
			textMsg("user", "hello"),
		},
		Tools: []core.ToolDefinition{{
			Name:        "echo",
			Description: "echo the query",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
		Options: core.CallOptions{
			Temperature:   &temp,
			MaxTokens:     64,
			StopSequences: []string{"END"},
			ToolChoice:    &core.ToolChoice{Mode: core.ToolChoiceAny},
			OutputSchema:  json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if captured["model"] != "vendor/model" {
		t.Errorf("model = %v", captured["model"])
	}
	if captured["stream"] == true {
		t.Errorf("stream = %v, want not true on Invoke", captured["stream"])
	}
	msgs, _ := captured["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %#v", captured["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be brief" {
		t.Errorf("system message = %#v", msgs[0])
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "hello" {
		t.Errorf("user message = %#v", msgs[1])
	}
	tools, _ := captured["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", captured["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool = %#v", tool)
	}
	fn, _ := tool["function"].(map[string]any)
	if fn["name"] != "echo" || fn["description"] != "echo the query" {
		t.Errorf("function = %#v", fn)
	}
	params, _ := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("parameters = %#v", params)
	}
	if captured["temperature"] != 0.5 {
		t.Errorf("temperature = %v", captured["temperature"])
	}
	if captured["max_tokens"] != float64(64) {
		t.Errorf("max_tokens = %v", captured["max_tokens"])
	}
	stop, _ := captured["stop"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %#v", captured["stop"])
	}
	if got, ok := captured["tool_choice"]; !ok || got != "required" {
		t.Errorf("tool_choice = %#v, want required", captured["tool_choice"])
	}
	rf, _ := captured["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Fatalf("response_format = %#v", captured["response_format"])
	}
	js, _ := rf["json_schema"].(map[string]any)
	if js["name"] != "response" {
		t.Errorf("json_schema = %#v", js)
	}
}

func TestNoOutputSchemaMeansNoResponseFormat(t *testing.T) {
	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &captured)
		writeJSON(w, minimalResult)
	})
	if _, err := p.Invoke(context.Background(), core.Request{Messages: []core.Message{textMsg("user", "x")}}); err != nil {
		t.Fatal(err)
	}
	if _, present := captured["response_format"]; present {
		t.Errorf("response_format sent without OutputSchema: %#v", captured["response_format"])
	}
}

func TestProviderImplementsStructuredOutputCapability(t *testing.T) {
	var p core.Provider = New("vendor/model")
	so, ok := p.(core.StructuredOutputProvider)
	if !ok || !so.SupportsNativeStructuredOutput() {
		t.Fatal("provider must advertise native structured output")
	}
}
