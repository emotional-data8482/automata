// Package openrouter implements a [core.StreamProvider] against OpenRouter
// (https://openrouter.ai) through the official Go SDK, giving one provider
// surface over the hundreds of models OpenRouter routes to. The model string
// is an OpenRouter model ID such as "anthropic/claude-sonnet-4.5" or
// "openai/gpt-5.2".
//
//	p := openrouter.New("anthropic/claude-sonnet-4.5", openrouter.WithAPIKey(key))
//	agent, err := core.New(p, core.AgentConfig{})
//
// Retries: SDK-internal retries are disabled at construction; transient
// failures are classified by the wrapped [APIError] so automata's retry layer
// owns every retry, and provider attempts stay visible to run accounting.
//
// Block-model notes: thinking and provider-raw blocks are dropped on send
// (Chat Completions-style input has no thinking channel, and OpenRouter
// cannot preserve thinking signatures across its normalization); reasoning
// returned by the model becomes a [core.ThinkingBlock] without a signature.
// Tool results map to role:"tool" messages with an "error:" content prefix
// when the result failed, and non-text tool-result blocks degrade to
// placeholder text. Images map to image_url content parts (data: URLs for
// inline bytes). CallOptions.ThinkingBudget is ignored (OpenRouter exposes
// reasoning effort levels, not token budgets).
package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	orsdk "github.com/OpenRouterTeam/go-sdk"
	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/OpenRouterTeam/go-sdk/models/operations"
	"github.com/OpenRouterTeam/go-sdk/models/sdkerrors"
	"github.com/OpenRouterTeam/go-sdk/optionalnullable"
	orsdkretry "github.com/OpenRouterTeam/go-sdk/retry"

	"github.com/emotional-data8482/automata/core"
)

var _ core.StreamProvider = (*Provider)(nil)
var _ core.StructuredOutputProvider = (*Provider)(nil)

// APIError wraps a non-2xx OpenRouter response and implements retry.Retryable,
// so automata's retry layer recovers from 429s, 408s, and 5xxs. SDK-internal
// retries are disabled (see New), so nothing retries behind this
// classification.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openrouter api returned %d: %s", e.StatusCode, e.Body)
}

func (e *APIError) Retryable() bool {
	return e.StatusCode == 408 || e.StatusCode == 429 || e.StatusCode >= 500
}

// wrapAPIError converts an OpenRouter SDK error into a *APIError so the retry
// layer can classify it. The SDK returns *sdkerrors.APIError for statuses
// without a typed shape and typed errors for known statuses; both map to the
// HTTP status they represent. Non-SDK errors (network failures, context
// cancellation) pass through unchanged.
func wrapAPIError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) {
		return &APIError{StatusCode: apiErr.StatusCode, Body: apiErr.Body}
	}
	for _, typed := range []struct {
		target any
		code   int
	}{
		{new(*sdkerrors.TooManyRequestsResponseError), 429},
		{new(*sdkerrors.RequestTimeoutResponseError), 408},
		{new(*sdkerrors.InternalServerResponseError), 500},
		{new(*sdkerrors.BadGatewayResponseError), 502},
		{new(*sdkerrors.ServiceUnavailableResponseError), 503},
		{new(*sdkerrors.GatewayTimeoutResponseError), 504},
		{new(*sdkerrors.EdgeNetworkTimeoutResponseError), 522},
		{new(*sdkerrors.ProviderOverloadedResponseError), 529},
	} {
		if errors.As(err, typed.target) {
			return &APIError{StatusCode: typed.code, Body: err.Error()}
		}
	}
	return err
}

// SupportsNativeStructuredOutput reports that OpenRouter can enforce a JSON
// schema via response_format when [core.CallOptions.OutputSchema] is set.
// Capability is per provider, not per model: OpenRouter routes to hundreds of
// models, so the schema travels without the OpenAI strict-mode flag (strict
// subsets are provider-specific) and a model that rejects schema enforcement
// surfaces that as an invocation error. Core still validates the payload.
func (p *Provider) SupportsNativeStructuredOutput() bool { return true }

// Provider is an OpenRouter-backed core.StreamProvider.
type Provider struct {
	model  string
	client *orsdk.OpenRouter
}

// Option configures a [Provider] at construction.
type Option func(*settings)

type settings struct {
	apiKey    string
	serverURL string
	referer   string
	title     string
	client    *http.Client
}

// WithAPIKey sets the OpenRouter API key. When empty, requests are sent
// unauthenticated and OpenRouter rejects them.
func WithAPIKey(key string) Option {
	return func(s *settings) { s.apiKey = key }
}

// WithServerURL overrides the OpenRouter base URL, for OpenAI-compatible
// gateways that speak the OpenRouter chat surface and for tests.
func WithServerURL(url string) Option {
	return func(s *settings) { s.serverURL = url }
}

