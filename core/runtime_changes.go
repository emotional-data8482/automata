package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// runtimeChangesBucket is the runtime-wide change feed: one entry per run
	// per commit, keyed by a global sequence.
	runtimeChangesBucket = "runtime_changes"
	// runtimeRunChangesBucket records each run's latest change sequence, so
	// retention can hold runs an external consumer has not synced.
	runtimeRunChangesBucket = "runtime_run_changes"
	// changeHeadMeta holds the feed's head and retained floor.
	changeHeadMeta = "change_head"
)

const (
	defaultChangePageLimit = 256
	maxChangePageLimit     = 1024
)

// RunChange is one entry of the runtime-wide change feed: the committed
// events one storage commit appended to one run. Sequence numbers are global
// to the store, start at one, and increase by one per change, so a consumer
// that indexes or exports runs keeps a single cursor instead of one per run.
type RunChange struct {
	Sequence uint64
	// Time is the wall-clock time of the commit.
	Time  time.Time
	RunID string
	// FirstEvent and LastEvent bound the run's committed events the commit
	// appended (see [RunHandle.Events]); both are zero when Pruned.
	FirstEvent uint64
	LastEvent  uint64
	// State is the run's state after the commit, or empty when the commit
	// did not write the run record (for example a wait or tool outcome).
	State RuntimeState
	// Pruned reports that retention deleted the run (see
	// [RetentionPolicy].Runs).
	Pruned bool
	// Events holds the committed events FirstEvent..LastEvent, transcript
	// messages attached, when the page was read with [WithChangeEvents].
	Events []CommittedEvent
	// EventsPruned reports that events were requested but retention has
	// removed them; read the run's [RunHandle.Snapshot] instead.
	EventsPruned bool
}

// ChangePage is one bounded page of the change feed.
type ChangePage struct {
	Changes []RunChange
	// Next is the cursor for the following page: the last returned sequence,
	// or the requested cursor when the page is empty.
	Next uint64
	// Head is the latest committed change sequence when the page was read.
	Head uint64
}

// ChangeOption configures a change feed read.
type ChangeOption func(*changeOptions)

type changeOptions struct {
	events bool
}

// WithChangeEvents attaches each change's committed events, with their
// transcript messages, to the page. A page carrying payloads is bounded by
// size as well as count and always carries at least one change.
func WithChangeEvents() ChangeOption {
	return func(o *changeOptions) { o.events = true }
}

type storedRunChange struct {
	Version    int          `json:"version"`
	Sequence   uint64       `json:"sequence"`
	Time       time.Time    `json:"time"`
	RunID      string       `json:"run_id"`
	FirstEvent uint64       `json:"first_event,omitempty"`
	LastEvent  uint64       `json:"last_event,omitempty"`
	State      RuntimeState `json:"state,omitempty"`
	Pruned     bool         `json:"pruned,omitempty"`
}

func changeStorageKey(sequence uint64) string { return fmt.Sprintf("%016x", sequence) }

func getChangeHead(tx StoreTransaction) (storedEventHead, error) {
	raw, err := tx.Get(runtimeMetaBucket, changeHeadMeta)
	if errors.Is(err, ErrStoreKeyNotFound) {
		return storedEventHead{}, nil
	}
	if err != nil {
		return storedEventHead{}, err
	}
	var head storedEventHead
	if err := json.Unmarshal(raw, &head); err != nil {
		return storedEventHead{}, fmt.Errorf("decode change feed head: %w", err)
	}
	return head, nil
}

// lastRunChange is the run's latest change sequence, zero for a run with no
// recorded change (stored before the feed existed).
func lastRunChange(tx StoreTransaction, runID string) (uint64, error) {
	raw, err := tx.Get(runtimeRunChangesBucket, runID)
	if errors.Is(err, ErrStoreKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var sequence uint64
	if err := json.Unmarshal(raw, &sequence); err != nil {
		return 0, fmt.Errorf("decode run %s change sequence: %w", runID, err)
	}
	return sequence, nil
}

// appendRunChanges appends one feed entry per committed run change, inside
// the commit that made it.
func appendRunChanges(tx StoreTransaction, commits []runCommit, now time.Time) error {
	if len(commits) == 0 {
		return nil
	}
	head, err := getChangeHead(tx)
	if err != nil {
		return err
	}
	for _, commit := range commits {
		head.Head++
		change := storedRunChange{
			Version: runtimeEncodingVersion, Sequence: head.Head, Time: now, RunID: commit.runID,
			State: commit.state, Pruned: commit.pruned,
		}
		if !commit.pruned {
			change.FirstEvent, change.LastEvent = commit.firstEvent, commit.head
			if err := putStoredJSON(tx, runtimeRunChangesBucket, commit.runID, head.Head); err != nil {
				return err
			}
		}
		if err := putStoredJSON(tx, runtimeChangesBucket, changeStorageKey(head.Head), change); err != nil {
			return err
		}
	}
	return putStoredJSON(tx, runtimeMetaBucket, changeHeadMeta, head)
}

func changePageLimit(limit int) int {
	if limit <= 0 {
		return defaultChangePageLimit
	}
	return min(limit, maxChangePageLimit)
}

// Changes returns runtime-wide changes with a sequence greater than after,
// in commit order, as one bounded page. It never blocks. Pass 0 to read from
// the beginning and ChangePage.Next to continue. A cursor that is no longer
// retained, or is ahead of the head, returns [ErrEventGap]. Changes record
// commits to this store made through any Runtime; runs stored before the
// feed existed have no changes until their next commit.
func (r *Runtime) Changes(ctx context.Context, after uint64, limit int, opts ...ChangeOption) (ChangePage, error) {
	var options changeOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	var page ChangePage
	err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		page, err = readChangePage(tx, after, changePageLimit(limit), options)
		return err
	})
	return page, err
}

