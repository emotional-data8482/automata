package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const runtimeConversationsBucket = "runtime_conversations"

var (
	ErrConversationNotFound = errors.New("conversation not found")
	// ErrConversationConflict rejects a turn whose expected head or pinned
	// definition no longer matches the committed conversation.
	ErrConversationConflict = errors.New("conversation head or definition does not match the submission")
	// ErrConversationBusy rejects a turn while another admitted turn of the
	// same conversation has not terminalized.
	ErrConversationBusy = errors.New("conversation has an active run")
	// ErrConversationBlocked rejects continuing a head whose committed history
	// is incomplete or whose tool or child work is unresolved.
	ErrConversationBlocked = errors.New("conversation history cannot be continued")
)

// ConversationOptions admits a Runtime run as the next turn of a durable
// conversation. Turns are serialized: a conversation has at most one active
// run, and each submission names the committed head it continues, so competing
// or stale admissions are rejected instead of merged.
type ConversationOptions struct {
	// Scope namespaces conversation IDs, for example by tenant. It may be
	// empty.
	Scope string
	// ID identifies the conversation within Scope. An empty ID submits an
	// ordinary run.
	ID string
	// ExpectedHead is the run ID of the committed turn this submission
	// continues, or empty to start the conversation.
	ExpectedHead string
}

// ConversationSnapshot is the committed state of a durable conversation.
type ConversationSnapshot struct {
	Scope              string
	ID                 string
	DefinitionID       string
	DefinitionRevision string
	// Head is the run ID of the last turn that terminalized; empty until the
	// first turn does.
	Head string
	// ActiveRunID is the admitted turn that has not terminalized; empty when
	// the conversation accepts a new turn.
	ActiveRunID string
	// Turns counts terminalized turns.
	Turns int
}

type storedConversation struct {
	Version            int    `json:"version"`
	Scope              string `json:"scope"`
	ID                 string `json:"id"`
	DefinitionID       string `json:"definition_id"`
	DefinitionRevision string `json:"definition_revision"`
	Head               string `json:"head,omitempty"`
	ActiveRunID        string `json:"active_run_id,omitempty"`
	Turns              int    `json:"turns,omitempty"`
}

func conversationKey(scope, id string) string { return scope + "\x00" + id }

func validateConversationOptions(options ConversationOptions) error {
	if options.ID == "" && (options.Scope != "" || options.ExpectedHead != "") {
		return errors.New("conversation id is required")
	}
	if strings.ContainsRune(options.Scope, '\x00') || strings.ContainsRune(options.ID, '\x00') || strings.ContainsRune(options.ExpectedHead, '\x00') {
		return errors.New("conversation identities cannot contain NUL")
	}
	return nil
}

func getConversation(tx StoreTransaction, key string) (storedConversation, error) {
	raw, err := tx.Get(runtimeConversationsBucket, key)
	if errors.Is(err, ErrStoreKeyNotFound) {
		return storedConversation{}, ErrConversationNotFound
	}
	if err != nil {
		return storedConversation{}, err
	}
	var conversation storedConversation
	if err := json.Unmarshal(raw, &conversation); err != nil {
		return storedConversation{}, fmt.Errorf("decode conversation: %w", err)
	}
	if conversation.Version != runtimeEncodingVersion {
		return storedConversation{}, fmt.Errorf("unsupported conversation version %d", conversation.Version)
	}
	return conversation, nil
}

