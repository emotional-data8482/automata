// Package tavily implements [tools.Searcher] against the Tavily search API
// (https://tavily.com), so a researcher agent can use the vendor-neutral
// tools.WebSearch tool with Tavily as the backend:
//
//	agent.RegisterTool(tools.WebSearch(tavily.New(apiKey)))
package tavily

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/emotional-data/automata/tools"
)

const defaultBaseURL = "https://api.tavily.com"

// Client calls the Tavily search API. The zero value is not usable — construct
// it with [New], then override the exported fields if needed.
type Client struct {
	APIKey string
	// BaseURL overrides the API endpoint (useful in tests). Defaults to
	// https://api.tavily.com.
	BaseURL string
	// HTTPClient overrides the HTTP client. Defaults to a 30s-timeout client.
	HTTPClient *http.Client
	// Depth is the Tavily search_depth: "basic" (the default, cheapest) or
	// "advanced".
	Depth string
}

var _ tools.Searcher = (*Client)(nil)

// New returns a Client with the default endpoint, a 30-second timeout, and
// "basic" search depth.
func New(apiKey string) *Client {
	return &Client{APIKey: apiKey}
}

type searchRequest struct {
	APIKey        string `json:"api_key"`
	Query         string `json:"query"`
	MaxResults    int    `json:"max_results"`
	SearchDepth   string `json:"search_depth"`
	IncludeAnswer bool   `json:"include_answer"`
}

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type searchResponse struct {
	Results []searchResult `json:"results"`
}

// Search implements [tools.Searcher]. It never requests Tavily's synthesized
// answer — the Searcher contract is raw results; synthesis is the agent's job.
func (c *Client) Search(ctx context.Context, query string, max int) ([]tools.Result, error) {
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	depth := c.Depth
	if depth == "" {
		depth = "basic"
	}

	body, _ := json.Marshal(searchRequest{
		APIKey:        c.APIKey,
		Query:         query,
		MaxResults:    max,
		SearchDepth:   depth,
		IncludeAnswer: false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snip, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("tavily returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snip)))
	}

	var out searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode tavily response: %w", err)
	}

	results := make([]tools.Result, len(out.Results))
	for i, r := range out.Results {
		results[i] = tools.Result{Title: r.Title, URL: r.URL, Content: r.Content}
	}
	return results, nil
}
