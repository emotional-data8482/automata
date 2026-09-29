package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SubmitOption configures one admission. See [WithIdempotencyKey],
// [WithDeadline], and [WithConversation].
type SubmitOption func(*submitOptions)

type submitOptions struct {
	Scope        string
	Key          string
	Deadline     time.Time
	Conversation conversationOptions
	err          error
}

// WithIdempotencyKey names the admission with an external identity, for
// example a tenant and the ID of the task or workflow node the run performs.
// An exact retry returns the original run, even after a lost acknowledgement
// or a restart; a retry that changes the definition, task, deadline, or
// conversation returns [ErrAdmissionConflict]. Scope and key are both
// required.
func WithIdempotencyKey(scope, key string) SubmitOption {
	return func(o *submitOptions) {
		if scope == "" || key == "" {
			o.err = errors.New("idempotency scope and key are both required")
		}
		o.Scope, o.Key = scope, key
	}
}

// WithDeadline sets the run's logical deadline. It is persisted with the
// run, survives restarts and suspension, and is inherited by child runs.
func WithDeadline(deadline time.Time) SubmitOption {
	return func(o *submitOptions) { o.Deadline = deadline }
}

// WithConversation admits the run as the next turn of a durable
// conversation. expectedHead is the run ID of the committed turn this one
// continues, or empty to start the conversation. Turns are serialized: a
// competing turn returns [ErrConversationBusy], and a stale head or a
// different definition returns [ErrConversationConflict].
func WithConversation(conversation ConversationRef, expectedHead string) SubmitOption {
	return func(o *submitOptions) {
		o.Conversation = conversationOptions{Scope: conversation.Scope, ID: conversation.ID, ExpectedHead: expectedHead}
		if conversation.ID == "" {
			o.err = errors.New("conversation id is required")
		}
	}
}

func resolveSubmitOptions(opts []SubmitOption) (submitOptions, error) {
	var options submitOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	return options, options.err
}

// admissionPayload is the canonical admission identity payload. Its JSON
// encoding remains the version 2+ digest rule: changing any field, tag, or encoding
// changes every persisted admission digest and requires a new encoding
// version.
type admissionPayload struct {
	DefinitionID string    `json:"definition_id"`
	Revision     string    `json:"revision"`
	Task         string    `json:"task"`
	Deadline     time.Time `json:"deadline,omitempty"`
	// Conversation fields are omitted when empty so ordinary admission
	// digests keep their encoding.
	ConversationScope string `json:"conversation_scope,omitempty"`
	ConversationID    string `json:"conversation_id,omitempty"`
	ExpectedHead      string `json:"expected_head,omitempty"`
}

