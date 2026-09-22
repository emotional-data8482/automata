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
// Earlier pre-release stores are rejected without implicit rewrite.
const runtimeEncodingVersion = 7

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
}

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
)

// RunAttention describes a run needing host intervention. BlockingRunID names
// the pending child whose state blocks a parent in child attention.
type RunAttention struct {
	Kind          AttentionKind
	Reason        string
	BlockingRunID string
}

// RunAccounting separates uncertain local attempts from known subtree totals.
type RunAccounting struct {
	// UnknownAttempts counts provider calls this run may have dispatched
	// without a recorded outcome. Their usage is not in Result.Usage.
	UnknownAttempts int
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
	// Corrections is the cumulative structured-output correction-turn count of
	// the declared contract (see [StructuredOutputConfig]). It persists
	// atomically with the transition that re-dispatches the correction so a
	// restart cannot replay or extend the budget.
	Corrections int `json:"corrections,omitempty"`
	// UnknownAttempts is persisted unknown provider-attempt evidence (see
	// [RunAccounting.UnknownAttempts]). UnknownAttemptGeneration marks the interrupted
	// generation already counted so repeated recovery counts it once.
	// HookDelivery names the committed-run hooks whose delivery started. It
	// commits before the first hook is invoked: a finalizing run without it
	// provably delivered no hook, so recovery can finish it; with it, delivery
	// outcome is unknown and needs attention (see [RunHandle.AcknowledgeHooks]).
	HookDelivery             []string            `json:"hook_delivery,omitempty"`
	UnknownAttempts          int                 `json:"unknown_attempts,omitempty"`
	UnknownAttemptGeneration uint64              `json:"unknown_attempt_generation,omitempty"`
	ToolBatches              []ToolBatchSnapshot `json:"-"`
	Waits                    []WaitSnapshot      `json:"-"`
	Tree                     TreeAccounting      `json:"-"`
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
	live        map[string]liveRuntimeRun
	failures    map[string]error
	subscribers map[string]map[uint64]chan runtimeStreamItem
	nextSubID   uint64
	wg          sync.WaitGroup
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
		live:        make(map[string]liveRuntimeRun),
		failures:    make(map[string]error),
		subscribers: make(map[string]map[uint64]chan runtimeStreamItem),
		closeDone:   make(chan struct{}),
	}
	if err := r.initialize(ctx); err != nil {
		cancel()
		_ = config.Store.Close()
		return nil, err
	}
	return r, nil
}

