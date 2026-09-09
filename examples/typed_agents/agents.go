package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// ---------------------------------------------------------------------------
// Typed tool schemas
//
// Every struct below is a schema. Exported fields become JSON-schema
// properties (name from the `json` tag, documentation from the `desc` tag), and
// a field without `omitempty` is required. The same reflection drives
// core.Func, core.AsTool/AsToolFunc, and the typed run's hidden
// structured-output tool, so one struct definition is enough for all of them.
// ---------------------------------------------------------------------------

// librarianQuery is what the orchestrator's model fills in to call the
// librarian sub-agent.
type librarianQuery struct {
	Problem  string   `json:"problem" desc:"The customer's problem in one sentence."`
	Keywords []string `json:"keywords" desc:"Two to five lowercase search keywords."`
}

// diagnosisRequest is what the orchestrator's model fills in to call the
// diagnostician sub-agent.
type diagnosisRequest struct {
	Component string `json:"component" desc:"Component to investigate: ingest-api, exports, billing, or dashboard."`
	Symptoms  string `json:"symptoms" desc:"What the customer observes, in one or two sentences."`
	SinceUTC  string `json:"since_utc,omitempty" desc:"Optional: when the symptoms started, e.g. 09:00 UTC."`
}

// Diagnosis is the diagnostician's answer. Unlike a prose sub-agent reply, this
// struct is validated against its schema before the orchestrator ever sees it:
// the sub-agent runs through core.RunTyped[Diagnosis] inside typedAgentTool.
type Diagnosis struct {
	Component      string   `json:"component" desc:"The component investigated."`
	Status         string   `json:"status" desc:"The status string returned verbatim by the service_status tool."`
	RootCause      string   `json:"root_cause" desc:"The most likely cause, in one or two sentences."`
	Confidence     string   `json:"confidence" desc:"One of: high, medium, low."`
	Evidence       []string `json:"evidence" desc:"Concrete observations that support the root cause."`
	SuggestedFix   string   `json:"suggested_fix" desc:"The remediation the owning team should apply."`
	CustomerImpact string   `json:"customer_impact,omitempty" desc:"Optional: who else is affected."`
}

// ---------------------------------------------------------------------------
// Sub-agents
// ---------------------------------------------------------------------------

// newLibrarian builds the knowledge-base sub-agent. Its prompt says nothing
// about JSON: the orchestrator's typed arguments are rendered into plain
// English by the AsToolFunc renderer in newOrchestrator.
func newLibrarian(p core.Provider) (*core.Agent, error) {
	return core.New(p, core.AgentConfig{SystemPrompt: `You are a support knowledge-base librarian.

Search the knowledge base with the kb_search tool, then answer in at most six
lines. Always cite the article IDs you used (e.g. KB-104). If nothing relevant
exists, say so plainly instead of guessing.`, MaxTurns: 6, ToolPolicy: core.ToolPolicy{Timeout: 15 * time.Second, MaxCalls: 6}, Tools: []core.Tool{kbSearchTool()}})
}

// newDiagnostician builds the service-health sub-agent. It answers through
// core.RunTyped, so its system prompt describes the investigation, not the
// output format — the schema derived from Diagnosis is what constrains the
// answer.
func newDiagnostician(p core.Provider) (*core.Agent, error) {
	return core.New(p, core.AgentConfig{SystemPrompt: `You are a site-reliability engineer on the incident desk.

Check the named component with the service_status tool before concluding
anything, and check its dependencies when the status mentions them. Base every
claim on tool output; never invent metrics. Report low confidence rather than
overstating a cause you could not confirm.`, MaxTurns: 8, ToolPolicy: core.ToolPolicy{Timeout: 15 * time.Second, MaxCalls: 8}, Tools: []core.Tool{serviceStatusTool()}})
}

// ---------------------------------------------------------------------------
// Sub-agents as typed tools
// ---------------------------------------------------------------------------

