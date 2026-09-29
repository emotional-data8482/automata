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

// runtimeEncodingVersion is the storage encoding this core build reads and
// writes. Version 5 added durable question and approval waits. Version 6 adds
// declared structured-output contracts: persisted correction-turn counts and
// the validated final structured payload inside the run result. Version 7 adds
// durable child runs, parent operation links, and internal child waits.
// Version 8 keys transcript chunks by their first message index and adds
// integrity digests to payloads, transcript base references for conversation
// turns, provider attempt records, the committed event log with its per-run
// heads, the active-run index, and the retention indexes and tombstones.
// Version 9 renames the max-steps stop reason and failure kinds to max-turns,
// persists approval decisions as strings, and records the tool name and call
// ID that admitted a child run. Earlier pre-release stores are rejected
// without implicit rewrite.
const runtimeEncodingVersion = 9

const (
	runtimeMetaBucket         = "runtime_meta"
	runtimeRunsBucket         = "runtime_runs"
	runtimeAdmissionBucket    = "runtime_admissions"
	runtimeFactsBucket        = "runtime_facts"
	runtimeReceiptsBucket     = "runtime_receipts"
	runtimeBatchesBucket      = "runtime_batches"
	runtimeInvocationsBucket  = "runtime_invocations"
	runtimeEffectGuardsBucket = "runtime_effect_guards"
	runtimeWaitsBucket        = "runtime_waits"
	runtimeChildLinksBucket   = "runtime_child_links"
)

const (
	// transitionProviderAttemptStarted commits before every provider call.
	// A running run whose last transition is still this one was stopped while
	// the provider may have been processing the request.
	transitionProviderAttemptStarted = "provider_attempt_started"
	// transitionProviderAttemptRecovered marks a run that recovery made ready
	// for a policy-authorized fresh attempt: no attempt is in flight.
	transitionProviderAttemptRecovered = "provider_attempt_recovered"
)

type storedRuntimeRun struct {
	Version            int          `json:"version"`
	RunID              string       `json:"run_id"`
	DefinitionID       string       `json:"definition_id"`
	DefinitionRevision string       `json:"definition_revision"`
	Task               string       `json:"task"`
	Deadline           time.Time    `json:"deadline,omitempty"`
	State              RuntimeState `json:"state"`
	// Generation is the run's state version. Every state transition
	// increments it, and generation-guarded writes (attention, stale-worker
	// checks) compare it inside their transaction. It is not a write
	// counter: budget charges and the attempt, hook-delivery, and payload
	// markers leave it unchanged, so every writer re-reads the record inside
	// its transaction rather than trusting a cached generation.
	Generation         uint64           `json:"generation"`
	Result             RunResult        `json:"result"`
	TranscriptChunks   int              `json:"transcript_chunks"`
	TranscriptMessages int              `json:"transcript_messages"`
	Error              string           `json:"error,omitempty"`
	ErrorKind          FailureKind      `json:"error_kind,omitempty"`
	ErrorStopReason    StopReason       `json:"error_stop_reason,omitempty"`
	ErrorRawReason     string           `json:"error_raw_reason,omitempty"`
	ErrorViolations    []string         `json:"error_violations,omitempty"`
	LastTransition     string           `json:"last_transition,omitempty"`
	EffectiveTools     []string         `json:"effective_tools,omitempty"`
	AttentionReason    string           `json:"attention_reason,omitempty"`
	AttentionKind      AttentionKind    `json:"attention_kind,omitempty"`
	HookResults        []RunHookResult  `json:"hook_results,omitempty"`
	PendingBatchID     string           `json:"pending_batch_id,omitempty"`
	NextBatchOrdinal   int              `json:"next_batch_ordinal,omitempty"`
	ToolBudget         storedToolBudget `json:"tool_budget"`
	// ParentRunID and ParentOperationID identify the parent run and durable
	// invocation that admitted this child. They are empty on ordinary runs.
	// ParentTool and ParentCallID name the parent's tool call; they tag the
	// child's provisional events in the parent's live views.
	ParentRunID       string `json:"parent_run_id,omitempty"`
	ParentOperationID string `json:"parent_operation_id,omitempty"`
	ParentTool        string `json:"parent_tool,omitempty"`
	ParentCallID      string `json:"parent_call_id,omitempty"`
	// ConversationScope and ConversationID name the durable conversation this
	// run is a turn of. Both are empty on ordinary runs.
	ConversationScope string `json:"conversation_scope,omitempty"`
	ConversationID    string `json:"conversation_id,omitempty"`
	// TranscriptBase names the terminal run whose committed transcript this
	// run continues (the conversation head it was admitted after), and
	// TranscriptBaseMessages is that transcript's length. The run's own chunks
	// start at that index; the base is referenced, never copied.
	TranscriptBase         string `json:"transcript_base,omitempty"`
	TranscriptBaseMessages int    `json:"transcript_base_messages,omitempty"`
	// HistoryPruned records that retention removed the transcript and tool
	// result payloads (see [RetentionPolicy]).
	HistoryPruned bool `json:"history_pruned,omitempty"`
	// PayloadError records why the run needs attention for a payload it could
	// not store or read. PayloadNeeded is the size a rejected payload needed,
	// zero when the payload is unavailable rather than too large. Recovery
	// resumes such a run only once MaxPayloadBytes covers PayloadNeeded.
	PayloadError  string `json:"payload_error,omitempty"`
	PayloadNeeded int    `json:"payload_needed,omitempty"`
	// Corrections is the cumulative structured-output correction-turn count of
	// the declared contract (see [StructuredOutputConfig]). It persists
	// atomically with the transition that re-dispatches the correction so a
	// restart cannot replay or extend the budget.
	Corrections int `json:"corrections,omitempty"`
	// UnknownAttempts is persisted unknown provider-attempt evidence (see
	// [RunAccounting.UnknownAttempts]). UnknownAttemptGeneration marks the interrupted
	// generation already counted so repeated recovery counts it once.
	// FreshAttempts counts fresh provider attempts recovery started in place
	// of interrupted ones (see [ProviderRecoveryPolicy]).
	// HookDelivery names the committed-run hooks whose delivery started. It
	// commits before the first hook is invoked: a finalizing run without it
	// provably delivered no hook, so recovery can finish it; with it, delivery
	// outcome is unknown and needs attention (see [RunHandle.AcknowledgeHooks]).
	HookDelivery             []string            `json:"hook_delivery,omitempty"`
	UnknownAttempts          int                 `json:"unknown_attempts,omitempty"`
	UnknownAttemptGeneration uint64              `json:"unknown_attempt_generation,omitempty"`
	FreshAttempts            int                 `json:"fresh_attempts,omitempty"`
	ToolBatches              []ToolBatchSnapshot `json:"-"`
	Waits                    []WaitSnapshot      `json:"-"`
	Tree                     TreeAccounting      `json:"-"`
	EventSequence            uint64              `json:"-"`
}