// WithHTTPClient overrides the HTTP client the SDK uses (useful in tests).
func WithHTTPClient(c *http.Client) Option {
	return func(s *settings) { s.client = c }
}

// WithAppInfo sets the HTTP-Referer and X-Title headers OpenRouter uses for
// app attribution on its public rankings. Both are optional.
func WithAppInfo(refererURL, title string) Option {
	return func(s *settings) { s.referer, s.title = refererURL, title }
}

// New builds a Provider for the given OpenRouter model ID (for example
// "anthropic/claude-sonnet-4.5"). SDK-internal retries are disabled here so
// automata's retry layer owns every retry; transient failures are classified
// through [APIError].
func New(model string, opts ...Option) *Provider {
	s := settings{}
	for _, opt := range opts {
		if opt != nil {
			opt(&s)
		}
	}
	sdkOpts := []orsdk.SDKOption{
		orsdk.WithRetryConfig(orsdkretry.Config{}), // strategy "" → single attempt
	}
	if s.apiKey != "" {
		sdkOpts = append(sdkOpts, orsdk.WithSecurity(s.apiKey))
	}
	if s.serverURL != "" {
		sdkOpts = append(sdkOpts, orsdk.WithServerURL(s.serverURL))
	}
	if s.client != nil {
		sdkOpts = append(sdkOpts, orsdk.WithClient(s.client))
	}
	if s.referer != "" {
		sdkOpts = append(sdkOpts, orsdk.WithHTTPReferer(s.referer))
	}
	if s.title != "" {
		sdkOpts = append(sdkOpts, orsdk.WithXTitle(s.title))
	}
	return &Provider{model: model, client: orsdk.New(sdkOpts...)}
}

// buildRequest assembles the SDK ChatRequest from a core Request. stream is
// forwarded as the request's stream flag.
func (p *Provider) buildRequest(req core.Request, stream bool) (components.ChatRequest, error) {
	msgs, err := convertMessages(req.Messages)
	if err != nil {
		return components.ChatRequest{}, err
	}
	body := components.ChatRequest{
		Model:    orsdk.Pointer(p.model),
		Messages: msgs,
		Stream:   orsdk.Pointer(stream),
	}
	if len(req.Tools) > 0 {
		tools, err := convertTools(req.Tools)
		if err != nil {
			return components.ChatRequest{}, err
		}
		body.Tools = tools
	}
	applyCallOptions(&body, req.Options)
	return body, nil
}

// applyCallOptions maps core.CallOptions onto the request. ThinkingBudget is
// ignored (OpenRouter exposes reasoning effort levels, not token budgets).
// OutputSchema becomes a response_format json_schema object; the strict flag
// is deliberately unset (see SupportsNativeStructuredOutput).
func applyCallOptions(body *components.ChatRequest, o core.CallOptions) {
	if o.Temperature != nil {
		body.Temperature = optionalnullable.From(orsdk.Pointer(*o.Temperature))
	}
	if o.MaxTokens > 0 {
		body.MaxTokens = optionalnullable.From(orsdk.Pointer(int64(o.MaxTokens)))
	}
	if len(o.StopSequences) > 0 {
		body.Stop = optionalnullable.From(orsdk.Pointer(components.CreateStopArrayOfStr(o.StopSequences)))
	}
	if o.ToolChoice != nil {
		switch o.ToolChoice.Mode {
		case core.ToolChoiceNone:
			body.ToolChoice = orsdk.Pointer(components.CreateChatToolChoiceChatToolChoiceNone(components.ChatToolChoiceNoneNone))
		case core.ToolChoiceAny:
			body.ToolChoice = orsdk.Pointer(components.CreateChatToolChoiceChatToolChoiceRequired(components.ChatToolChoiceRequiredRequired))
		case core.ToolChoiceTool:
			body.ToolChoice = orsdk.Pointer(components.CreateChatToolChoiceChatNamedToolChoice(components.ChatNamedToolChoice{
				Type:     components.ChatNamedToolChoiceTypeFunction,
				Function: components.ChatNamedToolChoiceFunction{Name: o.ToolChoice.Name},
			}))
		}
	}
	if len(o.OutputSchema) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(o.OutputSchema, &schema); err == nil {
			body.ResponseFormat = orsdk.Pointer(components.CreateResponseFormatJSONSchema(components.ChatFormatJSONSchemaConfig{
				JSONSchema: components.ChatJSONSchemaConfig{Name: "response", Schema: schema},
			}))
		}
	}
}

