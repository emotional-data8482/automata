package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Retention indexes hold each terminal run once per retention class, ordered
// by terminal commit time, until that class has been applied to it. A class
// pass therefore reads only runs it has not finished, and a run it cannot
// prune yet (an unsettled subtree) stays for a later pass.
const (
	runtimeRetainEventsBucket  = "runtime_retain_events"
	runtimeRetainHistoryBucket = "runtime_retain_history"
	runtimeRetainRunsBucket    = "runtime_retain_runs"
	// runtimePrunedBucket keeps a tombstone per pruned run so its identity is
	// reported as pruned, never as unknown or reusable.
	runtimePrunedBucket = "runtime_pruned"
)

const defaultPruneLimit = 128

// ErrRunPruned reports a run removed by [Runtime.Prune]. Every handle
// operation on it, and every exact retry of its admission or of a command
// against it, returns this error: expired receipts never become new work.
var ErrRunPruned = errors.New("runtime run was pruned by retention")

// RetentionPolicy selects what [Runtime.Prune] removes. Each class has its own
// age, measured from the run's terminal commit; zero keeps that class
// forever. Only settled runs are eligible: terminal, with every descendant
// terminal and no dispatched or uncertain tool invocation anywhere in the
// subtree. Effect guards are never pruned, so a semantic duplicate stays
// rejected after its run is gone.
type RetentionPolicy struct {
	// Events removes a run's committed event log. Cursors inside the removed
	// range return ErrEventGap; Snapshot still reports the run.
	Events time.Duration
	// History removes a run's transcript chunks and its tool invocations'
	// result payloads. The record keeps its state, accepted output, final
	// message, usage, and every invocation's state and effect report;
	// RunSnapshot.HistoryPruned reports the removal. Conversation turns are
	// never history-pruned: later turns reference their transcripts.
	History time.Duration
	// Runs deletes a whole root run and its durable descendants, including
	// their receipts, leaving tombstones: operations and exact retries on
	// them return ErrRunPruned. Child runs are deleted only with their root,
	// and conversation turns are kept.
	Runs time.Duration
	// Limit bounds how many runs each class prunes in one call. Zero selects
	// 128. PruneReport.More reports that eligible runs remain.
	Limit int
}

// PruneReport counts the runs one [Runtime.Prune] call pruned per class.
type PruneReport struct {
	Events  int
	History int
	Runs    int
	// Skipped counts runs left in place because a payload they hold is
	// unavailable (see ErrPayloadUnavailable); they stay indexed.
	Skipped int
	// More is true when a class stopped at the limit with runs due.
	More bool
}

// pruneOutcome is what one retention class did with one indexed run.
type pruneOutcome int

const (
	// pruneLater keeps the run indexed: it is not settled yet.
	pruneLater pruneOutcome = iota
	// pruneSkipped drops the run from the class without pruning it, because
	// the class never applies to it (for example a conversation turn).
	pruneSkipped
	// pruned applied the class.
	pruned
)

func retentionIndexKey(at time.Time, runID string) string {
	return fmt.Sprintf("%016x/%s", at.UnixNano(), runID)
}

// Prune applies retention to terminal runs. Each class is applied
// independently, one run per transaction, oldest terminal first. Pruning never
// touches a run that is not settled, so it cannot make retained work look
// replay-safe or lose evidence a pending decision needs.
func (r *Runtime) Prune(ctx context.Context, policy RetentionPolicy) (PruneReport, error) {
	var report PruneReport
	if policy.Events < 0 || policy.History < 0 || policy.Runs < 0 || policy.Limit < 0 {
		return report, errors.New("retention ages and limit cannot be negative")
	}
	limit := policy.Limit
	if limit == 0 {
		limit = defaultPruneLimit
	}
	now := time.Now()
	classes := []struct {
		bucket string
		age    time.Duration
		count  *int
		apply  func(StoreTransaction, storedRuntimeRun) (pruneOutcome, error)
	}{
		{runtimeRetainEventsBucket, policy.Events, &report.Events, pruneRunEvents},
		{runtimeRetainHistoryBucket, policy.History, &report.History, pruneRunHistory},
		{runtimeRetainRunsBucket, policy.Runs, &report.Runs, pruneRunTree},
	}
	for _, class := range classes {
		if class.age == 0 {
			continue
		}
		cutoff := retentionIndexKey(now.Add(-class.age), "\xff")
		more, err := r.pruneClass(ctx, class.bucket, cutoff, limit, class.count, &report.Skipped, class.apply)
		if err != nil {
			return report, err
		}
		report.More = report.More || more
	}
	return report, nil
}

