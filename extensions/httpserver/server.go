// Package httpserver serves a core.Runtime over net/http: durable run
// admission, listing and status, conversation state, committed events and the runtime-wide change
// feed as JSON pages or server-sent events, cancellation, durable wait
// answers, and definition manifests.
//
// The server holds no lifecycle state of its own. Every response is read
// from the Runtime's committed store, so a client can submit, disconnect,
// reconnect from any replica sharing the store, and resume an event stream
// exactly where it stopped. Authentication, authorization, tenancy, and
// request attributes are the host's: supply them through [Config].
package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// Action names what a request asks to do, for [Config].Authorize.
type Action string

const (
	ActionSubmit   Action = "submit"
	ActionList     Action = "list"
	ActionRead     Action = "read"
	ActionCancel   Action = "cancel"
	ActionResolve  Action = "resolve_wait"
	ActionChanges  Action = "changes"
	ActionDescribe Action = "describe"
	// ActionConversation reads a conversation's head and active turn.
	ActionConversation Action = "read_conversation"
	// ActionReconcile settles an uncertain tool operation; ActionAcknowledge
	// acknowledges interrupted hook delivery. Both are operator decisions
	// about external effects.
	ActionReconcile   Action = "reconcile"
	ActionAcknowledge Action = "acknowledge_hooks"
)

// Request describes an authorized operation. RunID is set for operations on
// one run, Definition for submissions and manifests, WaitID for wait
// answers, OperationID for reconciliation, and Conversation for
// conversation reads.
type Request struct {
	Action       Action
	RunID        string
	WaitID       string
	OperationID  string
	Definition   core.DefinitionRef
	Conversation string
}

// ErrUnauthenticated is returned by an Authorize function to answer 401
// instead of 403.
var ErrUnauthenticated = errors.New("unauthenticated")

// Config configures a [Server]. Runtime and Authorize are required.
type Config struct {
	Runtime *core.Runtime
	// Authorize admits or rejects every request before it touches the
	// Runtime. Return ErrUnauthenticated (or wrap it) for 401; any other
	// error answers 403. Use [AllowAll] only behind another authenticating
	// layer.
	Authorize func(r *http.Request, req Request) error
	// Scope returns the tenant scope of a request. When set, every
	// submission is admitted under that idempotency scope (a request without
	// an idempotency key gets a random one), conversations are scoped to it,
	// listing is restricted to it, a run outside it answers 404, and the
	// runtime-wide change feed is not served. Without Scope the server is
	// single-tenant.
	Scope func(r *http.Request) (string, error)
	// Labels returns request attributes attached to every submission, for
	// example the authenticated actor or a request ID. They override labels
	// with the same key in the request body.
	Labels func(r *http.Request) map[string]string
	// Actor returns the audit identity recorded on wait answers.
	Actor func(r *http.Request) string
	// Resolve maps a requested definition to a registered revision, for
	// example to choose the current revision when the client names only an
	// ID. By default both ID and revision are required as sent.
	Resolve func(ctx context.Context, ref core.DefinitionRef) (core.DefinitionRef, error)
	// ExposeContent includes model and tool content in responses: tasks,
	// outputs, transcripts, wait prompts and arguments, and failure
	// messages. It is off by default, so status and event endpoints carry
	// only lifecycle facts.
	ExposeContent bool
	// MaxBodyBytes bounds request bodies. Zero selects 1 MiB.
	MaxBodyBytes int64
	// MaxStreams bounds concurrent server-sent event streams; further
	// stream requests answer 503. Zero selects 1024.
	MaxStreams int
	// Heartbeat is the idle interval after which a stream sends a comment
	// to keep intermediaries from closing it. Zero selects 15 seconds.
	Heartbeat time.Duration
	// MaxStreamDuration closes a stream after this long; clients resume
	// with Last-Event-ID. Zero leaves streams open until the client leaves
	// or the run ends.
	MaxStreamDuration time.Duration
}

// AllowAll authorizes every request. Use it only when another layer
// authenticates and authorizes requests before they reach the Server.
func AllowAll(*http.Request, Request) error { return nil }

// Server is an http.Handler serving one Runtime. Mount it at the root or
// under a prefix with http.StripPrefix. Routes:
//
//	POST /v1/runs                      admit a run
//	GET  /v1/runs                      list runs
//	GET  /v1/runs/{id}                 run status
//	GET  /v1/runs/{id}/events          committed events (JSON page or SSE)
//	POST /v1/runs/{id}/cancel          cancel a run and its descendants
//	POST /v1/runs/{id}/waits/{wait}    answer a question or approval
//	POST /v1/runs/{id}/operations/{op}/reconcile
//	                                   settle an uncertain tool operation
//	POST /v1/runs/{id}/hooks/acknowledge
//	                                   acknowledge interrupted hook delivery
//	GET  /v1/conversations/{id}        conversation head and active turn
//	GET  /v1/changes                   runtime-wide change feed (JSON or SSE)
//	GET  /v1/definitions/{id}/{rev}    definition manifest
type Server struct {
	config  Config
	mux     *http.ServeMux
	streams chan struct{}
}

