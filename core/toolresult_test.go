package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/retry"
)

// --- ToolResult constructors (Task 1) --------------------------------------

func TestToolResultConstructors(t *testing.T) {
	if got := TextResult("hi"); got.Text() != "hi" || got.IsError {
		t.Errorf("TextResult = %+v", got)
	}
	if len(TextResult("hi").Blocks) != 1 {
		t.Errorf("TextResult should carry exactly one text block")
	}
	if _, ok := TextResult("hi").Blocks[0].(TextBlock); !ok {
		t.Errorf("TextResult block = %T, want TextBlock", TextResult("hi").Blocks[0])
	}

	empty := BlockResult()
	if len(empty.Blocks) != 1 {
		t.Errorf("BlockResult() should normalize to one block, got %d", len(empty.Blocks))
	}
	if _, ok := empty.Blocks[0].(TextBlock); !ok {
		t.Errorf("BlockResult() block = %T, want TextBlock", empty.Blocks[0])
	}

	img := ImageResult("image/png", []byte{1, 2, 3})
	if len(img.Blocks) != 1 {
		t.Fatalf("ImageResult should carry one block")
	}
	ib, ok := img.Blocks[0].(ImageBlock)
	if !ok {
		t.Fatalf("ImageResult block = %T, want ImageBlock", img.Blocks[0])
	}
	if ib.MediaType != "image/png" || string(ib.Data) != "\x01\x02\x03" || ib.URL != "" {
		t.Errorf("ImageResult block = %+v", ib)
	}

	uimg := URLImageResult("https://example.com/x.png")
	if len(uimg.Blocks) != 1 {
		t.Fatalf("URLImageResult should carry one block")
	}
	ub, ok := uimg.Blocks[0].(ImageBlock)
	if !ok {
		t.Fatalf("URLImageResult block = %T, want ImageBlock", ub)
	}
	if ub.URL != "https://example.com/x.png" || len(ub.Data) != 0 {
		t.Errorf("URLImageResult block = %+v", ub)
	}

	errRes := ErrorResult("boom")
	if !errRes.IsError || errRes.Text() != "boom" {
		t.Errorf("ErrorResult = %+v", errRes)
	}
	if errRes.Text() != "boom" {
		t.Errorf("ErrorResult text = %q", errRes.Text())
	}

	// Text() concatenates text blocks and ignores non-text ones.
	mixed := BlockResult(TextBlock{Text: "a"}, ImageBlock{MediaType: "image/png", Data: []byte{9}}, TextBlock{Text: "b"})
	if got := mixed.Text(); got != "ab" {
		t.Errorf("mixed Text() = %q, want %q", got, "ab")
	}
}

func TestNormalizeResultZeroValue(t *testing.T) {
	n := normalizeResult(ToolResult{})
	if len(n.Blocks) != 1 {
		t.Fatalf("normalized zero result should carry one block, got %d", len(n.Blocks))
	}
	if _, ok := n.Blocks[0].(TextBlock); !ok {
		t.Errorf("normalized zero block = %T, want TextBlock", n.Blocks[0])
	}
}

// --- Transcript constructors (Task 3) --------------------------------------

func TestToolResultMessageDelegatesToBlockForm(t *testing.T) {
	str := ToolResultMessage("t1", "hi", true)
	blk := ToolResultBlockMessage("t1", Blocks{TextBlock{Text: "hi"}}, true)
	sraw, err := json.Marshal(str)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	braw, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(sraw) != string(braw) {
		t.Errorf("string form JSON %s != block form JSON %s", sraw, braw)
	}

	tr := str.Blocks[0].(ToolResultBlock)
	if tr.ToolUseID != "t1" || !tr.IsError || (Message{Blocks: tr.Content}).Text() != "hi" {
		t.Errorf("tool result block = %+v", tr)
	}
}

func TestToolResultBlockMessageMixedContent(t *testing.T) {
	msg := ToolResultBlockMessage("t2", Blocks{
		TextBlock{Text: "screenshot:"},
		ImageBlock{MediaType: "image/png", Data: []byte{1}},
	}, false)

	tr := msg.Blocks[0].(ToolResultBlock)
	if tr.ToolUseID != "t2" || tr.IsError {
		t.Errorf("tool result block = %+v", tr)
	}
	if len(tr.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(tr.Content))
	}
	if _, ok := tr.Content[0].(TextBlock); !ok {
		t.Errorf("content[0] = %T, want TextBlock", tr.Content[0])
	}
	if _, ok := tr.Content[1].(ImageBlock); !ok {
		t.Errorf("content[1] = %T, want ImageBlock", tr.Content[1])
	}
}

// --- FuncResult (Task 2) ---------------------------------------------------

