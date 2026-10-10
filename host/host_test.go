package host

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/httpserver"
)

// fakeProvider calls the ask tool for tasks containing "ask", answers "done"
// after a tool result, and "ok" otherwise.
type fakeProvider struct{}

func (fakeProvider) Invoke(_ context.Context, req core.Request) (core.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	for _, block := range last.Blocks {
		if _, ok := block.(core.ToolResultBlock); ok {
			return core.Response{Message: core.AssistantMessage(core.TextBlock{Text: "done"}), StopReason: core.StopEndTurn}, nil
		}
	}
	if strings.Contains(last.Text(), "ask") {
		return core.Response{
			Message:    core.AssistantMessage(core.ToolUseBlock{ID: "q1", Name: "ask", Input: json.RawMessage(`{}`)}),
			StopReason: core.StopToolUse,
		}, nil
	}
	return core.Response{Message: core.AssistantMessage(core.TextBlock{Text: "ok"}), StopReason: core.StopEndTurn}, nil
}

func register(rt *core.Runtime) error {
	ask := core.WithDurableWait(core.Func("ask", "ask a human", func(context.Context, struct{}) (string, error) {
		return "unused", nil
	}), core.DurableWaitPolicy{Kind: core.WaitQuestion})
	agent, err := core.New(fakeProvider{}, core.AgentConfig{Tools: []core.Tool{ask}})
	if err != nil {
		return err
	}
	_, err = rt.Register("agent", "v1", agent)
	return err
}

type running struct {
	url  string
	stop context.CancelFunc
	done chan error
}

func start(t *testing.T, opts Options) *running {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opts.Listener = listener
	ready := make(chan net.Addr, 1)
	opts.Ready = func(addr net.Addr) { ready <- addr }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	select {
	case addr := <-ready:
		return &running{url: "http://" + addr.String(), stop: cancel, done: done}
	case err := <-done:
		cancel()
		t.Fatalf("host did not start: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("host did not start")
	}
	return nil
}

func (r *running) shutdown(t *testing.T) {
	t.Helper()
	r.stop()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("host stopped with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host did not stop")
	}
}

func call(t *testing.T, method, url, token string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestRunRequiresRegisterAndAuthorization(t *testing.T) {
	t.Setenv("AUTOMATA_TOKEN", "")
	if err := Run(context.Background(), Options{}); err == nil {
		t.Fatal("Run started without Register")
	}
	if err := Run(context.Background(), Options{Register: register}); err == nil || !strings.Contains(err.Error(), "AUTOMATA_TOKEN") {
		t.Fatalf("Run without authorization = %v", err)
	}
}

// A run waiting on a person survives a host restart on the same SQLite
// store: the new host recovers it, the answer arrives over HTTP, and the run
// finishes without repeating its committed turn.
func TestHostRecoversWaitingRunAcrossRestart(t *testing.T) {
	t.Setenv("AUTOMATA_TOKEN", "secret")
	opts := Options{
		Register:     register,
		DatabasePath: filepath.Join(t.TempDir(), "runtime.db"),
		Retention:    &core.RetentionPolicy{Events: time.Hour},
	}
	first := start(t, opts)
	if status, _ := call(t, http.MethodGet, first.url+"/healthz", "", nil); status != http.StatusNoContent {
		t.Fatalf("healthz = %d", status)
	}
	submit := httpserver.SubmitRequest{Definition: httpserver.DefinitionView{ID: "agent", Revision: "v1"}, Task: "please ask", IdempotencyKey: "ticket-1"}
	if status, _ := call(t, http.MethodPost, first.url+"/v1/runs", "wrong", submit); status != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d", status)
	}
	status, data := call(t, http.MethodPost, first.url+"/v1/runs", "secret", submit)
	if status != http.StatusAccepted {
		t.Fatalf("submit = %d %s", status, data)
	}
	var run httpserver.RunView
	if err := json.Unmarshal(data, &run); err != nil {
		t.Fatal(err)
	}
	var waiting httpserver.RunView
	deadline := time.Now().Add(5 * time.Second)
	for waiting.State != core.RuntimeWaiting {
		if time.Now().After(deadline) {
			t.Fatalf("run never waited: %+v", waiting)
		}
		time.Sleep(10 * time.Millisecond)
		_, data = call(t, http.MethodGet, first.url+"/v1/runs/"+run.RunID, "secret", nil)
		waiting = httpserver.RunView{}
		_ = json.Unmarshal(data, &waiting)
	}
	first.shutdown(t)

	second := start(t, opts)
	defer second.shutdown(t)
	status, data = call(t, http.MethodPost, second.url+"/v1/runs", "secret", submit)
	var retry httpserver.RunView
	_ = json.Unmarshal(data, &retry)
	if status != http.StatusAccepted || retry.RunID != run.RunID || retry.State != core.RuntimeWaiting {
		t.Fatalf("retry after restart = %d %+v", status, retry)
	}
	status, data = call(t, http.MethodPost, second.url+"/v1/runs/"+run.RunID+"/waits/"+waiting.Waits[0].ID, "secret",
		httpserver.ResolveWaitRequest{Answer: json.RawMessage(`"yes"`)})
	if status != http.StatusOK {
		t.Fatalf("resolve = %d %s", status, data)
	}
	var final httpserver.RunView
	deadline = time.Now().Add(5 * time.Second)
	for final.State != core.RuntimeTerminal {
		if time.Now().After(deadline) {
			t.Fatalf("run never finished: %+v", final)
		}
		time.Sleep(10 * time.Millisecond)
		_, data = call(t, http.MethodGet, second.url+"/v1/runs/"+run.RunID, "secret", nil)
		final = httpserver.RunView{}
		_ = json.Unmarshal(data, &final)
	}
	if final.Status != core.RunCompleted || final.Turns != 2 {
		t.Fatalf("final = %+v", final)
	}
}

// Stopping the host ends open event streams instead of waiting for them.
func TestShutdownEndsStreams(t *testing.T) {
	opts := Options{
		Register:        register,
		Store:           func(context.Context) (core.Store, error) { return core.NewMemoryStore(), nil },
		Server:          httpserver.Config{Authorize: httpserver.AllowAll},
		ShutdownTimeout: 5 * time.Second,
	}
	host := start(t, opts)
	req, _ := http.NewRequest(http.MethodGet, host.url+"/v1/changes?stream=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	started := time.Now()
	host.shutdown(t)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("shutdown waited %s for an open stream", elapsed)
	}
}
