package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emotional-data/automata/core"
)

func TestAPIErrorRetryable(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{{429, true}, {500, true}, {503, true}, {400, false}, {404, false}}
	for _, c := range cases {
		e := &APIError{StatusCode: c.code}
		if e.Retryable() != c.want {
			t.Errorf("Retryable(%d) = %v, want %v", c.code, e.Retryable(), c.want)
		}
	}
}

// TestInvokeRequestShape pins the request body: system/user/assistant/tool
// messages, tool schemas, and options all map to OpenAI wire fields.
func TestInvokeRequestShape(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	temp := 0.4
	p := New("gpt-test", srv.URL).WithAPIKey("k")
	req := core.Request{
		Messages: []core.Message{
			core.SystemMessage("be brief"),
			core.UserMessage("hello"),
			core.AssistantMessage(core.ToolUseBlock{ID: "c1", Name: "search", Input: []byte(`{"q":"x"}`)}),
			core.ToolResultMessage("c1", "result-text", false),
		},
		Tools: []core.Tool{core.Func("search", "search the web", func(_ context.Context, _ struct {
			Q string `json:"q"`
		}) (string, error) {
			return "", nil
		})},
		Options: core.CallOptions{Temperature: &temp, MaxTokens: 256, StopSequences: []string{"STOP"}},
	}
	resp, err := p.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Message.Text() != "hi" || resp.StopReason != "stop" {
		t.Errorf("response = %+v, want text 'hi' / stop", resp)
	}
	if resp.Message.Usage == nil || resp.Message.Usage.InputTokens != 5 {
		t.Errorf("usage = %+v, want InputTokens 5", resp.Message.Usage)
	}

	// Request body assertions.
	if gotBody["model"] != "gpt-test" || gotBody["max_tokens"].(float64) != 256 {
		t.Errorf("model/max_tokens wrong: %v", gotBody)
	}
	if gotBody["temperature"].(float64) != 0.4 {
		t.Errorf("temperature = %v, want 0.4", gotBody["temperature"])
	}
	msgs := gotBody["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("sent %d messages, want 4", len(msgs))
	}
	// The assistant message must carry a tool_calls array.
	asst := msgs[2].(map[string]any)
	if _, ok := asst["tool_calls"]; !ok {
		t.Errorf("assistant message missing tool_calls: %v", asst)
	}
	// The tool result maps to a role:"tool" message with tool_call_id.
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" {
		t.Errorf("tool message wrong: %v", tool)
	}
	// Tools advertised.
	tools := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("sent %d tools, want 1", len(tools))
	}
}

