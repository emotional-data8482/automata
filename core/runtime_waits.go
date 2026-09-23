package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (r *Runtime) authorizeApproval(ctx context.Context, request ApprovalAuthorization) (err error) {
	if r.authorizer == nil {
		return ErrApprovalUnauthorized
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("approval authorizer panicked: %v", recovered)
		}
	}()
	if err := r.authorizer.AuthorizeApproval(ctx, request); err != nil {
		return err
	}
	return ctx.Err()
}

func (r *Runtime) createInvocationWait(tx StoreTransaction, record storedRuntimeRun, invocation *storedToolInvocation, tool Tool) error {
	policy, configured, err := waitPolicyFor(tool)
	if err != nil || !configured {
		return err
	}
	if policy.Kind == WaitApproval && r.authorizer == nil {
		return errors.New("durable approval requires a runtime authorizer")
	}
	prompt := ""
	if policy.Prompt != nil {
		prompt, err = policy.Prompt(append(json.RawMessage(nil), invocation.Call.Input...))
		if err != nil {
			return fmt.Errorf("build durable wait prompt: %w", err)
		}
	}
	target := ""
	if policy.Target != nil {
		target, err = policy.Target(append(json.RawMessage(nil), invocation.Call.Input...))
		if err != nil {
			return fmt.Errorf("build durable wait target: %w", err)
		}
	}
	if len(prompt) > maxWaitTextBytes || len(target) > maxWaitTextBytes {
		return errors.New("durable wait prompt or target is too large")
	}
	if policy.Kind == WaitApproval && target == "" {
		return errors.New("durable approval target cannot be empty")
	}
	now := time.Now().UTC()
	wait := storedWait{
		Version: runtimeEncodingVersion, RunID: record.RunID, ID: waitIDFor(invocation.OperationID),
		Kind: policy.Kind, State: WaitPending, OperationID: invocation.OperationID,
		BatchID: invocation.BatchID, Ordinal: invocation.Ordinal, Tool: invocation.Call.Name,
		Arguments: append(json.RawMessage(nil), invocation.Call.Input...), Prompt: prompt, Target: target,
		DefinitionID: record.DefinitionID, DefinitionRevision: record.DefinitionRevision,
		PolicyContext: policy.PolicyContext, CreatedAt: now,
	}
	if policy.ExpiresAfter > 0 {
		wait.ExpiresAt = now.Add(policy.ExpiresAfter)
	}
	if wait.Kind == WaitApproval {
		wait.ActionDigest = approvalActionDigest(wait)
	}
	invocation.WaitID = wait.ID
	return putStoredJSON(tx, runtimeWaitsBucket, waitStorageKey(record.RunID, wait.ID), wait)
}

func getStoredWait(tx StoreTransaction, runID, waitID string) (storedWait, error) {
	raw, err := tx.Get(runtimeWaitsBucket, waitStorageKey(runID, waitID))
	if errors.Is(err, ErrStoreKeyNotFound) {
		if missing := missingRunError(tx, runID); errors.Is(missing, ErrRunPruned) {
			return storedWait{}, missing
		}
		return storedWait{}, ErrWaitNotFound
	}
	if err != nil {
		return storedWait{}, err
	}
	var wait storedWait
	if err := json.Unmarshal(raw, &wait); err != nil {
		return storedWait{}, err
	}
	if wait.Version != runtimeEncodingVersion {
		return storedWait{}, fmt.Errorf("unsupported durable wait version %d", wait.Version)
	}
	return wait, nil
}

func putStoredWait(tx StoreTransaction, wait storedWait) error {
	return putStoredJSON(tx, runtimeWaitsBucket, waitStorageKey(wait.RunID, wait.ID), wait)
}

