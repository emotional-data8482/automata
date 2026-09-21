package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// EffectStatus describes what an executor authoritatively knows about an
// external mutation. It is deliberately independent of ToolResult.IsError and
// the Go error returned by Tool.Execute.
type EffectStatus string

const (
	// EffectUnreported is the zero value used by legacy tools. It makes no claim
	// about whether external state changed.
	EffectUnreported EffectStatus = ""
	// EffectNone means the operation was observational and made no external
	// mutation.
	EffectNone EffectStatus = "none"
	// EffectApplied means the mutation is known to have happened. Receipt should
	// identify the destination-side result when one exists.
	EffectApplied EffectStatus = "applied"
	// EffectNotApplied means the operation is authoritatively known not to have
	// changed external state.
	EffectNotApplied EffectStatus = "not_applied"
	// EffectUnknown means a mutation may have happened and must not be replayed
	// without authoritative reconciliation.
	EffectUnknown EffectStatus = "unknown"
)

// EffectReport is durable evidence about an external effect. Receipt is
// opaque application data (for example, a destination operation ID or content
// digest); core stores it but does not interpret it.
type EffectReport struct {
	Status  EffectStatus `json:"status,omitempty"`
	Receipt string       `json:"receipt,omitempty"`
}

// ToolEffectKind declares the recovery contract of a tool binding.
type ToolEffectKind string

const (
	// ToolEffectLegacy preserves compatibility for an unclassified Tool. A
	// returned result is recorded normally, but a process loss after dispatch is
	// uncertain and cannot be retried automatically.
	ToolEffectLegacy ToolEffectKind = "legacy"
	// ToolEffectReadOnly declares that successful execution has no external
	// mutation. Read-only does not make an interrupted request replay-safe.
	ToolEffectReadOnly ToolEffectKind = "read_only"
	// ToolEffectMutating requires the tool to return an explicit EffectReport.
	ToolEffectMutating ToolEffectKind = "mutating"
)

// ToolEffectPolicy declares how Runtime records and guards one tool binding.
//
// SemanticKey is optional. For mutating tools, when Scope and SemanticKey are
// both set, Runtime derives the key from the model-requested arguments, reserves
// Scope+tool-name+key before dispatch, and rejects a second operation after an
// applied or unresolved mutation. Approval-modified action binding is added by
// the durable approval lifecycle. This is an
// application-level semantic guard, not a substitute for destination-side
// idempotency.
type ToolEffectPolicy struct {
	Kind        ToolEffectKind
	Scope       string
	SemanticKey func(json.RawMessage) (string, error)
}

// ToolOperation is the stable identity attached to a durable tool execution.
// Mutating tools should pass ID (or a deterministic derivative) to external
// APIs that support idempotency keys.
type ToolOperation struct {
	ID             string
	RunID          string
	BatchID        string
	Invocation     int
	IdempotencyKey string
}

type toolOperationContextKey struct{}

// ToolOperationFromContext returns the durable operation identity supplied by
// Runtime. Direct Agent runs do not install one.
func ToolOperationFromContext(ctx context.Context) (ToolOperation, bool) {
	op, ok := ctx.Value(toolOperationContextKey{}).(ToolOperation)
	return op, ok
}

func withToolOperation(ctx context.Context, op ToolOperation) context.Context {
	return context.WithValue(ctx, toolOperationContextKey{}, op)
}

type effectPolicyTool struct {
	Tool
	policy ToolEffectPolicy
}

func (t *effectPolicyTool) toolEffectPolicy() ToolEffectPolicy { return t.policy }
func (t *effectPolicyTool) durableWaitPolicy() DurableWaitPolicy {
	policy, _, _ := waitPolicyFor(t.Tool)
	return policy
}

// WithToolEffectPolicy attaches an explicit durable effect contract to a Tool.
// Invalid policies are rejected when the tool is registered for a run.
func WithToolEffectPolicy(tool Tool, policy ToolEffectPolicy) Tool {
	return &effectPolicyTool{Tool: tool, policy: policy}
}

type toolEffectPolicyProvider interface {
	toolEffectPolicy() ToolEffectPolicy
}

func effectPolicyFor(tool Tool) (ToolEffectPolicy, error) {
	policy := ToolEffectPolicy{Kind: ToolEffectLegacy}
	if provider, ok := tool.(toolEffectPolicyProvider); ok {
		policy = provider.toolEffectPolicy()
	}
	switch policy.Kind {
	case ToolEffectLegacy, ToolEffectReadOnly, ToolEffectMutating:
	default:
		return ToolEffectPolicy{}, fmt.Errorf("invalid tool effect kind %q", policy.Kind)
	}
	if policy.SemanticKey != nil {
		if policy.Kind != ToolEffectMutating {
			return ToolEffectPolicy{}, errors.New("semantic effect guards require a mutating tool")
		}
		if policy.Scope == "" {
			return ToolEffectPolicy{}, errors.New("semantic effect guards require a scope")
		}
	}
	return policy, nil
}

func cloneToolResult(result ToolResult) ToolResult {
	result.Blocks = cloneBlocks(result.Blocks)
	return result
}

func validEffectReport(report EffectReport) bool {
	switch report.Status {
	case EffectUnreported, EffectNone, EffectApplied, EffectNotApplied, EffectUnknown:
		return true
	default:
		return false
	}
}

// ToolInvocationState is the durable progress state of one model-requested
// tool call.
type ToolInvocationState string

const (
	ToolInvocationReserved   ToolInvocationState = "reserved"
	ToolInvocationDispatched ToolInvocationState = "dispatched"
	ToolInvocationCompleted  ToolInvocationState = "completed"
	ToolInvocationUncertain  ToolInvocationState = "uncertain"
)

// ToolInvocationSnapshot is an inspectable per-call durable record. Calls are
// returned in model request order; partial batches are not inserted into the
// canonical transcript until every invocation is complete.
type ToolInvocationSnapshot struct {
	OperationID string
	Ordinal     int
	Call        ToolUseBlock
	State       ToolInvocationState
	Result      ToolResult
	Effect      EffectReport
	Error       string
	GuardKey    string
	WaitID      string
}

// ToolBatchSnapshot describes one durable model tool-request batch.
type ToolBatchSnapshot struct {
	BatchID     string
	Ordinal     int
	Committed   bool
	Invocations []ToolInvocationSnapshot
}

// EffectResolution is an authoritative reconciliation of an uncertain
// invocation. Result is the content to commit to canonical model history;
// Effect must be None, Applied, or NotApplied (never Unknown or Unreported).
type EffectResolution struct {
	Result ToolResult
	Effect EffectReport
}

func equalResolution(a, b EffectResolution) bool {
	return reflect.DeepEqual(cloneToolResult(a.Result), cloneToolResult(b.Result)) && a.Effect == b.Effect
}