// pruneClass walks one retention index up to cutoff. Runs apply defers stay
// indexed for a later pass; count reports the runs it pruned.
func (r *Runtime) pruneClass(ctx context.Context, bucket, cutoff string, limit int, count, skipped *int, apply func(StoreTransaction, storedRuntimeRun) (pruneOutcome, error)) (bool, error) {
	after := ""
	for {
		var keys []string
		if err := r.transaction(ctx, false, func(tx StoreTransaction) error {
			_, err := tx.ScanPage(bucket, "", after, limit, func(key string, _ []byte) error {
				if key > cutoff {
					return errStopScan
				}
				keys = append(keys, key)
				return nil
			})
			if errors.Is(err, errStopScan) {
				err = nil
			}
			return err
		}); err != nil {
			return false, err
		}
		if len(keys) == 0 {
			return false, nil
		}
		for _, key := range keys {
			if *count >= limit {
				return true, nil
			}
			_, runID, _ := strings.Cut(key, "/")
			outcome := pruneLater
			err := r.transaction(ctx, true, func(tx StoreTransaction) error {
				outcome = pruneLater
				record, err := getRuntimeRun(tx, runID)
				if errors.Is(err, ErrRunPruned) {
					// Removed with its root by an earlier Runs pass.
					outcome = pruneSkipped
					return tx.Delete(bucket, key)
				}
				if err != nil {
					return err
				}
				if outcome, err = apply(tx, record); err != nil || outcome == pruneLater {
					return err
				}
				return tx.Delete(bucket, key)
			})
			if errors.Is(err, ErrPayloadUnavailable) {
				// One damaged run must not stall retention for every run
				// after it; it stays indexed and is reported.
				*skipped++
				continue
			}
			if err != nil {
				return false, err
			}
			if outcome == pruned {
				*count++
			}
		}
		after = keys[len(keys)-1]
	}
}

// settledForRetention reports whether a terminal run's subtree can no longer
// change or need a decision.
func settledForRetention(tx StoreTransaction, record storedRuntimeRun) (bool, error) {
	if record.State != RuntimeTerminal {
		return false, nil
	}
	unresolved, err := childHasUnresolvedEvidence(tx, record)
	return !unresolved, err
}

func pruneRunEvents(tx StoreTransaction, record storedRuntimeRun) (pruneOutcome, error) {
	if settled, err := settledForRetention(tx, record); err != nil || !settled {
		return pruneLater, err
	}
	head, err := getEventHead(tx, record.RunID)
	if err != nil {
		return pruneLater, err
	}
	if err := deletePrefix(tx, runtimeEventsBucket, record.RunID+"/"); err != nil {
		return pruneLater, err
	}
	head.Floor = head.Head
	return pruned, putStoredJSON(tx, runtimeEventHeadsBucket, record.RunID, head)
}