func completeWaitInvocation(tx StoreTransaction, wait storedWait, result ToolResult) error {
	raw, err := tx.Get(runtimeInvocationsBucket, invocationStorageKey(wait.RunID, wait.BatchID, wait.Ordinal))
	if err != nil {
		return err
	}
	var invocation storedToolInvocation
	if err := json.Unmarshal(raw, &invocation); err != nil {
		return err
	}
	if invocation.OperationID != wait.OperationID || invocation.WaitID != wait.ID {
		return ErrWaitStale
	}
	if invocation.State != ToolInvocationReserved {
		return ErrWaitStale
	}
	invocation.State = ToolInvocationCompleted
	invocation.Result = normalizeResult(cloneToolResult(result))
	invocation.Effect = EffectReport{Status: EffectNotApplied}
	invocation.Result.Effect = EffectReport{}
	if invocation.GuardKey != "" {
		if err := putStoredJSON(tx, runtimeEffectGuardsBucket, invocation.GuardKey, storedEffectGuard{OperationID: invocation.OperationID, Status: EffectNotApplied}); err != nil {
			return err
		}
	}
	return putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(wait.RunID, wait.BatchID, wait.Ordinal), invocation)
}

func noPendingRunWaits(tx StoreTransaction, runID string) (bool, error) {
	pending := false
	err := tx.Scan(runtimeWaitsBucket, runID+"/", func(_ string, raw []byte) error {
		var wait storedWait
		if err := json.Unmarshal(raw, &wait); err != nil {
			return err
		}
		if wait.State == WaitPending {
			pending = true
		}
		return nil
	})
	return !pending, err
}

func markRunReadyAfterWaits(tx StoreTransaction, runID string) (bool, error) {
	ready, err := noPendingRunWaits(tx, runID)
	if err != nil || !ready {
		return false, err
	}
	record, err := getRuntimeRun(tx, runID)
	if err != nil {
		return false, err
	}
	if record.State != RuntimeWaiting && !(record.State == RuntimeNeedsAttention && record.AttentionKind == "child") {
		return false, nil
	}
	record.State = RuntimeReady
	record.AttentionReason = ""
	record.AttentionKind = ""
	record.Generation++
	if err := putRuntimeRun(tx, record); err != nil {
		return false, err
	}
	return true, nil
}

func expireWait(tx StoreTransaction, wait *storedWait, now time.Time) error {
	if wait.State != WaitPending && wait.State != WaitResolved {
		return nil
	}
	wasPending := wait.State == WaitPending
	wait.State = WaitExpired
	wait.ResolvedAt = now
	if wasPending {
		wait.Resolution = WaitResolution{Reason: "expired"}
		wait.ResolutionDigest = waitResolutionDigest(wait.Resolution)
		wait.ResolutionError = "expired"
	}
	if err := completeWaitInvocation(tx, *wait, ErrorResult("denied: wait expired")); err != nil {
		return err
	}
	return putStoredWait(tx, *wait)
}

// expireRunWaits resolves elapsed waits without retaining a worker. It returns
// true when the run became runnable.
func (r *Runtime) expireRunWaits(ctx context.Context, runID string) (bool, error) {
	ready := false
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		waits, err := loadRunWaits(tx, runID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for i := range waits {
			if waits[i].State == WaitPending && !waits[i].ExpiresAt.IsZero() && !now.Before(waits[i].ExpiresAt) {
				if err := expireWait(tx, &waits[i], now); err != nil {
					return err
				}
			}
		}
		ready, err = markRunReadyAfterWaits(tx, runID)
		return err
	})
	return ready, err
}

func historicalWaitResult(wait storedWait, digest string) (bool, error) {
	if wait.State == WaitPending {
		return false, nil
	}
	if wait.State == WaitExpired && wait.ResolutionError == "expired" {
		// The wait expired while pending, so no resolution was ever accepted:
		// every late answer is rejected as expired, whether Runtime or an
		// earlier late answer recorded the expiry. An approval accepted before
		// it expired keeps its original receipt below.
		return true, ErrWaitExpired
	}
	if wait.ResolutionDigest != digest {
		return true, ErrWaitConflict
	}
	switch wait.ResolutionError {
	case "expired":
		return true, ErrWaitExpired
	case "deadline":
		return true, context.DeadlineExceeded
	default:
		return true, nil
	}
}

// suspendedForDeadline reports a run whose logical deadline is enforced
// without a worker: waiting on waits, or blocked on required-child attention.
func suspendedForDeadline(record storedRuntimeRun) bool {
	return record.State == RuntimeWaiting || (record.State == RuntimeNeedsAttention && record.AttentionKind == "child")
}

