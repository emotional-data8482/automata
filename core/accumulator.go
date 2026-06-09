package core

import "sync"

// ToolCallView is the accumulator's record of one tool call: the call as the
// model requested it, and — once the StreamToolResult arrives — its outcome.
type ToolCallView struct {
	Call   ToolCall
	Result string // the string fed back to the model
	Err    error  // non-nil if the tool returned an error
	Done   bool   // true once the result has arrived
}

// AgentView is a per-agent snapshot of an in-progress (or finished) streaming
// run: the text streamed so far, every tool call with its result, and the
// summed token usage attributed to that agent.
type AgentView struct {
	// Agent is the StreamEvent.Agent tag: "" for the top-level agent, the
	// sub-agent tool's name for events forwarded by [AsTool] / [AsToolFunc].
	Agent     string
	Text      string // assembled streamed text
	ToolCalls []ToolCallView
	Usage     Usage // summed across this agent's turns
}

// agentState is the accumulator's mutable per-agent record; AgentView is the
// copied snapshot handed to callers.
type agentState struct {
	agent     string
	text      []byte
	toolCalls []ToolCallView
	usage     Usage
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
	agents []*agentState // first-seen order; "" (top-level) forced to front
	byName map[string]*agentState
	totals Usage
}

// Add folds one stream event into the accumulator. Events are grouped by
// StreamEvent.Agent; per agent, text deltas concatenate in arrival order,
// tool results are paired to their calls by ToolCall.ID, and StreamUsage
// events are summed (per agent and into Totals).
func (a *StreamAccumulator) Add(ev StreamEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()

	st := a.agent(ev.Agent)
	switch ev.Kind {
	case StreamText:
		st.text = append(st.text, ev.Text...)
	case StreamToolCall:
		st.toolCalls = append(st.toolCalls, ToolCallView{Call: ev.ToolCall})
	case StreamToolResult:
		// Pair with the announced call. Search backwards so the most recent
		// matching pending call wins if IDs ever repeat.
		for i := len(st.toolCalls) - 1; i >= 0; i-- {
			tc := &st.toolCalls[i]
			if !tc.Done && tc.Call.ID == ev.ToolCall.ID {
				tc.Result = ev.Result
				tc.Err = ev.Err
				tc.Done = true
				return
			}
		}
		// No announced call (e.g. the consumer attached mid-run): record the
		// result as an already-done call rather than dropping it.
		st.toolCalls = append(st.toolCalls, ToolCallView{
			Call: ev.ToolCall, Result: ev.Result, Err: ev.Err, Done: true,
		})
	case StreamUsage:
		st.usage.Add(ev.Usage)
		a.totals.Add(ev.Usage)
	}
}

// agent returns the state record for the tag, creating it on first sight. The
// top-level agent ("") is kept at the front of the order; sub-agents follow in
// first-seen order. Callers must hold a.mu.
func (a *StreamAccumulator) agent(name string) *agentState {
	if st, ok := a.byName[name]; ok {
		return st
	}
	if a.byName == nil {
		a.byName = make(map[string]*agentState)
	}
	st := &agentState{agent: name}
	a.byName[name] = st
	if name == "" {
		a.agents = append([]*agentState{st}, a.agents...)
	} else {
		a.agents = append(a.agents, st)
	}
	return st
}

// Views returns a snapshot of every agent seen so far: the top-level agent
// first (if it has produced any events), then sub-agents in first-seen order.
// The returned views are copies — later Add calls do not mutate them.
func (a *StreamAccumulator) Views() []AgentView {
	a.mu.Lock()
	defer a.mu.Unlock()

	views := make([]AgentView, len(a.agents))
	for i, st := range a.agents {
		views[i] = AgentView{
			Agent:     st.agent,
			Text:      string(st.text),
			ToolCalls: append([]ToolCallView(nil), st.toolCalls...),
			Usage:     st.usage,
		}
	}
	return views
}

// View returns the snapshot for one agent tag ("" for the top-level agent)
// and whether that agent has produced any events yet.
func (a *StreamAccumulator) View(agent string) (AgentView, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	st, ok := a.byName[agent]
	if !ok {
		return AgentView{}, false
	}
	return AgentView{
		Agent:     st.agent,
		Text:      string(st.text),
		ToolCalls: append([]ToolCallView(nil), st.toolCalls...),
		Usage:     st.usage,
	}, true
}

// Totals returns the token usage summed across all agents and turns seen so
// far.
func (a *StreamAccumulator) Totals() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.totals
}
