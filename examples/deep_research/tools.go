package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/emotional-data8482/automata/core"
)

// --- agent → TUI bridge messages produced by tools ------------------------

// todoMsg is pushed whenever the to-do list changes so the TUI can redraw it.
type todoMsg struct{ items []todoItem }

// savedMsg is pushed when the writer saves the report to disk.
type savedMsg struct{ path string }

// The researcher's web_search tool lives in the framework now: see
// tools.WebSearch (vendor-neutral) + extensions/tavily (the backend) wired up
// in buildResearcher.

// --- todo list ------------------------------------------------------------

type todoEntry struct {
	Text string `json:"text" desc:"the to-do item text"`
	Done bool   `json:"done,omitempty" desc:"whether this item is complete"`
}

type todoParams struct {
	Items []todoEntry `json:"items" desc:"the full current to-do list, in order"`
}

// todoItem is the rendered form the TUI displays.
type todoItem struct {
	Text string
	Done bool
}

// todoStore holds the current to-do list, guarded for the concurrent tool
// goroutines the loop may spawn.
type todoStore struct {
	mu    sync.Mutex
	items []todoItem
}

func (s *todoStore) set(entries []todoEntry) []todoItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = make([]todoItem, len(entries))
	for i, e := range entries {
		s.items[i] = todoItem{Text: e.Text, Done: e.Done}
	}
	return append([]todoItem(nil), s.items...)
}

// todoTool lets the orchestrator maintain its plan. The model passes the whole
// list each call (no IDs to track), which is the most robust pattern for an LLM.
func todoTool(store *todoStore, sink func(tea.Msg)) core.Tool {
	return core.Func("todo",
		"Record or update the research to-do list. Pass the FULL list each time; it replaces the previous one. Mark items done as you complete them.",
		func(_ context.Context, p todoParams) (string, error) {
			items := store.set(p.Items)
			sink(todoMsg{items: items})
			done := 0
			for _, it := range items {
				if it.Done {
					done++
				}
			}
			return fmt.Sprintf("to-do list updated: %d items, %d done", len(items), done), nil
		})
}

// --- save_document --------------------------------------------------------

type saveDocParams struct {
	Filename string `json:"filename,omitempty" desc:"optional filename (no directory); defaults to a slug of the topic"`
	Content  string `json:"content" desc:"the full Markdown document to save"`
}

// saveDocumentTool writes the final report to disk. The writer agent owns it:
// it composes the Markdown, saves it, and returns the path. If outPath is set
// (the -o flag) it always wins; otherwise the file lands in the working
// directory under the model's filename or a topic slug + timestamp.
func saveDocumentTool(topic, outPath string, sink func(tea.Msg)) core.Tool {
	return core.Func("save_document",
		"Save the final Markdown report to disk and return the absolute path it was written to.",
		func(_ context.Context, p saveDocParams) (string, error) {
			if strings.TrimSpace(p.Content) == "" {
				return "", fmt.Errorf("content is required")
			}

			path := outPath
			if path == "" {
				name := sanitizeFilename(p.Filename)
				if name == "" {
					name = slug(topic) + "-" + time.Now().Format("20060102-150405") + ".md"
				} else if !strings.HasSuffix(strings.ToLower(name), ".md") {
					name += ".md"
				}
				path = name
			}

			if err := os.WriteFile(path, []byte(p.Content), 0o644); err != nil {
				return "", fmt.Errorf("write %s: %w", path, err)
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				abs = path
			}
			sink(savedMsg{path: abs})
			return abs, nil
		})
}

// --- helpers --------------------------------------------------------------

var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

// slug turns a topic into a filesystem-friendly stem.
func slug(s string) string {
	s = nonSlugChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "report"
	}
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

// sanitizeFilename strips any directory components and spaces from a
// model-proposed filename. Returns "" for an empty input.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return strings.ReplaceAll(filepath.Base(name), " ", "-")
}

// snippet collapses whitespace and truncates to n runes for compact display.
func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n]) + "…"
	}
	return s
}
