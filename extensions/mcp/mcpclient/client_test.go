package mcpclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/mcp/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect serves server over in-memory transports and returns a connected
// client, closed at cleanup.
func connect(t *testing.T, server *mcp.Server) *mcpclient.Client {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	client, err := mcpclient.Connect(ctx, clientTransport, mcpclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func newServer(opts *mcp.ServerOptions) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v1"}, opts)
}

func addRaw(server *mcp.Server, name, schema string, h mcp.ToolHandler) {
	server.AddTool(&mcp.Tool{Name: name, Description: name + " tool", InputSchema: json.RawMessage(schema)}, h)
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// echoArgs returns the raw arguments the server received.
func echoArgs(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return text(string(req.Params.Arguments)), nil
}

func toolNamed(t *testing.T, tools []core.Tool, name string) core.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Definition().Name == name {
			return tool
		}
	}
	t.Fatalf("no tool %q", name)
	return nil
}

func TestToolsDiscovery(t *testing.T) {
	server := newServer(&mcp.ServerOptions{PageSize: 1}) // forces pagination
	addRaw(server, "search", `{"type":"object","properties":{"q":{"type":"string"},
		"limit":{"anyOf":[{"type":"integer"},{"type":"null"}],"default":null}},"required":["q"]}`, echoArgs)
	addRaw(server, "delete", `{"type":"object","properties":{"id":{"type":"string"}}}`, echoArgs)
	addRaw(server, "stat", `{"type":"object"}`, echoArgs)
	client := connect(t, server)
	ctx := context.Background()

	tools, err := client.Tools(ctx, mcpclient.ToolOptions{Prefix: "srv_"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Definition().Name)
	}
	slices.Sort(names)
	if want := []string{"srv_delete", "srv_search", "srv_stat"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	search := toolNamed(t, tools, "srv_search").Definition()
	if search.Description != "search tool" {
		t.Fatalf("description = %q", search.Description)
	}
	var schema map[string]any
	if err := json.Unmarshal(search.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	limit := schema["properties"].(map[string]any)["limit"].(map[string]any)
	if !slices.Equal(limit["type"].([]any), []any{"integer", "null"}) {
		t.Fatalf("limit schema = %v, want a lowered nullable integer", limit)
	}
	// The lowered tools register with core.
	if _, err := core.New(noProvider{}, core.AgentConfig{Tools: tools}); err != nil {
		t.Fatal(err)
	}

	only, err := client.Tools(ctx, mcpclient.ToolOptions{Include: []string{"stat"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Definition().Name != "stat" {
		t.Fatalf("include = %v", only)
	}
	if _, err := client.Tools(ctx, mcpclient.ToolOptions{Include: []string{"stat", "rm", "mv"}}); err == nil ||
		!strings.Contains(err.Error(), `["mv" "rm"]`) {
		t.Fatalf("missing include err = %v", err)
	}
}

func TestToolsRejectsUnusableSchema(t *testing.T) {
	server := newServer(nil)
	addRaw(server, "ok", `{"type":"object"}`, echoArgs)
	addRaw(server, "deep", `{"type":"object","properties":{"p":`+strings.Repeat(`{"type":"array","items":`, 80)+`{}`+strings.Repeat(`}`, 80)+`}}`, echoArgs)
	client := connect(t, server)
	if _, err := client.Tools(context.Background(), mcpclient.ToolOptions{}); err == nil || !strings.Contains(err.Error(), `tool "deep"`) {
		t.Fatalf("err = %v, want the unusable tool named", err)
	}
	if _, err := client.Tools(context.Background(), mcpclient.ToolOptions{Include: []string{"ok"}}); err != nil {
		t.Fatalf("excluding the unusable tool: %v", err)
	}
}

func TestExecute(t *testing.T) {
	server := newServer(nil)
	addRaw(server, "echo", `{"type":"object"}`, echoArgs)
	addRaw(server, "rich", `{"type":"object"}`, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "chart"}, &mcp.ImageContent{MIMEType: "image/png", Data: []byte("png")}}}, nil
	})
	addRaw(server, "fails", `{"type":"object"}`, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "quota exceeded"}}, IsError: true}, nil
	})
	addRaw(server, "breaks", `{"type":"object"}`, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("database unavailable")
	})
	addRaw(server, "gone", `{"type":"object"}`, echoArgs)
	client := connect(t, server)
	ctx := context.Background()
	tools, err := client.Tools(ctx, mcpclient.ToolOptions{Prefix: "p_"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("arguments pass through under the server's name", func(t *testing.T) {
		got, err := toolNamed(t, tools, "p_echo").Execute(ctx, json.RawMessage(`{"a":[1,2]}`))
		if err != nil || got.IsError || got.Text() != `{"a":[1,2]}` {
			t.Fatalf("got %#v, %v", got, err)
		}
	})
	t.Run("absent arguments are sent as an empty object", func(t *testing.T) {
		for _, args := range []json.RawMessage{nil, json.RawMessage("null")} {
			got, err := toolNamed(t, tools, "p_echo").Execute(ctx, args)
			if err != nil || got.Text() != `{}` {
				t.Fatalf("args %q: got %#v, %v", args, got, err)
			}
		}
	})
	t.Run("rich content keeps order", func(t *testing.T) {
		got, err := toolNamed(t, tools, "p_rich").Execute(ctx, nil)
		want := core.Blocks{core.TextBlock{Text: "chart"}, core.ImageBlock{MediaType: "image/png", Data: []byte("png")}}
		if err != nil || !slices.EqualFunc(got.Blocks, want, func(a, b core.Block) bool {
			ai, aok := a.(core.ImageBlock)
			bi, bok := b.(core.ImageBlock)
			if aok && bok {
				return ai.MediaType == bi.MediaType && string(ai.Data) == string(bi.Data)
			}
			return a == b
		}) {
			t.Fatalf("got %#v, %v", got, err)
		}
	})
	t.Run("isError results are recoverable", func(t *testing.T) {
		got, err := toolNamed(t, tools, "p_fails").Execute(ctx, nil)
		if err != nil || !got.IsError || got.Text() != "quota exceeded" {
			t.Fatalf("got %#v, %v", got, err)
		}
	})
	t.Run("server JSON-RPC errors are recoverable", func(t *testing.T) {
		got, err := toolNamed(t, tools, "p_breaks").Execute(ctx, nil)
		if err != nil || !got.IsError || !strings.Contains(got.Text(), "database unavailable") {
			t.Fatalf("got %#v, %v", got, err)
		}
		server.RemoveTools("gone")
		got, err = toolNamed(t, tools, "p_gone").Execute(ctx, nil)
		if err != nil || !got.IsError || !strings.Contains(got.Text(), `unknown tool "gone"`) {
			t.Fatalf("removed tool: got %#v, %v", got, err)
		}
	})
}

func TestExecuteCancellation(t *testing.T) {
	server := newServer(nil)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	addRaw(server, "slow", `{"type":"object"}`, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	client := connect(t, server)
	tools, err := client.Tools(context.Background(), mcpclient.ToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := tools[0].Execute(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("server handler was not cancelled")
	}
}

func TestExecuteAfterCloseIsFatal(t *testing.T) {
	server := newServer(nil)
	addRaw(server, "echo", `{"type":"object"}`, echoArgs)
	client := connect(t, server)
	tools, err := client.Tools(context.Background(), mcpclient.ToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := tools[0].Execute(context.Background(), nil); err == nil {
		t.Fatalf("got %#v with nil error, want a fatal error", got)
	}
}

// severable records the server side of a transport so a test can cut the
// connection underneath a live session, as a crashed server process would.
type severable struct {
	mcp.Transport
	conn chan mcp.Connection
}

func (s *severable) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := s.Transport.Connect(ctx)
	if err == nil {
		s.conn <- conn
	}
	return conn, err
}

// TestServerLostMidCallIsFatal pins that the SDK's connection-failure wire
// errors are not mistaken for recoverable answers from the server.
func TestServerLostMidCallIsFatal(t *testing.T) {
	server := newServer(nil)
	started := make(chan struct{})
	addRaw(server, "hang", `{"type":"object"}`, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	cut := &severable{Transport: serverTransport, conn: make(chan mcp.Connection, 1)}
	serverSession, err := server.Connect(ctx, cut, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client, err := mcpclient.Connect(ctx, clientTransport, mcpclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools, err := client.Tools(ctx, mcpclient.ToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	conn := <-cut.conn
	go func() {
		<-started
		_ = conn.Close()
	}()
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	got, err := tools[0].Execute(callCtx, nil)
	if callCtx.Err() != nil {
		t.Fatal("call did not observe the lost connection")
	}
	if err == nil {
		t.Fatalf("got recoverable %#v, want a fatal error", got)
	}
	// Later calls on the dead connection fail inside the SDK with its
	// "client is closing" wire error, which must stay fatal too.
	if got, err := tools[0].Execute(ctx, nil); err == nil {
		t.Fatalf("call after the connection dropped: got recoverable %#v", got)
	}
}

func TestConcurrentCalls(t *testing.T) {
	server := newServer(nil)
	addRaw(server, "echo", `{"type":"object"}`, echoArgs)
	client := connect(t, server)
	tools, err := client.Tools(context.Background(), mcpclient.ToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			args := json.RawMessage(`{"i":"` + string(rune('a'+i)) + `"}`)
			got, err := tools[0].Execute(context.Background(), args)
			if err != nil || got.Text() != string(args) {
				t.Errorf("call %d: got %q, %v", i, got.Text(), err)
			}
		})
	}
	wg.Wait()
}

type noProvider struct{}

func (noProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	return core.Response{}, errors.New("not called")
}
