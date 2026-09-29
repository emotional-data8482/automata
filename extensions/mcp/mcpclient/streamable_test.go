package mcpclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emotional-data8482/automata/extensions/mcp/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestStreamableFailures drives the streamable HTTP transport, which reports
// both network failures and a server's JSON-RPC answers on an HTTP error
// status as the SDK's "rejected by transport" error. Only the answer is
// recoverable.
func TestStreamableFailures(t *testing.T) {
	server := newServer(nil)
	addRaw(server, "echo", `{"type":"object"}`, echoArgs)
	var mode atomic.Value
	mode.Store("")
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case "unavailable":
			http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
		case "answer":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"bad arguments"}}`))
		case "drop":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		default:
			handler.ServeHTTP(w, r)
		}
	}))
	defer httpServer.Close()

	ctx := context.Background()
	client, err := mcpclient.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: httpServer.URL, HTTPClient: httpServer.Client(), MaxRetries: -1, DisableStandaloneSSE: true,
	}, mcpclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools, err := client.Tools(ctx, mcpclient.ToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	echo := tools[0]

	for _, m := range []string{"unavailable", "drop"} {
		mode.Store(m)
		if got, err := echo.Execute(ctx, nil); err == nil {
			t.Fatalf("%s: got recoverable %#v, want a fatal error", m, got)
		}
	}
	mode.Store("answer")
	got, err := echo.Execute(ctx, nil)
	if err != nil || !got.IsError || !strings.Contains(got.Text(), "-32602: bad arguments") {
		t.Fatalf("server answer: got %#v, %v", got, err)
	}
	// Rejections do not break the session.
	mode.Store("")
	if got, err := echo.Execute(ctx, nil); err != nil || got.Text() != "{}" {
		t.Fatalf("after recovery: got %#v, %v", got, err)
	}
}