// newOrchestrator wires both sub-agents onto the triage agent as tools. A
// sub-agent is not a special kind of object here — it is an ordinary Agent
// adapted to the Tool interface, so the orchestrator's model picks it the same
// way it picks kb_search.
func newOrchestrator(p core.Provider, meter *usageMeter, native bool) (*core.Agent, error) {
	librarian, err := newLibrarian(p)
	if err != nil {
		return nil, err
	}
	diagnostician, err := newDiagnostician(p)
	if err != nil {
		return nil, err
	}

	// Options for the diagnostician's own typed run. WithNativeStructuredOutput
	// asks the provider to enforce the schema itself (Anthropic
	// output_config.format, OpenAI response_format); providers without native
	// support fall back to the hidden tool, so it is safe to set
	// unconditionally.
	childOpts := []core.RunOption{core.WithMaxCorrectionTurns(1)}
	if native {
		childOpts = append(childOpts, core.WithNativeStructuredOutput())
	}

	return core.New(p, core.AgentConfig{SystemPrompt: orchestratorPrompt, MaxTurns: 12, ToolPolicy: core.ToolPolicy{
		// A timeout here bounds a complete sub-agent run, not just one of
		// its turns.
		Timeout: 90 * time.Second,
		// Total call budgets are shared atomically with nested sub-agent
		// runs, so a chatty child cannot outspend the parent's budget.
		MaxCalls:    12,
		MaxParallel: 2,
	}, Tools: []core.Tool{

		// Pattern 1 — typed in, prose out. AsToolFunc advertises the
		// schema derived from librarianQuery and renders the decoded
		// params into the child's task string. (core.AsTool[P] is the
		// zero-config variant: it forwards the raw JSON arguments as the
		// task instead.)
		core.AsToolFunc[librarianQuery](librarian, "librarian",
			"Search the internal knowledge base for prior write-ups about a customer problem.",
			func(q librarianQuery) string {
				return fmt.Sprintf("Customer problem: %s\n\nSearch the knowledge base for: %s",
					q.Problem, strings.Join(q.Keywords, ", "))
			}),

		// Pattern 2 — typed in, typed out. The child's answer is decoded
		// and validated as a Diagnosis before the orchestrator sees it.
		typedAgentTool[diagnosisRequest, Diagnosis](diagnostician, "diagnostician",
			"Diagnose a failing component against live service status. Returns a JSON diagnosis.",
			meter,
			func(r diagnosisRequest) string {
				task := fmt.Sprintf("Investigate the %s component.\nSymptoms: %s", r.Component, r.Symptoms)
				if r.SinceUTC != "" {
					task += "\nStarted at: " + r.SinceUTC
				}
				return task
			},
			childOpts...)}},
	)
}

// typedAgentTool adapts an agent into a tool whose input *and* output are
// typed. P drives the JSON schema the parent's model fills in (exactly like
// core.AsTool); render turns the decoded params into the child's task; and the
// child answers through core.RunTyped[R], so a reply that omits a required
// field or mistypes one is corrected by the child before it reaches the parent,
// or surfaces as a tool error. The parent receives R as JSON.
//
// Reach for this when the parent must consume the child's answer as data —
// numbers to compare, an enum to branch on, a list to iterate. When prose is
// fine, core.AsToolFunc is the one-liner; see the librarian above.
func typedAgentTool[P, R any](
	a *core.Agent,
	name, description string,
	meter *usageMeter,
	render func(P) string,
	opts ...core.RunOption,
) core.Tool {
	return core.Func(name, description, func(ctx context.Context, params P) (string, error) {
		// ctx carries the parent's tool-call budget and deadline, so the child
		// run stays inside the parent's ToolPolicy.
		out, res, err := core.RunTyped[R](ctx, a, render(params), opts...)
		meter.add(name, res.Usage) // populated even when the run failed
		if err != nil {
			// An ordinary tool error: the parent's model sees "error: …" and
			// can retry with different arguments or route around the failure.
			// Only context cancellation aborts the whole run.
			return "", fmt.Errorf("%s: %w", name, err)
		}
		blob, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("encode %s result: %w", name, err)
		}
		return string(blob), nil
	})
}

// ---------------------------------------------------------------------------
// Leaf tools — plain functions the sub-agents call
// ---------------------------------------------------------------------------

type kbSearchArgs struct {
	Keywords []string `json:"keywords" desc:"Keywords matched case-insensitively against article titles and bodies."`
}

func kbSearchTool() core.Tool {
	return core.Func("kb_search",
		"Search the internal support knowledge base. Returns matching article IDs with their text.",
		func(_ context.Context, a kbSearchArgs) (string, error) {
			var hits []string
			for _, art := range knowledgeBase {
				haystack := strings.ToLower(art.Title + " " + art.Body)
				for _, kw := range a.Keywords {
					if kw != "" && strings.Contains(haystack, strings.ToLower(kw)) {
						hits = append(hits, fmt.Sprintf("%s — %s\n%s", art.ID, art.Title, art.Body))
						break
					}
				}
			}
			if len(hits) == 0 {
				return "no matching articles", nil
			}
			return strings.Join(hits, "\n\n"), nil
		})
}

