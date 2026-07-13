package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/emotional-data/automata/retry"
)

// partialBlock accumulates the deltas for one content block as a stream
// progresses, keyed by the block's provider index (see [consumeStream]).
type partialBlock struct {
	typ       string          // "text" | "thinking" | "tool_use"
	content   strings.Builder // text or thinking content
	signature string          // thinking signature
	id        string          // tool_use id
	name      string          // tool_use name
	input     strings.Builder // tool_use input (partial JSON)
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
func (a *Agent) RunStream(ctx context.Context, task string, onEvent func(StreamEvent), opts ...RunOption) (RunResult, error) {
	return a.runStream(ctx, newLoop(a, nil), task, onEvent, opts...)
}

// runStream drives a pre-built loop through the streaming path. Split from
// RunStream so a [Session] can supply a loop seeded with its transcript.
func (a *Agent) runStream(ctx context.Context, l *Loop, task string, onEvent func(StreamEvent), opts ...RunOption) (RunResult, error) {
	if onEvent == nil {
		onEvent = func(StreamEvent) {}
	}
	cfg := a.newRunConfig(opts)

	sp, streamOK := a.provider.(StreamProvider)

	l.streaming = true
	var mu sync.Mutex
	l.emit = func(ev StreamEvent) {
		mu.Lock()
		defer mu.Unlock()
		onEvent(ev)
	}

	if !streamOK {
		return l.run(ctx, task, "fallback", cfg, func(ctx context.Context, _ *slog.Logger, req Request) (Response, error) {
			resp, err := retry.Do(ctx, a.retryCfg, func() (Response, error) {
				return a.provider.Invoke(ctx, req)
			})
			if err == nil {
				if think := resp.Message.Thinking(); think != "" {
					l.emit(StreamEvent{Kind: StreamThinking, Text: think})
				}
				if text := resp.Message.Text(); text != "" {
					l.emit(StreamEvent{Kind: StreamText, Text: text})
				}
			}
			return resp, err
		})
	}

	return l.run(ctx, task, "stream", cfg, func(ctx context.Context, log *slog.Logger, req Request) (Response, error) {
		var emitted bool
		streamEmit := func(kind StreamEventKind, delta string) {
			emitted = true
			l.emit(StreamEvent{Kind: kind, Text: delta})
		}
		resp, err := retry.Do(ctx, a.retryCfg, func() (Response, error) {
			m, finish, cerr := consumeStream(ctx, sp, req, streamEmit, log)
			if cerr != nil && emitted {
				return Response{}, &terminalStreamError{err: cerr}
			}
			return Response{Message: m, StopReason: finish}, cerr
		})
		if err != nil {
			if t, ok := errors.AsType[*terminalStreamError](err); ok {
				err = t.err
			}
		}
		return resp, err
	})
}

// consumeStream reads a provider's StreamChunks, assembling the block deltas
// into the final assistant [Message] and the provider's finish reason. onDelta
// receives each text or thinking delta as it arrives (with the matching event
// kind) so the caller can forward it live.
func consumeStream(ctx context.Context, sp StreamProvider, req Request, onDelta func(StreamEventKind, string), log *slog.Logger) (Message, string, error) {
	ch, err := sp.InvokeStream(ctx, req)
	if err != nil {
		return Message{}, "", err
	}

	var partials []partialBlock
	var usage *Usage
	var finishReason string

	for {
		select {
		case <-ctx.Done():
			// Producer's send selects on ctx.Done() too, so it will exit on its own
			// once it notices cancellation. Drain defensively in case a chunk is
			// already in flight.
			go drain(ch)
			return Message{}, "", ctx.Err()
		case chunk, ok := <-ch:
			if !ok {
				return buildAssistantMessage(partials, usage), finishReason, nil
			}
			if chunk.Err != nil {
				// Abandon the stream — producer may still have unsent chunks.
				go drain(ch)
				return Message{}, "", chunk.Err
			}
			if chunk.FinishReason != "" {
				finishReason = chunk.FinishReason
			}
			for _, d := range chunk.Deltas {
				for len(partials) <= d.Index {
					partials = append(partials, partialBlock{})
				}
				slot := &partials[d.Index]
				if d.Type != "" {
					slot.typ = d.Type
					log.DebugContext(ctx, "content block started",
						"index", d.Index, "type", d.Type, "id", d.ID, "name", d.Name)
				}
				if d.ID != "" {
					slot.id = d.ID
				}
				if d.Name != "" {
					slot.name = d.Name
				}
				if d.Signature != "" {
					slot.signature += d.Signature
				}
				if d.Text != "" {
					slot.content.WriteString(d.Text)
					if slot.typ == "thinking" {
						onDelta(StreamThinking, d.Text)
					} else {
						onDelta(StreamText, d.Text)
					}
				}
				if d.PartialJSON != "" {
					slot.input.WriteString(d.PartialJSON)
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

// buildAssistantMessage assembles the accumulated per-index block partials into
// the final assistant [Message], in index order. Empty gap slots (an index that
// received no type) are skipped.
func buildAssistantMessage(partials []partialBlock, usage *Usage) Message {
	var blocks Blocks
	for _, p := range partials {
		switch p.typ {
		case "text":
			if p.content.Len() > 0 {
				blocks = append(blocks, TextBlock{Text: p.content.String()})
			}
		case "thinking":
			blocks = append(blocks, ThinkingBlock{
				Thinking:  p.content.String(),
				Signature: p.signature,
			})
		case "tool_use":
			// A real tool call always carries a name; an unnamed slot is a gap.
			if p.name == "" {
				continue
			}
			input := p.input.String()
			if input == "" {
				input = "{}"
			}
			blocks = append(blocks, ToolUseBlock{
				ID:    p.id,
				Name:  p.name,
				Input: json.RawMessage(input),
			})
		}
	}
	return Message{Role: "assistant", Blocks: blocks, Usage: usage}
}

// terminalStreamError marks an error from consumeStream as non-retryable because
// onChunk has already emitted content. Retrying would cause the caller to see
// duplicate or divergent content across attempts.
type terminalStreamError struct{ err error }

func (e *terminalStreamError) Error() string   { return e.err.Error() }
func (e *terminalStreamError) Unwrap() error   { return e.err }
func (e *terminalStreamError) Retryable() bool { return false }
