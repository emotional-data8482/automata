package core

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/emotional-data/automata/retry"
)

type partialToolCall struct {
	id        string
	callType  string
	name      string
	arguments strings.Builder
}

// RunStream runs the agent like [Agent.Run] but delivers a live event stream to
// onEvent as the run progresses: assistant content deltas ([StreamText]), each
// tool call the model requests ([StreamToolCall]), and each tool result
// ([StreamToolResult]). Switch on [StreamEvent.Kind] to handle each variant.
//
// If the provider does not implement [StreamProvider], it falls back to a single
// non-streaming invocation and delivers the whole content as one [StreamText]
// event; tool-call and tool-result events still fire. A nil onEvent is treated
// as a no-op.
//
// onEvent may be called from multiple goroutines (tool results are produced
// concurrently), but RunStream serializes the calls, so the callback need not be
// safe for concurrent use.
func (a *Agent) RunStream(ctx context.Context, task string, onEvent func(StreamEvent)) (string, error) {
	if onEvent == nil {
		onEvent = func(StreamEvent) {}
	}

	sp, streamOK := a.provider.(StreamProvider)

	l := newLoop(a)
	l.streaming = true
	var mu sync.Mutex
	l.emit = func(ev StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		onEvent(ev)
	}

	if !streamOK {
		return l.run(ctx, task, "fallback", func(ctx context.Context, _ *slog.Logger, messages []Message, tools []Tool) (Message, error) {
			msg, err := retry.Do(ctx, a.retryCfg, func() (Message, error) {
				return a.provider.Invoke(ctx, messages, tools)
			})
			if err == nil && msg.Content != nil && *msg.Content != "" {
				l.emit(StreamEvent{Kind: StreamText, Text: *msg.Content})
			}
			return msg, err
		})
	}

	return l.run(ctx, task, "stream", func(ctx context.Context, log *slog.Logger, messages []Message, tools []Tool) (Message, error) {
		var emitted bool
		streamOnChunk := func(delta string) {
			emitted = true
			l.emit(StreamEvent{Kind: StreamText, Text: delta})
		}
		msg, err := retry.Do(ctx, a.retryCfg, func() (Message, error) {
			m, cerr := consumeStream(ctx, sp, messages, tools, streamOnChunk, log)
			if cerr != nil && emitted {
				return Message{}, &terminalStreamError{err: cerr}
			}
			return m, cerr
		})
		if err != nil {
			if t, ok := errors.AsType[*terminalStreamError](err); ok {
				err = t.err
			}
		}
		return msg, err
	})
}

func consumeStream(ctx context.Context, sp StreamProvider, messages []Message, tools []Tool, onChunk func(string), log *slog.Logger) (Message, error) {
	ch, err := sp.InvokeStream(ctx, messages, tools)
	if err != nil {
		return Message{}, err
	}

	var content strings.Builder
	var partials []partialToolCall
	var usage *Usage

	for {
		select {
		case <-ctx.Done():
			// Producer's send selects on ctx.Done() too, so it will exit on its own
			// once it notices cancellation. Drain defensively in case a chunk is
			// already in flight.
			go drain(ch)
			return Message{}, ctx.Err()
		case chunk, ok := <-ch:
			if !ok {
				for i, p := range partials {
					log.DebugContext(ctx, "tool call assembled",
						"index", i,
						"id", p.id,
						"name", p.name,
						"args", p.arguments.String(),
					)
				}
				return buildAssistantMessage(content.String(), partials, usage), nil
			}
			if chunk.Err != nil {
				// Abandon the stream — producer may still have unsent chunks.
				go drain(ch)
				return Message{}, chunk.Err
			}
			if chunk.ContentDelta != "" {
				content.WriteString(chunk.ContentDelta)
				onChunk(chunk.ContentDelta)
			}
			for _, frag := range chunk.ToolCalls {
				for len(partials) <= frag.Index {
					partials = append(partials, partialToolCall{})
				}
				slot := &partials[frag.Index]
				isFirst := frag.ID != "" || frag.Name != ""
				if frag.ID != "" {
					slot.id = frag.ID
				}
				if frag.Type != "" {
					slot.callType = frag.Type
				}
				if frag.Name != "" {
					slot.name = frag.Name
				}
				if frag.Arguments != "" {
					slot.arguments.WriteString(frag.Arguments)
				}
				if isFirst {
					log.DebugContext(ctx, "tool call started",
						"index", frag.Index,
						"id", slot.id,
						"name", slot.name,
					)
				}
				if frag.Arguments != "" {
					log.DebugContext(ctx, "tool call args fragment",
						"index", frag.Index,
						"name", slot.name,
						"fragment", frag.Arguments,
						"total_len", slot.arguments.Len(),
					)
				}
			}
			if chunk.Usage != nil {
				if usage == nil {
					usage = &Usage{}
				}
				usage.Merge(chunk.Usage)
			}
		}
	}
}

func drain(ch <-chan StreamChunk) {
	for range ch {
	}
}

func buildAssistantMessage(content string, partials []partialToolCall, usage *Usage) Message {
	msg := Message{Role: "assistant"}
	if content != "" {
		c := content
		msg.Content = &c
	}
	var calls []ToolCall
	for _, p := range partials {
		// Skip gap slots. Fragments are placed by content-block index, which
		// counts non-tool blocks too (e.g. leading assistant text), so partials
		// can hold empty holes when tool_use blocks don't start at index 0. A
		// real tool call always carries a name; an unnamed slot is a gap.
		if p.name == "" {
			continue
		}
		calls = append(calls, ToolCall{
			ID:   p.id,
			Type: p.callType,
			Function: FunctionCall{
				Name:      p.name,
				Arguments: p.arguments.String(),
			},
		})
	}
	msg.ToolCalls = calls
	msg.Usage = usage
	return msg
}

// terminalStreamError marks an error from consumeStream as non-retryable because
// onChunk has already emitted content. Retrying would cause the caller to see
// duplicate or divergent content across attempts.
type terminalStreamError struct{ err error }

func (e *terminalStreamError) Error() string   { return e.err.Error() }
func (e *terminalStreamError) Unwrap() error   { return e.err }
func (e *terminalStreamError) Retryable() bool { return false }
