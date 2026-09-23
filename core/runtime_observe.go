package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	runtimeEventsBucket     = "runtime_events"
	runtimeEventHeadsBucket = "runtime_event_heads"
	// runtimeActiveBucket indexes every non-terminal run, so recovery and the
	// driver page through the working set instead of every run ever stored.
	runtimeActiveBucket = "runtime_active"
)

const (
	defaultEventPageLimit = 256
	maxEventPageLimit     = 1024
	// maxEventPageBytes bounds the transcript payload one event page attaches.
	// A page always carries at least one event.
	maxEventPageBytes = 4 << 20
)

// ErrEventGap reports an event cursor outside the run's retained committed
// sequence: events after it were pruned, or it is ahead of the committed head
// (for example after restoring an older backup). Resynchronize with
// [RunHandle.Snapshot] and continue from [RunSnapshot.EventSequence].
var ErrEventGap = errors.New("runtime event cursor is outside the retained sequence")

// CommittedEventKind names what a [CommittedEvent] records. Consumers must
// tolerate kinds added later.
type CommittedEventKind string

const (
	// CommittedRunState records a run state change, including a change of
	// attention kind or reason while the run needs attention.
	CommittedRunState CommittedEventKind = "run_state"
	// CommittedMessages records transcript messages appended to the run.
	CommittedMessages CommittedEventKind = "messages"
	// CommittedInvocation records a tool invocation reservation, dispatch,
	// outcome, or effect status change.
	CommittedInvocation CommittedEventKind = "invocation"
	// CommittedWait records a durable wait being created, resolved, expired,
	// or canceled.
	CommittedWait CommittedEventKind = "wait"
)

// CommittedEvent is one durable fact about a run. Runtime appends it in the
// same storage transaction as the state change it describes, so it is never
// provisional: unlike [StreamEvent] deltas, committed events can be replayed
// from a cursor after a disconnect or restart. Sequence numbers are per run,
// start at one, and increase by one per event. One commit can append several
// events; their relative order within the commit is invocations, waits,
// messages, then the run state.
type CommittedEvent struct {
	RunID    string
	Sequence uint64
	// Time is the wall-clock time of the commit that appended the event.
	Time time.Time
	Kind CommittedEventKind

	// State and PreviousState are set for CommittedRunState. PreviousState is
	// empty for the admission event. AttentionKind and Reason describe a
	// RuntimeNeedsAttention state.
	State         RuntimeState
	PreviousState RuntimeState
	AttentionKind string
	Reason        string

	// MessageIndex and MessageCount locate the appended messages in the run
	// transcript (RunResult.Messages) for CommittedMessages. Messages holds
	// them, attached when the page is read rather than stored twice.
	MessageIndex int
	MessageCount int
	Messages     []Message
	// MessagesPruned reports that Messages is nil because retention removed
	// the run's history (see RunSnapshot.HistoryPruned).
	MessagesPruned bool

	// OperationID, Tool, InvocationState, and Effect are set for
	// CommittedInvocation; OperationID and Tool also identify the invocation of
	// a CommittedWait.
	OperationID     string
	Tool            string
	InvocationState ToolInvocationState
	Effect          EffectStatus

	// WaitID, WaitKind, and WaitState are set for CommittedWait.
	WaitID    string
	WaitKind  WaitKind
	WaitState WaitState

	// ChildRunID links an invocation or wait to its durable child run.
	ChildRunID string
}

// EventPage is one bounded page of committed events.
type EventPage struct {
	Events []CommittedEvent
	// Next is the cursor for the following page: the last returned sequence,
	// or the requested cursor when the page is empty.
	Next uint64
	// Head is the run's latest committed sequence when the page was read.
	// Next < Head means more events are already available.
	Head uint64
}

