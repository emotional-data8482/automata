package openrouter

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

// runTask runs task on agent through an ephemeral Runtime, the only way an
// Agent executes. stream selects the live-view path.
func runTask(t *testing.T, agent *core.Agent, task string, stream bool) (core.RunResult, error) {
	t.Helper()
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	ref, err := runtime.Register("test", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		return runtime.RunStream(context.Background(), ref, task, nil)
	}
	return runtime.Run(context.Background(), ref, task)
}

// invokeOnly hides a provider's streaming method so a run exercises the
// non-streaming Invoke conversion.
type invokeOnly struct{ core.Provider }

// echoTool is the tool the scripted models call.
func echoTool() core.Tool {
	return core.Func("echo", "echo the query", func(_ context.Context, in struct {
		Q string `json:"q" desc:"the query"`
	}) (string, error) {
		return "echo: " + in.Q, nil
	})
}

func newAgent(p core.Provider) *core.Agent {
	agent, err := core.New(p, core.AgentConfig{Tools: []core.Tool{echoTool()}})
	if err != nil {
		panic(err)
	}
	return agent
}

// scriptedProvider serves canned responses in request order: first a tool
// call, then the final answer.
func scriptedProvider(t *testing.T, stream bool) *Provider {
	t.Helper()
	var calls atomic.Int32
	return newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			if stream {
				writeSSE(w,
					`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"vendor/model","choices":[{"index":0,"delta":{"content":"Checking.","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"q\":\"hi\"}"}}]},"finish_reason":null}]}`,
					`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"vendor/model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
				)
				return
			}
			writeJSON(w, `{"id":"r1","object":"chat.completion","created":1,"model":"vendor/model",
			  "choices":[{"index":0,"finish_reason":"tool_calls",
			    "message":{"role":"assistant","content":"Checking.",
			      "tool_calls":[{"id":"call_1","type":"function",
			        "function":{"name":"echo","arguments":"{\"q\":\"hi\"}"}}]}}],
			  "usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
			return
		}
		if stream {
			writeSSE(w,
				`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"vendor/model","choices":[{"index":0,"delta":{"content":"All done."},"finish_reason":null}]}`,
				`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"vendor/model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`,
			)
			return
		}
		writeJSON(w, `{"id":"r2","object":"chat.completion","created":1,"model":"vendor/model",
		  "choices":[{"index":0,"finish_reason":"stop",
		    "message":{"role":"assistant","content":"All done."}}],
		  "usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`)
	})
}

// assertToolLoop pins the shared outcome of both e2e paths: the model calls
// the echo tool, sees its result, then answers.
func assertToolLoop(t *testing.T, res core.RunResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "All done." {
		t.Errorf("output = %q, want All done.", res.Output)
	}
	if res.Status != core.RunCompleted {
		t.Errorf("status = %v, want completed", res.Status)
	}
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2", res.Turns)
	}
	// Usage accumulates across turns: (11,7) + (20,10).
	if res.Usage != (core.Usage{InputTokens: 31, OutputTokens: 17}) {
		t.Errorf("usage = %+v, want {31 17}", res.Usage)
	}
	// The transcript carries the tool call and its result.
	roles := ""
	for _, m := range res.Messages {
		roles += m.Role + ","
	}
	if !contains(roles, "assistant,tool,assistant") {
		t.Errorf("transcript roles = %s, want assistant,tool,assistant", roles)
	}
}

func TestRuntimeToolLoopStreams(t *testing.T) {
	p := scriptedProvider(t, true)
	res, err := runTask(t, newAgent(p), "run", true)
	assertToolLoop(t, res, err)
}

func TestRuntimeToolLoopNonStreaming(t *testing.T) {
	// Runtime always streams from a StreamProvider; wrapping the provider in
	// invokeOnly exercises the non-streaming Invoke conversion instead.
	p := scriptedProvider(t, false)
	res, err := runTask(t, newAgent(invokeOnly{p}), "run", false)
	assertToolLoop(t, res, err)
}

// TestRuntimeStreamErrorFailsRun pins that a mid-stream failure fails the run
// with the partial transcript preserved, and that the error text names the
// stream error.
func TestRuntimeStreamErrorFailsRun(t *testing.T) {
	p := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"vendor/model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"vendor/model","choices":[],"error":{"code":502,"message":"upstream broke"}}`,
		)
	})
	_, err := runTask(t, newAgent(p), "run", true)
	if err == nil {
		t.Fatal("expected run failure")
	}
	if !contains(err.Error(), "upstream broke") {
		t.Errorf("err = %v, want stream error message preserved", err)
	}
}
