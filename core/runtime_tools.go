package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrToolEffectUncertain    = errors.New("tool effect outcome is uncertain")
	ErrReconciliationConflict = errors.New("tool reconciliation conflicts with a committed resolution")
	ErrOperationNotFound      = errors.New("tool operation not found")
	errDurableBatchIncomplete = errors.New("durable tool batch requires attention")
)

type storedToolBatch struct {
	Version   int    `json:"version"`
	RunID     string `json:"run_id"`
	BatchID   string `json:"batch_id"`
	Ordinal   int    `json:"ordinal"`
	Count     int    `json:"count"`
	Committed bool   `json:"committed,omitempty"`
}

type storedToolInvocation struct {
	Version         int                 `json:"version"`
	RunID           string              `json:"run_id"`
	BatchID         string              `json:"batch_id"`
	OperationID     string              `json:"operation_id"`
	Ordinal         int                 `json:"ordinal"`
	Call            ToolUseBlock        `json:"call"`
	State           ToolInvocationState `json:"state"`
	Budget          toolBudgetUsage     `json:"budget"`
	Result          ToolResult          `json:"result"`
	Effect          EffectReport        `json:"effect"`
	Error           string              `json:"error,omitempty"`
	ErrorKind       string              `json:"error_kind,omitempty"`
	ErrorStopReason StopReason          `json:"error_stop_reason,omitempty"`
	ErrorRawReason  string              `json:"error_raw_reason,omitempty"`
	Fatal           bool                `json:"fatal,omitempty"`
	EffectKind      ToolEffectKind      `json:"effect_kind,omitempty"`
	GuardKey        string              `json:"guard_key,omitempty"`
	GuardDisplay    string              `json:"guard_display,omitempty"`
	WaitID          string              `json:"wait_id,omitempty"`
	// ChildRunID links a durable child invocation to its admitted child run.
	// It is empty for ordinary tool invocations.
	ChildRunID string `json:"child_run_id,omitempty"`
	// ResultDigest covers the encoded Result exactly as stored. It is stamped
	// on every write and verified against the stored bytes on every read, so
	// a missing, altered, or truncated result is reported as
	// ErrPayloadUnavailable instead of becoming canonical history.
	ResultDigest string `json:"result_digest,omitempty"`
	// ResultPruned records that retention removed Result; the effect report
	// and state remain.
	ResultPruned bool `json:"result_pruned,omitempty"`
}

// storedToolInvocationFields has the fields of storedToolInvocation without
// its JSON methods.
type storedToolInvocationFields storedToolInvocation

