package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/emotional-data8482/automata/core"
)

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// events serves a run's committed events as one JSON page, or as a
// server-sent event stream that resumes from Last-Event-ID and ends after
// the run's terminal state.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	scope, ok := s.authorize(w, r, Request{Action: ActionRead, RunID: runID})
	if !ok {
		return
	}
	after, err := cursor(r)
	if err != nil {
		badRequest(w, "cursor must be an event sequence")
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	snapshot, err := s.runSnapshot(r.Context(), scope, runID)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	handle := s.config.Runtime.Handle(runID)
	if !wantsStream(r) {
		page, err := handle.Events(r.Context(), after, limit)
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		view := EventPageView{Events: make([]EventView, 0, len(page.Events)), Next: page.Next, Head: page.Head}
		for _, event := range page.Events {
			view.Events = append(view.Events, s.eventView(event))
		}
		writeJSON(w, http.StatusOK, view)
		return
	}
	// Validate the cursor before committing the SSE response headers.
	if _, err := handle.Events(r.Context(), after, 1); err != nil {
		writeRuntimeError(w, err)
		return
	}
	stream, ok := s.openStream(w, r)
	if !ok {
		return
	}
	defer stream.close()
	terminal := snapshot.State == core.RuntimeTerminal
	if terminal && after == snapshot.EventSequence {
		stream.send("end", "", map[string]any{"run_id": runID, "state": snapshot.State})
		return
	}
	for {
		page, err := stream.wait(func(ctx context.Context) (any, error) {
			return handle.WaitEvents(ctx, after, limit)
		})
		if err != nil {
			stream.fail(err)
			return
		}
		if page == nil {
			continue // heartbeat
		}
		events := page.(core.EventPage)
		for _, event := range events.Events {
			if !stream.send(string(event.Kind), strconv.FormatUint(event.Sequence, 10), s.eventView(event)) {
				return
			}
			terminal = terminal || (event.Kind == core.CommittedRunState && event.State == core.RuntimeTerminal)
		}
		after = events.Next
		if terminal && after >= events.Head {
			stream.send("end", "", map[string]any{"run_id": runID, "state": core.RuntimeTerminal})
			return
		}
	}
}

// changes serves the runtime-wide change feed. It is an operator endpoint:
// a server with a tenant Scope does not serve it.
func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	if s.config.Scope != nil {
		writeError(w, http.StatusNotFound, "not_found", "the change feed is not served by a multi-tenant server")
		return
	}
	if _, ok := s.authorize(w, r, Request{Action: ActionChanges}); !ok {
		return
	}
	after, err := cursor(r)
	if err != nil {
		badRequest(w, "cursor must be a change sequence")
		return
	}
	limit, err := pageLimit(r)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	var opts []core.ChangeOption
	if r.URL.Query().Get("events") == "true" {
		opts = append(opts, core.WithChangeEvents())
	}
	if !wantsStream(r) {
		page, err := s.config.Runtime.Changes(r.Context(), after, limit, opts...)
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		view := ChangePageView{Changes: make([]ChangeView, 0, len(page.Changes)), Next: page.Next, Head: page.Head}
		for _, change := range page.Changes {
			view.Changes = append(view.Changes, s.changeView(change))
		}
		writeJSON(w, http.StatusOK, view)
		return
	}
	// Report a stale cursor as a status before the stream starts.
	if _, err := s.config.Runtime.Changes(r.Context(), after, 1); err != nil {
		writeRuntimeError(w, err)
		return
	}
	stream, ok := s.openStream(w, r)
	if !ok {
		return
	}
	defer stream.close()
	for {
		page, err := stream.wait(func(ctx context.Context) (any, error) {
			return s.config.Runtime.WaitChanges(ctx, after, limit, opts...)
		})
		if err != nil {
			stream.fail(err)
			return
		}
		if page == nil {
			continue
		}
		changes := page.(core.ChangePage)
		for _, change := range changes.Changes {
			if !stream.send("change", strconv.FormatUint(change.Sequence, 10), s.changeView(change)) {
				return
			}
		}
		after = changes.Next
	}
}