func (record storedRuntimeRun) definition() DefinitionRef {
	return DefinitionRef{ID: record.DefinitionID, Revision: record.DefinitionRevision}
}

func (r *Runtime) load(ctx context.Context, runID string) (storedRuntimeRun, error) {
	var record storedRuntimeRun
	err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		record, err = loadRuntimeRun(tx, runID)
		return err
	})
	return record, err
}

func getRuntimeRun(tx StoreTransaction, runID string) (storedRuntimeRun, error) {
	raw, err := tx.Get(runtimeRunsBucket, runID)
	if errors.Is(err, ErrStoreKeyNotFound) {
		return storedRuntimeRun{}, missingRunError(tx, runID)
	}
	if err != nil {
		return storedRuntimeRun{}, err
	}
	return decodeRuntimeRun(raw)
}

// transcriptFactKey keys a transcript chunk by the index of its first
// message, in fixed-width hexadecimal so ascending key order is transcript
// order and a reader can seek directly to the chunk a message range starts
// in.
func transcriptFactKey(runID string, firstMessage int) string {
	return fmt.Sprintf("%s/%016x", runID, firstMessage)
}

// appendTranscript persists the not-yet-stored suffix of an append-only run
// transcript as one fact chunk and updates the record's transcript counts. The
// run record itself stays compact: compact state changes never rewrite
// committed history.
func appendTranscript(tx StoreTransaction, runID string, record *storedRuntimeRun, messages []Message) error {
	if len(messages) < record.TranscriptMessages {
		return fmt.Errorf("run %s transcript shrank from %d to %d messages", runID, record.TranscriptMessages, len(messages))
	}
	return appendTranscriptDelta(tx, runID, record, messages[record.TranscriptMessages:])
}

// appendTranscriptDelta persists delta as one chunk starting at the record's
// current transcript length.
func appendTranscriptDelta(tx StoreTransaction, runID string, record *storedRuntimeRun, delta []Message) error {
	if len(delta) > 0 {
		data, err := encodeTranscriptChunk(delta)
		if err != nil {
			return fmt.Errorf("encode run %s transcript chunk: %w", runID, err)
		}
		if err := tx.Put(runtimeFactsBucket, transcriptFactKey(runID, record.TranscriptMessages), data); err != nil {
			return err
		}
		record.TranscriptChunks++
	}
	record.TranscriptMessages += len(delta)
	return nil
}

