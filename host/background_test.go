package host

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/httpserver"
)

// Background runs beside the server on the recovered Runtime, sees its
// context end when the host stops, and finishes before Run returns.
func TestBackgroundRunsUntilStop(t *testing.T) {
	t.Setenv("AUTOMATA_TOKEN", "secret")
	admitted := make(chan string, 1)
	var finished atomic.Bool
	host := start(t, Options{
		Register:     register,
		DatabasePath: filepath.Join(t.TempDir(), "automata.db"),
		Background: func(ctx context.Context, rt *core.Runtime) error {
			handle, err := rt.Submit(ctx, core.DefinitionRef{ID: "agent", Revision: "v1"}, "scheduled",
				core.WithIdempotencyKey("schedule", "slot-1"))
			if err != nil {
				return err
			}
			admitted <- handle.ID()
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond) // work that outlives cancellation
			finished.Store(true)
			return nil
		},
	})
	var runID string
	select {
	case runID = <-admitted:
	case <-time.After(5 * time.Second):
		t.Fatal("background did not start")
	}
	if status, body := call(t, http.MethodGet, host.url+"/v1/runs/"+runID, "secret", nil); status != http.StatusOK {
		t.Fatalf("run admitted in background: %d %s", status, body)
	}
	host.shutdown(t)
	if !finished.Load() {
		t.Fatal("Run returned before Background finished")
	}
}

// A Background error stops the host and is returned from Run; returning nil
// early leaves the host serving.
func TestBackgroundErrorStopsHost(t *testing.T) {
	t.Setenv("AUTOMATA_TOKEN", "secret")
	fail := make(chan struct{})
	host := start(t, Options{
		Register:     register,
		DatabasePath: filepath.Join(t.TempDir(), "automata.db"),
		Background: func(ctx context.Context, _ *core.Runtime) error {
			select {
			case <-fail:
				return errors.New("scheduler broke")
			case <-ctx.Done():
				return nil
			}
		},
	})
	if status, _ := call(t, http.MethodGet, host.url+"/healthz", "", nil); status != http.StatusNoContent {
		t.Fatalf("healthz = %d", status)
	}
	close(fail)
	select {
	case err := <-host.done:
		if err == nil || err.Error() != "host: background: scheduler broke" {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host did not stop")
	}
	host.stop()

	done := start(t, Options{
		Register:     register,
		DatabasePath: filepath.Join(t.TempDir(), "automata.db"),
		Background:   func(context.Context, *core.Runtime) error { return nil },
	})
	time.Sleep(50 * time.Millisecond)
	if status, _ := call(t, http.MethodGet, done.url+"/healthz", "", nil); status != http.StatusNoContent {
		t.Fatalf("host stopped after Background returned nil: %d", status)
	}
	done.shutdown(t)
}

// Background may fail after shutdown starts. Run must collect that error
// even when cancellation or a serve failure wins the lifecycle select.
func TestBackgroundErrorDuringShutdown(t *testing.T) {
	for _, phase := range []string{"cancel", "serve"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			backgroundErr := errors.New("background cleanup failed")
			serveErr := errors.New("listener failed")
			opts := Options{
				Register: func(*core.Runtime) error { return nil },
				Store: func(context.Context) (core.Store, error) {
					return core.NewMemoryStore(), nil
				},
				Server:   httpserver.Config{Authorize: httpserver.AllowAll},
				Listener: listener,
				Background: func(backgroundCtx context.Context, _ *core.Runtime) error {
					if phase == "cancel" {
						cancel()
					}
					<-backgroundCtx.Done()
					return backgroundErr
				},
			}
			if phase == "serve" {
				opts.Listener = failingListener{Listener: listener, err: serveErr}
			}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, opts) }()
			select {
			case err := <-done:
				if !errors.Is(err, backgroundErr) {
					t.Fatalf("Run = %v, want background error", err)
				}
				if phase == "serve" && !errors.Is(err, serveErr) {
					t.Fatalf("Run = %v, want serve error too", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("host did not stop")
			}
		})
	}
}

type failingListener struct {
	net.Listener
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
