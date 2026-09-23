package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// The examples run against scripted providers so they are deterministic and
// need no credentials. In an application, provider is a real adapter such as
// claude.New(model, apiKey) or openai.New(model, baseURL). The text between
// the "README" markers is quoted verbatim in the repository README, and
// TestReadmeExamplesMatch fails when the two drift apart.

// scripted replays assistant messages in order.
type scripted struct {
	mu      sync.Mutex
	replies []core.Message
}

func script(replies ...core.Message) *scripted { return &scripted{replies: replies} }

func (p *scripted) Invoke(context.Context, core.Request) (core.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.replies) == 0 {
		return core.Response{}, errors.New("script exhausted")
	}
	reply := p.replies[0]
	p.replies = p.replies[1:]
	stop := core.StopEndTurn
	if len(reply.ToolUses()) > 0 {
		stop = core.StopToolUse
	}
	return core.Response{Message: reply, StopReason: stop}, nil
}

func say(text string) core.Message { return core.AssistantMessage(core.TextBlock{Text: text}) }

func call(id, tool, input string) core.Message {
	return core.AssistantMessage(core.ToolUseBlock{ID: id, Name: tool, Input: json.RawMessage(input)})
}

// Runs log through slog.Default; keep the test output readable.
func init() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) }

type WeatherArgs struct {
	City string `json:"city" desc:"city to look up"`
}

// A Runtime runs every Agent. NewEphemeralRuntime keeps runs in memory.
func ExampleRuntime() {
	provider := script(call("w1", "weather", `{"city":"Paris"}`), say("It is sunny in Paris."))
	// README: quickstart
	runtime, err := core.NewEphemeralRuntime()
	if err != nil {
		panic(err)
	}
	defer runtime.Close()

	agent, err := core.New(provider, core.AgentConfig{
		SystemPrompt: "You are a concise assistant.",
		MaxTurns:     5,
		Tools: []core.Tool{core.Func("weather", "Get the weather for a city",
			func(ctx context.Context, in WeatherArgs) (string, error) {
				return "sunny in " + in.City, nil
			})},
	})
	if err != nil {
		panic(err)
	}
	assistant, err := runtime.Register("assistant", "v1", agent)
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := runtime.Run(ctx, assistant, "What's the weather in Paris?")
	if err != nil {
		// result still holds the transcript, usage, and turns so far.
		panic(fmt.Sprintf("failed after %d turns: %v", result.Turns, err))
	}
	fmt.Println(result.Output)
	fmt.Println(result.Turns, "turns")
	// README end

	// Output:
	// It is sunny in Paris.
	// 2 turns
}

// A persistent runtime admits work before running it and resumes after a
// restart. An exact retry of an idempotency key returns the original run.
func ExampleNewRuntime() {
	ctx := context.Background()
	agent, err := core.New(script(say("Summary: all good.")), core.AgentConfig{})
	if err != nil {
		panic(err)
	}
	// README: persistent
	// In production: store, err := sqlite.Open(ctx, "automata.db")
	store := core.NewMemoryStore()
	runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{Store: store})
	if err != nil {
		panic(err)
	}
	defer runtime.Close()

	// Register every definition stored runs may pin, then adopt their work.
	summarizer, err := runtime.Register("summarizer", "2026-09-23", agent)
	if err != nil {
		panic(err)
	}
	if err := runtime.Recover(ctx); err != nil {
		panic(err)
	}

	// The external task ID makes admission idempotent. An exact retry repeats
	// the same task and options, deadline included.
	admission := []core.SubmitOption{
		core.WithIdempotencyKey("tickets", "42"),
		core.WithDeadline(time.Now().Add(10 * time.Minute)),
	}
	handle, err := runtime.Submit(ctx, summarizer, "Summarize ticket 42", admission...)
	if err != nil {
		panic(err)
	}
	result, err := handle.Await(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Output)

	retry, err := runtime.Submit(ctx, summarizer, "Summarize ticket 42", admission...)
	if err != nil {
		panic(err)
	}
	fmt.Println(retry.ID() == handle.ID())
	// README end

	// Output:
	// Summary: all good.
	// true
}

