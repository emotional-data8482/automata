package tavily

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchMapsResults(t *testing.T) {
	var got searchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %q, want /search", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("Authorization = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]string{
				{"title": "A", "url": "https://a.example", "content": "alpha"},
				{"title": "B", "url": "https://b.example", "content": "beta"},
			},
		})
	}))
	defer srv.Close()

	c := New("test-key")
	c.BaseURL = srv.URL

	results, err := c.Search(context.Background(), "golang agents", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if got.Query != "golang agents" || got.MaxResults != 2 || got.SearchDepth != "basic" {
		t.Errorf("request = %+v", got)
	}
	if got.IncludeAnswer {
		t.Error("include_answer was requested; the Searcher contract is results-only")
	}
	if len(results) != 2 || results[0].Title != "A" || results[1].URL != "https://b.example" {
		t.Errorf("results = %+v", results)
	}
}

func TestSearchSurfacesAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New("bad-key")
	c.BaseURL = srv.URL

	if _, err := c.Search(context.Background(), "x", 1); err == nil {
		t.Fatal("expected error on 401")
	}
}
