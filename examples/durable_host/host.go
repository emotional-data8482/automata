package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/sqlite"
)

// host is one short-lived process: it owns the store until it exits.
type host struct {
	dir       string
	runtime   *core.Runtime
	publisher core.DefinitionRef
}

// ticket is the host's own record of an external work item. A platform would
// keep this in its database; the runtime only needs the admission identity.
type ticket struct {
	RunID  string `json:"run_id"`
	Task   string `json:"task"`
	Cursor uint64 `json:"cursor,omitempty"`
}

// openHost opens the store, registers the definitions every stored run may
// pin, and recovers work a previous process left.
func openHost(ctx context.Context, dir string) (*host, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, filepath.Join(dir, "runtime.db"))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{
		Store: store,
		// Authentication happens in the host; this check runs when an
		// approval is accepted and again just before dispatch.
		Authorizer: core.ApprovalAuthorizerFunc(func(_ context.Context, check core.ApprovalAuthorization) error {
			if check.Actor != "ops@example.com" {
				return fmt.Errorf("%q may not approve %s", check.Actor, check.Target)
			}
			return nil
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("open runtime: %w", err)
	}
	publisher, err := registerDefinitions(runtime, dir)
	if err != nil {
		runtime.Close()
		return nil, fmt.Errorf("register definitions: %w", err)
	}
	if err := runtime.Recover(ctx); err != nil {
		runtime.Close()
		return nil, fmt.Errorf("recover: %w", err)
	}
	return &host{dir: dir, runtime: runtime, publisher: publisher}, nil
}

func (h *host) Close() error { return h.runtime.Close() }

// submit admits the ticket's task. The ticket ID is the idempotency key, so
// submitting the same ticket again, even from another process after a lost
// response, returns the same run.
func (h *host) submit(ctx context.Context, out io.Writer, id, task string) error {
	handle, err := h.runtime.Submit(ctx, h.publisher, task, core.WithIdempotencyKey("tickets", id))
	if err != nil {
		return err
	}
	tickets, err := h.tickets()
	if err != nil {
		return err
	}
	existing, known := tickets[id]
	tickets[id] = ticket{RunID: handle.ID(), Task: task, Cursor: existing.Cursor}
	if err := h.saveTickets(tickets); err != nil {
		return err
	}
	if known && existing.RunID == handle.ID() {
		fmt.Fprintf(out, "ticket %s already admitted as run %s\n", id, handle.ID())
	} else {
		fmt.Fprintf(out, "ticket %s admitted as run %s\n", id, handle.ID())
	}
	state, err := h.waitForStop(ctx, handle)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "run is %s\n", state)
	if state == core.RuntimeWaiting {
		snapshot, err := handle.Snapshot(ctx)
		if err != nil {
			return err
		}
		for _, wait := range snapshot.Waits {
			if wait.Kind == core.WaitApproval && wait.State == core.WaitPending {
				fmt.Fprintf(out, "pending approval: %s\n", wait.Prompt)
			}
		}
	}
	return nil
}

// approve resolves the ticket's pending approval and waits for the run to
// settle. The run resumes in this process; nothing waited in between.
func (h *host) approve(ctx context.Context, out io.Writer, id string) error {
	handle, err := h.handle(id)
	if err != nil {
		return err
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		return err
	}
	var pending *core.WaitSnapshot
	for i, wait := range snapshot.Waits {
		if wait.Kind == core.WaitApproval && wait.State == core.WaitPending {
			pending = &snapshot.Waits[i]
		}
	}
	if pending == nil {
		return fmt.Errorf("ticket %s has no pending approval (run is %s)", id, snapshot.State)
	}
	err = handle.ResolveWait(ctx, pending.ID, core.WaitResolution{
		Decision:     core.Allow,
		Actor:        "ops@example.com",
		ActionDigest: pending.ActionDigest,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "approved %s\n", pending.Target)
	return h.report(ctx, out, handle)
}

// reconcile settles a publish whose outcome was lost in a crash. It asks the
// destination, never re-executes the tool, and resumes the run.
func (h *host) reconcile(ctx context.Context, out io.Writer, id string) error {
	handle, err := h.handle(id)
	if err != nil {
		return err
	}
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		return err
	}
	if snapshot.Attention != nil {
		fmt.Fprintf(out, "run needs attention (%s): %s\n", snapshot.Attention.Kind, snapshot.Attention.Reason)
	}
	reconciled := 0
	for _, batch := range snapshot.ToolBatches {
		for _, invocation := range batch.Invocations {
			if invocation.State != core.ToolInvocationUncertain {
				continue
			}
			var in PublishInput
			if err := json.Unmarshal(invocation.Call.Input, &in); err != nil {
				return err
			}
			// Durable tools receive the operation ID as their idempotency
			// key, so the destination can answer authoritatively.
			applied, err := ledgerHas(h.dir, invocation.OperationID)
			if err != nil {
				return err
			}
			resolution := core.EffectResolution{
				Result: core.ErrorResult("not published: the write never reached the destination"),
				Effect: core.EffectReport{Status: core.EffectNotApplied},
			}
			if applied {
				resolution = core.EffectResolution{
					Result: core.TextResult("published " + in.Path),
					Effect: core.EffectReport{Status: core.EffectApplied, Receipt: in.Path},
				}
			}
			if err := handle.Reconcile(ctx, invocation.OperationID, resolution); err != nil {
				return err
			}
			fmt.Fprintf(out, "reconciled %s as %s from the destination ledger\n", in.Path, resolution.Effect.Status)
			reconciled++
		}
	}
	if reconciled == 0 {
		return fmt.Errorf("ticket %s has no uncertain operation", id)
	}
	return h.report(ctx, out, handle)
}