type Person struct {
	Name string `json:"name"`
	Age  int    `json:"age" desc:"age in years"`
}

// A definition can require validated structured output; Decode returns it
// as a Go value.
func ExampleDecode() {
	ctx := context.Background()
	runtime, _ := core.NewEphemeralRuntime()
	defer runtime.Close()
	// The first answer omits a required field; the run corrects it in place.
	provider := script(say(`{"name":"Ada Lovelace"}`), say(`{"name":"Ada Lovelace","age":36}`))
	// README: typed
	agent, err := core.New(provider, core.AgentConfig{
		StructuredOutput: &core.StructuredOutputConfig{
			Schema:         core.OutputSchema[Person](),
			MaxCorrections: 1,
		},
	})
	if err != nil {
		panic(err)
	}
	biographer, err := runtime.Register("biographer", "v1", agent)
	if err != nil {
		panic(err)
	}
	result, err := runtime.Run(ctx, biographer, "Who wrote the first program?")
	if err != nil {
		panic(err)
	}
	person, err := core.Decode[Person](result)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s, %d (%d turns)\n", person.Name, person.Age, result.Turns)
	// README end

	// Output:
	// Ada Lovelace, 36 (2 turns)
}

type ResearchRequest struct {
	Topic string `json:"topic" desc:"the subtopic to research"`
}

// A ChildTool delegates to another registered definition. Each call is a
// durable child run; its live events reach the parent's view tagged with the
// tool name.
func ExampleChildTool() {
	ctx := context.Background()
	runtime, _ := core.NewEphemeralRuntime()
	defer runtime.Close()
	researcherProvider := script(say("Tides follow the moon."))
	leadProvider := script(call("r1", "research", `{"topic":"tides"}`), say("Report: tides follow the moon."))
	// README: children
	researcher, err := runtime.Register("researcher", "v1", must(core.New(researcherProvider, core.AgentConfig{
		SystemPrompt: `You receive {"topic": ...}. Research it and reply with notes.`,
	})))
	if err != nil {
		panic(err)
	}
	lead, err := runtime.Register("lead", "v1", must(core.New(leadProvider, core.AgentConfig{
		Tools: []core.Tool{
			core.ChildTool[ResearchRequest]("research", "Research one topic.", researcher),
		},
		// Call caps are shared by the whole run tree.
		ToolPolicy: core.ToolPolicy{MaxCalls: 10},
	})))
	if err != nil {
		panic(err)
	}

	var views core.StreamAccumulator
	result, err := runtime.RunStream(ctx, lead, "Write a report on tides.", views.Add)
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Output)
	for _, view := range views.Views() {
		fmt.Printf("%q (call %q): %s\n", view.Agent, view.InvocationID, view.Text)
	}
	// README end

	// Output:
	// Report: tides follow the moon.
	// "" (call ""): Report: tides follow the moon.
	// "research" (call "r1"): Tides follow the moon.
}

// Conversation turns are serialized runs that continue the committed
// history of the previous turn.
func ExampleWithConversation() {
	ctx := context.Background()
	runtime, _ := core.NewEphemeralRuntime()
	defer runtime.Close()
	provider := script(say("Draft: refunds within 14 days."), say("Draft: refunds within 30 days, no questions asked."))
	// README: conversation
	writer, err := runtime.Register("writer", "v1", must(core.New(provider, core.AgentConfig{})))
	if err != nil {
		panic(err)
	}
	thread := core.ConversationRef{Scope: "tenant-1", ID: "refund-policy"}
	first, err := runtime.Run(ctx, writer, "Draft a refund policy.", core.WithConversation(thread, ""))
	if err != nil {
		panic(err)
	}
	// The next turn names the head it continues; a stale or concurrent turn
	// is rejected rather than merged.
	second, err := runtime.Run(ctx, writer, "Make it 30 days.", core.WithConversation(thread, first.RunID))
	if err != nil {
		panic(err)
	}
	fmt.Println(second.Output)
	fmt.Println(len(second.Messages), "messages in the conversation")
	// README end

	// Output:
	// Draft: refunds within 30 days, no questions asked.
	// 4 messages in the conversation
}