type storedRunEvent struct {
	Version         int                 `json:"version"`
	Sequence        uint64              `json:"sequence"`
	Time            time.Time           `json:"time"`
	Kind            CommittedEventKind  `json:"kind"`
	State           RuntimeState        `json:"state,omitempty"`
	PreviousState   RuntimeState        `json:"previous_state,omitempty"`
	AttentionKind   string              `json:"attention_kind,omitempty"`
	Reason          string              `json:"reason,omitempty"`
	MessageIndex    int                 `json:"message_index,omitempty"`
	MessageCount    int                 `json:"message_count,omitempty"`
	OperationID     string              `json:"operation_id,omitempty"`
	Tool            string              `json:"tool,omitempty"`
	InvocationState ToolInvocationState `json:"invocation_state,omitempty"`
	Effect          EffectStatus        `json:"effect,omitempty"`
	WaitID          string              `json:"wait_id,omitempty"`
	WaitKind        WaitKind            `json:"wait_kind,omitempty"`
	WaitState       WaitState           `json:"wait_state,omitempty"`
	ChildRunID      string              `json:"child_run_id,omitempty"`
}

// storedEventHead is the committed event sequence of one run. Events with a
// sequence at or below Floor were removed by retention.
type storedEventHead struct {
	Head  uint64 `json:"head"`
	Floor uint64 `json:"floor,omitempty"`
}

func eventStorageKey(runID string, sequence uint64) string {
	return fmt.Sprintf("%s/%016x", runID, sequence)
}

func getEventHead(tx StoreTransaction, runID string) (storedEventHead, error) {
	raw, err := tx.Get(runtimeEventHeadsBucket, runID)
	if errors.Is(err, ErrStoreKeyNotFound) {
		return storedEventHead{}, nil
	}
	if err != nil {
		return storedEventHead{}, err
	}
	var head storedEventHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return storedEventHead{}, fmt.Errorf("decode run %s event head: %w", runID, err)
	}
	return head, nil
}

// --- commit wrapper ------------------------------------------------------------

// commitTx wraps every writable Runtime transaction. It records the before and
// after images of run, invocation, and wait records and, just before the
// transaction commits, derives the committed events those writes imply and
// appends them in the same transaction. Deriving events from the writes
// themselves means no transition site can commit a change without its event.
type commitTx struct {
	StoreTransaction
	images map[commitImageKey]*commitImage
	order  []commitImageKey
	// maxPayload bounds each transcript chunk and invocation record written.
	maxPayload int
}

type commitImageKey struct{ bucket, key string }

type commitImage struct {
	before  []byte // nil when the key did not exist before the transaction
	after   []byte
	deleted bool
}

func newCommitTx(tx StoreTransaction, maxPayload int) *commitTx {
	return &commitTx{StoreTransaction: tx, images: make(map[commitImageKey]*commitImage), maxPayload: maxPayload}
}

func observedBucket(bucket string) bool {
	switch bucket {
	case runtimeRunsBucket, runtimeInvocationsBucket, runtimeWaitsBucket:
		return true
	}
	return false
}

