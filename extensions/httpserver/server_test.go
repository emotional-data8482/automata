package httpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// fakeProvider answers "done" after a tool result, calls the ask tool for
// tasks containing "ask", and otherwise answers "secret answer".
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
	return core.Response{Message: core.AssistantMessage(core.TextBlock{Text: "secret answer"}), StopReason: core.StopEndTurn}, nil
}

type harness struct {
	runtime *core.Runtime
	server  *httptest.Server
	ref     core.DefinitionRef
}

func newHarness(t *testing.T, configure func(*Config)) *harness {
	t.Helper()
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	ask := core.WithDurableWait(core.Func("ask", "ask a human", func(context.Context, struct{}) (string, error) {
		return "unused", nil
	}), core.DurableWaitPolicy{Kind: core.WaitQuestion, Prompt: func(json.RawMessage) (string, error) { return "secret prompt", nil }})
	agent, err := core.New(fakeProvider{}, core.AgentConfig{SystemPrompt: "secret system", Tools: []core.Tool{ask}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := runtime.Register("agent", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Runtime: runtime, Authorize: AllowAll, Heartbeat: 50 * time.Millisecond}
	if configure != nil {
		configure(&config)
	}
	server, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return &harness{runtime: runtime, server: httpServer, ref: ref}
}

func (h *harness) do(t *testing.T, method, path string, body any, header http.Header) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	for key, values := range header {
		req.Header[key] = values
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func decodeInto[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return value
}

func (h *harness) submit(t *testing.T, task, key string) RunView {
	t.Helper()
	resp, data := h.do(t, http.MethodPost, "/v1/runs", SubmitRequest{
		Definition: DefinitionView{ID: "agent", Revision: "v1"}, Task: task, IdempotencyKey: key,
		Labels: map[string]string{"source": "test"},
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit = %d %s", resp.StatusCode, data)
	}
	return decodeInto[RunView](t, data)
}

func (h *harness) await(t *testing.T, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = h.runtime.Handle(runID).Await(ctx)
}

func TestNewRequiresRuntimeAndAuthorize(t *testing.T) {
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := New(Config{Authorize: AllowAll}); err == nil {
		t.Fatal("New accepted a nil Runtime")
	}
	if _, err := New(Config{Runtime: runtime}); err == nil {
		t.Fatal("New accepted a nil Authorize")
	}
}

func TestSubmitStatusAndRedaction(t *testing.T) {
	h := newHarness(t, nil)
	run := h.submit(t, "hello", "k1")
	if run.RunID == "" || run.Labels["source"] != "test" || run.Key != "k1" {
		t.Fatalf("submitted = %+v", run)
	}
	retry := h.submit(t, "hello", "k1")
	if retry.RunID != run.RunID {
		t.Fatalf("idempotent retry admitted %s, want %s", retry.RunID, run.RunID)
	}
	resp, data := h.do(t, http.MethodPost, "/v1/runs", SubmitRequest{
		Definition: DefinitionView{ID: "agent", Revision: "v1"}, Task: "different", IdempotencyKey: "k1",
	}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting retry = %d %s", resp.StatusCode, data)
	}
	h.await(t, run.RunID)

	resp, data = h.do(t, http.MethodGet, "/v1/runs/"+run.RunID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d %s", resp.StatusCode, data)
	}
	status := decodeInto[RunView](t, data)
	if status.State != core.RuntimeTerminal || status.Status != core.RunCompleted || status.Output != "" || status.Messages != nil {
		t.Fatalf("redacted status = %+v", status)
	}
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "hello") {
		t.Fatalf("status leaked content: %s", data)
	}
	_, data = h.do(t, http.MethodGet, "/v1/runs/"+run.RunID+"/events", nil, nil)
	if strings.Contains(string(data), "secret") {
		t.Fatalf("events leaked content: %s", data)
	}
	_, data = h.do(t, http.MethodGet, "/v1/definitions/agent/v1", nil, nil)
	if strings.Contains(string(data), "secret system") || !strings.Contains(string(data), `"ask"`) {
		t.Fatalf("manifest = %s", data)
	}

	exposed := newHarness(t, func(c *Config) { c.ExposeContent = true })
	run = exposed.submit(t, "hello", "")
	exposed.await(t, run.RunID)
	_, data = exposed.do(t, http.MethodGet, "/v1/runs/"+run.RunID, nil, nil)
	if status := decodeInto[RunView](t, data); status.Output != "secret answer" || len(status.Messages) == 0 {
		t.Fatalf("exposed status = %+v", status)
	}

	for path, want := range map[string]int{
		"/v1/runs/missing":            http.StatusNotFound,
		"/v1/definitions/agent/v9":    http.StatusNotFound,
		"/v1/runs/missing/events":     http.StatusNotFound,
		"/v1/runs?limit=-1":           http.StatusBadRequest,
		"/v1/runs?label=nocolon":      http.StatusBadRequest,
		"/v1/changes?after=999999999": http.StatusGone,
	} {
		if resp, data := h.do(t, http.MethodGet, path, nil, nil); resp.StatusCode != want {
			t.Errorf("GET %s = %d %s, want %d", path, resp.StatusCode, data, want)
		}
	}
	for name, body := range map[string]string{
		"unknown field":  `{"definition":{"id":"agent","revision":"v1"},"task":"x","nope":1}`,
		"missing task":   `{"definition":{"id":"agent","revision":"v1"}}`,
		"trailing value": `{"definition":{"id":"agent","revision":"v1"},"task":"x"} {}`,
	} {
		resp, err := http.Post(h.server.URL+"/v1/runs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d", name, resp.StatusCode)
		}
	}
}

func TestBodyLimitAndAuthorization(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.MaxBodyBytes = 64
		c.Authorize = func(r *http.Request, req Request) error {
			switch r.Header.Get("Authorization") {
			case "":
				return ErrUnauthenticated
			case "reader":
				if req.Action != ActionRead && req.Action != ActionList {
					return errors.New("read only")
				}
			}
			return nil
		}
	})
	body := SubmitRequest{Definition: DefinitionView{ID: "agent", Revision: "v1"}, Task: "go"}
	if resp, _ := h.do(t, http.MethodPost, "/v1/runs", body, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous submit = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/v1/runs", body, http.Header{"Authorization": {"reader"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("reader submit = %d", resp.StatusCode)
	}
	body.Task = strings.Repeat("x", 100)
	if resp, _ := h.do(t, http.MethodPost, "/v1/runs", body, http.Header{"Authorization": {"admin"}}); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized submit = %d", resp.StatusCode)
	}
}