func (r *Runtime) transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	r.storeMu.RLock()
	defer r.storeMu.RUnlock()
	return r.store.Transaction(ctx, writable, fn)
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
			// Reserve the conversation's active slot and seed the committed
			// history plus this task atomically with the new run.
			seed, err := admitConversationTurnTx(tx, options.Conversation, definitionID, revision, task, runID)
			if err != nil {
				return err
			}
			record.ConversationScope, record.ConversationID = options.Conversation.Scope, options.Conversation.ID
			if err := appendTranscript(tx, runID, &record, seed); err != nil {
				return err
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

// Recover enumerates persisted work in bounded pages. Ready work with a
// registered binding is resumed. A run found in running state came from an
// interrupted owner and is conservatively marked attention-needed; T03/T04 add
// finer-grained recovery.
func (r *Runtime) Recover(ctx context.Context) error {
	var records []storedRuntimeRun
	if err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		// Records are compact; one read transaction pages through every run
		// without decoding any transcript.
		after := ""
		for {
			last, err := tx.ScanPage(runtimeRunsBucket, "", after, runtimeRecoverPageSize, func(_ string, raw []byte) error {
				record, err := decodeRuntimeRun(raw)
				if err != nil {
					return err
				}
				records = append(records, record)
				return nil
			})
			if err != nil {
				return err
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
		switch record.State {
		case RuntimeReady:
			if _, err := r.binding(record.DefinitionID, record.DefinitionRevision); err != nil {
				// Keep admitted work ready so registration followed by another
				// recovery pass can make progress.
				continue
			} else {
				r.start(record.RunID)
			}
		case RuntimeWaiting:
			finalized, err := r.expireWaitingDeadline(ctx, record.RunID)
			if err != nil {
				return err
			}
			if finalized {
				continue
			}
			// Child waits are rechecked before generic wait expiry: a linked
			// child may have terminalized while this owner was away, or be
			// ready to start after registration.
			ready, err := r.reconcileChildWaits(ctx, record.RunID)
			if err != nil {
				return err
			}
			if ready {
				r.start(record.RunID)
				continue
			}
			ready, err = r.expireRunWaits(ctx, record.RunID)
			if err != nil {
				return err
			}
			if ready {
				r.start(record.RunID)
			}
		case RuntimeRunning:
			if err := r.recordInterruptedAttempt(ctx, record); err != nil {
				return err
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
			if record.AttentionKind == "child" {
				// A parent blocked on child attention is still suspended, so
				// its logical deadline applies as for a waiting run.
				finalized, err := r.expireWaitingDeadline(ctx, record.RunID)
				if err != nil {
					return err
				}
				if finalized {
					continue
				}
				// Child attention was recorded on this parent; recheck the
				// linked children so a later clean child completion can unblock
				// ordinary continuation instead of leaving a stale attention.
				ready, err := r.reconcileChildWaits(ctx, record.RunID)
				if err != nil {
					return err
				}
				if ready {
					r.start(record.RunID)
				}
				continue
			}
			if record.AttentionKind == "execution" && (record.PendingBatchID != "" || record.LastTransition == "batch_ready" || record.LastTransition == "response_classified" || record.LastTransition == "batch_committed") {
				resumable, err := r.recoverToolBatch(ctx, record)
				if err != nil {
					return err
				}
				if resumable {
					r.start(record.RunID)
				}
			}
		case RuntimeFinalizing:
			if len(record.HookDelivery) == 0 {
				// No hook was invoked for this committed result, so finishing
				// it cannot repeat a delivery; the terminal commit wakes a
				// parent or advances a conversation as usual.
				if err := r.completeRunHooks(record.RunID); err != nil {
					return err
				}
				continue
			}
			if err := r.markAttentionKind(ctx, record.RunID, record.Generation, "hooks", "previous owner stopped while delivering committed-run hooks; delivery outcome is unknown"); err != nil {
				return err
			}
		}
	}
	return nil
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
		inFlight, err := providerAttemptMayBeInFlight(tx, record)
		if err != nil || !inFlight {
			return err
		}
		record.UnknownAttempts++
		record.UnknownAttemptGeneration = record.Generation
		return putRuntimeRun(tx, record)
	})
}

// providerAttemptMayBeInFlight reports whether the loop's next step from the
// last committed transition was a provider call.
func providerAttemptMayBeInFlight(tx StoreTransaction, record storedRuntimeRun) (bool, error) {
	if record.PendingBatchID != "" {
		return false, nil
	}
	switch record.LastTransition {
	case "":
		return true, nil
	case "batch_committed":
		// A committed fatal outcome or accepted structured output only
		// finalizes.
		return record.Error == "" && len(record.Result.StructuredOutput) == 0, nil
	case "response_classified":
		// A final classification ends on the assistant turn; a correction
		// appends the prompt its next provider turn answers.
		if record.TranscriptChunks == 0 {
			return false, nil
		}
		raw, err := tx.Get(runtimeFactsBucket, transcriptFactKey(record.RunID, record.TranscriptChunks-1))
		if err != nil {
			return false, err
		}
		var chunk []Message
		if err := json.Unmarshal(raw, &chunk); err != nil {
			return false, fmt.Errorf("decode run %s transcript chunk: %w", record.RunID, err)
		}
		return len(chunk) > 0 && chunk[len(chunk)-1].Role != "assistant", nil
	}
	return false, nil
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
		}
		r.wg.Done()
	}()
	record, claimed, err := r.claim(workerCtx, runID)
	if err != nil {
		r.setFailure(runID, err)
		return
	}
	if !claimed {
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
	full, err := r.load(context.Background(), runID)
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
		case RuntimeRunning, RuntimeCancelRequested, RuntimeFinalizing, RuntimeNeedsAttention, RuntimeTerminal:
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
		if record.PendingBatchID != "" && len(result.Messages) > record.TranscriptMessages {
			// A failed batch transition may leave complete results only in memory.
			// Retain them atomically with the batch marker and fatal outcome, just
			// as the original transition would, never as transcript-only progress.
			if err := commitPendingToolBatch(tx, &record); err != nil {
				return err
			}
			record.LastTransition = "batch_committed"
		}
		if err := appendTranscript(tx, runID, &record, result.Messages); err != nil {
			return err
		}
		record.Result = cloneRunResult(result)
		record.Result.Messages = nil
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
	for {
		select {
		case item := <-events:
			if item.terminal {
				return h.Await(ctx)
			}
			if onEvent != nil {
				onEvent(item.event)
			}
		case <-ctx.Done():
			// Return the last snapshot already obtained by this view. A detached
			// storage read could outlive ctx and can lose the admitted identity if
			// that read fails.
			return snapshot.Result, ctx.Err()
		}
	}
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

func (h *RunHandle) Snapshot(ctx context.Context) (RunSnapshot, error) {
	record, err := h.runtime.load(ctx, h.runID)
	if err != nil {
		return RunSnapshot{}, err
	}
	snapshot := snapshotFromRecord(record)
	if snapshot.State == RuntimeReady {
		h.runtime.mu.Lock()
		_, registered := h.runtime.bindings[bindingKey(record.DefinitionID, record.DefinitionRevision)]
		h.runtime.mu.Unlock()
		if !registered {
			snapshot.State = RuntimeNeedsAttention
			snapshot.Attention = &RunAttention{Kind: AttentionExecution, Reason: ErrDefinitionNotRegistered.Error()}
		}
	}
	return snapshot, nil
}

// Observe attaches a provisional live view to a run. Canceling ctx detaches
// only this view. Delivery is bounded and may omit events when the observer is
// slow; durable replay/cursors are added in T08, while Snapshot remains the
// authoritative T02 view.
func (h *RunHandle) Observe(ctx context.Context, onEvent func(StreamEvent)) error {
	events, unsubscribe := h.runtime.subscribe(h.runID)
	defer unsubscribe()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
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
		case <-ticker.C:
			snapshot, err := h.Snapshot(ctx)
			if err != nil {
				return err
			}
			if snapshot.State == RuntimeTerminal || snapshot.State == RuntimeNeedsAttention {
				return nil
			}
		}
	}
}

