package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// runtimeEncodingVersion is the storage encoding this core build reads and
// writes. Version 5 added durable question and approval waits. Version 6 adds
// declared structured-output contracts: persisted correction-turn counts and
// the validated final structured payload inside the run result. Version 7 adds
// durable child runs, parent operation links, and internal child waits.
// Version 8 keys transcript chunks by their first message index and adds
// integrity digests to payloads, transcript base references for conversation
// turns, provider attempt records, the committed event log with its per-run
// heads, the active-run index, and the retention indexes and tombstones.
// Earlier pre-release stores are rejected without implicit rewrite.
const runtimeEncodingVersion = 8

const (
	runtimeMetaBucket         = "runtime_meta"
	runtimeRunsBucket         = "runtime_runs"
	runtimeAdmissionBucket    = "runtime_admissions"
	runtimeFactsBucket        = "runtime_facts"
	runtimeReceiptsBucket     = "runtime_receipts"
	runtimeBatchesBucket      = "runtime_batches"
	runtimeInvocationsBucket  = "runtime_invocations"
	runtimeEffectGuardsBucket = "runtime_effect_guards"
	runtimeWaitsBucket        = "runtime_waits"
	runtimeChildLinksBucket   = "runtime_child_links"
)

// runtimeRecoverPageSize bounds how many run records one recovery scan page
// decodes. Records are compact; transcripts load only when a run is inspected.
const runtimeRecoverPageSize = 128

var (
	ErrDefinitionNotRegistered = errors.New("runtime definition is not registered")
	ErrDefinitionConflict      = errors.New("runtime definition registration conflicts")
	ErrAdmissionConflict       = errors.New("submission identity reused with different input")
	ErrRunNotFound             = errors.New("runtime run not found")
	ErrRunNeedsAttention       = errors.New("runtime run needs attention")
	ErrRuntimeClosed           = errors.New("runtime is closed")
)

type RuntimeState string

const (
	RuntimeReady           RuntimeState = "ready"
	RuntimeRunning         RuntimeState = "running"
	RuntimeWaiting         RuntimeState = "waiting"
	RuntimeCancelRequested RuntimeState = "cancel_requested"
	RuntimeFinalizing      RuntimeState = "finalizing"
	RuntimeNeedsAttention  RuntimeState = "needs_attention"
	RuntimeTerminal        RuntimeState = "terminal"
)

type RuntimeConfig struct {
	Store      Store
	Hooks      []CommittedRunHook
	Authorizer ApprovalAuthorizer
	// ProviderRecovery decides what recovery does with a provider attempt
	// that was recorded as started but never recorded an outcome.
	ProviderRecovery ProviderRecoveryPolicy
	// MaxPayloadBytes bounds the encoded size of one durable payload: a
	// transcript chunk (one provider turn or one committed tool batch) or one
	// tool invocation record with its result. Zero selects
	// DefaultMaxPayloadBytes. Runtime never truncates an oversized payload:
	// the transition that would store it needs attention instead, and an
	// oversized tool result leaves its invocation awaiting reconciliation
	// with its effect report intact.
	MaxPayloadBytes int
}

// DefaultMaxPayloadBytes is the payload bound used when
// RuntimeConfig.MaxPayloadBytes is zero.
const DefaultMaxPayloadBytes = 16 << 20

var (
	// ErrPayloadTooLarge reports a durable payload over the configured bound.
	ErrPayloadTooLarge = errors.New("durable payload exceeds the configured size limit")
	// ErrPayloadUnavailable reports a durable payload that is missing or fails
	// its integrity check. Runtime reports it instead of inventing history.
	ErrPayloadUnavailable = errors.New("durable payload is unavailable")
)

// ProviderRecoveryPolicy authorizes fresh provider attempts after an
// interrupted one. Runtime records each provider attempt before sending it;
// when a worker stops before the attempt's outcome commits, the provider may
// already have received (and billed) the request, and its response is lost.
// Such an attempt is counted in RunAccounting.UnknownAttempts and is never
// silently repeated.
type ProviderRecoveryPolicy struct {
	// MaxFreshAttempts bounds how many fresh attempts recovery may start per
	// run in place of interrupted ones. Zero, the default, leaves the run in
	// RuntimeNeedsAttention with AttentionProvider; cancel it, or raise the
	// bound and call Recover again. Each fresh attempt is counted in
	// RunAccounting.FreshAttempts and consumes the run's turn budget like any
	// other turn.
	MaxFreshAttempts int
}

const (
	// transitionProviderAttemptStarted commits before every provider call.
	// A running run whose last transition is still this one was stopped while
	// the provider may have been processing the request.
	transitionProviderAttemptStarted = "provider_attempt_started"
	// transitionProviderAttemptRecovered marks a run that recovery made ready
	// for a policy-authorized fresh attempt: no attempt is in flight.
	transitionProviderAttemptRecovered = "provider_attempt_recovered"
)

type SubmitOptions struct {
	Scope    string
	Key      string
	Deadline time.Time
	// Conversation admits the run as the next serialized turn of a durable
	// conversation (see [ConversationOptions]).
	Conversation ConversationOptions
}

// RunSnapshot is the host-facing view of a durable run. Optional groups are
// nil when the run has no parent, conversation, failure, or attention.
// Result.Usage is local; Accounting.Tree includes linked descendants.
type RunSnapshot struct {
	RunID        string
	Definition   DefinitionRef
	State        RuntimeState
	Parent       *ParentRef
	Conversation *ConversationRef
	Result       RunResult
	Failure      *RunFailure
	Attention    *RunAttention
	Accounting   RunAccounting
	Hooks        []RunHookResult
	ToolBatches  []ToolBatchSnapshot
	Waits        []WaitSnapshot
	// EventSequence is the run's committed event head read atomically with
	// this snapshot. Continue [RunHandle.Events] from it after resynchronizing.
	EventSequence uint64
	// HistoryPruned reports that retention removed this run's transcript and
	// tool result payloads: Result.Messages is empty and invocation results
	// are marked pruned, while state, accepted output, usage, and effect
	// reports remain.
	HistoryPruned bool
}

// DefinitionRef identifies the pinned definition of a run.
type DefinitionRef struct {
	ID       string
	Revision string
}

// ParentRef links a child to the durable invocation that admitted it.
type ParentRef struct {
	RunID       string
	OperationID string
}

// ConversationRef identifies the conversation containing a turn.
type ConversationRef struct {
	Scope string
	ID    string
}

// FailureKind classifies a persisted execution error independently of its message.
type FailureKind string

const (
	FailureDeadline                        FailureKind = "deadline"
	FailureCancelled                       FailureKind = "cancelled"
	FailureMaxSteps                        FailureKind = "max_steps"
	FailureMaxStepsInvalidStructuredOutput FailureKind = "max_steps_invalid_structured_output"
	FailureInvalidMaxSteps                 FailureKind = "invalid_max_steps"
	FailureEmptyResponse                   FailureKind = "empty_response"
	FailureInvalidStructuredOutput         FailureKind = "invalid_structured_output"
	FailureCompletion                      FailureKind = "completion"
	FailureGeneric                         FailureKind = "generic"
)

// RunFailure preserves the error message and any provider completion details.
type RunFailure struct {
	Message    string
	Kind       FailureKind
	StopReason StopReason
	RawReason  string
}

// AttentionKind identifies the reason a durable run cannot continue normally.
type AttentionKind string

const (
	AttentionExecution AttentionKind = "execution"
	AttentionHooks     AttentionKind = "hooks"
	AttentionChild     AttentionKind = "child"
	// AttentionProvider marks an interrupted provider attempt whose outcome
	// and usage are unknown (see [ProviderRecoveryPolicy]).
	AttentionProvider AttentionKind = "provider"
)

// RunAttention describes why a run cannot continue normally. Child attention
// may be advisory while a canceled subtree settles without host action.
// BlockingRunID names the immediate pending child blocking a parent, not
// necessarily the descendant that needs action; inspect that child's snapshot.
type RunAttention struct {
	Kind          AttentionKind
	Reason        string
	BlockingRunID string
}

// RunAccounting separates uncertain local attempts from known subtree totals.
type RunAccounting struct {
	// UnknownAttempts counts provider attempts this run started without a
	// recorded outcome: the provider may have received them. They are
	// included in Result.ProviderAttempts, but their usage is not in
	// Result.Usage. Retries inside one provider turn share one attempt
	// record, so an interrupted turn counts once.
	UnknownAttempts int
	// FreshAttempts counts provider attempts recovery started in place of
	// interrupted ones under RuntimeConfig.ProviderRecovery. They are new
	// requests, never replays of a lost response.
	FreshAttempts int
	// Tree aggregates recorded accounting over this run and its descendants.
	Tree TreeAccounting
}

type definitionBinding struct {
	revision string
	agent    *Agent
	source   *Agent
}

