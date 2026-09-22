package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var errChildAdmissionConflict = errors.New("child admission identity reused with a different child request")

var errChildWaitHostResolution = errors.New("durable child wait is resolved by child completion")

// storedChildLink is the durable identity record joining a parent run's
// durable invocation (operation) to its admitted child run. It is the
// idempotency record for child admission: a replayed admission with the same
// parent operation and payload resolves the original child instead of creating
// a duplicate or an orphan.
type storedChildLink struct {
	Version            int       `json:"version"`
	ParentRunID        string    `json:"parent_run_id"`
	OperationID        string    `json:"operation_id"`
	ChildRunID         string    `json:"child_run_id"`
	DefinitionID       string    `json:"definition_id"`
	DefinitionRevision string    `json:"definition_revision"`
	TaskDigest         string    `json:"task_digest"`
	CreatedAt          time.Time `json:"created_at"`
}

func childLinkKey(parentRunID, operationID string) string {
	return parentRunID + "\x00" + operationID
}

// childAdmissionPayload is the canonical child admission payload. Its digest
// distinguishes an exact idempotent replay from a conflicting reuse of the
// same parent operation identity.
type childAdmissionPayload struct {
	DefinitionID string `json:"definition_id"`
	Revision     string `json:"revision"`
	Task         string `json:"task"`
}

