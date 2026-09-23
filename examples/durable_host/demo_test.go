package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain routes a re-executed test binary into run: the demo starts each
// step as a separate process of its own executable.
func TestMain(m *testing.M) {
	if os.Getenv(childProcessEnv) == "1" {
		if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestDemoSurvivesRestartsAndCrash runs the reference scenario across real
// process exits and a crash in the dispatch window, from a fresh store.
func TestDemoSurvivesRestartsAndCrash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var out bytes.Buffer
	if err := run(ctx, []string{"-dir", t.TempDir(), "demo"}, &out); err != nil {
		t.Fatalf("demo: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"pending approval: Publish quarterly-report.md",
		"reviewer verdict: approve (checked quarterly-report.md)",
		"ticket T-1 already admitted",
		"reconciled incident-summary.md as applied",
		"verified: 2 tickets, 2 destination writes, none repeated",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("demo output lacks %q:\n%s", want, out.String())
		}
	}
}