// admissionDigest derives the canonical admission identity digest. The
// deadline is normalized to UTC before encoding: Go marshals time.Time in the
// value's own location, and the same instant expressed in a different zone
// must resolve the same run, not mint a new identity.
func admissionDigest(payload admissionPayload) string {
	payload.Deadline = payload.Deadline.UTC()
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type admissionRecord struct {
	RunID  string `json:"run_id"`
	Digest string `json:"digest"`
}

// Submit durably admits task as a new run of the registered definition and
// starts it. It returns once admission commits; ctx bounds only admission.
// The run continues after the caller disconnects: use the returned handle to
// await, observe, answer, cancel, or reconcile it.
func (r *Runtime) Submit(ctx context.Context, definition DefinitionRef, task string, opts ...SubmitOption) (*RunHandle, error) {
	return r.submit(ctx, definition, task, opts, true)
}

func (r *Runtime) submit(ctx context.Context, definition DefinitionRef, task string, opts []SubmitOption, start bool) (*RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if task == "" {
		return nil, fmt.Errorf("task is required")
	}
	options, err := resolveSubmitOptions(opts)
	if err != nil {
		return nil, err
	}
	definitionID, revision := definition.ID, definition.Revision
	if strings.ContainsRune(options.Scope, '\x00') || strings.ContainsRune(options.Key, '\x00') {
		return nil, fmt.Errorf("submission scope and key cannot contain NUL")
	}
	if err := validateConversationOptions(options.Conversation); err != nil {
		return nil, err
	}
	binding, err := r.binding(definition)
	if err != nil {
		return nil, err
	}
	// Pin the definition's total subtree cap and local PerTool caps on the
	// record at admission so reservations and descendant checks read persisted
	// values, never context pointers. Replayed admissions keep their original
	// pinned caps.
	caps := pinnedToolCaps(binding.agent.toolPolicy)
	digestText := admissionDigest(admissionPayload{
		DefinitionID: definitionID, Revision: revision, Task: task, Deadline: options.Deadline,
		ConversationScope: options.Conversation.Scope, ConversationID: options.Conversation.ID,
		ExpectedHead: options.Conversation.ExpectedHead,
	})
	var runID string
	var shouldStart bool
	err = r.transaction(ctx, true, func(tx StoreTransaction) error {
		if options.Key != "" {
			admissionKey := options.Scope + "\x00" + options.Key
			raw, err := tx.Get(runtimeAdmissionBucket, admissionKey)
			if err == nil {
				var existing admissionRecord
				if err := json.Unmarshal(raw, &existing); err != nil {
					return err
				}
				if existing.Digest != digestText {
					return ErrAdmissionConflict
				}
				runID = existing.RunID
				record, err := getRuntimeRun(tx, runID)
				if err != nil {
					return err
				}
				shouldStart = record.State == RuntimeReady
				return nil
			}
			if !errors.Is(err, ErrStoreKeyNotFound) {
				return err
			}
		}
		runID = newRuntimeID()
		shouldStart = true
		record := storedRuntimeRun{
			Version: runtimeEncodingVersion, RunID: runID, DefinitionID: definitionID,
			DefinitionRevision: revision, Task: task, Deadline: options.Deadline,
			State: RuntimeReady, Generation: 1, Result: RunResult{RunID: runID},
			ToolBudget: caps,
		}
		if options.Conversation.ID != "" {
			// Reserve the conversation's active slot and, atomically with the
			// new run, continue the head's committed history: the turn
			// references that immutable transcript instead of copying it and
			// appends only this task.
			head, continues, err := admitConversationTurnTx(tx, options.Conversation, definitionID, revision, task, runID)
			if err != nil {
				return err
			}
			record.ConversationScope, record.ConversationID = options.Conversation.Scope, options.Conversation.ID
			if continues {
				record.TranscriptBase, record.TranscriptBaseMessages = head.RunID, head.TranscriptMessages
				record.TranscriptMessages = head.TranscriptMessages
				if err := appendTranscriptDelta(tx, runID, &record, []Message{UserMessage(task)}); err != nil {
					return err
				}
			}
		}
		if err := putRuntimeRun(tx, record); err != nil {
			return err
		}
		if options.Key != "" {
			data, _ := json.Marshal(admissionRecord{RunID: runID, Digest: digestText})
			return tx.Put(runtimeAdmissionBucket, options.Scope+"\x00"+options.Key, data)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	h := &RunHandle{runtime: r, runID: runID}
	if start && shouldStart {
		r.start(runID)
	}
	return h, nil
}

func newRuntimeID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}

// Run is [Runtime.Submit] followed by [RunHandle.Await]. Canceling ctx stops
// the wait, not the run: the returned result carries the run ID, so the
// caller can reattach with [Runtime.Handle].
func (r *Runtime) Run(ctx context.Context, definition DefinitionRef, task string, opts ...SubmitOption) (RunResult, error) {
	h, err := r.Submit(ctx, definition, task, opts...)
	if err != nil {
		return RunResult{}, err
	}
	return h.Await(ctx)
}

// RunStream is [Runtime.Run] with a live view: onEvent receives the run's
// provisional events (text and thinking deltas, tool calls and results,
// usage), including those of its child runs tagged with [StreamEvent].Agent
// and InvocationID. Calls to onEvent are serialized. The view is bounded and
// drops events rather than slowing the run; committed facts stay replayable
// through [RunHandle.Events]. Canceling ctx detaches the view without
// canceling the run. A nil onEvent is a no-op.
func (r *Runtime) RunStream(ctx context.Context, definition DefinitionRef, task string, onEvent func(StreamEvent), opts ...SubmitOption) (RunResult, error) {
	h, err := r.submit(ctx, definition, task, opts, false)
	if err != nil {
		return RunResult{}, err
	}
	events, unsubscribe := r.subscribe(h.runID)
	defer unsubscribe()
	snapshot, err := h.Snapshot(ctx)
	if err != nil {
		// Admission has committed. Scheduling is owned by the Runtime and must
		// not depend on whether this caller can attach its view. Do not issue a
		// detached read here: the view context must bound this call.
		r.start(h.runID)
		return RunResult{RunID: h.runID}, err
	}
	snapshot.Result = resultWithRunID(snapshot.Result, h.runID)
	if snapshot.State == RuntimeTerminal || snapshot.State == RuntimeNeedsAttention {
		return h.Await(ctx)
	}
	r.start(h.runID)
	err = h.follow(ctx, events, onEvent)
	if err == nil {
		return h.Await(ctx)
	}
	if ctx.Err() != nil {
		// Return the last snapshot already obtained by this view. A detached
		// storage read could outlive ctx and can lose the admitted identity if
		// that read fails.
		return snapshot.Result, ctx.Err()
	}
	return snapshot.Result, err
}