func childAdmissionDigest(payload childAdmissionPayload) string {
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// childWaitIDFor derives the internal child wait ID from the parent operation.
// It is deliberately a different derivation from host-facing wait IDs so a
// child wait and a question/approval wait can never collide.
func childWaitIDFor(operationID string) string {
	sum := sha256.Sum256([]byte("childwait\x00" + operationID))
	return hex.EncodeToString(sum[:16])
}

// childAdmissionReceipt resolves a child admission exactly, so a lost
// acknowledgement can be replayed without creating a second child.
type childAdmissionReceipt struct {
	ChildRunID string
	WaitID     string
	Created    bool
}

// admitChildRun atomically persists an ordinary child run, the stable parent
// operation link, and the internal child wait within the caller's writable
// transaction, so slices that also reserve the parent invocation share one
// atomic boundary. It is idempotent by parent run plus operation identity:
//
//   - An existing link with the same payload digest resolves the original
//     child run, link, and pending wait (Created is false).
//   - An existing link with a different payload digest conflicts.
//   - A new identity requires the pinned child definition binding to be
//     registered (fail closed) and a non-empty deterministic task projection
//     and complete invocation identity; nothing partial can become runnable.
//
// The child run inherits the parent deadline here; propagation semantics for
// cancellation and optional child-local durations are owned by later slices.
// The child is created RuntimeReady but only the caller schedules it after the
// transaction commits.
func (r *Runtime) admitChildRun(tx StoreTransaction, parent storedRuntimeRun, invocation storedToolInvocation, policy DurableChildPolicy, task string) (childAdmissionReceipt, error) {
	receipt := childAdmissionReceipt{}
	if strings.ContainsRune(policy.DefinitionID, '\x00') || strings.ContainsRune(policy.Revision, '\x00') {
		return receipt, errors.New("durable child identities cannot contain NUL")
	}
	if policy.DefinitionID == "" || policy.Revision == "" {
		return receipt, errors.New("durable child policy requires a definition id and revision")
	}
	if invocation.OperationID == "" {
		return receipt, errors.New("child admission requires an operation identity")
	}
	if invocation.BatchID == "" || invocation.Ordinal < 0 {
		// A child wait that could never be completed must never be stored.
		return receipt, fmt.Errorf("child admission for operation %q requires a complete invocation identity", invocation.OperationID)
	}
	if task == "" {
		return receipt, fmt.Errorf("child admission for operation %q requires a projected task", invocation.OperationID)
	}
	digest := childAdmissionDigest(childAdmissionPayload{DefinitionID: policy.DefinitionID, Revision: policy.Revision, Task: task})

	rawLink, err := tx.Get(runtimeChildLinksBucket, childLinkKey(parent.RunID, invocation.OperationID))
	if err != nil && !errors.Is(err, ErrStoreKeyNotFound) {
		return receipt, err
	}
	if err == nil {
		var link storedChildLink
		if err := json.Unmarshal(rawLink, &link); err != nil {
			return receipt, fmt.Errorf("decode child link: %w", err)
		}
		if link.TaskDigest != digest {
			return receipt, fmt.Errorf("%w: operation %q", errChildAdmissionConflict, invocation.OperationID)
		}
		return r.resolveExistingChildAdmission(tx, link, invocation)
	}

	binding, err := r.binding(policy.DefinitionID, policy.Revision)
	if err != nil {
		// Fail closed: a child of an unregistered definition must not be
		// admitted or implicitly re-registered against a different revision.
		return receipt, fmt.Errorf("admit child for operation %q: %w", invocation.OperationID, err)
	}
	now := time.Now().UTC()
	childRunID := newRuntimeID()
	child := storedRuntimeRun{
		Version:            runtimeEncodingVersion,
		RunID:              childRunID,
		DefinitionID:       policy.DefinitionID,
		DefinitionRevision: policy.Revision,
		Task:               task,
		Deadline:           parent.Deadline,
		State:              RuntimeReady,
		Generation:         1,
		Result:             RunResult{RunID: childRunID},
		ParentRunID:        parent.RunID,
		ParentOperationID:  invocation.OperationID,
		// Pin the child definition's own subtree and PerTool caps on the child
		// record at admission; the child charges its ancestors from their
		// persisted records, not from any context.
		ToolBudget: pinnedToolCaps(binding.agent.toolPolicy),
	}
	if err := putRuntimeRun(tx, child); err != nil {
		return receipt, err
	}
	link := storedChildLink{
		Version: runtimeEncodingVersion, ParentRunID: parent.RunID, OperationID: invocation.OperationID,
		ChildRunID: childRunID, DefinitionID: policy.DefinitionID, DefinitionRevision: policy.Revision,
		TaskDigest: digest, CreatedAt: now,
	}
	if err := putStoredJSON(tx, runtimeChildLinksBucket, childLinkKey(parent.RunID, invocation.OperationID), link); err != nil {
		return receipt, err
	}
	wait := storedWait{
		Version: runtimeEncodingVersion, RunID: parent.RunID, ID: childWaitIDFor(invocation.OperationID),
		Kind: WaitChild, State: WaitPending, OperationID: invocation.OperationID,
		BatchID: invocation.BatchID, Ordinal: invocation.Ordinal, Tool: invocation.Call.Name,
		Arguments:    append(json.RawMessage(nil), invocation.Call.Input...),
		DefinitionID: policy.DefinitionID, DefinitionRevision: policy.Revision,
		ChildRunID: childRunID, CreatedAt: now,
	}
	if err := putStoredWait(tx, wait); err != nil {
		return receipt, err
	}
	return childAdmissionReceipt{ChildRunID: childRunID, WaitID: wait.ID, Created: true}, nil
}

// resolveExistingChildAdmission verifies that a replayed admission resolves a
// still-consistent child/link/wait triple. A missing or inconsistent piece is
// an internal-storage failure, never a reason to recreate the child.
func (r *Runtime) resolveExistingChildAdmission(tx StoreTransaction, link storedChildLink, invocation storedToolInvocation) (childAdmissionReceipt, error) {
	child, err := getRuntimeRun(tx, link.ChildRunID)
	if err != nil {
		return childAdmissionReceipt{}, fmt.Errorf("linked child run for operation %q: %w", link.OperationID, err)
	}
	if child.ParentRunID != link.ParentRunID || child.ParentOperationID != link.OperationID {
		return childAdmissionReceipt{}, fmt.Errorf("child run %q does not match its admission link for operation %q", child.RunID, link.OperationID)
	}
	wait, err := getStoredWait(tx, link.ParentRunID, childWaitIDFor(link.OperationID))
	if err != nil {
		return childAdmissionReceipt{}, fmt.Errorf("child wait for operation %q: %w", link.OperationID, err)
	}
	if wait.Kind != WaitChild || wait.ChildRunID != link.ChildRunID || wait.OperationID != link.OperationID ||
		wait.BatchID != invocation.BatchID || wait.Ordinal != invocation.Ordinal {
		return childAdmissionReceipt{}, fmt.Errorf("child wait for operation %q does not match its admission link", link.OperationID)
	}
	return childAdmissionReceipt{ChildRunID: link.ChildRunID, WaitID: wait.ID, Created: false}, nil
}

// durableChildDeclaration resolves the durable child declaration behind a
// registered executor, unwrapping first-party wrappers. A nil result with a
// nil error means the executor is an ordinary leaf tool. A detected transient
// child adapter is an invariant violation (Register rejects them), not a
// silently-executed child.
func durableChildDeclaration(tool Tool) (*durableChildTool, error) {
	inspection, err := inspectChildTool(tool)
	if err != nil {
		return nil, err
	}
	if inspection.transientAdapter {
		return nil, fmt.Errorf("process-local child adapter %q cannot be dispatched inside a durable batch", tool.Definition().Name)
	}
	if inspection.hasChild {
		return inspection.childTool, nil
	}
	return nil, nil
}

// scheduleChild starts an admitted child run if it is still admitted-but-
// unstarted and its binding is registered. It is a scheduling hint: calling it
// on a claimed, running, or terminal child is benign, and a missing binding
// leaves the child RuntimeReady so registration followed by Recover makes
// progress.
func (r *Runtime) scheduleChild(ctx context.Context, childRunID string) {
	record, err := r.load(ctx, childRunID)
	if err != nil {
		// Scheduling is a hint; the recovery scan is the durable backstop for
		// storage failures here.
		return
	}
	if record.State != RuntimeReady {
		return
	}
	if _, err := r.binding(record.DefinitionID, record.DefinitionRevision); err != nil {
		return
	}
	r.start(childRunID)
}

// childResultProjection builds the parent-facing tool result for one terminal
// child run. Accepted structured output stays the model-facing projection for
// declared contracts; otherwise the child's final rich blocks are carried
// through. A child that terminalized as failed or canceled becomes a
// model-visible error result: it is authoritative (the child had no uncertain
// effects or it would not be consumable) but the parent may still react.
// Child-side effect evidence is never duplicated here; it stays inspectable on
// the child run and through the linked wait/invocation snapshots.
func childResultProjection(child storedRuntimeRun) ToolResult {
	if child.Result.Status != RunCompleted {
		reason := child.Error
		if reason == "" {
			reason = string(child.Result.Status)
		}
		return ErrorResult(fmt.Sprintf("child run %s did not complete: %s", child.RunID, reason))
	}
	if len(child.Result.StructuredOutput) > 0 {
		// The accepted structured payload is the child's declared answer;
		// never copy the hidden structured-output tool call into the result.
		return TextResult(string(child.Result.StructuredOutput))
	}
	result := ToolResult{Blocks: cloneBlocks(child.Result.FinalMessage.Blocks)}
	for _, block := range result.Blocks {
		if err := validateBlock(block); err != nil {
			return TextResult(child.Result.FinalMessage.Text())
		}
	}
	return result
}

// completeChildInvocation completes the parent's child invocation with the
// projected result. The invocation itself applies no parent-side external
// effect (EffectNone): the child's own effect evidence lives on the child run
// and is reachable through ChildRunID on the wait and invocation snapshots.
func completeChildInvocation(tx StoreTransaction, wait storedWait, result ToolResult) error {
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
	invocation.Result.Effect = EffectReport{}
	invocation.Effect = EffectReport{Status: EffectNone}
	return putStoredJSON(tx, runtimeInvocationsBucket, invocationStorageKey(wait.RunID, wait.BatchID, wait.Ordinal), invocation)
}

// childHasUnresolvedEvidence reports whether a terminal child's subtree still
// carries uncertain (or dispatched, never-settled) effect evidence or any
// non-terminal descendant. Such a child must never be consumed as an ordinary
// completed (or failed) tool call; the parent stays blocked with explicit
// child attention instead. The whole subtree matters because a canceled child
// terminalizes before its non-cooperative descendants settle.
func childHasUnresolvedEvidence(tx StoreTransaction, child storedRuntimeRun) (bool, error) {
	seen := map[string]bool{child.RunID: true}
	queue := []string{child.RunID}
	for len(queue) > 0 {
		runID := queue[0]
		queue = queue[1:]
		unresolved := false
		err := tx.Scan(runtimeInvocationsBucket, runID+"/", func(_ string, raw []byte) error {
			var invocation storedToolInvocation
			if err := json.Unmarshal(raw, &invocation); err != nil {
				return err
			}
			if invocation.RunID == runID && (invocation.State == ToolInvocationUncertain || invocation.State == ToolInvocationDispatched) {
				unresolved = true
			}
			return nil
		})
		if err != nil || unresolved {
			return unresolved, err
		}
		children, err := linkedChildRunIDs(tx, runID)
		if err != nil {
			return false, err
		}
		for _, childRunID := range children {
			if seen[childRunID] {
				continue
			}
			seen[childRunID] = true
			descendant, err := getRuntimeRun(tx, childRunID)
			if err != nil {
				return false, err
			}
			if descendant.State != RuntimeTerminal {
				return true, nil
			}
			queue = append(queue, childRunID)
		}
	}
	return false, nil
}

// linkedChildRunIDs lists the child runs admitted by runID. Records are read
// by the caller after the scan closes.
func linkedChildRunIDs(tx StoreTransaction, runID string) ([]string, error) {
	var children []string
	err := tx.Scan(runtimeChildLinksBucket, runID+"\x00", func(_ string, raw []byte) error {
		var link storedChildLink
		if err := json.Unmarshal(raw, &link); err != nil {
			return fmt.Errorf("decode child link: %w", err)
		}
		children = append(children, link.ChildRunID)
		return nil
	})
	return children, err
}

// propagateCancellationTx durably cancels every non-terminal descendant
// linked beneath runID inside the caller's transaction, so the intent commits
// atomically with the ancestor's own transition: recovery can never lose it,
// and no descendant can admit children or dispatch reserved tools beneath the
// canceled ancestor. Suspended descendants terminalize at once; running
// descendants move to cancel-requested and are returned so the caller can
// stop their live workers after commit. A deadline propagates to suspended
// descendants only: each running descendant inherited that same deadline at
// admission and its own worker already enforces it.
func propagateCancellationTx(tx StoreTransaction, runID string, cause error) ([]string, error) {
	deadline := errors.Is(cause, context.DeadlineExceeded)
	var running []string
	seen := map[string]bool{runID: true}
	queue := []string{runID}
	for len(queue) > 0 {
		children, err := linkedChildRunIDs(tx, queue[0])
		if err != nil {
			return nil, err
		}
		queue = queue[1:]
		for _, childRunID := range children {
			if seen[childRunID] {
				continue
			}
			seen[childRunID] = true
			queue = append(queue, childRunID)
			child, err := getRuntimeRun(tx, childRunID)
			if err != nil {
				return nil, err
			}
			switch child.State {
			case RuntimeReady, RuntimeWaiting, RuntimeNeedsAttention:
				child.State = RuntimeTerminal
				child.Result.Status = RunCancelled
				child.AttentionReason = ""
				child.AttentionKind = ""
				setRuntimeError(&child, cause)
			case RuntimeRunning:
				if deadline {
					continue
				}
				child.State = RuntimeCancelRequested
				running = append(running, child.RunID)
			default:
				continue
			}
			if err := resolveReservedInvocations(tx, child); err != nil {
				return nil, err
			}
			if err := cancelRunWaits(tx, child.RunID); err != nil {
				return nil, err
			}
			child.Generation++
			if err := putRuntimeRun(tx, child); err != nil {
				return nil, err
			}
		}
	}
	return running, nil
}

// markParentChildAttentionTx moves a waiting parent to needs-attention with a
// child-specific kind so Snapshot/Await report blocked child progress instead
// of pretending ordinary waiting. It never touches a terminal parent (a
// canceled parent keeps its retained links and unresolved evidence), and a
// late notice for a child wait that is no longer pending is ignored.
func markParentChildAttentionTx(tx StoreTransaction, parentRunID, childRunID, reason string) error {
	record, err := getRuntimeRun(tx, parentRunID)
	if err != nil {
		return err
	}
	if record.State != RuntimeWaiting {
		return nil
	}
	child, err := getRuntimeRun(tx, childRunID)
	if err != nil {
		return err
	}
	if child.State != RuntimeNeedsAttention && child.State != RuntimeTerminal {
		// A delayed notice about a child that already resumed is stale.
		return nil
	}
	wait, err := getStoredWait(tx, parentRunID, childWaitIDFor(child.ParentOperationID))
	if errors.Is(err, ErrWaitNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if wait.State != WaitPending || wait.ChildRunID != childRunID {
		return nil
	}
	record.State = RuntimeNeedsAttention
	record.AttentionKind = "child"
	record.AttentionReason = fmt.Sprintf("durable child %s requires attention: %s", childRunID, reason)
	record.Generation++
	return putRuntimeRun(tx, record)
}

// refreshParentChildAttentionTx recomputes child attention on a parent that
// still has pending waits: it stays blocked only while some pending child
// needs attention or settled with unresolved evidence, and otherwise returns
// to ordinary waiting.
func refreshParentChildAttentionTx(tx StoreTransaction, parentRunID string) error {
	record, err := getRuntimeRun(tx, parentRunID)
	if err != nil {
		return err
	}
	if record.State != RuntimeNeedsAttention || record.AttentionKind != "child" {
		return nil
	}
	waits, err := loadRunWaits(tx, parentRunID)
	if err != nil {
		return err
	}
	for _, wait := range waits {
		if wait.Kind != WaitChild || wait.State != WaitPending {
			continue
		}
		child, err := getRuntimeRun(tx, wait.ChildRunID)
		if err != nil {
			return err
		}
		blocked := child.State == RuntimeNeedsAttention
		reason := child.AttentionReason
		if child.State == RuntimeTerminal {
			if blocked, err = childHasUnresolvedEvidence(tx, child); err != nil {
				return err
			}
			reason = "child retains unresolved effect evidence or descendants"
		}
		if blocked {
			record.AttentionReason = fmt.Sprintf("durable child %s requires attention: %s", child.RunID, reason)
			return putRuntimeRun(tx, record)
		}
	}
	record.State = RuntimeWaiting
	record.AttentionReason = ""
	record.AttentionKind = ""
	record.Generation++
	return putRuntimeRun(tx, record)
}

// notifyParentChildAttention propagates child-side attention to the parent
// after the child's attention transition committed.
func (r *Runtime) notifyParentChildAttention(ctx context.Context, parentRunID, childRunID, reason string) error {
	return r.transaction(ctx, true, func(tx StoreTransaction) error {
		return markParentChildAttentionTx(tx, parentRunID, childRunID, reason)
	})
}

// consumeChildCompletion consumes one terminal child outcome exactly once.
// It completes the child wait and the parent invocation, then makes the parent
// runnable when no other wait blocks it (including clearing a child-specific
// attention). A terminal child with uncertain effects or unresolved
// descendants is not consumed; the parent records child attention instead.
// An already consumed (or canceled) wait makes consumption a no-op, so a
// canceled parent is never resurrected by a late child completion.
func (r *Runtime) consumeChildCompletion(tx StoreTransaction, child storedRuntimeRun) (bool, error) {
	if child.State != RuntimeTerminal {
		return false, nil
	}
	wait, err := getStoredWait(tx, child.ParentRunID, childWaitIDFor(child.ParentOperationID))
	if err != nil {
		if errors.Is(err, ErrWaitNotFound) {
			return false, fmt.Errorf("child run %s has no parent child-wait for operation %q", child.RunID, child.ParentOperationID)
		}
		return false, err
	}
	if wait.Kind != WaitChild || wait.ChildRunID != child.RunID {
		return false, fmt.Errorf("child wait for parent %s does not match terminal child run %s", child.ParentRunID, child.RunID)
	}
	if wait.State != WaitPending {
		// Already consumed, expired, or canceled with the parent. Late child
		// completions never resurrect or double-wake the parent.
		return false, nil
	}
	blocked, err := childHasUnresolvedEvidence(tx, child)
	if err != nil {
		return false, err
	}
	if blocked {
		return false, markParentChildAttentionTx(tx, child.ParentRunID, child.RunID, "child retains unresolved effect evidence or descendants")
	}
	wait.State = WaitConsumed
	wait.ResolvedAt = time.Now().UTC()
	if err := putStoredWait(tx, wait); err != nil {
		return false, err
	}
	if err := completeChildInvocation(tx, wait, childResultProjection(child)); err != nil {
		return false, err
	}
	ready, err := markRunReadyAfterWaits(tx, child.ParentRunID)
	if err != nil || ready {
		return ready, err
	}
	// Other waits remain; the consumed child may have been the only reason
	// the parent reported child attention.
	return false, refreshParentChildAttentionTx(tx, child.ParentRunID)
}

// wakeParentFromChild is the parent-side completion boundary for one terminal
// child run; for a non-terminal child it only recomputes the parent's child
// attention. It is idempotent and safe to call from any terminal writer,
// reconciliation, or recovery pass; only a pending child wait is consumed, and
// only a waiting parent (or one blocked on child attention) becomes runnable.
// When the parent is itself terminal (for example canceled before this
// descendant settled), the notification climbs to the parent's own parent so
// a late settlement deep in a canceled subtree still unblocks the nearest
// live ancestor.
func (r *Runtime) wakeParentFromChild(ctx context.Context, childRunID string) error {
	for depth := 0; childRunID != "" && depth <= maxReservationAncestorDepth; depth++ {
		var ready, climb bool
		var parentRunID string
		err := r.transaction(ctx, true, func(tx StoreTransaction) error {
			ready, climb, parentRunID = false, false, ""
			child, err := getRuntimeRun(tx, childRunID)
			if err != nil {
				return err
			}
			if child.ParentRunID == "" {
				return nil
			}
			if child.State != RuntimeTerminal {
				// A child that left attention (for example after reconciliation)
				// no longer blocks its waiting parent.
				return refreshParentChildAttentionTx(tx, child.ParentRunID)
			}
			parentRunID = child.ParentRunID
			ready, err = r.consumeChildCompletion(tx, child)
			if err != nil {
				return err
			}
			parent, err := getRuntimeRun(tx, parentRunID)
			if err != nil {
				return err
			}
			climb = parent.State == RuntimeTerminal && parent.ParentRunID != ""
			return nil
		})
		if err != nil {
			return err
		}
		if ready {
			r.start(parentRunID)
		}
		if !climb {
			return nil
		}
		childRunID = parentRunID
	}
	return nil
}

// reconcileChildWaits rechecks every pending child wait of one run during
// recovery. Terminal children are consumed (wake once) or turned into explicit
// child attention when they retain unresolved evidence; ready children are
// collected for scheduling after the transaction commits. It returns true when
// the run became runnable.
func (r *Runtime) reconcileChildWaits(ctx context.Context, runID string) (bool, error) {
	var schedule []string
	ready := false
	err := r.transaction(ctx, true, func(tx StoreTransaction) error {
		waits, err := loadRunWaits(tx, runID)
		if err != nil {
			return err
		}
		for _, wait := range waits {
			if wait.Kind != WaitChild || wait.State != WaitPending {
				continue
			}
			child, err := getRuntimeRun(tx, wait.ChildRunID)
			if err != nil {
				return err
			}
			switch child.State {
			case RuntimeTerminal:
				ready, err = r.consumeChildCompletion(tx, child)
				if err != nil {
					return err
				}
			case RuntimeReady:
				schedule = append(schedule, child.RunID)
			case RuntimeNeedsAttention:
				if err := markParentChildAttentionTx(tx, runID, child.RunID, child.AttentionReason); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, childRunID := range schedule {
		r.scheduleChild(ctx, childRunID)
	}
	return ready, nil
}

// TreeAccounting aggregates the recorded provider accounting of a run and
// every durable descendant linked beneath it. It is derived from each run's
// persisted local values in one read transaction, so every run counts exactly
// once no matter how often recovery or child completion repeats. It is not a
// budget or ledger: RunResult.Usage on every run remains local.
type TreeAccounting struct {
	// Runs is the number of runs counted: the run itself plus its linked
	// descendants.
	Runs int
	// Usage sums each counted run's known local RunResult.Usage.
	Usage Usage
	// ProviderAttempts sums each counted run's recorded provider attempts.
	ProviderAttempts int
	// UnknownAttempts sums each counted run's unknown provider attempts (see
	// [RunAccounting.UnknownAttempts]). Their usage is not included in Usage.
	UnknownAttempts int
	// Unsettled counts descendants that are not terminal; their local values
	// can still grow.
	Unsettled int
}

// loadTreeAccounting walks the child links beneath root breadth-first,
// reading compact records only.
func loadTreeAccounting(tx StoreTransaction, root storedRuntimeRun) (TreeAccounting, error) {
	var tree TreeAccounting
	seen := map[string]bool{root.RunID: true}
	queue := []storedRuntimeRun{root}
	for len(queue) > 0 {
		run := queue[0]
		queue = queue[1:]
		tree.Runs++
		usage := run.Result.Usage
		tree.Usage.Add(&usage)
		tree.ProviderAttempts += run.Result.ProviderAttempts
		tree.UnknownAttempts += run.UnknownAttempts
		if run.RunID != root.RunID && run.State != RuntimeTerminal {
			tree.Unsettled++
		}
		children, err := linkedChildRunIDs(tx, run.RunID)
		if err != nil {
			return TreeAccounting{}, err
		}
		for _, childRunID := range children {
			if seen[childRunID] {
				continue
			}
			seen[childRunID] = true
			child, err := getRuntimeRun(tx, childRunID)
			if err != nil {
				return TreeAccounting{}, fmt.Errorf("linked child run %s: %w", childRunID, err)
			}
			queue = append(queue, child)
		}
	}
	return tree, nil
}
