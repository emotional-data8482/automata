package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/emotional-data8482/automata/core"
)

func (s *Server) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	body := http.MaxBytesReader(w, r.Body, s.config.MaxBodyBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body is too large")
		} else {
			badRequest(w, "invalid JSON body: %v", err)
		}
		return false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		badRequest(w, "request body must be one JSON value")
		return false
	}
	return true
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var body SubmitRequest
	if !s.decode(w, r, &body) {
		return
	}
	ref := core.DefinitionRef{ID: body.Definition.ID, Revision: body.Definition.Revision}
	if s.config.Resolve != nil {
		resolved, err := s.config.Resolve(r.Context(), ref)
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		ref = resolved
	}
	if ref.ID == "" || ref.Revision == "" {
		badRequest(w, "definition id and revision are required")
		return
	}
	if body.Task == "" {
		badRequest(w, "task is required")
		return
	}
	scope, ok := s.authorize(w, r, Request{Action: ActionSubmit, Definition: ref})
	if !ok {
		return
	}
	labels := maps.Clone(body.Labels)
	if s.config.Labels != nil {
		if labels == nil {
			labels = map[string]string{}
		}
		maps.Copy(labels, s.config.Labels(r))
	}
	opts := []core.SubmitOption{core.WithLabels(labels)}
	key := body.IdempotencyKey
	switch {
	case s.config.Scope != nil:
		if key == "" {
			key = randomKey()
		}
		opts = append(opts, core.WithIdempotencyKey(scope, key))
	case key != "":
		idempotencyScope := body.IdempotencyScope
		if idempotencyScope == "" {
			idempotencyScope = "http"
		}
		opts = append(opts, core.WithIdempotencyKey(idempotencyScope, key))
	}
	if body.Deadline != nil {
		opts = append(opts, core.WithDeadline(*body.Deadline))
	}
	if body.Conversation != nil {
		opts = append(opts, core.WithConversation(core.ConversationRef{Scope: scope, ID: body.Conversation.ID}, body.Conversation.ExpectedHead))
	}
	handle, err := s.config.Runtime.Submit(r.Context(), ref, body.Task, opts...)
	if err != nil {
		if isValidation(err) {
			badRequest(w, "%v", err)
			return
		}
		writeRuntimeError(w, err)
		return
	}
	snapshot, err := handle.Snapshot(r.Context())
	if err != nil {
		// Admission committed; report the identity even if the read failed.
		writeJSON(w, http.StatusAccepted, RunView{RunID: handle.ID(), Definition: definitionView(ref)})
		return
	}
	w.Header().Set("Location", "/v1/runs/"+handle.ID())
	writeJSON(w, http.StatusAccepted, s.snapshotView(snapshot))
}

// isValidation reports an admission rejected for its input rather than for
// runtime state.
func isValidation(err error) bool {
	for _, known := range []error{
		core.ErrDefinitionNotRegistered, core.ErrAdmissionConflict, core.ErrConversationBusy,
		core.ErrConversationConflict, core.ErrConversationBlocked, core.ErrConversationNotFound,
		core.ErrRuntimeClosed, core.ErrRunPruned,
	} {
		if errors.Is(err, known) {
			return false
		}
	}
	return !errors.Is(err, errors.ErrUnsupported) && !isContextError(err)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorize(w, r, Request{Action: ActionList})
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := core.RunFilter{
		Definition: core.DefinitionRef{ID: query.Get("definition"), Revision: query.Get("revision")},
		After:      query.Get("after"),
		RootsOnly:  query.Get("roots") == "true",
	}
	if s.config.Scope != nil {
		filter.Scope = scope
	} else {
		filter.Scope = query.Get("scope")
	}
	for _, state := range query["state"] {
		filter.States = append(filter.States, core.RuntimeState(state))
	}
	for _, kind := range query["attention"] {
		filter.AttentionKinds = append(filter.AttentionKinds, core.AttentionKind(kind))
	}
	for _, label := range query["label"] {
		key, value, found := strings.Cut(label, ":")
		if !found || key == "" {
			badRequest(w, "label filters are key:value")
			return
		}
		if filter.Labels == nil {
			filter.Labels = map[string]string{}
		}
		filter.Labels[key] = value
	}
	for name, field := range map[string]*time.Time{"created_after": &filter.CreatedAfter, "created_before": &filter.CreatedBefore} {
		if raw := query.Get(name); raw != "" {
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				badRequest(w, "%s must be an RFC 3339 time", name)
				return
			}
			*field = parsed
		}
	}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 {
			badRequest(w, "limit must be a non-negative integer")
			return
		}
		filter.Limit = limit
	}
	page, err := s.config.Runtime.Runs(r.Context(), filter)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	view := RunPageView{Runs: make([]RunView, 0, len(page.Runs)), Next: page.Next, More: page.More}
	for _, run := range page.Runs {
		view.Runs = append(view.Runs, s.summaryView(run))
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	scope, ok := s.authorize(w, r, Request{Action: ActionRead, RunID: runID})
	if !ok {
		return
	}
	snapshot, err := s.runSnapshot(r.Context(), scope, runID)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotView(snapshot))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	scope, ok := s.authorize(w, r, Request{Action: ActionCancel, RunID: runID})
	if !ok {
		return
	}
	if _, err := s.runSnapshot(r.Context(), scope, runID); err != nil {
		writeRuntimeError(w, err)
		return
	}
	handle := s.config.Runtime.Handle(runID)
	if err := handle.Cancel(r.Context()); err != nil {
		writeRuntimeError(w, err)
		return
	}
	snapshot, err := handle.Snapshot(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotView(snapshot))
}