// A tenant sees only runs admitted under its scope, including their child
// runs, and never the runtime-wide feed.
func TestScopeIsolatesTenants(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Scope = func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil }
		c.Labels = func(r *http.Request) map[string]string { return map[string]string{"source": "server"} }
	})
	tenantA := http.Header{"X-Tenant": {"a"}}
	tenantB := http.Header{"X-Tenant": {"b"}}
	resp, data := h.do(t, http.MethodPost, "/v1/runs", SubmitRequest{
		Definition: DefinitionView{ID: "agent", Revision: "v1"}, Task: "hello", Labels: map[string]string{"source": "client"},
	}, tenantA)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit = %d %s", resp.StatusCode, data)
	}
	run := decodeInto[RunView](t, data)
	if run.Key == "" || run.Labels["source"] != "server" {
		t.Fatalf("scoped submission = %+v", run)
	}
	if resp, _ := h.do(t, http.MethodGet, "/v1/runs/"+run.RunID, nil, tenantB); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other tenant read = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/v1/runs/"+run.RunID+"/cancel", nil, tenantB); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other tenant cancel = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodGet, "/v1/runs/"+run.RunID, nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unscoped read = %d", resp.StatusCode)
	}
	_, data = h.do(t, http.MethodGet, "/v1/runs?scope=a", nil, tenantB)
	if page := decodeInto[RunPageView](t, data); len(page.Runs) != 0 {
		t.Fatalf("tenant b listed %+v", page.Runs)
	}
	_, data = h.do(t, http.MethodGet, "/v1/runs", nil, tenantA)
	if page := decodeInto[RunPageView](t, data); len(page.Runs) != 1 || page.Runs[0].RunID != run.RunID {
		t.Fatalf("tenant a listed %+v", page.Runs)
	}
	if resp, _ := h.do(t, http.MethodGet, "/v1/changes", nil, tenantA); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("scoped change feed = %d", resp.StatusCode)
	}
}

type sseEvent struct {
	id, event, data string
}

// readSSE reads events until stop returns true or the stream ends.
func readSSE(t *testing.T, body io.Reader, stop func(sseEvent) bool) []sseEvent {
	t.Helper()
	var events []sseEvent
	var current sseEvent
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if current.event != "" {
				events = append(events, current)
				if stop(current) {
					return events
				}
			}
			current = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			current.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			current.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			current.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return events
}

func (h *harness) stream(t *testing.T, path, lastEventID string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("stream %s = %d %s", path, resp.StatusCode, data)
	}
	return resp
}

