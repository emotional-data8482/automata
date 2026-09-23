package main

import (
	"io"
	"log/slog"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/claude"
	"github.com/emotional-data8482/automata/extensions/tavily"
	"github.com/emotional-data8482/automata/tools"
)

// researchParams is the assignment the orchestrator fills when calling the
// researcher. Each call becomes a durable child run whose task is this value
// as JSON.
type researchParams struct {
	Topic     string   `json:"topic" desc:"the subtopic to research"`
	Questions []string `json:"questions,omitempty" desc:"specific questions the research should answer"`
}

// writeParams is the assignment for the writer child run.
type writeParams struct {
	Title   string   `json:"title" desc:"the report title"`
	Outline []string `json:"outline,omitempty" desc:"section headings in order"`
	Notes   string   `json:"notes" desc:"the combined research findings to write up, including source URLs"`
}

const researcherPrompt = `You are a meticulous research specialist. Each assignment arrives as a JSON object with a "topic" and, optionally, "questions" it should answer.

Use the web_search tool to gather current, credible information — issue multiple focused searches as needed rather than one broad query. Synthesize what you find into concise, well-organized notes that directly answer the questions. Always include the source URLs you relied on.

Return notes (bullet points are good), not polished prose — the writer will turn them into the final document.`

const writerPrompt = `You are a skilled technical writer. Each assignment arrives as a JSON object with a report "title", an optional "outline" of section headings, and research "notes".

Compose a clear, well-structured Markdown document: a single top-level "# Title", logical "## Section" headings (follow the outline when provided), tightened prose, and bullet lists where they help. End with a "## Sources" section listing the URLs found in the notes.

When the document is ready, call save_document with the full Markdown to write it to disk, then reply with only the saved file path. Do not add commentary outside the document.`

const orchestratorPrompt = `You orchestrate a deep-research project that produces a polished Markdown report. Work in phases:

1. Plan: break the user's topic into 3-6 concrete subtopics and record them with the "todo" tool (all unchecked).
2. Research: for each subtopic, call the "researcher" tool with the topic and focused questions. After each result, call "todo" again with the updated list, marking completed items done.
3. Write: once research is complete, call the "writer" tool with a title, an outline of sections, and the combined notes (including source URLs) from all the research. The writer saves the file and returns its path.
4. Finish: reply with a short summary and the saved file path.

Keep the to-do list accurate throughout. Delegate research and writing — do not do them yourself.`

// discardLogger returns a logger that drops everything. The agents run beneath
// a Bubble Tea TUI that owns the terminal, so slog's default stderr output
// would smear the display.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// registerAgents registers the researcher, the writer, and the orchestrator
// that delegates to them, and returns the orchestrator's reference. The
// researcher and writer are durable child definitions: each orchestrator call
// to them is admitted as its own run, linked to the call, and its streamed
// events reach the orchestrator's live view tagged with the tool name.
func registerAgents(runtime *core.Runtime, cfg appConfig, store *todoStore, sink func(tea.Msg)) (core.DefinitionRef, error) {
	researcher, err := core.New(claude.New(cfg.researchModel, cfg.apiKey), core.AgentConfig{
		Logger: discardLogger(), SystemPrompt: researcherPrompt, MaxTurns: 10,
		Tools: []core.Tool{tools.WebSearch(tavily.New(cfg.tavilyKey))},
	})
	if err != nil {
		return core.DefinitionRef{}, err
	}
	researcherRef, err := runtime.Register("researcher", "v1", researcher)
	if err != nil {
		return core.DefinitionRef{}, err
	}

	writer, err := core.New(claude.New(cfg.writerModel, cfg.apiKey), core.AgentConfig{
		Logger: discardLogger(), SystemPrompt: writerPrompt, MaxTurns: 5,
		Tools: []core.Tool{saveDocumentTool(cfg.topic, cfg.outPath, sink)},
	})
	if err != nil {
		return core.DefinitionRef{}, err
	}
	writerRef, err := runtime.Register("writer", "v1", writer)
	if err != nil {
		return core.DefinitionRef{}, err
	}

	orchestrator, err := core.New(claude.New(cfg.orchestratorModel, cfg.apiKey), core.AgentConfig{
		Logger: discardLogger(), SystemPrompt: orchestratorPrompt, MaxTurns: 30,
		// The call cap is a subtree cap: it bounds the orchestrator's calls
		// and every call its children make.
		ToolPolicy: core.ToolPolicy{MaxCalls: 80},
		Tools: []core.Tool{
			todoTool(store, sink),
			core.ChildTool[researchParams]("researcher",
				"Delegate a focused research assignment. Provide a topic and optional specific questions; returns concise notes with source URLs.",
				researcherRef),
			core.ChildTool[writeParams]("writer",
				"Delegate writing the final report. Provide a title, an outline, and the combined research notes; it composes the Markdown, saves the file, and returns the saved path.",
				writerRef),
		},
	})
	if err != nil {
		return core.DefinitionRef{}, err
	}
	return runtime.Register("orchestrator", "v1", orchestrator)
}