func toolResultDigest(result ToolResult) (string, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (i storedToolInvocation) MarshalJSON() ([]byte, error) {
	digest, err := toolResultDigest(i.Result)
	if err != nil {
		return nil, err
	}
	fields := storedToolInvocationFields(i)
	fields.ResultDigest = digest
	return json.Marshal(fields)
}

func (i *storedToolInvocation) UnmarshalJSON(data []byte) error {
	// The outer Result shadows the embedded one, capturing the stored bytes
	// for verification before they are decoded.
	var stored struct {
		storedToolInvocationFields
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	sum := sha256.Sum256(stored.Result)
	if stored.ResultDigest == "" || hex.EncodeToString(sum[:]) != stored.ResultDigest {
		return fmt.Errorf("%w: tool invocation %s result fails its integrity check", ErrPayloadUnavailable, stored.OperationID)
	}
	fields := stored.storedToolInvocationFields
	if err := json.Unmarshal(stored.Result, &fields.Result); err != nil {
		return fmt.Errorf("%w: tool invocation %s result: %v", ErrPayloadUnavailable, stored.OperationID, err)
	}
	*i = storedToolInvocation(fields)
	return nil
}

type storedEffectGuard struct {
	OperationID string       `json:"operation_id"`
	Status      EffectStatus `json:"status"`
}

type reconcileReceipt struct {
	Version     int              `json:"version"`
	RunID       string           `json:"run_id"`
	OperationID string           `json:"operation_id"`
	Digest      string           `json:"digest"`
	Resolution  EffectResolution `json:"resolution"`
}

func batchStorageKey(runID, batchID string) string { return runID + "/" + batchID }
func invocationStorageKey(runID, batchID string, ordinal int) string {
	return fmt.Sprintf("%s/%s/%016x", runID, batchID, ordinal)
}
func reconcileReceiptKey(runID, operationID string) string {
	return "reconcile\x00" + runID + "\x00" + operationID
}

func putStoredJSON(tx StoreTransaction, bucket, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return tx.Put(bucket, key, data)
}

func getStoredToolBatch(tx StoreTransaction, runID, batchID string) (storedToolBatch, error) {
	raw, err := tx.Get(runtimeBatchesBucket, batchStorageKey(runID, batchID))
	if err != nil {
		return storedToolBatch{}, err
	}
	var batch storedToolBatch
	if err := json.Unmarshal(raw, &batch); err != nil {
		return storedToolBatch{}, err
	}
	if batch.Version != runtimeEncodingVersion {
		return storedToolBatch{}, fmt.Errorf("unsupported tool batch version %d", batch.Version)
	}
	return batch, nil
}

func loadStoredInvocations(tx StoreTransaction, runID, batchID string) ([]storedToolInvocation, error) {
	var invocations []storedToolInvocation
	err := tx.Scan(runtimeInvocationsBucket, batchStorageKey(runID, batchID)+"/", func(_ string, raw []byte) error {
		var invocation storedToolInvocation
		if err := json.Unmarshal(raw, &invocation); err != nil {
			return err
		}
		if invocation.Version != runtimeEncodingVersion {
			return fmt.Errorf("unsupported tool invocation version %d", invocation.Version)
		}
		invocations = append(invocations, invocation)
		return nil
	})
	return invocations, err
}

func loadToolBatchSnapshots(tx StoreTransaction, runID string) ([]ToolBatchSnapshot, error) {
	var batches []storedToolBatch
	if err := tx.Scan(runtimeBatchesBucket, runID+"/", func(_ string, raw []byte) error {
		var batch storedToolBatch
		if err := json.Unmarshal(raw, &batch); err != nil {
			return err
		}
		if batch.Version != runtimeEncodingVersion {
			return fmt.Errorf("unsupported tool batch version %d", batch.Version)
		}
		batches = append(batches, batch)
		return nil
	}); err != nil {
		return nil, err
	}
	snapshots := make([]ToolBatchSnapshot, 0, len(batches))
	for _, batch := range batches {
		invocations, err := loadStoredInvocations(tx, runID, batch.BatchID)
		if err != nil {
			return nil, err
		}
		snapshot := ToolBatchSnapshot{BatchID: batch.BatchID, Ordinal: batch.Ordinal, Committed: batch.Committed}
		for _, invocation := range invocations {
			snapshot.Invocations = append(snapshot.Invocations, ToolInvocationSnapshot{
				OperationID:  invocation.OperationID,
				Ordinal:      invocation.Ordinal,
				Call:         cloneBlock(invocation.Call).(ToolUseBlock),
				State:        invocation.State,
				Result:       cloneToolResult(invocation.Result),
				ResultPruned: invocation.ResultPruned,
				Effect:       invocation.Effect,
				Error:        invocation.Error,
				GuardKey:     invocation.GuardDisplay,
				WaitID:       invocation.WaitID,
				ChildRunID:   invocation.ChildRunID,
			})
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func cloneToolBatchSnapshots(in []ToolBatchSnapshot) []ToolBatchSnapshot {
	out := make([]ToolBatchSnapshot, len(in))
	for i, batch := range in {
		out[i] = batch
		out[i].Invocations = make([]ToolInvocationSnapshot, len(batch.Invocations))
		for j, invocation := range batch.Invocations {
			out[i].Invocations[j] = invocation
			out[i].Invocations[j].Call = cloneBlock(invocation.Call).(ToolUseBlock)
			out[i].Invocations[j].Result = cloneToolResult(invocation.Result)
		}
	}
	return out
}

func resultFromMessage(message Message) ToolResult {
	if len(message.Blocks) == 1 {
		if block, ok := message.Blocks[0].(ToolResultBlock); ok {
			return ToolResult{Blocks: cloneBlocks(block.Content), IsError: block.IsError}
		}
	}
	return ErrorResult("invalid stored tool result")
}

func invocationResultMessage(invocation storedToolInvocation) Message {
	return ToolResultBlockMessage(invocation.Call.ID, invocation.Result.Blocks, invocation.Result.IsError)
}

func effectGuardStorageKey(scope, tool, semantic string) string {
	digest := sha256.Sum256([]byte(scope + "\x00" + tool + "\x00" + semantic))
	return hex.EncodeToString(digest[:])
}

// maxReservationAncestorDepth bounds the ancestor chain walk a durable batch
// reservation performs. Admitted runs form a tree, so a deeper chain can only
// come from a corrupt store; fail closed instead of looping.
const maxReservationAncestorDepth = 64

// reserveDurableInvocation atomically reserves one known invocation across
// every applicable ancestor subtree total plus the preparing run's own total
// and per-tool counters, inside the caller's batch-creation transaction. A
// child invocation costs one reservation on each applicable ancestor total;
// a descendant's known invocations reserve the same chain against its own
// record. All counters live on the persisted records, so concurrent sibling
// reservations serialize on the store, a replayed batch never re-reserves
// (the pending-batch path returns before reserving), and a rolled-back
// transaction reverts every charge. Zero caps stay unlimited and PerTool caps
// stay run-local. On denial no counter is charged anywhere.
func reserveDurableInvocation(tx StoreTransaction, record *storedRuntimeRun, tool string) (toolBudgetUsage, error) {
	parentID := record.ParentRunID
	var capped []storedRuntimeRun
	for depth := 0; parentID != ""; depth++ {
		if depth > maxReservationAncestorDepth {
			return toolBudgetUsage{}, fmt.Errorf("ancestor chain for run %s exceeds %d", record.RunID, maxReservationAncestorDepth)
		}
		parent, err := getRuntimeRun(tx, parentID)
		if err != nil {
			return toolBudgetUsage{}, fmt.Errorf("load ancestor %s for tool reservation: %w", parentID, err)
		}
		if parent.ToolBudget.TotalCap > 0 {
			capped = append(capped, parent)
		}
		parentID = parent.ParentRunID
	}
	return chargeDurableBudget(tx, record, capped, tool)
}

// chargeDurableBudget checks then charges the persisted cap counters of the
// preparing run and its capped ancestors under the caller's store transaction:
// the run's own total, its local per-tool cap, then each ancestor from nearest
// to root. Charges land on the persisted records themselves, never on a local
// snapshot, so an ancestor's own next batch cannot overwrite a descendant's
// charge and concurrent siblings serialize on the store.
func chargeDurableBudget(tx StoreTransaction, record *storedRuntimeRun, capped []storedRuntimeRun, tool string) (toolBudgetUsage, error) {
	var counters []*toolBudgetCounter
	var localTotal, localPerTool *toolBudgetCounter
	if cap := record.ToolBudget.TotalCap; cap > 0 {
		localTotal = &toolBudgetCounter{max: cap, used: record.ToolBudget.Total}
		counters = append(counters, localTotal)
	}
	if cap := record.ToolBudget.PerToolCap[tool]; cap > 0 {
		localPerTool = &toolBudgetCounter{max: cap, used: record.ToolBudget.PerTool[tool], tool: tool}
		counters = append(counters, localPerTool)
	}
	for i := range capped {
		counters = append(counters, &toolBudgetCounter{max: capped[i].ToolBudget.TotalCap, used: capped[i].ToolBudget.Total})
	}
	if usage, err := chargeBudgetCounters(counters); err != nil {
		return usage, err
	}
	var usage toolBudgetUsage
	if localTotal != nil {
		record.ToolBudget.Total = localTotal.used
		usage.used, usage.max = localTotal.used, localTotal.max
	}
	if localPerTool != nil {
		if record.ToolBudget.PerTool == nil {
			record.ToolBudget.PerTool = make(map[string]int)
		}
		record.ToolBudget.PerTool[tool] = localPerTool.used
		usage.toolUsed, usage.toolMax = localPerTool.used, localPerTool.max
	}
	for i := range capped {
		capped[i].ToolBudget.Total++
		if err := putRuntimeRun(tx, capped[i]); err != nil {
			return toolBudgetUsage{}, err
		}
	}
	if localTotal == nil && len(capped) > 0 {
		usage.used, usage.max = capped[0].ToolBudget.Total, capped[0].ToolBudget.TotalCap
	}
	return usage, nil
}

// reserveNestedCall charges one known tool call that a process-local agent,
// running inside runID's tool execution, reserved for itself. It persists the
// charge on runID's subtree cap and every capped ancestor immediately, so
// nested work cannot exceed a durable cap or lose its charges on restart.
func (r *Runtime) reserveNestedCall(runID string) (toolBudgetUsage, error) {
	var usage toolBudgetUsage
	err := r.transaction(context.Background(), true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.State != RuntimeRunning {
			// A canceled or no longer owned run admits no new nested work.
			return context.Canceled
		}
		// Nested tool names belong to the nested agent, so no local per-tool
		// cap of this run applies.
		if usage, err = reserveDurableInvocation(tx, &record, ""); err != nil {
			return err
		}
		return putRuntimeRun(tx, record)
	})
	return usage, err
}

// prepareDurableBatch creates the run's pending tool batch or reloads it. It
// reports whether it created the batch and whether it committed the run as
// waiting; a suspended worker must return without dispatching, because only
// the wait resolution that makes the run ready again may resume it.
func (r *Runtime) prepareDurableBatch(ctx context.Context, runID string, l *loop, calls []ToolUseBlock) (batch storedToolBatch, invocations []storedToolInvocation, created, suspended bool, err error) {
	err = r.transaction(context.WithoutCancel(ctx), true, func(tx StoreTransaction) error {
		created, suspended = false, false
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		// Cancellation and terminal ownership are authoritative at the same
		// boundary that creates reservations and waits. The transaction is
		// deliberately detached from the worker context so it can observe the
		// committed command, but it must never reverse that command.
		if record.State == RuntimeCancelRequested || record.State == RuntimeFinalizing || record.State == RuntimeTerminal {
			return context.Canceled
		}
		if record.State != RuntimeRunning {
			return fmt.Errorf("run cannot prepare a tool batch from %s", record.State)
		}
		if record.PendingBatchID != "" {
			batch, err = getStoredToolBatch(tx, runID, record.PendingBatchID)
			if err != nil {
				return err
			}
			invocations, err = loadStoredInvocations(tx, runID, record.PendingBatchID)
			if err != nil {
				return err
			}
			// A replayed batch that still holds a pending wait suspends the
			// run exactly as creation does, so the worker never leaves it
			// running without an owner.
			for _, invocation := range invocations {
				if invocation.WaitID == "" || invocation.State != ToolInvocationReserved {
					continue
				}
				wait, err := getStoredWait(tx, runID, invocation.WaitID)
				if err != nil {
					return err
				}
				if wait.State == WaitPending {
					record.State = RuntimeWaiting
					record.Generation++
					suspended = true
					return putRuntimeRun(tx, record)
				}
			}
			return nil
		}

		created = true
		batch = storedToolBatch{
			Version: runtimeEncodingVersion, RunID: runID,
			BatchID: fmt.Sprintf("%016x", record.NextBatchOrdinal),
			Ordinal: record.NextBatchOrdinal, Count: len(calls),
		}
		invocations = make([]storedToolInvocation, len(calls))
		for i, call := range calls {
			invocation := storedToolInvocation{
				Version: runtimeEncodingVersion, RunID: runID, BatchID: batch.BatchID,
				OperationID: fmt.Sprintf("%s:%s:%016x", runID, batch.BatchID, i),
				Ordinal:     i, Call: cloneBlock(call).(ToolUseBlock), State: ToolInvocationReserved,
			}
			if registered, known := l.toolsByName[call.Name]; known {
				// Durable reservation: known invocations reserve across the
				// preparing run's own total and per-tool counters plus every
				// applicable ancestor subtree total, atomically with this batch
				// creation. Denial produces a recoverable, not-applied
				// model-visible result without charging any counter.
				invocation.Budget, err = reserveDurableInvocation(tx, &record, call.Name)
				if err != nil && !errors.Is(err, ErrToolBudgetExhausted) {
					// Only exhaustion is a model-visible denial. A storage or
					// ancestor-chain failure aborts batch creation; it must never
					// be committed as a recoverable, not-applied tool result.
					return err
				}
				if err != nil {
					message := l.policyDeniedToolResult(ctx, call, invocation.Budget, err)
					invocation.State = ToolInvocationCompleted
					invocation.Result = resultFromMessage(message)
					invocation.Effect = EffectReport{Status: EffectNotApplied}
				} else if child, childErr := durableChildDeclaration(registered.executor); childErr != nil {
					return childErr
				} else if child != nil {
					// Durable child invocation. The reservation above charged this
					// child invocation once against each applicable ancestor total;
					// the child run, operation link, and internal child wait commit
					// atomically in this same transaction, so an exhausted or
					// failed admission cannot leave a charged reservation without a
					// persisted outcome. The model's raw arguments are validated
					// against the frozen child schema and forwarded verbatim
					// as the deterministic child task.
					if err := validateToolArguments(child.definition.InputSchema, call.Input); err != nil {
						invocation.State = ToolInvocationCompleted
						invocation.Result = ErrorResult("invalid child arguments: " + err.Error())
						invocation.Effect = EffectReport{Status: EffectNotApplied}
					} else {
						receipt, admitErr := r.admitChildRun(tx, record, invocation, child.policy, string(call.Input))
						if admitErr != nil {
							return admitErr
						}
						invocation.WaitID = receipt.WaitID
						invocation.ChildRunID = receipt.ChildRunID
					}
					invocations[i] = invocation
					continue
				} else {
					effectPolicy, policyErr := effectPolicyFor(registered.executor)
					if policyErr != nil {
						return policyErr
					}
					invocation.EffectKind = effectPolicy.Kind
					if effectPolicy.SemanticKey != nil && validateToolArguments(registered.definition.InputSchema, call.Input) == nil {
						semantic, semanticErr := effectPolicy.SemanticKey(append(json.RawMessage(nil), call.Input...))
						if semanticErr != nil || semantic == "" {
							if semanticErr == nil {
								semanticErr = errors.New("empty semantic key")
							}
							invocation.State = ToolInvocationCompleted
							invocation.Result = ErrorResult("effect guard: " + semanticErr.Error())
							invocation.Effect = EffectReport{Status: EffectNotApplied}
							invocation.Error = semanticErr.Error()
							invocation.Fatal = true
						} else {
							invocation.GuardKey = effectGuardStorageKey(effectPolicy.Scope, call.Name, semantic)
							invocation.GuardDisplay = effectPolicy.Scope + ":" + semantic
						}
					}
				}
			}
			// Reserve semantic guards in model order in the same transaction as
			// batch creation. Parallel dispatch must not decide which duplicate wins.
			if invocation.State == ToolInvocationReserved && invocation.GuardKey != "" {
				rawGuard, guardErr := tx.Get(runtimeEffectGuardsBucket, invocation.GuardKey)
				if guardErr == nil {
					var guard storedEffectGuard
					if err := json.Unmarshal(rawGuard, &guard); err != nil {
						return err
					}
					if guard.OperationID != invocation.OperationID && guard.Status != EffectNotApplied && guard.Status != EffectNone {
						invocation.State = ToolInvocationCompleted
						invocation.Result = ErrorResult("denied: semantic mutation already applied or unresolved")
						invocation.Effect = EffectReport{Status: EffectNotApplied}
					}
				} else if !errors.Is(guardErr, ErrStoreKeyNotFound) {
					return guardErr
				}
				if invocation.State == ToolInvocationReserved {
					if err := putStoredJSON(tx, runtimeEffectGuardsBucket, invocation.GuardKey, storedEffectGuard{OperationID: invocation.OperationID, Status: EffectUnknown}); err != nil {
						return err
					}
				}
			}
			if invocation.State == ToolInvocationReserved {
				if registered, known := l.toolsByName[call.Name]; known && validateToolArguments(registered.definition.InputSchema, call.Input) == nil {
					if err := r.createInvocationWait(tx, record, &invocation, registered.executor); err != nil {
						return err
					}
				}
			}
			invocations[i] = invocation
		}
		if err := putStoredJSON(tx, runtimeBatchesBucket, batchStorageKey(runID, batch.BatchID), batch); err != nil {
			return err
		}
		for _, invocation := range invocations {
			if err := putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(runID, batch.BatchID, invocation.Ordinal), invocation); err != nil {
				return err
			}
		}
		record.PendingBatchID = batch.BatchID
		record.NextBatchOrdinal++
		// record.ToolBudget is maintained by chargeDurableBudget directly on the
		// persisted record, never from an in-memory snapshot: a snapshot would
		// overwrite the descendant charges this run's record accumulates.
		for _, invocation := range invocations {
			if invocation.WaitID != "" && invocation.State == ToolInvocationReserved {
				record.State = RuntimeWaiting
				suspended = true
				break
			}
		}
		record.Generation++
		return putRuntimeRun(tx, record)
	})
	return batch, invocations, created, suspended, err
}

func (r *Runtime) beginToolDispatch(ctx context.Context, invocation storedToolInvocation) (storedToolInvocation, bool, error) {
	var dispatch bool
	err := r.transaction(context.WithoutCancel(ctx), true, func(tx StoreTransaction) error {
		raw, err := tx.Get(runtimeInvocationsBucket, invocationStorageKey(invocation.RunID, invocation.BatchID, invocation.Ordinal))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &invocation); err != nil {
			return err
		}
		if invocation.State != ToolInvocationReserved {
			return nil
		}
		if invocation.WaitID != "" {
			wait, err := getStoredWait(tx, invocation.RunID, invocation.WaitID)
			if err != nil {
				return err
			}
			if wait.Kind == WaitApproval {
				if wait.State != WaitResolved || wait.Resolution.Decision != Allow || wait.ActionDigest != approvalActionDigest(wait) || wait.Resolution.ActionDigest != wait.ActionDigest {
					return ErrWaitStale
				}
				if !wait.ExpiresAt.IsZero() && !time.Now().UTC().Before(wait.ExpiresAt) {
					if err := expireWait(tx, &wait, time.Now().UTC()); err != nil {
						return err
					}
					raw, err := tx.Get(runtimeInvocationsBucket, invocationStorageKey(invocation.RunID, invocation.BatchID, invocation.Ordinal))
					if err != nil {
						return err
					}
					return json.Unmarshal(raw, &invocation)
				}
				wait.State = WaitConsumed
				if err := putStoredWait(tx, wait); err != nil {
					return err
				}
			}
		}
		if invocation.GuardKey != "" {
			rawGuard, err := tx.Get(runtimeEffectGuardsBucket, invocation.GuardKey)
			if err == nil {
				var guard storedEffectGuard
				if err := json.Unmarshal(rawGuard, &guard); err != nil {
					return err
				}
				if guard.OperationID != invocation.OperationID && guard.Status != EffectNotApplied && guard.Status != EffectNone {
					invocation.State = ToolInvocationCompleted
					invocation.Result = ErrorResult("denied: semantic mutation already applied or unresolved")
					invocation.Effect = EffectReport{Status: EffectNotApplied}
					return putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(invocation.RunID, invocation.BatchID, invocation.Ordinal), invocation)
				}
			} else if !errors.Is(err, ErrStoreKeyNotFound) {
				return err
			}
			if err := putStoredJSON(tx, runtimeEffectGuardsBucket, invocation.GuardKey, storedEffectGuard{OperationID: invocation.OperationID, Status: EffectUnknown}); err != nil {
				return err
			}
		}
		invocation.State = ToolInvocationDispatched
		dispatch = true
		return putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(invocation.RunID, invocation.BatchID, invocation.Ordinal), invocation)
	})
	return invocation, dispatch, err
}