type storedRuntimeRun struct {
	Version            int              `json:"version"`
	RunID              string           `json:"run_id"`
	DefinitionID       string           `json:"definition_id"`
	DefinitionRevision string           `json:"definition_revision"`
	Task               string           `json:"task"`
	Deadline           time.Time        `json:"deadline,omitempty"`
	State              RuntimeState     `json:"state"`
	Generation         uint64           `json:"generation"`
	Result             RunResult        `json:"result"`
	TranscriptChunks   int              `json:"transcript_chunks"`
	TranscriptMessages int              `json:"transcript_messages"`
	Error              string           `json:"error,omitempty"`
	ErrorKind          string           `json:"error_kind,omitempty"`
	ErrorStopReason    StopReason       `json:"error_stop_reason,omitempty"`
	ErrorRawReason     string           `json:"error_raw_reason,omitempty"`
	LastTransition     string           `json:"last_transition,omitempty"`
	EffectiveTools     []string         `json:"effective_tools,omitempty"`
	AttentionReason    string           `json:"attention_reason,omitempty"`
	AttentionKind      string           `json:"attention_kind,omitempty"`
	HookResults        []RunHookResult  `json:"hook_results,omitempty"`
	PendingBatchID     string           `json:"pending_batch_id,omitempty"`
	NextBatchOrdinal   int              `json:"next_batch_ordinal,omitempty"`
	ToolBudget         storedToolBudget `json:"tool_budget"`
	// ParentRunID and ParentOperationID identify the parent run and durable
	// invocation that admitted this child. They are empty on ordinary runs.
	ParentRunID       string `json:"parent_run_id,omitempty"`
	ParentOperationID string `json:"parent_operation_id,omitempty"`
	// ConversationScope and ConversationID name the durable conversation this
	// run is a turn of. Both are empty on ordinary runs.
	ConversationScope string `json:"conversation_scope,omitempty"`
	ConversationID    string `json:"conversation_id,omitempty"`
	// TranscriptBase names the terminal run whose committed transcript this
	// run continues (the conversation head it was admitted after), and
	// TranscriptBaseMessages is that transcript's length. The run's own chunks
	// start at that index; the base is referenced, never copied.
	TranscriptBase         string `json:"transcript_base,omitempty"`
	TranscriptBaseMessages int    `json:"transcript_base_messages,omitempty"`
	// HistoryPruned records that retention removed the transcript and tool
	// result payloads (see [RetentionPolicy]).
	HistoryPruned bool `json:"history_pruned,omitempty"`
	// PayloadError records why the run needs attention for a payload it could
	// not store or read. PayloadNeeded is the size a rejected payload needed,
	// zero when the payload is unavailable rather than too large. Recovery
	// resumes such a run only once MaxPayloadBytes covers PayloadNeeded.
	PayloadError  string `json:"payload_error,omitempty"`
	PayloadNeeded int    `json:"payload_needed,omitempty"`
	// Corrections is the cumulative structured-output correction-turn count of
	// the declared contract (see [StructuredOutputConfig]). It persists
	// atomically with the transition that re-dispatches the correction so a
	// restart cannot replay or extend the budget.
	Corrections int `json:"corrections,omitempty"`
	// UnknownAttempts is persisted unknown provider-attempt evidence (see
	// [RunAccounting.UnknownAttempts]). UnknownAttemptGeneration marks the interrupted
	// generation already counted so repeated recovery counts it once.
	// FreshAttempts counts fresh provider attempts recovery started in place
	// of interrupted ones (see [ProviderRecoveryPolicy]).
	// HookDelivery names the committed-run hooks whose delivery started. It
	// commits before the first hook is invoked: a finalizing run without it
	// provably delivered no hook, so recovery can finish it; with it, delivery
	// outcome is unknown and needs attention (see [RunHandle.AcknowledgeHooks]).
	HookDelivery             []string            `json:"hook_delivery,omitempty"`
	UnknownAttempts          int                 `json:"unknown_attempts,omitempty"`
	UnknownAttemptGeneration uint64              `json:"unknown_attempt_generation,omitempty"`
	FreshAttempts            int                 `json:"fresh_attempts,omitempty"`
	ToolBatches              []ToolBatchSnapshot `json:"-"`
	Waits                    []WaitSnapshot      `json:"-"`
	Tree                     TreeAccounting      `json:"-"`
	EventSequence            uint64              `json:"-"`
}

// admissionPayload is the canonical admission identity payload. Its JSON
// encoding remains the version 2+ digest rule: changing any field, tag, or encoding
// changes every persisted admission digest and requires a new encoding
// version.
type admissionPayload struct {
	DefinitionID string    `json:"definition_id"`
	Revision     string    `json:"revision"`
	Task         string    `json:"task"`
	Deadline     time.Time `json:"deadline,omitempty"`
	// Conversation fields are omitted when empty so ordinary admission
	// digests keep their encoding.
	ConversationScope string `json:"conversation_scope,omitempty"`
	ConversationID    string `json:"conversation_id,omitempty"`
	ExpectedHead      string `json:"expected_head,omitempty"`
}

