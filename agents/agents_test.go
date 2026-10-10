package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/emotional-data8482/automata/agents"
	"github.com/emotional-data8482/automata/core"
)

type ReviewRequest struct {
	Artifact string `json:"artifact" desc:"artifact to review"`
}

type ReviewVerdict struct {
	Verdict string `json:"verdict"`
	Notes   string `json:"notes"`
}

// scripted replays fixed assistant messages and records each request's task:
// the first user message.
type scripted struct {
	mu    sync.Mutex
	turns []core.Message
	tasks []string
}

func (p *scripted) Invoke(_ context.Context, req core.Request) (core.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range req.Messages {
		if m.Role == "user" {
			p.tasks = append(p.tasks, m.Text())
			break
		}
	}
	if len(p.turns) == 0 {
		return core.Response{}, errors.New("script exhausted")
	}
	msg := p.turns[0]
	p.turns = p.turns[1:]
	reason := core.StopEndTurn
	if len(msg.ToolUses()) > 0 {
		reason = core.StopToolUse
	}
	return core.Response{Message: msg, StopReason: reason}, nil
}

func call(id, name, input string) core.Message {
	return core.AssistantMessage(core.ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(input)})
}

func text(s string) core.Message { return core.AssistantMessage(core.TextBlock{Text: s}) }

func verdict(id, payload string) core.Message { return call(id, "automata_structured_output", payload) }

func newRuntime(t *testing.T) *core.Runtime {
	t.Helper()
	rt, err := core.NewEphemeralRuntime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	return rt
}

func TestDefineRejectsInvalidContracts(t *testing.T) {
	rt := newRuntime(t)
	p := &scripted{}
	cases := []struct {
		name   string
		define func() error
		want   string
	}{
		{"explicit schema", func() error {
			_, err := agents.Define[ReviewRequest, ReviewVerdict](rt, agents.Spec{ID: "a", Revision: "v1", Provider: p,
				Config: core.AgentConfig{StructuredOutput: &core.StructuredOutputConfig{Schema: core.OutputSchema[ReviewVerdict]()}}})
			return err
		}, "derived from the output type"},
		{"string output with contract", func() error {
			_, err := agents.Define[ReviewRequest, string](rt, agents.Spec{ID: "b", Revision: "v1", Provider: p,
				Config: core.AgentConfig{StructuredOutput: &core.StructuredOutputConfig{}}})
			return err
		}, "string output cannot declare StructuredOutput"},
		{"non-struct input", func() error {
			_, err := agents.Define[string, ReviewVerdict](rt, agents.Spec{ID: "c", Revision: "v1", Provider: p})
			return err
		}, "input type string must be a struct"},
		{"non-struct output", func() error {
			_, err := agents.Define[ReviewRequest, int](rt, agents.Spec{ID: "d", Revision: "v1", Provider: p})
			return err
		}, "output type int must be a struct or string"},
		{"missing revision", func() error {
			_, err := agents.Define[ReviewRequest, ReviewVerdict](rt, agents.Spec{ID: "e", Provider: p})
			return err
		}, `register "e"`},
		{"nil provider", func() error {
			_, err := agents.Define[ReviewRequest, ReviewVerdict](rt, agents.Spec{ID: "f", Revision: "v1"})
			return err
		}, `define "f"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.define()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestRunEncodesTaskAndDecodesOutput(t *testing.T) {
	rt := newRuntime(t)
	p := &scripted{turns: []core.Message{verdict("v-1", `{"verdict":"approve","notes":"ok"}`)}}
	reviewer, err := agents.Define[ReviewRequest, ReviewVerdict](rt, agents.Spec{ID: "reviewer", Revision: "v1", Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	got, result, err := reviewer.Run(context.Background(), rt, ReviewRequest{Artifact: "report.md"})
	if err != nil {
		t.Fatal(err)
	}
	if got != (ReviewVerdict{Verdict: "approve", Notes: "ok"}) {
		t.Fatalf("output = %+v", got)
	}
	if result.RunID == "" {
		t.Fatal("result has no run ID")
	}
	if want := `{"artifact":"report.md"}`; p.tasks[0] != want {
		t.Fatalf("task = %q, want %q", p.tasks[0], want)
	}
}

func TestDefineDerivedContractAllowsOneCorrection(t *testing.T) {
	cases := []struct {
		name     string
		declared *core.StructuredOutputConfig
		wantErr  bool
	}{
		{"default budget", nil, false},
		{"declared zero budget kept", &core.StructuredOutputConfig{MaxCorrections: 0}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRuntime(t)
			p := &scripted{turns: []core.Message{
				verdict("v-1", `{"verdict":42,"notes":"wrong type"}`),
				verdict("v-2", `{"verdict":"approve","notes":"fixed"}`),
			}}
			reviewer, err := agents.Define[ReviewRequest, ReviewVerdict](rt, agents.Spec{ID: "reviewer", Revision: "v1", Provider: p,
				Config: core.AgentConfig{StructuredOutput: tc.declared}})
			if err != nil {
				t.Fatal(err)
			}
			got, result, err := reviewer.Run(context.Background(), rt, ReviewRequest{Artifact: "a.md"})
			if tc.wantErr {
				if !errors.Is(err, core.ErrInvalidStructuredOutput) {
					t.Fatalf("err = %v, want invalid structured output", err)
				}
				if result.Turns != 1 {
					t.Fatalf("partial result turns = %d, want 1", result.Turns)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Notes != "fixed" {
				t.Fatalf("output = %+v", got)
			}
		})
	}
}

func TestTextOutput(t *testing.T) {
	rt := newRuntime(t)
	p := &scripted{turns: []core.Message{text("notes with sources")}}
	researcher, err := agents.Define[ReviewRequest, string](rt, agents.Spec{ID: "researcher", Revision: "v1", Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := researcher.Run(context.Background(), rt, ReviewRequest{Artifact: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "notes with sources" {
		t.Fatalf("output = %q", got)
	}
}

// TestToolMatchesRun checks that a definition delegated to as a child tool
// receives the same task, and yields the same typed output, as a direct Run.
func TestToolMatchesRun(t *testing.T) {
	rt := newRuntime(t)
	in := ReviewRequest{Artifact: "report.md"}
	child := &scripted{turns: []core.Message{
		verdict("v-1", `{"verdict":"approve","notes":"direct"}`),
		verdict("v-2", `{"verdict":"approve","notes":"delegated"}`),
	}}
	reviewer, err := agents.Define[ReviewRequest, ReviewVerdict](rt, agents.Spec{ID: "reviewer", Revision: "v1", Provider: child})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reviewer.Run(context.Background(), rt, in); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(in)
	parent := &scripted{turns: []core.Message{call("r-1", "review", string(args)), text("done")}}
	lead, err := agents.Register(rt, agents.Spec{ID: "lead", Revision: "v1", Provider: parent,
		Config: core.AgentConfig{Tools: []core.Tool{reviewer.Tool("review", "Review one artifact.")}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := rt.Run(context.Background(), lead, "get it reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if child.tasks[0] != child.tasks[1] {
		t.Fatalf("direct task %q != delegated task %q", child.tasks[0], child.tasks[1])
	}
	snapshot, err := rt.Handle(result.RunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childRun, err := rt.Handle(snapshot.ToolBatches[0].Invocations[0].ChildRunID).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := reviewer.Output(childRun.Result)
	if err != nil {
		t.Fatal(err)
	}
	if got.Notes != "delegated" {
		t.Fatalf("child output = %+v", got)
	}
}
