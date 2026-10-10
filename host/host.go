// Package host is the entry point of a durable agent server: it opens the
// store, registers the definitions stored runs may pin, recovers work a
// previous process left, applies retention, and serves the Runtime over HTTP
// until it is asked to stop. The same program runs on a laptop, a single
// server, or a hosted platform.
//
//	func main() {
//		host.Main(host.Options{
//			Register: func(rt *core.Runtime) error {
//				_, err := rt.Register("support", "2026-10-01", supportAgent())
//				return err
//			},
//		})
//	}
//
// Configuration not set in [Options] comes from the environment:
// AUTOMATA_ADDR (listen address, default ":8080"), AUTOMATA_DB (SQLite path,
// default "automata.db"), and AUTOMATA_TOKEN (a bearer token every request
// must present when no Authorize function is configured).
package host

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/extensions/httpserver"
	"github.com/emotional-data8482/automata/extensions/sqlite"
)

// Options configures a host. Register is required.
type Options struct {
	// Register registers every definition revision that stored runs may
	// pin. It runs before recovery, so recovered runs find their bindings.
	Register func(*core.Runtime) error
	// Store opens the durable store. By default the host opens SQLite at
	// DatabasePath. The host closes the store through the Runtime.
	Store func(context.Context) (core.Store, error)
	// DatabasePath is the SQLite file used without Store. Empty selects
	// AUTOMATA_DB, then "automata.db".
	DatabasePath string
	// Runtime customizes the Runtime configuration (hooks, approval
	// authorizer, provider recovery, payload limits). Its Store is set by
	// the host.
	Runtime func(*core.RuntimeConfig)
	// Server configures the HTTP API. Its Runtime is set by the host. When
	// Authorize is nil, the host requires the bearer token in AUTOMATA_TOKEN
	// and refuses to start without one.
	Server httpserver.Config
	// Middleware wraps the whole HTTP handler, for logging, tracing, or
	// rate limiting.
	Middleware func(http.Handler) http.Handler
	// Addr is the listen address. Empty selects AUTOMATA_ADDR, then
	// ":8080". Listener, when set, is used instead.
	Addr     string
	Listener net.Listener
	// Retention, when set, is applied every RetentionInterval (default one
	// hour) while the host runs.
	Retention         *core.RetentionPolicy
	RetentionInterval time.Duration
	// ShutdownTimeout bounds the graceful drain of HTTP requests on stop.
	// Zero selects 30 seconds. Event streams end at once; clients resume
	// from their cursor.
	ShutdownTimeout time.Duration
	// Ready, when set, is called with the bound address once the host
	// serves requests.
	Ready func(addr net.Addr)
	// Logger receives lifecycle logs. Nil selects slog.Default.
	Logger *slog.Logger
}

// Main runs the host until SIGINT or SIGTERM and exits the process with a
// non-zero status if it fails.
func Main(opts Options) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := Run(ctx, opts)
	stop()
	if err != nil {
		logger(opts).Error("host stopped", "error", err)
		os.Exit(1)
	}
}

func logger(opts Options) *slog.Logger {
	if opts.Logger != nil {
		return opts.Logger
	}
	return slog.Default()
}

// Run opens the store, registers definitions, recovers, and serves until ctx
// ends; then it drains HTTP requests and closes the Runtime, which stops
// workers at a safe boundary. Work left unfinished resumes on the next Run.
func Run(ctx context.Context, opts Options) error {
	log := logger(opts)
	if opts.Register == nil {
		return errors.New("host: Register is required")
	}
	server := opts.Server
	if server.Authorize == nil {
		token := os.Getenv("AUTOMATA_TOKEN")
		if token == "" {
			return errors.New("host: set Server.Authorize or AUTOMATA_TOKEN")
		}
		server.Authorize = BearerToken(token)
	}

	store, err := openStore(ctx, opts)
	if err != nil {
		return fmt.Errorf("host: open store: %w", err)
	}
	if store == nil {
		return errors.New("host: open store returned nil")
	}
	// NewRuntime may fail before taking ownership, or close the store on
	// initialization failure. Ensure either path closes it exactly once.
	owned := &ownedStore{Store: store}
	defer func() {
		if err := owned.Close(); err != nil {
			log.Error("close store", "error", err)
		}
	}()
	config := core.RuntimeConfig{}
	if opts.Runtime != nil {
		opts.Runtime(&config)
	}
	config.Store = owned
	runtime, err := core.NewRuntime(ctx, config)
	if err != nil {
		return fmt.Errorf("host: open runtime: %w", err)
	}
	defer func() {
		if err := runtime.Close(); err != nil {
			log.Error("close runtime", "error", err)
		}
	}()
	if err := opts.Register(runtime); err != nil {
		return fmt.Errorf("host: register definitions: %w", err)
	}
	if err := runtime.Recover(ctx); err != nil {
		return fmt.Errorf("host: recover: %w", err)
	}

	server.Runtime = runtime
	api, err := httpserver.New(server)
	if err != nil {
		return fmt.Errorf("host: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.Handle("/", api)
	var handler http.Handler = mux
	if opts.Middleware != nil {
		handler = opts.Middleware(handler)
	}

	listener := opts.Listener
	if listener == nil {
		listener, err = net.Listen("tcp", addr(opts))
		if err != nil {
			return fmt.Errorf("host: listen: %w", err)
		}
	}
	// Streams and long polls derive from base; canceling it on stop ends
	// them so the drain does not wait on idle clients.
	base, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return base },
	}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	log.Info("host serving", "addr", listener.Addr().String())
	if opts.Ready != nil {
		opts.Ready(listener.Addr())
	}

	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		applyRetention(base, runtime, opts, log)
	}()

	select {
	case err = <-served:
		cancelBase()
		<-retentionDone
		return fmt.Errorf("host: serve: %w", err)
	case <-ctx.Done():
	}
	log.Info("host stopping")
	cancelBase()
	timeout := opts.ShutdownTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	drain, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := httpServer.Shutdown(drain)
	<-served
	<-retentionDone
	if shutdownErr != nil {
		return fmt.Errorf("host: drain: %w", shutdownErr)
	}
	return nil
}

type ownedStore struct {
	core.Store
	once sync.Once
	err  error
}

func (s *ownedStore) Close() error {
	s.once.Do(func() { s.err = s.Store.Close() })
	return s.err
}

func openStore(ctx context.Context, opts Options) (core.Store, error) {
	if opts.Store != nil {
		return opts.Store(ctx)
	}
	path := opts.DatabasePath
	if path == "" {
		path = os.Getenv("AUTOMATA_DB")
	}
	if path == "" {
		path = "automata.db"
	}
	return sqlite.Open(ctx, path)
}

func addr(opts Options) string {
	if opts.Addr != "" {
		return opts.Addr
	}
	if env := os.Getenv("AUTOMATA_ADDR"); env != "" {
		return env
	}
	return ":8080"
}

func applyRetention(ctx context.Context, runtime *core.Runtime, opts Options, log *slog.Logger) {
	if opts.Retention == nil {
		return
	}
	interval := opts.RetentionInterval
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for {
			report, err := runtime.Prune(ctx, *opts.Retention)
			if err != nil {
				if ctx.Err() == nil {
					log.Error("retention", "error", err)
				}
				break
			}
			if !report.More {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// BearerToken authorizes requests that present token as a bearer
// credential, compared in constant time, and answers 401 otherwise.
func BearerToken(token string) func(*http.Request, httpserver.Request) error {
	want := []byte(token)
	return func(r *http.Request, _ httpserver.Request) error {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			return httpserver.ErrUnauthenticated
		}
		return nil
	}
}
