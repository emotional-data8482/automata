package main

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/emotional-data/automata/core"
	"github.com/emotional-data/automata/extensions/claude"
	"github.com/emotional-data/automata/extensions/tavily"
	"github.com/emotional-data/automata/tools"
)

// researchParams is the assignment schema the orchestrator fills when calling
// the researcher; core.AsToolFunc renders it into a natural-language task.
type researchParams struct {
	Topic     string   `json:"topic" desc:"the subtopic to research"`
	Questions []string `json:"questions,omitempty" desc:"specific questions the research should answer"`
}

// writeParams is the assignment schema for the writer sub-agent.
type writeParams struct {
	Title   string   `json:"title" desc:"the report title"`
	Outline []string `json:"outline,omitempty" desc:"section headings in order"`
	Notes   string   `json:"notes" desc:"the combined research findings to write up, including source URLs"`
}

func renderResearchTask(p researchParams) string {
	var b strings.Builder
	b.WriteString("Research this topic: ")
	b.WriteString(p.Topic)
	if len(p.Questions) > 0 {
		b.WriteString("\n\nAnswer these questions:\n- ")
		b.WriteString(strings.Join(p.Questions, "\n- "))
	}
	return b.String()
}

func renderWriteTask(p writeParams) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write the final report, titled %q.\n", p.Title)
	if len(p.Outline) > 0 {
		b.WriteString("\nFollow this outline:\n- ")
		b.WriteString(strings.Join(p.Outline, "\n- "))
		b.WriteString("\n")
	}
	b.WriteString("\nResearch notes:\n")
	b.WriteString(p.Notes)
	return b.String()
}

const researcherPrompt = `You are a meticulous research specialist. Each assignment gives you a topic and the questions it should answer.

Use the web_search tool to gather current, credible information — issue multiple focused searches as needed rather than one broad query. Synthesize what you find into concise, well-organized notes that directly answer the questions. Always include the source URLs you relied on.

Return notes (bullet points are good), not polished prose — the writer will turn them into the final document.`

const writerPrompt = `You are a skilled technical writer. Each assignment gives you a report title, an optional outline, and research notes.

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

// buildResearcher builds the researcher sub-agent: web search only. The tool
// is the framework's vendor-neutral tools.WebSearch with Tavily as backend.
func buildResearcher(cfg appConfig) *core.Agent {
	a := core.New(claude.New(cfg.researchModel, cfg.apiKey)).
		WithLogger(discardLogger()).
		WithSystemPrompt(researcherPrompt).
		WithMaxSteps(10)
	a.RegisterTool(tools.WebSearch(tavily.New(cfg.tavilyKey)))
	return a
}

// buildWriter builds the writer sub-agent: it composes Markdown and saves it.
func buildWriter(cfg appConfig, sink func(tea.Msg)) *core.Agent {
	a := core.New(claude.New(cfg.writerModel, cfg.apiKey)).
		WithLogger(discardLogger()).
		WithSystemPrompt(writerPrompt).
		WithMaxSteps(5)
	a.RegisterTool(saveDocumentTool(cfg.topic, cfg.outPath, sink))
	return a
}

// buildOrchestrator builds the top-level agent: it owns the to-do list and
// delegates to the researcher and writer, which are registered as streaming
// sub-agent tools. core.AsToolFunc renders each typed assignment into natural
// language, so the sub-agent prompts carry no JSON-parsing boilerplate.
func buildOrchestrator(cfg appConfig, store *todoStore, sink func(tea.Msg)) *core.Agent {
	researcher := buildResearcher(cfg)
	writer := buildWriter(cfg, sink)

	a := core.New(claude.New(cfg.orchestratorModel, cfg.apiKey)).
		WithLogger(discardLogger()).
		WithSystemPrompt(orchestratorPrompt).
		WithMaxSteps(30)
	a.RegisterTool(todoTool(store, sink))
	a.RegisterTool(core.AsToolFunc(researcher, "researcher",
		"Delegate a focused research assignment. Provide a topic and optional specific questions; returns concise notes with source URLs.",
		renderResearchTask))
	a.RegisterTool(core.AsToolFunc(writer, "writer",
		"Delegate writing the final report. Provide a title, an outline, and the combined research notes; it composes the Markdown, saves the file, and returns the saved path.",
		renderWriteTask))
	return a
}
