// Package browser gives an agent a real Chromium page through a small set of
// [core.Tool] values backed by the Chrome DevTools Protocol (via go-rod):
//
//	session, err := browser.New(ctx, browser.Config{AllowedHosts: []string{"example.com"}})
//	if err != nil { /* no browser found, or launch failed */ }
//	defer session.Close()
//	agent, err := core.New(provider, core.AgentConfig{Tools: session.Tools()})
//
// The model sees the page as a pruned accessibility-tree snapshot:
//
//	main
//	  heading "Sign in" (level 1)
//	  [e3] textbox "Email" (required)
//	  [e6] button "Save" (disabled)
//
// Only interactive nodes get refs. A ref names a DOM node (by its backend
// node id) and stays valid while that node lives; acting on a removed node
// returns a recoverable "stale ref" error that tells the model to take a new
// snapshot.
//
// Observation tools (browser_navigate, browser_snapshot, browser_screenshot)
// declare [core.ToolEffectReadOnly]. browser_act is [core.ToolEffectMutating]:
// it reports [core.EffectApplied] when its input reached the page,
// [core.EffectNotApplied] when it failed before dispatching any input, and
// [core.EffectUnknown] when an input dispatch itself failed midway.
//
// Security: page content is untrusted model input. Restrict navigation with
// [Config.AllowedHosts]; when set, document requests (including redirects,
// link clicks and same-process iframes) to other hosts are blocked inside
// the browser. Navigation never accepts non-http(s) URLs such as file://.
// Popups and out-of-process iframes are not intercepted.
package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// Defaults applied by [New] to zero Config fields.
const (
	DefaultViewportWidth     = 1280
	DefaultViewportHeight    = 800
	DefaultMaxNodes          = 250
	DefaultNavigationTimeout = 30 * time.Second
	DefaultActionTimeout     = 10 * time.Second
)

// ErrNoBrowser is returned by [New] when no Chrome or Chromium executable is
// configured or found. The package never downloads a browser.
var ErrNoBrowser = errors.New("browser: no Chrome or Chromium executable found; set Config.BrowserPath")

// Viewport is the page size in CSS pixels.
type Viewport struct {
	Width, Height int
}

// Config configures a [Session]. The zero value launches a headless browser
// with no host restriction.
type Config struct {
	// AllowedHosts restricts which hosts the page may load documents from.
	// An entry matches the host exactly, or any subdomain when written as
	// "*.example.com". Ports are ignored. Empty means any http(s) host.
	AllowedHosts []string
	// Headed shows the browser window. The default is headless.
	Headed bool
	// Viewport defaults to 1280x800.
	Viewport Viewport
	// MaxNodes bounds the lines in one snapshot page. Defaults to 250.
	MaxNodes int
	// BrowserPath is the Chrome or Chromium executable. When empty, common
	// install locations are searched; nothing is downloaded.
	BrowserPath string
	// NavigationTimeout bounds one navigation including load. Defaults to 30s.
	NavigationTimeout time.Duration
	// ActionTimeout bounds each browser_act action and each snapshot or
	// screenshot capture. Defaults to 10s.
	ActionTimeout time.Duration
}

// Session owns one browser process and one page. Its tools serialize their
// calls, so it is safe for an agent's concurrent tool batches.
type Session struct {
	cfg      Config
	launcher *launcher.Launcher
	browser  *rod.Browser
	page     *rod.Page

	mu     sync.Mutex
	refs   *refRegistry
	closed bool
}

