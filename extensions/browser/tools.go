package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/go-rod/rod"
)

const (
	maxActions        = 20
	defaultScreenshot = 1024
	// settleDelay gives a click-triggered navigation time to start before
	// the post-action load wait.
	settleDelay = 150 * time.Millisecond
)

// Tools returns the session's tools: browser_navigate, browser_snapshot,
// browser_act and browser_screenshot, plus browser_request_access when
// Config.RequestAccess or Config.AskAccess is set.
func (s *Session) Tools() []core.Tool {
	readOnly := core.ToolEffectPolicy{Kind: core.ToolEffectReadOnly}
	tools := []core.Tool{
		core.WithToolEffectPolicy(core.FuncResult("browser_navigate",
			"Open an http(s) URL in the browser page and return its title, final URL, and an interactive snapshot of the viewport. Refs like [e6] in the snapshot identify elements for browser_act.",
			guard(s, s.navigate)), readOnly),
		core.WithToolEffectPolicy(core.FuncResult("browser_snapshot",
			"Return an accessibility snapshot of the current page. mode interactive (default) lists controls in the viewport; content adds visible text and images; full covers the whole page. within limits output to one ref's subtree. Long snapshots are paged.",
			guard(s, s.snapshot)), readOnly),
		core.WithToolEffectPolicy(core.FuncResult("browser_act",
			"Perform a batch of actions on snapshot refs, in order, then return a new snapshot. Ops: click, type (replaces the field's text), select (an option by its visible text), press (a key such as Enter, Tab, ArrowDown or Control+a; ref optional), hover, scroll (to a ref, or text up/down for the page). Stops at the first failing action and reports its index.",
			guard(s, s.act)), core.ToolEffectPolicy{Kind: core.ToolEffectMutating}),
		core.WithToolEffectPolicy(core.FuncResult("browser_screenshot",
			"Capture a PNG screenshot of the viewport, the full page, or one element ref. The long edge is scaled down to max_px (default 1024).",
			guard(s, s.screenshot)), readOnly),
	}
	if s.requestsEnabled() {
		tools = append(tools, s.requestAccess())
	}
	return tools
}

// guard serializes tool calls, rejects calls after Close, and keeps parent
// cancellation fatal while turning other failures into recoverable results.
func guard[P any](s *Session, fn func(context.Context, *rod.Page, P) (core.ToolResult, error)) func(context.Context, P) (core.ToolResult, error) {
	return func(ctx context.Context, p P) (core.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return core.ToolResult{}, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return notApplied("browser session is closed"), nil
		}
		s.takeBlocked() // report only what this call's navigations hit
		result, err := fn(ctx, s.page.Context(ctx), p)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err != nil {
			out := core.ErrorResult(err.Error())
			out.Effect = result.Effect
			return out, nil
		}
		return result, nil
	}
}

func notApplied(msg string) core.ToolResult {
	r := core.ErrorResult(msg)
	r.Effect = core.EffectReport{Status: core.EffectNotApplied}
	return r
}

type navigateParams struct {
	URL string `json:"url" desc:"the http(s) URL to open"`
}

func (s *Session) navigate(ctx context.Context, page *rod.Page, p navigateParams) (core.ToolResult, error) {
	if err := s.checkURL(p.URL); err != nil {
		return notApplied(err.Error()), nil
	}
	nav := page.Timeout(s.cfg.NavigationTimeout)
	defer nav.CancelTimeout()
	if err := nav.Navigate(strings.TrimSpace(p.URL)); err != nil {
		var navErr *rod.NavigationError
		if errors.As(err, &navErr) && strings.Contains(navErr.Reason, "BLOCKED_BY_CLIENT") {
			if note := s.blockedNote(s.takeBlocked()); note != "" {
				return core.ToolResult{}, errors.New(strings.TrimSpace(note))
			}
			return core.ToolResult{}, errors.New("navigation blocked: a redirect left the allowed hosts")
		}
		return core.ToolResult{}, fmt.Errorf("navigate: %w", err)
	}
	loadNote := ""
	if err := nav.WaitLoad(); err != nil {
		if ctx.Err() != nil {
			return core.ToolResult{}, ctx.Err()
		}
		loadNote = "(the page had not finished loading; the snapshot may be incomplete)\n"
	}
	text, err := s.renderPage(page, modeInteractive, "", 1)
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.TextResult(s.blockedNote(s.takeBlocked()) + loadNote + text), nil
}

type snapshotParams struct {
	Mode   string `json:"mode,omitempty" enum:"interactive,content,full" desc:"interactive (default): controls in the viewport; content: also visible text and images; full: content for the whole page"`
	Within string `json:"within,omitempty" desc:"a ref; limit the snapshot to that element's subtree"`
	Page   int    `json:"page,omitempty" desc:"1-based page of a long snapshot"`
}

func (s *Session) snapshot(_ context.Context, page *rod.Page, p snapshotParams) (core.ToolResult, error) {
	mode := p.Mode
	if mode == "" {
		mode = modeInteractive
	}
	switch mode {
	case modeInteractive, modeContent, modeFull:
	default:
		return core.ToolResult{}, fmt.Errorf("unknown mode %q: use interactive, content or full", p.Mode)
	}
	text, err := s.renderPage(page, mode, p.Within, p.Page)
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.TextResult(text), nil
}

// renderPage captures the page and renders a snapshot with a title/URL header.
func (s *Session) renderPage(page *rod.Page, mode, within string, pageNum int) (string, error) {
	timed := page.Timeout(s.cfg.ActionTimeout)
	defer timed.CancelTimeout()
	st, err := s.capture(timed)
	if err != nil {
		return "", err
	}
	if st.root == nil {
		return "", errors.New("the page has no accessibility tree yet")
	}
	root := st.root
	opts := renderOptions{Mode: mode, Viewport: st.viewport, Unbounded: mode == modeFull}
	if within != "" {
		id, err := s.refs.lookup(within)
		if err != nil {
			return "", err
		}
		if root = findBackend(st.root, id); root == nil {
			return "", staleRefError(strings.TrimSpace(within))
		}
		opts.Unbounded = true
	}
	body, err := formatSnapshot(render(root, s.refs, opts), pageNum, s.cfg.MaxNodes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("title: %s\nurl: %s\n\n%s", clip(st.title), st.url, body), nil
}
