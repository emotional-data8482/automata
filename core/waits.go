package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// WaitKind identifies why durable execution is suspended.
type WaitKind string

const (
	WaitQuestion WaitKind = "question"
	WaitApproval WaitKind = "approval"

	// WaitChild is the internal wait a Runtime run holds while an admitted
	// durable child run is pending. It is never created by host-facing
	// WithDurableWait policies and is resolved only by child completion.
	WaitChild WaitKind = "child"
)

// WaitState is the durable state of a question or approval.
type WaitState string

const (
	WaitPending   WaitState = "pending"
	WaitResolved  WaitState = "resolved"
	WaitConsumed  WaitState = "consumed"
	WaitExpired   WaitState = "expired"
	WaitCancelled WaitState = "cancelled"
)

const (
	maxWaitAnswerBytes = 1 << 20
	maxWaitTextBytes   = 64 << 10
	maxWaitAuditBytes  = 4 << 10
)

var (
	ErrWaitNotFound           = errors.New("durable wait not found")
	ErrWaitConflict           = errors.New("durable wait response conflicts with its committed resolution")
	ErrWaitStale              = errors.New("durable wait is no longer pending")
	ErrWaitExpired            = errors.New("durable wait expired")
	ErrApprovalUnauthorized   = errors.New("approval is not currently authorized")
	ErrApprovalActionMismatch = errors.New("approval action digest does not match")
	errDurableWaiting         = errors.New("durable run is waiting")
)

// DurableWaitPolicy makes a tool call suspend a Runtime run. A question's
// answer becomes the tool result and the wrapped tool is not executed. An
// approval authorizes the exact wrapped-tool invocation to dispatch.
//
// Prompt and Target run before the wait is committed and must be deterministic.
// Target is required for approvals. PolicyContext is an application revision or
// policy identity pinned into the action digest; it is not a credential.
type DurableWaitPolicy struct {
	Kind          WaitKind
	Prompt        func(json.RawMessage) (string, error)
	Target        func(json.RawMessage) (string, error)
	PolicyContext string
	ExpiresAfter  time.Duration
}

type durableWaitTool struct {
	Tool
	policy DurableWaitPolicy
}

func (t *durableWaitTool) durableWaitPolicy() DurableWaitPolicy { return t.policy }
func (t *durableWaitTool) toolEffectPolicy() ToolEffectPolicy {
	policy, _ := effectPolicyFor(t.Tool)
	return policy
}

type durableWaitPolicyProvider interface {
	durableWaitPolicy() DurableWaitPolicy
}

// WithDurableWait attaches a durable question or approval policy to a tool.
// The policy is used only by Runtime; direct Agent runs retain the existing
// process-local Approver and execute the wrapped tool normally.
func WithDurableWait(tool Tool, policy DurableWaitPolicy) Tool {
	return &durableWaitTool{Tool: tool, policy: policy}
}

func waitPolicyFor(tool Tool) (DurableWaitPolicy, bool, error) {
	provider, ok := tool.(durableWaitPolicyProvider)
	if !ok {
		return DurableWaitPolicy{}, false, nil
	}
	policy := provider.durableWaitPolicy()
	if policy.Kind == "" {
		return DurableWaitPolicy{}, false, nil
	}
	if policy.Kind != WaitQuestion && policy.Kind != WaitApproval {
		return DurableWaitPolicy{}, true, fmt.Errorf("invalid durable wait kind %q", policy.Kind)
	}
	if policy.ExpiresAfter < 0 {
		return DurableWaitPolicy{}, true, errors.New("durable wait expiry cannot be negative")
	}
	if len(policy.PolicyContext) > maxWaitAuditBytes {
		return DurableWaitPolicy{}, true, errors.New("durable wait policy context is too large")
	}
	if policy.Kind == WaitApproval && policy.Target == nil {
		return DurableWaitPolicy{}, true, errors.New("durable approval requires a target resolver")
	}
	return policy, true, nil
}

// ApprovalAuthorization is supplied to the host at response time and again
// immediately before dispatch. Actor is audit/authentication context supplied
// by the host; core never treats the string itself as proof of identity.
type ApprovalAuthorization struct {
	RunID              string
	WaitID             string
	OperationID        string
	Tool               string
	Arguments          json.RawMessage
	Target             string
	ActionDigest       string
	DefinitionID       string
	DefinitionRevision string
	PolicyContext      string
	Actor              string
}

// ApprovalAuthorizer validates current host authority. It must consult current
// credentials/policy rather than treating a persisted approval as a credential.
type ApprovalAuthorizer interface {
	AuthorizeApproval(context.Context, ApprovalAuthorization) error
}

// ApprovalAuthorizerFunc adapts a function to ApprovalAuthorizer.
type ApprovalAuthorizerFunc func(context.Context, ApprovalAuthorization) error

func (f ApprovalAuthorizerFunc) AuthorizeApproval(ctx context.Context, request ApprovalAuthorization) error {
	return f(ctx, request)
}