type RefundInput struct {
	Order  string `json:"order"`
	Amount int    `json:"amount"`
}

// An approval suspends the run without holding a worker. The host answers
// later, possibly from another process after a restart.
func ExampleWithDurableWait() {
	ctx := context.Background()
	provider := script(call("c1", "refund", `{"order":"A-7","amount":40}`), say("Refunded A-7."))
	// README: approval
	refund := core.WithDurableWait(
		core.Func("refund", "Refund an order.", func(ctx context.Context, in RefundInput) (string, error) {
			return fmt.Sprintf("refunded %d to %s", in.Amount, in.Order), nil
		}),
		core.DurableWaitPolicy{
			Kind:   core.WaitApproval,
			Target: func(raw json.RawMessage) (string, error) { return string(raw), nil },
		})
	runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{
		Store: core.NewMemoryStore(),
		// Consulted when an approval is accepted and again before dispatch.
		Authorizer: core.ApprovalAuthorizerFunc(func(ctx context.Context, check core.ApprovalAuthorization) error {
			if check.Actor != "support-lead" {
				return errors.New("not allowed")
			}
			return nil
		}),
	})
	if err != nil {
		panic(err)
	}
	defer runtime.Close()
	support, err := runtime.Register("support", "v1", must(core.New(provider, core.AgentConfig{Tools: []core.Tool{refund}})))
	if err != nil {
		panic(err)
	}

	handle, err := runtime.Submit(ctx, support, "Refund order A-7.")
	if err != nil {
		panic(err)
	}
	wait := pendingWait(ctx, handle)
	fmt.Println("approve?", wait.Tool, wait.Target)
	err = handle.ResolveWait(ctx, wait.ID, core.WaitResolution{
		Decision:     core.Allow,
		Actor:        "support-lead",
		ActionDigest: wait.ActionDigest, // binds the approval to this exact call
	})
	if err != nil {
		panic(err)
	}
	result, err := handle.Await(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Output)
	// README end

	// Output:
	// approve? refund {"order":"A-7","amount":40}
	// Refunded A-7.
}

// pendingWait waits for the run to suspend and returns its pending wait.
func pendingWait(ctx context.Context, handle *core.RunHandle) core.WaitSnapshot {
	var cursor uint64
	for {
		snapshot, err := handle.Snapshot(ctx)
		if err != nil {
			panic(err)
		}
		for _, wait := range snapshot.Waits {
			if wait.State == core.WaitPending {
				return wait
			}
		}
		page, err := handle.WaitEvents(ctx, cursor, 64)
		if err != nil {
			panic(err)
		}
		cursor = page.Next
	}
}

// Committed events are pulled from a cursor, so an observer can disconnect,
// restart, and resume without missing a fact.
func ExampleRunHandle_Events() {
	ctx := context.Background()
	runtime, _ := core.NewEphemeralRuntime()
	defer runtime.Close()
	assistant, _ := runtime.Register("assistant", "v1", must(core.New(script(say("done")), core.AgentConfig{})))
	handle, _ := runtime.Submit(ctx, assistant, "work")
	// README: events
	var cursor uint64 // persist this to resume after a restart
	for {
		page, err := handle.WaitEvents(ctx, cursor, 256)
		if errors.Is(err, core.ErrEventGap) {
			// Retention removed events behind the cursor: resynchronize.
			snapshot, err := handle.Snapshot(ctx)
			if err != nil {
				panic(err)
			}
			cursor = snapshot.EventSequence
			continue
		}
		if err != nil {
			panic(err)
		}
		for _, event := range page.Events {
			if event.Kind == core.CommittedRunState {
				fmt.Println("state:", event.State)
			}
		}
		cursor = page.Next
		if last := page.Events[len(page.Events)-1]; last.Kind == core.CommittedRunState && last.State == core.RuntimeTerminal {
			break
		}
	}
	// README end

	// Output:
	// state: ready
	// state: running
	// state: finalizing
	// state: terminal
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
