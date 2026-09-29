package mcpclient_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/mcp/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callOnce requests one tool call, then ends the run.
type callOnce struct {
	name  string
	turns atomic.Int32
}

func (p *callOnce) Invoke(context.Context, core.Request) (core.Response, error) {
	if p.turns.Add(1) == 1 {
		return core.Response{Message: core.AssistantMessage(core.ToolUseBlock{ID: "c1", Name: p.name, Input: json.RawMessage(`{}`)}), StopReason: core.StopToolUse}, nil
	}
	return core.Response{Message: core.AssistantMessage(core.TextBlock{Text: "done"}), StopReason: core.StopEndTurn}, nil
}

// TestProgressReachesRuntimeViews sends server progress notifications during
// a Runtime run and expects them as StreamToolProgress events for the call.
func TestProgressReachesRuntimeViews(t *testing.T) {
	steps := []*mcp.ProgressNotificationParams{{Progress: 1, Total: 2, Message: "half"}, {Progress: 2, Total: 2}}
	observed := make(chan struct{}, len(steps))
	var tokens []any
	server := newServer(nil)
	addRaw(server, "index", `{"type":"object"}`, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := req.Params.GetProgressToken()
		tokens = append(tokens, token)
		for _, step := range steps {
			step.ProgressToken = token
			if err := req.Session.NotifyProgress(ctx, step); err != nil {
				return nil, err
			}
			// The SDK may deliver a notification after the call's response,
			// when the report would be dropped; wait until it is seen.
			select {
			case <-observed:
			case <-time.After(5 * time.Second):
				t.Error("progress notification was not observed")
			}
		}
		return text("indexed"), nil
	})

	var forwarded atomic.Int32
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client, err := mcpclient.Connect(ctx, clientTransport, mcpclient.Options{ClientOptions: &mcp.ClientOptions{
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) { forwarded.Add(1) },
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools, err := client.Tools(ctx, mcpclient.ToolOptions{Prefix: "srv_"})
	if err != nil {
		t.Fatal(err)
	}

	agent, err := core.New(&callOnce{name: "srv_index"}, core.AgentConfig{Tools: tools, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ref, err := runtime.Register("indexer", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []core.StreamEvent
	if _, err := runtime.RunStream(ctx, ref, "go", func(ev core.StreamEvent) {
		if ev.Kind != core.StreamToolProgress && ev.Kind != core.StreamToolResult {
			return
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
		if ev.Kind == core.StreamToolProgress {
			observed <- struct{}{}
		}
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 || events[2].Kind != core.StreamToolResult {
		t.Fatalf("events = %+v, want two progress events then the result", events)
	}
	for i, step := range steps {
		ev := events[i]
		want := core.ToolProgress{Progress: step.Progress, Total: step.Total, Message: step.Message}
		if ev.Kind != core.StreamToolProgress || ev.ToolCall.ID != "c1" || ev.ToolCall.Name != "srv_index" || *ev.Progress != want {
			t.Fatalf("event %d = %+v (progress %+v), want %+v for call c1", i, ev, ev.Progress, want)
		}
	}
	if len(tokens) != 1 || tokens[0] == nil {
		t.Fatalf("progress tokens = %v, want one per call", tokens)
	}
	if forwarded.Load() != int32(len(steps)) {
		t.Fatalf("user handler saw %d notifications, want %d", forwarded.Load(), len(steps))
	}
}
