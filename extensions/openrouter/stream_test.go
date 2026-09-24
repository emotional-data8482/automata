package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

// writeSSE emits server-sent events followed by the [DONE] sentinel, matching
// OpenRouter's streaming wire format.
func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "data: %s\n\n", e)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// chunkJSON wraps a streaming choice payload in the ChatStreamChunk envelope.
func chunkJSON(choices, usage string) string {
	return fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"vendor/model","choices":[%s]%s}`,
		choices, usage)
}

func TestStreamAssemblesDeltas(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			chunkJSON(`{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}`, ""),
			chunkJSON(`{"index":0,"delta":{"reasoning":"hmm"},"finish_reason":null}`, ""),
			chunkJSON(`{"index":0,"delta":{"content":"lo","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"q\":"}}]},"finish_reason":null}`, ""),
			chunkJSON(`{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hi\"}"}}]},"finish_reason":"tool_calls"}`, ""),
			chunkJSON("", `,"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":4}}`),
		)
	})

	ch, err := p.InvokeStream(context.Background(), core.Request{Messages: []core.Message{textMsg("user", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []core.StreamChunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	if len(chunks) != 5 {
		t.Fatalf("chunks = %d, want 5: %+v", len(chunks), chunks)
	}
	// 1: text delta at block 0.
	if d := chunks[0].Deltas; len(d) != 1 || d[0].Index != textBlockIndex || d[0].Type != "text" || d[0].Text != "Hel" {
		t.Errorf("chunk 0 deltas = %+v", d)
	}
	// 2: thinking delta at block 1.
	if d := chunks[1].Deltas; len(d) != 1 || d[0].Index != thinkingBlockIndex || d[0].Type != "thinking" || d[0].Text != "hmm" {
		t.Errorf("chunk 1 deltas = %+v", d)
	}
	// 3: text continues at 0; tool call opens at block 2.
	d := chunks[2].Deltas
	if len(d) != 2 || d[0].Text != "lo" {
		t.Fatalf("chunk 2 deltas = %+v", d)
	}
	if d[1].Index != toolBlockBaseIndex || d[1].Type != "tool_use" || d[1].ID != "call_1" || d[1].Name != "echo" || d[1].PartialJSON != `{"q":` {
		t.Errorf("tool open delta = %+v", d[1])
	}
	// 4: tool arguments continue; finish reason lands.
	d = chunks[3].Deltas
	if len(d) != 1 || d[0].Index != toolBlockBaseIndex || d[0].PartialJSON != `"hi"}` {
		t.Errorf("tool continuation = %+v", d)
	}
	if chunks[3].StopReason != core.StopToolUse || chunks[3].RawStopReason != "tool_calls" {
		t.Errorf("finish = %v/%q", chunks[3].StopReason, chunks[3].RawStopReason)
	}
	// 5: usage on the final chunk.
	if u := chunks[4].Usage; u == nil || u.InputTokens != 11 || u.OutputTokens != 7 || u.CacheReadTokens != 4 {
		t.Errorf("usage = %+v", chunks[4].Usage)
	}
}

func TestStreamSkipsEmptyChunks(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			// Role-only delta carries no information.
			chunkJSON(`{"index":0,"delta":{"role":"assistant"},"finish_reason":null}`, ""),
			chunkJSON(`{"index":0,"delta":{"content":"hi"},"finish_reason":null}`, ""),
		)
	})
	ch, err := p.InvokeStream(context.Background(), core.Request{Messages: []core.Message{textMsg("user", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []core.StreamChunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1 (role-only dropped): %+v", len(chunks), chunks)
	}
	if d := chunks[0].Deltas; len(d) != 1 || d[0].Text != "hi" {
		t.Errorf("deltas = %+v", chunks[0].Deltas)
	}
}

func TestStreamRequestHasStreamTrue(t *testing.T) {
	var captured map[string]any
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &captured)
		writeSSE(w, chunkJSON(`{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}`, ""))
	})
	ch, err := p.InvokeStream(context.Background(), core.Request{Messages: []core.Message{textMsg("user", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if captured["stream"] != true {
		t.Errorf("stream = %v, want true on InvokeStream", captured["stream"])
	}
}

func TestStreamMidStreamErrorFailsTurn(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, chunkJSON(`{"index":0,"delta":{"content":"par"},"finish_reason":null}`, ""),
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"vendor/model","choices":[],"error":{"code":429,"message":"slow down"}}`)
	})
	ch, err := p.InvokeStream(context.Background(), core.Request{Messages: []core.Message{textMsg("user", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	var sawText, sawErr bool
	for c := range ch {
		if len(c.Deltas) > 0 {
			sawText = true
		}
		if c.Err != nil {
			sawErr = true
			if !strings.Contains(c.Err.Error(), "429") || !strings.Contains(c.Err.Error(), "slow down") {
				t.Errorf("err = %v, want openrouter stream error with code and message", c.Err)
			}
		}
	}
	if !sawText || !sawErr {
		t.Errorf("sawText=%v sawErr=%v, want partial content then stream error", sawText, sawErr)
	}
}

func TestStreamMalformedFrameSurfacesError(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {not json\n\n")
	})
	ch, err := p.InvokeStream(context.Background(), core.Request{Messages: []core.Message{textMsg("user", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	var gotErr bool
	for c := range ch {
		if c.Err != nil {
			gotErr = true
		}
	}
	if !gotErr {
		t.Fatal("expected an error chunk for malformed frame; incomplete output must not become a normal completion")
	}
}

func TestStreamRejectsHTTPErrors(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "rate limited")
	})
	_, err := p.InvokeStream(context.Background(), core.Request{Messages: []core.Message{textMsg("user", "x")}})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 || !apiErr.Retryable() {
		t.Fatalf("err = %v, want retryable 429 APIError", err)
	}
}
