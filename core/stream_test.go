package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// --- block-message test helpers -------------------------------------------

// asstText builds an assistant message carrying a single text block.
func asstText(s string) Message { return AssistantMessage(TextBlock{Text: s}) }

// toolUse builds a ToolUseBlock with the given raw JSON arguments.
func toolUse(id, name, args string) ToolUseBlock {
	return ToolUseBlock{ID: id, Name: name, Input: json.RawMessage(args)}
}

// asstTool builds an assistant message carrying a single tool_use block.
func asstTool(id, name, args string) Message {
	return AssistantMessage(toolUse(id, name, args))
}

// withUsage attaches usage to a message and returns it, for terse literals.
func withUsage(m Message, u *Usage) Message {
	m.Usage = u
	return m
}

// streamMessage emits a scripted assistant Message as the StreamChunk sequence a
// provider would produce: a start delta per block, then its content, then usage.
func streamMessage(ch chan<- StreamChunk, m Message) {
	for idx, blk := range m.Blocks {
		switch b := blk.(type) {
		case TextBlock:
			ch <- StreamChunk{Deltas: []BlockDelta{{Index: idx, Type: "text"}}}
			ch <- StreamChunk{Deltas: []BlockDelta{{Index: idx, Text: b.Text}}}
		case ThinkingBlock:
			ch <- StreamChunk{Deltas: []BlockDelta{{Index: idx, Type: "thinking"}}}
			ch <- StreamChunk{Deltas: []BlockDelta{{Index: idx, Text: b.Thinking}}}
			if b.Signature != "" {
				ch <- StreamChunk{Deltas: []BlockDelta{{Index: idx, Signature: b.Signature}}}
			}
		case ToolUseBlock:
			ch <- StreamChunk{Deltas: []BlockDelta{{Index: idx, Type: "tool_use", ID: b.ID, Name: b.Name}}}
			ch <- StreamChunk{Deltas: []BlockDelta{{Index: idx, PartialJSON: string(b.Input)}}}
		}
	}
	if m.Usage != nil {
		ch <- StreamChunk{Usage: m.Usage}
	}
}

// scriptedStreamProvider replays a fixed sequence of StreamChunks for each turn,
// letting tests drive RunStream's streaming path without a network.
type scriptedStreamProvider struct {
	turns [][]StreamChunk
	calls int
}

var _ StreamProvider = (*scriptedStreamProvider)(nil)

func (p *scriptedStreamProvider) Invoke(context.Context, Request) (Response, error) {
	return Response{}, errors.New("Invoke should not be used on the streaming path")
}

func (p *scriptedStreamProvider) InvokeStream(context.Context, Request) (<-chan StreamChunk, error) {
	if p.calls >= len(p.turns) {
		return nil, fmt.Errorf("no script for turn %d", p.calls)
	}
	chunks := p.turns[p.calls]
	p.calls++
	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		for _, c := range chunks {
			ch <- c
		}
	}()
	return ch, nil
}

// scriptedProvider replays one Message per turn. It implements only Provider
// (not StreamProvider), so RunStream takes its non-streaming fallback path.
type scriptedProvider struct {
	turns []Message
	calls int
}

func (p *scriptedProvider) Invoke(context.Context, Request) (Response, error) {
	if p.calls >= len(p.turns) {
		return Response{}, fmt.Errorf("no script for turn %d", p.calls)
	}
	m := p.turns[p.calls]
	p.calls++
	return Response{Message: m}, nil
}

// recordingProvider implements both Provider and StreamProvider, replaying the
// same scripted Messages on either path and recording which path each turn took.
// It lets tests assert whether a sub-agent ran streaming (InvokeStream) or
// non-streaming (Invoke).
type recordingProvider struct {
	turns       []Message
	calls       int
	syncCalls   int
	streamCalls int
}

var _ StreamProvider = (*recordingProvider)(nil)

func (p *recordingProvider) next() (Message, error) {
	if p.calls >= len(p.turns) {
		return Message{}, fmt.Errorf("no script for turn %d", p.calls)
	}
	m := p.turns[p.calls]
	p.calls++
	return m, nil
}

func (p *recordingProvider) Invoke(context.Context, Request) (Response, error) {
	p.syncCalls++
	m, err := p.next()
	return Response{Message: m}, err
}