// New launches a browser and opens a blank page. ctx bounds the launch only;
// call [Session.Close] to stop the browser.
func New(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Viewport.Width <= 0 || cfg.Viewport.Height <= 0 {
		cfg.Viewport = Viewport{DefaultViewportWidth, DefaultViewportHeight}
	}
	if cfg.MaxNodes <= 0 {
		cfg.MaxNodes = DefaultMaxNodes
	}
	if cfg.NavigationTimeout <= 0 {
		cfg.NavigationTimeout = DefaultNavigationTimeout
	}
	if cfg.ActionTimeout <= 0 {
		cfg.ActionTimeout = DefaultActionTimeout
	}
	for _, h := range cfg.AllowedHosts {
		if strings.TrimSpace(h) == "" {
			return nil, errors.New("browser: AllowedHosts contains an empty entry")
		}
	}
	cfg.AllowedHosts = append([]string(nil), cfg.AllowedHosts...)

	bin := cfg.BrowserPath
	if bin == "" {
		found, ok := launcher.LookPath()
		if !ok {
			return nil, ErrNoBrowser
		}
		bin = found
	} else if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoBrowser, err)
	}

	l := launcher.New().Context(ctx).Bin(bin).Headless(!cfg.Headed)
	controlURL, err := l.Launch()
	if err != nil {
		l.Cleanup()
		return nil, fmt.Errorf("browser: launch %s: %w", bin, err)
	}
	s := &Session{cfg: cfg, launcher: l, refs: newRefRegistry()}
	if err := s.connect(ctx, controlURL); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) connect(ctx context.Context, controlURL string) error {
	b := rod.New().ControlURL(controlURL).Context(ctx)
	if err := b.Connect(); err != nil {
		return fmt.Errorf("browser: connect: %w", err)
	}
	// Detach from the launch context: it only bounds startup.
	s.browser = b.Context(context.Background())
	page, err := s.browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return fmt.Errorf("browser: open page: %w", err)
	}
	s.page = page
	err = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: s.cfg.Viewport.Width, Height: s.cfg.Viewport.Height, DeviceScaleFactor: 1,
	})
	if err != nil {
		return fmt.Errorf("browser: set viewport: %w", err)
	}
	if len(s.cfg.AllowedHosts) > 0 {
		if err := s.interceptDocuments(); err != nil {
			return fmt.Errorf("browser: enable host filter: %w", err)
		}
	}
	return nil
}

// interceptDocuments fails document requests (navigations, redirects,
// frames) whose host is outside AllowedHosts.
func (s *Session) interceptDocuments() error {
	page := s.page
	go page.EachEvent(func(e *proto.FetchRequestPaused) {
		// Answer off the event loop so a slow reply never stalls other events.
		go func() {
			if s.checkURL(e.Request.URL) == nil {
				_ = proto.FetchContinueRequest{RequestID: e.RequestID}.Call(page)
				return
			}
			_ = proto.FetchFailRequest{RequestID: e.RequestID, ErrorReason: proto.NetworkErrorReasonBlockedByClient}.Call(page)
		}()
	})()
	return proto.FetchEnable{Patterns: []*proto.FetchRequestPattern{{
		URLPattern:   "*",
		ResourceType: proto.NetworkResourceTypeDocument,
		RequestStage: proto.FetchRequestStageRequest,
	}}}.Call(page)
}

// Close stops the browser and removes its temporary profile. It is safe to
// call more than once.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.browser != nil {
		err = s.browser.Close()
	}
	if s.launcher != nil {
		s.launcher.Kill()
		s.launcher.Cleanup()
	}
	return err
}

// checkURL validates a navigation target against the scheme rule and
// AllowedHosts.
func (s *Session) checkURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q: only http and https URLs can be opened", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("invalid url %q: missing host", raw)
	}
	if !hostAllowed(s.cfg.AllowedHosts, host) {
		return fmt.Errorf("host %q is not in the allowed hosts", host)
	}
	return nil
}

