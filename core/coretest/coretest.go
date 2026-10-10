// Package coretest provides test doubles and fixtures for code built on core:
// a scripted provider that replays model turns, and Runtime fixtures over an
// in-memory store. It is stdlib-only, so any module can use it from its tests.
package coretest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

// ErrScriptExhausted is returned when a scripted provider is called after its
// last turn.
var ErrScriptExhausted = errors.New("coretest: provider script exhausted")

// Turn is one scripted provider response.
type Turn struct {
	// Message is the assistant message returned.
	Message core.Message
	// StopReason is the reported stop reason. Zero infers it from Message:
	// tool use when it calls tools, end turn otherwise.
	StopReason core.StopReason
	// Usage, when set, is reported as the response's usage.
	Usage *core.Usage
	// Err fails the call. A streaming provider sends Message's content first
	// when there is any, then fails mid-stream.
	Err error
}

// Text returns a turn that answers with text and ends the turn.
func Text(text string) Turn {
	return Turn{Message: core.AssistantMessage(core.TextBlock{Text: text})}
}

// ToolCalls returns a turn that calls the given tools.
func ToolCalls(calls ...core.ToolUseBlock) Turn {
	blocks := make([]core.Block, len(calls))
	for i, call := range calls {
		blocks[i] = call
	}
	return Turn{Message: core.AssistantMessage(blocks...)}
}

// ToolCall returns a turn that calls one tool. See [Call] for input.
func ToolCall(id, name string, input any) Turn { return ToolCalls(Call(id, name, input)) }

// Call builds a tool call. input may be a JSON string, a [json.RawMessage],
// nil for {}, or any value, which is encoded as JSON.
func Call(id, name string, input any) core.ToolUseBlock {
	var raw json.RawMessage
	switch value := input.(type) {
	case nil:
		raw = json.RawMessage(`{}`)
	case json.RawMessage:
		raw = append(json.RawMessage(nil), value...)
	case string:
		raw = json.RawMessage(value)
	default:
		data, err := json.Marshal(value)
		if err != nil {
			panic(fmt.Sprintf("coretest: encode %s input: %v", name, err))
		}
		raw = data
	}
	return core.ToolUseBlock{ID: id, Name: name, Input: raw}
}

// Fail returns a turn that fails the provider call with err.
func Fail(err error) Turn { return Turn{Err: err} }

// Provider replays scripted turns in order and records every request. It is
// safe for concurrent use. It implements [core.Provider] only; use
// [StreamProvider] to exercise the streaming path.
type Provider struct {
	mu       sync.Mutex
	turns    []Turn
	requests []core.Request
}

// NewProvider returns a provider that replays turns in order.
func NewProvider(turns ...Turn) *Provider { return &Provider{turns: turns} }

// Append adds turns to the end of the script.
func (p *Provider) Append(turns ...Turn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.turns = append(p.turns, turns...)
}

// Calls reports how many times the provider was called.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// Remaining reports how many scripted turns have not been used.
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.turns)
}

// Requests returns the requests received so far, oldest first.
func (p *Provider) Requests() []core.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]core.Request(nil), p.requests...)
}

func (p *Provider) next(req core.Request) (Turn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.turns) == 0 {
		return Turn{}, fmt.Errorf("%w after %d calls", ErrScriptExhausted, len(p.requests)-1)
	}
	turn := p.turns[0]
	p.turns = p.turns[1:]
	return turn, nil
}

// Invoke returns the next scripted turn.
func (p *Provider) Invoke(_ context.Context, req core.Request) (core.Response, error) {
	turn, err := p.next(req)
	if err != nil {
		return core.Response{}, err
	}
	if turn.Err != nil {
		return core.Response{}, turn.Err
	}
	message := turn.Message
	if turn.Usage != nil {
		usage := *turn.Usage
		message.Usage = &usage
	}
	return core.Response{Message: message, StopReason: stopReason(turn)}, nil
}

func stopReason(turn Turn) core.StopReason {
	if turn.StopReason != "" {
		return turn.StopReason
	}
	if len(turn.Message.ToolUses()) > 0 {
		return core.StopToolUse
	}
	return core.StopEndTurn
}

