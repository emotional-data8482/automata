// Command typed_agents demonstrates automata's two typed seams:
//
//  1. Sub-agents as typed tools. An Agent becomes a tool on another agent, with
//     the JSON schema the parent's model fills in derived from a Go struct —
//     either with core.AsToolFunc (typed in, prose out) or by wrapping
//     core.RunTyped in a core.Func so the child's answer is validated too
//     (typed in, typed out). See agents.go.
//
//  2. Typed sessions. core.RunSessionTyped decodes each turn of a multi-turn
//     Session into a validated Go struct, so an orchestrator's decisions are
//     data rather than prose — and the conversation (sub-agent calls included)
//     persists to JSON and resumes between turns.
//
// The scenario is a support desk. An orchestrator triages one ticket by
// delegating to a librarian sub-agent (knowledge base) and a diagnostician
// sub-agent (service status), returns a validated Triage struct, is
// checkpointed to disk and resumed from that file, then drafts the customer
// reply as a second typed turn — a different result type on the same
// conversation.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/typed_agents
//	go run ./examples/typed_agents -native "Exports have been stuck since 09:00 UTC"
//
// The model defaults to claude-sonnet-4-6 and can be overridden with
// ANTHROPIC_MODEL.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/claude"
)

// Triage is the result type of the first typed turn. Fields without
// `omitempty` are required: the model is shown this schema, and a payload that
// omits Severity or sends a number for Summary is sent back for correction
// rather than returned as a half-filled struct.
type Triage struct {
	Summary         string   `json:"summary" desc:"One-sentence restatement of the customer's problem."`
	Severity        string   `json:"severity" desc:"One of: sev1, sev2, sev3 — using the definitions from the knowledge base."`
	Component       string   `json:"component" desc:"The component at fault."`
	OwningTeam      string   `json:"owning_team" desc:"Team that should own the fix, e.g. ingest, platform, billing."`
	RootCause       string   `json:"root_cause" desc:"The most likely cause, grounded in the diagnostician's evidence."`
	KBArticles      []string `json:"kb_articles" desc:"IDs of the knowledge-base articles used, e.g. KB-104."`
	NextSteps       []string `json:"next_steps" desc:"Ordered remediation steps for the owning team."`
	NeedsEscalation bool     `json:"needs_escalation" desc:"True when on-call must be paged now."`
	Caveat          string   `json:"caveat,omitempty" desc:"Optional: anything the triage could not confirm."`
}

// CustomerReply is the result type of the second typed turn on the same
// session. A session is not tied to one result type — each turn decodes into
// whatever T that decision needs.
type CustomerReply struct {
	Subject       string `json:"subject" desc:"Email subject line."`
	Body          string `json:"body" desc:"The full reply, plain text, no placeholders left unfilled."`
	FollowUpHours int    `json:"follow_up_hours" desc:"How many hours until we proactively follow up."`
	InternalNote  string `json:"internal_note,omitempty" desc:"Optional: context for the next agent on the thread."`
}

const defaultTicket = `Subject: exports stuck all morning

We schedule a CSV export of yesterday's events every morning at 09:00 UTC. Today
all three of our exports have been sitting at "preparing" for over two hours and
nothing has downloaded. Nobody on my team changed anything. The dashboard loads
but the numbers look stale. Is our data safe?

— Priya, Data Platform lead at Meridian Retail (enterprise plan)`

