package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// runtimeRunListBucket indexes every run by admission time, so listing
	// pages through runs in a stable order without reading every record.
	runtimeRunListBucket = "runtime_run_list"
	// runListIndexedMeta marks a store whose run list index covers every
	// run, including runs admitted before the index existed.
	runListIndexedMeta = "run_list_indexed"
	// runListClockMeta holds the latest admission time, so admission times
	// (and the index keys built from them) only move forward.
	runListClockMeta = "run_list_clock"
)

// Label bounds keep labels small enough to live on the compact run record.
const (
	maxRunLabels          = 32
	maxRunLabelKeyBytes   = 128
	maxRunLabelValueBytes = 512
)

const (
	defaultRunPageLimit = 100
	maxRunPageLimit     = 1000
	// maxRunListScan bounds the index entries one Runs call reads, so a
	// filter that matches little cannot turn one call into a full scan.
	maxRunListScan = 4096
)

// WithLabels attaches host annotations to the run, for example a schedule,
// actor, or request ID, so the host can correlate runs with its own records
// and filter [Runtime.Runs] by them. Labels are stored with the run and
// inherited by its durable child runs. They are not part of the admission
// identity: an exact retry of an idempotent admission returns the original
// run with its original labels. At most 32 labels are allowed; keys must be
// non-empty, at most 128 bytes, and values at most 512 bytes, all valid UTF-8
// without NUL. Repeated options merge, later values winning.
func WithLabels(labels map[string]string) SubmitOption {
	return func(o *submitOptions) {
		if len(labels) == 0 {
			return
		}
		if o.Labels == nil {
			o.Labels = make(map[string]string, len(labels))
		}
		maps.Copy(o.Labels, labels)
	}
}

func validateLabels(labels map[string]string) error {
	if len(labels) > maxRunLabels {
		return fmt.Errorf("run has %d labels, over the limit of %d", len(labels), maxRunLabels)
	}
	for key, value := range labels {
		if key == "" || len(key) > maxRunLabelKeyBytes || !utf8.ValidString(key) || strings.ContainsRune(key, '\x00') {
			return fmt.Errorf("invalid run label key %q", key)
		}
		if len(value) > maxRunLabelValueBytes || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid value for run label %q", key)
		}
	}
	return nil
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	return maps.Clone(labels)
}

func runListKey(record storedRuntimeRun) string {
	return runListTimeKey(record.CreatedAt, record.RunID)
}

// runListTimeKey orders by admission time; a zero time sorts first.
func runListTimeKey(at time.Time, runID string) string {
	if at.IsZero() {
		return fmt.Sprintf("%016x/%s", 0, runID)
	}
	return retentionIndexKey(at, runID)
}

// admissionTime returns now, or just after the latest admission when the
// wall clock stepped backwards, and records it. Run list keys then follow
// commit order, so a poller's saved cursor never skips a later admission.
func admissionTime(tx StoreTransaction, now time.Time) (time.Time, error) {
	now = now.UTC()
	raw, err := tx.Get(runtimeMetaBucket, runListClockMeta)
	if err == nil {
		var last time.Time
		if err := last.UnmarshalText(raw); err != nil {
			return time.Time{}, fmt.Errorf("run list clock: %w", err)
		}
		if !now.After(last) {
			now = last.Add(time.Nanosecond)
		}
	} else if !errors.Is(err, ErrStoreKeyNotFound) {
		return time.Time{}, err
	}
	text, err := now.MarshalText()
	if err != nil {
		return time.Time{}, err
	}
	return now, tx.Put(runtimeMetaBucket, runListClockMeta, text)
}

func putRunListIndex(tx StoreTransaction, record storedRuntimeRun) error {
	return tx.Put(runtimeRunListBucket, runListKey(record), nil)
}

