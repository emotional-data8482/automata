package core_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
)

// Exercise the same host-side helper used by the README's compiled recipe
// against real admission records, rather than only mocking sentinel errors.
func TestScheduleCatchUpPastExistingSlots(t *testing.T) {
	for _, scenario := range []string{"revision change", "retention", "unexpected task drift"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			runtime, err := core.NewEphemeralRuntime()
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			old, err := runtime.Register("nightly-report", "v1", must(core.New(script(say("Old report."), say("Next report.")), core.AgentConfig{})))
			if err != nil {
				t.Fatal(err)
			}
			first := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
			last := first.AddDate(0, 0, 1)
			task := func(day time.Time) string { return "Send the report for " + day.UTC().Format("2006-01-02") + "." }
			key := func(day time.Time) core.SubmitOption {
				return core.WithIdempotencyKey("schedule/nightly-report", day.UTC().Format("2006-01-02"))
			}
			seedTask := task(first)
			if scenario == "unexpected task drift" {
				seedTask = "Different task for the same slot."
			}
			original, err := runtime.Run(ctx, old, seedTask, key(first), core.WithLabels(map[string]string{"slot": first.Format("2006-01-02")}))
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "retention" {
				report, err := runtime.Prune(ctx, core.RetentionPolicy{Runs: time.Nanosecond})
				if err != nil || report.Runs != 1 {
					t.Fatalf("prune = %+v, %v; want one pruned run", report, err)
				}
			}
			current := old
			if scenario == "revision change" {
				current, err = runtime.Register("nightly-report", "v2", must(core.New(script(say("New report.")), core.AgentConfig{})))
				if err != nil {
					t.Fatal(err)
				}
			}
			var attempted []time.Time
			var admitted []*core.RunHandle
			submitSlot := func(day time.Time) (*core.RunHandle, error) {
				attempted = append(attempted, day)
				handle, err := runtime.Submit(ctx, current, task(day), key(day), core.WithLabels(map[string]string{"slot": day.UTC().Format("2006-01-02")}))
				if err == nil {
					admitted = append(admitted, handle)
				}
				return handle, err
			}
			var conflicts []time.Time
			onConflict := func(day time.Time, err error) {
				if !errors.Is(err, core.ErrAdmissionConflict) {
					t.Errorf("reported conflict = %v", err)
				}
				conflicts = append(conflicts, day)
			}
			if err := catchUpSlots(first, last, submitSlot, onConflict); err != nil {
				t.Fatal(err)
			}
			if len(attempted) != 2 || !attempted[0].Equal(first) || !attempted[1].Equal(last) {
				t.Fatalf("attempted slots = %v, want both days", attempted)
			}
			wantConflicts := 1
			if scenario == "retention" {
				wantConflicts = 0
			}
			if len(conflicts) != wantConflicts || (wantConflicts == 1 && !conflicts[0].Equal(first)) {
				t.Fatalf("reported conflicts = %v, want %d for first slot", conflicts, wantConflicts)
			}
			if len(admitted) != 1 || admitted[0].ID() == original.RunID {
				t.Fatalf("admitted runs = %v, want only a new run for the missing slot", admitted)
			}
			if _, err := admitted[0].Await(ctx); err != nil {
				t.Fatalf("missing slot failed to run: %v", err)
			}
			page, err := runtime.Runs(ctx, core.RunFilter{Scope: "schedule/nightly-report", Labels: map[string]string{"slot": first.Format("2006-01-02")}, RootsOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			wantRuns := 1
			if scenario == "retention" {
				wantRuns = 0
			}
			if len(page.Runs) != wantRuns || (wantRuns == 1 && page.Runs[0].RunID != original.RunID) {
				t.Fatalf("existing slot listing = %+v, want original identity preserved", page)
			}
		})
	}
}

func TestScheduleCatchUpStopsOnOtherErrors(t *testing.T) {
	first := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	failure := errors.New("store unavailable")
	calls := 0
	err := catchUpSlots(first, first.AddDate(0, 0, 1), func(time.Time) (*core.RunHandle, error) {
		calls++
		return nil, fmt.Errorf("admission failed: %w", failure)
	}, func(time.Time, error) { t.Error("ordinary failure reported as a conflict") })
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("catch-up = %v, calls = %d; want original error after one attempt", err, calls)
	}
}