// admissionDigest derives the canonical admission identity digest. The
// deadline is normalized to UTC before encoding: Go marshals time.Time in the
// value's own location, and the same instant expressed in a different zone
// must resolve the same run, not mint a new identity.
func admissionDigest(payload admissionPayload) string {
	payload.Deadline = payload.Deadline.UTC()
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// cancelReceipt is the durable receipt for one cancel command. It commits
// atomically with the state change it drove, so an exact retry resolves the
// original outcome even after a lost acknowledgement and any later commands.
type cancelReceipt struct {
	Version       int          `json:"version"`
	RunID         string       `json:"run_id"`
	Generation    uint64       `json:"generation"`
	ObservedState RuntimeState `json:"observed_state"`
}

func cancelReceiptKey(runID string) string { return "cancel\x00" + runID }

type admissionRecord struct {
	RunID  string `json:"run_id"`
	Digest string `json:"digest"`
}

type liveRuntimeRun struct {
	cancel  context.CancelFunc
	restart bool
}

type runtimeStreamItem struct {
	event    StreamEvent
	terminal bool
}

// Runtime owns durable admission, transitions, and worker lifetime. Execution
// currently uses loopMachine as its internal driver; that implementation is
// replaceable and is not a separate public lifecycle.
type Runtime struct {
	store   Store
	storeMu sync.RWMutex

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	closing     bool
	closed      bool
	closeDone   chan struct{}
	closeErr    error
	bindings    map[string]definitionBinding
	hooks       []CommittedRunHook
	authorizer  ApprovalAuthorizer
	providers   ProviderRecoveryPolicy
	maxPayload  int
	live        map[string]liveRuntimeRun
	failures    map[string]error
	subscribers map[string]map[uint64]chan runtimeStreamItem
	nextSubID   uint64
	wg          sync.WaitGroup
	hub         *commitHub
	driver      *runtimeDriver
}

func NewRuntime(ctx context.Context, config RuntimeConfig) (*Runtime, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("runtime store is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	hooks, err := validateCommittedRunHooks(config.Hooks)
	if err != nil {
		return nil, err
	}
	if config.ProviderRecovery.MaxFreshAttempts < 0 {
		return nil, fmt.Errorf("provider recovery MaxFreshAttempts cannot be negative")
	}
	maxPayload := config.MaxPayloadBytes
	if maxPayload < 0 {
		return nil, fmt.Errorf("MaxPayloadBytes cannot be negative")
	}
	if maxPayload == 0 {
		maxPayload = DefaultMaxPayloadBytes
	}
	workerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	authorizer := config.Authorizer
	if nilDependency(authorizer) {
		authorizer = nil
	}
	r := &Runtime{
		store:       config.Store,
		ctx:         workerCtx,
		cancel:      cancel,
		bindings:    make(map[string]definitionBinding),
		hooks:       hooks,
		authorizer:  authorizer,
		providers:   config.ProviderRecovery,
		maxPayload:  maxPayload,
		live:        make(map[string]liveRuntimeRun),
		failures:    make(map[string]error),
		subscribers: make(map[string]map[uint64]chan runtimeStreamItem),
		closeDone:   make(chan struct{}),
		hub:         newCommitHub(),
		driver:      newRuntimeDriver(),
	}
	if err := r.initialize(ctx); err != nil {
		cancel()
		_ = config.Store.Close()
		return nil, err
	}
	r.wg.Add(1)
	go r.runDriver()
	return r, nil
}

// transaction runs fn in a store transaction. Writable transactions derive
// and append their committed events before committing (see [commitTx]) and
// then wake waiters on the changed runs.
func (r *Runtime) transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	r.storeMu.RLock()
	defer r.storeMu.RUnlock()
	if !writable {
		return r.store.Transaction(ctx, false, fn)
	}
	var commits []runCommit
	err := r.store.Transaction(ctx, true, func(tx StoreTransaction) error {
		commits = nil
		observed := newCommitTx(tx, r.maxPayload)
		if err := fn(observed); err != nil {
			return err
		}
		var err error
		commits, err = observed.flush(time.Now())
		return err
	})
	if len(commits) > 0 {
		// Wake waiters even when the store reports an error after fn ran: the
		// commit outcome may be unknown, and a spurious wake only costs each
		// waiter one compact read. The hub records the proposed state only
		// for a confirmed commit.
		r.afterCommit(commits, err == nil)
	}
	return err
}

func NewEphemeralRuntime(hooks ...CommittedRunHook) (*Runtime, error) {
	return NewRuntime(context.Background(), RuntimeConfig{Store: newEphemeralStore(), Hooks: hooks})
}

func (r *Runtime) initialize(ctx context.Context) error {
	return r.store.Transaction(ctx, true, func(tx StoreTransaction) error {
		raw, err := tx.Get(runtimeMetaBucket, "encoding")
		if errors.Is(err, ErrStoreKeyNotFound) {
			data, _ := json.Marshal(runtimeEncodingVersion)
			return tx.Put(runtimeMetaBucket, "encoding", data)
		}
		if err != nil {
			return err
		}
		var version int
		if err := json.Unmarshal(raw, &version); err != nil {
			return fmt.Errorf("decode runtime storage version: %w", err)
		}
		if version != runtimeEncodingVersion {
			return fmt.Errorf("unsupported runtime storage version %d", version)
		}
		return nil
	})
}

// Register binds an immutable executable Agent to a persisted definition
// identity and revision. Re-registering the exact same Agent is idempotent;
// substituting another binding under the same identity is rejected.
func (r *Runtime) Register(definitionID, revision string, agent *Agent) error {
	if definitionID == "" || revision == "" || agent == nil || strings.ContainsRune(definitionID, '\x00') || strings.ContainsRune(revision, '\x00') {
		return fmt.Errorf("definition id, revision, and agent are required")
	}
	for _, tool := range agent.tools {
		policy, configured, err := waitPolicyFor(tool)
		if err != nil {
			return err
		}
		if configured && policy.Kind == WaitApproval && r.authorizer == nil {
			return errors.New("durable approval requires a runtime authorizer")
		}
		name := ""
		if tool != nil {
			name = tool.Definition().Name
		}
		inspection, err := inspectChildTool(tool)
		if err != nil {
			return fmt.Errorf("tool %q: %w", name, err)
		}
		// Durable child declarations are validated structurally here; the
		// pinned child binding itself is checked fail-closed at admission.
		if inspection.hasChild {
			// Process-local authority cannot be persisted with the child
			// admission, so a parent that intercepts calls with an Approver or
			// gates them with a per-call timeout/rate limiter cannot honestly
			// enforce those controls on durable child invocations. The child
			// run instead inherits the parent deadline and enforces its own
			// registered policy locally. Richer parent-side policy support is
			// later scope, not silently skipped enforcement.
			if agent.approver != nil && agent.approver != AllowAll {
				return fmt.Errorf("tool %q: durable child tools do not support process-local approver interception; use Runtime approvals or a child definition without an Approver", name)
			}
			limits := agent.toolPolicy.limitsFor(name)
			if limits.Timeout > 0 || limits.RateLimiter != nil {
				return fmt.Errorf("tool %q: durable child tools do not support per-call timeout or rate limiting; the child run inherits the parent deadline and enforces its own policy locally", name)
			}
		}
		if inspection.transientAdapter {
			return fmt.Errorf("tool %q: process-local child adapters (AsTool/AsToolFunc) cannot be registered on a durable runtime; declare the child with DurableChildTool and register its definition", name)
		}
	}
	key := bindingKey(definitionID, revision)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing || r.closed {
		return ErrRuntimeClosed
	}
	if existing, ok := r.bindings[key]; ok {
		if existing.source == agent {
			return nil
		}
		return ErrDefinitionConflict
	}
	r.bindings[key] = definitionBinding{revision: revision, agent: cloneAgentDefinition(agent), source: agent}
	return nil
}

func cloneAgentDefinition(agent *Agent) *Agent {
	frozen := *agent
	frozen.tools = append([]Tool(nil), agent.tools...)
	frozen.defaultCallOptions = cloneCallOptions(agent.defaultCallOptions)
	frozen.toolPolicy = agent.toolPolicy.clone()
	frozen.preSendHooks = append([]PreSendHook(nil), agent.preSendHooks...)
	frozen.observers = append([]RunObserver(nil), agent.observers...)
	return &frozen
}

func bindingKey(id, revision string) string { return id + "\x00" + revision }

func (r *Runtime) Submit(ctx context.Context, definitionID, revision, task string, options SubmitOptions) (*RunHandle, error) {
	return r.submit(ctx, definitionID, revision, task, options, true)
}

func (r *Runtime) submit(ctx context.Context, definitionID, revision, task string, options SubmitOptions, start bool) (*RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if task == "" {
		return nil, fmt.Errorf("task is required")
	}
	if (options.Scope == "") != (options.Key == "") {
		return nil, fmt.Errorf("submission scope and key must be supplied together")
	}
	if strings.ContainsRune(options.Scope, '\x00') || strings.ContainsRune(options.Key, '\x00') {
		return nil, fmt.Errorf("submission scope and key cannot contain NUL")
	}
	if err := validateConversationOptions(options.Conversation); err != nil {
		return nil, err
	}
	binding, err := r.binding(definitionID, revision)
	if err != nil {
		return nil, err
	}
	// Pin the definition's total subtree cap and local PerTool caps on the
	// record at admission so reservations and descendant checks read persisted
	// values, never context pointers. Replayed admissions keep their original
	// pinned caps.
	caps := pinnedToolCaps(binding.agent.toolPolicy)
	digestText := admissionDigest(admissionPayload{
		DefinitionID: definitionID, Revision: revision, Task: task, Deadline: options.Deadline,
		ConversationScope: options.Conversation.Scope, ConversationID: options.Conversation.ID,
		ExpectedHead: options.Conversation.ExpectedHead,
	})
	var runID string
	var shouldStart bool
	err = r.transaction(ctx, true, func(tx StoreTransaction) error {
		if options.Key != "" {
			admissionKey := options.Scope + "\x00" + options.Key
			raw, err := tx.Get(runtimeAdmissionBucket, admissionKey)
			if err == nil {
				var existing admissionRecord
				if err := json.Unmarshal(raw, &existing); err != nil {
					return err
				}
				if existing.Digest != digestText {
					return ErrAdmissionConflict
				}
				runID = existing.RunID
				record, err := getRuntimeRun(tx, runID)
				if err != nil {
					return err
				}
				shouldStart = record.State == RuntimeReady
				return nil
			}
			if !errors.Is(err, ErrStoreKeyNotFound) {
				return err
			}
		}
		runID = newRuntimeID()
		shouldStart = true
		record := storedRuntimeRun{
			Version: runtimeEncodingVersion, RunID: runID, DefinitionID: definitionID,
			DefinitionRevision: revision, Task: task, Deadline: options.Deadline,
			State: RuntimeReady, Generation: 1, Result: RunResult{RunID: runID},
			ToolBudget: caps,
		}
		if options.Conversation.ID != "" {
			// Reserve the conversation's active slot and, atomically with the
			// new run, continue the head's committed history: the turn
			// references that immutable transcript instead of copying it and
			// appends only this task.
			head, continues, err := admitConversationTurnTx(tx, options.Conversation, definitionID, revision, task, runID)
			if err != nil {
				return err
			}
			record.ConversationScope, record.ConversationID = options.Conversation.Scope, options.Conversation.ID
			if continues {
				record.TranscriptBase, record.TranscriptBaseMessages = head.RunID, head.TranscriptMessages
				record.TranscriptMessages = head.TranscriptMessages
				if err := appendTranscriptDelta(tx, runID, &record, []Message{UserMessage(task)}); err != nil {
					return err
				}
			}
		}
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		if options.Key != "" {
			data, _ := json.Marshal(admissionRecord{RunID: runID, Digest: digestText})
			return tx.Put(runtimeAdmissionBucket, options.Scope+"\x00"+options.Key, data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	h := &RunHandle{runtime: r, runID: runID}
	if start && shouldStart {
		r.start(runID)
	}
	return h, nil
}

func newRuntimeID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}

func (r *Runtime) binding(id, revision string) (definitionBinding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing || r.closed {
		return definitionBinding{}, ErrRuntimeClosed
	}
	binding, ok := r.bindings[bindingKey(id, revision)]
	if !ok {
		return definitionBinding{}, ErrDefinitionNotRegistered
	}
	return binding, nil
}

func (r *Runtime) Handle(runID string) *RunHandle {
	return &RunHandle{runtime: r, runID: runID}
}

// Recover adopts persisted work that no worker in this process owns, paging
// through the non-terminal runs only. Ready work with a registered binding
// starts. A run whose previous owner stopped resumes from its last committed
// transition when no external work was in flight; an open provider attempt
// follows RuntimeConfig.ProviderRecovery, and a dispatched tool call becomes
// uncertain and needs reconciliation. Suspended runs have their deadlines,
// child outcomes, and wait expiries applied and their timers armed for the
// background driver. A run whose payload is unavailable or too large needs
// attention while recovery continues with the others.
func (r *Runtime) Recover(ctx context.Context) error {
	var records []storedRuntimeRun
	if err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		// Page through the active index, which holds exactly the non-terminal
		// runs, and decode their compact records only: terminal history never
		// adds recovery cost.
		after := ""
		for {
			var page []string
			last, err := tx.ScanPage(runtimeActiveBucket, "", after, runtimeRecoverPageSize, func(runID string, _ []byte) error {
				page = append(page, runID)
				return nil
			})
			if err != nil {
				return err
			}
			// Records are read after the page's scan closes, so no adapter has
			// to serve a read inside an open scan.
			for _, runID := range page {
				record, err := getRuntimeRun(tx, runID)
				if err != nil {
					return fmt.Errorf("active run %s: %w", runID, err)
				}
				records = append(records, record)
			}
			if last == "" {
				return nil
			}
			after = last
		}
	}); err != nil {
		return err
	}
	for _, record := range records {
		r.mu.Lock()
		_, live := r.live[record.RunID]
		r.mu.Unlock()
		if live {
			continue
		}
		err := r.recoverRecord(ctx, record)
		if errors.Is(err, ErrPayloadUnavailable) || errors.Is(err, ErrPayloadTooLarge) {
			// Report the unreadable or unstorable payload on its run and keep
			// recovering the others; never invent or truncate history.
			err = r.markPayloadAttention(ctx, record.RunID, err)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// recoverRecord applies the recovery decision for one scanned run that has no
// live worker in this process.
func (r *Runtime) recoverRecord(ctx context.Context, record storedRuntimeRun) error {
	switch record.State {
	case RuntimeReady:
		// Without its binding, admitted work stays ready so registration
		// followed by another recovery pass can make progress.
		if _, err := r.binding(record.DefinitionID, record.DefinitionRevision); err == nil {
			r.start(record.RunID)
		}
	case RuntimeWaiting:
		// Enforce the deadline, consume children that settled while this
		// owner was away, expire elapsed waits, and arm the driver for
		// whatever is still pending.
		if err := r.maintainSuspended(ctx, record); err != nil {
			return err
		}
		return r.armSuspended(ctx, record.RunID)
	case RuntimeRunning:
		if err := r.recordInterruptedAttempt(ctx, record); err != nil {
			return err
		}
		if providerAttemptMayBeInFlight(record) {
			return r.recoverProviderAttempt(ctx, record)
		}
		resumable, err := r.recoverToolBatch(ctx, record)
		if err != nil {
			return err
		}
		if resumable {
			r.start(record.RunID)
		} else if err := r.markAttention(ctx, record.RunID, record.Generation, "previous owner stopped during execution"); err != nil {
			return err
		}
	case RuntimeCancelRequested:
		// Classify interrupted dispatches for inspection, but finish the
		// acknowledged cancellation instead of making it resumable.
		if err := r.recordInterruptedAttempt(ctx, record); err != nil {
			return err
		}
		if _, err := r.recoverToolBatch(ctx, record); err != nil {
			return err
		}
	case RuntimeNeedsAttention:
		if record.AttentionKind == "provider" {
			// A raised ProviderRecovery bound applies on the next pass.
			return r.recoverProviderAttempt(ctx, record)
		}
		if record.AttentionKind == "child" {
			// A parent blocked on child attention is still suspended: its
			// logical deadline applies as for a waiting run, and a later
			// clean child completion can unblock ordinary continuation
			// instead of leaving a stale attention.
			if err := r.maintainSuspended(ctx, record); err != nil {
				return err
			}
			return r.armSuspended(ctx, record.RunID)
		}
		if record.AttentionKind != "execution" || !r.payloadFits(record) {
			return nil
		}
		// Execution attention resumes from its last committed transition
		// when no provider attempt is open: before the first attempt record,
		// after a recovered attempt, or at a batch or classification boundary.
		switch record.LastTransition {
		case "", transitionProviderAttemptRecovered, "batch_ready", "response_classified", "batch_committed":
		default:
			if record.PendingBatchID == "" {
				return nil
			}
		}
		resumable, err := r.recoverToolBatch(ctx, record)
		if err != nil {
			return err
		}
		if resumable {
			r.start(record.RunID)
		}
	case RuntimeFinalizing:
		if len(record.HookDelivery) == 0 {
			// No hook was invoked for this committed result, so finishing
			// it cannot repeat a delivery; the terminal commit wakes a
			// parent or advances a conversation as usual.
			return r.completeRunHooks(record.RunID)
		}
		return r.markAttentionKind(ctx, record.RunID, record.Generation, "hooks", "previous owner stopped while delivering committed-run hooks; delivery outcome is unknown")
	}
	return nil
}

// payloadFits reports whether a run blocked on a payload may resume: it has
// no payload block, or the configured limit now covers the rejected size.
// An unavailable payload never resumes on its own.
func (r *Runtime) payloadFits(record storedRuntimeRun) bool {
	return record.PayloadError == "" || (record.PayloadNeeded > 0 && record.PayloadNeeded <= r.maxPayload)
}

// recordPayloadBlock stores a payload failure on the record.
func recordPayloadBlock(record *storedRuntimeRun, cause error) {
	record.PayloadError, record.PayloadNeeded = cause.Error(), 0
	var tooLarge *payloadTooLargeError
	if errors.As(cause, &tooLarge) {
		record.PayloadNeeded = tooLarge.size
	}
}

// markPayloadAttention records that a run's durable payload cannot be read or
// stored. The run needs attention and recovery leaves it there (see
// payloadFits). An acknowledged cancellation still ends the run: it finishes
// from the compact record, without committed-run hooks, since the history they
// would observe is unreadable. A terminal run keeps its state; its reads
// report the error.
func (r *Runtime) markPayloadAttention(ctx context.Context, runID string, cause error) error {
	var committed storedRuntimeRun
	terminal := false
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		committed, terminal = storedRuntimeRun{}, false
		record, err := getRuntimeRun(tx, runID)
		if err != nil || record.State == RuntimeTerminal {
			return err
		}
		recordPayloadBlock(&record, cause)
		if record.State == RuntimeCancelRequested {
			record.State = RuntimeTerminal
			record.Result.Status = RunCancelled
			record.AttentionReason, record.AttentionKind = "", ""
			setRuntimeError(&record, context.Canceled)
			record.Result.Diagnostics = append(record.Result.Diagnostics, RunDiagnostic{Kind: "payload_error", Message: cause.Error()})
			if err := releaseConversationTx(tx, record); err != nil {
				return err
			}
			terminal = true
		} else {
			record.State = RuntimeNeedsAttention
			record.AttentionKind = "execution"
			record.AttentionReason = cause.Error()
		}
		record.Generation++
		committed = record
		return putRuntimeRun(tx, record)
	})
	if err != nil || committed.RunID == "" {
		return err
	}
	if terminal {
		if committed.ParentRunID != "" {
			return r.wakeParentFromChild(context.WithoutCancel(ctx), runID)
		}
		return nil
	}
	return r.notifyParentOfAttention(ctx, committed)
}

// recordInterruptedAttempt persists unknown provider-attempt evidence for a
// run whose previous owner stopped while its next step was a provider call:
// that attempt may have been dispatched, and its usage, if any, was never
// recorded. It changes no lifecycle state, so the recovery transition that
// follows still matches the scanned generation, and the generation marker
// makes a repeated recovery pass count the same interruption once.
func (r *Runtime) recordInterruptedAttempt(ctx context.Context, scanned storedRuntimeRun) error {
	return r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, scanned.RunID)
		if err != nil {
			return err
		}
		if record.Generation != scanned.Generation || record.UnknownAttemptGeneration == record.Generation {
			return nil
		}
		if record.State != RuntimeRunning && record.State != RuntimeCancelRequested {
			return nil
		}
		if !providerAttemptMayBeInFlight(record) {
			return nil
		}
		// The attempt started, so it counts like any attempt; its outcome and
		// usage are unknown. A graceful stop records the same pair.
		record.Result.ProviderAttempts++
		record.UnknownAttempts++
		record.UnknownAttemptGeneration = record.Generation
		return putRuntimeRun(tx, record)
	})
}

