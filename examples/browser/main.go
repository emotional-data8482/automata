// Command browser-example lets Claude browse a fixed list of hosts and asks
// you in the terminal before it visits any other host. The approval is a
// Runtime durable approval (browser_request_access): the run pauses until you
// answer y or n.
//
// It needs ANTHROPIC_API_KEY and an installed Chrome or Chromium (set
// AUTOMATA_BROWSER_PATH to pick one). The model defaults to claude-sonnet-4-6
// and can be overridden with ANTHROPIC_MODEL.
//
//	export ANTHROPIC_API_KEY=sk-ant-...
//	go run ./examples/browser
//	go run ./examples/browser -allow example.com -headed "Open example.com and follow its link"
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
	"github.com/emotional-data8482/automata/extensions/claude"
)

const defaultTask = "Open https://example.com, follow its link to learn more about example domains, and tell me the title of the page it leads to."

func main() {
	allow := flag.String("allow", "example.com", "comma-separated hosts the browser may visit without asking")
	headed := flag.Bool("headed", false, "show the browser window")
	flag.Parse()
	task := strings.Join(flag.Args(), " ")
	if task == "" {
		task = defaultTask
	}
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	session, err := browser.New(ctx, browser.Config{
		AllowedHosts:  strings.Split(*allow, ","),
		RequestAccess: true,
		Headed:        *headed,
		BrowserPath:   os.Getenv("AUTOMATA_BROWSER_PATH"),
	})
	if err != nil {
		log.Fatalf("start browser: %v", err)
	}
	defer session.Close()

	agent, err := core.New(claude.New(model, os.Getenv("ANTHROPIC_API_KEY")), core.AgentConfig{
		SystemPrompt: "You operate a web browser through tools. Prefer snapshots over screenshots. If a host is not allowed, ask for access with browser_request_access and explain why.",
		MaxTurns:     20,
		Tools:        session.Tools(),
	})
	if err != nil {
		log.Fatalf("configure agent: %v", err)
	}

	runtime, err := core.NewRuntime(ctx, core.RuntimeConfig{
		Store: core.NewMemoryStore(),
		// Whoever answers at this terminal is the approval authority.
		Authorizer: core.ApprovalAuthorizerFunc(func(context.Context, core.ApprovalAuthorization) error { return nil }),
	})
	if err != nil {
		log.Fatalf("open runtime: %v", err)
	}
	defer runtime.Close()
	def, err := runtime.Register("browser", "v1", agent)
	if err != nil {
		log.Fatalf("register agent: %v", err)
	}

	handle, err := runtime.Submit(ctx, def, task)
	if err != nil {
		log.Fatalf("submit: %v", err)
	}
	if err := follow(ctx, handle, bufio.NewReader(os.Stdin)); err != nil {
		log.Fatal(err)
	}
	res, err := handle.Await(ctx)
	if err != nil {
		log.Fatalf("run failed after %d turns (partial output %q): %v", res.Turns, res.Output, err)
	}
	fmt.Printf("\n%s\n\n[%d turns]\n", res.Output, res.Turns)
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
			return nil
		}
		page, err := handle.WaitEvents(ctx, cursor, 64)
		if err != nil {
			return err
		}
		cursor = page.Next
	}
}
