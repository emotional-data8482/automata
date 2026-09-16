package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/core/storetest"
)

// The SQLite adapter must satisfy the same storage contract as the in-memory
// ephemeral store.
func TestSQLiteStoreConformance(t *testing.T) {
	storetest.Conformance(t, func(t *testing.T) core.Store {
		path := filepath.Join(t.TempDir(), "conformance.sqlite")
		store, err := Open(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	})
}
