package browser

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/emotional-data8482/automata/core"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

type action struct {
	Op    string `json:"op" enum:"click,type,select,press,hover,scroll"`
	Ref   string `json:"ref,omitempty" desc:"snapshot ref of the target element, such as e6"`
	Text  string `json:"text,omitempty" desc:"type: the text to enter; select: the option's visible text; press: a key such as Enter, Control+a or Control+Plus; scroll without ref: up or down"`
	Count int    `json:"count,omitempty" desc:"press: how many times to press the key (default 1, at most 50)"`
}

// maxPressCount caps a press action's repeat count.
const maxPressCount = 50

func (a action) String() string {
	parts := []string{a.Op}
	if a.Ref != "" {
		parts = append(parts, a.Ref)
	}
	if a.Text != "" {
		parts = append(parts, fmt.Sprintf("%q", clip(a.Text)))
	}
	if a.Count > 1 {
		parts = append(parts, fmt.Sprintf("x%d", a.Count))
	}
	return strings.Join(parts, " ")
}

type actParams struct {
	Actions []action `json:"actions" desc:"actions to perform in order; the batch stops at the first failure"`
}

// act runs the batch, then returns a fresh snapshot. Its effect report is
// Applied once any input reached the page, NotApplied when it failed before
// any dispatch, and Unknown when a dispatch itself failed.
func (s *Session) act(ctx context.Context, page *rod.Page, p actParams) (core.ToolResult, error) {
	if len(p.Actions) == 0 {
		return notApplied("actions is empty"), nil
	}
	if len(p.Actions) > maxActions {
		return notApplied(fmt.Sprintf("at most %d actions per call", maxActions)), nil
	}
	for i, a := range p.Actions {
		if err := s.validate(a); err != nil {
			return notApplied(fmt.Sprintf("actions[%d] (%s): %v; no actions were performed", i, a, err)), nil
		}
	}

	status := core.EffectNotApplied
	failure := ""
	for i, a := range p.Actions {
		timed := page.Timeout(s.cfg.ActionTimeout)
		dispatched, err := s.do(timed, a)
		timed.CancelTimeout()
		if dispatched {
			status = core.EffectApplied
		}
		if err != nil {
			if ctx.Err() != nil {
				if dispatched {
					status = core.EffectUnknown
				}
				return core.ToolResult{Effect: core.EffectReport{Status: status}}, ctx.Err()
			}
			if dispatched {
				status = core.EffectUnknown
			}
			failure = fmt.Sprintf("actions[%d] (%s) failed: %v", i, a, err)
			if i > 0 {
				failure += fmt.Sprintf("\nactions[0..%d] completed.", i-1)
			}
			break
		}
	}

	var b strings.Builder
	if failure != "" {
		b.WriteString(failure)
	} else {
		fmt.Fprintf(&b, "completed %d action(s).", len(p.Actions))
	}
	if status != core.EffectNotApplied {
		s.settle(page)
	}
	if note := s.blockedNote(s.takeBlocked()); note != "" {
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(note))
	}
	if snap, err := s.renderPage(page, modeInteractive, "", 1); err != nil {
		if ctx.Err() != nil {
			return core.ToolResult{Effect: core.EffectReport{Status: status}}, ctx.Err()
		}
		fmt.Fprintf(&b, "\n\n(snapshot unavailable: %v)", err)
	} else {
		b.WriteString("\n\n")
		b.WriteString(snap)
	}
	result := core.TextResult(b.String())
	result.IsError = failure != ""
	result.Effect = core.EffectReport{Status: status}
	return result, nil
}

// validate checks an action's shape and ref before anything is dispatched.
func (s *Session) validate(a action) error {
	if a.Count != 0 && a.Op != "press" {
		return errors.New("count applies only to press")
	}
	switch a.Op {
	case "click", "hover":
		if a.Ref == "" {
			return errors.New("ref is required")
		}
	case "type":
		if a.Ref == "" && a.Text == "" {
			return errors.New("type without a ref needs text")
		}
	case "select":
		if a.Ref == "" || a.Text == "" {
			return errors.New("ref and text are required")
		}
	case "press":
		if a.Count < 0 || a.Count > maxPressCount {
			return fmt.Errorf("count must be between 1 and %d", maxPressCount)
		}
		if _, _, err := parseKey(a.Text); err != nil {
			return err
		}
	case "scroll":
		if a.Ref == "" {
			switch strings.ToLower(strings.TrimSpace(a.Text)) {
			case "", "up", "down":
			default:
				return fmt.Errorf("scroll text must be up or down, not %q", a.Text)
			}
		}
	default:
		return fmt.Errorf("unknown op %q", a.Op)
	}
	if a.Ref != "" {
		if _, err := s.refs.lookup(a.Ref); err != nil {
			return err
		}
	}
	return nil
}