func classifyInvocationEffect(kind ToolEffectKind, result ToolResult, executeErr error) (EffectReport, error) {
	report := result.Effect
	if !validEffectReport(report) {
		return EffectReport{Status: EffectUnknown}, fmt.Errorf("invalid effect status %q", report.Status)
	}
	switch kind {
	case ToolEffectReadOnly:
		if report.Status == EffectUnreported {
			return EffectReport{Status: EffectNone}, nil
		}
		if report.Status != EffectNone && report.Status != EffectNotApplied {
			return EffectReport{Status: EffectUnknown}, errors.New("read-only tool reported a mutation")
		}
	case ToolEffectMutating:
		if report.Status == EffectUnreported || report.Status == EffectNone {
			return EffectReport{Status: EffectUnknown}, errors.New("mutating tool did not return an authoritative effect report")
		}
	}
	// A returned Go error alone does not imply uncertainty. Legacy tools keep
	// EffectUnreported; explicit reports remain authoritative alongside errors.
	_ = executeErr
	return report, nil
}

func setInvocationError(invocation *storedToolInvocation, err error) {
	record := storedRuntimeRun{}
	setRuntimeError(&record, err)
	invocation.Error = record.Error
	invocation.ErrorKind = record.ErrorKind
	invocation.ErrorStopReason = record.ErrorStopReason
	invocation.ErrorRawReason = record.ErrorRawReason
}