func (p *recordingProvider) InvokeStream(context.Context, Request) (<-chan StreamChunk, error) {
	p.streamCalls++
	m, err := p.next()
	if err != nil {
		return nil, err
	}
	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		streamMessage(ch, m)
	}()
	return ch, nil
}

type echoArgs struct {
	Msg string `json:"msg"`
}

// toolCallChunksAt builds the two-fragment chunk sequence Anthropic-style
// streams produce for one tool call at content-block index: a start fragment
// (id/name) then an args fragment. The index matters — providers count non-tool
// blocks (e.g. leading text) toward it, so tool calls don't always start at 0.
func toolCallChunksAt(index int, id, name, args string) []StreamChunk {
	return []StreamChunk{
		{Deltas: []BlockDelta{{Index: index, Type: "tool_use", ID: id, Name: name}}},
		{Deltas: []BlockDelta{{Index: index, PartialJSON: args}}},
	}
}

func toolCallChunks(id, name, args string) []StreamChunk {
	return toolCallChunksAt(0, id, name, args)
}

// textChunksAt builds the two-chunk sequence a stream produces for a text block
// at the given content-block index: a start delta (announcing the type) then a
// content delta.
func textChunksAt(index int, s string) []StreamChunk {
	return []StreamChunk{
		{Deltas: []BlockDelta{{Index: index, Type: "text"}}},
		{Deltas: []BlockDelta{{Index: index, Text: s}}},
	}
}

func textChunks(s string) []StreamChunk { return textChunksAt(0, s) }

func TestRunStreamEmitsTextToolCallAndResult(t *testing.T) {
	provider := &scriptedStreamProvider{turns: [][]StreamChunk{
		// Turn 1: leading text (block 0), then a tool call (block 1).
		append(textChunks("Let me check. "), toolCallChunksAt(1, "call_1", "echo", `{"msg":"hi"}`)...),
		// Turn 2: the final answer, no tool calls.
		textChunks("Done."),
	}}

	agent := New(provider)
	agent.RegisterTool(Func("echo", "echoes msg", func(_ context.Context, a echoArgs) (string, error) {
		return "echoed:" + a.Msg, nil
	}))

	var got []StreamEvent
	out, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		got = append(got, ev)
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out.Output != "Done." {
		t.Errorf("final output = %q, want %q", out.Output, "Done.")
	}

	call := toolUse("call_1", "echo", `{"msg":"hi"}`)
	want := []StreamEvent{
		{Kind: StreamText, Text: "Let me check. "},
		{Kind: StreamToolCall, ToolCall: call},
		{Kind: StreamToolResult, ToolCall: call, Result: "echoed:hi"},
		{Kind: StreamText, Text: "Done."},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d:\n got: %+v\nwant: %+v", len(got), len(want), got, want)
	}
	// Compare field-by-field; all Err are expected nil here (avoid DeepEqual
	// over the error field and the RawMessage in ToolCall).
	for i := range want {
		if got[i].Err != nil {
			t.Errorf("event %d: unexpected Err %v", i, got[i].Err)
		}
		if got[i].Kind != want[i].Kind || got[i].Text != want[i].Text ||
			got[i].Result != want[i].Result || !toolCallEqual(got[i].ToolCall, want[i].ToolCall) {
			t.Errorf("event %d mismatch:\n got: %+v\nwant: %+v", i, got[i], want[i])
		}
	}
}

// toolCallEqual compares two ToolUseBlocks by value, treating Input as raw
// bytes (json.RawMessage is not comparable with ==).
func toolCallEqual(a, b ToolUseBlock) bool {
	return a.ID == b.ID && a.Name == b.Name && string(a.Input) == string(b.Input)
}