// providerAttemptMayBeInFlight reports whether the last committed transition
// is an attempt record without an outcome: the provider may have received the
// request.
func providerAttemptMayBeInFlight(record storedRuntimeRun) bool {
	return record.PendingBatchID == "" && record.LastTransition == transitionProviderAttemptStarted
}

// recoverProviderAttempt resolves a run whose provider attempt was
// interrupted. Within the ProviderRecovery bound it starts a fresh attempt,
// recorded as such; otherwise the run needs attention. The scanned generation
// guards against acting on a run that moved on.
func (r *Runtime) recoverProviderAttempt(ctx context.Context, scanned storedRuntimeRun) error {
	var committed storedRuntimeRun
	ready := false
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, scanned.RunID)
		if err != nil {
			return err
		}
		if record.Generation != scanned.Generation {
			return nil
		}
		if record.FreshAttempts < r.providers.MaxFreshAttempts {
			record.FreshAttempts++
			record.State = RuntimeReady
			record.AttentionReason, record.AttentionKind = "", ""
			record.LastTransition = transitionProviderAttemptRecovered
			ready = true
		} else if record.State == RuntimeRunning {
			record.State = RuntimeNeedsAttention
			record.AttentionKind = "provider"
			record.AttentionReason = "previous owner stopped during execution: a provider attempt may have been sent and its outcome and usage are unknown"
		} else {
			return nil
		}
		record.Generation++
		committed = record
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		if ready && record.ParentRunID != "" {
			// A parent blocked on this child's provider attention returns to
			// waiting once the child can run again.
			return refreshParentChildAttentionTx(tx, record.ParentRunID)
		}
		return nil
	})
	if err != nil || committed.RunID == "" {
		return err
	}
	if ready {
		r.start(committed.RunID)
		return nil
	}
	return r.notifyParentOfAttention(ctx, committed)
}