type serviceStatusArgs struct {
	Component string `json:"component" desc:"Component name: ingest-api, exports, billing, or dashboard."`
}

func serviceStatusTool() core.Tool {
	return core.Func("service_status",
		"Return the current health of one service component.",
		func(_ context.Context, a serviceStatusArgs) (string, error) {
			status, ok := componentStatus[strings.ToLower(strings.TrimSpace(a.Component))]
			if !ok {
				// Returned to the model as "error: …", which it can recover
				// from by calling again with a valid component.
				return "", fmt.Errorf("unknown component %q; known components: ingest-api, exports, billing, dashboard", a.Component)
			}
			return status, nil
		})
}

// ---------------------------------------------------------------------------
// Stand-in data for the systems a real desk would call
// ---------------------------------------------------------------------------

type kbArticle struct{ ID, Title, Body string }

var knowledgeBase = []kbArticle{
	{
		ID:    "KB-104",
		Title: "Scheduled exports stall when ingest lags",
		Body:  "Exports read from the ingest watermark. When ingest-api falls behind, export jobs stay queued rather than failing, and customers see 'preparing' indefinitely. Resolution: drain the ingest backlog; exports self-recover within one scheduling interval.",
	},
	{
		ID:    "KB-118",
		Title: "Ingest-api 5xx spikes after schema changes",
		Body:  "A tenant pushing a changed event schema can trip the validation cache and drive 5xx rates up. Mitigation: invalidate the tenant schema cache, then roll the ingest-api pods.",
	},
	{
		ID:    "KB-131",
		Title: "Sev definitions",
		Body:  "sev1: data loss or full outage across tenants. sev2: a core workflow degraded for one or more tenants with no workaround. sev3: degraded non-core workflow or a documented workaround exists.",
	},
	{
		ID:    "KB-142",
		Title: "Billing invoice rounding",
		Body:  "Invoice totals round per line item, not per invoice. Differences under one cent are expected and are not a defect.",
	},
}

var componentStatus = map[string]string{
	"ingest-api": "DEGRADED since 08:41 UTC — p99 write latency 12s (normal 300ms), 4% 5xx, backlog 1.2M events. Upstream of: exports, dashboard.",
	"exports":    "HEALTHY — job runner idle, queue depth 340 jobs waiting on the ingest watermark.",
	"billing":    "HEALTHY — no incidents in the last 7 days.",
	"dashboard":  "HEALTHY — serving cached aggregates up to 22 minutes stale while ingest is behind.",
}

const orchestratorPrompt = `You run the support triage desk for Northwind Analytics.

You are given one customer ticket. Before you answer:
  - call the librarian for prior write-ups and the severity definitions, and
  - call the diagnostician for the health of any component you suspect.

Delegate rather than speculate: you have no direct access to the knowledge base
or to service status. Cite the article IDs the librarian returns, and ground
your root cause in the diagnostician's evidence. When you have enough to be
useful, stop calling tools and return your final answer as structured data.`

// usageMeter totals token usage per agent. RunResult.Usage covers only the run
// it came from — sub-agent usage is deliberately not folded into the parent's —
// so the typed tool wrapper records each child run here. (The other way to see
// child usage is RunStream: sub-agent events arrive tagged with the tool name
// and carry StreamUsage. See docs/streaming.md.)
type usageMeter struct {
	mu      sync.Mutex
	byAgent map[string]core.Usage
}

func (m *usageMeter) add(agent string, u core.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byAgent == nil {
		m.byAgent = map[string]core.Usage{}
	}
	total := m.byAgent[agent]
	total.Add(&u)
	m.byAgent[agent] = total
}

func (m *usageMeter) report() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.byAgent) == 0 {
		return "none recorded"
	}
	parts := make([]string, 0, len(m.byAgent))
	for agent, u := range m.byAgent {
		parts = append(parts, fmt.Sprintf("%s %d in / %d out", agent, u.InputTokens, u.OutputTokens))
	}
	return strings.Join(parts, ", ")
}