// TestRunStreamSkipsGapToolSlots reproduces the case where the model emits
// leading text (content block 0) then tool calls (blocks 1, 2). The block-0 gap
// must not become a phantom unnamed tool call.
func TestRunStreamSkipsGapToolSlots(t *testing.T) {
	turn1 := textChunks("Let me fetch both. ")
	turn1 = append(turn1, toolCallChunksAt(1, "c1", "now", `{"tz":"Asia/Tokyo"}`)...)
	turn1 = append(turn1, toolCallChunksAt(2, "c2", "now", `{"tz":""}`)...)
	provider := &scriptedStreamProvider{turns: [][]StreamChunk{
		turn1,
		textChunks("done"),
	}}

	agent := New(provider)
	agent.RegisterTool(Func("now", "returns a time", func(_ context.Context, _ struct{}) (string, error) {
		return "a-time", nil
	}))

	var calls, results int
	out, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		switch ev.Kind {
		case StreamToolCall:
			calls++
			if ev.ToolCall.Name == "" {
				t.Errorf("phantom tool call with empty name: %+v", ev.ToolCall)
			}
		case StreamToolResult:
			results++
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out.Output != "done" {
		t.Errorf("final output = %q, want %q", out.Output, "done")
	}
	if calls != 2 || results != 2 {
		t.Errorf("got %d tool calls / %d results, want 2 / 2", calls, results)
	}
}

func TestRunStreamToolResultCarriesError(t *testing.T) {
	provider := &scriptedStreamProvider{turns: [][]StreamChunk{
		toolCallChunks("call_1", "boom", `{}`),
		textChunks("recovered"),
	}}

	agent := New(provider)
	agent.RegisterTool(Func("boom", "always fails", func(_ context.Context, _ struct{}) (string, error) {
		return "", errors.New("kaboom")
	}))

	var result *StreamEvent
	if _, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		if ev.Kind == StreamToolResult {
			e := ev
			result = &e
		}
	}); err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	if result == nil {
		t.Fatal("no StreamToolResult event emitted")
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "kaboom") {
		t.Errorf("result.Err = %v, want one containing %q", result.Err, "kaboom")
	}
	// The core result is the raw error text (no "error: " prefix — that's the
	// provider's job now), and IsError is set.
	if result.Result != "kaboom" {
		t.Errorf("result.Result = %q, want %q", result.Result, "kaboom")
	}
	if !result.IsError {
		t.Error("result.IsError = false, want true")
	}
}

func TestRunStreamFallbackEmitsToolEvents(t *testing.T) {
	provider := &scriptedProvider{turns: []Message{
		AssistantMessage(TextBlock{Text: "thinking"}, toolUse("c1", "echo", `{"msg":"yo"}`)),
		asstText("final"),
	}}

	agent := New(provider)
	agent.RegisterTool(Func("echo", "echoes msg", func(_ context.Context, a echoArgs) (string, error) {
		return "echoed:" + a.Msg, nil
	}))

	var kinds []StreamEventKind
	var result string
	out, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == StreamToolResult {
			result = ev.Result
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out.Output != "final" {
		t.Errorf("final output = %q, want %q", out.Output, "final")
	}

	wantKinds := []StreamEventKind{StreamText, StreamToolCall, StreamToolResult, StreamText}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Errorf("event kinds = %v, want %v", kinds, wantKinds)
	}
	if result != "echoed:yo" {
		t.Errorf("tool result = %q, want %q", result, "echoed:yo")
	}
}

// TestRunStreamEmitsUsage verifies the loop surfaces per-turn token usage as a
// StreamUsage event when the provider reports it.
func TestRunStreamEmitsUsage(t *testing.T) {
	provider := &scriptedStreamProvider{turns: [][]StreamChunk{
		append(textChunks("hi"),
			StreamChunk{Usage: &Usage{InputTokens: 12, OutputTokens: 7}},
		),
	}}

	agent := New(provider)

	var usage *Usage
	var usageEvents int
	if _, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		if ev.Kind == StreamUsage {
			usageEvents++
			usage = ev.Usage
		}
	}); err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	if usageEvents != 1 {
		t.Fatalf("got %d StreamUsage events, want 1", usageEvents)
	}
	if usage == nil || usage.InputTokens != 12 || usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want {InputTokens:12 OutputTokens:7}", usage)
	}
}

