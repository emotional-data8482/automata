package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/emotional-data/automata/core"
)

// Searcher is the pluggable backend behind [WebSearch]: the tool stays
// vendor-neutral while keyed providers (extensions/tavily, …) implement this
// interface in their own modules.
type Searcher interface {
	Search(ctx context.Context, query string, max int) ([]Result, error)
}

// Result is one web search hit.
type Result struct {
	Title   string
	URL     string
	Content string
}

type webSearchParams struct {
	Query      string `json:"query" desc:"the search query"`
	MaxResults int    `json:"max_results,omitempty" desc:"maximum number of results to return (default 5)"`
}

// WebSearch adapts any [Searcher] into a "web_search" tool. Results are
// formatted as a numbered list of title, URL and content snippet — sources the
// model can cite directly.
func WebSearch(s Searcher) core.Tool {
	return core.Func("web_search",
		"Search the web for current information. Returns a list of source results, each with a title, URL, and content snippet.",
		func(ctx context.Context, p webSearchParams) (string, error) {
			query := strings.TrimSpace(p.Query)
			if query == "" {
				return "", fmt.Errorf("query is required")
			}
			max := p.MaxResults
			if max <= 0 {
				max = 5
			}

			results, err := s.Search(ctx, query, max)
			if err != nil {
				return "", err
			}
			if len(results) == 0 {
				return "No results found.", nil
			}

			var b strings.Builder
			b.WriteString("Sources:\n")
			for i, r := range results {
				fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n",
					i+1, strings.TrimSpace(r.Title), r.URL, snippet(r.Content, 280))
			}
			return b.String(), nil
		})
}

// snippet collapses whitespace and truncates to n runes for compact display.
func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
