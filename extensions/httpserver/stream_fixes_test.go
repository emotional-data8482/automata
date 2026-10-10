package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// pipeWriter uses a real deadline-aware connection whose peer never reads.
// It can stall any one of the initial flush, event write, or heartbeat.
type pipeWriter struct {
	conn    net.Conn
	header  http.Header
	phase   string
	blocked chan struct{}
	once    sync.Once
}

func (w *pipeWriter) Header() http.Header                 { return w.header }
func (*pipeWriter) WriteHeader(int)                       {}
func (w *pipeWriter) SetWriteDeadline(at time.Time) error { return w.conn.SetWriteDeadline(at) }
func (w *pipeWriter) stall(p []byte) (int, error) {
	w.once.Do(func() { close(w.blocked) })
	return w.conn.Write(p)
}
func (w *pipeWriter) Write(p []byte) (int, error) {
	if w.phase != "initial" {
		return w.stall(p)
	}
	return len(p), nil
}
func (w *pipeWriter) FlushError() error {
	if w.phase == "initial" {
		_, err := w.stall([]byte("headers"))
		return err
	}
	return nil
}

func TestStreamBlockedWritesReleaseSlot(t *testing.T) {
	for _, phase := range []string{"initial", "event", "heartbeat"} {
		for _, cancellation := range []bool{false, true} {
			name := phase + "/duration"
			if cancellation {
				name = phase + "/cancellation"
			}
			t.Run(name, func(t *testing.T) {
				runtime, err := core.NewEphemeralRuntime()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = runtime.Close() })
				config := Config{Runtime: runtime, Authorize: AllowAll, MaxStreams: 1, Heartbeat: 5 * time.Millisecond, MaxStreamDuration: 50 * time.Millisecond}
				if cancellation {
					config.MaxStreamDuration = 0
				}
				server, err := New(config)
				if err != nil {
					t.Fatal(err)
				}
				writerConn, peer := net.Pipe()
				t.Cleanup(func() { _ = writerConn.Close(); _ = peer.Close() })
				writer := &pipeWriter{conn: writerConn, header: http.Header{}, phase: phase, blocked: make(chan struct{})}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				request := httptest.NewRequest(http.MethodGet, "/v1/changes?stream=true", nil).WithContext(ctx)
				done := make(chan struct{})
				go func() {
					defer close(done)
					stream, ok := server.openStream(writer, request)
					if !ok {
						return
					}
					defer stream.close()
					if phase == "event" {
						stream.send("change", "1", map[string]string{"state": "ready"})
					} else if phase == "heartbeat" {
						_, _ = stream.wait(func(ctx context.Context) (any, error) { <-ctx.Done(); return nil, ctx.Err() })
					}
				}()
				select {
				case <-writer.blocked:
				case <-time.After(2 * time.Second):
					t.Fatal("write never blocked")
				}
				if got := len(server.streams); got != 1 {
					t.Fatalf("held slots = %d", got)
				}
				if cancellation {
					cancel()
				}
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("blocked write ignored stream end")
				}
				if got := len(server.streams); got != 0 {
					t.Fatalf("leaked %d stream slots", got)
				}
			})
		}
	}
}

func TestStreamRejectsWriterWithoutDeadlines(t *testing.T) {
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	server, err := New(Config{Runtime: runtime, Authorize: AllowAll, MaxStreams: 1})
	if err != nil {
		t.Fatal(err)
	}
	writer := httptest.NewRecorder()
	if _, ok := server.openStream(writer, httptest.NewRequest(http.MethodGet, "/v1/changes", nil)); ok {
		t.Fatal("accepted a writer without deadlines")
	}
	if writer.Code != http.StatusNotImplemented || len(server.streams) != 0 {
		t.Fatalf("status=%d slots=%d", writer.Code, len(server.streams))
	}
}

func TestEventStreamRejectsAheadAndPrunedCursors(t *testing.T) {
	h := newHarness(t, nil)
	run := h.submit(t, "hello", "cursor-fix")
	h.await(t, run.RunID)
	resp, data := h.do(t, http.MethodGet, "/v1/runs/"+run.RunID+"/events?stream=true&after=99999999", nil, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("ahead cursor = %d %s", resp.StatusCode, data)
	}
	if _, err := h.runtime.Prune(context.Background(), core.RetentionPolicy{Events: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	resp, data = h.do(t, http.MethodGet, "/v1/runs/"+run.RunID+"/events?stream=true", nil, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("pruned cursor = %d %s", resp.StatusCode, data)
	}
}

func TestEventStreamEndsAfterLateReconciliation(t *testing.T) {
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	charge := core.WithToolEffectPolicy(core.FuncResult("charge", "charge", func(context.Context, struct{}) (core.ToolResult, error) {
		return core.ToolResult{Effect: core.EffectReport{Status: core.EffectUnknown}}, nil
	}), core.ToolEffectPolicy{Kind: core.ToolEffectMutating})
	agent, err := core.New(chargeProvider{}, core.AgentConfig{Tools: []core.Tool{charge}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := runtime.Register("billing", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), ref, "charge")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := waitForState(t, runtime, handle.ID(), core.RuntimeNeedsAttention)
	if err := handle.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	operation := snapshot.ToolBatches[0].Invocations[0].OperationID
	if err := handle.Reconcile(context.Background(), operation, core.EffectResolution{Result: core.TextResult("settled"), Effect: core.EffectReport{Status: core.EffectNotApplied}}); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Runtime: runtime, Authorize: AllowAll})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	for _, afterTerminal := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		path := httpServer.URL + "/v1/runs/" + handle.ID() + "/events?stream=true&limit=1"
		if afterTerminal {
			page, err := handle.Events(context.Background(), 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range page.Events {
				if event.Kind == core.CommittedRunState && event.State == core.RuntimeTerminal {
					path += "&after=" + itoa(event.Sequence)
				}
			}
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		ended := false
		readSSE(t, resp.Body, func(event sseEvent) bool {
			ended = event.event == "end"
			return ended
		})
		_ = resp.Body.Close()
		if !ended || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			cancel()
			t.Fatal("post-terminal event stream did not end")
		}
		cancel()
	}
}
