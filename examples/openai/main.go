// Command openai is a minimal tool-using agent on the automata framework,
// backed by the OpenAI Chat Completions API (or any OpenAI-compatible endpoint).
//
// Environment:
//
//	OPENAI_API_KEY   - API key (optional for local backends like Ollama)
//	OPENAI_BASE_URL  - API base URL (default https://api.openai.com/v1)
//	OPENAI_MODEL     - model id (default gpt-4o-mini)
//
//	go run ./examples/openai
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/openai"
)

type timeArgs struct {
	TZ string `json:"tz" desc:"IANA timezone, e.g. Asia/Tokyo"`
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	provider := openai.
		New(envOr("OPENAI_MODEL", "gpt-4o-mini"), envOr("OPENAI_BASE_URL", "https://api.openai.com/v1")).
		WithAPIKey(os.Getenv("OPENAI_API_KEY"))

	agent := core.New(provider).
		WithSystemPrompt("You are a concise assistant. Use tools when they help.")

	agent.RegisterTool(core.Func("current_time", "Get the current time in a timezone",
		func(_ context.Context, a timeArgs) (string, error) {
			loc := time.UTC
			if a.TZ != "" {
				if l, err := time.LoadLocation(a.TZ); err == nil {
					loc = l
				}
			}
			return time.Now().In(loc).Format(time.RFC1123), nil
		}))

	res, err := agent.RunStream(
		context.Background(),
		"What time is it right now in Tokyo, and how many hours ahead of UTC is that?",
		func(ev core.StreamEvent) {
			switch ev.Kind {
			case core.StreamText:
				fmt.Print(ev.Text)
			case core.StreamToolCall:
				fmt.Printf("\n\033[2m→ %s(%s)\033[0m\n", ev.ToolCall.Name, string(ev.ToolCall.Input))
			case core.StreamToolResult:
				fmt.Printf("\033[2m← %s\033[0m\n", ev.Result)
			}
		},
	)
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "run failed:", err)
		os.Exit(1)
	}
	fmt.Printf("\n[%d steps, %d output tokens]\n", res.Steps, res.Usage.OutputTokens)
}