func (h *RunHandle) Await(ctx context.Context) (RunResult, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	lastResult := RunResult{RunID: h.runID}
	for {
		if workerErr := h.runtime.failure(h.runID); workerErr != nil {
			if snapshot, err := h.Snapshot(ctx); err == nil {
				lastResult = resultWithRunID(snapshot.Result, h.runID)
			}
			return lastResult, workerErr
		}
		if err := ctx.Err(); err != nil {
			return lastResult, err
		}
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
		select {
		case <-ctx.Done():
			return lastResult, ctx.Err()
		case <-ticker.C:
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
		return storedRuntimeRun{}, ErrRunNotFound
	}
	if err != nil {
		return storedRuntimeRun{}, err
	}
	return decodeRuntimeRun(raw)
}

// transcriptFactKey orders transcript chunks by fixed-width hexadecimal
// sequence so ascending key order is also chunk order.
func transcriptFactKey(runID string, chunk int) string {
	return fmt.Sprintf("%s/%016x", runID, chunk)
}

// appendTranscript persists the not-yet-stored suffix of an append-only run
// transcript as one fact chunk and updates the record's transcript counts. The
// run record itself stays compact: compact state changes never rewrite
// committed history.
func appendTranscript(tx StoreTransaction, runID string, record *storedRuntimeRun, messages []Message) error {
	if len(messages) < record.TranscriptMessages {
		return fmt.Errorf("run %s transcript shrank from %d to %d messages", runID, record.TranscriptMessages, len(messages))
	}
	delta := messages[record.TranscriptMessages:]
	if len(delta) > 0 {
		data, err := json.Marshal(delta)
		if err != nil {
			return fmt.Errorf("encode run %s transcript chunk: %w", runID, err)
		}
		if err := tx.Put(runtimeFactsBucket, transcriptFactKey(runID, record.TranscriptChunks), data); err != nil {
			return err
		}
		record.TranscriptChunks++
	}
	record.TranscriptMessages = len(messages)
	return nil
}

// loadRuntimeRun reads a run record and reassembles its transcript from the
// run's append-only fact chunks.
func loadRuntimeRun(tx StoreTransaction, runID string) (storedRuntimeRun, error) {
	record, err := getRuntimeRun(tx, runID)
	if err != nil {
		return storedRuntimeRun{}, err
	}
	if record.TranscriptMessages > 0 {
		messages := make([]Message, 0, record.TranscriptMessages)
		err = tx.Scan(runtimeFactsBucket, runID+"/", func(_ string, raw []byte) error {
			var chunk []Message
			if err := json.Unmarshal(raw, &chunk); err != nil {
				return fmt.Errorf("decode run %s transcript chunk: %w", runID, err)
			}
			messages = append(messages, chunk...)
			return nil
		})
		if err != nil {
			return storedRuntimeRun{}, err
		}
		if len(messages) != record.TranscriptMessages {
			return storedRuntimeRun{}, fmt.Errorf("run %s transcript has %d stored messages but its record expects %d", runID, len(messages), record.TranscriptMessages)
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
	record.Tree, err = loadTreeAccounting(tx, record)
	if err != nil {
		return storedRuntimeRun{}, err
	}
	return record, nil
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
	return tx.Put(runtimeRunsBucket, record.RunID, data)
}

func snapshotFromRecord(record storedRuntimeRun) RunSnapshot {
	snapshot := RunSnapshot{
		RunID: record.RunID, Definition: DefinitionRef{ID: record.DefinitionID, Revision: record.DefinitionRevision},
		State: record.State, Result: cloneRunResult(record.Result),
		Accounting:  RunAccounting{UnknownAttempts: record.UnknownAttempts, Tree: record.Tree},
		Hooks:       append([]RunHookResult(nil), record.HookResults...),
		ToolBatches: cloneToolBatchSnapshots(record.ToolBatches),
		Waits:       append([]WaitSnapshot(nil), record.Waits...),
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
