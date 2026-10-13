// Command browser-example uses OpenRouter to browse a fixed list of hosts and asks
// you in the terminal before it visits any other host. The approval is a
// Runtime durable approval (browser_request_access): the run pauses until you
// answer y or n.
//
// It needs OPENROUTER_API_KEY and an installed Chrome or Chromium (set
// AUTOMATA_BROWSER_PATH to pick one). The model defaults to openai/gpt-5.2
// and can be overridden with OPENROUTER_MODEL. The browser is visible by default.
// Run from examples/browser:
//
//	export OPENROUTER_API_KEY=sk-or-...
//	go run .
//	go run . -headed=false "Search Wikipedia for the Go programming language and summarize the article"
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/browser"
	"github.com/emotional-data8482/automata/extensions/openrouter"
)

const defaultTask = "Visit https://en.wikipedia.org and choose a harmless, interesting topic to learn about. Use Wikipedia's search field and submit your query, then open a matching article if search results appear. Read the article and follow one internal Wikipedia link to a related article. Report the exact search query, the titles and final URLs of both articles you actually visited, and three interesting facts you observed, identifying which article each fact came from. Briefly explain why you chose the topic. If you encounter a CAPTCHA or an access block, follow your captcha instructions rather than bypassing it, and never invent facts."

func main() {
	// run returns only after its deferred runtime and browser cleanup.
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	allow := flag.String("allow", "wikipedia.org,www.wikipedia.org,en.wikipedia.org", "comma-separated hosts the browser may visit without asking")
	headed := flag.Bool("headed", true, "show the browser window (use -headed=false for headless)")
	checkpoint := flag.Bool("checkpoint", true, "offer browser_checkpoint so you can solve CAPTCHAs in the visible browser (needs -headed=true)")
	minutes := flag.Int("minutes", 10, "run deadline in minutes")
	flag.Parse()
	hosts := parseHosts(*allow)
	if len(hosts) == 0 && strings.TrimSpace(*allow) != "" {
		return fmt.Errorf("-allow %q names no usable host", *allow)
	}
	task := strings.Join(flag.Args(), " ")
	if task == "" {
		task = defaultTask
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if apiKey == "" {
		return fmt.Errorf("set OPENROUTER_API_KEY before running this example")
	}
	model := strings.TrimSpace(os.Getenv("OPENROUTER_MODEL"))
	if model == "" {
		model = "openai/gpt-5.2"
	}
	fmt.Printf("Model: %s\nTask: %s\n\n", model, task)

	if *checkpoint && !*headed {
		return fmt.Errorf("-checkpoint needs -headed=true: a CAPTCHA must be solved in a visible browser window")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*minutes)*time.Minute)
	defer cancel()

	session, err := browser.New(ctx, browser.Config{
		AllowedHosts:    hosts,
		RequestAccess:   true,
		Headed:          *headed,
		BrowserPath:     os.Getenv("AUTOMATA_BROWSER_PATH"),
		HumanCheckpoint: *checkpoint,
	})
	if err != nil {
		return fmt.Errorf("start browser: %w", err)
	}
	defer session.Close()

	systemPrompt := "You operate a web browser through tools. Prefer snapshots over screenshots. If a host is not allowed, ask for access with browser_request_access and explain why."
	if *checkpoint {
		systemPrompt += " If a page shows a CAPTCHA or similar challenge: when it is simple text or a question you can read from a screenshot, answer it through browser_act; otherwise call browser_checkpoint with a short note describing the challenge and wait for the user to solve it in the browser window. Never try to bypass a challenge."
	}
	agent, err := core.New(openrouter.New(model, openrouter.WithAPIKey(apiKey)), core.AgentConfig{
		SystemPrompt: systemPrompt,
		MaxTurns:     50,
		Tools:        session.Tools(),
		// Keep browser actions sequential and bound the total tool requests.
		ToolPolicy: core.ToolPolicy{MaxCalls: 40, MaxParallel: 1},
	})
	if err != nil {
		return fmt.Errorf("configure agent: %w", err)
	}

	runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{
		Store: core.NewMemoryStore(),
		// Whoever answers at this terminal is the approval authority.
		Authorizer: core.ApprovalAuthorizerFunc(func(context.Context, core.ApprovalAuthorization) error { return nil }),
	})
	if err != nil {
		return fmt.Errorf("open runtime: %w", err)
	}
	defer runtime.Close()
	def, err := runtime.Register("browser", "v1", agent)
	if err != nil {
		return fmt.Errorf("register agent: %w", err)
	}

	deadline, _ := ctx.Deadline()
	handle, err := runtime.Submit(ctx, def, task, core.WithDeadline(deadline))
	if err != nil {
		return fmt.Errorf("submit: %w", err)
	}
	if err := follow(ctx, handle, bufio.NewReader(os.Stdin)); err != nil {
		return fmt.Errorf("follow run: %w", err)
	}
	res, err := handle.Await(ctx)
	if err != nil {
		return fmt.Errorf("run failed after %d turns (partial output %q): %w", res.Turns, res.Output, err)
	}
	fmt.Printf("\n%s\n\n[%d turns]\n", res.Output, res.Turns)
	return nil
}

// parseHosts splits a comma-separated host list, trimming whitespace and
// dropping empty entries so a trailing comma is not an error. An empty
// result ("-allow ”") is valid: the browser then refuses every navigation.
func parseHosts(list string) []string {
	var hosts []string
	for _, h := range strings.Split(list, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// follow prints each tool call and asks at the terminal for every pending
// approval until the run settles.
func follow(ctx context.Context, handle *core.RunHandle, in *bufio.Reader) error {
	seenCalls, answered := map[string]bool{}, map[string]bool{}
	var cursor uint64
	for {
		snap, err := handle.Snapshot(ctx)
		if err != nil {
			return err
		}
		for _, batch := range snap.ToolBatches {
			for _, inv := range batch.Invocations {
				if !seenCalls[inv.OperationID] {
					seenCalls[inv.OperationID] = true
					fmt.Printf("\033[2m→ %s %s\033[0m\n", inv.Call.Name, inv.Call.Input)
				}
			}
		}
		for _, wait := range snap.Waits {
			if wait.State != core.WaitPending || answered[wait.ID] {
				continue
			}
			answered[wait.ID] = true
			fmt.Printf("\n%s [y/N] ", wait.Prompt)
			line, _ := in.ReadString('\n')
			resolution := core.WaitResolution{Decision: core.Deny, Reason: "the user said no", Actor: "terminal", ActionDigest: wait.ActionDigest}
			if answer := strings.ToLower(strings.TrimSpace(line)); answer == "y" || answer == "yes" {
				resolution.Decision, resolution.Reason = core.Allow, ""
			}
			if err := handle.ResolveWait(ctx, wait.ID, resolution); err != nil {
				return fmt.Errorf("answer approval: %w", err)
			}
		}
		if snap.State == core.RuntimeTerminal || snap.State == core.RuntimeNeedsAttention {
			if snap.State == core.RuntimeNeedsAttention {
				// The run stopped outside the ordinary flow; say why instead of
				// leaving a quiet terminal.
				fmt.Println("\nthe run stopped and needs attention; unresolved prompts below, the run result follows")
				for _, wait := range snap.Waits {
					if wait.State == core.WaitPending {
						fmt.Printf("  pending: %s\n", wait.Prompt)
					}
				}
			}
			return nil
		}
		page, err := handle.WaitEvents(ctx, cursor, 64)
		if err != nil {
			return err
		}
		cursor = page.Next
	}
}