// WaitChanges is [Runtime.Changes] that first waits until at least one
// change after the cursor has committed or ctx ends. It wakes on this
// Runtime's commits; commits by another process sharing the store are seen
// on the next wake or call.
func (r *Runtime) WaitChanges(ctx context.Context, after uint64, limit int, opts ...ChangeOption) (ChangePage, error) {
	for {
		changed, closed := r.hub.anyChange()
		page, err := r.Changes(ctx, after, limit, opts...)
		if err != nil || len(page.Changes) > 0 {
			return page, err
		}
		if closed {
			return page, ErrRuntimeClosed
		}
		select {
		case <-ctx.Done():
			return page, ctx.Err()
		case <-changed:
		}
	}
}

func readChangePage(tx StoreTransaction, after uint64, limit int, options changeOptions) (ChangePage, error) {
	page := ChangePage{Next: after}
	head, err := getChangeHead(tx)
	if err != nil {
		return page, err
	}
	page.Head = head.Head
	if after < head.Floor || after > head.Head {
		return page, fmt.Errorf("%w: change feed retains %d..%d, cursor %d", ErrEventGap, head.Floor+1, head.Head, after)
	}
	var stored []storedRunChange
	if _, err := tx.ScanPage(runtimeChangesBucket, "", changeStorageKey(after), limit, func(_ string, raw []byte) error {
		var change storedRunChange
		if err := json.Unmarshal(raw, &change); err != nil {
			return fmt.Errorf("decode run change: %w", err)
		}
		if change.Version != runtimeEncodingVersion {
			return fmt.Errorf("unsupported run change version %d", change.Version)
		}
		stored = append(stored, change)
		return nil
	}); err != nil {
		return page, err
	}
	bytes := 0
	for _, item := range stored {
		change := RunChange{
			Sequence: item.Sequence, Time: item.Time, RunID: item.RunID,
			FirstEvent: item.FirstEvent, LastEvent: item.LastEvent, State: item.State, Pruned: item.Pruned,
		}
		if options.events && !item.Pruned {
			events, size, err := loadEventRange(tx, item.RunID, item.FirstEvent, item.LastEvent)
			if err != nil {
				return page, err
			}
			if len(page.Changes) > 0 && bytes+size > maxEventPageBytes {
				break
			}
			bytes += size
			change.Events = events
			change.EventsPruned = events == nil
		}
		page.Changes = append(page.Changes, change)
		page.Next = item.Sequence
	}
	return page, nil
}

// loadEventRange reads one run's committed events first..last with their
// transcript messages. It returns nil events when retention removed them or
// the run, and the encoded transcript size read.
func loadEventRange(tx StoreTransaction, runID string, first, last uint64) ([]CommittedEvent, int, error) {
	record, err := getRuntimeRun(tx, runID)
	if errors.Is(err, ErrRunPruned) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	head, err := getEventHead(tx, runID)
	if err != nil {
		return nil, 0, err
	}
	if first <= head.Floor {
		return nil, 0, nil
	}
	var stored []storedRunEvent
	if _, err := tx.ScanPage(runtimeEventsBucket, runID+"/", eventStorageKey(runID, first-1), int(last-first+1), func(_ string, raw []byte) error {
		var event storedRunEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return fmt.Errorf("decode run %s event: %w", runID, err)
		}
		stored = append(stored, event)
		return nil
	}); err != nil {
		return nil, 0, err
	}
	events := make([]CommittedEvent, 0, len(stored))
	size := 0
	for _, item := range stored {
		event := committedEventFromStored(runID, item)
		if item.Kind == CommittedMessages && record.HistoryPruned {
			event.MessagesPruned = true
		} else if item.Kind == CommittedMessages {
			messages, n, err := loadTranscriptRange(tx, runID, item.MessageIndex, item.MessageCount)
			if err != nil {
				return nil, 0, err
			}
			size += n
			event.Messages = messages
		}
		events = append(events, event)
	}
	return events, size, nil
}

// pruneChanges removes change feed entries committed before cutoff and, when
// hold is set, at or below synced only. It removes at most limit entries and
// reports whether more are due.
func pruneChanges(tx StoreTransaction, cutoff time.Time, hold bool, synced uint64, limit int) (int, bool, error) {
	head, err := getChangeHead(tx)
	if err != nil {
		return 0, false, err
	}
	var stored []storedRunChange
	if _, err := tx.ScanPage(runtimeChangesBucket, "", changeStorageKey(head.Floor), limit+1, func(_ string, raw []byte) error {
		var change storedRunChange
		if err := json.Unmarshal(raw, &change); err != nil {
			return err
		}
		stored = append(stored, change)
		return nil
	}); err != nil {
		return 0, false, err
	}
	removed := 0
	for _, change := range stored {
		if !change.Time.Before(cutoff) || (hold && change.Sequence > synced) {
			break
		}
		if removed == limit {
			return removed, true, putStoredJSON(tx, runtimeMetaBucket, changeHeadMeta, head)
		}
		if err := tx.Delete(runtimeChangesBucket, changeStorageKey(change.Sequence)); err != nil {
			return 0, false, err
		}
		head.Floor = change.Sequence
		removed++
	}
	if removed == 0 {
		return 0, false, nil
	}
	return removed, false, putStoredJSON(tx, runtimeMetaBucket, changeHeadMeta, head)
}