func finalizeWaitingDeadline(tx StoreTransaction, record *storedRuntimeRun, now time.Time) error {
	if !suspendedForDeadline(*record) || record.Deadline.IsZero() || now.Before(record.Deadline) {
		return nil
	}
	if err := resolveReservedInvocations(tx, *record); err != nil {
		return err
	}
	if err := cancelRunWaits(tx, record.RunID); err != nil {
		return err
	}
	record.State = RuntimeFinalizing
	record.Result.Status = RunCancelled
	record.AttentionReason = ""
	record.AttentionKind = ""
	setRuntimeError(record, context.DeadlineExceeded)
	record.Generation++
	if err := putRuntimeRun(tx, *record); err != nil {
		return err
	}
	_, err := propagateCancellationTx(tx, record.RunID, context.DeadlineExceeded)
	return err
}

func (r *Runtime) expireWaitingDeadline(ctx context.Context, runID string) (bool, error) {
	finalized := false
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if !suspendedForDeadline(record) || record.Deadline.IsZero() || time.Now().UTC().Before(record.Deadline) {
			return nil
		}
		if err := finalizeWaitingDeadline(tx, &record, time.Now().UTC()); err != nil {
			return err
		}
		finalized = true
		return nil
	})
	if err == nil && finalized {
		err = r.completeRunHooks(runID)
	}
	return finalized, err
}

// ResolveWait durably answers a question or approval. Repeating the same
// resolution returns success; a conflicting answer is rejected even after the
// run has advanced. The wait ID itself is the idempotency scope.
func (h *RunHandle) ResolveWait(ctx context.Context, waitID string, resolution WaitResolution) error {
	if waitID == "" {
		return ErrWaitNotFound
	}
	resolution = cloneWaitResolution(resolution)
	if len(resolution.Answer) > maxWaitAnswerBytes || len(resolution.Actor) > maxWaitAuditBytes || len(resolution.Reason) > maxWaitAuditBytes {
		return errors.New("durable wait resolution is too large")
	}
	digest := waitResolutionDigest(resolution)
	var observed storedWait
	var historicalErr error
	readyFromHistory := false
	if err := h.runtime.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		observed, err = getStoredWait(tx, h.runID, waitID)
		if err != nil {
			return err
		}
		// Child waits are internal: only child completion resolves them.
		if observed.Kind == WaitChild {
			return errChildWaitHostResolution
		}
		if historical, resultErr := historicalWaitResult(observed, digest); historical {
			historicalErr = resultErr
			if errors.Is(resultErr, ErrWaitConflict) {
				return nil
			}
			record, err := getRuntimeRun(tx, h.runID)
			if err != nil {
				return err
			}
			readyFromHistory = record.State == RuntimeReady
		}
		return nil
	}); err != nil {
		return err
	}
	if observed.State != WaitPending {
		if readyFromHistory {
			h.runtime.start(h.runID)
		}
		return historicalErr
	}
	if observed.Kind == WaitQuestion {
		if len(resolution.Answer) == 0 || !json.Valid(resolution.Answer) {
			return errors.New("question resolution requires a valid JSON answer")
		}
		if resolution.ActionDigest != "" || resolution.Decision != Allow {
			return errors.New("question resolution cannot authorize an action")
		}
	} else {
		if resolution.Decision == Modify {
			return errors.New("modified actions require a new approval")
		}
		if resolution.Decision != Allow && resolution.Decision != Deny {
			return errors.New("invalid approval decision")
		}
		if resolution.ActionDigest != observed.ActionDigest {
			return ErrApprovalActionMismatch
		}
		if resolution.Decision == Allow {
			check := approvalAuthorization(observed)
			check.Actor = resolution.Actor
			if err := h.runtime.authorizeApproval(ctx, check); err != nil {
				return errors.Join(ErrApprovalUnauthorized, err)
			}
		}
	}
	ready := false
	expired := false
	deadline := false
	err := h.runtime.transaction(ctx, true, func(tx StoreTransaction) error {
		wait, err := getStoredWait(tx, h.runID, waitID)
		if err != nil {
			return err
		}
		if wait.Kind == WaitChild {
			return errChildWaitHostResolution
		}
		if historical, resultErr := historicalWaitResult(wait, digest); historical {
			if resultErr != nil {
				if errors.Is(resultErr, ErrWaitExpired) {
					expired = true
					return nil
				}
				return resultErr
			}
			record, err := getRuntimeRun(tx, h.runID)
			if err != nil {
				return err
			}
			ready = record.State == RuntimeReady
			return nil
		}
		now := time.Now().UTC()
		record, err := getRuntimeRun(tx, h.runID)
		if err != nil {
			return err
		}
		if !record.Deadline.IsZero() && !now.Before(record.Deadline) {
			if err := finalizeWaitingDeadline(tx, &record, now); err != nil {
				return err
			}
			deadline = true
			return nil
		}
		if !wait.ExpiresAt.IsZero() && !now.Before(wait.ExpiresAt) {
			expired = true
			if err := expireWait(tx, &wait, now); err != nil {
				return err
			}
			// Preserve the rejected command digest so a lost expiry response is
			// resolved identically after later run transitions.
			wait.Resolution = resolution
			wait.ResolutionDigest = digest
			wait.ResolutionError = "expired"
			if err := putStoredWait(tx, wait); err != nil {
				return err
			}
		} else {
			wait.State = WaitResolved
			wait.Resolution = resolution
			wait.ResolutionDigest = digest
			wait.ResolvedAt = now
			if wait.Kind == WaitQuestion {
				if err := completeWaitInvocation(tx, wait, TextResult(string(resolution.Answer))); err != nil {
					return err
				}
			} else if resolution.Decision == Deny {
				reason := resolution.Reason
				if reason == "" {
					reason = "denied"
				}
				if err := completeWaitInvocation(tx, wait, ErrorResult("denied: "+reason)); err != nil {
					return err
				}
			}
			if err := putStoredWait(tx, wait); err != nil {
				return err
			}
		}
		ready, err = markRunReadyAfterWaits(tx, h.runID)
		return err
	})
	if err == nil && deadline {
		if hookErr := h.runtime.completeRunHooks(h.runID); hookErr != nil {
			return hookErr
		}
		return context.DeadlineExceeded
	}
	if err == nil && ready {
		h.runtime.start(h.runID)
	}
	if err == nil && expired {
		return ErrWaitExpired
	}
	return err
}