func (r *Runtime) start(runID string) {
	r.mu.Lock()
	if r.closing || r.closed {
		r.mu.Unlock()
		return
	}
	if live, ok := r.live[runID]; ok {
		live.restart = true
		r.live[runID] = live
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	r.live[runID] = liveRuntimeRun{cancel: cancel}
	r.wg.Add(1)
	r.mu.Unlock()
	go r.execute(ctx, runID)
}

func (r *Runtime) execute(workerCtx context.Context, runID string) {
	logicalComplete := true
	stale := false
	defer func() {
		r.mu.Lock()
		live := r.live[runID]
		delete(r.live, runID)
		restart := live.restart && !r.closing && !r.closed
		var nextCtx context.Context
		if restart {
			var cancel context.CancelFunc
			nextCtx, cancel = context.WithCancel(r.ctx)
			r.live[runID] = liveRuntimeRun{cancel: cancel}
			r.wg.Add(1)
		}
		r.mu.Unlock()
		if logicalComplete {
			r.publishTerminal(runID)
		}
		if restart {
			go r.execute(nextCtx, runID)
		} else if !logicalComplete || stale {
			// The run suspended, or this was a stale start. A due time that
			// fired while this worker was live was dropped; rearm now that no
			// worker owns the run.
			_ = r.armSuspended(r.ctx, runID)
		}
		r.wg.Done()
	}()
	record, claimed, err := r.claim(workerCtx, runID)
	if err != nil {
		r.setFailure(runID, err)
		return
	}
	if !claimed {
		stale = true
		return
	}
	r.mu.Lock()
	delete(r.failures, runID)
	r.mu.Unlock()
	binding, err := r.binding(record.DefinitionID, record.DefinitionRevision)
	if err != nil {
		if markErr := r.markAttention(context.Background(), runID, record.Generation, err.Error()); markErr != nil {
			r.setFailure(runID, errors.Join(err, markErr))
		}
		return
	}
	execCtx := workerCtx
	var cancel context.CancelFunc
	if !record.Deadline.IsZero() {
		execCtx, cancel = context.WithDeadline(workerCtx, record.Deadline)
		defer cancel()
	}
	// Load with the worker context: an expired logical deadline must finalize
	// the claimed run below, not strand it running behind a failed read.
	record, err = r.load(workerCtx, runID)
	if errors.Is(err, ErrPayloadUnavailable) {
		// The committed history cannot be read truthfully; the run needs
		// attention rather than a guessed continuation.
		if markErr := r.markPayloadAttention(context.Background(), runID, err); markErr != nil {
			r.setFailure(runID, errors.Join(err, markErr))
		}
		return
	}
	if err != nil {
		r.setFailure(runID, err)
		return
	}
	cfg := binding.agent.newRunConfig(nil)
	// Restore the persisted correction-turn count so a restarted correction
	// continues the original budget rather than resetting it.
	if cfg.structuredOutput != nil {
		cfg.structuredOutput.correctionsUsed = record.Corrections
	}
	// Runtime observation is attached through RunHandle. Agent observers are
	// synchronous direct-run instrumentation and do not participate here.
	cfg.observers = nil
	cfg.resume = record.TranscriptMessages > 0
	cfg.resumeTools = append([]string(nil), record.EffectiveTools...)
	var transitionErr error
	cfg.durableTransition = func(ctx context.Context, transition durableLoopTransition) error {
		err := r.persistTransition(ctx, runID, transition)
		if err != nil {
			transitionErr = err
		}
		return err
	}
	cfg.durableBatch = func(ctx context.Context, l *loop, calls []ToolUseBlock, messages []Message, policy *toolPolicyState) ([]Message, error) {
		return r.executeDurableToolBatch(ctx, runID, l, calls, messages, policy)
	}
	history := record.Result.Messages
	scope, beginErr := binding.agent.beginRunWithID(execCtx, cfg, history, nil, "runtime", runID)
	if beginErr != nil {
		result, finishErr := scope.finalize(scope.result, beginErr)
		if commitErr := r.completeExecution(runID, result, finishErr, workerCtx.Err()); commitErr != nil {
			r.setFailure(runID, commitErr)
		}
		return
	}
	scope.result = cloneRunResult(record.Result)
	scope.result.Messages = cloneMessages(history)
	scope.turns = record.Result.Turns
	scope.providerAttempts = record.Result.ProviderAttempts
	scope.usage = record.Result.Usage
	scope.checkpointMessages = cloneMessages(history)
	scope.ctx = scope.policy.withDurableBudget(scope.ctx, func() (toolBudgetUsage, error) {
		return r.reserveNestedCall(runID)
	})
	cfg.scope = scope
	result := cloneRunResult(record.Result)
	var runErr error
	if record.LastTransition == "batch_committed" && record.Error != "" {
		// The batch and its fatal outcome committed before the previous worker
		// stopped. Only finalization remains; do not ask the provider to continue.
		runErr = snapshotError(snapshotFromRecord(record))
	} else {
		result, runErr = binding.agent.runStream(scope.ctx, newLoop(binding.agent, history), record.Task, func(event StreamEvent) {
			r.publish(runID, event)
		}, cfg)
	}
	result, runErr = scope.finalize(result, runErr)
	if transitionErr != nil {
		if markErr := r.markAttentionWithResult(context.Background(), runID, result, "durable transition failed: "+transitionErr.Error()); markErr != nil {
			r.setFailure(runID, errors.Join(transitionErr, markErr))
		}
		return
	}
	if errors.Is(runErr, errDurableWaiting) {
		logicalComplete = false
		return
	}
	if errors.Is(runErr, errDurableBatchIncomplete) {
		if markErr := r.markAttentionWithResult(context.Background(), runID, result, runErr.Error()); markErr != nil {
			r.setFailure(runID, errors.Join(runErr, markErr))
		}
		return
	}
	if commitErr := r.completeExecution(runID, result, runErr, workerCtx.Err()); commitErr != nil {
		r.setFailure(runID, commitErr)
	}
}

func (r *Runtime) completeExecution(runID string, result RunResult, runErr, workerErr error) error {
	record, err := r.finishExecution(runID, result, runErr, workerErr)
	if err != nil {
		return err
	}
	if record.State == RuntimeNeedsAttention {
		// Best effort, like the terminal wake: the child's attention already
		// committed, and Recover rechecks a waiting parent's children.
		_ = r.notifyParentOfAttention(context.Background(), record)
		return nil
	}
	return r.completeRunHooks(runID)
}

// notifyParentOfAttention surfaces a child's committed attention on its
// waiting parent so required-child attention blocks ordinary parent
// continuation visibly, not just through a later recovery pass.
func (r *Runtime) notifyParentOfAttention(ctx context.Context, record storedRuntimeRun) error {
	if record.ParentRunID == "" {
		return nil
	}
	return r.notifyParentChildAttention(ctx, record.ParentRunID, record.RunID, record.AttentionReason)
}

func (r *Runtime) completeRunHooks(runID string) error {
	// Committed-run hooks observe the record as committed, including its
	// reassembled transcript.
	full, err := r.loadSnapshotRecord(context.Background(), runID)
	if err != nil {
		return err
	}
	if len(r.hooks) > 0 {
		claimed, err := r.startHookDelivery(runID)
		if err != nil || !claimed {
			return err
		}
	}
	return r.finishHooks(runID, r.invokeCommittedRunHooks(full))
}

// startHookDelivery commits HookDelivery before any hook runs. Exactly one
// caller claims delivery for a finalizing run; others leave it alone.
func (r *Runtime) startHookDelivery(runID string) (bool, error) {
	claimed := false
	err := r.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.State != RuntimeFinalizing || len(record.HookDelivery) > 0 {
			return nil
		}
		for _, hook := range r.hooks {
			record.HookDelivery = append(record.HookDelivery, hook.Name)
		}
		claimed = true
		return putRuntimeRun(tx, record)
	})
	return claimed, err
}

func (r *Runtime) persistTransition(ctx context.Context, runID string, transition durableLoopTransition) error {
	return r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.State != RuntimeRunning && record.State != RuntimeCancelRequested {
			return fmt.Errorf("run %s cannot commit transition from %s", runID, record.State)
		}
		if err := appendTranscript(tx, runID, &record, transition.Result.Messages); err != nil {
			return err
		}
		if transition.Kind == "batch_committed" {
			if err := commitPendingToolBatch(tx, &record); err != nil {
				return err
			}
		}
		record.Result = cloneRunResult(transition.Result)
		record.Result.Messages = nil
		record.Corrections = transition.StructuredCorrections
		record.LastTransition = transition.Kind
		if transition.Kind == "provider_accepted" {
			record.EffectiveTools = append([]string(nil), transition.EffectiveTools...)
		}
		record.Generation++
		return putRuntimeRun(tx, record)
	})
}

func (r *Runtime) claim(ctx context.Context, runID string) (storedRuntimeRun, bool, error) {
	var claimed storedRuntimeRun
	var didClaim bool
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		switch record.State {
		case RuntimeRunning, RuntimeWaiting, RuntimeCancelRequested, RuntimeFinalizing, RuntimeNeedsAttention, RuntimeTerminal:
			// A stale scheduling attempt, such as a restart requested while the
			// previous worker was suspending the run, is benign: the transition
			// that makes the run ready again schedules it.
			return nil
		case RuntimeReady:
			// Claim below.
		default:
			return fmt.Errorf("run %s has invalid state %q", runID, record.State)
		}
		record.State = RuntimeRunning
		record.Generation++
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		claimed = record
		didClaim = true
		return nil
	})
	return claimed, didClaim, err
}

func (r *Runtime) finishExecution(runID string, result RunResult, runErr, workerErr error) (storedRuntimeRun, error) {
	var committed storedRuntimeRun
	err := r.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if err := appendTranscript(tx, runID, &record, result.Messages); err != nil {
			return err
		}
		record.Result = cloneRunResult(result)
		record.Result.Messages = nil
		record.Generation++
		switch {
		case record.State == RuntimeCancelRequested:
			record.State = RuntimeFinalizing
			record.Result.Status = RunCancelled
			setRuntimeError(&record, context.Canceled)
		case workerErr != nil:
			record.State = RuntimeNeedsAttention
			record.AttentionReason = "worker stopped during execution"
			record.AttentionKind = "execution"
			if record.LastTransition == transitionProviderAttemptStarted {
				// The stopped attempt may have reached the provider: its outcome
				// and usage are unknown, exactly as after a crash, and the same
				// recovery policy decides whether a fresh attempt may follow.
				record.AttentionReason = "worker stopped during execution: a provider attempt may have been sent and its outcome and usage are unknown"
				record.AttentionKind = "provider"
				record.UnknownAttempts++
			}
			setRuntimeError(&record, runErr)
		default:
			record.State = RuntimeFinalizing
			setRuntimeError(&record, runErr)
		}
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		committed = record
		return nil
	})
	return committed, err
}

func (r *Runtime) invokeCommittedRunHooks(record storedRuntimeRun) []RunHookResult {
	results := make([]RunHookResult, 0, len(r.hooks))
	for _, hook := range r.hooks {
		timeout := hook.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), timeout)
		err := invokeCommittedRunHook(ctx, hook, snapshotFromRecord(record))
		cancel()
		results = append(results, RunHookResult{Name: hook.Name, Error: errorString(err)})
	}
	return results
}

func invokeCommittedRunHook(ctx context.Context, hook CommittedRunHook, snapshot RunSnapshot) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return hook.Handle(ctx, snapshot)
}

func (r *Runtime) finishHooks(runID string, results []RunHookResult) error {
	var childOf string
	err := r.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.State == RuntimeTerminal {
			// Another finisher already committed this run's terminal state.
			return nil
		}
		if record.State != RuntimeFinalizing {
			return fmt.Errorf("run %s cannot finish hooks from %s", runID, record.State)
		}
		record.HookResults = append([]RunHookResult(nil), results...)
		record.State = RuntimeTerminal
		record.Generation++
		childOf = record.ParentRunID
		if err := releaseConversationTx(tx, record); err != nil {
			return err
		}
		return putRuntimeRun(tx, record)
	})
	if err != nil {
		return err
	}
	if childOf != "" {
		// Central terminal boundary: every path that durably terminalizes a
		// child (execution, cancellation, waiting-deadline finalization,
		// recovery) notifies the parent here. If this wake transaction fails,
		// the parent stays inspectable as Waiting with its pending child wait
		// and Recover consumes the terminal child on the next pass.
		_ = r.wakeParentFromChild(context.Background(), runID)
	}
	return nil
}

