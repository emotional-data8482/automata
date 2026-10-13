package browser

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/emotional-data8482/automata/core"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	maxScreenshotPx = 4096
	// maxCaptureHeight bounds a full-page capture in CSS pixels.
	maxCaptureHeight = 16384
)

type screenshotParams struct {
	Ref      string `json:"ref,omitempty" desc:"capture only this element"`
	FullPage bool   `json:"full_page,omitempty" desc:"capture the whole page instead of the viewport"`
	MaxPx    int    `json:"max_px,omitempty" desc:"maximum long-edge size in pixels (default 1024)"`
}

func (s *Session) screenshot(_ context.Context, page *rod.Page, p screenshotParams) (core.ToolResult, error) {
	maxPx := p.MaxPx
	if maxPx == 0 {
		maxPx = defaultScreenshot
	}
	if maxPx < 16 || maxPx > maxScreenshotPx {
		return core.ToolResult{}, fmt.Errorf("max_px must be between 16 and %d", maxScreenshotPx)
	}
	if p.Ref != "" && p.FullPage {
		return core.ToolResult{}, errors.New("pass ref or full_page, not both")
	}
	page = page.Timeout(s.cfg.ActionTimeout)
	defer page.CancelTimeout()

	var clip rect
	beyond := false
	note := ""
	if p.Ref != "" {
		el, err := s.element(page, p.Ref)
		if err != nil {
			return core.ToolResult{}, err
		}
		if err := el.ScrollIntoView(); err != nil {
			return core.ToolResult{}, err
		}
		shape, err := el.Shape()
		if err != nil {
			return core.ToolResult{}, err
		}
		box := shape.Box()
		if box == nil || box.Width <= 0 || box.Height <= 0 {
			return core.ToolResult{}, errors.New("the element has no visible area")
		}
		vv, err := visualViewport(page)
		if err != nil {
			return core.ToolResult{}, err
		}
		// Content quads are viewport-relative; the clip is in document space.
		clip = rect{box.X + vv.PageX, box.Y + vv.PageY, box.Width, box.Height}
		beyond = true
	} else {
		metrics, err := proto.PageGetLayoutMetrics{}.Call(page)
		if err != nil {
			return core.ToolResult{}, err
		}
		vv := metrics.CSSVisualViewport
		clip = rect{vv.PageX, vv.PageY, vv.ClientWidth, vv.ClientHeight}
		if p.FullPage && metrics.CSSContentSize != nil {
			clip = rect{0, 0, metrics.CSSContentSize.Width, metrics.CSSContentSize.Height}
			beyond = true
			if clip.H > maxCaptureHeight {
				note = fmt.Sprintf("the page is %.0fpx tall; the capture stops at %dpx", clip.H, maxCaptureHeight)
				clip.H = maxCaptureHeight
			}
		}
	}

	scale := math.Min(1, float64(maxPx)/math.Max(clip.W, clip.H))
	shot, err := proto.PageCaptureScreenshot{
		Format:                proto.PageCaptureScreenshotFormatPng,
		Clip:                  &proto.PageViewport{X: clip.X, Y: clip.Y, Width: clip.W, Height: clip.H, Scale: scale},
		CaptureBeyondViewport: beyond,
	}.Call(page)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("capture screenshot: %w", err)
	}
	image := core.ImageBlock{MediaType: "image/png", Data: shot.Data}
	if note != "" {
		return core.BlockResult(core.TextBlock{Text: note}, image), nil
	}
	return core.BlockResult(image), nil
}

func visualViewport(page *rod.Page) (*proto.PageVisualViewport, error) {
	metrics, err := proto.PageGetLayoutMetrics{}.Call(page)
	if err != nil {
		return nil, err
	}
	return metrics.CSSVisualViewport, nil
}
