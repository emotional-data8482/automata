package core

import (
	"context"
	"errors"
	"testing"
)

// A panicking writer must leave committed state exactly as it was: the
// memory store writes in place under its exclusive lock, so its undo log has
// to run on panic as well as on error.
func TestMemoryStorePanicRollsBackInPlaceWrites(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
		return tx.Put("b", "keep", []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = store.Transaction(context.Background(), true, func(tx StoreTransaction) error {
			_ = tx.Put("b", "keep", []byte("changed"))
			_ = tx.Put("b", "new", []byte("2"))
			_ = tx.Delete("b", "keep")
			panic("writer panic")
		})
	}()
	if err := store.Transaction(context.Background(), false, func(tx StoreTransaction) error {
		value, err := tx.Get("b", "keep")
		if err != nil || string(value) != "1" {
			t.Fatalf("keep = %q, %v; want the committed value", value, err)
		}
		if _, err := tx.Get("b", "new"); !errors.Is(err, ErrStoreKeyNotFound) {
			t.Fatalf("write from the panicking transaction survived: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