// Invoke performs a non-streaming chat completion and converts the result
// into a core Response.
func (p *Provider) Invoke(ctx context.Context, req core.Request) (core.Response, error) {
	body, err := p.buildRequest(req, false)
	if err != nil {
		return core.Response{}, err
	}
	res, err := p.client.Chat.Send(ctx, body, nil)
	if err != nil {
		return core.Response{}, fmt.Errorf("openrouter invoke: %w", wrapAPIError(err))
	}
	if res == nil || res.Type != operations.SendChatCompletionRequestResponseTypeChatResult {
		return core.Response{}, errors.New("openrouter: expected chat result")
	}
	result := res.ChatResult
	if len(result.Choices) == 0 {
		return core.Response{}, errors.New("openrouter: no choices in response")
	}
	choice := result.Choices[0]
	msg := convertResponse(choice.Message)
	msg.Usage = usageToCore(result.Usage)
	raw := ""
	if choice.FinishReason != nil {
		raw = string(*choice.FinishReason)
	}
	stop := mapFinishReason(raw)
	if refusal, ok := choice.Message.Refusal.Get(); ok && refusal != nil && *refusal != "" {
		stop = core.StopContentFilter
	}
	return core.Response{
		Message:       msg,
		StopReason:    stop,
		RawStopReason: raw,
	}, nil
}

// Streaming block-index scheme: OpenRouter deltas carry no content-block
// index, so blocks are assigned fixed slots — text at 0, thinking at 1, and
// tool call i at 2+i. The stream assembler keys blocks by these indices.
const (
	textBlockIndex     = 0
	thinkingBlockIndex = 1
	toolBlockBaseIndex = 2
)

// InvokeStream starts a streaming chat completion and forwards converted
// [core.StreamChunk]s: text and reasoning deltas, fragmented tool-call
// arguments, the finish reason, and the usage OpenRouter reports on the final
// chunk. A mid-stream OpenRouter error becomes a chunk error, which fails the
// turn with the partial transcript preserved.
func (p *Provider) InvokeStream(ctx context.Context, req core.Request) (<-chan core.StreamChunk, error) {
	body, err := p.buildRequest(req, true)
	if err != nil {
		return nil, err
	}
	res, err := p.client.Chat.Send(ctx, body, nil)
	if err != nil {
		return nil, fmt.Errorf("openrouter stream: %w", wrapAPIError(err))
	}
	if res == nil || res.Type != operations.SendChatCompletionRequestResponseTypeEventStream || res.EventStream == nil {
		return nil, errors.New("openrouter: expected event stream")
	}
	stream := res.EventStream

	out := make(chan core.StreamChunk)
	go func() {
		defer close(out)
		defer stream.Close()

		send := func(c core.StreamChunk) bool {
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for stream.Next() {
			chunk := stream.Value()
			if chunk == nil {
				continue
			}
			core, ok := streamChunkToCore(chunk.Data)
			if !ok {
				continue
			}
			if !send(core) {
				return
			}
		}
		if err := stream.Err(); err != nil && !send(core.StreamChunk{Err: err}) {
			return
		}
	}()
	return out, nil
}

// streamChunkToCore maps one OpenRouter stream chunk to a core StreamChunk.
// It reports false for chunks that carry no information (keepalives, role-only
// deltas), matching how the wire drops empty frames.
func streamChunkToCore(chunk components.ChatStreamChunk) (core.StreamChunk, bool) {
	out := core.StreamChunk{}
	if chunk.Error != nil {
		out.Err = fmt.Errorf("openrouter stream error %d: %s", chunk.Error.Code, chunk.Error.Message)
		return out, true
	}
	if usage := usageToCore(chunk.Usage); usage != nil {
		out.Usage = usage
	}
	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]
		if choice.FinishReason != nil {
			raw := string(*choice.FinishReason)
			out.RawStopReason = raw
			out.FinishReason = raw
			out.StopReason = mapFinishReason(raw)
		}
		if r, ok := choice.Delta.Reasoning.Get(); ok && r != nil && *r != "" {
			out.Deltas = append(out.Deltas, core.BlockDelta{
				Index: thinkingBlockIndex, Type: "thinking", Text: *r,
			})
		}
		if c, ok := choice.Delta.Content.Get(); ok && c != nil && *c != "" {
			out.Deltas = append(out.Deltas, core.BlockDelta{
				Index: textBlockIndex, Type: "text", Text: *c,
			})
		}
		for _, tc := range choice.Delta.ToolCalls {
			d := core.BlockDelta{Index: toolBlockBaseIndex + int(tc.Index)}
			if tc.ID != nil {
				d.ID = *tc.ID
			}
			if tc.Function != nil && tc.Function.Name != nil {
				d.Name = *tc.Function.Name
			}
			if d.ID != "" || d.Name != "" {
				d.Type = "tool_use"
			}
			if tc.Function != nil && tc.Function.Arguments != nil {
				d.PartialJSON = *tc.Function.Arguments
			}
			out.Deltas = append(out.Deltas, d)
		}
	}
	if out.Err == nil && len(out.Deltas) == 0 && out.RawStopReason == "" && out.Usage == nil {
		return core.StreamChunk{}, false
	}
	return out, true
}
