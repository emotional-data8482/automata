package core

import (
	"container/heap"
	"context"
	"encoding/json"
	"sync"
	"time"
)

// runtimeRepairDelay is how long the driver waits after a child's terminal or
// attention commit before checking that the parent consumed it. The commit's
// own post-commit wake normally wins; the driver repairs a lost one. Tests
// shorten it.
var runtimeRepairDelay = time.Second

// runtimeDriver keeps suspended work moving without a host-driven Recover.
// Commits feed it: a suspended run's logical deadline, a new wait's expiry,
// and a child's terminal or attention commit (which schedules a check of the
// parent) become due times in a heap. One goroutine owned by the Runtime pops
// due runs and applies the same suspended-run checks as Recover. It holds no
// durable state: after a restart, Recover rearms the timers of the previous
// owner's suspended runs.
type runtimeDriver struct {
	mu      sync.Mutex
	due     dueHeap
	pending map[string]time.Time
	wake    chan struct{}
}

type dueEntry struct {
	at    time.Time
	runID string
}

type dueHeap []dueEntry

func (h dueHeap) Len() int           { return len(h) }
func (h dueHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h dueHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *dueHeap) Push(x any)        { *h = append(*h, x.(dueEntry)) }
func (h *dueHeap) Pop() any {
	old := *h
	entry := old[len(old)-1]
	*h = old[:len(old)-1]
	return entry
}

func newRuntimeDriver() *runtimeDriver {
	return &runtimeDriver{pending: make(map[string]time.Time), wake: make(chan struct{}, 1)}
}

// schedule makes runID due at at, keeping only the earliest pending time per
// run. A check that finds the run still suspended reschedules its next due
// time, so dropping a later time loses nothing.
func (d *runtimeDriver) schedule(runID string, at time.Time) {
	d.mu.Lock()
	if existing, ok := d.pending[runID]; ok && !at.Before(existing) {
		d.mu.Unlock()
		return
	}
	d.pending[runID] = at
	heap.Push(&d.due, dueEntry{at: at, runID: runID})
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// popDue removes and returns the runs due at or before now, and the next due
// time after them (zero when nothing is scheduled).
func (d *runtimeDriver) popDue(now time.Time) ([]string, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var due []string
	for d.due.Len() > 0 {
		entry := d.due[0]
		if current, ok := d.pending[entry.runID]; !ok || !current.Equal(entry.at) {
			heap.Pop(&d.due) // superseded by an earlier schedule
			continue
		}
		if entry.at.After(now) {
			return due, entry.at
		}
		heap.Pop(&d.due)
		delete(d.pending, entry.runID)
		due = append(due, entry.runID)
	}
	return due, time.Time{}
}

// runDriver is the driver goroutine. It stops when the Runtime closes.
func (r *Runtime) runDriver() {
	defer r.wg.Done()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		due, next := r.driver.popDue(time.Now())
		for _, runID := range due {
			if r.ctx.Err() != nil {
				return
			}
			r.maintainRun(r.ctx, runID)
		}
		if len(due) > 0 {
			continue
		}
		var fire <-chan time.Time
		if !next.IsZero() {
			timer.Reset(time.Until(next))
			fire = timer.C
		}
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return
		case <-r.driver.wake:
		case <-fire:
		}
		timer.Stop()
	}
}

// observeCommits schedules driver work implied by committed run changes.
func (r *Runtime) observeCommits(commits []runCommit) {
	now := time.Now()
	for _, commit := range commits {
		if !commit.waitExpiry.IsZero() {
			r.driver.schedule(commit.runID, commit.waitExpiry)
		}
		record := commit.record
		if record == nil {
			continue
		}
		if suspendedForDeadline(storedRuntimeRun{State: record.State, AttentionKind: record.AttentionKind}) && !record.Deadline.IsZero() {
			r.driver.schedule(commit.runID, record.Deadline)
		}
		if record.ParentRunID != "" && commit.previous != record.State &&
			(record.State == RuntimeTerminal || record.State == RuntimeNeedsAttention) {
			r.driver.schedule(record.ParentRunID, now.Add(runtimeRepairDelay))
		}
	}
}

// maintainRun applies the suspended-run checks to one run and rearms its next
// due time. Errors are left for the next trigger or Recover: the driver is a
// backstop and never records a worker failure of its own.
func (r *Runtime) maintainRun(ctx context.Context, runID string) {
	r.mu.Lock()
	_, live := r.live[runID]
	r.mu.Unlock()
	if live {
		return
	}
	record, err := r.compactRecord(ctx, runID)
	if err != nil || !suspendedForDeadline(record) {
		return
	}
	if err := r.maintainSuspended(ctx, record); err != nil {
		return
	}
	_ = r.armSuspended(ctx, runID)
}

func (r *Runtime) compactRecord(ctx context.Context, runID string) (storedRuntimeRun, error) {
	var record storedRuntimeRun
	err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		record, err = getRuntimeRun(tx, runID)
		return err
	})
	return record, err
}

// maintainSuspended applies, in order, the checks for a run suspended without
// a worker: its logical deadline, its linked children's outcomes, and the
// expiry of its waits. Recover and the driver share it.
func (r *Runtime) maintainSuspended(ctx context.Context, record storedRuntimeRun) error {
	finalized, err := r.expireWaitingDeadline(ctx, record.RunID)
	if err != nil || finalized {
		return err
	}
	// Child waits are rechecked before generic wait expiry: a linked child may
	// have terminalized without its wake reaching this run, or be ready to
	// start after registration.
	ready, err := r.reconcileChildWaits(ctx, record.RunID)
	if err != nil {
		return err
	}
	if ready {
		r.start(record.RunID)
		return nil
	}
	if record.State != RuntimeWaiting {
		return nil
	}
	ready, err = r.expireRunWaits(ctx, record.RunID)
	if err != nil {
		return err
	}
	if ready {
		r.start(record.RunID)
	}
	return nil
}

// armSuspended schedules a still-suspended run for its next due time: the
// earlier of its logical deadline and, while it is waiting, its first pending
// wait expiry (maintainSuspended expires waits only for waiting runs). A due
// time that is not in the future is deferred by runtimeRepairDelay: the checks
// just ran, so rearming it at once could only spin.
func (r *Runtime) armSuspended(ctx context.Context, runID string) error {
	var next time.Time
	err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil || !suspendedForDeadline(record) {
			return err
		}
		next = record.Deadline
		if record.State != RuntimeWaiting {
			return nil
		}
		return tx.Scan(runtimeWaitsBucket, runID+"/", func(_ string, raw []byte) error {
			var wait waitEventView
			if err := json.Unmarshal(raw, &wait); err != nil {
				return err
			}
			if wait.State == WaitPending && !wait.ExpiresAt.IsZero() && (next.IsZero() || wait.ExpiresAt.Before(next)) {
				next = wait.ExpiresAt
			}
			return nil
		})
	})
	if err == nil && !next.IsZero() {
		if now := time.Now(); !next.After(now) {
			next = now.Add(runtimeRepairDelay)
		}
		r.driver.schedule(runID, next)
	}
	return err
}