// TestInvokeToolResultIsErrorPrefix pins that an error tool result is rendered
// with an "error:" prefix (OpenAI has no native error flag).
func TestInvokeToolResultIsErrorPrefix(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	p := New("m", srv.URL)
	_, err := p.Invoke(context.Background(), core.Request{Messages: []core.Message{
		core.ToolResultMessage("c1", "boom", true),
	}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	tool := gotBody["messages"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(tool["content"].(string), "error: ") {
		t.Errorf("error tool result = %q, want an 'error:' prefix", tool["content"])
	}
}

// TestInvokeUserImage pins that an image block becomes an image_url content part
// with a data URL.
func TestInvokeUserImage(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"seen"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	p := New("m", srv.URL)
	_, err := p.Invoke(context.Background(), core.Request{Messages: []core.Message{
		{Role: "user", Blocks: core.Blocks{
			core.TextBlock{Text: "what is this"},
			core.ImageBlock{MediaType: "image/png", Data: []byte{1, 2, 3}},
		}},
	}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	raw, _ := json.Marshal(gotBody)
	if !strings.Contains(string(raw), "image_url") || !strings.Contains(string(raw), "data:image/png;base64,") {
		t.Errorf("image not converted to a data URL: %s", raw)
	}
}

// TestInvokeThinkingDroppedOnSend pins that thinking blocks are omitted from the
// wire (Chat Completions has no input for them).
func TestInvokeThinkingDroppedOnSend(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	p := New("m", srv.URL)
	_, err := p.Invoke(context.Background(), core.Request{Messages: []core.Message{
		{Role: "assistant", Blocks: core.Blocks{
			core.ThinkingBlock{Thinking: "secret reasoning", Signature: "sig"},
			core.TextBlock{Text: "visible answer"},
		}},
	}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	raw, _ := json.Marshal(gotBody)
	if strings.Contains(string(raw), "secret reasoning") {
		t.Errorf("thinking leaked onto the wire: %s", raw)
	}
	if !strings.Contains(string(raw), "visible answer") {
		t.Errorf("assistant text missing: %s", raw)
	}
}

// TestInvokeError pins non-2xx handling as a retryable APIError.
func TestInvokeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, "rate limited")
	}))
	defer srv.Close()

	p := New("m", srv.URL)
	_, err := p.Invoke(context.Background(), core.Request{Messages: []core.Message{core.UserMessage("hi")}})
	var apiErr *APIError
	if err == nil || !isAPIError(err, &apiErr) || apiErr.StatusCode != 429 || !apiErr.Retryable() {
		t.Fatalf("err = %v, want retryable 429 APIError", err)
	}
}

func isAPIError(err error, target **APIError) bool {
	e, ok := err.(*APIError)
	if ok {
		*target = e
	}
	return ok
}

// TestInvokeStreamAssemblesTextAndToolCalls pins SSE parsing: text deltas and
// tool-call deltas (at synthetic block indices) assemble into the right events,
// and the final usage chunk is reported.
func TestInvokeStreamAssemblesTextAndToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"search","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"x\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4}}`,
		`data: [DONE]`,
		"",
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse)
	}))
	defer srv.Close()

	p := New("m", srv.URL).WithStreamUsage()
	ch, err := p.InvokeStream(context.Background(), core.Request{Messages: []core.Message{core.UserMessage("go")}})
	if err != nil {
		t.Fatalf("InvokeStream: %v", err)
	}

	var text strings.Builder
	var toolName, toolArgs string
	var toolIndex = -1
	var usageSeen bool
	var finish string
	for chunk := range ch {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usageSeen = true
			if chunk.Usage.InputTokens != 11 {
				t.Errorf("usage InputTokens = %d, want 11", chunk.Usage.InputTokens)
			}
		}
		for _, d := range chunk.Deltas {
			switch {
			case d.Index == 0 && d.Text != "":
				text.WriteString(d.Text)
			case d.Index >= 1:
				toolIndex = d.Index
				if d.Name != "" {
					toolName = d.Name
				}
				toolArgs += d.PartialJSON
			}
		}
	}
	if text.String() != "Hello" {
		t.Errorf("assembled text = %q, want Hello", text.String())
	}
	if toolIndex != 1 {
		t.Errorf("tool block index = %d, want 1 (0 reserved for text)", toolIndex)
	}
	if toolName != "search" || toolArgs != `{"q":"x"}` {
		t.Errorf("tool call = %s(%s), want search({\"q\":\"x\"})", toolName, toolArgs)
	}
	if finish != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", finish)
	}
	if !usageSeen {
		t.Error("usage chunk not reported")
	}
}

// TestStreamEndToEndThroughLoop drives a full RunStream through the provider to
// prove the assembled message flows back into the run loop and a tool executes.
func TestStreamEndToEndThroughLoop(t *testing.T) {
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if turn == 0 {
			turn++
			io.WriteString(w, strings.Join([]string{
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"echo","arguments":"{}"}}]}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`, "",
			}, "\n"))
			return
		}
		io.WriteString(w, strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"done"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`, "",
		}, "\n"))
	}))
	defer srv.Close()

	agent := core.New(New("m", srv.URL))
	agent.RegisterTool(core.Func("echo", "echo", func(_ context.Context, _ struct{}) (string, error) {
		return "echoed", nil
	}))

	res, err := agent.RunStream(context.Background(), "go", nil)
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if res.Output != "done" {
		t.Errorf("output = %q, want done", res.Output)
	}
}
