package core

import "sync"

// ToolCallView is the accumulator's record of one tool call: the call as the
// model requested it, and — once the StreamToolResult arrives — its outcome.
type ToolCallView struct {
	Call    ToolUseBlock
	Result  string // the string fed back to the model
	IsError bool   // true if the tool failed
	Err     error  // non-nil if the tool returned an error
	Done    bool   // true once the result has arrived
}

// AgentView is a per-invocation snapshot of an in-progress (or finished)
// streaming run: the text streamed so far, every tool call with its result, and
// the summed token usage attributed to that invocation.
type AgentView struct {
	// Agent is the StreamEvent.Agent tag: "" for the top-level agent, the
	// sub-agent tool's name for events forwarded by [AsTool] / [AsToolFunc].
	Agent string
	// InvocationID is the StreamEvent.InvocationID tag: "" for the top-level
	// agent, the starting tool-call ID for a sub-agent invocation. It separates
	// two concurrent calls to the same sub-agent tool, which share an Agent.
	InvocationID string
	Text         string // assembled streamed text
	Thinking     string // assembled streamed thinking
	ToolCalls    []ToolCallView
	Usage        Usage // summed across this invocation's turns
}

// agentKey identifies one lane in the accumulator: an (Agent, InvocationID)
// pair. The top-level agent is the zero key.
type agentKey struct {
	agent        string
	invocationID string
}

// agentState is the accumulator's mutable per-invocation record; AgentView is
// the copied snapshot handed to callers.
type agentState struct {
	agent        string
	invocationID string
	text         []byte
	thinking     []byte
	toolCalls    []ToolCallView
	usage        Usage
}

// view copies the mutable state into an immutable snapshot.
func (st *agentState) view() AgentView {
	return AgentView{
		Agent:        st.agent,
		InvocationID: st.invocationID,
		Text:         string(st.text),
		Thinking:     string(st.thinking),
		ToolCalls:    append([]ToolCallView(nil), st.toolCalls...),
		Usage:        st.usage,
	}
}

// StreamAccumulator folds a [RunStream] event stream into structured per-agent
// state, so consumers (a TUI, a web handler, a log writer) can render a
// snapshot instead of hand-rolling delta bookkeeping. Feed every event to
// [StreamAccumulator.Add] and read [StreamAccumulator.Views] /
// [StreamAccumulator.Totals] whenever a render is due:
//
//	var acc core.StreamAccumulator
//	out, err := agent.RunStream(ctx, task, func(ev core.StreamEvent) {
//	    acc.Add(ev)
//	    render(acc.Views(), acc.Totals())
//	})
//
// The zero value is ready to use. All methods are safe for concurrent use, so
// a server can snapshot Views from another goroutine while the run streams.
type StreamAccumulator struct {
	mu     sync.Mutex
	agents []*agentState // first-seen order; the top-level lane forced to front
	byKey  map[agentKey]*agentState
	totals Usage
}

// Add folds one stream event into the accumulator. Events are grouped by
// (StreamEvent.Agent, StreamEvent.InvocationID); per lane, text and thinking
// deltas concatenate in arrival order, tool results are paired to their calls
// by ToolUseBlock.ID, and StreamUsage events are summed (per lane and into
// Totals).
func (a *StreamAccumulator) Add(ev StreamEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()

	st := a.agent(ev.Agent, ev.InvocationID)
	switch ev.Kind {
	case StreamText:
		st.text = append(st.text, ev.Text...)
	case StreamThinking:
		st.thinking = append(st.thinking, ev.Text...)
	case StreamToolCall:
		st.toolCalls = append(st.toolCalls, ToolCallView{Call: ev.ToolCall})
	case StreamToolResult:
		// Pair with the announced call. Search backwards so the most recent
		// matching pending call wins if IDs ever repeat.
		for i := len(st.toolCalls) - 1; i >= 0; i-- {
			tc := &st.toolCalls[i]
			if !tc.Done && tc.Call.ID == ev.ToolCall.ID {
				tc.Result = ev.Result
				tc.IsError = ev.IsError
				tc.Err = ev.Err
				tc.Done = true
				return
			}
		}
		// No announced call (e.g. the consumer attached mid-run): record the
		// result as an already-done call rather than dropping it.
		st.toolCalls = append(st.toolCalls, ToolCallView{
			Call: ev.ToolCall, Result: ev.Result, IsError: ev.IsError, Err: ev.Err, Done: true,
		})
	case StreamUsage:
		st.usage.Add(ev.Usage)
		a.totals.Add(ev.Usage)
	}
}

// agent returns the state record for the (name, invocationID) lane, creating it
// on first sight. The top-level agent (the zero key) is kept at the front of
// the order; sub-agent invocations follow in first-seen order. Callers must
// hold a.mu.
func (a *StreamAccumulator) agent(name, invocationID string) *agentState {
	key := agentKey{agent: name, invocationID: invocationID}
	if st, ok := a.byKey[key]; ok {
		return st
	}
	if a.byKey == nil {
		a.byKey = make(map[agentKey]*agentState)
	}
	st := &agentState{agent: name, invocationID: invocationID}
	a.byKey[key] = st
	if name == "" {
		a.agents = append([]*agentState{st}, a.agents...)
	} else {
		a.agents = append(a.agents, st)
	}
	return st
}

// Views returns a snapshot of every lane seen so far: the top-level agent
// first (if it has produced any events), then each sub-agent invocation in
// first-seen order. Parallel calls to the same sub-agent tool appear as
// separate views with the same Agent but distinct InvocationIDs. The returned
// views are copies — later Add calls do not mutate them.
func (a *StreamAccumulator) Views() []AgentView {
	a.mu.Lock()
	defer a.mu.Unlock()

	views := make([]AgentView, len(a.agents))
	for i, st := range a.agents {
		views[i] = st.view()
	}
	return views
}

// View returns the snapshot for one lane, identified by agent tag ("" for the
// top-level agent) and invocation ID ("" for the top-level agent), and whether
// that lane has produced any events yet.
func (a *StreamAccumulator) View(agent, invocationID string) (AgentView, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	st, ok := a.byKey[agentKey{agent: agent, invocationID: invocationID}]
	if !ok {
		return AgentView{}, false
	}
	return st.view(), true
}

// ViewsFor returns every invocation lane for a given agent tag, in first-seen
// order. Use it when a sub-agent tool may be called more than once (e.g.
// parallel sub-agents) and you want all lanes sharing that name. Pass "" for
// the top-level agent.
func (a *StreamAccumulator) ViewsFor(agent string) []AgentView {
	a.mu.Lock()
	defer a.mu.Unlock()

	var views []AgentView
	for _, st := range a.agents {
		if st.agent == agent {
			views = append(views, st.view())
		}
	}
	return views
}

// Totals returns the token usage summed across all agents and turns seen so
// far.
func (a *StreamAccumulator) Totals() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.totals
}