// loadRuntimeRun reads a run record and reassembles its transcript from the
// run's append-only fact chunks, following its transcript base chain.
func loadRuntimeRun(tx StoreTransaction, runID string) (storedRuntimeRun, error) {
	record, err := getRuntimeRun(tx, runID)
	if err != nil {
		return storedRuntimeRun{}, err
	}
	if record.TranscriptMessages > 0 && !record.HistoryPruned {
		messages, err := loadTranscript(tx, record)
		if err != nil {
			return storedRuntimeRun{}, err
		}
		record.Result.Messages = messages
	}
	batches, err := loadToolBatchSnapshots(tx, runID)
	if err != nil {
		return storedRuntimeRun{}, err
	}
	record.ToolBatches = batches
	waits, err := loadRunWaits(tx, runID)
	if err != nil {
		return storedRuntimeRun{}, err
	}
	for _, wait := range waits {
		record.Waits = append(record.Waits, waitSnapshot(wait))
	}
	return record, nil
}

// missingRunError distinguishes a run retention removed from one that never
// existed.
func missingRunError(tx StoreTransaction, runID string) error {
	if _, err := tx.Get(runtimePrunedBucket, runID); err == nil {
		return ErrRunPruned
	} else if !errors.Is(err, ErrStoreKeyNotFound) {
		return err
	}
	return ErrRunNotFound
}

// loadTranscript reassembles a run's committed transcript: the transcripts of
// its base chain, oldest first, then its own chunks.
func loadTranscript(tx StoreTransaction, record storedRuntimeRun) ([]Message, error) {
	chain := []storedRuntimeRun{record}
	for current := record; current.TranscriptBase != ""; {
		base, err := getRuntimeRun(tx, current.TranscriptBase)
		if errors.Is(err, ErrRunNotFound) {
			err = ErrPayloadUnavailable
		}
		if err != nil {
			return nil, fmt.Errorf("run %s transcript base %s: %w", current.RunID, current.TranscriptBase, err)
		}
		if base.TranscriptMessages != current.TranscriptBaseMessages {
			return nil, fmt.Errorf("%w: run %s transcript base %s has %d messages, want %d", ErrPayloadUnavailable, current.RunID, base.RunID, base.TranscriptMessages, current.TranscriptBaseMessages)
		}
		chain = append(chain, base)
		current = base
	}
	messages := make([]Message, 0, record.TranscriptMessages)
	for i := len(chain) - 1; i >= 0; i-- {
		run := chain[i]
		if err := tx.Scan(runtimeFactsBucket, run.RunID+"/", func(key string, raw []byte) error {
			if want := transcriptFactKey(run.RunID, len(messages)); key != want {
				return fmt.Errorf("%w: run %s transcript chunk %s follows message %d", ErrPayloadUnavailable, run.RunID, key, len(messages))
			}
			chunk, err := decodeTranscriptChunk(run.RunID, raw)
			if err != nil {
				return err
			}
			messages = append(messages, chunk...)
			return nil
		}); err != nil {
			return nil, err
		}
		if len(messages) != run.TranscriptMessages {
			return nil, fmt.Errorf("%w: run %s transcript has %d stored messages but its record expects %d", ErrPayloadUnavailable, run.RunID, len(messages), run.TranscriptMessages)
		}
	}
	return messages, nil
}

// storedTranscriptChunk is one append-only transcript fact. The digest covers
// the exact encoded messages, so a missing, truncated, or altered chunk is
// reported as ErrPayloadUnavailable instead of silently changing history.
type storedTranscriptChunk struct {
	Digest   string          `json:"sha256"`
	Messages json.RawMessage `json:"messages"`
}

func encodeTranscriptChunk(messages []Message) ([]byte, error) {
	data, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return json.Marshal(storedTranscriptChunk{Digest: hex.EncodeToString(sum[:]), Messages: data})
}

func decodeTranscriptChunk(runID string, raw []byte) ([]Message, error) {
	var chunk storedTranscriptChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return nil, fmt.Errorf("%w: run %s transcript chunk: %v", ErrPayloadUnavailable, runID, err)
	}
	sum := sha256.Sum256(chunk.Messages)
	if hex.EncodeToString(sum[:]) != chunk.Digest {
		return nil, fmt.Errorf("%w: run %s transcript chunk fails its integrity check", ErrPayloadUnavailable, runID)
	}
	var messages []Message
	if err := json.Unmarshal(chunk.Messages, &messages); err != nil {
		return nil, fmt.Errorf("%w: run %s transcript chunk: %v", ErrPayloadUnavailable, runID, err)
	}
	return messages, nil
}

func decodeRuntimeRun(raw []byte) (storedRuntimeRun, error) {
	var record storedRuntimeRun
	if err := json.Unmarshal(raw, &record); err != nil {
		return storedRuntimeRun{}, fmt.Errorf("decode runtime run: %w", err)
	}
	if record.Version != runtimeEncodingVersion {
		return storedRuntimeRun{}, fmt.Errorf("unsupported runtime run version %d", record.Version)
	}
	return record, nil
}

func putRuntimeRun(tx StoreTransaction, record storedRuntimeRun) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := tx.Put(runtimeRunsBucket, record.RunID, data); err != nil {
		return err
	}
	return syncRunIndexes(tx, record)
}