func invocationError(invocation storedToolInvocation) error {
	return snapshotError(RunSnapshot{Failure: failureFromRecord(storedRuntimeRun{
		Error: invocation.Error, ErrorKind: invocation.ErrorKind,
		ErrorStopReason: invocation.ErrorStopReason, ErrorRawReason: invocation.ErrorRawReason,
	})})
}

func (r *Runtime) completeToolInvocation(ctx context.Context, invocation storedToolInvocation, result ToolResult, executeErr error, canonical ToolResult, fatal bool) error {
	effect, contractErr := classifyInvocationEffect(invocation.EffectKind, result, executeErr)
	if contractErr != nil {
		executeErr = errors.Join(executeErr, contractErr)
		fatal = true
	}
	invocation.Result = normalizeResult(canonical)
	invocation.Result = cloneToolResult(invocation.Result)
	invocation.Result.Effect = EffectReport{}
	invocation.Effect = effect
	setInvocationError(&invocation, executeErr)
	invocation.Fatal = fatal
	if effect.Status == EffectUnknown {
		invocation.State = ToolInvocationUncertain
	} else {
		invocation.State = ToolInvocationCompleted
	}
	if encoded, err := json.Marshal(invocation); err != nil {
		return err
	} else if len(encoded) > r.maxPayload {
		// The result cannot be stored whole, and a truncated result would
		// become false canonical history. Keep the effect report and leave the
		// invocation for an authoritative (smaller) resolution: the run needs
		// attention and Reconcile continues it without running the tool again.
		invocation.State = ToolInvocationUncertain
		invocation.Result = ToolResult{}
		invocation.Fatal = false
		setInvocationError(&invocation, fmt.Errorf("%w: tool result record is %d bytes, over the %d-byte limit", ErrPayloadTooLarge, len(encoded), r.maxPayload))
	}
	return r.transaction(context.WithoutCancel(ctx), true, func(tx StoreTransaction) error {
		if invocation.GuardKey != "" {
			status := effect.Status
			if status == EffectUnreported {
				status = EffectUnknown
			}
			if err := putStoredJSON(tx, runtimeEffectGuardsBucket, invocation.GuardKey, storedEffectGuard{OperationID: invocation.OperationID, Status: status}); err != nil {
				return err
			}
		}
		return putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(invocation.RunID, invocation.BatchID, invocation.Ordinal), invocation)
	})
}