func (r *Runtime) markAttentionWithResult(ctx context.Context, runID string, result RunResult, reason string) error {
	var cancelled bool
	var committed storedRuntimeRun
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		batchResults := record.PendingBatchID != "" && len(result.Messages) > record.TranscriptMessages
		if err := appendTranscript(tx, runID, &record, result.Messages); errors.Is(err, ErrPayloadTooLarge) {
			// The in-memory progress cannot be stored whole. Keep the committed
			// transcript and final message, never a truncated suffix, and say
			// why; the accounting the worker knows stays truthful. Recovery
			// resumes it only once the payload limit covers it.
			recordPayloadBlock(&record, err)
			if !strings.Contains(reason, err.Error()) {
				reason += ": " + err.Error()
			}
			record.Result.Turns, record.Result.Steps = result.Turns, result.Steps
			record.Result.ProviderAttempts, record.Result.Usage = result.ProviderAttempts, result.Usage
		} else if err != nil {
			return err
		} else {
			if batchResults {
				// A failed batch transition may leave complete results only in
				// memory. Retain them atomically with the batch marker and fatal
				// outcome, just as the original transition would, never as
				// transcript-only progress.
				if err := commitPendingToolBatch(tx, &record); err != nil {
					return err
				}
				record.LastTransition = "batch_committed"
			}
			record.Result = cloneRunResult(result)
			record.Result.Messages = nil
		}
		cancelled = record.State == RuntimeCancelRequested
		if cancelled {
			// Uncertainty is retained on the invocation, but cannot undo the
			// durable cancellation or allow reconciliation to restart the run.
			record.State = RuntimeFinalizing
			record.Result.Status = RunCancelled
			record.AttentionReason = ""
			setRuntimeError(&record, context.Canceled)
		} else {
			record.State = RuntimeNeedsAttention
			record.AttentionReason = reason
			record.AttentionKind = "execution"
		}
		record.Generation++
		committed = record
		return putRuntimeRun(tx, record)
	})
	if err != nil {
		return err
	}
	if !cancelled {
		_ = r.notifyParentOfAttention(ctx, committed)
		return nil
	}
	return r.completeRunHooks(runID)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func setRuntimeError(record *storedRuntimeRun, err error) {
	failure := failureFromError(err)
	record.Error, record.ErrorKind, record.ErrorStopReason, record.ErrorRawReason = "", "", "", ""
	if failure != nil {
		record.Error, record.ErrorKind = failure.Message, string(failure.Kind)
		record.ErrorStopReason, record.ErrorRawReason = failure.StopReason, failure.RawReason
	}
}

func failureFromError(err error) *RunFailure {
	if err == nil {
		return nil
	}
	failure := &RunFailure{Message: err.Error()}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		failure.Kind = FailureDeadline
	case errors.Is(err, context.Canceled):
		failure.Kind = FailureCancelled
	case errors.Is(err, ErrMaxStepsExceeded) && errors.Is(err, ErrInvalidStructuredOutput):
		failure.Kind = FailureMaxStepsInvalidStructuredOutput
	case errors.Is(err, ErrMaxStepsExceeded):
		failure.Kind = FailureMaxSteps
	case errors.Is(err, ErrInvalidMaxSteps):
		failure.Kind = FailureInvalidMaxSteps
	case errors.Is(err, ErrEmptyResponse):
		failure.Kind = FailureEmptyResponse
	case errors.Is(err, ErrInvalidStructuredOutput):
		failure.Kind = FailureInvalidStructuredOutput
	default:
		var completion *CompletionError
		if errors.As(err, &completion) {
			failure.Kind = FailureCompletion
			failure.StopReason, failure.RawReason = completion.Reason, completion.RawReason
		} else {
			failure.Kind = FailureGeneric
		}
	}
	return failure
}

func (r *Runtime) setFailure(runID string, err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	r.failures[runID] = err
	r.mu.Unlock()
	r.hub.wake(runID)
}

func (r *Runtime) failure(runID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, live := r.live[runID]; live {
		return nil
	}
	return r.failures[runID]
}

func (r *Runtime) markAttention(ctx context.Context, runID string, generation uint64, reason string) error {
	return r.markAttentionKind(ctx, runID, generation, "execution", reason)
}

func (r *Runtime) markAttentionKind(ctx context.Context, runID string, generation uint64, kind, reason string) error {
	var childOf string
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.Generation != generation {
			return nil
		}
		record.State = RuntimeNeedsAttention
		record.AttentionReason = reason
		record.AttentionKind = kind
		record.Generation++
		childOf = record.ParentRunID
		return putRuntimeRun(tx, record)
	})
	if err == nil && childOf != "" {
		// A required child that needs attention blocks ordinary parent
		// continuation. Surface that on the parent while keeping the child's
		// own evidence authoritative on the child run.
		if err := r.notifyParentChildAttention(ctx, childOf, runID, reason); err != nil {
			return err
		}
	}
	return err
}

func (r *Runtime) publish(runID string, event StreamEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.subscribers[runID] {
		select {
		case ch <- runtimeStreamItem{event: cloneStreamEvent(event)}:
		default:
		}
	}
}

func (r *Runtime) publishTerminal(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.subscribers[runID] {
		select {
		case ch <- runtimeStreamItem{terminal: true}:
		default:
			// Provisional events may be dropped, but completion must wake views.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- runtimeStreamItem{terminal: true}:
			default:
			}
		}
	}
}

func (r *Runtime) subscribe(runID string) (<-chan runtimeStreamItem, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextSubID++
	id := r.nextSubID
	ch := make(chan runtimeStreamItem, 64)
	if r.subscribers[runID] == nil {
		r.subscribers[runID] = make(map[uint64]chan runtimeStreamItem)
	}
	r.subscribers[runID][id] = ch
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if subscribers := r.subscribers[runID]; subscribers != nil {
			delete(subscribers, id)
			if len(subscribers) == 0 {
				delete(r.subscribers, runID)
			}
		}
	}
}

func (r *Runtime) Run(ctx context.Context, definitionID, revision, task string, options SubmitOptions) (RunResult, error) {
	h, err := r.Submit(ctx, definitionID, revision, task, options)
	if err != nil {
		return RunResult{}, err
	}
	return h.Await(ctx)
}

func (r *Runtime) RunStream(ctx context.Context, definitionID, revision, task string, onEvent func(StreamEvent), options SubmitOptions) (RunResult, error) {
	h, err := r.submit(ctx, definitionID, revision, task, options, false)
	if err != nil {
		return RunResult{}, err
	}
	events, unsubscribe := r.subscribe(h.runID)
	defer unsubscribe()
	snapshot, err := h.Snapshot(ctx)
	if err != nil {
		// Admission has committed. Scheduling is owned by the Runtime and must
		// not depend on whether this caller can attach its view. Do not issue a
		// detached read here: the view context must bound this call.
		r.start(h.runID)
		return RunResult{RunID: h.runID}, err
	}
	snapshot.Result = resultWithRunID(snapshot.Result, h.runID)
	if snapshot.State == RuntimeTerminal || snapshot.State == RuntimeNeedsAttention {
		return h.Await(ctx)
	}
	r.start(h.runID)
	err = h.follow(ctx, events, onEvent)
	if err == nil {
		return h.Await(ctx)
	}
	if ctx.Err() != nil {
		// Return the last snapshot already obtained by this view. A detached
		// storage read could outlive ctx and can lose the admitted identity if
		// that read fails.
		return snapshot.Result, ctx.Err()
	}
	return snapshot.Result, err
}

func (r *Runtime) Close() error {
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	if r.closing {
		done := r.closeDone
		r.mu.Unlock()
		<-done
		r.mu.Lock()
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	r.closing = true
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
	r.storeMu.Lock()
	err := r.store.Close()
	r.storeMu.Unlock()
	// Waiters re-read after this wake and observe the closed store.
	r.hub.close()
	r.mu.Lock()
	r.closed = true
	r.closeErr = err
	close(r.closeDone)
	r.mu.Unlock()
	return err
}

type RunHandle struct {
	runtime *Runtime
	runID   string
}

func (h *RunHandle) ID() string { return h.runID }

// Snapshot returns the authoritative committed view of the run, including
// its full transcript, tool batches, waits, and tree accounting. Its cost
// grows with the run's history; use [RunHandle.Events] for incremental
// observation and resynchronize from EventSequence.
func (h *RunHandle) Snapshot(ctx context.Context) (RunSnapshot, error) {
	record, err := h.runtime.loadSnapshotRecord(ctx, h.runID)
	if err != nil {
		return RunSnapshot{}, err
	}
	snapshot := snapshotFromRecord(record)
	if snapshot.State == RuntimeReady && !h.runtime.registered(record) {
		snapshot.State = RuntimeNeedsAttention
		snapshot.Attention = &RunAttention{Kind: AttentionExecution, Reason: ErrDefinitionNotRegistered.Error()}
	}
	return snapshot, nil
}

// loadSnapshotRecord reads everything a host-facing snapshot shows in one read
// transaction: the full record, tree accounting, and the event head. Execution
// paths use loadRuntimeRun, which skips the tree walk.
func (r *Runtime) loadSnapshotRecord(ctx context.Context, runID string) (storedRuntimeRun, error) {
	var record storedRuntimeRun
	err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		record, err = loadRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.Tree, err = loadTreeAccounting(tx, record); err != nil {
			return err
		}
		head, err := getEventHead(tx, runID)
		record.EventSequence = head.Head
		return err
	})
	return record, err
}

func (r *Runtime) registered(record storedRuntimeRun) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.bindings[bindingKey(record.DefinitionID, record.DefinitionRevision)]
	return ok
}

// settledState reads only the compact run record and reports whether a view
// waiting for the run to finish should stop: the run is terminal, needs
// attention, or is admitted without a registered binding.
func (h *RunHandle) settledState(ctx context.Context) (storedRuntimeRun, bool, error) {
	var record storedRuntimeRun
	err := h.runtime.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		record, err = getRuntimeRun(tx, h.runID)
		return err
	})
	if err != nil {
		return record, false, err
	}
	switch record.State {
	case RuntimeTerminal, RuntimeNeedsAttention:
		return record, true, nil
	case RuntimeReady:
		return record, !h.runtime.registered(record), nil
	}
	return record, false, nil
}

