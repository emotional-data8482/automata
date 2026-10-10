package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

func removeHistory(t *testing.T, store core.Store, runID string) {
	t.Helper()
	if err := store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
		var keys []string
		if err := tx.Scan("runtime_facts", runID+"/", func(key string, _ []byte) error { keys = append(keys, key); return nil }); err != nil {
			return err
		}
		for _, key := range keys {
			if err := tx.Delete("runtime_facts", key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorPreflightsDoNotReadHistory(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, action := range []string{"cancel", "resolve", "reconcile", "acknowledge"} {
			name := action + "/single_tenant"
			if scoped {
				name = action + "/scoped"
			}
			t.Run(name, func(t *testing.T) {
				store := core.NewMemoryStore()
				runtime, err := core.NewRuntime(context.Background(), core.RuntimeConfig{Store: store})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = runtime.Close() })
				ask := core.WithDurableWait(core.Func("ask", "ask", func(context.Context, struct{}) (string, error) { return "unused", nil }), core.DurableWaitPolicy{Kind: core.WaitQuestion})
				agent, err := core.New(fakeProvider{}, core.AgentConfig{Tools: []core.Tool{ask}})
				state := core.RuntimeWaiting
				if action == "reconcile" {
					charge := core.WithToolEffectPolicy(core.FuncResult("charge", "charge", func(context.Context, struct{}) (core.ToolResult, error) {
						return core.ToolResult{Effect: core.EffectReport{Status: core.EffectUnknown}}, nil
					}), core.ToolEffectPolicy{Kind: core.ToolEffectMutating})
					agent, err = core.New(chargeProvider{}, core.AgentConfig{Tools: []core.Tool{charge}})
					state = core.RuntimeNeedsAttention
				}
				if err != nil {
					t.Fatal(err)
				}
				ref, err := runtime.Register("agent", "v1", agent)
				if err != nil {
					t.Fatal(err)
				}
				handle, err := runtime.Submit(context.Background(), ref, "please ask", core.WithIdempotencyKey("a", "request"))
				if err != nil {
					t.Fatal(err)
				}
				snapshot := waitForState(t, runtime, handle.ID(), state)
				removeHistory(t, store, handle.ID())
				if _, err := handle.Snapshot(context.Background()); !errors.Is(err, core.ErrPayloadUnavailable) {
					t.Fatalf("history error = %v", err)
				}
				config := Config{Runtime: runtime, Authorize: AllowAll}
				if scoped {
					config.Scope = func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil }
				}
				server, err := New(config)
				if err != nil {
					t.Fatal(err)
				}
				h := &harness{runtime: runtime, server: httptest.NewServer(server)}
				t.Cleanup(h.server.Close)
				path := "/v1/runs/" + handle.ID()
				owner := http.Header{"X-Tenant": {"a"}}
				if scoped {
					resp, data := h.do(t, http.MethodGet, path, nil, http.Header{"X-Tenant": {"b"}})
					if resp.StatusCode != http.StatusNotFound {
						t.Fatalf("other tenant damaged read = %d %s", resp.StatusCode, data)
					}
				}
				resp, data := h.do(t, http.MethodGet, path, nil, owner)
				if resp.StatusCode != http.StatusInternalServerError || decodeInto[errorBody](t, data).Error.Code != "payload_unavailable" {
					t.Fatalf("damaged read = %d %s", resp.StatusCode, data)
				}
				var body any
				want := http.StatusOK
				switch action {
				case "cancel":
					path += "/cancel"
				case "resolve":
					path += "/waits/" + snapshot.Waits[0].ID
					body = ResolveWaitRequest{Answer: json.RawMessage(`"yes"`)}
				case "reconcile":
					path += "/operations/" + snapshot.ToolBatches[0].Invocations[0].OperationID + "/reconcile"
					body = ReconcileRequest{Result: ReconcileResult{Text: "verified"}, Effect: core.EffectReport{Status: core.EffectNotApplied}}
				case "acknowledge":
					path += "/hooks/acknowledge"
					want = http.StatusConflict
				}
				if scoped {
					resp, data = h.do(t, http.MethodPost, path, body, http.Header{"X-Tenant": {"b"}})
					if resp.StatusCode != http.StatusNotFound {
						t.Fatalf("other tenant mutation = %d %s", resp.StatusCode, data)
					}
				}
				resp, data = h.do(t, http.MethodPost, path, body, owner)
				if resp.StatusCode != want {
					t.Fatalf("%s = %d %s, want %d", action, resp.StatusCode, data, want)
				}
				if want == http.StatusOK {
					view := decodeInto[RunView](t, data)
					if !view.HistoryUnavailable || view.RunID != handle.ID() {
						t.Fatalf("damaged operator response = %s", data)
					}
				}
				if action == "cancel" {
					summary, err := handle.Summary(context.Background())
					if err != nil || summary.State != core.RuntimeTerminal || summary.Status != core.RunCancelled {
						t.Fatalf("canceled summary = %+v, %v", summary, err)
					}
					resp, data = h.do(t, http.MethodPost, path, body, owner)
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("cancel retry = %d %s", resp.StatusCode, data)
					}
				}
			})
		}
	}
}

func TestSubmitAuthorizesBeforeResolvingAndChecksResolvedDefinition(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(map[bool]string{false: "unauthenticated", true: "resolved_definition_denied"}[authenticated], func(t *testing.T) {
			resolved := false
			h := newHarness(t, func(config *Config) {
				config.Authorize = func(_ *http.Request, request Request) error {
					if !authenticated {
						return ErrUnauthenticated
					}
					if request.Definition.ID == "private" {
						return errors.New("private definition")
					}
					return nil
				}
				config.Resolve = func(context.Context, core.DefinitionRef) (core.DefinitionRef, error) {
					resolved = true
					return core.DefinitionRef{ID: "private", Revision: "v1"}, nil
				}
			})
			resp, data := h.do(t, http.MethodPost, "/v1/runs", SubmitRequest{Definition: DefinitionView{ID: "public"}, Task: "hello"}, nil)
			want := http.StatusUnauthorized
			if authenticated {
				want = http.StatusForbidden
			}
			if resp.StatusCode != want || resolved != authenticated {
				t.Fatalf("status=%d resolved=%v body=%s", resp.StatusCode, resolved, data)
			}
		})
	}
}
