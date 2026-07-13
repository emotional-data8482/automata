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
	model             string
	maxTokens         int64
	cacheSystem       bool
	cacheConversation bool
	client            anthropic.Client
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

// WithConversationCache attaches a cache_control: ephemeral breakpoint to the
// last content block of the last message on every request, so the whole
// conversation prefix up to that point is cached and re-read cheaply on the next
// turn. This is the big cost lever for multi-turn agent loops, where the prefix
// is stable and grows by one turn each request.
//
// It composes with [Provider.WithSystemPromptCache] (that caches the
// tools+system prefix; this caches the messages prefix) — two of Anthropic's
// four cache breakpoints. The breakpoint is placed on the last cacheable block,
// walking back past thinking blocks, which reject cache_control.
//
// Note: a context [core.Compactor] rewrites the message prefix when it
// compacts, which invalidates this cache for that turn; the cache re-warms on
// the turns that follow.
func (p *Provider) WithConversationCache() *Provider {
	p.cacheConversation = true
	return p
}

func (p *Provider) buildParams(req core.Request) (anthropic.MessageNewParams, error) {
	system, msgs, err := convertMessages(req.Messages)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}
	maxTokens := p.maxTokens
	if req.Options.MaxTokens > 0 {
		maxTokens = int64(req.Options.MaxTokens)
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: maxTokens,
		Messages:  msgs,
	}
	if len(system) > 0 {
		if p.cacheSystem {
			system[len(system)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		params.System = system
	}
	if len(req.Tools) > 0 {
		params.Tools = convertTools(req.Tools)
	}
	if p.cacheConversation {
		applyConversationCache(params.Messages)
	}
	applyCallOptions(&params, req.Options)
	return params, nil
}

// applyConversationCache stamps a cache_control breakpoint on the last cacheable
// content block of the last message, walking back past blocks that reject it
// (thinking / redacted_thinking).
func applyConversationCache(msgs []anthropic.MessageParam) {
	if len(msgs) == 0 {
		return
	}
	last := &msgs[len(msgs)-1]
	for i := len(last.Content) - 1; i >= 0; i-- {
		if setBlockCacheControl(&last.Content[i]) {
			return
		}
	}
}

// setBlockCacheControl sets an ephemeral cache_control on b's active variant,
// reporting whether the variant supports it (thinking blocks do not).
func setBlockCacheControl(b *anthropic.ContentBlockParamUnion) bool {
	cc := anthropic.NewCacheControlEphemeralParam()
	switch {
	case b.OfText != nil:
		b.OfText.CacheControl = cc
	case b.OfImage != nil:
		b.OfImage.CacheControl = cc
	case b.OfToolUse != nil:
		b.OfToolUse.CacheControl = cc
	case b.OfToolResult != nil:
		b.OfToolResult.CacheControl = cc
	default:
		return false
	}
	return true
}

// applyCallOptions maps the provider-agnostic [core.CallOptions] onto Anthropic
// request params. Options the API cannot honor are ignored.
func applyCallOptions(params *anthropic.MessageNewParams, o core.CallOptions) {
	if o.Temperature != nil {
		params.Temperature = anthropic.Float(*o.Temperature)
	}
	if len(o.StopSequences) > 0 {
		params.StopSequences = o.StopSequences
	}
	if o.ThinkingBudget > 0 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(int64(o.ThinkingBudget))
	}
	if o.ToolChoice != nil {
		switch o.ToolChoice.Mode {
		case core.ToolChoiceNone:
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		case core.ToolChoiceAny:
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
		case core.ToolChoiceTool:
			params.ToolChoice = anthropic.ToolChoiceUnionParam{
				OfTool: &anthropic.ToolChoiceToolParam{Name: o.ToolChoice.Name},
			}
		}
	}
}

func (p *Provider) Invoke(ctx context.Context, req core.Request) (core.Response, error) {
	params, err := p.buildParams(req)
	if err != nil {
		return core.Response{}, err
	}
	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return core.Response{}, fmt.Errorf("anthropic invoke: %w", wrapAPIError(err))
	}
	return core.Response{Message: convertResponse(resp), StopReason: string(resp.StopReason)}, nil
}

func (p *Provider) InvokeStream(ctx context.Context, req core.Request) (<-chan core.StreamChunk, error) {
	params, err := p.buildParams(req)
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
				// A new content block starts: announce its index and type so the
				// stream assembler can register the slot (and, for tool_use, its
				// id/name) before delta fragments arrive.
				switch cb := ev.ContentBlock.AsAny().(type) {
				case anthropic.TextBlock:
					if !send(core.StreamChunk{Deltas: []core.BlockDelta{{
						Index: int(ev.Index), Type: "text",
					}}}) {
						return
					}
				case anthropic.ThinkingBlock:
					if !send(core.StreamChunk{Deltas: []core.BlockDelta{{
						Index: int(ev.Index), Type: "thinking",
					}}}) {
						return
					}
				case anthropic.ToolUseBlock:
					if !send(core.StreamChunk{Deltas: []core.BlockDelta{{
						Index: int(ev.Index), Type: "tool_use", ID: cb.ID, Name: cb.Name,
					}}}) {
						return
					}
				}
			case anthropic.ContentBlockDeltaEvent:
				switch d := ev.Delta.AsAny().(type) {
				case anthropic.TextDelta:
					if d.Text == "" {
						continue
					}
					if !send(core.StreamChunk{Deltas: []core.BlockDelta{{
						Index: int(ev.Index), Text: d.Text,
					}}}) {
						return
					}
				case anthropic.ThinkingDelta:
					if d.Thinking == "" {
						continue
					}
					if !send(core.StreamChunk{Deltas: []core.BlockDelta{{
						Index: int(ev.Index), Text: d.Thinking,
					}}}) {
						return
					}
				case anthropic.SignatureDelta:
					if d.Signature == "" {
						continue
					}
					if !send(core.StreamChunk{Deltas: []core.BlockDelta{{
						Index: int(ev.Index), Signature: d.Signature,
					}}}) {
						return
					}
				case anthropic.InputJSONDelta:
					if d.PartialJSON == "" {
						continue
					}
					if !send(core.StreamChunk{Deltas: []core.BlockDelta{{
						Index: int(ev.Index), PartialJSON: d.PartialJSON,
					}}}) {
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