// do performs one action. dispatched reports that input may have reached the
// page; errors with dispatched false happened before any input was sent.
func (s *Session) do(page *rod.Page, a action) (dispatched bool, err error) {
	var el *rod.Element
	if a.Ref != "" {
		if el, err = s.element(page, a.Ref); err != nil {
			return false, err
		}
	}
	switch a.Op {
	case "click", "hover":
		if err := el.ScrollIntoView(); err != nil {
			return false, err
		}
		pt, err := el.Interactable()
		if err != nil {
			return false, interactError(err)
		}
		if err := page.Mouse.MoveTo(*pt); err != nil {
			return true, err
		}
		if a.Op == "click" {
			return true, page.Mouse.Click(proto.InputMouseButtonLeft, 1)
		}
		return true, nil

	case "type":
		if el == nil {
			return typeKeys(page, a.Text)
		}
		editable, err := el.Eval(`() => this.isContentEditable ||
			((this.tagName === 'INPUT' || this.tagName === 'TEXTAREA') && !this.disabled && !this.readOnly)`)
		if err != nil {
			return false, err
		}
		if !editable.Value.Bool() {
			return false, errors.New("the element is not an editable text field")
		}
		if err := el.Focus(); err != nil {
			return false, err
		}
		if err := el.SelectAllText(); err != nil {
			return true, err
		}
		if a.Text == "" {
			return true, page.Keyboard.Type(input.Backspace)
		}
		return true, page.InsertText(a.Text)

	case "select":
		res, err := el.Eval(`(text) => {
			if (this.tagName !== 'SELECT') return 'not a select element; click the option instead';
			if (this.disabled) return 'the select is disabled';
			const want = text.trim();
			const opt = Array.from(this.options).find(o => o.text.trim() === want || o.value === want);
			if (!opt) return 'no option ' + JSON.stringify(want) + '; options: ' +
				Array.from(this.options).map(o => JSON.stringify(o.text.trim())).join(', ');
			opt.selected = true;
			this.dispatchEvent(new Event('input', {bubbles: true}));
			this.dispatchEvent(new Event('change', {bubbles: true}));
			return '';
		}`, a.Text)
		if err != nil {
			return true, err
		}
		if msg := res.Value.Str(); msg != "" {
			return false, errors.New(msg)
		}
		return true, nil

	case "press":
		mods, key, _ := parseKey(a.Text)
		if el != nil {
			if err := el.Focus(); err != nil {
				return false, err
			}
		}
		keys := make([]input.Key, max(a.Count, 1))
		for i := range keys {
			keys[i] = key
		}
		return true, page.KeyActions().Press(mods...).Type(keys...).Do()

	case "scroll":
		if el != nil {
			return true, el.ScrollIntoView()
		}
		dir := 1
		if strings.EqualFold(strings.TrimSpace(a.Text), "up") {
			dir = -1
		}
		_, err := page.Eval(`(dir) => window.scrollBy(0, dir * window.innerHeight * 0.8)`, dir)
		return true, err
	}
	return false, fmt.Errorf("unknown op %q", a.Op)
}

// element resolves a ref to a live element, reporting stale refs.
func (s *Session) element(page *rod.Page, ref string) (*rod.Element, error) {
	id, err := s.refs.lookup(ref)
	if err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	node, err := proto.DOMResolveNode{BackendNodeID: proto.DOMBackendNodeID(id)}.Call(page)
	if err != nil {
		if page.GetContext().Err() != nil {
			return nil, page.GetContext().Err()
		}
		return nil, staleRefError(ref)
	}
	el, err := page.ElementFromObject(node.Object)
	if err != nil {
		return nil, err
	}
	connected, err := el.Eval(`() => this.isConnected`)
	if err != nil {
		return nil, err
	}
	if !connected.Value.Bool() {
		return nil, staleRefError(ref)
	}
	return el, nil
}

