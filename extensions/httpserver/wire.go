package httpserver

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// SubmitRequest is the body of POST /v1/runs.
type SubmitRequest struct {
	Definition DefinitionView `json:"definition"`
	Task       string         `json:"task"`
	// IdempotencyKey makes admission idempotent: an exact retry returns the
	// original run. IdempotencyScope names its namespace on a server
	// without a tenant Scope; it defaults to "http".
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
	IdempotencyScope string            `json:"idempotency_scope,omitempty"`
	Deadline         *time.Time        `json:"deadline,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	Conversation     *ConversationTurn `json:"conversation,omitempty"`
}

// ConversationTurn admits a run as the next turn of a durable conversation.
// ExpectedHead is the run ID of the turn it continues, empty to start one.
type ConversationTurn struct {
	ID           string `json:"id"`
	ExpectedHead string `json:"expected_head,omitempty"`
}

// ResolveWaitRequest is the body of POST /v1/runs/{id}/waits/{wait}: an
// answer for a question, or a decision with the approval's action digest.
type ResolveWaitRequest struct {
	Answer       json.RawMessage `json:"answer,omitempty"`
	Decision     core.Decision   `json:"decision,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	ActionDigest string          `json:"action_digest,omitempty"`
}

// ReconcileRequest is the body of POST
// /v1/runs/{id}/operations/{op}/reconcile: what the destination
// authoritatively reports about an uncertain operation, and the tool result
// to commit to the model's history. Effect.Status must be "none",
// "applied", or "not_applied". Result carries either Text or Blocks.
type ReconcileRequest struct {
	Result ReconcileResult   `json:"result"`
	Effect core.EffectReport `json:"effect"`
}

type ReconcileResult struct {
	Text    string      `json:"text,omitempty"`
	Blocks  core.Blocks `json:"blocks,omitempty"`
	IsError bool        `json:"is_error,omitempty"`
}

// OperationView is a tool operation awaiting reconciliation. Arguments is
// set only when the server exposes content.
type OperationView struct {
	OperationID string                   `json:"operation_id"`
	Tool        string                   `json:"tool"`
	State       core.ToolInvocationState `json:"state"`
	Effect      core.EffectStatus        `json:"effect,omitempty"`
	ChildRunID  string                   `json:"child_run_id,omitempty"`
	Arguments   json.RawMessage          `json:"arguments,omitempty"`
}

type DefinitionView struct {
	ID       string `json:"id"`
	Revision string `json:"revision,omitempty"`
}

type AttentionView struct {
	Kind          core.AttentionKind `json:"kind"`
	Reason        string             `json:"reason,omitempty"`
	BlockingRunID string             `json:"blocking_run_id,omitempty"`
}

type FailureView struct {
	Kind       core.FailureKind `json:"kind"`
	StopReason core.StopReason  `json:"stop_reason,omitempty"`
	// Message is set only when the server exposes content.
	Message string `json:"message,omitempty"`
}

type ParentView struct {
	RunID       string `json:"run_id"`
	OperationID string `json:"operation_id"`
}

type ConversationView struct {
	Scope string `json:"scope,omitempty"`
	ID    string `json:"id"`
}

// RunView is a run's status. Content fields are set only when the server
// exposes content.
type RunView struct {
	RunID         string            `json:"run_id"`
	Definition    DefinitionView    `json:"definition"`
	State         core.RuntimeState `json:"state"`
	Status        core.RunStatus    `json:"status,omitempty"`
	Attention     *AttentionView    `json:"attention,omitempty"`
	Failure       *FailureView      `json:"failure,omitempty"`
	Parent        *ParentView       `json:"parent,omitempty"`
	Conversation  *ConversationView `json:"conversation,omitempty"`
	Key           string            `json:"idempotency_key,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	CreatedAt     *time.Time        `json:"created_at,omitempty"`
	UpdatedAt     *time.Time        `json:"updated_at,omitempty"`
	EventSequence uint64            `json:"event_sequence,omitempty"`
	Turns         int               `json:"turns,omitempty"`
	Usage         *core.Usage       `json:"usage,omitempty"`
	TreeRuns      int               `json:"tree_runs,omitempty"`
	Waits         []WaitView        `json:"waits,omitempty"`
	// Uncertain lists tool operations whose outcome is unknown; settle each
	// with the reconcile endpoint.
	Uncertain     []OperationView      `json:"uncertain_operations,omitempty"`
	Hooks         []core.RunHookResult `json:"hooks,omitempty"`
	HistoryPruned bool                 `json:"history_pruned,omitempty"`
	// HistoryUnavailable marks a successful operator response containing
	// only compact status because history could not be read.
	HistoryUnavailable bool `json:"history_unavailable,omitempty"`

	Output           string          `json:"output,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	Messages         []core.Message  `json:"messages,omitempty"`
}

