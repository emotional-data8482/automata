package core

import (
	"context"
)

type liveRuntimeRun struct {
	cancel  context.CancelFunc
	restart bool
	// ancestors are the run's parent chain, nearest first, while its worker
	// forwards provisional events to their live views.
	ancestors []streamAncestor
}

// streamAncestor is one hop of a child run's parent chain: the ancestor run
// and the tool call through which the chain descends from it.
type streamAncestor struct {
	runID, tool, callID string
}

type runtimeStreamItem struct {
	event    StreamEvent
	terminal bool
}

// publish delivers a provisional event to the run's live views and, for a
// child run, to its ancestors' views. At each hop an untagged event is
// tagged with the tool name and call ID through which the chain descends, so
// the innermost tag wins.
func (r *Runtime) publish(runID string, event StreamEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deliver(runID, event)
	for _, hop := range r.live[runID].ancestors {
		if event.Agent == "" {
			event.Agent = hop.tool
		}
		if event.InvocationID == "" {
			event.InvocationID = hop.callID
		}
		r.deliver(hop.runID, event)
	}
}

func (r *Runtime) deliver(runID string, event StreamEvent) {
	for _, ch := range r.subscribers[runID] {
		select {
		case ch <- runtimeStreamItem{event: cloneStreamEvent(event)}:
		default:
		}
	}
}

// maxStreamAncestors bounds the parent chain a child's worker forwards
// provisional events through.
const maxStreamAncestors = 64

// attachAncestors records a child run's parent chain on its live worker so
// publish can forward provisional events to ancestor views. The chain comes
// from compact committed records, so it is rebuilt after a restart. A read
// failure only disables forwarding: live views are provisional.
func (r *Runtime) attachAncestors(ctx context.Context, record storedRuntimeRun) {
	if record.ParentRunID == "" {
		return
	}
	var ancestors []streamAncestor
	_ = r.transaction(ctx, false, func(tx StoreTransaction) error {
		current := record
		for current.ParentRunID != "" && len(ancestors) < maxStreamAncestors {
			ancestors = append(ancestors, streamAncestor{runID: current.ParentRunID, tool: current.ParentTool, callID: current.ParentCallID})
			parent, err := getRuntimeRun(tx, current.ParentRunID)
			if err != nil {
				return err
			}
			current = parent
		}
		return nil
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if live, ok := r.live[record.RunID]; ok {
		live.ancestors = ancestors
		r.live[record.RunID] = live
	}
}

func (r *Runtime) publishTerminal(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.subscribers[runID] {
		select {
		case ch <- runtimeStreamItem{terminal: true}:
		default:
			// Provisional events may be dropped, but completion must wake views.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- runtimeStreamItem{terminal: true}:
			default:
			}
		}
	}
}

func (r *Runtime) subscribe(runID string) (<-chan runtimeStreamItem, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextSubID++
	id := r.nextSubID
	ch := make(chan runtimeStreamItem, 64)
	if r.subscribers[runID] == nil {
		r.subscribers[runID] = make(map[uint64]chan runtimeStreamItem)
	}
	r.subscribers[runID][id] = ch
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if subscribers := r.subscribers[runID]; subscribers != nil {
			delete(subscribers, id)
			if len(subscribers) == 0 {
				delete(r.subscribers, runID)
			}
		}
	}
}
