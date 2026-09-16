package core_test

import (
	"testing"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/core/storetest"
)

// The in-memory ephemeral store is a full Store implementation and must pass
// the same contract suite as real adapters.
func TestMemoryStoreConformance(t *testing.T) {
	storetest.Conformance(t, func(t *testing.T) core.Store {
		return core.NewMemoryStore()
	})
}
