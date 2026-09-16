// Package storetest holds the storage contract suite for core.Store
// implementations. It is stdlib-only so any adapter module can call
// Conformance from its own tests without taking on new dependencies.
package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

// Conformance runs the full storage contract suite against a Store produced by
// open. Every test receives its own fresh store; implementations must close
// them themselves (the suite does not close stores, mirroring the Runtime's
// exclusive-ownership contract).
func Conformance(t *testing.T, open func(t *testing.T) core.Store) {
	t.Helper()

	t.Run("GetMissingKey", func(t *testing.T) {
		store := open(t)
		err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			_, err := tx.Get("b", "missing")
			return err
		})
		if !errors.Is(err, core.ErrStoreKeyNotFound) {
			t.Fatalf("missing key = %v, want ErrStoreKeyNotFound", err)
		}
	})

	t.Run("PutGetRoundTripDetachesValues", func(t *testing.T) {
		store := open(t)
		err := store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
			return tx.Put("b", "k", []byte("v1"))
		})
		if err != nil {
			t.Fatal(err)
		}
		held := []byte("v1")
		if err := store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
			return tx.Put("b", "k", held)
		}); err != nil {
			t.Fatal(err)
		}
		held[0] = 'X'
		var got []byte
		if err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			value, err := tx.Get("b", "k")
			got = value
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if string(got) != "v1" {
			t.Fatalf("mutated caller buffer leaked into the store: %q", got)
		}
		got[0] = 'Y'
		if err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			value, err := tx.Get("b", "k")
			if string(value) != "v1" {
				t.Fatalf("mutated returned buffer aliased store state: %q then %q", got, value)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ReadOnlyTransactionRejectsWrites", func(t *testing.T) {
		store := open(t)
		err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			return tx.Put("b", "k", []byte("v"))
		})
		if err == nil {
			t.Fatal("read-only transaction accepted a write")
		}
		if err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			_, err := tx.Get("b", "k")
			if !errors.Is(err, core.ErrStoreKeyNotFound) {
				t.Fatalf("rejected write partially committed: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("FailedTransactionCommitsNothing", func(t *testing.T) {
		store := open(t)
		boom := errors.New("boom")
		err := store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
			if err := tx.Put("b", "a", []byte("1")); err != nil {
				return err
			}
			if err := tx.Put("b", "b", []byte("2")); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("transaction error = %v", err)
		}
		if err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			for _, key := range []string{"a", "b"} {
				if _, err := tx.Get("b", key); !errors.Is(err, core.ErrStoreKeyNotFound) {
					t.Fatalf("key %q committed despite failure: %v", key, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("MultiPutCommitsAtomically", func(t *testing.T) {
		store := open(t)
		if err := store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
			if err := tx.Put("b", "a", []byte("1")); err != nil {
				return err
			}
			return tx.Put("b", "b", []byte("2"))
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			for key, want := range map[string]string{"a": "1", "b": "2"} {
				got, err := tx.Get("b", key)
				if err != nil || string(got) != want {
					t.Fatalf("key %q = %q, %v", key, got, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ScanOrdersAndFiltersPrefixes", func(t *testing.T) {
		store := open(t)
		if err := store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
			for key, value := range map[string]string{
				"x/a": "1", "a/1": "2", "a/0": "3", "a/2": "4", "b/1": "5",
			} {
				if err := tx.Put("b", key, []byte(value)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var seen []string
		if err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			return tx.Scan("b", "a/", func(key string, value []byte) error {
				seen = append(seen, key+"="+string(value))
				return nil
			})
		}); err != nil {
			t.Fatal(err)
		}
		want := []string{"a/0=3", "a/1=2", "a/2=4"}
		if len(seen) != len(want) {
			t.Fatalf("scan = %v, want %v", seen, want)
		}
		for i := range want {
			if seen[i] != want[i] {
				t.Fatalf("scan = %v, want %v", seen, want)
			}
		}
	})

	t.Run("ScanPagePagesThroughKeys", func(t *testing.T) {
		store := open(t)
		if err := store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
			for i := range 5 {
				if err := tx.Put("b", "k/"+string(rune('a'+i)), []byte{byte('0' + i)}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var collected []string
		after := ""
		pages := 0
		for {
			var last string
			err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
				var err error
				last, err = tx.ScanPage("b", "k/", after, 2, func(key string, _ []byte) error {
					collected = append(collected, key)
					return nil
				})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if last == "" {
				break
			}
			pages++
			after = last
			if pages > 10 {
				t.Fatal("pagination did not terminate")
			}
		}
		if pages != 3 || len(collected) != 5 {
			t.Fatalf("pages=%d collected=%v, want 3 pages over 5 keys", pages, collected)
		}
		for i, key := range collected {
			if want := "k/" + string(rune('a'+i)); key != want {
				t.Fatalf("collected[%d] = %q, want %q (ascending order)", i, key, want)
			}
		}
	})

	t.Run("ScanPageCombinesPrefixAndCursor", func(t *testing.T) {
		store := open(t)
		if err := store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
			for _, key := range []string{"p/1", "p/2", "q/1", "p/3", "q/2"} {
				if err := tx.Put("b", key, nil); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var seen []string
		var last string
		err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			var err error
			last, err = tx.ScanPage("b", "p/", "p/1", 0, func(key string, _ []byte) error {
				seen = append(seen, key)
				return nil
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if last != "p/3" || len(seen) != 2 || seen[0] != "p/2" || seen[1] != "p/3" {
			t.Fatalf("last=%q seen=%v, want p/3 over [p/2 p/3]", last, seen)
		}
	})

	t.Run("ScanVisitErrorFailsTransaction", func(t *testing.T) {
		store := open(t)
		if err := store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
			return tx.Put("b", "k", []byte("v"))
		}); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("visitor boom")
		err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			_, err := tx.ScanPage("b", "", "", 0, func(string, []byte) error { return boom })
			return err
		})
		if !errors.Is(err, boom) {
			t.Fatalf("visit error = %v", err)
		}
	})

	t.Run("ConcurrentWritersSerialize", func(t *testing.T) {
		store := open(t)
		if err := store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
			return tx.Put("b", "count", []byte("0"))
		}); err != nil {
			t.Fatal(err)
		}
		const writers = 8
		var wg sync.WaitGroup
		errs := make(chan error, writers)
		for range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- store.Transaction(context.Background(), true, func(tx core.StoreTransaction) error {
					raw, err := tx.Get("b", "count")
					if err != nil {
						return err
					}
					var count int
					for _, c := range raw {
						count = count*10 + int(c-'0')
					}
					count++
					digits := []byte{}
					if count == 0 {
						digits = append(digits, '0')
					}
					for count > 0 {
						digits = append([]byte{byte('0' + count%10)}, digits...)
						count /= 10
					}
					return tx.Put("b", "count", digits)
				})
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent writer: %v", err)
			}
		}
		if err := store.Transaction(context.Background(), false, func(tx core.StoreTransaction) error {
			raw, err := tx.Get("b", "count")
			if err != nil {
				return err
			}
			if string(raw) != "8" {
				t.Fatalf("concurrent writers lost updates: count = %q, want 8", raw)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("TransactionRespectsContextCancellation", func(t *testing.T) {
		store := open(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := store.Transaction(ctx, true, func(core.StoreTransaction) error {
			t.Error("transaction function ran with a canceled context")
			return nil
		})
		if err == nil {
			t.Fatal("canceled context did not fail the transaction")
		}
	})

	t.Run("ClosedStoreRejectsWorkAndReCloses", func(t *testing.T) {
		store := open(t)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("second close = %v", err)
		}
		err := store.Transaction(context.Background(), false, func(core.StoreTransaction) error {
			t.Error("transaction ran against a closed store")
			return nil
		})
		if err == nil {
			t.Fatal("closed store accepted a transaction")
		}
	})
}
