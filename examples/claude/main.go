// Command claude-example runs a minimal tool-using agent against the
// Anthropic API.
//
// Set ANTHROPIC_API_KEY in the environment before running. The model defaults
// to claude-sonnet-4-6 and can be overridden with ANTHROPIC_MODEL.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/claude
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/emotional-data/automata/core"
	"github.com/emotional-data/automata/extensions/claude"
)

// currentTimeParams is the typed input for the current_time tool. The exported
// fields and their `json`/`desc` tags drive the JSON schema advertised to the
// model.
type currentTimeParams struct {
	Timezone string `json:"timezone" desc:"IANA timezone name, e.g. America/New_York or Asia/Tokyo. Empty for UTC."`
}

func main() {
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	provider := claude.New(model, os.Getenv("ANTHROPIC_API_KEY"))

	agent := core.New(provider).
		WithSystemPrompt("You are a concise assistant. Use the available tools when they help you answer precisely.").
		WithMaxSteps(5)

	agent.RegisterTool(core.Func("current_time",
		"Returns the current wall-clock time in the given IANA timezone.",
		func(_ context.Context, p currentTimeParams) (string, error) {
			loc := time.UTC
			if p.Timezone != "" {
				l, err := time.LoadLocation(p.Timezone)
				if err != nil {
					return "", fmt.Errorf("unknown timezone %q", p.Timezone)
				}
				loc = l
			}
			return time.Now().In(loc).Format(time.RFC1123), nil
		}))

	_, err := agent.RunStream(
		context.Background(),
		"What time is it right now in Tokyo, and how many hours ahead of UTC is that?",
		func(ev core.StreamEvent) {
			switch ev.Kind {
			case core.StreamText:
				fmt.Print(ev.Text)
			case core.StreamToolCall:
				fmt.Printf("\n\033[2m→ %s(%s)\033[0m\n", ev.ToolCall.Function.Name, ev.ToolCall.Function.Arguments)
			case core.StreamToolResult:
				if ev.Err != nil {
					fmt.Printf("\033[2m← error: %v\033[0m\n", ev.Err)
				} else {
					fmt.Printf("\033[2m← %s\033[0m\n", ev.Result)
				}
			}
		},
	)
	fmt.Println()
	if err != nil {
		log.Fatalf("run failed: %v", err)
	}
}