func (tx *commitTx) image(bucket, key string) (*commitImage, error) {
	if !observedBucket(bucket) {
		return nil, nil
	}
	k := commitImageKey{bucket, key}
	if image, ok := tx.images[k]; ok {
		return image, nil
	}
	before, err := tx.StoreTransaction.Get(bucket, key)
	if errors.Is(err, ErrStoreKeyNotFound) {
		before, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	image := &commitImage{before: before}
	tx.images[k] = image
	tx.order = append(tx.order, k)
	return image, nil
}

// payloadTooLargeError is ErrPayloadTooLarge with the size the rejected
// payload needed, so recovery can tell when a raised limit covers it.
type payloadTooLargeError struct {
	bucket, key string
	size, limit int
}

func (e *payloadTooLargeError) Error() string {
	return fmt.Sprintf("%v: %s %s is %d bytes, over the %d-byte limit", ErrPayloadTooLarge, e.bucket, e.key, e.size, e.limit)
}

func (e *payloadTooLargeError) Is(target error) bool { return target == ErrPayloadTooLarge }

func (tx *commitTx) Put(bucket, key string, value []byte) error {
	if (bucket == runtimeFactsBucket || bucket == runtimeInvocationsBucket) && tx.maxPayload > 0 && len(value) > tx.maxPayload {
		return &payloadTooLargeError{bucket: bucket, key: key, size: len(value), limit: tx.maxPayload}
	}
	image, err := tx.image(bucket, key)
	if err != nil {
		return err
	}
	if err := tx.StoreTransaction.Put(bucket, key, value); err != nil {
		return err
	}
	if image != nil {
		image.after, image.deleted = append([]byte(nil), value...), false
	}
	return nil
}

func (tx *commitTx) Delete(bucket, key string) error {
	image, err := tx.image(bucket, key)
	if err != nil {
		return err
	}
	if err := tx.StoreTransaction.Delete(bucket, key); err != nil {
		return err
	}
	if image != nil {
		image.after, image.deleted = nil, true
	}
	return nil
}

// runCommit summarizes one run's committed change for post-commit wakeups.
type runCommit struct {
	runID string
	head  uint64
	// state is set when the run record itself was written.
	state    RuntimeState
	previous RuntimeState
	record   *runEventView
	// waitExpiry is the earliest expiry of a wait created by this commit.
	waitExpiry time.Time
	// pruned reports that retention deleted the run record.
	pruned bool
}

// runEventView decodes only the run-record fields event derivation reads.
type runEventView struct {
	State              RuntimeState `json:"state"`
	TranscriptMessages int          `json:"transcript_messages"`
	TranscriptBase     int          `json:"transcript_base_messages,omitempty"`
	AttentionKind      string       `json:"attention_kind,omitempty"`
	AttentionReason    string       `json:"attention_reason,omitempty"`
	ParentRunID        string       `json:"parent_run_id,omitempty"`
	Deadline           time.Time    `json:"deadline,omitempty"`
}

type invocationEventView struct {
	OperationID string                `json:"operation_id"`
	Call        struct{ Name string } `json:"call"`
	State       ToolInvocationState   `json:"state"`
	Effect      EffectReport          `json:"effect"`
	ChildRunID  string                `json:"child_run_id,omitempty"`
}

type waitEventView struct {
	ID          string    `json:"id"`
	Kind        WaitKind  `json:"kind"`
	State       WaitState `json:"state"`
	OperationID string    `json:"operation_id"`
	Tool        string    `json:"tool"`
	ChildRunID  string    `json:"child_run_id,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
}

func decodeView[T any](raw []byte) (*T, error) {
	if raw == nil {
		return nil, nil
	}
	var view T
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

func runIDFromStorageKey(bucket, key string) string {
	if bucket == runtimeRunsBucket {
		return key
	}
	runID, _, _ := strings.Cut(key, "/")
	return runID
}

type pendingRunEvents struct {
	admission   []storedRunEvent
	invocations []storedRunEvent
	waits       []storedRunEvent
	messages    []storedRunEvent
	states      []storedRunEvent
	commit      runCommit
}

// flush appends the committed events implied by this transaction's writes and
// returns one summary per affected run, in first-write order.
func (tx *commitTx) flush(now time.Time) ([]runCommit, error) {
	if len(tx.order) == 0 {
		return nil, nil
	}
	keys := slices.Clone(tx.order)
	// Deterministic order within each category: storage key order, which is
	// model order for invocations.
	slices.SortStableFunc(keys, func(a, b commitImageKey) int { return strings.Compare(a.key, b.key) })
	pending := make(map[string]*pendingRunEvents)
	var runs []string
	entry := func(runID string) *pendingRunEvents {
		p := pending[runID]
		if p == nil {
			p = &pendingRunEvents{commit: runCommit{runID: runID}}
			pending[runID] = p
			runs = append(runs, runID)
		}
		return p
	}
	for _, k := range keys {
		image := tx.images[k]
		runID := runIDFromStorageKey(k.bucket, k.key)
		if image.deleted {
			// Retention removes records together with their event logs; the
			// deleted run record still wakes its waiters.
			if k.bucket == runtimeRunsBucket {
				entry(runID).commit.pruned = true
			}
			continue
		}
		switch k.bucket {
		case runtimeRunsBucket:
			before, err := decodeView[runEventView](image.before)
			if err != nil {
				return nil, fmt.Errorf("decode run %s before image: %w", runID, err)
			}
			after, err := decodeView[runEventView](image.after)
			if err != nil {
				return nil, fmt.Errorf("decode run %s after image: %w", runID, err)
			}
			p := entry(runID)
			p.commit.state, p.commit.record = after.State, after
			from := after.TranscriptBase
			if before != nil {
				p.commit.previous = before.State
				from = max(from, before.TranscriptMessages)
			}
			if count := after.TranscriptMessages - from; count > 0 {
				p.messages = append(p.messages, storedRunEvent{Kind: CommittedMessages, MessageIndex: from, MessageCount: count})
			}
			if before == nil || before.State != after.State ||
				(after.State == RuntimeNeedsAttention && (before.AttentionKind != after.AttentionKind || before.AttentionReason != after.AttentionReason)) {
				event := storedRunEvent{Kind: CommittedRunState, State: after.State}
				if before != nil {
					event.PreviousState = before.State
				}
				if after.State == RuntimeNeedsAttention {
					event.AttentionKind, event.Reason = after.AttentionKind, after.AttentionReason
				}
				if before == nil {
					// Admission precedes the messages a conversation turn seeds.
					p.admission = append(p.admission, event)
				} else {
					p.states = append(p.states, event)
				}
			}
		case runtimeInvocationsBucket:
			before, err := decodeView[invocationEventView](image.before)
			if err != nil {
				return nil, fmt.Errorf("decode invocation %s before image: %w", k.key, err)
			}
			after, err := decodeView[invocationEventView](image.after)
			if err != nil {
				return nil, fmt.Errorf("decode invocation %s after image: %w", k.key, err)
			}
			if before != nil && before.State == after.State && before.Effect.Status == after.Effect.Status && before.ChildRunID == after.ChildRunID {
				continue
			}
			p := entry(runID)
			p.invocations = append(p.invocations, storedRunEvent{
				Kind: CommittedInvocation, OperationID: after.OperationID, Tool: after.Call.Name,
				InvocationState: after.State, Effect: after.Effect.Status, ChildRunID: after.ChildRunID,
			})
		case runtimeWaitsBucket:
			before, err := decodeView[waitEventView](image.before)
			if err != nil {
				return nil, fmt.Errorf("decode wait %s before image: %w", k.key, err)
			}
			after, err := decodeView[waitEventView](image.after)
			if err != nil {
				return nil, fmt.Errorf("decode wait %s after image: %w", k.key, err)
			}
			if before != nil && before.State == after.State {
				continue
			}
			p := entry(runID)
			p.waits = append(p.waits, storedRunEvent{
				Kind: CommittedWait, WaitID: after.ID, WaitKind: after.Kind, WaitState: after.State,
				OperationID: after.OperationID, Tool: after.Tool, ChildRunID: after.ChildRunID,
			})
			if before == nil && after.State == WaitPending && !after.ExpiresAt.IsZero() &&
				(p.commit.waitExpiry.IsZero() || after.ExpiresAt.Before(p.commit.waitExpiry)) {
				p.commit.waitExpiry = after.ExpiresAt
			}
		}
	}
	commits := make([]runCommit, 0, len(runs))
	for _, runID := range runs {
		p := pending[runID]
		events := slices.Concat(p.admission, p.invocations, p.waits, p.messages, p.states)
		if p.commit.pruned {
			commits = append(commits, p.commit)
			continue
		}
		if len(events) == 0 {
			continue
		}
		head, err := getEventHead(tx.StoreTransaction, runID)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			head.Head++
			event.Version = runtimeEncodingVersion
			event.Sequence = head.Head
			event.Time = now
			if err := putStoredJSON(tx.StoreTransaction, runtimeEventsBucket, eventStorageKey(runID, event.Sequence), event); err != nil {
				return nil, err
			}
		}
		if err := putStoredJSON(tx.StoreTransaction, runtimeEventHeadsBucket, runID, head); err != nil {
			return nil, err
		}
		p.commit.head = head.Head
		commits = append(commits, p.commit)
	}
	return commits, nil
}

// --- active and retention indexes ------------------------------------------------

// syncRunIndexes keeps the active and retention indexes consistent with a run
// record being written. The active entry exists exactly while the run is not
// terminal; leaving the active index is the one transition into the retention
// indexes, so a run is indexed for retention once, at its terminal commit.
func syncRunIndexes(tx StoreTransaction, record storedRuntimeRun) error {
	_, err := tx.Get(runtimeActiveBucket, record.RunID)
	active := err == nil
	if err != nil && !errors.Is(err, ErrStoreKeyNotFound) {
		return err
	}
	if record.State != RuntimeTerminal {
		if active {
			return nil
		}
		return tx.Put(runtimeActiveBucket, record.RunID, nil)
	}
	if !active {
		return nil
	}
	if err := tx.Delete(runtimeActiveBucket, record.RunID); err != nil {
		return err
	}
	key := retentionIndexKey(time.Now(), record.RunID)
	for _, bucket := range []string{runtimeRetainEventsBucket, runtimeRetainHistoryBucket, runtimeRetainRunsBucket} {
		if err := tx.Put(bucket, key, nil); err != nil {
			return err
		}
	}
	return nil
}

// --- commit hub ------------------------------------------------------------------

// commitHub wakes in-process waiters when a run's committed state may have
// changed. Each commit also leaves the run's committed state and event head
// on the signal, taken from the commit's own after-image, so a waiter can
// skip storage reads for transitions that cannot end its wait. A wake is only
// a hint: if a commit's outcome was unknown, the waiter's next read settles
// it, and a missed wake cannot happen because a waiter subscribes before it
// reads.
type commitHub struct {
	mu      sync.Mutex
	closed  bool
	signals map[string]*commitSignal
}

type commitSignal struct {
	ch   chan struct{}
	refs int
	// state is the run's latest committed state notified while this signal
	// existed, or empty when unknown; head is the event head of that commit.
	// A notification is applied only if its head is newer, so notifications
	// delivered out of commit order never roll the state back.
	state RuntimeState
	head  uint64
	// gone records that retention deleted the run.
	gone bool
}

var closedSignal = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

func newCommitHub() *commitHub {
	return &commitHub{signals: make(map[string]*commitSignal)}
}

// commitSubscription is one waiter's continuous registration on a run.
type commitSubscription struct {
	hub    *commitHub
	runID  string
	signal *commitSignal
	once   sync.Once
}

// commitView is what a waiter knows between reads: a channel closed by the
// next commit on the run, the latest committed state and event head the hub
// saw (empty and zero when unknown), and whether the run was deleted or the
// Runtime closed.
type commitView struct {
	changed <-chan struct{}
	state   RuntimeState
	head    uint64
	gone    bool
	closed  bool
}

// subscribe registers a waiter on runID until close.
func (h *commitHub) subscribe(runID string) *commitSubscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub := &commitSubscription{hub: h, runID: runID}
	if h.closed {
		return sub
	}
	signal := h.signals[runID]
	if signal == nil {
		signal = &commitSignal{ch: make(chan struct{})}
		h.signals[runID] = signal
	}
	signal.refs++
	sub.signal = signal
	return sub
}

func (s *commitSubscription) next() commitView {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if s.signal == nil || s.hub.closed {
		return commitView{changed: closedSignal, closed: true}
	}
	return commitView{changed: s.signal.ch, state: s.signal.state, head: s.signal.head, gone: s.signal.gone}
}

func (s *commitSubscription) close() {
	s.once.Do(func() {
		if s.signal == nil {
			return
		}
		s.hub.mu.Lock()
		defer s.hub.mu.Unlock()
		s.signal.refs--
		if s.signal.refs == 0 && s.hub.signals[s.runID] == s.signal {
			delete(s.hub.signals, s.runID)
		}
	})
}

// notify wakes the waiters of each committed run and records what the commit
// made of it.
func (h *commitHub) notify(commits []runCommit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for _, commit := range commits {
		signal := h.signals[commit.runID]
		if signal == nil {
			continue
		}
		if commit.pruned {
			signal.gone = true
		} else if commit.head > signal.head {
			signal.head = commit.head
			if commit.state != "" {
				signal.state = commit.state
			}
		}
		close(signal.ch)
		signal.ch = make(chan struct{})
	}
}

// wake wakes the waiters of runID without new committed facts, for
// in-process changes such as a recorded worker failure.
func (h *commitHub) wake(runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if signal := h.signals[runID]; signal != nil {
		close(signal.ch)
		signal.ch = make(chan struct{})
	}
}

// close wakes every waiter and makes later waits report the closed Runtime
// instead of blocking.
func (h *commitHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, signal := range h.signals {
		close(signal.ch)
	}
}

// mayHaveSettled reports whether a committed state could end a wait for the
// run to finish: terminal, attention, or ready (whose binding may be
// unregistered). An unknown state must be read.
func mayHaveSettled(state RuntimeState) bool {
	switch state {
	case RuntimeRunning, RuntimeWaiting, RuntimeCancelRequested, RuntimeFinalizing:
		return false
	}
	return true
}

// afterCommit wakes waiters on the runs a writable transaction changed and
// schedules the driver work those changes imply.
func (r *Runtime) afterCommit(commits []runCommit) {
	r.hub.notify(commits)
	r.observeCommits(commits)
}

// --- committed event reads -------------------------------------------------------

var errStopScan = errors.New("stop scan")

func eventPageLimit(limit int) int {
	if limit <= 0 {
		return defaultEventPageLimit
	}
	return min(limit, maxEventPageLimit)
}

// Events returns committed events with a sequence greater than after, in
// order, as one bounded page. It never blocks. Pass 0 to read from the
// beginning and EventPage.Next to continue. A cursor that is no longer
// retained, or is ahead of the committed head, returns [ErrEventGap].
func (h *RunHandle) Events(ctx context.Context, after uint64, limit int) (EventPage, error) {
	var page EventPage
	err := h.runtime.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		page, err = readEventPage(tx, h.runID, after, eventPageLimit(limit))
		return err
	})
	return page, err
}

// WaitEvents is [RunHandle.Events] that first waits until at least one event
// after the cursor has committed or ctx ends. Waiting holds no storage reads:
// it wakes on the run's committed transitions. A slow or absent consumer
// never delays commits, because events are read from storage on demand rather
// than buffered for delivery.
func (h *RunHandle) WaitEvents(ctx context.Context, after uint64, limit int) (EventPage, error) {
	sub := h.runtime.hub.subscribe(h.runID)
	defer sub.close()
	read := true
	for {
		view := sub.next()
		if read || view.gone || view.closed || view.head > after {
			page, err := h.Events(ctx, after, limit)
			if err != nil || len(page.Events) > 0 {
				return page, err
			}
			if view.closed {
				return page, ErrRuntimeClosed
			}
		}
		read = false
		select {
		case <-ctx.Done():
			return EventPage{Next: after, Head: view.head}, ctx.Err()
		case <-view.changed:
		}
	}
}

func readEventPage(tx StoreTransaction, runID string, after uint64, limit int) (EventPage, error) {
	page := EventPage{Next: after}
	record, err := getRuntimeRun(tx, runID)
	if err != nil {
		return page, err
	}
	head, err := getEventHead(tx, runID)
	if err != nil {
		return page, err
	}
	page.Head = head.Head
	if after < head.Floor || after > head.Head {
		return page, fmt.Errorf("%w: run %s retains events %d..%d, cursor %d", ErrEventGap, runID, head.Floor+1, head.Head, after)
	}
	var stored []storedRunEvent
	if _, err := tx.ScanPage(runtimeEventsBucket, runID+"/", eventStorageKey(runID, after), limit, func(_ string, raw []byte) error {
		var event storedRunEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return fmt.Errorf("decode run %s event: %w", runID, err)
		}
		if event.Version != runtimeEncodingVersion {
			return fmt.Errorf("unsupported runtime event version %d", event.Version)
		}
		stored = append(stored, event)
		return nil
	}); err != nil {
		return page, err
	}
	// Attach transcript payloads after the event scan closes, so no adapter
	// has to serve a read inside an open scan.
	bytes := 0
	for _, item := range stored {
		event := committedEventFromStored(runID, item)
		if item.Kind == CommittedMessages && record.HistoryPruned {
			event.MessagesPruned = true
		} else if item.Kind == CommittedMessages {
			messages, size, err := loadTranscriptRange(tx, runID, item.MessageIndex, item.MessageCount)
			if err != nil {
				return page, err
			}
			if len(page.Events) > 0 && bytes+size > maxEventPageBytes {
				break
			}
			bytes += size
			event.Messages = messages
		}
		page.Events = append(page.Events, event)
		page.Next = item.Sequence
	}
	return page, nil
}

func committedEventFromStored(runID string, stored storedRunEvent) CommittedEvent {
	return CommittedEvent{
		RunID: runID, Sequence: stored.Sequence, Time: stored.Time, Kind: stored.Kind,
		State: stored.State, PreviousState: stored.PreviousState,
		AttentionKind: stored.AttentionKind, Reason: stored.Reason,
		MessageIndex: stored.MessageIndex, MessageCount: stored.MessageCount,
		OperationID: stored.OperationID, Tool: stored.Tool,
		InvocationState: stored.InvocationState, Effect: stored.Effect,
		WaitID: stored.WaitID, WaitKind: stored.WaitKind, WaitState: stored.WaitState,
		ChildRunID: stored.ChildRunID,
	}
}

// loadTranscriptRange reads count messages starting at index from one run's
// own transcript chunks. Chunks are keyed by their first message index and
// every committed messages event starts at a chunk boundary, so the read seeks
// directly to the first chunk. It returns the encoded size read.
func loadTranscriptRange(tx StoreTransaction, runID string, from, count int) ([]Message, int, error) {
	messages := make([]Message, 0, count)
	size := 0
	after := runID + "/"
	if from > 0 {
		after = transcriptFactKey(runID, from-1)
	}
	_, err := tx.ScanPage(runtimeFactsBucket, runID+"/", after, 0, func(key string, raw []byte) error {
		if key != transcriptFactKey(runID, from+len(messages)) {
			return fmt.Errorf("%w: run %s transcript has no chunk starting at message %d", ErrPayloadUnavailable, runID, from+len(messages))
		}
		chunk, err := decodeTranscriptChunk(runID, raw)
		if err != nil {
			return err
		}
		size += len(raw)
		messages = append(messages, chunk...)
		if len(messages) >= count {
			return errStopScan
		}
		return nil
	})
	if errors.Is(err, errStopScan) {
		err = nil
	}
	if err != nil {
		return nil, 0, err
	}
	if len(messages) < count {
		return nil, 0, fmt.Errorf("%w: run %s transcript has %d messages from %d, event expects %d", ErrPayloadUnavailable, runID, len(messages), from, count)
	}
	return messages[:count], size, nil
}