func emitStoredToolResult(l *loop, invocation storedToolInvocation, err error) {
	result := invocation.Result
	l.emit(StreamEvent{
		Kind: StreamToolResult, ToolCall: invocation.Call, Result: result.Text(),
		ResultBlocks: cloneBlocks(result.Blocks), IsError: result.IsError, Err: err,
	})
}

func (r *Runtime) executeDurableToolBatch(ctx context.Context, runID string, l *loop, calls []ToolUseBlock, messages []Message, policy *toolPolicyState) ([]Message, error) {
	batch, invocations, created, suspended, err := r.prepareDurableBatch(ctx, runID, l, calls)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: prepare tool batch: %v", errDurableBatchIncomplete, err)
	}
	if len(invocations) != len(calls) || batch.Count != len(calls) {
		return nil, fmt.Errorf("%w: stored batch shape mismatch", errDurableBatchIncomplete)
	}
	if created {
		for _, invocation := range invocations {
			// Budget denial already emits while producing its result. These are
			// the synthetic semantic-guard outcomes created by durable planning.
			if invocation.State == ToolInvocationCompleted && (invocation.GuardKey != "" || invocation.Fatal) {
				emitStoredToolResult(l, invocation, invocationError(invocation))
			}
		}
	}

	// Any pending wait suspends the entire batch before workers run. All
	// pending child waits are rechecked here so the first child can never
	// strand its siblings: every admitted child is scheduled before the
	// worker suspends, and scheduling is idempotent.
	pendingChild := false
	for _, invocation := range invocations {
		if invocation.WaitID == "" || invocation.State != ToolInvocationReserved {
			continue
		}
		var wait storedWait
		if err := r.transaction(context.WithoutCancel(ctx), false, func(tx StoreTransaction) error {
			var err error
			wait, err = getStoredWait(tx, runID, invocation.WaitID)
			return err
		}); err != nil {
			return nil, fmt.Errorf("%w: inspect durable wait: %v", errDurableBatchIncomplete, err)
		}
		if wait.State != WaitPending {
			continue
		}
		if wait.Kind == WaitChild {
			// Covers a newly created batch and a batch reloaded after a crash
			// between child admission and scheduling.
			r.scheduleChild(ctx, wait.ChildRunID)
			pendingChild = true
		} else {
			pendingChild = true
		}
	}
	// A committed waiting state suspends even if a wait was resolved in the
	// meantime: that resolution made the run ready and owns its resumption.
	if pendingChild || suspended {
		return nil, errDurableWaiting
	}

	jobs := make(chan storedToolInvocation, len(invocations))
	for _, invocation := range invocations {
		if invocation.State == ToolInvocationReserved {
			jobs <- invocation
		}
	}
	close(jobs)
	workers := len(jobs)
	if max := policy.policy.MaxParallel; max > 0 && workers > max {
		workers = max
	}
	batchCtx, cancelBatch := context.WithCancelCause(ctx)
	defer cancelBatch(nil)
	var fatalOnce sync.Once
	var workErr error
	var workErrMu sync.Mutex
	recordWorkErr := func(err error) {
		if err == nil {
			return
		}
		workErrMu.Lock()
		workErr = errors.Join(workErr, err)
		workErrMu.Unlock()
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for invocation := range jobs {
				call := invocation.Call
				if cause := context.Cause(batchCtx); cause != nil {
					canonical := ErrorResult(canceledToolResult(cause))
					returned := ToolResult{Effect: EffectReport{Status: EffectNotApplied}}
					if err := r.completeToolInvocation(ctx, invocation, returned, batchCtx.Err(), canonical, false); err != nil {
						recordWorkErr(err)
					} else {
						invocation.Result = canonical
						emitStoredToolResult(l, invocation, batchCtx.Err())
					}
					continue
				}
				current := invocation
				dispatchAttempted := false
				dispatched := false
				var authorityErr error
				op := ToolOperation{ID: current.OperationID, RunID: runID, BatchID: batch.BatchID, Invocation: current.Ordinal, IdempotencyKey: current.OperationID}
				execCtx := withToolOperation(batchCtx, op)
				if current.WaitID != "" {
					execCtx = withDurableApproval(execCtx)
				}
				execCtx = withDurableToolDispatch(execCtx, func() error {
					// Nested process-local tools may inherit this context until T07 turns
					// children into durable runs. Only the outer binding owns this dispatch.
					if dispatchAttempted {
						return nil
					}
					dispatchAttempted = true
					// This check is intentionally inside the dispatch callback: approval,
					// timeout, and rate-limit work above may block. No host callback is held
					// inside the following storage transaction.
					if err := r.revalidateApproval(execCtx, runID, current); err != nil {
						// Parent cancellation remains fatal. A tool-owned deadline that
						// elapses during authorization is a recoverable, not-applied
						// pre-dispatch outcome like other tool policy timeouts.
						if parentErr := ctx.Err(); parentErr != nil {
							return parentErr
						}
						if policyErr := execCtx.Err(); policyErr != nil {
							authorityErr = policyErr
						} else if errors.Is(err, ErrApprovalUnauthorized) || errors.Is(err, ErrApprovalActionMismatch) || errors.Is(err, ErrWaitStale) || errors.Is(err, ErrWaitExpired) {
							authorityErr = err
						}
						return err
					}
					updated, didDispatch, err := r.beginToolDispatch(execCtx, current)
					if err != nil {
						return err
					}
					current = updated
					if !didDispatch {
						return errors.New("tool invocation is no longer reserved")
					}
					dispatched = true
					return nil
				})
				result, executeErr := l.safelyExecuteTool(execCtx, call, messages, policy, current.Budget)
				if dispatchAttempted && !dispatched {
					if authorityErr != nil {
						canonical := ErrorResult("denied: " + authorityErr.Error())
						returned := canonical
						returned.Effect = EffectReport{Status: EffectNotApplied}
						if completeErr := r.completeToolInvocation(ctx, current, returned, nil, canonical, false); completeErr != nil {
							recordWorkErr(completeErr)
						} else {
							current.Result = canonical
							emitStoredToolResult(l, current, authorityErr)
						}
						continue
					}
					if current.State == ToolInvocationCompleted {
						emitStoredToolResult(l, current, invocationError(current))
						continue
					}
					recordWorkErr(executeErr)
					cancelBatch(executeErr)
					continue
				}
				if !dispatched && result.Effect.Status == EffectUnreported {
					// Argument rejection, approval denial/failure, and limiter failure
					// happened before the external executor dispatch boundary.
					result.Effect = EffectReport{Status: EffectNotApplied}
				}
				canonical := result
				fatal := executeErr != nil
				if executeErr != nil {
					canonical = ErrorResult("aborted: tool execution failed")
					first := false
					fatalOnce.Do(func() {
						first = true
						cancelBatch(executeErr)
					})
					if !first && (errors.Is(executeErr, context.Canceled) || errors.Is(executeErr, context.DeadlineExceeded)) {
						canonical = ErrorResult(canceledToolResult(context.Cause(batchCtx)))
						fatal = false
					}
				}
				if err := r.completeToolInvocation(ctx, current, result, executeErr, canonical, fatal); err != nil {
					recordWorkErr(err)
					cancelBatch(err)
				} else if executeErr != nil {
					current.Result = canonical
					emitStoredToolResult(l, current, executeErr)
				}
			}
		}()
	}
	wg.Wait()
	if workErr != nil {
		return nil, fmt.Errorf("%w: persist tool outcome: %v", errDurableBatchIncomplete, workErr)
	}

	var current []storedToolInvocation
	if err := r.transaction(context.WithoutCancel(ctx), false, func(tx StoreTransaction) error {
		var err error
		current, err = loadStoredInvocations(tx, runID, batch.BatchID)
		return err
	}); err != nil {
		return nil, fmt.Errorf("%w: inspect tool outcomes: %v", errDurableBatchIncomplete, err)
	}
	results := make([]Message, len(current))
	var fatalErr error
	for i, invocation := range current {
		if invocation.State != ToolInvocationCompleted {
			if invocation.Error != "" {
				return nil, fmt.Errorf("%w: %w (%s: %s)", errDurableBatchIncomplete, ErrToolEffectUncertain, invocation.OperationID, invocation.Error)
			}
			return nil, fmt.Errorf("%w: %w (%s)", errDurableBatchIncomplete, ErrToolEffectUncertain, invocation.OperationID)
		}
		results[i] = invocationResultMessage(invocation)
		if invocation.Fatal && fatalErr == nil {
			fatalErr = invocationError(invocation)
			l.diagnostics = append(l.diagnostics, RunDiagnostic{InvocationID: invocation.OperationID, ToolCallID: invocation.Call.ID, Kind: "tool_execution_error", Message: invocation.Error, Data: append([]byte(nil), invocation.Call.Input...)})
		}
	}
	return results, fatalErr
}

