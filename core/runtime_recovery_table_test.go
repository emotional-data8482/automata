package core

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestClassifyRecoveryBoundaries(t *testing.T) {
	// Each transition row is exercised in running, execution-attention, and
	// cancellation states, with every batch inspection/uncertainty combination.
	rows := []struct {
		kind      transitionKind
		running   recoveryAction
		attention recoveryAction
	}{
		{transitionNone, recoveryResume, recoveryResume},
		{transitionProviderAttemptStarted, recoveryProviderAttention, recoveryLeave},
		{transitionProviderAttemptRecovered, recoveryResume, recoveryResume},
		{transitionProviderAccepted, recoveryExecutionAttention, recoveryLeave},
		{transitionResponseClassified, recoveryResume, recoveryResume},
		{transitionBatchReady, recoveryResume, recoveryResume},
		{transitionBatchCommitted, recoveryResume, recoveryResume},
		{"unknown", recoveryExecutionAttention, recoveryLeave},
	}
	for _, row := range rows {
		for _, state := range []RuntimeState{RuntimeRunning, RuntimeNeedsAttention, RuntimeCancelRequested} {
			for _, batch := range []bool{false, true} {
				for _, inspected := range []bool{false, true} {
					for _, uncertain := range []bool{false, true} {
						name := fmt.Sprintf("%s/%s/batch=%t/inspected=%t/uncertain=%t", state, row.kind, batch, inspected, uncertain)
						t.Run(name, func(t *testing.T) {
							record := storedRuntimeRun{State: state, LastTransition: row.kind, AttentionKind: AttentionExecution}
							if batch {
								record.PendingBatchID = "batch"
							}
							facts := recoveryFacts{payloadFits: true, batchInspected: inspected, batchUncertain: uncertain}
							want := row.running
							if state == RuntimeNeedsAttention {
								want = row.attention
							}
							if state == RuntimeCancelRequested {
								want = recoveryCancel
							}
							if batch {
								switch {
								case !inspected:
									want = recoveryInspectBatch
								case state == RuntimeCancelRequested:
									want = recoveryCancel
								case uncertain:
									want = recoveryExecutionAttention
								default:
									want = recoveryResume
								}
							}
							assertRecoveryAction(t, record, facts, want)
						})
					}
				}
			}
		}
	}
}

func TestClassifyRecoveryMarkers(t *testing.T) {
	rows := []struct {
		name   string
		record storedRuntimeRun
		facts  recoveryFacts
		want   recoveryAction
	}{
		{"ready missing binding", storedRuntimeRun{State: RuntimeReady}, recoveryFacts{}, recoveryLeave},
		{"ready registered", storedRuntimeRun{State: RuntimeReady}, recoveryFacts{bindingRegistered: true}, recoveryStart},
		{"terminal", storedRuntimeRun{State: RuntimeTerminal}, recoveryFacts{}, recoveryLeave},
		{"unknown state", storedRuntimeRun{State: "unknown"}, recoveryFacts{}, recoveryLeave},
		{"finalizing before hook marker", storedRuntimeRun{State: RuntimeFinalizing}, recoveryFacts{}, recoveryFinishHooks},
		{"finalizing after hook marker", storedRuntimeRun{State: RuntimeFinalizing, HookDelivery: []string{"audit"}}, recoveryFacts{}, recoveryHookAttention},
		{"hook attention", storedRuntimeRun{State: RuntimeNeedsAttention, AttentionKind: AttentionHooks}, recoveryFacts{payloadFits: true}, recoveryLeave},
		{"unknown attention", storedRuntimeRun{State: RuntimeNeedsAttention, AttentionKind: "unknown"}, recoveryFacts{payloadFits: true}, recoveryLeave},
		{"blocked payload", storedRuntimeRun{State: RuntimeNeedsAttention, AttentionKind: AttentionExecution, PendingBatchID: "batch"}, recoveryFacts{}, recoveryLeave},
		{"accepted prose", storedRuntimeRun{State: RuntimeRunning, LastTransition: transitionProviderAccepted}, recoveryFacts{acceptedFinal: true}, recoveryResume},
		{"accepted attention is conservative", storedRuntimeRun{State: RuntimeNeedsAttention, AttentionKind: AttentionExecution, LastTransition: transitionProviderAccepted}, recoveryFacts{acceptedFinal: true, payloadFits: true}, recoveryLeave},
		{"fresh interrupted attempt", storedRuntimeRun{State: RuntimeRunning, LastTransition: transitionProviderAttemptStarted}, recoveryFacts{maxFreshAttempts: 1}, recoveryProviderFresh},
		{"exhausted fresh budget", storedRuntimeRun{State: RuntimeRunning, LastTransition: transitionProviderAttemptStarted, FreshAttempts: 1}, recoveryFacts{maxFreshAttempts: 1}, recoveryProviderAttention},
		{"raised provider budget", storedRuntimeRun{State: RuntimeNeedsAttention, AttentionKind: AttentionProvider, FreshAttempts: 1}, recoveryFacts{maxFreshAttempts: 2}, recoveryProviderFresh},
		{"unchanged provider budget", storedRuntimeRun{State: RuntimeNeedsAttention, AttentionKind: AttentionProvider, FreshAttempts: 1}, recoveryFacts{maxFreshAttempts: 1}, recoveryLeave},
		{"cancel open attempt", storedRuntimeRun{State: RuntimeCancelRequested, LastTransition: transitionProviderAttemptStarted}, recoveryFacts{maxFreshAttempts: 1}, recoveryCancel},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) { assertRecoveryAction(t, row.record, row.facts, row.want) })
	}
}