func main() {
	native := flag.Bool("native", false, "ask the provider to enforce the output schema natively (falls back to the hidden tool when unsupported)")
	statePath := flag.String("state", filepath.Join(os.TempDir(), "automata-triage-session.json"), "file the session transcript is checkpointed to")
	flag.Parse()

	ticket := defaultTicket
	if args := flag.Args(); len(args) > 0 {
		ticket = strings.Join(args, " ")
	}

	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	meter := &usageMeter{}
	orchestrator, err := newOrchestrator(claude.New(model, os.Getenv("ANTHROPIC_API_KEY")), meter, *native)

	if err != nil {
		exitf("construct agents: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Run options shared by both typed turns. WithMaxCorrectionTurns bounds how
	// many turns are spent feeding validation errors back to the model; the
	// post-run hook checkpoints the transcript after each underlying run,
	// including failures.
	opts := []core.RunOption{
		core.WithMaxCorrectionTurns(2),
		core.WithCheckpointHook(checkpointTo(*statePath)),
	}
	if *native {
		opts = append(opts, core.WithNativeStructuredOutput())
	}

	// --- Turn 1: triage --------------------------------------------------
	session := orchestrator.NewSession()

	fmt.Println("── triaging ticket ──")
	seen := len(session.Messages())
	triage, res, err := core.RunSessionTyped[Triage](ctx, session,
		"Triage this ticket:\n\n"+ticket, opts...)
	if err != nil {
		fail("triage", res, err)
	}
	printToolCalls(res, seen)
	printTriage(triage)
	printRun("triage", res, meter)

	// --- Persist and resume ----------------------------------------------
	// The transcript is plain data. Every block round-trips through JSON —
	// text, thinking with its signature, the sub-agent tool calls and their
	// results — so reloading it into a fresh Session is what another process
	// (a queue worker picking the ticket back up) would do.
	transcript, err := loadTranscript(*statePath)
	if err != nil {
		exitf("reload session: %v", err)
	}
	session, err = orchestrator.ResumeSession(transcript)
	if err != nil {
		exitf("resume session: %v", err)
	}
	fmt.Printf("\n\033[2mcheckpointed %d messages to %s and resumed from it\033[0m\n",
		len(transcript), *statePath)

	// --- Turn 2: customer reply, same conversation, different result type --
	fmt.Println("\n── drafting reply ──")
	seen = len(session.Messages())
	reply, res, err := core.RunSessionTyped[CustomerReply](ctx, session,
		"Draft the reply we send to Priya. Use the triage you just produced — the "+
			"sub-agents' findings are already in this conversation, so only call them "+
			"again if something is genuinely missing.", opts...)
	if err != nil {
		fail("reply", res, err)
	}
	printToolCalls(res, seen)
	printReply(reply)
	printRun("reply", res, meter)

	fmt.Printf("\n\033[2mconversation is now %d messages; sub-agent usage: %s\033[0m\n",
		len(session.Messages()), meter.report())
}

// checkpointTo writes each changed canonical transcript boundary to path,
// including tool turns, typed corrections, fallback, and partial failures. The hook's context keeps the
// run's values but drops its deadline, so a checkpoint still completes when the
// run was canceled; a real store should impose its own timeout.
func checkpointTo(path string) core.CheckpointHook {
	return func(_ context.Context, res core.Checkpoint, _ error) error {
		blob, err := json.MarshalIndent(res.Messages, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, blob, 0o600)
	}
}

func loadTranscript(path string) ([]core.Message, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var transcript []core.Message
	if err := json.Unmarshal(blob, &transcript); err != nil {
		return nil, err
	}
	return transcript, nil
}

// fail reports a typed-run failure. A payload that could not be made valid
// arrives as *core.InvalidStructuredOutputError with the per-field violations,
// and the RunResult is still populated as far as the run got.
func fail(label string, res core.RunResult, err error) {
	var invalid *core.InvalidStructuredOutputError
	if errors.As(err, &invalid) {
		fmt.Fprintf(os.Stderr, "%s: model never produced a valid payload after %d steps:\n", label, res.Steps)
		for _, v := range invalid.Violations {
			fmt.Fprintf(os.Stderr, "  - %s\n", v)
		}
		if invalid.Cause != nil {
			fmt.Fprintf(os.Stderr, "  cause: %v\n", invalid.Cause)
		}
		os.Exit(1)
	}
	exitf("%s failed after %d steps (stop=%s): %v", label, res.Steps, res.StopReason, err)
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// printToolCalls shows which sub-agents the orchestrator chose this turn.
// RunResult.Messages is the whole conversation, so from skips what earlier
// turns already reported. The transcript is block-based, so the tool calls are
// structured data, not text to parse.
func printToolCalls(res core.RunResult, from int) {
	if from > len(res.Messages) {
		from = len(res.Messages)
	}
	for _, m := range res.Messages[from:] {
		for _, call := range m.ToolUses() {
			fmt.Printf("\033[2m→ %s(%s)\033[0m\n", call.Name, truncate(string(call.Input), 140))
		}
	}
}

func printTriage(t Triage) {
	fmt.Printf("\nseverity:   %s (%s, owner: %s)\n", t.Severity, t.Component, t.OwningTeam)
	fmt.Printf("summary:    %s\n", t.Summary)
	fmt.Printf("root cause: %s\n", t.RootCause)
	fmt.Printf("articles:   %s\n", strings.Join(t.KBArticles, ", "))
	fmt.Printf("escalate:   %t\n", t.NeedsEscalation)
	fmt.Println("next steps:")
	for i, step := range t.NextSteps {
		fmt.Printf("  %d. %s\n", i+1, step)
	}
	if t.Caveat != "" {
		fmt.Printf("caveat:     %s\n", t.Caveat)
	}
}

func printReply(r CustomerReply) {
	fmt.Printf("\nsubject:   %s\nfollow-up: %dh\n\n%s\n", r.Subject, r.FollowUpHours, r.Body)
	if r.InternalNote != "" {
		fmt.Printf("\n\033[2minternal: %s\033[0m\n", r.InternalNote)
	}
}

// printRun reports the orchestrator's own usage. Sub-agent tokens are not
// included in RunResult.Usage — the meter carries those. When a typed run needs
// a correction turn, the RunResult is the final underlying run's, so Steps
// counts that run rather than the whole typed call.
func printRun(label string, res core.RunResult, meter *usageMeter) {
	fmt.Printf("\n\033[2m%s: %d steps, stop=%s, orchestrator %d in / %d out tokens; sub-agents: %s\033[0m\n",
		label, res.Steps, res.StopReason, res.Usage.InputTokens, res.Usage.OutputTokens, meter.report())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