// WaitResolution answers a durable wait. Question answers must contain valid
// JSON. Approval decisions support only Allow or Deny: modified actions require
// a new model-requested invocation and approval.
type WaitResolution struct {
	Answer       json.RawMessage `json:"answer,omitempty"`
	Decision     Outcome         `json:"decision,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	Actor        string          `json:"actor,omitempty"`
	ActionDigest string          `json:"action_digest,omitempty"`
}

// WaitSnapshot is the host-facing durable question/approval record.
type WaitSnapshot struct {
	ID                 string
	Kind               WaitKind
	State              WaitState
	OperationID        string
	Tool               string
	Arguments          json.RawMessage
	Prompt             string
	Target             string
	ActionDigest       string
	DefinitionRevision string
	PolicyContext      string
	// ChildRunID is set on internal child waits and links the suspended
	// invocation to its admitted child run. It is empty on question and
	// approval waits.
	ChildRunID string
	ExpiresAt  time.Time
	Resolution WaitResolution
	CreatedAt  time.Time
	ResolvedAt time.Time
}

type storedWait struct {
	Version            int             `json:"version"`
	RunID              string          `json:"run_id"`
	ID                 string          `json:"id"`
	Kind               WaitKind        `json:"kind"`
	State              WaitState       `json:"state"`
	OperationID        string          `json:"operation_id"`
	BatchID            string          `json:"batch_id"`
	Ordinal            int             `json:"ordinal"`
	Tool               string          `json:"tool"`
	Arguments          json.RawMessage `json:"arguments"`
	Prompt             string          `json:"prompt,omitempty"`
	Target             string          `json:"target,omitempty"`
	ActionDigest       string          `json:"action_digest,omitempty"`
	DefinitionID       string          `json:"definition_id"`
	DefinitionRevision string          `json:"definition_revision"`
	ChildRunID         string          `json:"child_run_id,omitempty"`
	PolicyContext      string          `json:"policy_context,omitempty"`
	ExpiresAt          time.Time       `json:"expires_at,omitzero"`
	Resolution         WaitResolution  `json:"resolution,omitzero"`
	ResolutionDigest   string          `json:"resolution_digest,omitempty"`
	ResolutionError    string          `json:"resolution_error,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	ResolvedAt         time.Time       `json:"resolved_at,omitzero"`
}

func waitStorageKey(runID, waitID string) string { return runID + "/" + waitID }

func waitIDFor(operationID string) string {
	sum := sha256.Sum256([]byte("wait\x00" + operationID))
	return hex.EncodeToString(sum[:16])
}

func approvalActionDigest(wait storedWait) string {
	payload := struct {
		OperationID        string          `json:"operation_id"`
		Tool               string          `json:"tool"`
		Arguments          json.RawMessage `json:"arguments"`
		Target             string          `json:"target"`
		DefinitionRevision string          `json:"definition_revision"`
		PolicyContext      string          `json:"policy_context"`
	}{wait.OperationID, wait.Tool, wait.Arguments, wait.Target, wait.DefinitionRevision, wait.PolicyContext}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func waitResolutionDigest(resolution WaitResolution) string {
	data, _ := json.Marshal(resolution)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func waitSnapshot(wait storedWait) WaitSnapshot {
	return WaitSnapshot{
		ID: wait.ID, Kind: wait.Kind, State: wait.State, OperationID: wait.OperationID,
		Tool: wait.Tool, Arguments: append(json.RawMessage(nil), wait.Arguments...), Prompt: wait.Prompt,
		Target: wait.Target, ActionDigest: wait.ActionDigest, DefinitionRevision: wait.DefinitionRevision,
		PolicyContext: wait.PolicyContext, ChildRunID: wait.ChildRunID, ExpiresAt: wait.ExpiresAt,
		Resolution: cloneWaitResolution(wait.Resolution), CreatedAt: wait.CreatedAt, ResolvedAt: wait.ResolvedAt,
	}
}

func cloneWaitResolution(in WaitResolution) WaitResolution {
	in.Answer = append(json.RawMessage(nil), in.Answer...)
	return in
}

func loadRunWaits(tx StoreTransaction, runID string) ([]storedWait, error) {
	var waits []storedWait
	err := tx.Scan(runtimeWaitsBucket, runID+"/", func(_ string, raw []byte) error {
		var wait storedWait
		if err := json.Unmarshal(raw, &wait); err != nil {
			return err
		}
		if wait.Version != runtimeEncodingVersion {
			return fmt.Errorf("unsupported durable wait version %d", wait.Version)
		}
		waits = append(waits, wait)
		return nil
	})
	return waits, err
}

func approvalAuthorization(wait storedWait) ApprovalAuthorization {
	return ApprovalAuthorization{
		RunID: wait.RunID, WaitID: wait.ID, OperationID: wait.OperationID, Tool: wait.Tool,
		Arguments: append(json.RawMessage(nil), wait.Arguments...), Target: wait.Target,
		ActionDigest: wait.ActionDigest, DefinitionID: wait.DefinitionID,
		DefinitionRevision: wait.DefinitionRevision, PolicyContext: wait.PolicyContext,
		Actor: wait.Resolution.Actor,
	}
}
