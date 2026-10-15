package browser

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/input"
)

// These tests exercise pruning and formatting on synthetic trees, so they
// run without a browser.

var viewport = rect{0, 0, 1000, 800}

func box(y float64) *rect { return &rect{10, y, 100, 20} }

func el(role, name string, y float64, kids ...*axNode) *axNode {
	return &axNode{Role: role, Name: name, Bounds: box(y), Children: kids, BackendID: nextBackend()}
}

var backendSeq int

func nextBackend() int { backendSeq++; return backendSeq }

func renderText(t *testing.T, root *axNode, opts renderOptions) string {
	t.Helper()
	if opts.Mode == "" {
		opts.Mode = modeInteractive
	}
	if opts.Viewport == (rect{}) {
		opts.Viewport = viewport
	}
	out, err := formatSnapshot(render(root, newRefRegistry(), opts), 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRenderHoistsAndCollapses(t *testing.T) {
	generic := func(kids ...*axNode) *axNode { return &axNode{Role: "generic", Children: kids} }
	root := &axNode{Role: "RootWebArea", Name: "Page", Children: []*axNode{
		generic(generic(generic(el("button", "Deep", 10)))),
		{Role: "list", Children: []*axNode{{Role: "listitem", Children: []*axNode{el("link", "Only", 40)}}}},
		{Role: "navigation", Name: "Site", Children: []*axNode{el("link", "A", 70), el("link", "B", 90)}},
		{Role: "navigation", Name: "Empty"},
		{Role: "generic", Ignored: true, Children: []*axNode{el("checkbox", "Ignored parent", 120)}},
	}}
	got := renderText(t, root, renderOptions{})
	want := `[e1] button "Deep"
[e2] link "Only"
navigation "Site"
  [e3] link "A"
  [e4] link "B"
[e5] checkbox "Ignored parent"
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderFocusableGeneric(t *testing.T) {
	focusable := func(n *axNode) *axNode { n.Props = map[string]string{"focusable": "true"}; return n }
	text := func(s string, y float64) *axNode { return &axNode{Role: "StaticText", Name: s, Bounds: box(y)} }
	root := &axNode{Role: "RootWebArea", Children: []*axNode{
		focusable(el("generic", "", 0, text("Save", 0), el("generic", "", 0, text("draft", 0)))),
		focusable(el("generic", "Close panel", 30)),
		focusable(el("generic", "", 60)),                              // no name: no ref
		el("generic", "", 90, text("Not focusable", 90)),              // no ref
		focusable(el("generic", "", 120, el("button", "Inner", 120))), // wrapper: only the button
		focusable(el("generic", "", 900, text("Below", 900))),         // off screen
	}}
	got := renderText(t, root, renderOptions{Mode: modeContent})
	want := `[e1] generic "Save draft"
[e2] generic "Close panel"
text "Not focusable"
[e3] button "Inner"
-- below the viewport: 1 interactive element (scroll down to reveal)
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderDropsInvisible(t *testing.T) {
	root := &axNode{Role: "RootWebArea", Children: []*axNode{
		{Role: "button", Name: "No layout"},
		{Role: "button", Name: "Zero", Bounds: &rect{0, 0, 0, 0}},
		{Role: "button", Name: "Ignored", Ignored: true, Bounds: box(0)},
		el("button", "Shown", 0),
		{Role: "StaticText", Name: "hidden text"},
	}}
	got := renderText(t, root, renderOptions{Mode: modeContent})
	if got != "[e1] button \"Shown\"\n" {
		t.Fatalf("got:\n%s", got)
	}
}

func TestRenderStatesAndTruncation(t *testing.T) {
	long := strings.Repeat("word ", 30)
	textbox := el("textbox", "Email", 0)
	textbox.Value = "a@b.c"
	textbox.Props = map[string]string{"required": "true", "focused": "true", "invalid": "false"}
	check := el("checkbox", "Agree", 30)
	check.Props = map[string]string{"checked": "mixed", "disabled": "true"}
	heading := el("heading", "Title", 60)
	heading.Props = map[string]string{"level": "2"}
	tab := el("tab", "Tab", 90)
	tab.Props = map[string]string{"selected": "true", "expanded": "false", "checked": "false"}
	root := &axNode{Role: "RootWebArea", Children: []*axNode{textbox, check, heading, tab, el("button", long, 120)}}

	got := renderText(t, root, renderOptions{})
	want := `[e1] textbox "Email" value="a@b.c" (required, focused)
[e2] checkbox "Agree" (disabled, mixed)
heading "Title" (level 2)
[e3] tab "Tab" (selected, collapsed)
[e4] button "` + long[:79] + `…"
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderViewportHint(t *testing.T) {
	root := &axNode{Role: "RootWebArea", Children: []*axNode{
		el("heading", "Above", -400),
		el("link", "Up", -300),
		el("button", "Here", 100),
		el("heading", "Below 1", 900),
		el("link", "Down", 1000),
		el("button", "Further", 2000),
	}}
	got := renderText(t, root, renderOptions{})
	want := `[e1] button "Here"
-- above the viewport: 1 interactive element; headings: "Above" (scroll up to reveal)
-- below the viewport: 2 interactive elements; headings: "Below 1" (scroll down to reveal)
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if full := renderText(t, root, renderOptions{Mode: modeFull, Unbounded: true}); strings.Contains(full, "--") || !strings.Contains(full, "Further") {
		t.Fatalf("full mode should show everything without hints:\n%s", full)
	}
}

func TestRenderCollapsesRepeatedSiblings(t *testing.T) {
	items := func(n int) []*axNode {
		var out []*axNode
		for i := range n {
			out = append(out, &axNode{Role: "listitem", Children: []*axNode{el("link", fmt.Sprint("Item ", i+1), float64(i*20))}})
		}
		return out
	}
	list := &axNode{Role: "list", Children: items(repeatThreshold + 2)}
	got := renderText(t, &axNode{Role: "RootWebArea", Children: []*axNode{list}}, renderOptions{Viewport: rect{0, 0, 1000, 10000}})
	if !strings.Contains(got, `[e5] link "Item 5"`) || strings.Contains(got, "Item 6") ||
		!strings.Contains(got, "  … (22 items, showing 5; use mode=full to see all)") {
		t.Fatalf("got:\n%s", got)
	}

	short := &axNode{Role: "list", Children: items(repeatThreshold)}
	if got := renderText(t, &axNode{Role: "RootWebArea", Children: []*axNode{short}}, renderOptions{Viewport: rect{0, 0, 1000, 10000}}); strings.Contains(got, "showing") {
		t.Fatalf("%d items should not collapse:\n%s", repeatThreshold, got)
	}
}

func TestRenderContentDedupesText(t *testing.T) {
	label := &axNode{Role: "LabelText", Children: []*axNode{{Role: "StaticText", Name: "Email", Bounds: box(0)}}}
	link := el("link", "Read more", 30, &axNode{Role: "StaticText", Name: "Read more", Bounds: box(30)})
	para := &axNode{Role: "paragraph", Children: []*axNode{{Role: "StaticText", Name: "Body  copy", Bounds: box(60)}}}
	root := &axNode{Role: "RootWebArea", Children: []*axNode{label, el("textbox", "Email", 0), link, para, el("image", "", 90), el("image", "Logo", 90)}}
	got := renderText(t, root, renderOptions{Mode: modeContent})
	want := `[e1] textbox "Email"
[e2] link "Read more"
text "Body copy"
image "Logo"
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatSnapshotPaging(t *testing.T) {
	res := snapshotResult{Lines: []string{"a", "b", "c", "d", "e"}}
	got, err := formatSnapshot(res, 2, 2)
	if err != nil || got != "c\nd\n-- page 2 of 3 (pass page=3 for more)\n" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, _ := formatSnapshot(res, 3, 2); got != "e\n-- page 3 of 3\n" {
		t.Fatalf("last page %q", got)
	}
	if _, err := formatSnapshot(res, 4, 2); err == nil {
		t.Fatal("page 4 should be out of range")
	}
	if got, _ := formatSnapshot(snapshotResult{}, 1, 2); got != "(no visible elements in this view)\n" {
		t.Fatalf("empty %q", got)
	}
}

func TestRefRegistry(t *testing.T) {
	r := newRefRegistry()
	r.observe("doc1")
	a, b := r.ref(100), r.ref(200)
	if a != "e1" || b != "e2" || r.ref(100) != "e1" {
		t.Fatalf("refs %s %s", a, b)
	}
	if id, err := r.lookup(" e2 "); err != nil || id != 200 {
		t.Fatalf("lookup e2 = %d, %v", id, err)
	}

	r.observe("doc1")
	if _, err := r.lookup("e1"); err != nil {
		t.Fatalf("same document must keep refs: %v", err)
	}
	r.observe("doc2")
	var stale staleRefError
	if _, err := r.lookup("e1"); !errors.As(err, &stale) {
		t.Fatalf("ref from the previous document: %v", err)
	}
	if got := r.ref(100); got != "e3" {
		t.Fatalf("numbering must not restart after navigation, got %s", got)
	}
	if _, err := r.lookup("e9"); err == nil || !strings.Contains(err.Error(), "never issued") {
		t.Fatalf("unissued: %v", err)
	}
	if _, err := r.lookup("button"); err == nil || !strings.Contains(err.Error(), "not a snapshot ref") {
		t.Fatalf("malformed: %v", err)
	}
}

func TestCheckURL(t *testing.T) {
	s := &Session{hosts: []string{"example.com", "*.docs.example.org", "LOCALHOST:8080"}}
	for raw, ok := range map[string]bool{
		"https://example.com/a":          true,
		"http://EXAMPLE.com.:8443/":      true,
		"https://sub.example.com/":       false,
		"https://a.docs.example.org/x":   true,
		"https://docs.example.org/":      false,
		"https://evil-docs.example.org/": false,
		"http://localhost:3000/":         true,
		"file:///etc/passwd":             false,
		"javascript:alert(1)":            false,
		"ftp://example.com/":             false,
		"https:///nohost":                false,
		"chrome://settings":              false,
	} {
		if err := s.checkURL(raw); (err == nil) != ok {
			t.Errorf("checkURL(%q) = %v, want allowed=%v", raw, err, ok)
		}
	}
	closed := &Session{}
	if err := closed.checkURL("https://anything.test/"); err == nil || !strings.Contains(err.Error(), "browser.Config.AllowedHosts") {
		t.Errorf("empty AllowedHosts must block navigation and name the setting: %v", err)
	}
	asking := &Session{cfg: Config{RequestAccess: true}}
	if err := asking.checkURL("https://anything.test/"); err == nil || !strings.Contains(err.Error(), "call browser_request_access") {
		t.Errorf("with RequestAccess the error must point the model to browser_request_access: %v", err)
	}
	if err := closed.checkURL("file:///tmp/x"); err == nil {
		t.Error("file URLs must be rejected")
	}
}

func TestParseKey(t *testing.T) {
	mods, key, err := parseKey("Control+Shift+a")
	if err != nil || len(mods) != 2 || key != input.Key('A') {
		t.Fatalf("Control+Shift+a = %v %q %v", mods, key, err)
	}
	if _, key, err := parseKey("enter"); err != nil || key != input.Enter {
		t.Fatalf("enter = %q %v", key, err)
	}
	for spec, want := range map[string]input.Key{"F5": input.F5, "insert": input.Insert, "Plus": input.NumpadAdd, "+": input.NumpadAdd} {
		if mods, key, err := parseKey(spec); err != nil || len(mods) != 0 || key != want {
			t.Errorf("%s = %v %q %v", spec, mods, key, err)
		}
	}
	for _, spec := range []string{"Control++", "Control+Plus", "Control+Shift++"} {
		if mods, key, err := parseKey(spec); err != nil || len(mods) == 0 || mods[0] != input.ControlLeft || key != input.NumpadAdd {
			t.Errorf("%s = %v %q %v", spec, mods, key, err)
		}
	}
	for _, bad := range []string{"", "Control+", "Hyper+a", "NotAKey", "é", "Hyper++"} {
		if _, _, err := parseKey(bad); err == nil {
			t.Errorf("parseKey(%q) should fail", bad)
		}
	}
}

func TestValidateRejectsBeforeDispatch(t *testing.T) {
	s := &Session{refs: newRefRegistry()}
	ref := s.refs.ref(7)
	for _, a := range []action{
		{Op: "click"},
		{Op: "select", Ref: ref},
		{Op: "press", Text: "Nope"},
		{Op: "scroll", Text: "sideways"},
		{Op: "drag", Ref: ref},
		{Op: "hover", Ref: "e42"},
		{Op: "type"},
		{Op: "press", Text: "Tab", Count: maxPressCount + 1},
		{Op: "press", Text: "Tab", Count: -1},
		{Op: "click", Ref: ref, Count: 2},
		{Op: "back", Ref: ref},
		{Op: "back", Text: "1"},
	} {
		if err := s.validate(a); err == nil {
			t.Errorf("validate(%+v) should fail", a)
		}
	}
	for _, a := range []action{
		{Op: "click", Ref: ref}, {Op: "type", Ref: ref}, {Op: "press", Text: "Tab"},
		{Op: "scroll"}, {Op: "scroll", Text: "Up"}, {Op: "select", Ref: ref, Text: "Pro"},
		{Op: "type", Text: "hi"}, {Op: "press", Text: "ArrowDown", Count: maxPressCount},
		{Op: "back"},
	} {
		if err := s.validate(a); err != nil {
			t.Errorf("validate(%+v): %v", a, err)
		}
	}
}
