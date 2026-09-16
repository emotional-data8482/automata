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
// writes. Version 2 moves run transcripts into append-only fact chunks and
// introduces command receipts; version 1 stores were a pre-release prototype
// and are rejected without rewrite.
const runtimeEncodingVersion = 2

const (
	runtimeMetaBucket      = "runtime_meta"
	runtimeRunsBucket      = "runtime_runs"
	runtimeAdmissionBucket = "runtime_admissions"
	runtimeFactsBucket     = "runtime_facts"
	runtimeReceiptsBucket  = "runtime_receipts"
)

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
	RuntimeCancelRequested RuntimeState = "cancel_requested"
	RuntimeFinalizing      RuntimeState = "finalizing"
	RuntimeNeedsAttention  RuntimeState = "needs_attention"
	RuntimeTerminal        RuntimeState = "terminal"
)

type RuntimeConfig struct {
	Store Store
	Hooks []CommittedRunHook
}

type SubmitOptions struct {
	Scope    string
	Key      string
	Deadline time.Time
}

type RunSnapshot struct {
	RunID              string
	DefinitionID       string
	DefinitionRevision string
	State              RuntimeState
	Result             RunResult
	Error              string
	ErrorKind          string
	ErrorStopReason    StopReason
	ErrorRawReason     string
	AttentionReason    string
	HookResults        []RunHookResult
}

type definitionBinding struct {
	revision string
	agent    *Agent
	source   *Agent
}

type storedRuntimeRun struct {
	Version            int             `json:"version"`
	RunID              string          `json:"run_id"`
	DefinitionID       string          `json:"definition_id"`
	DefinitionRevision string          `json:"definition_revision"`
	Task               string          `json:"task"`
	Deadline           time.Time       `json:"deadline,omitempty"`
	State              RuntimeState    `json:"state"`
	Generation         uint64          `json:"generation"`
	Result             RunResult       `json:"result"`
	TranscriptChunks   int             `json:"transcript_chunks"`
	TranscriptMessages int             `json:"transcript_messages"`
	Error              string          `json:"error,omitempty"`
	ErrorKind          string          `json:"error_kind,omitempty"`
	ErrorStopReason    StopReason      `json:"error_stop_reason,omitempty"`
	ErrorRawReason     string          `json:"error_raw_reason,omitempty"`
	LastTransition     string          `json:"last_transition,omitempty"`
	AttentionReason    string          `json:"attention_reason,omitempty"`
	HookResults        []RunHookResult `json:"hook_results,omitempty"`
}