// watch prints committed events after the ticket's saved cursor, then saves
// the new cursor, so each call resumes where the last one stopped, in any
// process.
func (h *host) watch(ctx context.Context, out io.Writer, id string) error {
	tickets, err := h.tickets()
	if err != nil {
		return err
	}
	t, ok := tickets[id]
	if !ok {
		return fmt.Errorf("unknown ticket %s", id)
	}
	handle := h.runtime.Handle(t.RunID)
	cursor := t.Cursor
	for {
		page, err := handle.Events(ctx, cursor, 64)
		if errors.Is(err, core.ErrEventGap) {
			// Retention removed events behind the cursor: resynchronize
			// from the authoritative snapshot.
			snapshot, err := handle.Snapshot(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "cursor %d is no longer retained; resynchronized at %d (run is %s)\n", cursor, snapshot.EventSequence, snapshot.State)
			cursor = snapshot.EventSequence
			continue
		}
		if err != nil {
			return err
		}
		for _, event := range page.Events {
			fmt.Fprintf(out, "  #%d %s\n", event.Sequence, describe(event))
		}
		cursor = page.Next
		if page.Next >= page.Head {
			break
		}
	}
	fmt.Fprintf(out, "cursor for %s is now %d\n", id, cursor)
	t.Cursor = cursor
	tickets[id] = t
	return h.saveTickets(tickets)
}

// result prints the settled run's answer, the reviewer child's typed verdict
// read from the child run, and the destination's write count.
func (h *host) result(ctx context.Context, out io.Writer, id string) error {
	handle, err := h.handle(id)
	if err != nil {
		return err
	}
	return h.report(ctx, out, handle)
}

func (h *host) report(ctx context.Context, out io.Writer, handle *core.RunHandle) error {
	result, err := handle.Await(ctx)
	if err != nil {
		return fmt.Errorf("run %s failed after %d turns (output %q): %w", handle.ID(), result.Turns, result.Output, err)
	}
	fmt.Fprintf(out, "output: %s\n", result.Output)
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		return err
	}
	for _, batch := range snapshot.ToolBatches {
		for _, invocation := range batch.Invocations {
			if invocation.ChildRunID == "" {
				continue
			}
			child, err := h.runtime.Handle(invocation.ChildRunID).Snapshot(ctx)
			if err != nil {
				return err
			}
			verdict, err := core.Decode[ReviewVerdict](child.Result)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "reviewer verdict: %s (%s) from child run %s\n", verdict.Verdict, verdict.Notes, child.RunID)
		}
	}
	fmt.Fprintf(out, "runs in tree: %d\n", snapshot.Accounting.Tree.Runs)
	entries, err := readLedger(h.dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "destination writes so far: %d\n", len(entries))
	return nil
}

// waitForStop follows committed events until the run waits, needs
// attention, or ends. It reads storage only when a commit arrives.
func (h *host) waitForStop(ctx context.Context, handle *core.RunHandle) (core.RuntimeState, error) {
	snapshot, err := handle.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	state, cursor := snapshot.State, snapshot.EventSequence
	for !stopped(state) {
		page, err := handle.WaitEvents(ctx, cursor, 64)
		if err != nil {
			return "", err
		}
		for _, event := range page.Events {
			if event.Kind == core.CommittedRunState {
				state = event.State
			}
		}
		cursor = page.Next
	}
	return state, nil
}

func stopped(state core.RuntimeState) bool {
	return state == core.RuntimeWaiting || state == core.RuntimeNeedsAttention || state == core.RuntimeTerminal
}

func (h *host) handle(id string) (*core.RunHandle, error) {
	tickets, err := h.tickets()
	if err != nil {
		return nil, err
	}
	t, ok := tickets[id]
	if !ok {
		return nil, fmt.Errorf("unknown ticket %s", id)
	}
	return h.runtime.Handle(t.RunID), nil
}

func (h *host) tickets() (map[string]ticket, error) {
	data, err := os.ReadFile(filepath.Join(h.dir, "tickets.json"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]ticket{}, nil
	}
	if err != nil {
		return nil, err
	}
	tickets := map[string]ticket{}
	return tickets, json.Unmarshal(data, &tickets)
}

func (h *host) saveTickets(tickets map[string]ticket) error {
	data, err := json.MarshalIndent(tickets, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.dir, "tickets.json"), data, 0o644)
}

func describe(event core.CommittedEvent) string {
	switch event.Kind {
	case core.CommittedRunState:
		if event.PreviousState == "" {
			return fmt.Sprintf("admitted (%s)", event.State)
		}
		if event.Reason != "" {
			return fmt.Sprintf("state %s → %s (%s)", event.PreviousState, event.State, event.Reason)
		}
		return fmt.Sprintf("state %s → %s", event.PreviousState, event.State)
	case core.CommittedMessages:
		return fmt.Sprintf("%d message(s) committed at %d", event.MessageCount, event.MessageIndex)
	case core.CommittedInvocation:
		if event.Effect != core.EffectUnreported {
			return fmt.Sprintf("%s %s (effect %s)", event.Tool, event.InvocationState, event.Effect)
		}
		return fmt.Sprintf("%s %s", event.Tool, event.InvocationState)
	case core.CommittedWait:
		return fmt.Sprintf("%s wait %s", event.WaitKind, event.WaitState)
	default:
		return string(event.Kind)
	}
}