// backfillRunListIndex indexes runs stored before the run list index
// existed, once per store. Their admission time is unknown, so they sort
// first.
func backfillRunListIndex(tx StoreTransaction) error {
	if _, err := tx.Get(runtimeMetaBucket, runListIndexedMeta); err == nil {
		return nil
	} else if !errors.Is(err, ErrStoreKeyNotFound) {
		return err
	}
	var keys []string
	if err := tx.Scan(runtimeRunsBucket, "", func(_ string, raw []byte) error {
		record, err := decodeRuntimeRun(raw)
		if err != nil {
			return err
		}
		keys = append(keys, runListKey(record))
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		if err := tx.Put(runtimeRunListBucket, key, nil); err != nil {
			return err
		}
	}
	return tx.Put(runtimeMetaBucket, runListIndexedMeta, nil)
}

// RunFilter selects runs for [Runtime.Runs]. Zero fields match every run;
// set fields must all match.
type RunFilter struct {
	// Definition matches the run's definition ID and, when set, revision.
	Definition DefinitionRef
	// Scope matches the idempotency scope the run was admitted under; child
	// runs carry their root's scope.
	Scope string
	// States matches any of the listed states. A ready run whose definition
	// is not registered in this Runtime is reported, and matched, as
	// needing attention, as [RunHandle.Snapshot] reports it.
	States []RuntimeState
	// AttentionKinds matches runs needing attention for any listed kind.
	AttentionKinds []AttentionKind
	// Labels matches runs carrying every listed label with the same value.
	Labels map[string]string
	// RootsOnly excludes durable child runs.
	RootsOnly bool
	// CreatedAfter and CreatedBefore bound the admission time; runs stored
	// before admission time was recorded sort before every time.
	CreatedAfter  time.Time
	CreatedBefore time.Time
	// After continues a listing from [RunPage].Next.
	After string
	// Limit bounds the runs returned. Zero selects 100; the maximum is 1000.
	Limit int
}

// RunSummary is the compact view of one run in a listing. Use
// [RunHandle.Snapshot] for its transcript, tool batches, and waits.
type RunSummary struct {
	RunID        string
	Definition   DefinitionRef
	State        RuntimeState
	Status       RunStatus
	Attention    *RunAttention
	Failure      *RunFailure
	Parent       *ParentRef
	Conversation *ConversationRef
	// Scope and Key are the run's idempotency identity, empty without one;
	// a child run carries its root's scope and no key.
	Scope  string
	Key    string
	Labels map[string]string
	// CreatedAt is the admission time. It never moves backwards across
	// admissions: after a wall-clock step back it is just after the previous
	// admission, so listing order follows admission order.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RunPage is one bounded page of a run listing, oldest admission first.
type RunPage struct {
	Runs []RunSummary
	// Next is the cursor for the following page. When More is false the
	// listing has reached the runs admitted so far; Next still resumes
	// after them, so polling with it returns only later admissions.
	Next string
	More bool
}

// Runs lists runs matching filter in admission order. It reads compact
// records only and never blocks. One call reads a bounded part of the
// index, so a selective filter may return fewer runs than its limit, or
// none, with More set; continue from Next.
func (r *Runtime) Runs(ctx context.Context, filter RunFilter) (RunPage, error) {
	page := RunPage{Next: filter.After}
	limit := filter.Limit
	if limit < 0 {
		return page, errors.New("run listing limit cannot be negative")
	}
	if limit == 0 {
		limit = defaultRunPageLimit
	}
	limit = min(limit, maxRunPageLimit)
	after := filter.After
	if !filter.CreatedAfter.IsZero() {
		after = max(after, runListTimeKey(filter.CreatedAfter, "\xff"))
	}
	end := ""
	if !filter.CreatedBefore.IsZero() {
		end = runListTimeKey(filter.CreatedBefore, "")
	}
	err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		var keys []string
		_, err := tx.ScanPage(runtimeRunListBucket, "", after, maxRunListScan, func(key string, _ []byte) error {
			if end != "" && key >= end {
				return errStopScan
			}
			keys = append(keys, key)
			return nil
		})
		// A full scan page may have more index entries after it.
		more := len(keys) == maxRunListScan
		if errors.Is(err, errStopScan) {
			err, more = nil, false
		}
		if err != nil {
			return err
		}
		for i, key := range keys {
			_, runID, _ := strings.Cut(key, "/")
			record, err := getRuntimeRun(tx, runID)
			if errors.Is(err, ErrRunPruned) {
				continue
			}
			if err != nil {
				return err
			}
			summary := r.runSummary(record)
			if !filter.matches(summary, record) {
				continue
			}
			page.Runs = append(page.Runs, summary)
			if len(page.Runs) == limit {
				page.Next = key
				page.More = i < len(keys)-1 || more
				return nil
			}
		}
		if len(keys) > 0 {
			page.Next = keys[len(keys)-1]
		}
		page.More = more
		return nil
	})
	return page, err
}

func (r *Runtime) runSummary(record storedRuntimeRun) RunSummary {
	summary := RunSummary{
		RunID: record.RunID, Definition: record.definition(), State: record.State,
		Status: record.Result.Status, Failure: failureFromRecord(record),
		Scope: record.AdmissionScope, Key: record.AdmissionKey,
		Labels: cloneLabels(record.Labels), CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	if record.ParentRunID != "" {
		summary.Parent = &ParentRef{RunID: record.ParentRunID, OperationID: record.ParentOperationID}
	}
	if record.ConversationID != "" {
		summary.Conversation = &ConversationRef{Scope: record.ConversationScope, ID: record.ConversationID}
	}
	switch {
	case record.State == RuntimeNeedsAttention:
		summary.Attention = &RunAttention{Kind: record.AttentionKind, Reason: record.AttentionReason}
	case record.State == RuntimeReady && !r.registered(record):
		summary.State = RuntimeNeedsAttention
		summary.Attention = &RunAttention{Kind: AttentionExecution, Reason: ErrDefinitionNotRegistered.Error()}
	}
	return summary
}

func (f RunFilter) matches(summary RunSummary, record storedRuntimeRun) bool {
	if f.Definition.ID != "" && f.Definition.ID != record.DefinitionID {
		return false
	}
	if f.Definition.Revision != "" && f.Definition.Revision != record.DefinitionRevision {
		return false
	}
	if f.Scope != "" && f.Scope != record.AdmissionScope {
		return false
	}
	if f.RootsOnly && record.ParentRunID != "" {
		return false
	}
	if len(f.States) > 0 && !slices.Contains(f.States, summary.State) {
		return false
	}
	if len(f.AttentionKinds) > 0 && (summary.Attention == nil || !slices.Contains(f.AttentionKinds, summary.Attention.Kind)) {
		return false
	}
	for key, value := range f.Labels {
		if got, ok := record.Labels[key]; !ok || got != value {
			return false
		}
	}
	return true
}