// admissionPayload is the canonical admission identity payload. Its JSON
// encoding is the version 2 digest rule: changing any field, tag, or encoding
// changes every persisted admission digest and requires a new encoding
// version.
type admissionPayload struct {
	DefinitionID string    `json:"definition_id"`
	Revision     string    `json:"revision"`
	Task         string    `json:"task"`
	Deadline     time.Time `json:"deadline,omitempty"`
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
	cancel context.CancelFunc
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
	r := &Runtime{
		store:       config.Store,
		ctx:         workerCtx,
		cancel:      cancel,
		bindings:    make(map[string]definitionBinding),
		hooks:       hooks,
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
	if _, err := r.binding(definitionID, revision); err != nil {
		return nil, err
	}
	digestText := admissionDigest(admissionPayload{DefinitionID: definitionID, Revision: revision, Task: task, Deadline: options.Deadline})
	var runID string
	var shouldStart bool
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
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

// Recover enumerates persisted work. Ready work with a registered binding is
// resumed. A run found in running state came from an interrupted owner and is
// conservatively marked attention-needed; T03/T04 add finer-grained recovery.
func (r *Runtime) Recover(ctx context.Context) error {
	var records []storedRuntimeRun
	if err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		return tx.Scan(runtimeRunsBucket, "", func(_ string, raw []byte) error {
			record, err := decodeRuntimeRun(raw)
			if err != nil {
				return err
			}
			records = append(records, record)
			return nil
		})
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
		case RuntimeRunning, RuntimeCancelRequested:
			if err := r.markAttention(ctx, record.RunID, record.Generation, "previous owner stopped during execution"); err != nil {
				return err
			}
		case RuntimeFinalizing:
			if err := r.markAttention(ctx, record.RunID, record.Generation, "previous owner stopped while delivering committed-run hooks; delivery outcome is unknown"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runtime) start(runID string) {
	r.mu.Lock()
	if r.closing || r.closed {
		r.mu.Unlock()
		return
	}
	if _, ok := r.live[runID]; ok {
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
	defer r.wg.Done()
	defer r.publishTerminal(runID)
	defer func() {
		r.mu.Lock()
		delete(r.live, runID)
		r.mu.Unlock()
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
	cfg := binding.agent.newRunConfig(nil)
	// Runtime observation is attached through RunHandle. Agent observers are
	// synchronous direct-run instrumentation and do not participate here.
	cfg.observers = nil
	var transitionErr error
	cfg.durableTransition = func(ctx context.Context, transition durableLoopTransition) error {
		err := r.persistTransition(ctx, runID, transition)
		if err != nil {
			transitionErr = err
		}
		return err
	}
	scope, beginErr := binding.agent.beginRunWithID(execCtx, cfg, nil, nil, "runtime", runID)
	if beginErr != nil {
		result, finishErr := scope.finalize(scope.result, beginErr)
		if commitErr := r.completeExecution(runID, result, finishErr, workerCtx.Err()); commitErr != nil {
			r.setFailure(runID, commitErr)
		}
		return
	}
	cfg.scope = scope
	result, runErr := binding.agent.runStream(scope.ctx, newLoop(binding.agent, nil), record.Task, func(event StreamEvent) {
		r.publish(runID, event)
	}, cfg)
	result, runErr = scope.finalize(result, runErr)
	if transitionErr != nil {
		if markErr := r.markAttentionWithResult(context.Background(), runID, result, "durable transition failed: "+transitionErr.Error()); markErr != nil {
			r.setFailure(runID, errors.Join(transitionErr, markErr))
		}
		return
	}
	if commitErr := r.completeExecution(runID, result, runErr, workerCtx.Err()); commitErr != nil {
		r.setFailure(runID, commitErr)
	}
}

func (r *Runtime) completeExecution(runID string, result RunResult, runErr, workerErr error) error {
	record, err := r.finishExecution(runID, result, runErr, workerErr)
	if err != nil || record.State == RuntimeNeedsAttention {
		return err
	}
	// Committed-run hooks observe the record as committed, including its
	// reassembled transcript.
	full, err := r.load(context.Background(), runID)
	if err != nil {
		return err
	}
	return r.finishHooks(runID, r.invokeCommittedRunHooks(full))
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
		record.Result = cloneRunResult(transition.Result)
		record.Result.Messages = nil
		record.LastTransition = transition.Kind
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
	return r.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.State != RuntimeFinalizing {
			return fmt.Errorf("run %s cannot finish hooks from %s", runID, record.State)
		}
		record.HookResults = append([]RunHookResult(nil), results...)
		record.State = RuntimeTerminal
		record.Generation++
		return putRuntimeRun(tx, record)
	})
}

func (r *Runtime) markAttentionWithResult(ctx context.Context, runID string, result RunResult, reason string) error {
	return r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if err := appendTranscript(tx, runID, &record, result.Messages); err != nil {
			return err
		}
		record.Result = cloneRunResult(result)
		record.Result.Messages = nil
		record.State = RuntimeNeedsAttention
		record.AttentionReason = reason
		record.Generation++
		return putRuntimeRun(tx, record)
	})
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func setRuntimeError(record *storedRuntimeRun, err error) {
	record.Error = errorString(err)
	record.ErrorKind = ""
	record.ErrorStopReason = ""
	record.ErrorRawReason = ""
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		record.ErrorKind = "deadline"
	case errors.Is(err, context.Canceled):
		record.ErrorKind = "cancelled"
	case errors.Is(err, ErrMaxStepsExceeded):
		record.ErrorKind = "max_steps"
	case errors.Is(err, ErrInvalidMaxSteps):
		record.ErrorKind = "invalid_max_steps"
	case errors.Is(err, ErrEmptyResponse):
		record.ErrorKind = "empty_response"
	default:
		var completion *CompletionError
		if errors.As(err, &completion) {
			record.ErrorKind = "completion"
			record.ErrorStopReason = completion.Reason
			record.ErrorRawReason = completion.RawReason
		} else {
			record.ErrorKind = "generic"
		}
	}
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
	return r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.Generation != generation {
			return nil
		}
		record.State = RuntimeNeedsAttention
		record.AttentionReason = reason
		record.Generation++
		return putRuntimeRun(tx, record)
	})
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
			snapshot.AttentionReason = ErrDefinitionNotRegistered.Error()
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
			return lastResult, fmt.Errorf("%w: %s", ErrRunNeedsAttention, snapshot.AttentionReason)
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
	var cancel context.CancelFunc
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
		switch record.State {
		case RuntimeFinalizing, RuntimeTerminal:
			return nil
		case RuntimeReady, RuntimeNeedsAttention:
			record.State = RuntimeTerminal
			record.Result.Status = RunCancelled
			setRuntimeError(&record, context.Canceled)
		case RuntimeRunning:
			record.State = RuntimeCancelRequested
		}
		record.Generation++
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		data, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		if err := tx.Put(runtimeReceiptsBucket, cancelReceiptKey(h.runID), data); err != nil {
			return err
		}
		h.runtime.mu.Lock()
		if live, ok := h.runtime.live[h.runID]; ok {
			cancel = live.cancel
		}
		h.runtime.mu.Unlock()
		return nil
	})
	if err == nil && cancel != nil {
		cancel()
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
	if record.TranscriptMessages == 0 {
		return record, nil
	}
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
	return RunSnapshot{
		RunID: record.RunID, DefinitionID: record.DefinitionID,
		DefinitionRevision: record.DefinitionRevision, State: record.State,
		Result: cloneRunResult(record.Result), Error: record.Error,
		ErrorKind: record.ErrorKind, ErrorStopReason: record.ErrorStopReason,
		ErrorRawReason:  record.ErrorRawReason,
		AttentionReason: record.AttentionReason,
		HookResults:     append([]RunHookResult(nil), record.HookResults...),
	}
}

func snapshotError(snapshot RunSnapshot) error {
	if snapshot.Error == "" {
		return nil
	}
	switch snapshot.ErrorKind {
	case "deadline":
		return context.DeadlineExceeded
	case "cancelled":
		return context.Canceled
	case "max_steps":
		return fmt.Errorf("%s: %w", snapshot.Error, ErrMaxStepsExceeded)
	case "invalid_max_steps":
		return fmt.Errorf("%s: %w", snapshot.Error, ErrInvalidMaxSteps)
	case "empty_response":
		return fmt.Errorf("%s: %w", snapshot.Error, ErrEmptyResponse)
	case "completion":
		return &CompletionError{Reason: snapshot.ErrorStopReason, RawReason: snapshot.ErrorRawReason}
	}
	if snapshot.Result.Status == RunCancelled {
		return context.Canceled
	}
	return errors.New(snapshot.Error)
}