func pruneRunHistory(tx StoreTransaction, record storedRuntimeRun) (pruneOutcome, error) {
	if record.ConversationID != "" {
		// Later turns reference this transcript; keep it and stop tracking.
		return pruneSkipped, nil
	}
	if settled, err := settledForRetention(tx, record); err != nil || !settled {
		return pruneLater, err
	}
	if err := deletePrefix(tx, runtimeFactsBucket, record.RunID+"/"); err != nil {
		return pruneLater, err
	}
	var invocations []storedToolInvocation
	if err := tx.Scan(runtimeInvocationsBucket, record.RunID+"/", func(_ string, raw []byte) error {
		var invocation storedToolInvocation
		if err := json.Unmarshal(raw, &invocation); err != nil {
			return err
		}
		invocations = append(invocations, invocation)
		return nil
	}); err != nil {
		return pruneLater, err
	}
	for _, invocation := range invocations {
		if invocation.ResultPruned {
			continue
		}
		invocation.Result = ToolResult{}
		invocation.ResultPruned = true
		if err := putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(invocation.RunID, invocation.BatchID, invocation.Ordinal), invocation); err != nil {
			return pruneLater, err
		}
	}
	record.HistoryPruned = true
	return pruned, putRuntimeRun(tx, record)
}

// pruneRunTree deletes a settled root run and its descendants in one
// transaction, leaving a tombstone for each.
func pruneRunTree(tx StoreTransaction, record storedRuntimeRun) (pruneOutcome, error) {
	if record.ParentRunID != "" || record.ConversationID != "" {
		// Children go with their root; conversations keep their turns.
		return pruneSkipped, nil
	}
	if settled, err := settledForRetention(tx, record); err != nil || !settled {
		return pruneLater, err
	}
	runs := []string{record.RunID}
	for i := 0; i < len(runs); i++ {
		children, err := linkedChildRunIDs(tx, runs[i])
		if err != nil {
			return pruneLater, err
		}
		runs = append(runs, children...)
	}
	now := time.Now().UTC()
	for _, runID := range runs {
		if err := deleteRun(tx, runID, now); err != nil {
			return pruneLater, err
		}
	}
	return pruned, nil
}

type storedPrunedRun struct {
	RunID    string    `json:"run_id"`
	PrunedAt time.Time `json:"pruned_at"`
}

// deleteRun removes one run's records, facts, events, and receipts and leaves
// its tombstone. Effect guards and admission records stay: they keep
// semantic duplicates rejected and resolve an exact admission retry to the
// pruned identity instead of new work.
func deleteRun(tx StoreTransaction, runID string, now time.Time) error {
	for _, prefix := range []struct{ bucket, prefix string }{
		{runtimeFactsBucket, runID + "/"},
		{runtimeEventsBucket, runID + "/"},
		{runtimeBatchesBucket, runID + "/"},
		{runtimeInvocationsBucket, runID + "/"},
		{runtimeWaitsBucket, runID + "/"},
		{runtimeChildLinksBucket, runID + "\x00"},
		{runtimeReceiptsBucket, "reconcile\x00" + runID + "\x00"},
	} {
		if err := deletePrefix(tx, prefix.bucket, prefix.prefix); err != nil {
			return err
		}
	}
	for _, key := range []struct{ bucket, key string }{
		{runtimeEventHeadsBucket, runID},
		{runtimeReceiptsBucket, cancelReceiptKey(runID)},
		{runtimeReceiptsBucket, hooksAckReceiptKey(runID)},
		{runtimeActiveBucket, runID},
		{runtimeRunsBucket, runID},
	} {
		if err := tx.Delete(key.bucket, key.key); err != nil {
			return err
		}
	}
	// Indexed descendants are dropped from the other classes as their pass
	// reaches them (getRuntimeRun reports them pruned).
	return putStoredJSON(tx, runtimePrunedBucket, runID, storedPrunedRun{RunID: runID, PrunedAt: now})
}

// deletePrefix deletes every key under prefix. Keys are collected before
// deleting so no adapter has to support writes during an open scan.
func deletePrefix(tx StoreTransaction, bucket, prefix string) error {
	var keys []string
	if err := tx.Scan(bucket, prefix, func(key string, _ []byte) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		if err := tx.Delete(bucket, key); err != nil {
			return err
		}
	}
	return nil
}