// StreamProvider replays the same script as [Provider] through
// [core.StreamProvider]: each turn's blocks are streamed as content deltas,
// then its stop reason and usage.
type StreamProvider struct{ *Provider }

// NewStreamProvider returns a streaming provider that replays turns in order.
func NewStreamProvider(turns ...Turn) StreamProvider {
	return StreamProvider{Provider: NewProvider(turns...)}
}

// InvokeStream streams the next scripted turn.
func (p StreamProvider) InvokeStream(ctx context.Context, req core.Request) (<-chan core.StreamChunk, error) {
	turn, err := p.next(req)
	if err != nil {
		return nil, err
	}
	if turn.Err != nil && len(turn.Message.Blocks) == 0 {
		return nil, turn.Err
	}
	chunks := streamChunks(turn)
	ch := make(chan core.StreamChunk)
	go func() {
		defer close(ch)
		for _, chunk := range chunks {
			select {
			case ch <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func streamChunks(turn Turn) []core.StreamChunk {
	var chunks []core.StreamChunk
	for index, block := range turn.Message.Blocks {
		switch block := block.(type) {
		case core.TextBlock:
			chunks = append(chunks, core.StreamChunk{Deltas: []core.BlockDelta{{Index: index, Type: "text", Text: block.Text}}})
		case core.ThinkingBlock:
			chunks = append(chunks, core.StreamChunk{Deltas: []core.BlockDelta{{Index: index, Type: "thinking", Text: block.Thinking, Signature: block.Signature}}})
		case core.ToolUseBlock:
			chunks = append(chunks, core.StreamChunk{Deltas: []core.BlockDelta{{Index: index, Type: "tool_use", ID: block.ID, Name: block.Name, PartialJSON: string(block.Input)}}})
		}
	}
	if turn.Err != nil {
		return append(chunks, core.StreamChunk{Usage: turn.Usage, Err: turn.Err})
	}
	return append(chunks, core.StreamChunk{StopReason: stopReason(turn), Usage: turn.Usage})
}

// ProviderFunc adapts a function to [core.Provider], for responses that
// depend on the request.
type ProviderFunc func(context.Context, core.Request) (core.Response, error)

// Invoke calls f.
func (f ProviderFunc) Invoke(ctx context.Context, req core.Request) (core.Response, error) {
	return f(ctx, req)
}

// NewAgent builds an Agent or fails the test.
func NewAgent(t testing.TB, provider core.Provider, config core.AgentConfig) *core.Agent {
	t.Helper()
	agent, err := core.New(provider, config)
	if err != nil {
		t.Fatalf("coretest: new agent: %v", err)
	}
	return agent
}

// NewRuntime opens a Runtime and closes it when the test ends. A nil
// config.Store selects a fresh in-memory store.
func NewRuntime(t testing.TB, config core.RuntimeConfig) *core.Runtime {
	t.Helper()
	if config.Store == nil {
		config.Store = core.NewMemoryStore()
	}
	runtime, err := core.NewRuntime(context.Background(), config)
	if err != nil {
		t.Fatalf("coretest: new runtime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

// Register registers agent under id and revision or fails the test.
func Register(t testing.TB, runtime *core.Runtime, id, revision string, agent *core.Agent) core.DefinitionRef {
	t.Helper()
	ref, err := runtime.Register(id, revision, agent)
	if err != nil {
		t.Fatalf("coretest: register %s@%s: %v", id, revision, err)
	}
	return ref
}

// ReusableStore wraps a store so closing a Runtime leaves it open. A restart
// test closes one Runtime and opens another on the same store, as a process
// restart would:
//
//	store := coretest.ReusableStore(core.NewMemoryStore())
//	first := coretest.NewRuntime(t, core.RuntimeConfig{Store: store})
//	// ... submit work, then first.Close()
//	second := coretest.NewRuntime(t, core.RuntimeConfig{Store: store})
//	// ... Register, then second.Recover(ctx)
func ReusableStore(store core.Store) core.Store { return reusableStore{Store: store} }

type reusableStore struct{ core.Store }

func (reusableStore) Close() error { return nil }
