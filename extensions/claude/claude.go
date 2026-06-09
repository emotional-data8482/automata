package claude

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/emotional-data/automata/core"
)

// APIError wraps an HTTP error from the Anthropic API. It implements the
// retry.Retryable interface, so automata's retry layer can recover from
// transient failures that slip past the SDK's own internal retries
// (option.WithMaxRetries, default 2).
//
// Retryable codes: 408 (request timeout), 429 (rate limit), 529 (overloaded),
// and any 5xx (server errors). Skip 409 — that's a semantic conflict
// (idempotency mismatch), retrying won't help.
//
// This is slightly more generous than a plain `429 || >= 500` policy —
// Anthropic surfaces 529 distinctly for overload, and 408/timeout is transient
// by definition. The divergence is intentional.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic api returned %d: %s", e.StatusCode, e.Body)
}

func (e *APIError) Retryable() bool {
	return e.StatusCode == 408 ||
		e.StatusCode == 429 ||
		e.StatusCode == 529 ||
		e.StatusCode >= 500
}

// wrapAPIError converts an Anthropic SDK error into a *APIError so the retry
// layer can classify it. Non-SDK errors (network failures, context
// cancellation, etc.) pass through unchanged.
func wrapAPIError(err error) error {
	if err == nil {
		return nil
	}
	var sdkErr *anthropic.Error
	if errors.As(err, &sdkErr) {
		return &APIError{StatusCode: sdkErr.StatusCode, Body: sdkErr.Error()}
	}
	return err
}

var _ core.StreamProvider = (*Provider)(nil)

// DefaultMaxTokens is used when the Provider is constructed without calling
// WithMaxTokens. The Anthropic API requires max_tokens on every request; 16k
// keeps non-streaming calls comfortably under SDK HTTP timeouts.
const DefaultMaxTokens int64 = 16000

type Provider struct {
	model       string
	maxTokens   int64
	cacheSystem bool
	client      anthropic.Client
}

// New builds a Provider for the given Claude model. If apiKey is empty the
// SDK falls back to ANTHROPIC_API_KEY from the environment.
func New(model, apiKey string) *Provider {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return &Provider{
		model:     model,
		maxTokens: DefaultMaxTokens,
		client:    anthropic.NewClient(opts...),
	}
}

// WithMaxTokens overrides the default max_tokens sent on every request.
// For values much above ~16k, prefer InvokeStream to avoid HTTP timeouts.
func (p *Provider) WithMaxTokens(n int64) *Provider {
	p.maxTokens = n
	return p
}

// WithSystemPromptCache attaches a cache_control: ephemeral breakpoint to the
// last system block on every request. Anthropic renders tools → system →
// messages, so a breakpoint on the trailing system block caches the entire
// tools + system prefix together — which is the slice that's stable across
// every turn of an agent run.
//
// The minimum cacheable prefix is model-dependent (~4096 tokens on Opus and
// Haiku 4.5; ~2048 on Sonnet 4.6). Below that the API silently won't cache
// and you'll see cache_creation_input_tokens stay at 0. No error, just no
// hit. Verify via response usage on the first vs. second call.
func (p *Provider) WithSystemPromptCache() *Provider {
	p.cacheSystem = true
	return p
}

func (p *Provider) buildParams(messages []core.Message, tools []core.Tool) (anthropic.MessageNewParams, error) {
	system, msgs, err := convertMessages(messages)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.maxTokens,
		Messages:  msgs,
	}
	if len(system) > 0 {
		if p.cacheSystem {
			system[len(system)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		params.System = system
	}
	if len(tools) > 0 {
		params.Tools = convertTools(tools)
	}
	return params, nil
}

func (p *Provider) Invoke(ctx context.Context, messages []core.Message, tools []core.Tool) (core.Message, error) {
	params, err := p.buildParams(messages, tools)
	if err != nil {
		return core.Message{}, err
	}
	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return core.Message{}, fmt.Errorf("anthropic invoke: %w", wrapAPIError(err))
	}
	return convertResponse(resp), nil
}

func (p *Provider) InvokeStream(ctx context.Context, messages []core.Message, tools []core.Tool) (<-chan core.StreamChunk, error) {
	params, err := p.buildParams(messages, tools)
	if err != nil {
		return nil, err
	}

	stream := p.client.Messages.NewStreaming(ctx, params)
	out := make(chan core.StreamChunk)

	go func() {
		defer close(out)

		send := func(c core.StreamChunk) bool {
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for stream.Next() {
			event := stream.Current()
			switch ev := event.AsAny().(type) {
			case anthropic.MessageStartEvent:
				// Anthropic reports input_tokens (and the cache_*_input_tokens
				// breakdown) on message_start, then output_tokens on each
				// message_delta. Emit a usage-only chunk now; the stream
				// assembler merges Usage fields across chunks.
				u := ev.Message.Usage
				if u.InputTokens > 0 || u.CacheCreationInputTokens > 0 || u.CacheReadInputTokens > 0 {
					if !send(core.StreamChunk{Usage: &core.Usage{
						InputTokens:         int(u.InputTokens),
						CacheCreationTokens: int(u.CacheCreationInputTokens),
						CacheReadTokens:     int(u.CacheReadInputTokens),
					}}) {
						return
					}
				}
			case anthropic.ContentBlockStartEvent:
				// A new tool_use block starts: announce its index/id/name so the
				// stream assembler can register the slot before InputJSONDelta
				// fragments arrive.
				if tu, ok := ev.ContentBlock.AsAny().(anthropic.ToolUseBlock); ok {
					if !send(core.StreamChunk{
						ToolCalls: []core.StreamToolCallFragment{{
							Index: int(ev.Index),
							ID:    tu.ID,
							Type:  "function",
							Name:  tu.Name,
						}},
					}) {
						return
					}
				}
			case anthropic.ContentBlockDeltaEvent:
				switch d := ev.Delta.AsAny().(type) {
				case anthropic.TextDelta:
					if d.Text == "" {
						continue
					}
					if !send(core.StreamChunk{ContentDelta: d.Text}) {
						return
					}
				case anthropic.InputJSONDelta:
					if d.PartialJSON == "" {
						continue
					}
					if !send(core.StreamChunk{
						ToolCalls: []core.StreamToolCallFragment{{
							Index:     int(ev.Index),
							Arguments: d.PartialJSON,
						}},
					}) {
						return
					}
				}
			case anthropic.MessageDeltaEvent:
				chunk := core.StreamChunk{}
				if ev.Delta.StopReason != "" {
					chunk.FinishReason = string(ev.Delta.StopReason)
				}
				if ev.Usage.OutputTokens > 0 || ev.Usage.InputTokens > 0 ||
					ev.Usage.CacheCreationInputTokens > 0 || ev.Usage.CacheReadInputTokens > 0 {
					chunk.Usage = &core.Usage{
						InputTokens:         int(ev.Usage.InputTokens),
						OutputTokens:        int(ev.Usage.OutputTokens),
						CacheCreationTokens: int(ev.Usage.CacheCreationInputTokens),
						CacheReadTokens:     int(ev.Usage.CacheReadInputTokens),
					}
				}
				if chunk.FinishReason == "" && chunk.Usage == nil {
					continue
				}
				if !send(chunk) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			send(core.StreamChunk{Err: wrapAPIError(err)})
		}
	}()

	return out, nil
}