func commitPendingToolBatch(tx StoreTransaction, record *storedRuntimeRun) error {
	if record.PendingBatchID == "" {
		return nil
	}
	batch, err := getStoredToolBatch(tx, record.RunID, record.PendingBatchID)
	if err != nil {
		return err
	}
	invocations, err := loadStoredInvocations(tx, record.RunID, record.PendingBatchID)
	if err != nil {
		return err
	}
	if len(invocations) != batch.Count {
		return fmt.Errorf("tool batch %s has %d invocations, want %d", batch.BatchID, len(invocations), batch.Count)
	}
	var fatalErr error
	for _, invocation := range invocations {
		if invocation.State != ToolInvocationCompleted {
			return fmt.Errorf("tool operation %s is %s", invocation.OperationID, invocation.State)
		}
		if invocation.Fatal && fatalErr == nil {
			fatalErr = invocationError(invocation)
		}
	}
	// Commit the fatal outcome with the transcript, not in a later run
	// finalization transaction. Recovery must never advance past this error.
	setRuntimeError(record, fatalErr)
	batch.Committed = true
	if err := putStoredJSON(tx, runtimeBatchesBucket, batchStorageKey(record.RunID, batch.BatchID), batch); err != nil {
		return err
	}
	record.PendingBatchID = ""
	return nil
}