// New validates config and returns a Server.
func New(config Config) (*Server, error) {
	if config.Runtime == nil {
		return nil, errors.New("httpserver: Runtime is required")
	}
	if config.Authorize == nil {
		return nil, errors.New("httpserver: Authorize is required; use AllowAll only behind another authorizing layer")
	}
	if config.MaxBodyBytes < 0 || config.MaxStreams < 0 || config.Heartbeat < 0 || config.MaxStreamDuration < 0 {
		return nil, errors.New("httpserver: limits cannot be negative")
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = 1 << 20
	}
	if config.MaxStreams == 0 {
		config.MaxStreams = 1024
	}
	if config.Heartbeat == 0 {
		config.Heartbeat = 15 * time.Second
	}
	s := &Server{config: config, mux: http.NewServeMux(), streams: make(chan struct{}, config.MaxStreams)}
	s.mux.HandleFunc("POST /v1/runs", s.submit)
	s.mux.HandleFunc("GET /v1/runs", s.list)
	s.mux.HandleFunc("GET /v1/runs/{id}", s.get)
	s.mux.HandleFunc("GET /v1/runs/{id}/events", s.events)
	s.mux.HandleFunc("POST /v1/runs/{id}/cancel", s.cancel)
	s.mux.HandleFunc("POST /v1/runs/{id}/waits/{wait}", s.resolveWait)
	s.mux.HandleFunc("POST /v1/runs/{id}/operations/{op}/reconcile", s.reconcile)
	s.mux.HandleFunc("POST /v1/runs/{id}/hooks/acknowledge", s.acknowledgeHooks)
	s.mux.HandleFunc("GET /v1/conversations/{id}", s.conversation)
	s.mux.HandleFunc("GET /v1/changes", s.changes)
	s.mux.HandleFunc("GET /v1/definitions/{id}/{revision}", s.describe)
	s.mux.HandleFunc("GET /v1/openapi.json", s.openAPI)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// authorize runs the host's check and resolves the request's scope.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, req Request) (string, bool) {
	if err := s.config.Authorize(r, req); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		} else {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
		}
		return "", false
	}
	if s.config.Scope == nil {
		return "", true
	}
	scope, err := s.config.Scope(r)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return "", false
	}
	if scope == "" {
		writeError(w, http.StatusForbidden, "forbidden", "request has no tenant scope")
		return "", false
	}
	return scope, true
}

// runSummary verifies ownership without depending on readable history.
func (s *Server) runSummary(ctx context.Context, scope, runID string) (core.RunSummary, error) {
	summary, err := s.config.Runtime.Handle(runID).Summary(ctx)
	if err != nil {
		return summary, err
	}
	if s.config.Scope != nil && summary.Scope != scope {
		return core.RunSummary{}, core.ErrRunNotFound
	}
	return summary, nil
}

// runSnapshot checks ownership before assembling model and tool content.
func (s *Server) runSnapshot(ctx context.Context, scope, runID string) (core.RunSnapshot, error) {
	if _, err := s.runSummary(ctx, scope, runID); err != nil {
		return core.RunSnapshot{}, err
	}
	return s.config.Runtime.Handle(runID).Snapshot(ctx)
}

func randomKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// writeRuntimeError maps a Runtime error to its HTTP status.
func writeRuntimeError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, core.ErrRunNotFound), errors.Is(err, core.ErrWaitNotFound),
		errors.Is(err, core.ErrConversationNotFound), errors.Is(err, core.ErrOperationNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, core.ErrDefinitionNotRegistered):
		status, code = http.StatusNotFound, "definition_not_registered"
	case errors.Is(err, core.ErrRunPruned):
		status, code = http.StatusGone, "pruned"
	case errors.Is(err, core.ErrEventGap):
		status, code = http.StatusGone, "cursor_not_retained"
	case errors.Is(err, core.ErrAdmissionConflict):
		status, code = http.StatusConflict, "admission_conflict"
	case errors.Is(err, core.ErrConversationBusy):
		status, code = http.StatusConflict, "conversation_busy"
	case errors.Is(err, core.ErrConversationConflict), errors.Is(err, core.ErrConversationBlocked):
		status, code = http.StatusConflict, "conversation_conflict"
	case errors.Is(err, core.ErrReconciliationConflict):
		status, code = http.StatusConflict, "reconciliation_conflict"
	case errors.Is(err, core.ErrWaitConflict), errors.Is(err, core.ErrWaitStale),
		errors.Is(err, core.ErrWaitExpired), errors.Is(err, core.ErrApprovalActionMismatch):
		status, code = http.StatusConflict, "wait_conflict"
	case errors.Is(err, core.ErrApprovalUnauthorized):
		status, code = http.StatusForbidden, "approval_unauthorized"
	case errors.Is(err, core.ErrPayloadUnavailable):
		code = "payload_unavailable"
	case errors.Is(err, core.ErrRuntimeClosed):
		status, code = http.StatusServiceUnavailable, "unavailable"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusServiceUnavailable, "canceled"
	}
	message := err.Error()
	if status == http.StatusInternalServerError {
		message = "internal error"
		if code == "payload_unavailable" {
			message = core.ErrPayloadUnavailable.Error()
		}
	}
	writeError(w, status, code, message)
}

func badRequest(w http.ResponseWriter, format string, args ...any) {
	writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf(format, args...))
}