// TestAsToolStreamsSubAgentEvents verifies that when a parent run is streaming,
// an AsTool sub-agent runs in streaming mode and forwards its events into the
// parent stream tagged with the tool's name — and that a plain Run instead
// routes the sub-agent through its non-streaming path.
func TestAsToolStreamsSubAgentEvents(t *testing.T) {
	subCall := toolUse("s1", "subagent", "{}")
	final := "done"
	subText := "sub result"

	// Streaming parent: sub-agent should stream and its text should arrive tagged.
	t.Run("streaming propagates tagged events", func(t *testing.T) {
		subProvider := &recordingProvider{turns: []Message{asstText(subText)}}
		sub := New(subProvider)

		orch := New(&recordingProvider{turns: []Message{
			AssistantMessage(subCall),
			asstText(final),
		}})
		orch.RegisterTool(AsTool[struct{}](sub, "subagent", "a sub-agent"))

		var subTexts []StreamEvent
		var topText []StreamEvent
		out, err := orch.RunStream(context.Background(), "go", func(ev StreamEvent) {
			if ev.Kind != StreamText {
				return
			}
			switch ev.Agent {
			case "subagent":
				subTexts = append(subTexts, ev)
			case "":
				topText = append(topText, ev)
			}
		})
		if err != nil {
			t.Fatalf("RunStream: %v", err)
		}
		if out.Output != final {
			t.Errorf("output = %q, want %q", out.Output, final)
		}
		if subProvider.streamCalls != 1 || subProvider.syncCalls != 0 {
			t.Errorf("sub-agent path: streamCalls=%d syncCalls=%d, want 1/0", subProvider.streamCalls, subProvider.syncCalls)
		}
		if len(subTexts) != 1 || subTexts[0].Text != subText {
			t.Errorf("tagged sub-agent text events = %+v, want one %q", subTexts, subText)
		}
		if len(topText) != 1 || topText[0].Text != final {
			t.Errorf("top-level text events = %+v, want one %q", topText, final)
		}
	})

	// Non-streaming parent: sub-agent should run through Invoke, not InvokeStream.
	t.Run("plain Run does not stream sub-agent", func(t *testing.T) {
		subProvider := &recordingProvider{turns: []Message{asstText(subText)}}
		sub := New(subProvider)

		orch := New(&recordingProvider{turns: []Message{
			AssistantMessage(subCall),
			asstText(final),
		}})
		orch.RegisterTool(AsTool[struct{}](sub, "subagent", "a sub-agent"))

		out, err := orch.Run(context.Background(), "go")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if out.Output != final {
			t.Errorf("output = %q, want %q", out.Output, final)
		}
		if subProvider.syncCalls != 1 || subProvider.streamCalls != 0 {
			t.Errorf("sub-agent path: syncCalls=%d streamCalls=%d, want 1/0", subProvider.syncCalls, subProvider.streamCalls)
		}
	})
}

// TestRunStreamTurnOrdering pins the per-turn event ordering contract: a
// turn's text deltas arrive first (in order), then its StreamUsage (once per
// turn, when the provider reports usage), then its StreamToolCall events in
// batch order, then the StreamToolResult events (completion order — tools run
// concurrently, so results may arrive in any order within the turn).
func TestRunStreamTurnOrdering(t *testing.T) {
	turn1 := textChunks("Let me check. ")
	turn1 = append(turn1, toolCallChunksAt(1, "c1", "echo", `{"msg":"a"}`)...)
	turn1 = append(turn1, toolCallChunksAt(2, "c2", "echo", `{"msg":"b"}`)...)
	turn1 = append(turn1, StreamChunk{Usage: &Usage{InputTokens: 12, OutputTokens: 7}})
	provider := &scriptedStreamProvider{turns: [][]StreamChunk{
		turn1,
		append(textChunks("done"), StreamChunk{Usage: &Usage{InputTokens: 20, OutputTokens: 3}}),
	}}

	agent := New(provider)
	agent.RegisterTool(Func("echo", "echoes msg", func(_ context.Context, a echoArgs) (string, error) {
		return "echoed:" + a.Msg, nil
	}))

	var got []StreamEvent
	out, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		got = append(got, ev)
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out.Output != "done" {
		t.Errorf("final output = %q, want %q", out.Output, "done")
	}

	wantKinds := []StreamEventKind{
		StreamText,       // turn 1 content delta
		StreamUsage,      // turn 1 usage: after text, before tool calls
		StreamToolCall,   // c1 — batch order
		StreamToolCall,   // c2
		StreamToolResult, // c1/c2 in completion order
		StreamToolResult,
		StreamText,  // turn 2 final answer
		StreamUsage, // turn 2 usage
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(wantKinds), got)
	}
	for i, want := range wantKinds {
		if got[i].Kind != want {
			t.Errorf("event %d kind = %v, want %v (events: %+v)", i, got[i].Kind, want, got)
		}
	}
	if got[1].Usage == nil || got[1].Usage.InputTokens != 12 {
		t.Errorf("turn 1 usage = %+v, want InputTokens 12", got[1].Usage)
	}
	if got[2].ToolCall.ID != "c1" || got[3].ToolCall.ID != "c2" {
		t.Errorf("tool call order = %q, %q, want c1, c2", got[2].ToolCall.ID, got[3].ToolCall.ID)
	}
	results := map[string]string{
		got[4].ToolCall.ID: got[4].Result,
		got[5].ToolCall.ID: got[5].Result,
	}
	if results["c1"] != "echoed:a" || results["c2"] != "echoed:b" {
		t.Errorf("results = %v, want c1:echoed:a c2:echoed:b", results)
	}
}