func resolveReservedInvocations(tx StoreTransaction, record storedRuntimeRun) error {
	if record.PendingBatchID == "" {
		return nil
	}
	invocations, err := loadStoredInvocations(tx, record.RunID, record.PendingBatchID)
	if err != nil {
		return err
	}
	for _, invocation := range invocations {
		if invocation.State != ToolInvocationReserved {
			continue
		}
		invocation.State = ToolInvocationCompleted
		invocation.Result = ErrorResult("not executed: run cancelled")
		invocation.Effect = EffectReport{Status: EffectNotApplied}
		if invocation.ChildRunID != "" {
			// The admitted child may already have acted, or even completed
			// before its outcome was consumed. Its outcome and effect evidence
			// stay on the linked child run; the parent invocation itself
			// applies nothing, as for a consumed child.
			invocation.Result = ErrorResult(fmt.Sprintf("not consumed: run cancelled; the outcome of child run %s is recorded on that run", invocation.ChildRunID))
			invocation.Effect = EffectReport{Status: EffectNone}
		}
		setInvocationError(&invocation, nil)
		if invocation.GuardKey != "" {
			raw, guardErr := tx.Get(runtimeEffectGuardsBucket, invocation.GuardKey)
			if guardErr == nil {
				var guard storedEffectGuard
				if err := json.Unmarshal(raw, &guard); err != nil {
					return err
				}
				if guard.OperationID == invocation.OperationID {
					if err := putStoredJSON(tx, runtimeEffectGuardsBucket, invocation.GuardKey, storedEffectGuard{OperationID: invocation.OperationID, Status: EffectNotApplied}); err != nil {
						return err
					}
				}
			} else if !errors.Is(guardErr, ErrStoreKeyNotFound) {
				return guardErr
			}
		}
		if err := putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(record.RunID, record.PendingBatchID, invocation.Ordinal), invocation); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) recoverToolBatch(ctx context.Context, record storedRuntimeRun) (bool, error) {
	if record.PendingBatchID == "" {
		if record.State == RuntimeCancelRequested {
			full, err := r.load(ctx, record.RunID)
			if err != nil {
				return false, err
			}
			return false, r.completeExecution(record.RunID, full.Result, context.Canceled, nil)
		}
		if record.LastTransition == "provider_accepted" {
			full, err := r.load(ctx, record.RunID)
			if err != nil {
				return false, err
			}
			messages := full.Result.Messages
			if len(messages) > 0 {
				last := messages[len(messages)-1]
				if last.Role == "assistant" && len(last.ToolUses()) == 0 {
					return true, r.makeRunReady(ctx, record.RunID, record.Generation)
				}
			}
		}
		switch record.LastTransition {
		case "", "batch_ready", "response_classified", "batch_committed", transitionProviderAttemptRecovered:
			// No provider attempt is in flight: every attempt commits its record
			// before sending, and none is open. A run with no transition yet
			// stopped before its first attempt record.
			return true, r.makeRunReady(ctx, record.RunID, record.Generation)
		}
		return false, nil
	}
	var hasUncertain, cancelled bool
	var committed storedRuntimeRun
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		hasUncertain = false
		current, err := getRuntimeRun(tx, record.RunID)
		if err != nil {
			return err
		}
		if current.Generation != record.Generation {
			return nil
		}
		if current.State == RuntimeCancelRequested {
			if err := resolveReservedInvocations(tx, current); err != nil {
				return err
			}
		}
		invocations, err := loadStoredInvocations(tx, record.RunID, record.PendingBatchID)
		if err != nil {
			return err
		}
		for i := range invocations {
			if invocations[i].State == ToolInvocationDispatched {
				invocations[i].State = ToolInvocationUncertain
				invocations[i].Effect = EffectReport{Status: EffectUnknown}
				if err := putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(record.RunID, record.PendingBatchID, invocations[i].Ordinal), invocations[i]); err != nil {
					return err
				}
			}
			if invocations[i].State == ToolInvocationUncertain {
				hasUncertain = true
			}
		}
		cancelled = current.State == RuntimeCancelRequested
		if cancelled {
			current.State = RuntimeFinalizing
			current.Result.Status = RunCancelled
			current.AttentionReason = ""
			current.AttentionKind = ""
			setRuntimeError(&current, context.Canceled)
		} else if hasUncertain {
			current.State = RuntimeNeedsAttention
			current.AttentionReason = ErrToolEffectUncertain.Error()
			current.AttentionKind = "execution"
		} else {
			current.State = RuntimeReady
			current.AttentionReason = ""
			current.AttentionKind = ""
		}
		current.Generation++
		committed = current
		return putRuntimeRun(tx, current)
	})
	if err == nil && cancelled {
		return false, r.completeRunHooks(record.RunID)
	}
	if err == nil && hasUncertain {
		err = r.notifyParentOfAttention(ctx, committed)
	}
	return !hasUncertain && !cancelled, err
}

