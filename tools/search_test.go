package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeSearcher records the query/max it was called with and returns canned
// results.
type fakeSearcher struct {
	results []Result
	err     error

	gotQuery string
	gotMax   int
}

func (f *fakeSearcher) Search(_ context.Context, query string, max int) ([]Result, error) {
	f.gotQuery, f.gotMax = query, max
	return f.results, f.err
}

func TestWebSearchFormatsResults(t *testing.T) {
	s := &fakeSearcher{results: []Result{
		{Title: "First", URL: "https://a.example", Content: "alpha  content"},
		{Title: "Second", URL: "https://b.example", Content: "beta content"},
	}}

	out, err := WebSearch(s).Execute(context.Background(), `{"query":"golang agents","max_results":3}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if s.gotQuery != "golang agents" || s.gotMax != 3 {
		t.Errorf("searcher called with (%q, %d), want (golang agents, 3)", s.gotQuery, s.gotMax)
	}
	for _, want := range []string{"1. First", "https://a.example", "2. Second", "alpha content"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWebSearchDefaults(t *testing.T) {
	s := &fakeSearcher{}
	out, err := WebSearch(s).Execute(context.Background(), `{"query":"x"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if s.gotMax != 5 {
		t.Errorf("default max = %d, want 5", s.gotMax)
	}
	if out != "No results found." {
		t.Errorf("empty results output = %q", out)
	}
}

func TestWebSearchErrors(t *testing.T) {
	if _, err := WebSearch(&fakeSearcher{}).Execute(context.Background(), `{"query":"  "}`); err == nil {
		t.Error("blank query: expected error")
	}
	boom := &fakeSearcher{err: errors.New("backend down")}
	if _, err := WebSearch(boom).Execute(context.Background(), `{"query":"x"}`); err == nil || !strings.Contains(err.Error(), "backend down") {
		t.Errorf("searcher error not propagated: %v", err)
	}
}

func TestWebSearchSchema(t *testing.T) {
	var schema struct {
		Name       string `json:"name"`
		Parameters struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(WebSearch(&fakeSearcher{}).Schema(), &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if schema.Name != "web_search" {
		t.Errorf("name = %q, want web_search", schema.Name)
	}
	if _, ok := schema.Parameters.Properties["max_results"]; !ok {
		t.Error("schema missing max_results")
	}
	if len(schema.Parameters.Required) != 1 || schema.Parameters.Required[0] != "query" {
		t.Errorf("required = %v, want [query]", schema.Parameters.Required)
	}
}
