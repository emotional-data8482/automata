package core

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
)

// cloneMemoryStore copies every bucket, standing in for the durable state a
// process leaves behind when it stops.
func cloneMemoryStore(store *memoryStore) *memoryStore {
	store.mu.RLock()
	defer store.mu.RUnlock()
	clone := NewMemoryStore().(*memoryStore)
	for name, bucket := range store.buckets {
		copied := &memoryBucket{keys: slices.Clone(bucket.keys), values: make(map[string][]byte, len(bucket.values))}
		for key, value := range maps.All(bucket.values) {
			copied.values[key] = slices.Clone(value)
		}
		clone.buckets[name] = copied
	}
	return clone
}

// crashPointStore captures the store as it stood right after the first commit
// that leaves a run running at transition.
type crashPointStore struct {
	*memoryStore
	transition transitionKind
	once       sync.Once
	captured   *memoryStore
}

func (s *crashPointStore) Close() error { return nil }

func (s *crashPointStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	if err := s.memoryStore.Transaction(ctx, writable, fn); err != nil || !writable {
		return err
	}
	hit := false
	err := s.memoryStore.Transaction(ctx, false, func(tx StoreTransaction) error {
		return tx.Scan(runtimeRunsBucket, "", func(_ string, raw []byte) error {
			var record storedRuntimeRun
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			hit = hit || (record.State == RuntimeRunning && record.LastTransition == s.transition)
			return nil
		})
	})
	if err == nil && hit {
		s.once.Do(func() { s.captured = cloneMemoryStore(s.memoryStore) })
	}
	return nil
}

// A crash right after the exhausted correction batch commits must finalize
// the failure on recovery, not call the provider past MaxCorrections and turn
// the failed run into a success (bug review E7).
func TestRuntimeExhaustedCorrectionSurvivesCrash(t *testing.T) {
	cfg := &StructuredOutputConfig{Schema: json.RawMessage(summarySchema)} // MaxCorrections: 0
	store := &crashPointStore{memoryStore: NewMemoryStore().(*memoryStore), transition: transitionBatchCommitted}
	first := newStructuredAgent(t, &scriptedProvider{turns: []Message{asstCalls(structuredCall("s1", `{"summary":5}`))}}, nil, cfg, 8)
	runtime, err := NewRuntime(context.Background(), RuntimeConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Register("agent", "v1", first); err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Submit(context.Background(), DefinitionRef{ID: "agent", Revision: "v1"}, "produce summary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Await(context.Background()); !errors.Is(err, ErrInvalidStructuredOutput) {
		t.Fatalf("first run = %v, want ErrInvalidStructuredOutput", err)
	}
	_ = runtime.Close()
	if store.captured == nil {
		t.Fatal("no commit left the run running at batch_committed")
	}

	provider := &scriptedProvider{turns: []Message{asstCalls(structuredCall("s2", `{"summary":"done"}`))}}
	recovered, err := NewRuntime(context.Background(), RuntimeConfig{Store: store.captured})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, err := recovered.Register("agent", "v1", newStructuredAgent(t, provider, nil, cfg, 8)); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := recovered.Handle(handle.ID()).Await(context.Background())
	if !errors.Is(err, ErrInvalidStructuredOutput) || result.Status != RunFailed {
		t.Fatalf("recovered = %#v, %v; want the exhausted failure", result, err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider called %d times past MaxCorrections", provider.calls)
	}
}
