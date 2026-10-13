package browser

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
)

// These fakes exercise shutdown without launching another browser process.
type shutdownLauncher struct {
	exited  chan struct{}
	once    sync.Once
	killed  atomic.Bool
	cleaned atomic.Bool
}

func (l *shutdownLauncher) exit() { l.once.Do(func() { close(l.exited) }) }
func (l *shutdownLauncher) Kill() {
	l.killed.Store(true)
	l.exit()
}
func (l *shutdownLauncher) Cleanup() {
	<-l.exited
	l.cleaned.Store(true)
}

type shutdownClient struct {
	launcher *shutdownLauncher
	stall    bool
	release  chan struct{}
	deadline chan bool
}

func (c *shutdownClient) Event() <-chan *cdp.Event { return nil }
func (c *shutdownClient) Call(ctx context.Context, sessionID, method string, params interface{}) ([]byte, error) {
	if method != "Browser.close" {
		return nil, errors.New("unexpected CDP method: " + method)
	}
	_, bounded := ctx.Deadline()
	c.deadline <- bounded
	if !c.stall {
		c.launcher.exit()
		return nil, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.release:
		return nil, errors.New("test released stalled close")
	}
}

func TestProcessClose(t *testing.T) {
	for _, stall := range []bool{false, true} {
		name := "graceful"
		if stall {
			name = "unresponsive CDP"
		}
		t.Run(name, func(t *testing.T) {
			l := &shutdownLauncher{exited: make(chan struct{})}
			client := &shutdownClient{
				launcher: l, stall: stall,
				release: make(chan struct{}), deadline: make(chan bool, 1),
			}
			p := &process{launcher: l, browser: rod.New().Client(client)}
			done := make(chan error, 1)
			go func() { done <- p.close() }()
			// Also release the fake on failure, so a regression cannot leave
			// the test's shutdown goroutine blocked forever.
			t.Cleanup(func() {
				close(client.release)
				l.Kill()
			})
			select {
			case err := <-done:
				if stall && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("stalled close error = %v, want deadline exceeded", err)
				}
				if !stall && err != nil {
					t.Fatalf("graceful close: %v", err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("shutdown never reached its kill fallback")
			}
			if bounded := <-client.deadline; !bounded {
				t.Error("Browser.close was called without a deadline")
			}
			if l.killed.Load() != stall {
				t.Errorf("killed = %v, want %v", l.killed.Load(), stall)
			}
			if !l.cleaned.Load() {
				t.Error("shutdown returned before profile cleanup completed")
			}
		})
	}
}
