package httpserver

import (
	"errors"
	"net/http"
	"testing"
)

func TestConversationHeadContinuesTurns(t *testing.T) {
	var authorized []Request
	h := newHarness(t, func(c *Config) {
		c.Scope = func(r *http.Request) (string, error) { return r.Header.Get("X-Tenant"), nil }
		c.Authorize = func(r *http.Request, req Request) error {
			if req.Action == ActionConversation {
				authorized = append(authorized, req)
			}
			if r.Header.Get("X-Deny") != "" {
				return errors.New("denied")
			}
			return nil
		}
	})
	tenantA := http.Header{"X-Tenant": {"a"}}
	read := func(header http.Header) (int, ConversationStateView) {
		resp, data := h.do(t, http.MethodGet, "/v1/conversations/thread", nil, header)
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, ConversationStateView{}
		}
		return resp.StatusCode, decodeInto[ConversationStateView](t, data)
	}
	turn := func(head string) RunView {
		resp, data := h.do(t, http.MethodPost, "/v1/runs", SubmitRequest{
			Definition: DefinitionView{ID: "agent", Revision: "v1"}, Task: "hello",
			Conversation: &ConversationTurn{ID: "thread", ExpectedHead: head},
		}, tenantA)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("submit = %d %s", resp.StatusCode, data)
		}
		run := decodeInto[RunView](t, data)
		h.await(t, run.RunID)
		return run
	}

	if status, _ := read(tenantA); status != http.StatusNotFound {
		t.Fatalf("before the first turn: status %d, want 404", status)
	}
	first := turn("")
	status, got := read(tenantA)
	if status != http.StatusOK || got.Head != first.RunID || got.ActiveRunID != "" || got.Turns != 1 ||
		got.ID != "thread" || got.Scope != "a" || got.Definition != (DefinitionView{ID: "agent", Revision: "v1"}) {
		t.Fatalf("after one turn: %d %+v", status, got)
	}
	second := turn(got.Head)
	if _, got := read(tenantA); got.Head != second.RunID || got.Turns != 2 {
		t.Fatalf("after two turns: %+v", got)
	}

	// Another tenant's conversation with the same ID does not exist.
	if status, _ := read(http.Header{"X-Tenant": {"b"}}); status != http.StatusNotFound {
		t.Fatalf("other tenant: status %d, want 404", status)
	}
	if status, _ := read(http.Header{"X-Tenant": {"a"}, "X-Deny": {"1"}}); status != http.StatusForbidden {
		t.Fatalf("denied: status %d, want 403", status)
	}
	if len(authorized) == 0 || authorized[0].Conversation != "thread" {
		t.Fatalf("authorized requests = %+v", authorized)
	}
}
