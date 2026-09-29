package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/mcp/mcpclient"
	"github.com/emotional-data8482/automata/extensions/mcp/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// scripted replays one response per provider turn and records requests.
type scripted struct {
	mu        sync.Mutex
	responses []core.Response
	requests  []core.Request
}

func (s *scripted) Invoke(_ context.Context, req core.Request) (core.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if len(s.responses) == 0 {
		return core.Response{}, fmt.Errorf("unexpected turn %d", len(s.requests))
	}
	next := s.responses[0]
	s.responses = s.responses[1:]
	return next, nil
}

func toolCall(id, name, input string) core.Response {
	return core.Response{
		Message:    core.AssistantMessage(core.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(input)}),
		StopReason: core.StopToolUse,
	}
}

// TestRuntimeRoundTrip serves a core tool over MCP, rediscovers it through
// mcpclient, and runs it in a Runtime tool loop: a success, a recoverable
// remote error the model sees, and a final answer.
func TestRuntimeRoundTrip(t *testing.T) {
	type addArgs struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	add := core.FuncResult("add", "Add two integers", func(_ context.Context, in addArgs) (core.ToolResult, error) {
		if in.B == 0 {
			return core.ErrorResult("b must be non-zero"), nil
		}
		return core.TextResult(fmt.Sprint(in.A + in.B)), nil
	})
	server := newServer()
	if err := mcpserver.AddTools(server, add); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client, err := mcpclient.Connect(ctx, clientTransport, mcpclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools, err := client.Tools(ctx, mcpclient.ToolOptions{Prefix: "calc_"})
	if err != nil {
		t.Fatal(err)
	}

	provider := &scripted{responses: []core.Response{
		toolCall("call_1", "calc_add", `{"a":2,"b":3}`),
		toolCall("call_2", "calc_add", `{"a":2,"b":0}`),
		{Message: core.AssistantMessage(core.TextBlock{Text: "2+3=5"}), StopReason: core.StopEndTurn},
	}}
	agent, err := core.New(provider, core.AgentConfig{Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ref, err := runtime.Register("calc", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(ctx, ref, "add 2 and 3")
	if err != nil {
		t.Fatalf("run: %v (result %+v)", err, result)
	}
	if result.Output != "2+3=5" {
		t.Fatalf("output = %q", result.Output)
	}

	var results []core.ToolResultBlock
	for _, message := range result.Messages {
		for _, block := range message.Blocks {
			if r, ok := block.(core.ToolResultBlock); ok {
				results = append(results, r)
			}
		}
	}
	want := []struct {
		id, text string
		isError  bool
	}{{"call_1", "5", false}, {"call_2", "b must be non-zero", true}}
	if len(results) != len(want) {
		t.Fatalf("tool results = %+v", results)
	}
	for i, w := range want {
		r := results[i]
		if r.ToolUseID != w.id || r.IsError != w.isError || (core.Message{Blocks: r.Content}).Text() != w.text {
			t.Fatalf("result %d = %+v, want %+v", i, r, w)
		}
	}
	if defs := provider.requests[0].Tools; len(defs) != 1 || defs[0].Name != "calc_add" {
		t.Fatalf("advertised tools = %+v", defs)
	}
}