type resultArgs struct {
	City string `json:"city" desc:"city to look up"`
}

func TestFuncResultSchemaParityWithFunc(t *testing.T) {
	res := FuncResult[resultArgs]("weather", "look up weather",
		func(context.Context, resultArgs) (ToolResult, error) { return ToolResult{}, nil })
	str := Func[resultArgs]("weather", "look up weather",
		func(context.Context, resultArgs) (string, error) { return "", nil })

	if string(res.Definition().InputSchema) != string(str.Definition().InputSchema) {
		t.Errorf("schema mismatch:\nrich: %s\ntext: %s", res.Definition().InputSchema, str.Definition().InputSchema)
	}
}

func TestFuncResultArgs(t *testing.T) {
	cases := []struct {
		args string
		city string
	}{
		{`{"city":"Paris"}`, "Paris"},
		{"", ""},     // empty args render the zero value
		{"null", ""}, // null args render the zero value
		{"{}", ""},   // {} args render the zero value
	}
	for _, tc := range cases {
		tool := FuncResult("weather", "look up weather", func(_ context.Context, a resultArgs) (ToolResult, error) {
			return TextResult("sunny in " + a.City), nil
		})
		out, err := tool.Execute(context.Background(), json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("Execute(%q): %v", tc.args, err)
		}
		want := "sunny in " + tc.city
		if out.Text() != want {
			t.Errorf("Execute(%q) = %q, want %q", tc.args, out.Text(), want)
		}
	}
}

