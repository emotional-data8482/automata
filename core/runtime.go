package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrDefinitionNotRegistered = errors.New("runtime definition is not registered")
	ErrDefinitionConflict      = errors.New("runtime definition registration conflicts")
	ErrAdmissionConflict       = errors.New("submission identity reused with different input")
	ErrRunNotFound             = errors.New("runtime run not found")
	ErrRunNeedsAttention       = errors.New("runtime run needs attention")
	ErrRuntimeClosed           = errors.New("runtime is closed")
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

// DefinitionRef identifies a registered definition revision. [Runtime.Register]
// returns it; runs, child tools, and conversations pin it.
type DefinitionRef struct {
	ID       string
	Revision string
}

type definitionBinding struct {
	agent  *Agent
	source *Agent
}

// Runtime runs registered [Agent] definitions. It admits each task durably
// before any work, owns worker lifetimes independently of callers, persists
// every transition, and recovers after restart. It is the only way to execute
// an Agent. Construct it with [NewRuntime] and a [Store], or with
// [NewEphemeralRuntime] when losing runs on process exit is intended.
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

// NewRuntime opens a runtime on config.Store. Storage that cannot be opened
// or holds an unsupported encoding fails construction; Runtime never falls
// back to memory. The runtime owns the store and closes it in [Runtime.Close].
// Call [Runtime.Recover] after registering definitions to resume work a
// previous process left.
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

// NewEphemeralRuntime returns a runtime on a fresh in-memory store, for tests,
// scripts, and runs whose loss on process exit is intended. It has the same
// lifecycle as a persistent runtime; only its storage is volatile. Use
// NewRuntime with [NewMemoryStore] to configure hooks or an authorizer.
func NewEphemeralRuntime() (*Runtime, error) {
	return NewRuntime(context.Background(), RuntimeConfig{Store: NewMemoryStore()})
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

// Register binds an immutable Agent to a definition ID and revision and
// returns the reference that runs, child tools, and conversations pin. The
// revision is a host attestation: register a new revision whenever the
// Agent's behavior changes. Re-registering the same Agent is idempotent;
// another Agent under the same ID and revision returns
// [ErrDefinitionConflict]. Register every definition a store's runs pin
// before calling [Runtime.Recover].
func (r *Runtime) Register(definitionID, revision string, agent *Agent) (DefinitionRef, error) {
	ref := DefinitionRef{ID: definitionID, Revision: revision}
	if definitionID == "" || revision == "" || agent == nil || strings.ContainsRune(definitionID, '\x00') || strings.ContainsRune(revision, '\x00') {
		return DefinitionRef{}, fmt.Errorf("definition id, revision, and agent are required")
	}
	for _, tool := range agent.tools {
		policy, configured, err := waitPolicyFor(tool)
		if err != nil {
			return DefinitionRef{}, err
		}
		if configured && policy.Kind == WaitApproval && r.authorizer == nil {
			return DefinitionRef{}, errors.New("durable approval requires a runtime authorizer")
		}
		name := ""
		if tool != nil {
			name = tool.Definition().Name
		}
		isChild, err := isChildTool(tool)
		if err != nil {
			return DefinitionRef{}, fmt.Errorf("tool %q: %w", name, err)
		}
		// Child declarations are validated structurally here; the pinned
		// child binding itself is checked fail-closed at admission.
		if isChild {
			// A per-call timeout or rate limiter cannot honestly bound a
			// durable child run that outlives the parent's worker. The child
			// inherits the parent deadline and enforces its own policy.
			limits := agent.toolPolicy.limitsFor(name)
			if limits.Timeout > 0 || limits.RateLimiter != nil {
				return DefinitionRef{}, fmt.Errorf("tool %q: child tools do not support per-call timeout or rate limiting; the child run inherits the parent deadline and enforces its own policy locally", name)
			}
		}
	}
	key := bindingKey(definitionID, revision)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing || r.closed {
		return DefinitionRef{}, ErrRuntimeClosed
	}
	if existing, ok := r.bindings[key]; ok {
		if existing.source == agent {
			return ref, nil
		}
		return DefinitionRef{}, ErrDefinitionConflict
	}
	r.bindings[key] = definitionBinding{agent: cloneAgentDefinition(agent), source: agent}
	return ref, nil
}

func cloneAgentDefinition(agent *Agent) *Agent {
	frozen := *agent
	frozen.tools = append([]Tool(nil), agent.tools...)
	frozen.callOptions = cloneCallOptions(agent.callOptions)
	frozen.toolPolicy = agent.toolPolicy.clone()
	frozen.preSendHooks = append([]PreSendHook(nil), agent.preSendHooks...)
	return &frozen
}

func bindingKey(id, revision string) string { return id + "\x00" + revision }

func (r *Runtime) binding(ref DefinitionRef) (definitionBinding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing || r.closed {
		return definitionBinding{}, ErrRuntimeClosed
	}
	binding, ok := r.bindings[bindingKey(ref.ID, ref.Revision)]
	if !ok {
		return definitionBinding{}, ErrDefinitionNotRegistered
	}
	return binding, nil
}

// Close stops the runtime: it cancels worker contexts, waits for workers to
// yield at a safe boundary, stops the background driver, and closes the
// store. A run interrupted mid-segment is recovered by the next process's
// [Runtime.Recover]; closing never cancels a run logically.
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

func (r *Runtime) registered(record storedRuntimeRun) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.bindings[bindingKey(record.DefinitionID, record.DefinitionRevision)]
	return ok
}
