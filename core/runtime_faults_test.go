package core

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
)

// writeFaultStore fails the writable transaction that performs a matching
// write, after letting skip earlier matches commit. It names the fault
// boundary by what the transaction commits rather than by its ordinal among
// all writable transactions, so a new write elsewhere in the lifecycle does
// not move the fault.
type writeFaultStore struct {
	Store
	match func(bucket string, value []byte) bool
	skip  int32
	err   error
	seen  atomic.Int32
	fired atomic.Bool
}

type writeFaultTx struct {
	StoreTransaction
	store *writeFaultStore
}

func (s *writeFaultStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	if !writable || s.fired.Load() {
		return s.Store.Transaction(ctx, writable, fn)
	}
	return s.Store.Transaction(ctx, true, func(tx StoreTransaction) error {
		return fn(writeFaultTx{StoreTransaction: tx, store: s})
	})
}

func (tx writeFaultTx) Put(bucket, key string, value []byte) error {
	if !tx.store.fired.Load() && tx.store.match(bucket, value) && tx.store.seen.Add(1) > tx.store.skip {
		tx.store.fired.Store(true)
		if tx.store.err != nil {
			return tx.store.err
		}
		return errors.New("injected durable transition failure")
	}
	return tx.StoreTransaction.Put(bucket, key, value)
}

// runWrite matches a run record write that satisfies match.
func runWrite(match func(storedRuntimeRun) bool) func(string, []byte) bool {
	return func(bucket string, value []byte) bool {
		if bucket != runtimeRunsBucket {
			return false
		}
		var record storedRuntimeRun
		return json.Unmarshal(value, &record) == nil && match(record)
	}
}

// enteringState matches the first write of a run record in state.
func enteringState(state RuntimeState) func(string, []byte) bool {
	return runWrite(func(record storedRuntimeRun) bool { return record.State == state })
}

// committingTransition matches the first write that records a loop
// transition of kind.
func committingTransition(kind string) func(string, []byte) bool {
	return runWrite(func(record storedRuntimeRun) bool { return record.LastTransition == kind })
}

// creatingBatch matches the write that creates a pending tool batch.
func creatingBatch() func(string, []byte) bool {
	return runWrite(func(record storedRuntimeRun) bool { return record.PendingBatchID != "" })
}

// startingHookDelivery matches the hook-delivery marker commit.
func startingHookDelivery() func(string, []byte) bool {
	return runWrite(func(record storedRuntimeRun) bool { return len(record.HookDelivery) > 0 })
}

// dispatchingInvocation matches the write that marks a tool invocation
// dispatched, before the tool runs.
func dispatchingInvocation() func(string, []byte) bool {
	return func(bucket string, value []byte) bool {
		if bucket != runtimeInvocationsBucket {
			return false
		}
		var invocation storedToolInvocation
		return json.Unmarshal(value, &invocation) == nil && invocation.State == ToolInvocationDispatched
	}
}
