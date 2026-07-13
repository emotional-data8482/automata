// Package openai implements a [core.StreamProvider] against the OpenAI Chat
// Completions API using only the standard library, so it works against OpenAI
// and any OpenAI-compatible backend (Ollama, vLLM, OpenRouter, Azure, …) via a
// configurable base URL.
//
//	p := openai.New("gpt-4o", "https://api.openai.com/v1").WithAPIKey(key)
//	agent := core.New(p)
//
// Block-model notes: thinking and provider-raw blocks are dropped on send
// (Chat Completions has no input for them); images map to image_url content
// parts (data: URLs for inline bytes); tool results map to role:"tool" messages
// with an "error:" content prefix when the result is an error (no native error
// flag). CallOptions.ThinkingBudget is ignored.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/emotional-data/automata/core"
)

var _ core.StreamProvider = (*Provider)(nil)

type Provider struct {
	model       string
	baseURL     string
	apiKey      string
	streamUsage bool
	httpClient  *http.Client
}

// New builds a Provider for the given model against baseURL (e.g.
// "https://api.openai.com/v1"). Set the key with [Provider.WithAPIKey].
func New(model, baseURL string) *Provider {
	return &Provider{model: model, baseURL: strings.TrimRight(baseURL, "/"), httpClient: http.DefaultClient}
}

func (p *Provider) WithAPIKey(key string) *Provider {
	p.apiKey = key
	return p
}

// WithHTTPClient overrides the HTTP client (useful in tests).
func (p *Provider) WithHTTPClient(c *http.Client) *Provider {
	p.httpClient = c
	return p
}

// WithStreamUsage opts into sending stream_options.include_usage=true on
// streaming requests so usage is returned in the final chunk. Off by default:
// some OpenAI-compatible backends reject unknown request fields with HTTP 400.
func (p *Provider) WithStreamUsage() *Provider {
	p.streamUsage = true
	return p
}

// APIError wraps a non-2xx HTTP response and implements retry.Retryable so
// automata's retry layer recovers from 429s and 5xx.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openai api returned %d: %s", e.StatusCode, e.Body)
}
func (e *APIError) Retryable() bool { return e.StatusCode == 429 || e.StatusCode >= 500 }

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Tools         []wireTool     `json:"tools,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u wireUsage) toCore() *core.Usage {
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.PromptTokensDetails.CachedTokens == 0 {
		return nil
	}
	return &core.Usage{
		InputTokens:     u.PromptTokens,
		OutputTokens:    u.CompletionTokens,
		CacheReadTokens: u.PromptTokensDetails.CachedTokens,
	}
}

func (p *Provider) buildRequest(req core.Request, stream bool) (chatRequest, error) {
	msgs, err := convertMessages(req.Messages)
	if err != nil {
		return chatRequest{}, err
	}
	body := chatRequest{
		Model:    p.model,
		Messages: msgs,
		Stream:   stream,
	}
	if len(req.Tools) > 0 {
		body.Tools = convertTools(req.Tools)
	}
	applyCallOptions(&body, req.Options)
	if stream && p.streamUsage {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return body, nil
}

// applyCallOptions maps core.CallOptions onto the request. ThinkingBudget is
// ignored (Chat Completions has no equivalent).
func applyCallOptions(body *chatRequest, o core.CallOptions) {
	body.Temperature = o.Temperature
	body.MaxTokens = o.MaxTokens
	if len(o.StopSequences) > 0 {
		body.Stop = o.StopSequences
	}
	if o.ToolChoice != nil {
		switch o.ToolChoice.Mode {
		case core.ToolChoiceNone:
			body.ToolChoice = "none"
		case core.ToolChoiceAny:
			body.ToolChoice = "required"
		case core.ToolChoiceTool:
			body.ToolChoice = map[string]any{
				"type":     "function",
				"function": map[string]string{"name": o.ToolChoice.Name},
			}
		}
	}
}

func (p *Provider) newHTTPRequest(ctx context.Context, body chatRequest, stream bool) (*http.Request, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	return httpReq, nil
}

type chatResponse struct {
	Choices []struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage wireUsage `json:"usage"`
}

func (p *Provider) Invoke(ctx context.Context, req core.Request) (core.Response, error) {
	body, err := p.buildRequest(req, false)
	if err != nil {
		return core.Response{}, err
	}
	httpReq, err := p.newHTTPRequest(ctx, body, false)
	if err != nil {
		return core.Response{}, err
	}
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return core.Response{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return core.Response{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return core.Response{}, &APIError{StatusCode: resp.StatusCode, Body: string(raw)}
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return core.Response{}, err
	}
	if len(parsed.Choices) == 0 {
		return core.Response{}, errors.New("openai: no choices in response")
	}
	choice := parsed.Choices[0]
	msg := convertResponse(choice.Message)
	msg.Usage = parsed.Usage.toCore()
	return core.Response{Message: msg, StopReason: choice.FinishReason}, nil
}

type streamChunkWire struct {
	Choices []struct {
		Delta struct {
			Content   string         `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
}

func (p *Provider) InvokeStream(ctx context.Context, req core.Request) (<-chan core.StreamChunk, error) {
	body, err := p.buildRequest(req, true)
	if err != nil {
		return nil, err
	}
	httpReq, err := p.newHTTPRequest(ctx, body, true)
	if err != nil {
		return nil, err
	}
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(raw)}
	}

	out := make(chan core.StreamChunk)
	go func() {
		defer resp.Body.Close()
		defer close(out)

		send := func(c core.StreamChunk) bool {
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}

		reader := bufio.NewReader(resp.Body)
		for {
			lineBytes, readErr := reader.ReadBytes('\n')
			if len(lineBytes) > 0 {
				line := strings.TrimRight(string(lineBytes), "\r\n")
				if !handleSSELine(line, send) {
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					send(core.StreamChunk{Err: readErr})
				}
				return
			}
		}
	}()

	return out, nil
}

// handleSSELine parses one SSE line and sends a StreamChunk if it yields data.
// Returns false if the consumer went away or the line was malformed (error
// sent), so the caller stops reading.
func handleSSELine(line string, send func(core.StreamChunk) bool) bool {
	if !strings.HasPrefix(line, "data:") {
		return true
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return true
	}

	var wire streamChunkWire
	if err := json.Unmarshal([]byte(payload), &wire); err != nil {
		send(core.StreamChunk{Err: fmt.Errorf("decode sse chunk: %w", err)})
		return false
	}

	chunk := core.StreamChunk{}
	if wire.Usage != nil {
		chunk.Usage = wire.Usage.toCore()
	}
	if len(wire.Choices) > 0 {
		choice := wire.Choices[0]
		chunk.FinishReason = choice.FinishReason
		if choice.Delta.Content != "" {
			// Text is content-block index 0.
			chunk.Deltas = append(chunk.Deltas, core.BlockDelta{
				Index: 0, Type: "text", Text: choice.Delta.Content,
			})
		}
		for _, tc := range choice.Delta.ToolCalls {
			// Reserve block index 0 for text; tool-call index i → block 1+i.
			d := core.BlockDelta{Index: 1 + tc.Index, PartialJSON: tc.Function.Arguments}
			if tc.ID != "" || tc.Function.Name != "" {
				d.Type = "tool_use"
				d.ID = tc.ID
				d.Name = tc.Function.Name
			}
			chunk.Deltas = append(chunk.Deltas, d)
		}
	}

	if len(chunk.Deltas) == 0 && chunk.FinishReason == "" && chunk.Usage == nil {
		return true
	}
	return send(chunk)
}