func (s *Server) resolveWait(w http.ResponseWriter, r *http.Request) {
	runID, waitID := r.PathValue("id"), r.PathValue("wait")
	var body ResolveWaitRequest
	if !s.decode(w, r, &body) {
		return
	}
	scope, ok := s.authorize(w, r, Request{Action: ActionResolve, RunID: runID, WaitID: waitID})
	if !ok {
		return
	}
	if _, err := s.runSnapshot(r.Context(), scope, runID); err != nil {
		writeRuntimeError(w, err)
		return
	}
	resolution := core.WaitResolution{
		Answer: body.Answer, Decision: body.Decision, Reason: body.Reason, ActionDigest: body.ActionDigest,
	}
	if s.config.Actor != nil {
		resolution.Actor = s.config.Actor(r)
	}
	handle := s.config.Runtime.Handle(runID)
	if err := handle.ResolveWait(r.Context(), waitID, resolution); err != nil {
		if isValidation(err) && !isWaitError(err) {
			badRequest(w, "%v", err)
			return
		}
		writeRuntimeError(w, err)
		return
	}
	snapshot, err := handle.Snapshot(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotView(snapshot))
}

func isWaitError(err error) bool {
	for _, known := range []error{
		core.ErrWaitNotFound, core.ErrWaitConflict, core.ErrWaitStale, core.ErrWaitExpired,
		core.ErrApprovalUnauthorized, core.ErrApprovalActionMismatch, core.ErrRunNotFound,
		context.DeadlineExceeded,
	} {
		if errors.Is(err, known) {
			return true
		}
	}
	return false
}

func (s *Server) describe(w http.ResponseWriter, r *http.Request) {
	ref := core.DefinitionRef{ID: r.PathValue("id"), Revision: r.PathValue("revision")}
	if _, ok := s.authorize(w, r, Request{Action: ActionDescribe, Definition: ref}); !ok {
		return
	}
	manifest, err := s.config.Runtime.Describe(ref)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if !s.config.ExposeContent {
		manifest.SystemPrompt = ""
	}
	writeJSON(w, http.StatusOK, manifest)
}

// cursor reads a stream cursor from Last-Event-ID, falling back to ?after.
func cursor(r *http.Request) (uint64, error) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("after")
	}
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseUint(raw, 10, 64)
}

func pageLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 0 {
		return 0, errors.New("limit must be a non-negative integer")
	}
	return limit, nil
}

func wantsStream(r *http.Request) bool {
	return r.URL.Query().Get("stream") == "true" || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	runID, operationID := r.PathValue("id"), r.PathValue("op")
	var body ReconcileRequest
	if !s.decode(w, r, &body) {
		return
	}
	if body.Result.Text != "" && len(body.Result.Blocks) > 0 {
		badRequest(w, "result carries either text or blocks, not both")
		return
	}
	switch body.Effect.Status {
	case core.EffectNone, core.EffectApplied, core.EffectNotApplied:
	default:
		badRequest(w, "effect status must be none, applied, or not_applied")
		return
	}
	scope, ok := s.authorize(w, r, Request{Action: ActionReconcile, RunID: runID, OperationID: operationID})
	if !ok {
		return
	}
	if _, err := s.runSnapshot(r.Context(), scope, runID); err != nil {
		writeRuntimeError(w, err)
		return
	}
	result := core.ToolResult{Blocks: body.Result.Blocks, IsError: body.Result.IsError}
	if body.Result.Text != "" {
		result.Blocks = core.Blocks{core.TextBlock{Text: body.Result.Text}}
	}
	handle := s.config.Runtime.Handle(runID)
	if err := handle.Reconcile(r.Context(), operationID, core.EffectResolution{Result: result, Effect: body.Effect}); err != nil {
		if isValidation(err) && !isReconcileError(err) {
			badRequest(w, "%v", err)
			return
		}
		writeRuntimeError(w, err)
		return
	}
	s.writeRun(w, r, handle)
}

func isReconcileError(err error) bool {
	return errors.Is(err, core.ErrReconciliationConflict) || errors.Is(err, core.ErrOperationNotFound) ||
		errors.Is(err, core.ErrRunNotFound) || isContextError(err)
}

// acknowledgeHooks resolves a run left needing attention because committed-run
// hook delivery was interrupted. The operator asserts the hooks' possible
// effects are handled; they are never invoked again.
func (s *Server) acknowledgeHooks(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	scope, ok := s.authorize(w, r, Request{Action: ActionAcknowledge, RunID: runID})
	if !ok {
		return
	}
	snapshot, err := s.runSnapshot(r.Context(), scope, runID)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	handle := s.config.Runtime.Handle(runID)
	if err := handle.AcknowledgeHooks(r.Context()); err != nil {
		if errors.Is(err, core.ErrRunNotFound) || errors.Is(err, core.ErrRuntimeClosed) || isContextError(err) {
			writeRuntimeError(w, err)
			return
		}
		writeError(w, http.StatusConflict, "not_awaiting_acknowledgement",
			fmt.Sprintf("run is %s, not awaiting hook acknowledgement", snapshot.State))
		return
	}
	s.writeRun(w, r, handle)
}

func (s *Server) writeRun(w http.ResponseWriter, r *http.Request, handle *core.RunHandle) {
	snapshot, err := handle.Snapshot(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotView(snapshot))
}
