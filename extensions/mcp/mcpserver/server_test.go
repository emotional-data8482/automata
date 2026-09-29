package mcpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/mcp/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type stubTool struct {
	definition core.ToolDefinition
	execute    func(context.Context, json.RawMessage) (core.ToolResult, error)
	calls      atomic.Int32
}

func (s *stubTool) Definition() core.ToolDefinition { return s.definition }
func (s *stubTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolResult, error) {
	s.calls.Add(1)
	return s.execute(ctx, args)
}

func stub(name, schema string, execute func(context.Context, json.RawMessage) (core.ToolResult, error)) *stubTool {
	return &stubTool{definition: core.ToolDefinition{Name: name, Description: name + " tool", InputSchema: json.RawMessage(schema)}, execute: execute}
}

func returns(result core.ToolResult, err error) func(context.Context, json.RawMessage) (core.ToolResult, error) {
	return func(context.Context, json.RawMessage) (core.ToolResult, error) { return result, err }
}

func newServer() *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
}

// connect returns an SDK client session to server over in-memory transports.
func connect(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "v1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestAddToolsValidatesBeforeRegistering(t *testing.T) {
	ok := stub("ok", `{"type":"object"}`, returns(core.TextResult("ok"), nil))
	var typedNil *stubTool
	cases := []struct {
		name  string
		tools []core.Tool
		want  string
	}{
		{"nil tool", []core.Tool{ok, nil}, "nil tool"},
		{"typed nil tool", []core.Tool{ok, typedNil}, "nil tool"},
		{"empty name", []core.Tool{ok, stub("", `{"type":"object"}`, nil)}, "name is empty"},
		{"duplicate", []core.Tool{ok, stub("ok", `{"type":"object"}`, nil)}, `duplicate tool "ok"`},
		{"non-object schema", []core.Tool{ok, stub("s", `{"type":"string"}`, nil)}, "object schema"},
		{"malformed schema", []core.Tool{ok, stub("m", `{"type":`, nil)}, `tool "m"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer()
			if err := mcpserver.AddTools(server, tc.tools...); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			result, err := connect(t, server).ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Tools) != 0 {
				t.Fatalf("registered %d tools after a failed AddTools", len(result.Tools))
			}
		})
	}
	if err := mcpserver.AddTools(nil, ok); err == nil {
		t.Fatal("nil server accepted")
	}
}

func TestServedDefinitions(t *testing.T) {
	type args struct {
		Path  string `json:"path" desc:"file to read"`
		Limit *int   `json:"limit,omitempty"`
	}
	read := core.Func("read", "Read a file", func(context.Context, args) (string, error) { return "", nil })
	server := newServer()
	if err := mcpserver.AddTools(server, read); err != nil {
		t.Fatal(err)
	}
	result, err := connect(t, server).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tools) != 1 {
		t.Fatalf("tools = %v", result.Tools)
	}
	got := result.Tools[0]
	if got.Name != "read" || got.Description != "Read a file" {
		t.Fatalf("tool = %+v", got)
	}
	served, _ := json.Marshal(got.InputSchema)
	var gotSchema, wantSchema any
	_ = json.Unmarshal(served, &gotSchema)
	_ = json.Unmarshal(read.Definition().InputSchema, &wantSchema)
	if a, b := canonical(gotSchema), canonical(wantSchema); a != b {
		t.Fatalf("served schema %s, want %s", a, b)
	}
}

func canonical(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}

func TestCallTool(t *testing.T) {
	schema := `{"type":"object","properties":{"n":{"type":"integer","minimum":0}},"required":["n"],"additionalProperties":false}`
	var received json.RawMessage
	echo := stub("echo", schema, func(_ context.Context, args json.RawMessage) (core.ToolResult, error) {
		received = args
		return core.TextResult("ok"), nil
	})
	rich := stub("rich", `{"type":"object"}`, returns(core.BlockResult(
		core.TextBlock{Text: "see"},
		core.ImageBlock{MediaType: "image/png", Data: []byte("png")},
		core.ImageBlock{URL: "https://example.com/a.png"},
		core.RawBlock{Provider: "claude", Type: "redacted_thinking"},
		core.ThinkingBlock{Thinking: "hmm"},
	), nil))
	recoverable := stub("recoverable", `{"type":"object"}`, returns(core.ErrorResult("not found"), nil))
	fatal := stub("fatal", `{"type":"object"}`, returns(core.ToolResult{}, errors.New("disk failure")))
	empty := stub("empty", `{"type":"object"}`, returns(core.ToolResult{}, nil))
	server := newServer()
	if err := mcpserver.AddTools(server, echo, rich, recoverable, fatal, empty); err != nil {
		t.Fatal(err)
	}
	session := connect(t, server)
	ctx := context.Background()
	call := func(name string, args any) (*mcp.CallToolResult, error) {
		return session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	}
	textOf := func(result *mcp.CallToolResult) string {
		var b strings.Builder
		for _, c := range result.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}

	t.Run("valid arguments execute", func(t *testing.T) {
		result, err := call("echo", map[string]any{"n": 3})
		if err != nil || result.IsError || textOf(result) != "ok" || string(received) != `{"n":3}` {
			t.Fatalf("result %+v, err %v, received %s", result, err, received)
		}
	})
	t.Run("invalid arguments are recoverable and skip execution", func(t *testing.T) {
		before := echo.calls.Load()
		for _, args := range []any{map[string]any{}, map[string]any{"n": "3"}, map[string]any{"n": -1}, map[string]any{"n": 1, "x": 2}, map[string]any{"n": 1.5}} {
			result, err := call("echo", args)
			if err != nil || !result.IsError || !strings.HasPrefix(textOf(result), "invalid arguments: ") {
				t.Fatalf("args %v: result %+v, err %v", args, result, err)
			}
		}
		if echo.calls.Load() != before {
			t.Fatal("tool executed with invalid arguments")
		}
	})
	t.Run("rich blocks convert in order", func(t *testing.T) {
		result, err := call("rich", nil)
		if err != nil || len(result.Content) != 5 {
			t.Fatalf("result %+v, err %v", result, err)
		}
		if c, ok := result.Content[0].(*mcp.TextContent); !ok || c.Text != "see" {
			t.Fatalf("content[0] = %#v", result.Content[0])
		}
		if c, ok := result.Content[1].(*mcp.ImageContent); !ok || c.MIMEType != "image/png" || string(c.Data) != "png" {
			t.Fatalf("content[1] = %#v", result.Content[1])
		}
		if c, ok := result.Content[2].(*mcp.ResourceLink); !ok || c.URI != "https://example.com/a.png" {
			t.Fatalf("content[2] = %#v", result.Content[2])
		}
		if c, ok := result.Content[3].(*mcp.TextContent); !ok || c.Text != "[unsupported tool result block omitted: raw claude/redacted_thinking]" {
			t.Fatalf("content[3] = %#v", result.Content[3])
		}
		if c, ok := result.Content[4].(*mcp.TextContent); !ok || c.Text != "[unsupported tool result block omitted: core.ThinkingBlock]" {
			t.Fatalf("content[4] = %#v", result.Content[4])
		}
	})
	t.Run("error results set isError", func(t *testing.T) {
		result, err := call("recoverable", nil)
		if err != nil || !result.IsError || textOf(result) != "not found" {
			t.Fatalf("result %+v, err %v", result, err)
		}
	})
	t.Run("Go errors are JSON-RPC internal errors", func(t *testing.T) {
		_, err := call("fatal", nil)
		var wire *jsonrpc.Error
		if !errors.As(err, &wire) || wire.Code != jsonrpc.CodeInternalError || wire.Message != "disk failure" {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("empty results send one empty text item", func(t *testing.T) {
		result, err := call("empty", nil)
		if err != nil || len(result.Content) != 1 || textOf(result) != "" {
			t.Fatalf("result %+v, err %v", result, err)
		}
	})
}

func TestCallToolCancellation(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	slow := stub("slow", `{"type":"object"}`, func(ctx context.Context, _ json.RawMessage) (core.ToolResult, error) {
		close(started)
		<-ctx.Done()
		cancelled <- ctx.Err()
		return core.ToolResult{}, ctx.Err()
	})
	server := newServer()
	if err := mcpserver.AddTools(server, slow); err != nil {
		t.Fatal(err)
	}
	session := connect(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "slow"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("tool saw %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tool context was not cancelled")
	}
}