// follow delivers provisional live events until the run settles (see
// settledState) or ctx ends. It wakes on committed transitions of the run
// rather than polling, and reads only the compact record on each wake.
func (h *RunHandle) follow(ctx context.Context, events <-chan runtimeStreamItem, onEvent func(StreamEvent)) error {
	sub := h.runtime.hub.subscribe(h.runID)
	defer sub.close()
	read := true
	for {
		view := sub.next()
		if err := ctx.Err(); err != nil {
			return err
		}
		if read || view.gone || view.closed || mayHaveSettled(view.state) {
			_, settled, err := h.settledState(ctx)
			if err != nil || settled {
				return err
			}
			if view.closed {
				return ErrRuntimeClosed
			}
		}
		read = false
		changed := view.changed
	wait:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case item := <-events:
				if item.terminal {
					return nil
				}
				if onEvent != nil {
					onEvent(item.event)
				}
			case <-changed:
				break wait
			}
		}
	}
}

// Observe attaches a provisional live view to a run and returns when the run
// settles or ctx ends. Canceling ctx detaches only this view. Delivery is
// bounded and may omit events when the observer is slow; committed facts are
// replayable through [RunHandle.Events], and Snapshot remains authoritative.
func (h *RunHandle) Observe(ctx context.Context, onEvent func(StreamEvent)) error {
	events, unsubscribe := h.runtime.subscribe(h.runID)
	defer unsubscribe()
	return h.follow(ctx, events, onEvent)
}

// Await waits until the run is terminal or needs attention and returns its
// committed result. Waiting wakes on the run's committed transitions and
// reads only the compact run record per wake; the full result, transcript
// included, is read once when the run settles. If ctx ends first, Await
// returns the run identity with the last compact result observed, whose
// Messages are not loaded.
func (h *RunHandle) Await(ctx context.Context) (RunResult, error) {
	lastResult := RunResult{RunID: h.runID}
	sub := h.runtime.hub.subscribe(h.runID)
	defer sub.close()
	read := true
	for {
		view := sub.next()
		changed := view.changed
		if workerErr := h.runtime.failure(h.runID); workerErr != nil {
			if snapshot, err := h.Snapshot(ctx); err == nil {
				lastResult = resultWithRunID(snapshot.Result, h.runID)
			}
			return lastResult, workerErr
		}
		if err := ctx.Err(); err != nil {
			return lastResult, err
		}
		if !read && !view.gone && !view.closed && !mayHaveSettled(view.state) {
			// The committed transition that woke this wait cannot end it.
			select {
			case <-ctx.Done():
				return lastResult, ctx.Err()
			case <-changed:
			}
			continue
		}
		read = false
		record, settled, err := h.settledState(ctx)
		if err != nil {
			return lastResult, err
		}
		lastResult = resultWithRunID(cloneRunResult(record.Result), h.runID)
		if settled {
			snapshot, err := h.Snapshot(ctx)
			if err != nil {
				return lastResult, err
			}
			lastResult = resultWithRunID(snapshot.Result, h.runID)
			switch snapshot.State {
			case RuntimeTerminal:
				return lastResult, snapshotError(snapshot)
			case RuntimeNeedsAttention:
				return lastResult, fmt.Errorf("%w: %s", ErrRunNeedsAttention, snapshot.Attention.Reason)
			}
			// The run moved on between the compact read and the snapshot.
			read = true
			continue
		}
		if view.closed {
			return lastResult, ErrRuntimeClosed
		}
		select {
		case <-ctx.Done():
			return lastResult, ctx.Err()
		case <-changed:
		}
	}
}

func resultWithRunID(result RunResult, runID string) RunResult {
	if result.RunID == "" {
		result.RunID = runID
	}
	return result
}

func (h *RunHandle) Cancel(ctx context.Context) error {
	var cancels []context.CancelFunc
	var canceledChild string
	err := h.runtime.transaction(ctx, true, func(tx StoreTransaction) error {
		// Resolve an exact retry from the persisted receipt before touching
		// current state: a lost acknowledgement followed by any later command
		// still resolves the original outcome without re-deriving state.
		if _, err := tx.Get(runtimeReceiptsBucket, cancelReceiptKey(h.runID)); err == nil {
			return nil
		} else if !errors.Is(err, ErrStoreKeyNotFound) {
			return err
		}
		record, err := getRuntimeRun(tx, h.runID)
		if err != nil {
			return err
		}
		receipt := cancelReceipt{
			Version: runtimeEncodingVersion, RunID: h.runID,
			Generation: record.Generation, ObservedState: record.State,
		}
		if record.State == RuntimeReady || record.State == RuntimeWaiting || record.State == RuntimeNeedsAttention || record.State == RuntimeRunning {
			if err := resolveReservedInvocations(tx, record); err != nil {
				return err
			}
			if err := cancelRunWaits(tx, record.RunID); err != nil {
				return err
			}
		}
		switch record.State {
		case RuntimeFinalizing, RuntimeTerminal:
			return nil
		case RuntimeReady, RuntimeWaiting, RuntimeNeedsAttention:
			record.State = RuntimeTerminal
			record.Result.Status = RunCancelled
			record.AttentionReason = ""
			record.AttentionKind = ""
			setRuntimeError(&record, context.Canceled)
			if record.ParentRunID != "" {
				canceledChild = h.runID
			}
			if err := releaseConversationTx(tx, record); err != nil {
				return err
			}
		case RuntimeRunning:
			record.State = RuntimeCancelRequested
		}
		record.Generation++
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		// Required descendants are canceled in this same commit, before any
		// local worker is signaled.
		running, err := propagateCancellationTx(tx, h.runID, context.Canceled)
		if err != nil {
			return err
		}
		data, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		if err := tx.Put(runtimeReceiptsBucket, cancelReceiptKey(h.runID), data); err != nil {
			return err
		}
		cancels = cancels[:0]
		h.runtime.mu.Lock()
		for _, runID := range append([]string{h.runID}, running...) {
			if live, ok := h.runtime.live[runID]; ok {
				cancels = append(cancels, live.cancel)
			}
		}
		h.runtime.mu.Unlock()
		return nil
	})
	if err == nil {
		for _, cancel := range cancels {
			cancel()
		}
	}
	if err == nil && canceledChild != "" {
		// Cancellation durably terminalized a suspended child run; notify the
		// parent through the same central child-completion boundary so the
		// parent sees the model-visible cancellation outcome.
		_ = h.runtime.wakeParentFromChild(context.Background(), canceledChild)
	}
	return err
}

// hooksAckReceiptKey keys the receipt of a hook acknowledgement, so an exact
// retry after a lost response resolves the original outcome.
func hooksAckReceiptKey(runID string) string { return "hooks\x00" + runID }

// AcknowledgeHooks resolves the attention left when committed-run hook
// delivery was interrupted. The host asserts it has handled whatever the
// interrupted hooks may or may not have done; Runtime never invokes them
// again. The run becomes terminal with its committed execution result
// unchanged, each hook whose delivery had started is recorded as Unknown in
// RunSnapshot.Hooks, and the terminal commit wakes a waiting durable
// parent or advances the run's conversation like any other. An exact retry
// after success returns nil; a run not awaiting hook acknowledgement is an
// error.
func (h *RunHandle) AcknowledgeHooks(ctx context.Context) error {
	var childOf string
	err := h.runtime.transaction(ctx, true, func(tx StoreTransaction) error {
		childOf = ""
		if _, err := tx.Get(runtimeReceiptsBucket, hooksAckReceiptKey(h.runID)); err == nil {
			return nil
		} else if !errors.Is(err, ErrStoreKeyNotFound) {
			return err
		}
		record, err := getRuntimeRun(tx, h.runID)
		if err != nil {
			return err
		}
		if record.State != RuntimeNeedsAttention || record.AttentionKind != "hooks" {
			return fmt.Errorf("run %s is not awaiting hook acknowledgement (state %s)", h.runID, record.State)
		}
		record.HookResults = record.HookResults[:0]
		for _, name := range record.HookDelivery {
			record.HookResults = append(record.HookResults, RunHookResult{
				Name: name, Error: "delivery outcome unknown: interrupted and acknowledged", Unknown: true,
			})
		}
		record.State = RuntimeTerminal
		record.AttentionReason = ""
		record.AttentionKind = ""
		record.Generation++
		childOf = record.ParentRunID
		if err := releaseConversationTx(tx, record); err != nil {
			return err
		}
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		return putStoredJSON(tx, runtimeReceiptsBucket, hooksAckReceiptKey(h.runID), cancelReceipt{
			Version: runtimeEncodingVersion, RunID: h.runID, Generation: record.Generation, ObservedState: RuntimeNeedsAttention,
		})
	})
	if err == nil && childOf != "" {
		err = h.runtime.wakeParentFromChild(context.WithoutCancel(ctx), h.runID)
	}
	return err
}

func (r *Runtime) load(ctx context.Context, runID string) (storedRuntimeRun, error) {
	var record storedRuntimeRun
	err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		record, err = loadRuntimeRun(tx, runID)
		return err
	})
	return record, err
}

func getRuntimeRun(tx StoreTransaction, runID string) (storedRuntimeRun, error) {
	raw, err := tx.Get(runtimeRunsBucket, runID)
	if errors.Is(err, ErrStoreKeyNotFound) {
		return storedRuntimeRun{}, missingRunError(tx, runID)
	}
	if err != nil {
		return storedRuntimeRun{}, err
	}
	return decodeRuntimeRun(raw)
}

// transcriptFactKey keys a transcript chunk by the index of its first
// message, in fixed-width hexadecimal so ascending key order is transcript
// order and a reader can seek directly to the chunk a message range starts
// in.
func transcriptFactKey(runID string, firstMessage int) string {
	return fmt.Sprintf("%s/%016x", runID, firstMessage)
}