// WaitView is one durable wait. Prompt, Target, and Arguments are set only
// when the server exposes content.
type WaitView struct {
	ID           string         `json:"id"`
	Kind         core.WaitKind  `json:"kind"`
	State        core.WaitState `json:"state"`
	Tool         string         `json:"tool,omitempty"`
	OperationID  string         `json:"operation_id,omitempty"`
	ActionDigest string         `json:"action_digest,omitempty"`
	ChildRunID   string         `json:"child_run_id,omitempty"`
	ExpiresAt    *time.Time     `json:"expires_at,omitempty"`
	CreatedAt    *time.Time     `json:"created_at,omitempty"`
	ResolvedAt   *time.Time     `json:"resolved_at,omitempty"`

	Prompt    string          `json:"prompt,omitempty"`
	Target    string          `json:"target,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// RunPageView is a page of GET /v1/runs.
type RunPageView struct {
	Runs []RunView `json:"runs"`
	Next string    `json:"next,omitempty"`
	More bool      `json:"more"`
}

// EventView is one committed run event. Messages is set only when the
// server exposes content.
type EventView struct {
	RunID           string                   `json:"run_id"`
	Sequence        uint64                   `json:"sequence"`
	Time            time.Time                `json:"time"`
	Kind            core.CommittedEventKind  `json:"kind"`
	State           core.RuntimeState        `json:"state,omitempty"`
	PreviousState   core.RuntimeState        `json:"previous_state,omitempty"`
	AttentionKind   core.AttentionKind       `json:"attention_kind,omitempty"`
	Reason          string                   `json:"reason,omitempty"`
	MessageIndex    int                      `json:"message_index,omitempty"`
	MessageCount    int                      `json:"message_count,omitempty"`
	Messages        []core.Message           `json:"messages,omitempty"`
	MessagesPruned  bool                     `json:"messages_pruned,omitempty"`
	OperationID     string                   `json:"operation_id,omitempty"`
	Tool            string                   `json:"tool,omitempty"`
	InvocationState core.ToolInvocationState `json:"invocation_state,omitempty"`
	Effect          core.EffectStatus        `json:"effect,omitempty"`
	WaitID          string                   `json:"wait_id,omitempty"`
	WaitKind        core.WaitKind            `json:"wait_kind,omitempty"`
	WaitState       core.WaitState           `json:"wait_state,omitempty"`
	ChildRunID      string                   `json:"child_run_id,omitempty"`
}

// EventPageView is a JSON page of GET /v1/runs/{id}/events.
type EventPageView struct {
	Events []EventView `json:"events"`
	Next   uint64      `json:"next"`
	Head   uint64      `json:"head"`
}

// ChangeView is one runtime-wide change.
type ChangeView struct {
	Sequence     uint64            `json:"sequence"`
	Time         time.Time         `json:"time"`
	RunID        string            `json:"run_id"`
	FirstEvent   uint64            `json:"first_event,omitempty"`
	LastEvent    uint64            `json:"last_event,omitempty"`
	State        core.RuntimeState `json:"state,omitempty"`
	Pruned       bool              `json:"pruned,omitempty"`
	Events       []EventView       `json:"events,omitempty"`
	EventsPruned bool              `json:"events_pruned,omitempty"`
}

// ChangePageView is a JSON page of GET /v1/changes.
type ChangePageView struct {
	Changes []ChangeView `json:"changes"`
	Next    uint64       `json:"next"`
	Head    uint64       `json:"head"`
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code, body.Error.Message = code, message
	writeJSON(w, status, body)
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func definitionView(ref core.DefinitionRef) DefinitionView {
	return DefinitionView{ID: ref.ID, Revision: ref.Revision}
}

func attentionView(attention *core.RunAttention) *AttentionView {
	if attention == nil {
		return nil
	}
	return &AttentionView{Kind: attention.Kind, Reason: attention.Reason, BlockingRunID: attention.BlockingRunID}
}

func (s *Server) failureView(failure *core.RunFailure) *FailureView {
	if failure == nil {
		return nil
	}
	view := &FailureView{Kind: failure.Kind, StopReason: failure.StopReason}
	if s.config.ExposeContent {
		view.Message = failure.Message
	}
	return view
}

func (s *Server) snapshotView(snapshot core.RunSnapshot) RunView {
	view := RunView{
		RunID: snapshot.RunID, Definition: definitionView(snapshot.Definition), State: snapshot.State,
		Status: snapshot.Result.Status, Attention: attentionView(snapshot.Attention),
		Failure: s.failureView(snapshot.Failure), Key: snapshot.Key, Labels: snapshot.Labels,
		CreatedAt: timePtr(snapshot.CreatedAt), UpdatedAt: timePtr(snapshot.UpdatedAt),
		EventSequence: snapshot.EventSequence, Turns: snapshot.Result.Turns,
		TreeRuns: snapshot.Accounting.Tree.Runs, Hooks: snapshot.Hooks, HistoryPruned: snapshot.HistoryPruned,
	}
	if usage := snapshot.Result.Usage; usage != (core.Usage{}) {
		view.Usage = &usage
	}
	if snapshot.Parent != nil {
		view.Parent = &ParentView{RunID: snapshot.Parent.RunID, OperationID: snapshot.Parent.OperationID}
	}
	if snapshot.Conversation != nil {
		view.Conversation = &ConversationView{Scope: snapshot.Conversation.Scope, ID: snapshot.Conversation.ID}
	}
	for _, wait := range snapshot.Waits {
		view.Waits = append(view.Waits, s.waitView(wait))
	}
	for _, batch := range snapshot.ToolBatches {
		for _, invocation := range batch.Invocations {
			if invocation.State != core.ToolInvocationUncertain {
				continue
			}
			operation := OperationView{
				OperationID: invocation.OperationID, Tool: invocation.Call.Name, State: invocation.State,
				Effect: invocation.Effect.Status, ChildRunID: invocation.ChildRunID,
			}
			if s.config.ExposeContent {
				operation.Arguments = invocation.Call.Input
			}
			view.Uncertain = append(view.Uncertain, operation)
		}
	}
	if s.config.ExposeContent {
		view.Output = snapshot.Result.Output
		view.StructuredOutput = snapshot.Result.StructuredOutput
		view.Messages = snapshot.Result.Messages
	}
	return view
}

func (s *Server) summaryView(summary core.RunSummary) RunView {
	view := RunView{
		RunID: summary.RunID, Definition: definitionView(summary.Definition), State: summary.State,
		Status: summary.Status, Attention: attentionView(summary.Attention), Failure: s.failureView(summary.Failure),
		Key: summary.Key, Labels: summary.Labels,
		CreatedAt: timePtr(summary.CreatedAt), UpdatedAt: timePtr(summary.UpdatedAt),
	}
	if summary.Parent != nil {
		view.Parent = &ParentView{RunID: summary.Parent.RunID, OperationID: summary.Parent.OperationID}
	}
	if summary.Conversation != nil {
		view.Conversation = &ConversationView{Scope: summary.Conversation.Scope, ID: summary.Conversation.ID}
	}
	return view
}

func (s *Server) waitView(wait core.WaitSnapshot) WaitView {
	view := WaitView{
		ID: wait.ID, Kind: wait.Kind, State: wait.State, Tool: wait.Tool, OperationID: wait.OperationID,
		ActionDigest: wait.ActionDigest, ChildRunID: wait.ChildRunID,
		ExpiresAt: timePtr(wait.ExpiresAt), CreatedAt: timePtr(wait.CreatedAt), ResolvedAt: timePtr(wait.ResolvedAt),
	}
	if s.config.ExposeContent {
		view.Prompt, view.Target, view.Arguments = wait.Prompt, wait.Target, wait.Arguments
	}
	return view
}

func (s *Server) eventView(event core.CommittedEvent) EventView {
	view := EventView{
		RunID: event.RunID, Sequence: event.Sequence, Time: event.Time, Kind: event.Kind,
		State: event.State, PreviousState: event.PreviousState, AttentionKind: event.AttentionKind, Reason: event.Reason,
		MessageIndex: event.MessageIndex, MessageCount: event.MessageCount, MessagesPruned: event.MessagesPruned,
		OperationID: event.OperationID, Tool: event.Tool, InvocationState: event.InvocationState, Effect: event.Effect,
		WaitID: event.WaitID, WaitKind: event.WaitKind, WaitState: event.WaitState, ChildRunID: event.ChildRunID,
	}
	if s.config.ExposeContent {
		view.Messages = event.Messages
	}
	return view
}

func (s *Server) changeView(change core.RunChange) ChangeView {
	view := ChangeView{
		Sequence: change.Sequence, Time: change.Time, RunID: change.RunID,
		FirstEvent: change.FirstEvent, LastEvent: change.LastEvent, State: change.State,
		Pruned: change.Pruned, EventsPruned: change.EventsPruned,
	}
	for _, event := range change.Events {
		view.Events = append(view.Events, s.eventView(event))
	}
	return view
}
