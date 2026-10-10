package host

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/httpserver"
	"github.com/emotional-data8482/automata/extensions/sqlite"
)

type countedStore struct {
	core.Store
	closes             atomic.Int32
	failInitialization bool
}

func (s *countedStore) Transaction(ctx context.Context, writable bool, fn func(core.StoreTransaction) error) error {
	if s.failInitialization {
		return errors.New("injected initialization failure")
	}
	return s.Store.Transaction(ctx, writable, fn)
}

func (s *countedStore) Close() error {
	s.closes.Add(1)
	return s.Store.Close()
}

func TestStartupFailureClosesStoreExactlyOnce(t *testing.T) {
	for _, phase := range []string{"validation", "initialization", "registration"} {
		t.Run(phase, func(t *testing.T) {
			store := &countedStore{Store: core.NewMemoryStore(), failInitialization: phase == "initialization"}
			err := Run(context.Background(), Options{
				Store:    func(context.Context) (core.Store, error) { return store, nil },
				Register: func(*core.Runtime) error { return errors.New("injected registration failure") },
				Runtime: func(config *core.RuntimeConfig) {
					if phase == "validation" {
						config.MaxPayloadBytes = -1
					}
				},
				Server: httpserver.Config{Authorize: httpserver.AllowAll},
			})
			if err == nil {
				t.Fatal("Run unexpectedly succeeded")
			}
			if got := store.closes.Load(); got != 1 {
				t.Fatalf("store closed %d times, want 1", got)
			}
		})
	}
}

func TestInvalidRuntimeConfigReleasesSQLiteOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	err := Run(context.Background(), Options{
		DatabasePath: path,
		Register:     func(*core.Runtime) error { return nil },
		Runtime:      func(config *core.RuntimeConfig) { config.MaxPayloadBytes = -1 },
		Server:       httpserver.Config{Authorize: httpserver.AllowAll},
	})
	if err == nil {
		t.Fatal("Run accepted invalid configuration")
	}
	store, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen after failed startup: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