// appendTranscript persists the not-yet-stored suffix of an append-only run
// transcript as one fact chunk and updates the record's transcript counts. The
// run record itself stays compact: compact state changes never rewrite
// committed history.
func appendTranscript(tx StoreTransaction, runID string, record *storedRuntimeRun, messages []Message) error {
	if len(messages) < record.TranscriptMessages {
		return fmt.Errorf("run %s transcript shrank from %d to %d messages", runID, record.TranscriptMessages, len(messages))
	}
	return appendTranscriptDelta(tx, runID, record, messages[record.TranscriptMessages:])
}

// appendTranscriptDelta persists delta as one chunk starting at the record's
// current transcript length.
func appendTranscriptDelta(tx StoreTransaction, runID string, record *storedRuntimeRun, delta []Message) error {
	if len(delta) > 0 {
		data, err := encodeTranscriptChunk(delta)
		if err != nil {
			return fmt.Errorf("encode run %s transcript chunk: %w", runID, err)
		}
		if err := tx.Put(runtimeFactsBucket, transcriptFactKey(runID, record.TranscriptMessages), data); err != nil {
			return err
		}
		record.TranscriptChunks++
	}
	record.TranscriptMessages += len(delta)
	return nil
}

// loadRuntimeRun reads a run record and reassembles its transcript from the
// run's append-only fact chunks, following its transcript base chain.
func loadRuntimeRun(tx StoreTransaction, runID string) (storedRuntimeRun, error) {
	record, err := getRuntimeRun(tx, runID)
	if err != nil {
		return storedRuntimeRun{}, err
	}
	if record.TranscriptMessages > 0 && !record.HistoryPruned {
		messages, err := loadTranscript(tx, record)
		if err != nil {
			return storedRuntimeRun{}, err
		}
		record.Result.Messages = messages
	}
	batches, err := loadToolBatchSnapshots(tx, runID)
	if err != nil {
		return storedRuntimeRun{}, err
	}
	record.ToolBatches = batches
	waits, err := loadRunWaits(tx, runID)
	if err != nil {
		return storedRuntimeRun{}, err
	}
	for _, wait := range waits {
		record.Waits = append(record.Waits, waitSnapshot(wait))
	}
	return record, nil
}

// missingRunError distinguishes a run retention removed from one that never
// existed.
func missingRunError(tx StoreTransaction, runID string) error {
	if _, err := tx.Get(runtimePrunedBucket, runID); err == nil {
		return ErrRunPruned
	} else if !errors.Is(err, ErrStoreKeyNotFound) {
		return err
	}
	return ErrRunNotFound
}

// loadTranscript reassembles a run's committed transcript: the transcripts of
// its base chain, oldest first, then its own chunks.
func loadTranscript(tx StoreTransaction, record storedRuntimeRun) ([]Message, error) {
	chain := []storedRuntimeRun{record}
	for current := record; current.TranscriptBase != ""; {
		base, err := getRuntimeRun(tx, current.TranscriptBase)
		if errors.Is(err, ErrRunNotFound) {
			err = ErrPayloadUnavailable
		}
		if err != nil {
			return nil, fmt.Errorf("run %s transcript base %s: %w", current.RunID, current.TranscriptBase, err)
		}
		if base.TranscriptMessages != current.TranscriptBaseMessages {
			return nil, fmt.Errorf("%w: run %s transcript base %s has %d messages, want %d", ErrPayloadUnavailable, current.RunID, base.RunID, base.TranscriptMessages, current.TranscriptBaseMessages)
		}
		chain = append(chain, base)
		current = base
	}
	messages := make([]Message, 0, record.TranscriptMessages)
	for i := len(chain) - 1; i >= 0; i-- {
		run := chain[i]
		if err := tx.Scan(runtimeFactsBucket, run.RunID+"/", func(key string, raw []byte) error {
			if want := transcriptFactKey(run.RunID, len(messages)); key != want {
				return fmt.Errorf("%w: run %s transcript chunk %s follows message %d", ErrPayloadUnavailable, run.RunID, key, len(messages))
			}
			chunk, err := decodeTranscriptChunk(run.RunID, raw)
			if err != nil {
				return err
			}
			messages = append(messages, chunk...)
			return nil
		}); err != nil {
			return nil, err
		}
		if len(messages) != run.TranscriptMessages {
			return nil, fmt.Errorf("%w: run %s transcript has %d stored messages but its record expects %d", ErrPayloadUnavailable, run.RunID, len(messages), run.TranscriptMessages)
		}
	}
	return messages, nil
}

// storedTranscriptChunk is one append-only transcript fact. The digest covers
// the exact encoded messages, so a missing, truncated, or altered chunk is
// reported as ErrPayloadUnavailable instead of silently changing history.
type storedTranscriptChunk struct {
	Digest   string          `json:"sha256"`
	Messages json.RawMessage `json:"messages"`
}

func encodeTranscriptChunk(messages []Message) ([]byte, error) {
	data, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return json.Marshal(storedTranscriptChunk{Digest: hex.EncodeToString(sum[:]), Messages: data})
}

func decodeTranscriptChunk(runID string, raw []byte) ([]Message, error) {
	var chunk storedTranscriptChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return nil, fmt.Errorf("%w: run %s transcript chunk: %v", ErrPayloadUnavailable, runID, err)
	}
	sum := sha256.Sum256(chunk.Messages)
	if hex.EncodeToString(sum[:]) != chunk.Digest {
		return nil, fmt.Errorf("%w: run %s transcript chunk fails its integrity check", ErrPayloadUnavailable, runID)
	}
	var messages []Message
	if err := json.Unmarshal(chunk.Messages, &messages); err != nil {
		return nil, fmt.Errorf("%w: run %s transcript chunk: %v", ErrPayloadUnavailable, runID, err)
	}
	return messages, nil
}

func decodeRuntimeRun(raw []byte) (storedRuntimeRun, error) {
	var record storedRuntimeRun
	if err := json.Unmarshal(raw, &record); err != nil {
		return storedRuntimeRun{}, fmt.Errorf("decode runtime run: %w", err)
	}
	if record.Version != runtimeEncodingVersion {
		return storedRuntimeRun{}, fmt.Errorf("unsupported runtime run version %d", record.Version)
	}
	return record, nil
}

func putRuntimeRun(tx StoreTransaction, record storedRuntimeRun) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := tx.Put(runtimeRunsBucket, record.RunID, data); err != nil {
		return err
	}
	return syncRunIndexes(tx, record)
}

func snapshotFromRecord(record storedRuntimeRun) RunSnapshot {
	snapshot := RunSnapshot{
		RunID: record.RunID, Definition: DefinitionRef{ID: record.DefinitionID, Revision: record.DefinitionRevision},
		State: record.State, Result: cloneRunResult(record.Result),
		Accounting:    RunAccounting{UnknownAttempts: record.UnknownAttempts, FreshAttempts: record.FreshAttempts, Tree: record.Tree},
		Hooks:         append([]RunHookResult(nil), record.HookResults...),
		ToolBatches:   cloneToolBatchSnapshots(record.ToolBatches),
		Waits:         append([]WaitSnapshot(nil), record.Waits...),
		EventSequence: record.EventSequence,
		HistoryPruned: record.HistoryPruned,
	}
	if record.ParentRunID != "" {
		snapshot.Parent = &ParentRef{RunID: record.ParentRunID, OperationID: record.ParentOperationID}
	}
	if record.ConversationID != "" {
		snapshot.Conversation = &ConversationRef{Scope: record.ConversationScope, ID: record.ConversationID}
	}
	snapshot.Failure = failureFromRecord(record)
	if record.State == RuntimeNeedsAttention {
		snapshot.Attention = &RunAttention{Kind: AttentionKind(record.AttentionKind), Reason: record.AttentionReason}
		if snapshot.Attention.Kind == AttentionChild {
			// The persisted reason names the blocking child. Match it against a
			// pending child wait, rather than trusting an arbitrary string as an ID.
			for _, wait := range snapshot.Waits {
				if wait.Kind == WaitChild && wait.State == WaitPending && wait.ChildRunID != "" &&
					strings.HasPrefix(record.AttentionReason, "durable child "+wait.ChildRunID+" requires attention: ") {
					snapshot.Attention.BlockingRunID = wait.ChildRunID
					break
				}
			}
		}
	}
	return snapshot
}

func failureFromRecord(record storedRuntimeRun) *RunFailure {
	if record.Error == "" && record.ErrorKind == "" {
		return nil
	}
	return &RunFailure{Message: record.Error, Kind: FailureKind(record.ErrorKind), StopReason: record.ErrorStopReason, RawReason: record.ErrorRawReason}
}

func snapshotError(snapshot RunSnapshot) error {
	failure := snapshot.Failure
	if failure == nil || failure.Message == "" {
		return nil
	}
	switch failure.Kind {
	case FailureDeadline:
		return context.DeadlineExceeded
	case FailureCancelled:
		return context.Canceled
	case FailureMaxSteps:
		return fmt.Errorf("%s: %w", failure.Message, ErrMaxStepsExceeded)
	case FailureMaxStepsInvalidStructuredOutput:
		return fmt.Errorf("%s: %w", failure.Message, errors.Join(ErrMaxStepsExceeded, ErrInvalidStructuredOutput))
	case FailureInvalidMaxSteps:
		return fmt.Errorf("%s: %w", failure.Message, ErrInvalidMaxSteps)
	case FailureEmptyResponse:
		return fmt.Errorf("%s: %w", failure.Message, ErrEmptyResponse)
	case FailureInvalidStructuredOutput:
		return fmt.Errorf("%s: %w", failure.Message, ErrInvalidStructuredOutput)
	case FailureCompletion:
		return &CompletionError{Reason: failure.StopReason, RawReason: failure.RawReason}
	}
	if snapshot.Result.Status == RunCancelled {
		return context.Canceled
	}
	return errors.New(failure.Message)
}