func (r *Runtime) revalidateApproval(ctx context.Context, runID string, invocation storedToolInvocation) error {
	if invocation.WaitID == "" {
		return nil
	}
	var wait storedWait
	if err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		wait, err = getStoredWait(tx, runID, invocation.WaitID)
		return err
	}); err != nil {
		return err
	}
	if wait.Kind != WaitApproval {
		return nil
	}
	if wait.State != WaitResolved || wait.Resolution.Decision != Allow {
		return ErrWaitStale
	}
	if wait.ActionDigest != approvalActionDigest(wait) || wait.Resolution.ActionDigest != wait.ActionDigest {
		return ErrApprovalActionMismatch
	}
	if err := r.authorizeApproval(ctx, approvalAuthorization(wait)); err != nil {
		return errors.Join(ErrApprovalUnauthorized, err)
	}
	return nil
}

func cancelRunWaits(tx StoreTransaction, runID string) error {
	waits, err := loadRunWaits(tx, runID)
	if err != nil {
		return err
	}
	for _, wait := range waits {
		if wait.State != WaitPending && wait.State != WaitResolved {
			continue
		}
		wasPending := wait.State == WaitPending
		wait.State = WaitCancelled
		wait.ResolvedAt = time.Now().UTC()
		if wasPending {
			wait.Resolution = WaitResolution{Reason: "run cancelled"}
			wait.ResolutionDigest = waitResolutionDigest(wait.Resolution)
			wait.ResolutionError = "cancelled"
		}
		if err := putStoredWait(tx, wait); err != nil {
			return err
		}
	}
	return nil
}