func TestClassifyRecoverySuspendedPriority(t *testing.T) {
	for _, state := range []RuntimeState{RuntimeWaiting, RuntimeNeedsAttention} {
		record := storedRuntimeRun{State: state, AttentionKind: AttentionChild}
		rows := []struct {
			name  string
			facts recoveryFacts
			want  recoveryAction
		}{
			{"deadline first", recoveryFacts{}, recoveryMaintainSuspended},
			{"deadline wins", recoveryFacts{deadlineChecked: true, deadlineFinalized: true, childrenReady: true, waitsReady: true}, recoveryFinishHooks},
			{"children next", recoveryFacts{deadlineChecked: true}, recoveryInspectChildren},
			{"child continuation", recoveryFacts{deadlineChecked: true, childrenChecked: true, childrenReady: true}, recoveryStart},
		}
		for _, row := range rows {
			t.Run(string(state)+"/"+row.name, func(t *testing.T) { assertRecoveryAction(t, record, row.facts, row.want) })
		}
		facts := recoveryFacts{deadlineChecked: true, childrenChecked: true}
		want := recoveryExpireWaits
		if state == RuntimeNeedsAttention {
			want = recoveryLeave
		}
		assertRecoveryAction(t, record, facts, want)
		facts.waitsChecked = true
		assertRecoveryAction(t, record, facts, recoveryLeave)
		facts.waitsReady = true
		want = recoveryStart
		if state == RuntimeNeedsAttention {
			want = recoveryLeave
		}
		assertRecoveryAction(t, record, facts, want)
	}
}

func assertRecoveryAction(t *testing.T, record storedRuntimeRun, facts recoveryFacts, want recoveryAction) {
	t.Helper()
	before, _ := json.Marshal(record)
	if got := classifyRecovery(record, facts); got != want {
		t.Fatalf("action = %d, want %d", got, want)
	}
	after, _ := json.Marshal(record)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("classifier mutated record")
	}
	facts.liveWorker = true
	if got := classifyRecovery(record, facts); got != recoveryLeave {
		t.Fatalf("live worker action = %d", got)
	}
}

func TestTransitionKindPreservesEncoding(t *testing.T) {
	for _, kind := range []transitionKind{transitionNone, transitionProviderAttemptStarted, transitionProviderAttemptRecovered, transitionProviderAccepted, transitionResponseClassified, transitionBatchReady, transitionBatchCommitted} {
		raw, err := json.Marshal(storedRuntimeRun{Version: runtimeEncodingVersion, LastTransition: kind})
		if err != nil {
			t.Fatal(err)
		}
		var legacy struct {
			Version        int    `json:"version"`
			LastTransition string `json:"last_transition"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			t.Fatal(err)
		}
		if legacy.Version != 9 || legacy.LastTransition != string(kind) {
			t.Fatalf("encoding changed: %s", raw)
		}
		var decoded storedRuntimeRun
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.LastTransition != kind {
			t.Fatalf("round trip = %q, %v", decoded.LastTransition, err)
		}
	}
}