// Each write, including the initial flush, is bounded even when the overall
// stream has no duration limit. Slow clients reconnect from their cursor.
const streamWriteTimeout = 30 * time.Second

type sseStream struct {
	server     *Server
	w          http.ResponseWriter
	controller *http.ResponseController
	ctx        context.Context
	cancel     context.CancelFunc
	deadlineMu sync.Mutex
	stopWrites func() bool
	writesDone chan struct{}
}

func (s *Server) openStream(w http.ResponseWriter, r *http.Request) (*sseStream, bool) {
	select {
	case s.streams <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "too_many_streams", "too many open event streams")
		return nil, false
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if s.config.MaxStreamDuration > 0 {
		ctx, cancel = context.WithTimeout(r.Context(), s.config.MaxStreamDuration)
	} else {
		ctx, cancel = context.WithCancel(r.Context())
	}
	st := &sseStream{
		server: s, w: w, controller: http.NewResponseController(w),
		ctx: ctx, cancel: cancel, writesDone: make(chan struct{}),
	}
	// Context cancellation alone cannot interrupt a blocked socket write.
	// Serialize deadline updates so cancellation cannot be overwritten by a
	// writer that just started, and join this callback before returning.
	st.stopWrites = context.AfterFunc(ctx, func() {
		st.deadlineMu.Lock()
		_ = st.controller.SetWriteDeadline(time.Now())
		st.deadlineMu.Unlock()
		close(st.writesDone)
	})
	if err := st.prepareWrite(); err != nil {
		st.close()
		if !isContextError(err) {
			writeError(w, http.StatusNotImplemented, "streaming_unsupported", "the response writer must support write deadlines")
		}
		return nil, false
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := st.controller.Flush(); err != nil {
		st.close()
		return nil, false
	}
	return st, true
}

func (st *sseStream) prepareWrite() error {
	st.deadlineMu.Lock()
	defer st.deadlineMu.Unlock()
	if err := st.ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(streamWriteTimeout)
	if end, ok := st.ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	return st.controller.SetWriteDeadline(deadline)
}

func (st *sseStream) close() {
	if !st.stopWrites() {
		<-st.writesDone
	}
	if st.ctx.Err() == nil {
		// Do not leave a cleanly completed stream's deadline on a keep-alive
		// connection. A canceled stream keeps its expired deadline.
		_ = st.controller.SetWriteDeadline(time.Time{})
	}
	st.cancel()
	<-st.server.streams
}

// wait runs one blocking read bounded by the heartbeat. It returns a nil
// value after sending a heartbeat, and an error when the stream must end.
func (st *sseStream) wait(read func(context.Context) (any, error)) (any, error) {
	ctx, cancel := context.WithTimeout(st.ctx, st.server.config.Heartbeat)
	defer cancel()
	value, err := read(ctx)
	if err == nil {
		return value, nil
	}
	if st.ctx.Err() != nil {
		return nil, st.ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
		if err := st.prepareWrite(); err != nil {
			return nil, err
		}
		if _, werr := fmt.Fprint(st.w, ": heartbeat\n\n"); werr != nil {
			return nil, werr
		}
		return nil, st.controller.Flush()
	}
	return nil, err
}

func (st *sseStream) send(event, id string, data any) bool {
	payload, err := json.Marshal(data)
	if err != nil {
		return false
	}
	if err := st.prepareWrite(); err != nil {
		return false
	}
	if id != "" {
		if _, err := fmt.Fprintf(st.w, "id: %s\n", id); err != nil {
			return false
		}
	}
	if _, err := fmt.Fprintf(st.w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return false
	}
	return st.controller.Flush() == nil
}

// fail ends a stream with an error event, unless the client left or the
// stream reached its duration bound (the client resumes from its cursor).
func (st *sseStream) fail(err error) {
	if st.ctx.Err() != nil {
		return
	}
	code := "internal"
	switch {
	case errors.Is(err, core.ErrEventGap):
		code = "cursor_not_retained"
	case errors.Is(err, core.ErrRunPruned):
		code = "pruned"
	case errors.Is(err, core.ErrRuntimeClosed):
		code = "unavailable"
	}
	st.send("error", "", map[string]string{"code": code})
}
