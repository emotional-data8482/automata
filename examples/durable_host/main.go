// Command durable_host is a small platform host built on core.Runtime and the
// SQLite store. It maps external work items ("tickets") to durable runs and
// drives them from separate, short-lived processes, the way a web or queue
// worker would. It needs no credentials: the models are deterministic fakes.
//
// A publisher agent writes an artifact through an approval-gated, mutating
// tool, then asks a reviewer child definition for a typed verdict. Every
// command opens the same store, registers the same definition revisions,
// recovers, acts, and exits:
//
//	durable_host -dir state submit T-1 "Quarterly report"   # stops at approval
//	durable_host -dir state watch T-1                       # committed events from a saved cursor
//	durable_host -dir state approve T-1                     # a later process approves and finishes
//	durable_host -dir state result T-1                      # output, child verdict, write count
//	durable_host -dir state reconcile T-1                   # after a crash mid-write
//
// The demo command runs the full scenario, each step in its own process,
// including a crash between the external write and its commit:
//
//	go run ./examples/durable_host demo
//
// Platform seams it exercises: the ticket ID is the admission identity
// (WithIdempotencyKey), the host keeps its own ticket → run table, commands
// resolve waits and reconcile effects without holding a worker, observers
// resume from committed cursors, and the mutating tool hands the durable
// operation ID to its destination as an idempotency key.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run dispatches one command. Every command except demo runs in the calling
// process against the state directory.
func run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("durable_host", flag.ContinueOnError)
	dir := flags.String("dir", "", "state directory (demo defaults to a temporary one)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	command, rest := flags.Arg(0), flags.Args()
	if len(rest) > 0 {
		rest = rest[1:]
	}
	if command == "demo" {
		return demo(ctx, *dir, out)
	}
	if *dir == "" {
		return errors.New("-dir is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	h, err := openHost(ctx, *dir)
	if err != nil {
		return err
	}
	defer h.Close()

	need := func(n int) error {
		if len(rest) != n {
			return fmt.Errorf("%s expects %d argument(s)", command, n)
		}
		return nil
	}
	switch command {
	case "submit":
		if err := need(2); err != nil {
			return err
		}
		return h.submit(ctx, out, rest[0], rest[1])
	case "approve":
		if err := need(1); err != nil {
			return err
		}
		return h.approve(ctx, out, rest[0])
	case "reconcile":
		if err := need(1); err != nil {
			return err
		}
		return h.reconcile(ctx, out, rest[0])
	case "watch":
		if err := need(1); err != nil {
			return err
		}
		return h.watch(ctx, out, rest[0])
	case "result":
		if err := need(1); err != nil {
			return err
		}
		return h.result(ctx, out, rest[0])
	default:
		return fmt.Errorf("unknown command %q (want submit, approve, reconcile, watch, result, or demo)", command)
	}
}