// A client streams a waiting run's events, disconnects, the run is answered
// and resumes, and the reconnected stream delivers exactly the remaining
// events and ends after the terminal state.
func TestEventStreamResumesExactlyAndEnds(t *testing.T) {
	h := newHarness(t, nil)
	run := h.submit(t, "please ask", "")

	resp := h.stream(t, "/v1/runs/"+run.RunID+"/events", "")
	first := readSSE(t, resp.Body, func(e sseEvent) bool {
		return e.event == string(core.CommittedRunState) && strings.Contains(e.data, `"state":"waiting"`)
	})
	resp.Body.Close()
	lastID := first[len(first)-1].id

	_, data := h.do(t, http.MethodGet, "/v1/runs/"+run.RunID, nil, nil)
	status := decodeInto[RunView](t, data)
	if len(status.Waits) != 1 || status.Waits[0].Prompt != "" {
		t.Fatalf("waits = %+v", status.Waits)
	}
	resp2, data := h.do(t, http.MethodPost, "/v1/runs/"+run.RunID+"/waits/"+status.Waits[0].ID, ResolveWaitRequest{Answer: json.RawMessage(`"yes"`)}, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("resolve = %d %s", resp2.StatusCode, data)
	}
	resp2, _ = h.do(t, http.MethodPost, "/v1/runs/"+run.RunID+"/waits/"+status.Waits[0].ID, ResolveWaitRequest{Answer: json.RawMessage(`"no"`)}, nil)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting answer = %d", resp2.StatusCode)
	}
	h.await(t, run.RunID)

	resp = h.stream(t, "/v1/runs/"+run.RunID+"/events", lastID)
	rest := readSSE(t, resp.Body, func(sseEvent) bool { return false })
	resp.Body.Close()
	if rest[len(rest)-1].event != "end" {
		t.Fatalf("stream did not end after terminal state: %+v", rest)
	}
	_, data = h.do(t, http.MethodGet, "/v1/runs/"+run.RunID+"/events?limit=1000", nil, nil)
	all := decodeInto[EventPageView](t, data)
	ids := []string{}
	for _, event := range append(first, rest...) {
		if event.id != "" {
			ids = append(ids, event.id)
		}
	}
	if len(ids) != len(all.Events) {
		t.Fatalf("streamed %d events across reconnect, store has %d", len(ids), len(all.Events))
	}
	for i, event := range all.Events {
		if ids[i] != itoa(event.Sequence) {
			t.Fatalf("event %d streamed as %s, want %d", i, ids[i], event.Sequence)
		}
	}

	// A finished run's stream ends at once.
	resp = h.stream(t, "/v1/runs/"+run.RunID+"/events", itoa(all.Head))
	done := readSSE(t, resp.Body, func(sseEvent) bool { return false })
	resp.Body.Close()
	if len(done) != 1 || done[0].event != "end" {
		t.Fatalf("finished stream = %+v", done)
	}
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

func TestCancelAndChangeFeedStream(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.stream(t, "/v1/changes", "")
	defer resp.Body.Close()
	run := h.submit(t, "please ask", "")
	waitFor(t, func() bool {
		snapshot, err := h.runtime.Handle(run.RunID).Snapshot(context.Background())
		return err == nil && snapshot.State == core.RuntimeWaiting
	})
	r, data := h.do(t, http.MethodPost, "/v1/runs/"+run.RunID+"/cancel", nil, nil)
	if r.StatusCode != http.StatusOK || decodeInto[RunView](t, data).State != core.RuntimeTerminal {
		t.Fatalf("cancel = %d %s", r.StatusCode, data)
	}
	changes := readSSE(t, resp.Body, func(e sseEvent) bool {
		return strings.Contains(e.data, `"state":"terminal"`)
	})
	var sawFinalizing bool
	for _, change := range changes {
		view := decodeInto[ChangeView](t, []byte(change.data))
		if view.RunID != run.RunID || change.event != "change" {
			t.Fatalf("change = %+v", change)
		}
		sawFinalizing = sawFinalizing || view.State == core.RuntimeFinalizing
	}
	if !sawFinalizing {
		t.Fatalf("change feed missed the finalizing cancellation step: %+v", changes)
	}
	_, data = h.do(t, http.MethodGet, "/v1/changes?events=true", nil, nil)
	page := decodeInto[ChangePageView](t, data)
	if len(page.Changes) == 0 || len(page.Changes[0].Events) == 0 || page.Head != page.Next {
		t.Fatalf("change page = %+v", page)
	}
}

func TestStreamLimit(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxStreams = 1 })
	resp := h.stream(t, "/v1/changes", "")
	defer resp.Body.Close()
	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/v1/changes?stream=true", nil)
	second, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second stream = %d", second.StatusCode)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
