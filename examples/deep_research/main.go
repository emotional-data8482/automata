// Command deep_research is a CLI deep-research agent built on the automata
// framework. An orchestrator agent maintains a to-do list and delegates to a
// researcher (live Tavily web search) and a writer (which saves the report),
// all streamed live in a Bubble Tea terminal UI.
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := &todoStore{}
	sub := make(chan tea.Msg, 256)
	sink := func(msg tea.Msg) { sub <- msg }

	// Build the orchestrator once the topic is known (the save filename derives
	// from it), so it works for both the arg and interactive-prompt paths.
	build := func(topic string) *core.Agent {
		c := cfg
		c.topic = topic
		return buildOrchestrator(c, store, sink)
	}

	p := tea.NewProgram(newModel(cfg, sub, ctx, cancel, build), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
