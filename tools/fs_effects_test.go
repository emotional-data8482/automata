package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

// Failures before the file is written change nothing, so they must not park
// the run as uncertain (bug review X2).
func TestWriteFilePreWriteFailuresAreNotApplied(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"dot-dot":           "../escape.md",
		"absolute":          filepath.Join(parent, "abs.md"),
		"parent is a file":  "file.txt/child.md",
		"escaping symlink":  "outside/evil.md",
		"directory as file": ".",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"path": path, "content": "x"})
			result, err := WriteFile(root).Execute(context.Background(), args)
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || result.Effect.Status != core.EffectNotApplied {
				t.Fatalf("result = %#v, want a not-applied error", result)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(parent, "evil.md")); err == nil {
		t.Fatal("symlink write escaped the root")
	}
}

type scriptedProvider struct{ turns []core.Message }

func (p *scriptedProvider) Invoke(context.Context, core.Request) (core.Response, error) {
	if len(p.turns) == 0 {
		return core.Response{}, fmt.Errorf("no script left")
	}
	m := p.turns[0]
	p.turns = p.turns[1:]
	stop := core.StopEndTurn
	if len(m.ToolUses()) > 0 {
		stop = core.StopToolUse
	}
	return core.Response{Message: m, StopReason: stop}, nil
}

// A file the agent wrote can be revised by a later run; only an identical
// retry of the same content is deduplicated (bug review X1).
func TestWriteFileGuardAllowsRevisions(t *testing.T) {
	root := t.TempDir()
	runtime, err := core.NewRuntime(context.Background(), core.RuntimeConfig{Store: core.NewMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	write := func(id, content string) []core.Message {
		input, _ := json.Marshal(map[string]string{"path": "notes.md", "content": content})
		return []core.Message{
			core.AssistantMessage(core.ToolUseBlock{ID: id, Name: "write_file", Input: input}),
			core.AssistantMessage(core.TextBlock{Text: "done"}),
		}
	}
	var turns []core.Message
	turns = append(turns, write("w1", "v1")...)
	turns = append(turns, write("w2", "v2")...)
	turns = append(turns, write("w3", "v2")...)
	agent, err := core.New(&scriptedProvider{turns: turns}, core.AgentConfig{Tools: []core.Tool{WriteFile(root)}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := runtime.Register("writer", "v1", agent)
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for range 3 {
		result, err := runtime.Run(context.Background(), ref, "write the notes")
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range result.Messages {
			for _, block := range message.Blocks {
				if tr, ok := block.(core.ToolResultBlock); ok {
					results = append(results, core.Message{Blocks: tr.Content}.Text())
				}
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "notes.md"))
	if err != nil || string(data) != "v2" {
		t.Fatalf("file = %q, %v; want the revision v2", data, err)
	}
	if len(results) != 3 || !strings.HasPrefix(results[0], "wrote") || !strings.HasPrefix(results[1], "wrote") || !strings.Contains(results[2], "denied") {
		t.Fatalf("results = %q; want two writes then a denied identical retry", results)
	}
}
