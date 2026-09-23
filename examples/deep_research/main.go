// Command deep_research is a CLI deep-research agent built on the automata
// framework. An orchestrator agent maintains a to-do list and delegates to a
// researcher (live Tavily web search) and a writer (which saves the report).
// Each delegation is a durable child run; the whole tree streams live into a
// Bubble Tea terminal UI. The runtime is ephemeral, so an interrupted
// research run is not resumed; see examples/durable_host for a persistent
// store and recovery.
//
// Required environment:
//
//	ANTHROPIC_API_KEY  - Anthropic API key
//	TAVILY_API_KEY     - Tavily search API key
//
// Optional model overrides: ORCHESTRATOR_MODEL, RESEARCH_MODEL, WRITER_MODEL.
//
//	go run ./examples/deep_research "the impact of GLP-1 drugs on US healthcare costs"
//	go run ./examples/deep_research            # prompts for a topic
//	go run ./examples/deep_research -o out.md "some topic"
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/joho/godotenv"

	"github.com/emotional-data8482/automata/core"
)

type appConfig struct {
	apiKey            string
	tavilyKey         string
	orchestratorModel string
	researchModel     string
	writerModel       string
	topic             string
	outPath           string
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func configure() (appConfig, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return appConfig{}, errors.New("ANTHROPIC_API_KEY is required")
	}
	tavilyKey := os.Getenv("TAVILY_API_KEY")
	if tavilyKey == "" {
		return appConfig{}, errors.New("TAVILY_API_KEY is required")
	}
	return appConfig{
		apiKey:            apiKey,
		tavilyKey:         tavilyKey,
		orchestratorModel: envOr("ORCHESTRATOR_MODEL", "claude-opus-4-8"),
		researchModel:     envOr("RESEARCH_MODEL", "claude-haiku-4-5"),
		writerModel:       envOr("WRITER_MODEL", "claude-sonnet-4-6"),
	}, nil
}

func main() {
	out := flag.String("o", "", "output file path (default ./<slug>-<timestamp>.md)")
	flag.Parse()

	// Optional: load a local .env. Real environment variables already set take
	// precedence and suffice on their own, so a missing file is not an error.
	_ = godotenv.Load()

	cfg, err := configure()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	cfg.outPath = *out
	cfg.topic = strings.TrimSpace(strings.Join(flag.Args(), " "))

	// A whole research session is bounded; the TUI can also cancel it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	// Closing the runtime stops any child run still working when the TUI exits.
	defer runtime.Close()

	store := &todoStore{}
	sub := make(chan tea.Msg, 256)
	sink := func(msg tea.Msg) { sub <- msg }

	// Register the agents once the topic is known (the save filename derives
	// from it), so it works for both the arg and interactive-prompt paths.
	start := func(ctx context.Context, topic string, onEvent func(core.StreamEvent)) (core.RunResult, error) {
		c := cfg
		c.topic = topic
		orchestrator, err := registerAgents(runtime, c, store, sink)
		if err != nil {
			return core.RunResult{}, err
		}
		return runtime.RunStream(ctx, orchestrator, topic, onEvent, core.WithDeadline(time.Now().Add(25*time.Minute)))
	}

	p := tea.NewProgram(newModel(cfg, sub, ctx, cancel, start), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