// admitConversationTurnTx reserves the conversation's single active slot for
// runID and returns the head whose committed transcript the turn continues.
// It returns ok false when there is no committed history, and the turn then
// starts like any new run. The caller resolves an exact idempotent retry
// first, so a head that moved after a lost acknowledgement never rejects the
// original admission.
func admitConversationTurnTx(tx StoreTransaction, options ConversationOptions, definitionID, revision, task, runID string) (head storedRuntimeRun, ok bool, err error) {
	key := conversationKey(options.Scope, options.ID)
	conversation, err := getConversation(tx, key)
	switch {
	case errors.Is(err, ErrConversationNotFound):
		if options.ExpectedHead != "" {
			return head, false, fmt.Errorf("%w: conversation %q has no committed turn %s", ErrConversationConflict, options.ID, options.ExpectedHead)
		}
		conversation = storedConversation{
			Version: runtimeEncodingVersion, Scope: options.Scope, ID: options.ID,
			DefinitionID: definitionID, DefinitionRevision: revision,
		}
	case err != nil:
		return head, false, err
	default:
		if conversation.DefinitionID != definitionID || conversation.DefinitionRevision != revision {
			return head, false, fmt.Errorf("%w: conversation %q is pinned to %s@%s", ErrConversationConflict, options.ID, conversation.DefinitionID, conversation.DefinitionRevision)
		}
		if conversation.ActiveRunID != "" {
			return head, false, fmt.Errorf("%w: run %s", ErrConversationBusy, conversation.ActiveRunID)
		}
		if conversation.Head != options.ExpectedHead {
			return head, false, fmt.Errorf("%w: head is %q, not %q", ErrConversationConflict, conversation.Head, options.ExpectedHead)
		}
	}
	if conversation.Head != "" {
		if head, ok, err = continuationHead(tx, conversation.Head, task); err != nil {
			return head, false, err
		}
	}
	conversation.ActiveRunID = runID
	return head, ok, putStoredJSON(tx, runtimeConversationsBucket, key, conversation)
}

// continuationHead validates that the terminal head's committed transcript
// followed by task is a continuable history and returns the head. It never
// fabricates results: a head whose history is structurally incomplete, or
// whose subtree retains uncertain effects or unsettled descendants, blocks
// the conversation. ok is false when the head has no committed history.
func continuationHead(tx StoreTransaction, headRunID, task string) (storedRuntimeRun, bool, error) {
	head, err := loadRuntimeRun(tx, headRunID)
	if err != nil {
		return head, false, fmt.Errorf("conversation head %s: %w", headRunID, err)
	}
	if head.State != RuntimeTerminal {
		return head, false, fmt.Errorf("%w: head run %s is %s", ErrConversationBlocked, head.RunID, head.State)
	}
	unresolved, err := childHasUnresolvedEvidence(tx, head)
	if err != nil {
		return head, false, err
	}
	if unresolved {
		return head, false, fmt.Errorf("%w: head run %s retains unresolved effects or descendants", ErrConversationBlocked, head.RunID)
	}
	if len(head.Result.Messages) == 0 {
		return head, false, nil
	}
	if err := validateHistory(append(head.Result.Messages, UserMessage(task))); err != nil {
		return head, false, fmt.Errorf("%w: head run %s: %v", ErrConversationBlocked, head.RunID, err)
	}
	return head, true, nil
}

// releaseConversationTx advances the conversation head to a terminal turn and
// releases its active slot, inside the transaction that commits the run's
// terminal state.
func releaseConversationTx(tx StoreTransaction, record storedRuntimeRun) error {
	if record.ConversationID == "" {
		return nil
	}
	key := conversationKey(record.ConversationScope, record.ConversationID)
	conversation, err := getConversation(tx, key)
	if err != nil {
		return err
	}
	if conversation.ActiveRunID != record.RunID {
		return nil
	}
	conversation.Head = record.RunID
	conversation.ActiveRunID = ""
	conversation.Turns++
	return putStoredJSON(tx, runtimeConversationsBucket, key, conversation)
}

// Conversation returns the committed state of a durable conversation.
func (r *Runtime) Conversation(ctx context.Context, scope, id string) (ConversationSnapshot, error) {
	var conversation storedConversation
	err := r.transaction(ctx, false, func(tx StoreTransaction) error {
		var err error
		conversation, err = getConversation(tx, conversationKey(scope, id))
		return err
	})
	if err != nil {
		return ConversationSnapshot{}, err
	}
	return ConversationSnapshot{
		Scope: conversation.Scope, ID: conversation.ID,
		DefinitionID: conversation.DefinitionID, DefinitionRevision: conversation.DefinitionRevision,
		Head: conversation.Head, ActiveRunID: conversation.ActiveRunID, Turns: conversation.Turns,
	}, nil
}
