package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// chargeProvider calls the charge tool once, then answers with the tool
// result's text.
type chargeProvider struct{}

func (chargeProvider) Invoke(_ context.Context, req core.Request) (core.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	for _, block := range last.Blocks {
		if result, ok := block.(core.ToolResultBlock); ok {
			text := ""
			for _, inner := range result.Content {
				if t, ok := inner.(core.TextBlock); ok {
					text += t.Text
				}
			}
			return core.Response{Message: core.AssistantMessage(core.TextBlock{Text: "tool said: " + text}), StopReason: core.StopEndTurn}, nil
		}
	}
	return core.Response{
		Message:    core.AssistantMessage(core.ToolUseBlock{ID: "c1", Name: "charge", Input: json.RawMessage(`{}`)}),
		StopReason: core.StopToolUse,
	}, nil
}

func waitForState(t *testing.T, runtime *core.Runtime, runID string, state core.RuntimeState) core.RunSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := runtime.Handle(runID).Snapshot(context.Background())
		if err == nil && snapshot.State == state {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never reached %s (last %s, %v)", runID, state, snapshot.State, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An operation whose outcome is unknown is listed on the run, settled over
// HTTP without running the tool again, and the run then finishes with the
// reconciled result.
func TestReconcileUncertainOperation(t *testing.T) {
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	var executions atomic.Int32
	charge := core.WithToolEffectPolicy(core.FuncResult("charge", "charge a card", func(context.Context, struct{}) (core.ToolResult, error) {
		executions.Add(1)
		return core.ToolResult{Blocks: core.Blocks{core.TextBlock{Text: "timeout"}}, Effect: core.EffectReport{Status: core.EffectUnknown}}, nil
	}), core.ToolEffectPolicy{Kind: core.ToolEffectMutating})
	agent, err := core.New(chargeProvider{}, core.AgentConfig{Tools: []core.Tool{charge}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Register("billing", "v1", agent); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Runtime: runtime, Authorize: func(r *http.Request, req Request) error {
		if req.Action == ActionReconcile && r.Header.Get("Authorization") != "operator" {
			return errors.New("operators only")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{runtime: runtime, server: httptest.NewServer(server)}
	t.Cleanup(h.server.Close)

	_, data := h.do(t, http.MethodPost, "/v1/runs", SubmitRequest{Definition: DefinitionView{ID: "billing", Revision: "v1"}, Task: "charge it"}, nil)
	run := decodeInto[RunView](t, data)
	waitForState(t, runtime, run.RunID, core.RuntimeNeedsAttention)
	_, data = h.do(t, http.MethodGet, "/v1/runs/"+run.RunID, nil, nil)
	status := decodeInto[RunView](t, data)
	if len(status.Uncertain) != 1 || status.Uncertain[0].Tool != "charge" || status.Uncertain[0].Effect != core.EffectUnknown {
		t.Fatalf("uncertain operations = %+v (attention %+v)", status.Uncertain, status.Attention)
	}
	operation := status.Uncertain[0].OperationID
	path := "/v1/runs/" + run.RunID + "/operations/" + operation + "/reconcile"
	body := ReconcileRequest{Result: ReconcileResult{Text: "charged $5"}, Effect: core.EffectReport{Status: core.EffectApplied, Receipt: "ch_1"}}

	if resp, _ := h.do(t, http.MethodPost, path, body, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-operator reconcile = %d", resp.StatusCode)
	}
	operator := http.Header{"Authorization": {"operator"}}
	if resp, data := h.do(t, http.MethodPost, path, ReconcileRequest{Effect: core.EffectReport{Status: core.EffectUnknown}}, operator); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown effect = %d %s", resp.StatusCode, data)
	}
	if resp, data := h.do(t, http.MethodPost, "/v1/runs/"+run.RunID+"/operations/nope/reconcile", body, operator); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown operation = %d %s", resp.StatusCode, data)
	}
	resp, data := h.do(t, http.MethodPost, path, body, operator)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reconcile = %d %s", resp.StatusCode, data)
	}
	if resp, _ := h.do(t, http.MethodPost, path, body, operator); resp.StatusCode != http.StatusOK {
		t.Fatalf("exact retry = %d", resp.StatusCode)
	}
	conflicting := body
	conflicting.Effect.Status = core.EffectNotApplied
	if resp, data := h.do(t, http.MethodPost, path, conflicting, operator); resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting reconcile = %d %s", resp.StatusCode, data)
	}
	final := waitForState(t, runtime, run.RunID, core.RuntimeTerminal)
	if final.Result.Output != "tool said: charged $5" || executions.Load() != 1 {
		t.Fatalf("final output %q after %d executions", final.Result.Output, executions.Load())
	}
}

// faultStore fails writable transactions while armed.
type faultStore struct {
	core.Store
	armed atomic.Bool
}

func (s *faultStore) Transaction(ctx context.Context, writable bool, fn func(core.StoreTransaction) error) error {
	if writable && s.armed.Load() {
		return errors.New("injected write failure")
	}
	return s.Store.Transaction(ctx, writable, fn)
}

func (s *faultStore) Close() error { return nil }

// A run whose hook delivery was interrupted needs attention after recovery;
// acknowledging it over HTTP finishes the run without invoking the hook
// again.
func TestAcknowledgeInterruptedHooks(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		name := "readable"
		if damaged {
			name = "unavailable_history"
		}
		t.Run(name, func(t *testing.T) {
			store := &faultStore{Store: core.NewMemoryStore()}
			var deliveries atomic.Int32
			hooks := []core.CommittedRunHook{{Name: "audit", Handle: func(context.Context, core.RunSnapshot) error {
				// The first delivery's process "stops" after the hook ran but before
				// its outcome committed.
				if deliveries.Add(1) == 1 {
					store.armed.Store(true)
				}
				return nil
			}}}
			register := func(runtime *core.Runtime) {
				agent, err := core.New(fakeProvider{}, core.AgentConfig{})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := runtime.Register("agent", "v1", agent); err != nil {
					t.Fatal(err)
				}
			}
			first, err := core.NewRuntime(context.Background(), core.RuntimeConfig{Store: store, Hooks: hooks})
			if err != nil {
				t.Fatal(err)
			}
			register(first)
			handle, err := first.Submit(context.Background(), core.DefinitionRef{ID: "agent", Revision: "v1"}, "hello")
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for deliveries.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			_ = first.Close()
			store.armed.Store(false)

			runtime, err := core.NewRuntime(context.Background(), core.RuntimeConfig{Store: store, Hooks: hooks})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			register(runtime)
			if err := runtime.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			server, err := New(Config{Runtime: runtime, Authorize: AllowAll})
			if err != nil {
				t.Fatal(err)
			}
			h := &harness{runtime: runtime, server: httptest.NewServer(server)}
			t.Cleanup(h.server.Close)

			_, data := h.do(t, http.MethodGet, "/v1/runs/"+handle.ID(), nil, nil)
			if status := decodeInto[RunView](t, data); status.Attention == nil || status.Attention.Kind != core.AttentionHooks {
				t.Fatalf("status before acknowledgement = %s", data)
			}
			if damaged {
				removeHistory(t, store, handle.ID())
			}
			path := "/v1/runs/" + handle.ID() + "/hooks/acknowledge"
			resp, data := h.do(t, http.MethodPost, path, nil, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("acknowledge = %d %s", resp.StatusCode, data)
			}
			status := decodeInto[RunView](t, data)
			if status.State != core.RuntimeTerminal ||
				(damaged && !status.HistoryUnavailable) ||
				(!damaged && (len(status.Hooks) != 1 || !status.Hooks[0].Unknown)) {
				t.Fatalf("acknowledged = %s", data)
			}
			if resp, _ := h.do(t, http.MethodPost, path, nil, nil); resp.StatusCode != http.StatusOK {
				t.Fatalf("exact retry = %d", resp.StatusCode)
			}
			other := h.submitTo(t, "agent")
			waitForState(t, runtime, other, core.RuntimeTerminal)
			if resp, data := h.do(t, http.MethodPost, "/v1/runs/"+other+"/hooks/acknowledge", nil, nil); resp.StatusCode != http.StatusConflict || !strings.Contains(string(data), "not_awaiting_acknowledgement") {
				t.Fatalf("acknowledge of a finished run = %d %s", resp.StatusCode, data)
			}
			if deliveries.Load() != 2 {
				t.Fatalf("hook delivered %d times, want once per run", deliveries.Load())
			}
		})
	}
}

func (h *harness) submitTo(t *testing.T, definition string) string {
	t.Helper()
	resp, data := h.do(t, http.MethodPost, "/v1/runs", SubmitRequest{Definition: DefinitionView{ID: definition, Revision: "v1"}, Task: "again"}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit = %d %s", resp.StatusCode, data)
	}
	return decodeInto[RunView](t, data).RunID
}