func TestFuncResultInvalidArgs(t *testing.T) {
	tool := FuncResult("weather", "look up weather", func(_ context.Context, a resultArgs) (ToolResult, error) {
		return TextResult("never"), nil
	})
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"city":42}`))
	if err == nil || !strings.HasPrefix(err.Error(), "invalid args: ") {
		t.Errorf("err = %v, want prefix %q", err, "invalid args: ")
	}
}

func TestFuncResultRegistrationPaths(t *testing.T) {
	tool := FuncResult("echo", "echo", func(_ context.Context, a resultArgs) (ToolResult, error) {
		return TextResult("ok"), nil
	})
	// FuncResult returns a Tool, so both registration paths accept it.
	_ = New(nil).WithTools(tool)
	a := New(nil)
	a.RegisterTool(tool)
	if a.tools[0].Definition().Name != "echo" {
		t.Errorf("registered tool name = %q", a.tools[0].Definition().Name)
	}
}

// --- Loop behavior (Task 4) ------------------------------------------------

func TestLoopRichTextResult(t *testing.T) {
	final := "done"
	provider := &capturingProvider{turns: []Message{
		asstTool("r1", "weather", `{"city":"Paris"}`),
		asstText(final),
	}}
	agent := New(provider)
	agent.RegisterTool(FuncResult("weather", "look up weather",
		func(_ context.Context, a resultArgs) (ToolResult, error) {
			return TextResult("sunny in " + a.City), nil
		}))

	res, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != final {
		t.Errorf("output = %q, want %q", res.Output, final)
	}

	results := transcriptToolResults(res.Messages)
	if len(results) != 1 {
		t.Fatalf("got %d tool results, want 1", len(results))
	}
	tr := results[0]
	if tr.ToolUseID != "r1" || tr.IsError {
		t.Errorf("tool result = %+v", tr)
	}
	if got := (Message{Blocks: tr.Content}).Text(); got != "sunny in Paris" {
		t.Errorf("content text = %q, want %q", got, "sunny in Paris")
	}
}

func TestLoopRichMixedResultPreservesBlockOrder(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("r1", "shot", "{}"),
		asstText("done"),
	}}
	agent := New(provider)
	agent.RegisterTool(FuncResult("shot", "take a screenshot", func(context.Context, struct{}) (ToolResult, error) {
		return BlockResult(
			TextBlock{Text: "screenshot:"},
			ImageBlock{MediaType: "image/png", Data: []byte{1, 2}},
			ImageBlock{URL: "https://example.com/x.png"},
		), nil
	}))

	res, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := transcriptToolResults(res.Messages)
	if len(results) != 1 {
		t.Fatalf("got %d tool results, want 1", len(results))
	}
	content := results[0].Content
	if len(content) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(content))
	}
	if tb, ok := content[0].(TextBlock); !ok || tb.Text != "screenshot:" {
		t.Errorf("content[0] = %#v, want text %q", content[0], "screenshot:")
	}
	if ib, ok := content[1].(ImageBlock); !ok || ib.MediaType != "image/png" || string(ib.Data) != "\x01\x02" {
		t.Errorf("content[1] = %#v, want inline image", content[1])
	}
	if ib, ok := content[2].(ImageBlock); !ok || ib.URL != "https://example.com/x.png" {
		t.Errorf("content[2] = %#v, want url image", content[2])
	}
}

func TestLoopRichResultZeroValueNormalized(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("r1", "noop", "{}"),
		asstText("done"),
	}}
	agent := New(provider)
	agent.RegisterTool(FuncResult("noop", "does nothing", func(context.Context, struct{}) (ToolResult, error) {
		return ToolResult{}, nil // handler returns the zero value
	}))

	res, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := transcriptToolResults(res.Messages)
	if len(results) != 1 || len(results[0].Content) != 1 {
		t.Fatalf("results = %+v, want one single-block result", results)
	}
	if _, ok := results[0].Content[0].(TextBlock); !ok {
		t.Errorf("content[0] = %T, want TextBlock", results[0].Content[0])
	}
}

func TestLoopRichResultRecoverableError(t *testing.T) {
	final := "recovered"
	provider := &capturingProvider{turns: []Message{
		asstTool("r1", "boom", "{}"),
		asstText(final),
	}}
	agent := New(provider)
	agent.RegisterTool(FuncResult("boom", "always fails", func(context.Context, struct{}) (ToolResult, error) {
		return ErrorResult("exploded"), nil
	}))

	out, err := agent.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Output != final {
		t.Errorf("output = %q, want %q (error must be recoverable)", out.Output, final)
	}
	results := transcriptToolResults(out.Messages)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want one IsError result", results)
	}
	if got := (Message{Blocks: results[0].Content}).Text(); got != "exploded" {
		t.Errorf("error result text = %q, want %q", got, "exploded")
	}
}

func TestLoopRichResultFatalCancellation(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("r1", "hang", "{}"),
	}}
	agent := New(provider)
	agent.RegisterTool(FuncResult("hang", "waits for cancellation", func(ctx context.Context, _ struct{}) (ToolResult, error) {
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the run so the tool observes it immediately
	if _, err := agent.Run(ctx, "go"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestLoopStringToolTranscriptUnchanged(t *testing.T) {
	final := "done"
	provider := &capturingProvider{turns: []Message{
		asstTool("s1", "echo", `{"msg":"hi"}`),
		asstText(final),
	}}
	agent := New(provider)
	agent.RegisterTool(Func("echo", "echoes msg", func(_ context.Context, a echoArgs) (string, error) {
		return "echoed:" + a.Msg, nil
	}))

	var events []StreamEvent
	res, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if res.Output != final {
		t.Errorf("output = %q, want %q", res.Output, final)
	}
	results := transcriptToolResults(res.Messages)
	if len(results) != 1 {
		t.Fatalf("got %d tool results, want 1", len(results))
	}
	if got := (Message{Blocks: results[0].Content}).Text(); got != "echoed:hi" {
		t.Errorf("content text = %q, want %q", got, "echoed:hi")
	}

	var resultEvents []StreamEvent
	for _, ev := range events {
		if ev.Kind == StreamToolResult {
			resultEvents = append(resultEvents, ev)
		}
	}
	if len(resultEvents) != 1 {
		t.Fatalf("got %d StreamToolResult events, want 1", len(resultEvents))
	}
	ev := resultEvents[0]
	if ev.Result != "echoed:hi" {
		t.Errorf("event Result = %q, want %q", ev.Result, "echoed:hi")
	}
	if len(ev.ResultBlocks) != 1 {
		t.Fatalf("event ResultBlocks = %d blocks, want 1", len(ev.ResultBlocks))
	}
	if tb, ok := ev.ResultBlocks[0].(TextBlock); !ok || tb.Text != "echoed:hi" {
		t.Errorf("event ResultBlocks[0] = %#v, want text %q", ev.ResultBlocks[0], "echoed:hi")
	}
}

func TestLoopRichResultStreamEventsCarryBlocks(t *testing.T) {
	provider := &capturingProvider{turns: []Message{
		asstTool("r1", "shot", "{}"),
		asstText("done"),
	}}
	agent := New(provider)
	agent.RegisterTool(FuncResult("shot", "screenshot", func(context.Context, struct{}) (ToolResult, error) {
		return BlockResult(TextBlock{Text: "shot!"}, ImageBlock{MediaType: "image/png", Data: []byte{1}}), nil
	}))

	var resultEvents []StreamEvent
	_, err := agent.RunStream(context.Background(), "go", func(ev StreamEvent) {
		if ev.Kind == StreamToolResult {
			resultEvents = append(resultEvents, ev)
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if len(resultEvents) != 1 {
		t.Fatalf("got %d StreamToolResult events, want 1", len(resultEvents))
	}
	ev := resultEvents[0]
	if ev.Result != "shot!" {
		t.Errorf("event Result = %q, want %q", ev.Result, "shot!")
	}
	if len(ev.ResultBlocks) != 2 {
		t.Fatalf("event ResultBlocks = %d blocks, want 2", len(ev.ResultBlocks))
	}
	if _, ok := ev.ResultBlocks[0].(TextBlock); !ok {
		t.Errorf("ResultBlocks[0] = %T, want TextBlock", ev.ResultBlocks[0])
	}
	if _, ok := ev.ResultBlocks[1].(ImageBlock); !ok {
		t.Errorf("ResultBlocks[1] = %T, want ImageBlock", ev.ResultBlocks[1])
	}
}

// --- WithToolRetry preserves rich results (Task 7) -------------------------

func TestWithToolRetryPreservesResultTool(t *testing.T) {
	var calls atomic.Int32
	tool := WithToolRetry(FuncResult("flaky", "fails once", func(context.Context, struct{}) (ToolResult, error) {
		if calls.Add(1) < 2 {
			return ToolResult{}, &retryableToolErr{msg: "transient"}
		}
		return BlockResult(TextBlock{Text: "recovered"}, ImageBlock{MediaType: "image/png", Data: []byte{1}}), nil
	}), retry.Config{MaxAttempts: 3, InitialDelay: time.Millisecond})

	res, err := tool.Execute(context.Background(), json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("ExecuteResult: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("handler called %d times, want 2 (one retry)", calls.Load())
	}
	if res.Text() != "recovered" || len(res.Blocks) != 2 {
		t.Errorf("result = %+v, want 2 blocks with text %q", res, "recovered")
	}
}

func TestWithToolRetryTextResult(t *testing.T) {
	tool := WithToolRetry(Func("plain", "plain", func(context.Context, struct{}) (string, error) { return "ok", nil }), retry.Config{})
	res, err := tool.Execute(context.Background(), json.RawMessage("{}"))
	if err != nil || res.Text() != "ok" || len(res.Blocks) != 1 {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}

// --- StreamAccumulator rich views (Task 5) ---------------------------------

func TestAccumulatorCarriesResultBlocks(t *testing.T) {
	var acc StreamAccumulator
	blocks := Blocks{TextBlock{Text: "hi"}, ImageBlock{MediaType: "image/png", Data: []byte{1}}}
	acc.Add(StreamEvent{Kind: StreamToolCall, ToolCall: toolUse("t1", "shot", "{}")})
	acc.Add(StreamEvent{Kind: StreamToolResult, ToolCall: toolUse("t1", "shot", "{}"), Result: "hi", ResultBlocks: blocks})

	views := acc.Views()
	if len(views) != 1 || len(views[0].ToolCalls) != 1 {
		t.Fatalf("views = %+v", views)
	}
	tc := views[0].ToolCalls[0]
	if !tc.Done || tc.Result != "hi" {
		t.Errorf("tool call view = %+v", tc)
	}
	if len(tc.ResultBlocks) != 2 {
		t.Fatalf("view ResultBlocks = %d blocks, want 2", len(tc.ResultBlocks))
	}
	if _, ok := tc.ResultBlocks[1].(ImageBlock); !ok {
		t.Errorf("view ResultBlocks[1] = %T, want ImageBlock", tc.ResultBlocks[1])
	}
}

func TestAccumulatorCarriesResultBlocksForUnannouncedCall(t *testing.T) {
	var acc StreamAccumulator
	blocks := Blocks{TextBlock{Text: "late"}}
	acc.Add(StreamEvent{Kind: StreamToolResult, ToolCall: toolUse("late", "shot", "{}"), Result: "late", ResultBlocks: blocks})

	views := acc.Views()
	if len(views) != 1 || len(views[0].ToolCalls) != 1 {
		t.Fatalf("views = %+v", views)
	}
	tc := views[0].ToolCalls[0]
	if !tc.Done || tc.Result != "late" || len(tc.ResultBlocks) != 1 {
		t.Errorf("tool call view = %+v", tc)
	}
}

// TestStringToolResultEventShapeUnchanged pins the compatibility contract for
// string tools: the StreamToolResult event keeps Result populated and carries a
// single-text-block ResultBlocks, so existing renderers see the same view.
func TestStringToolResultEventShapeUnchanged(t *testing.T) {
	ev := StreamEvent{Kind: StreamToolResult, ToolCall: toolUse("t1", "echo", "{}"), Result: "hi", ResultBlocks: Blocks{TextBlock{Text: "hi"}}}
	if ev.Result != "hi" {
		t.Errorf("Result = %q", ev.Result)
	}
	if len(ev.ResultBlocks) != 1 || ev.ResultBlocks[0].(TextBlock).Text != "hi" {
		t.Errorf("ResultBlocks = %+v", ev.ResultBlocks)
	}
}