func interactError(err error) error {
	var covered *rod.CoveredError
	if errors.As(err, &covered) {
		return errors.New("the element is covered by another element; close overlays or scroll, then retry")
	}
	var invisible *rod.InvisibleShapeError
	if errors.As(err, &invisible) {
		return errors.New("the element has no visible area")
	}
	var noPointer *rod.NoPointerEventsError
	if errors.As(err, &noPointer) {
		return errors.New("the element does not accept pointer events")
	}
	return err
}

// settle waits briefly for a navigation an action may have started.
func (s *Session) settle(page *rod.Page) {
	ctx := page.GetContext()
	wait := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	if !wait(settleDelay) {
		return
	}
	deadline := time.Now().Add(s.cfg.ActionTimeout)
	for time.Now().Before(deadline) {
		timed := page.Timeout(time.Second)
		res, err := timed.Eval(`() => document.readyState`)
		timed.CancelTimeout()
		if err == nil && res.Value.Str() == "complete" {
			return
		}
		if !wait(100 * time.Millisecond) {
			return
		}
	}
}

// typeKeys types text into the focused element one key at a time, so custom
// widgets receive real key events. Characters without a key are inserted as
// text. Every key is a dispatch, so any failure is reported as dispatched.
func typeKeys(page *rod.Page, text string) (dispatched bool, err error) {
	for _, r := range text {
		switch k := input.Key(r); {
		case r == '\n':
			err = page.Keyboard.Type(input.Enter)
		case keyDefined(k):
			err = page.Keyboard.Type(k)
		default:
			err = page.InsertText(string(r))
		}
		if err != nil {
			return true, err
		}
	}
	return true, nil
}

var namedKeys = map[string]input.Key{
	"enter": input.Enter, "tab": input.Tab, "escape": input.Escape, "esc": input.Escape,
	"backspace": input.Backspace, "delete": input.Delete, "insert": input.Insert, "space": input.Space,
	"plus": input.NumpadAdd, "arrowup": input.ArrowUp, "arrowdown": input.ArrowDown,
	"arrowleft": input.ArrowLeft, "arrowright": input.ArrowRight,
	"home": input.Home, "end": input.End, "pageup": input.PageUp, "pagedown": input.PageDown,
	"f1": input.F1, "f2": input.F2, "f3": input.F3, "f4": input.F4, "f5": input.F5, "f6": input.F6,
	"f7": input.F7, "f8": input.F8, "f9": input.F9, "f10": input.F10, "f11": input.F11, "f12": input.F12,
}

var modifierKeys = map[string]input.Key{
	"control": input.ControlLeft, "ctrl": input.ControlLeft,
	"shift": input.ShiftLeft, "alt": input.AltLeft,
	"meta": input.MetaLeft, "cmd": input.MetaLeft, "command": input.MetaLeft,
}

// parseKey parses "Enter", "a", "Control+Shift+a", or a plus key written as
// "Plus", "+", or a trailing "++" ("Control++").
func parseKey(spec string) (mods []input.Key, key input.Key, err error) {
	spec = strings.TrimSpace(spec)
	modSpec, last := "", spec
	switch {
	case spec == "+":
	case strings.HasSuffix(spec, "++"):
		modSpec, last = spec[:len(spec)-2], "+"
	case strings.Contains(spec, "+"):
		i := strings.LastIndex(spec, "+")
		modSpec, last = spec[:i], strings.TrimSpace(spec[i+1:])
	}
	if last == "" {
		return nil, 0, errors.New("press needs a key in text, such as Enter or Control+a")
	}
	if modSpec != "" {
		for m := range strings.SplitSeq(modSpec, "+") {
			k, ok := modifierKeys[strings.ToLower(strings.TrimSpace(m))]
			if !ok {
				return nil, 0, fmt.Errorf("unknown modifier %q", m)
			}
			mods = append(mods, k)
		}
	}
	if k, ok := namedKeys[strings.ToLower(last)]; ok {
		return mods, k, nil
	}
	if r := []rune(last); len(r) == 1 && keyDefined(input.Key(r[0])) {
		key = input.Key(r[0])
		// rod sends a printable key's own text; Shift must pick the shifted key.
		if slices.Contains(mods, input.ShiftLeft) {
			if shifted, ok := key.Shift(); ok {
				key = shifted
			}
		}
		return mods, key, nil
	}
	return nil, 0, fmt.Errorf("unknown key %q", last)
}

func keyDefined(k input.Key) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	k.Info()
	return true
}