func (r *Runtime) makeRunReady(ctx context.Context, runID string, generation uint64) error {
	return r.transaction(ctx, true, func(tx StoreTransaction) error {
		record, err := getRuntimeRun(tx, runID)
		if err != nil {
			return err
		}
		if record.Generation != generation {
			return nil
		}
		record.State = RuntimeReady
		record.AttentionReason = ""
		record.AttentionKind = ""
		record.PayloadError, record.PayloadNeeded = "", 0
		record.Generation++
		return putRuntimeRun(tx, record)
	})
}

func reconciliationDigest(resolution EffectResolution) string {
	data, _ := json.Marshal(resolution)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// Reconcile authoritatively resolves an uncertain operation without executing
// the tool again. Exact retries are idempotent; a different resolution for an
// already resolved operation returns ErrReconciliationConflict.
func (h *RunHandle) Reconcile(ctx context.Context, operationID string, resolution EffectResolution) error {
	if operationID == "" {
		return ErrOperationNotFound
	}
	resolution.Result = normalizeResult(cloneToolResult(resolution.Result))
	resolution.Result.Effect = EffectReport{}
	if err := validateBlock(ToolResultBlock{ToolUseID: operationID, Content: resolution.Result.Blocks}); err != nil {
		return fmt.Errorf("invalid reconciled result: %w", err)
	}
	if resolution.Effect.Status != EffectNone && resolution.Effect.Status != EffectApplied && resolution.Effect.Status != EffectNotApplied {
		return fmt.Errorf("reconciliation requires an authoritative effect status")
	}
	digest := reconciliationDigest(resolution)
	ready := false
	err := h.runtime.transaction(ctx, true, func(tx StoreTransaction) error {
		receiptKey := reconcileReceiptKey(h.runID, operationID)
		if raw, err := tx.Get(runtimeReceiptsBucket, receiptKey); err == nil {
			var receipt reconcileReceipt
			if err := json.Unmarshal(raw, &receipt); err != nil {
				return err
			}
			if receipt.Digest != digest || !equalResolution(receipt.Resolution, resolution) {
				return ErrReconciliationConflict
			}
			return nil
		} else if !errors.Is(err, ErrStoreKeyNotFound) {
			return err
		}
		record, err := getRuntimeRun(tx, h.runID)
		if err != nil {
			return err
		}
		terminal := record.State == RuntimeTerminal
		if record.State != RuntimeNeedsAttention && !terminal {
			return fmt.Errorf("run %s is not waiting for reconciliation", h.runID)
		}
		var found *storedToolInvocation
		err = tx.Scan(runtimeInvocationsBucket, h.runID+"/", func(_ string, raw []byte) error {
			var invocation storedToolInvocation
			if err := json.Unmarshal(raw, &invocation); err != nil {
				return err
			}
			if invocation.OperationID == operationID {
				copy := invocation
				found = &copy
			}
			return nil
		})
		if err != nil {
			return err
		}
		if found == nil {
			return ErrOperationNotFound
		}
		if found.State != ToolInvocationUncertain {
			return fmt.Errorf("operation %s is %s, not uncertain", operationID, found.State)
		}
		found.State = ToolInvocationCompleted
		found.Result = cloneToolResult(resolution.Result)
		found.Result.Effect = EffectReport{}
		found.Effect = resolution.Effect
		found.Error = ""
		found.Fatal = false
		if err := putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(h.runID, found.BatchID, found.Ordinal), *found); err != nil {
			return err
		}
		if found.GuardKey != "" {
			if err := putStoredJSON(tx, runtimeEffectGuardsBucket, found.GuardKey, storedEffectGuard{OperationID: operationID, Status: resolution.Effect.Status}); err != nil {
				return err
			}
		}
		receipt := reconcileReceipt{Version: runtimeEncodingVersion, RunID: h.runID, OperationID: operationID, Digest: digest, Resolution: resolution}
		if err := putStoredJSON(tx, runtimeReceiptsBucket, receiptKey, receipt); err != nil {
			return err
		}
		invocations, err := loadStoredInvocations(tx, h.runID, record.PendingBatchID)
		if err != nil {
			return err
		}
		ready = !terminal
		for _, invocation := range invocations {
			if invocation.State == ToolInvocationUncertain || invocation.State == ToolInvocationDispatched {
				ready = false
				break
			}
		}
		if ready {
			record.State = RuntimeReady
			record.AttentionReason = ""
			record.AttentionKind = ""
			record.Generation++
			if err := putRuntimeRun(tx, record); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && ready {
		h.runtime.start(h.runID)
	}
	if err == nil {
		// Reconciling a terminal child's last uncertain effect (or one deep in
		// a canceled subtree) is what lets a blocked parent consume the child
		// outcome; the parent continues without replaying any child work.
		err = h.runtime.wakeParentFromChild(context.WithoutCancel(ctx), h.runID)
	}
	return err
}
