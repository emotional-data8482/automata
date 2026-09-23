// Command openai is a minimal tool-using agent on the automata framework,
// backed by the OpenAI Chat Completions API (or any OpenAI-compatible endpoint).
// The run is ephemeral: it executes through a core.Runtime on an in-memory
// store, which is lost on exit.
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

	agent, err := core.New(provider, core.AgentConfig{SystemPrompt: "You are a concise assistant. Use tools when they help.", Tools: []core.Tool{core.Func("current_time", "Get the current time in a timezone",
		func(_ context.Context, a timeArgs) (string, error) {
			loc := time.UTC
			if a.TZ != "" {
				if l, err := time.LoadLocation(a.TZ); err == nil {
					loc = l
				}
			}
			return time.Now().In(loc).Format(time.RFC1123), nil
		})}})

	if err != nil {
		fmt.Fprintln(os.Stderr, "configure agent:", err)
		os.Exit(1)
	}

	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open runtime:", err)
		os.Exit(1)
	}
	defer runtime.Close()
	assistant, err := runtime.Register("assistant", "v1", agent)
	if err != nil {
		fmt.Fprintln(os.Stderr, "register agent:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := runtime.RunStream(ctx, assistant,
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
		fmt.Fprintf(os.Stderr, "run failed after %d turns (partial output %q): %v\n", res.Turns, res.Output, err)
		os.Exit(1)
	}
	fmt.Printf("\n[%d turns, %d output tokens]\n", res.Turns, res.Usage.OutputTokens)
}