// TestAsToolNestedTagsAndUsage runs a three-level agent tree (orchestrator →
// mid → leaf) and pins two contracts at once: the innermost Agent tag wins on
// nested sub-agents, and tagged StreamUsage events let a StreamAccumulator
// attribute usage per agent while Totals sums everything.
func TestAsToolNestedTagsAndUsage(t *testing.T) {
	leafText := "leaf-says"
	leafProvider := &recordingProvider{turns: []Message{
		withUsage(asstText(leafText), &Usage{InputTokens: 5, OutputTokens: 3}),
	}}
	leaf := New(leafProvider)

	midText := "mid-says"
	midProvider := &recordingProvider{turns: []Message{
		withUsage(asstTool("L1", "leaf", "{}"), &Usage{InputTokens: 7, OutputTokens: 2}),
		withUsage(asstText(midText), &Usage{InputTokens: 9, OutputTokens: 4}),
	}}
	mid := New(midProvider)
	mid.RegisterTool(AsTool[struct{}](leaf, "leaf", "leaf sub-agent"))

	topText := "top-says"
	orchProvider := &recordingProvider{turns: []Message{
		withUsage(asstTool("M1", "mid", "{}"), &Usage{InputTokens: 11, OutputTokens: 1}),
		withUsage(asstText(topText), &Usage{InputTokens: 13, OutputTokens: 6}),
	}}
	orch := New(orchProvider)
	orch.RegisterTool(AsTool[struct{}](mid, "mid", "mid sub-agent"))

	var acc StreamAccumulator
	textByAgent := map[string]string{}
	out, err := orch.RunStream(context.Background(), "go", func(ev StreamEvent) {
		acc.Add(ev)
		if ev.Kind == StreamText {
			textByAgent[ev.Agent] += ev.Text
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if out.Output != topText {
		t.Errorf("output = %q, want %q", out.Output, topText)
	}

	wantText := map[string]string{"": topText, "mid": midText, "leaf": leafText}
	for agent, want := range wantText {
		if textByAgent[agent] != want {
			t.Errorf("text for agent %q = %q, want %q", agent, textByAgent[agent], want)
		}
	}

	// The orchestrator calls "mid" with tool-call ID "M1"; mid calls "leaf" with
	// "L1". Those IDs surface as the InvocationID on each lane, and events keep
	// the innermost (agent, invocation) pair.
	wantUsage := []struct {
		agent, invocationID string
		want                Usage
	}{
		{"", "", Usage{InputTokens: 24, OutputTokens: 7}},      // 11+13 / 1+6
		{"mid", "M1", Usage{InputTokens: 16, OutputTokens: 6}}, // 7+9 / 2+4
		{"leaf", "L1", Usage{InputTokens: 5, OutputTokens: 3}},
	}
	for _, c := range wantUsage {
		view, ok := acc.View(c.agent, c.invocationID)
		if !ok {
			t.Fatalf("no view for agent %q invocation %q", c.agent, c.invocationID)
		}
		if view.Usage != c.want {
			t.Errorf("usage for agent %q = %+v, want %+v", c.agent, view.Usage, c.want)
		}
	}
	if totals := acc.Totals(); totals != (Usage{InputTokens: 45, OutputTokens: 16}) {
		t.Errorf("totals = %+v, want {45 16}", totals)
	}

	views := acc.Views()
	wantOrder := []string{"", "mid", "leaf"}
	if len(views) != len(wantOrder) {
		t.Fatalf("got %d views, want %d", len(views), len(wantOrder))
	}
	for i, want := range wantOrder {
		if views[i].Agent != want {
			t.Errorf("views[%d].Agent = %q, want %q", i, views[i].Agent, want)
		}
	}
}