func hostAllowed(allowed []string, host string) bool {
	if len(allowed) == 0 {
		return true
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, entry := range allowed {
		entry = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(entry)), ".")
		if h, _, err := net.SplitHostPort(entry); err == nil {
			entry = h
		}
		entry = strings.Trim(entry, "[]")
		if suffix, ok := strings.CutPrefix(entry, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}

// pageState is one capture of the page: its accessibility tree joined with
// layout, the visual viewport, and identity.
type pageState struct {
	root     *axNode
	viewport rect
	content  rect
	url      string
	title    string
}

// capture reads the page. It also retires refs when the main frame has
// committed a new document.
func (s *Session) capture(page *rod.Page) (*pageState, error) {
	tree, err := proto.PageGetFrameTree{}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("read frame tree: %w", err)
	}
	frame := tree.FrameTree.Frame
	metrics, err := proto.PageGetLayoutMetrics{}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("read layout metrics: %w", err)
	}
	ax, err := proto.AccessibilityGetFullAXTree{}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("read accessibility tree: %w", err)
	}
	dom, err := proto.DOMSnapshotCaptureSnapshot{ComputedStyles: []string{}}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("read layout: %w", err)
	}

	s.refs.observe(string(frame.LoaderID))
	vv := metrics.CSSVisualViewport
	st := &pageState{
		root:     buildTree(ax.Nodes, layoutBounds(dom)),
		viewport: rect{vv.PageX, vv.PageY, vv.ClientWidth, vv.ClientHeight},
		url:      frame.URL + frame.URLFragment,
	}
	if cs := metrics.CSSContentSize; cs != nil {
		st.content = rect{0, 0, cs.Width, cs.Height}
	}
	if st.root != nil && st.root.Role == "RootWebArea" {
		st.title = st.root.Name
	}
	return st, nil
}

// layoutBounds maps backend node ids of the main document to their layout
// boxes in document coordinates.
func layoutBounds(snap *proto.DOMSnapshotCaptureSnapshotResult) map[int]rect {
	out := map[int]rect{}
	if len(snap.Documents) == 0 {
		return out
	}
	doc := snap.Documents[0]
	if doc.Nodes == nil || doc.Layout == nil {
		return out
	}
	for i, nodeIndex := range doc.Layout.NodeIndex {
		if i >= len(doc.Layout.Bounds) || nodeIndex >= len(doc.Nodes.BackendNodeID) {
			continue
		}
		b := doc.Layout.Bounds[i]
		if len(b) < 4 {
			continue
		}
		id := int(doc.Nodes.BackendNodeID[nodeIndex])
		r := rect{b[0], b[1], b[2], b[3]}
		if prev, ok := out[id]; ok {
			// A node with several layout objects (pseudo-elements, continuations)
			// takes their union.
			r = union(prev, r)
		}
		out[id] = r
	}
	return out
}

func union(a, b rect) rect {
	x0, y0 := min(a.X, b.X), min(a.Y, b.Y)
	x1, y1 := max(a.X+a.W, b.X+b.W), max(a.Y+a.H, b.Y+b.H)
	return rect{x0, y0, x1 - x0, y1 - y0}
}

// buildTree converts Chrome's flat AX node list into an axNode tree.
func buildTree(nodes []*proto.AccessibilityAXNode, bounds map[int]rect) *axNode {
	byID := make(map[proto.AccessibilityAXNodeID]*axNode, len(nodes))
	for _, n := range nodes {
		out := &axNode{
			Role:      axString(n.Role),
			Name:      axString(n.Name),
			Value:     axString(n.Value),
			Ignored:   n.Ignored,
			BackendID: int(n.BackendDOMNodeID),
		}
		if len(n.Properties) > 0 {
			out.Props = make(map[string]string, len(n.Properties))
			for _, p := range n.Properties {
				out.Props[string(p.Name)] = axString(p.Value)
			}
		}
		if b, ok := bounds[out.BackendID]; ok && out.BackendID != 0 {
			out.Bounds = &b
		}
		byID[n.NodeID] = out
	}
	var root *axNode
	for _, n := range nodes {
		parent := byID[n.NodeID]
		for _, id := range n.ChildIDs {
			if c, ok := byID[id]; ok {
				parent.Children = append(parent.Children, c)
			}
		}
		if root == nil && n.ParentID == "" {
			root = parent
		}
	}
	return root
}

func axString(v *proto.AccessibilityAXValue) string {
	if v == nil || v.Value.Nil() {
		return ""
	}
	switch val := v.Value.Val().(type) {
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		return fmt.Sprint(val)
	default:
		return v.Value.String()
	}
}
