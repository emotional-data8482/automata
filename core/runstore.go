package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// Delete removes key from bucket. Deleting a missing key is not an error.
	// Like Put, it is rejected by read-only transactions and commits only with
	// the rest of the transaction.
	Delete(bucket, key string) error
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
	return &memoryStore{buckets: make(map[string]*memoryBucket)}
}

func newEphemeralStore() Store {
	return NewMemoryStore()
}

// memoryStore serializes writers with an exclusive lock, so a writable
// transaction mutates committed state in place and keeps an undo log for
// rollback: a write costs O(log n) plus the key shift, never a copy of the
// whole store. Readers share the lock and never observe a writer's partial
// state.
type memoryStore struct {
	mu      sync.RWMutex
	buckets map[string]*memoryBucket
	closed  bool
}

// memoryBucket keeps its keys sorted so prefix and cursor scans seek directly
// to their range instead of sorting the bucket on every scan.
type memoryBucket struct {
	keys   []string
	values map[string][]byte
}

func (b *memoryBucket) insert(key string, value []byte) (previous []byte, existed bool) {
	previous, existed = b.values[key]
	if !existed {
		i, _ := slices.BinarySearch(b.keys, key)
		b.keys = slices.Insert(b.keys, i, key)
	}
	b.values[key] = value
	return previous, existed
}

func (b *memoryBucket) remove(key string) (previous []byte, existed bool) {
	previous, existed = b.values[key]
	if existed {
		i, _ := slices.BinarySearch(b.keys, key)
		b.keys = slices.Delete(b.keys, i, i+1)
		delete(b.values, key)
	}
	return previous, existed
}

type memoryUndo struct {
	bucket, key string
	value       []byte
	existed     bool
}

func (s *memoryStore) Transaction(ctx context.Context, writable bool, fn func(StoreTransaction) error) (err error) {
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
	tx := &memoryTransaction{store: s, writable: writable}
	if writable {
		committed := false
		defer func() {
			// Roll back on error and on panic alike.
			if !committed {
				tx.rollback()
			}
		}()
		if err := fn(tx); err != nil {
			return err
		}
		committed = true
		return nil
	}
	return fn(tx)
}

func (s *memoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

type memoryTransaction struct {
	store    *memoryStore
	writable bool
	undo     []memoryUndo
}

func (tx *memoryTransaction) rollback() {
	for i := len(tx.undo) - 1; i >= 0; i-- {
		entry := tx.undo[i]
		bucket := tx.store.buckets[entry.bucket]
		if entry.existed {
			bucket.insert(entry.key, entry.value)
		} else {
			bucket.remove(entry.key)
		}
	}
	tx.undo = nil
}

func (tx *memoryTransaction) Get(bucket, key string) ([]byte, error) {
	b := tx.store.buckets[bucket]
	if b == nil {
		return nil, ErrStoreKeyNotFound
	}
	value, ok := b.values[key]
	if !ok {
		return nil, ErrStoreKeyNotFound
	}
	return append([]byte(nil), value...), nil
}

func (tx *memoryTransaction) Put(bucket, key string, value []byte) error {
	if !tx.writable {
		return errors.New("read-only store transaction")
	}
	b := tx.store.buckets[bucket]
	if b == nil {
		b = &memoryBucket{values: make(map[string][]byte)}
		tx.store.buckets[bucket] = b
	}
	previous, existed := b.insert(key, append([]byte(nil), value...))
	tx.undo = append(tx.undo, memoryUndo{bucket: bucket, key: key, value: previous, existed: existed})
	return nil
}

func (tx *memoryTransaction) Delete(bucket, key string) error {
	if !tx.writable {
		return errors.New("read-only store transaction")
	}
	b := tx.store.buckets[bucket]
	if b == nil {
		return nil
	}
	if previous, existed := b.remove(key); existed {
		tx.undo = append(tx.undo, memoryUndo{bucket: bucket, key: key, value: previous, existed: true})
	}
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
	b := tx.store.buckets[bucket]
	if b == nil {
		return "", nil
	}
	start, _ := slices.BinarySearch(b.keys, prefix)
	if after >= prefix {
		i, found := slices.BinarySearch(b.keys, after)
		if found {
			i++
		}
		start = i
	}
	// Collect the page before visiting so a visitor that writes cannot shift
	// the keys being iterated.
	var keys []string
	for _, key := range b.keys[start:] {
		if !strings.HasPrefix(key, prefix) || (limit > 0 && len(keys) == limit) {
			break
		}
		keys = append(keys, key)
	}
	var last string
	for _, key := range keys {
		value, ok := b.values[key]
		if !ok {
			continue
		}
		if err := visit(key, append([]byte(nil), value...)); err != nil {
			return last, err
		}
		last = key
	}
	return last, nil
}
