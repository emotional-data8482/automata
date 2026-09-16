package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ErrStoreKeyNotFound is returned when a transaction reads a missing key.
var ErrStoreKeyNotFound = errors.New("runtime store key not found")

// Store is the provider-neutral persistence port used by Runtime. A Store owns
// its resources and is closed by the Runtime that receives it.
//
// Transaction must serialize writable transactions and commit all writes
// atomically if fn returns nil. If fn returns an error, no write may commit.
// Values handed to or returned by a transaction must be detached copies.
type Store interface {
	Transaction(context.Context, bool, func(StoreTransaction) error) error
	Close() error
}

// StoreTransaction is a deliberately small ordered key/value transaction.
// Buckets and encodings are owned by core; storage adapters must not interpret
// them as execution semantics.
type StoreTransaction interface {
	Get(bucket, key string) ([]byte, error)
	Put(bucket, key string, value []byte) error
	Scan(bucket, prefix string, visit func(key string, value []byte) error) error
	// ScanPage visits entries with the given prefix whose keys are strictly
	// greater than after, in ascending key order, up to limit entries. It
	// returns the last visited key, or "" when the page was empty (end of the
	// prefix range). A limit <= 0 removes the page bound. An error from visit
	// aborts the scan and fails the transaction; the returned key is the last
	// entry actually visited before the error.
	ScanPage(bucket, prefix, after string, limit int, visit func(key string, value []byte) error) (string, error)
}

// NewMemoryStore returns the explicitly ephemeral Store that backs
// [NewEphemeralRuntime]. It holds everything in process memory, promises no
// restart recovery, and exists for tests and intentionally temporary
// applications. Persistent runtimes must never fall back to it.
func NewMemoryStore() Store {
	return &memoryStore{buckets: make(map[string]map[string][]byte)}
}

func newEphemeralStore() Store {
	return NewMemoryStore()
}

type memoryStore struct {
	mu      sync.RWMutex
	buckets map[string]map[string][]byte
	closed  bool
}

func (s *memoryStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) error {
	if fn == nil {
		return fmt.Errorf("nil store transaction")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if writable {
		s.mu.Lock()
		defer s.mu.Unlock()
	} else {
		s.mu.RLock()
		defer s.mu.RUnlock()
	}
	if s.closed {
		return errors.New("runtime store is closed")
	}
	tx := &memoryTransaction{source: s.buckets, writable: writable}
	if writable {
		tx.source = cloneBuckets(s.buckets)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if writable {
		s.buckets = tx.source
	}
	return nil
}

func (s *memoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

type memoryTransaction struct {
	source   map[string]map[string][]byte
	writable bool
}

func (tx *memoryTransaction) Get(bucket, key string) ([]byte, error) {
	value, ok := tx.source[bucket][key]
	if !ok {
		return nil, ErrStoreKeyNotFound
	}
	return append([]byte(nil), value...), nil
}

func (tx *memoryTransaction) Put(bucket, key string, value []byte) error {
	if !tx.writable {
		return errors.New("read-only store transaction")
	}
	if tx.source[bucket] == nil {
		tx.source[bucket] = make(map[string][]byte)
	}
	tx.source[bucket][key] = append([]byte(nil), value...)
	return nil
}

func (tx *memoryTransaction) Scan(bucket, prefix string, visit func(string, []byte) error) error {
	_, err := tx.ScanPage(bucket, prefix, "", 0, visit)
	return err
}

func (tx *memoryTransaction) ScanPage(bucket, prefix, after string, limit int, visit func(string, []byte) error) (string, error) {
	if visit == nil {
		return "", fmt.Errorf("nil scan visitor")
	}
	values := tx.source[bucket]
	keys := make([]string, 0, len(values))
	for key := range values {
		if strings.HasPrefix(key, prefix) && key > after {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	var last string
	for _, key := range keys {
		if err := visit(key, append([]byte(nil), values[key]...)); err != nil {
			return last, err
		}
		last = key
	}
	return last, nil
}

func cloneBuckets(in map[string]map[string][]byte) map[string]map[string][]byte {
	out := make(map[string]map[string][]byte, len(in))
	for bucket, values := range in {
		out[bucket] = make(map[string][]byte, len(values))
		for key, value := range values {
			out[bucket][key] = append([]byte(nil), value...)
		}
	}
	return out
}
