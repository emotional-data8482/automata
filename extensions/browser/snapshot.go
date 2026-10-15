package browser

import (
	"fmt"
	"strings"
)

// Snapshot modes accepted by browser_snapshot.
const (
	modeInteractive = "interactive"
	modeContent     = "content"
	modeFull        = "full"
)

const (
	// maxNameRunes truncates accessible names and values in snapshot lines.
	maxNameRunes = 80
	// repeatThreshold is the number of same-role visible siblings above which
	// a run of siblings is collapsed to its first repeatShow items.
	repeatThreshold = 20
	repeatShow      = 5
	// maxHintHeadings bounds the off-viewport heading hint.
	maxHintHeadings = 5
)

// rect is a CSS-pixel box in document coordinates.
type rect struct{ X, Y, W, H float64 }

func (r rect) empty() bool { return r.W <= 0 || r.H <= 0 }

func (r rect) intersects(o rect) bool {
	return r.X < o.X+o.W && o.X < r.X+r.W && r.Y < o.Y+o.H && o.Y < r.Y+r.H
}

// axNode is the provider-neutral accessibility node the renderer consumes.
// The session builds it from Chrome's AX tree joined with DOM layout boxes;
// tests build it directly.
type axNode struct {
	Role      string
	Name      string
	Value     string
	Props     map[string]string
	Ignored   bool
	BackendID int
	// Bounds is nil when the DOM node has no layout box (display:none,
	// detached, or a node without DOM backing).
	Bounds   *rect
	Children []*axNode
}

func (n *axNode) prop(name string) string { return n.Props[name] }

func (n *axNode) visible() bool { return n.Bounds != nil && !n.Bounds.empty() }

var interactiveRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true,
	"checkbox": true, "radio": true, "combobox": true, "listbox": true,
	"option": true, "menuitem": true, "menuitemcheckbox": true,
	"menuitemradio": true, "tab": true, "switch": true, "slider": true,
	"spinbutton": true, "treeitem": true,
}

// structuralRoles survive pruning (without refs) when they contain kept
// descendants, so the model sees where controls sit on the page.
var structuralRoles = map[string]bool{
	"navigation": true, "main": true, "banner": true, "contentinfo": true,
	"complementary": true, "form": true, "search": true, "region": true,
	"dialog": true, "alertdialog": true, "list": true, "table": true,
	"row": true, "tablist": true, "menu": true, "menubar": true,
	"radiogroup": true, "group": true, "tree": true, "grid": true,
	"alert": true,
}

// valueRoles render their current value.
var valueRoles = map[string]bool{
	"textbox": true, "searchbox": true, "combobox": true, "slider": true,
	"spinbutton": true,
}

// renderOptions selects what a snapshot shows.
type renderOptions struct {
	Mode string
	// Viewport limits interactive and content modes to what is on screen.
	Viewport rect
	// Unbounded disables viewport limiting and sibling collapsing; full mode
	// and within-ref snapshots set it.
	Unbounded bool
}

// outNode is a pruned node ready to print. A node with only Text set is a
// synthetic marker line.
type outNode struct {
	n    *axNode
	ref  string
	name string // overrides n.Name when set
	kids []*outNode
	text string
}

// snapshotResult is the rendered tree plus what was left off screen.
type snapshotResult struct {
	Lines         []string
	AboveCount    int
	BelowCount    int
	AboveHeadings []string
	BelowHeadings []string
}

type renderer struct {
	opts renderOptions
	refs *refRegistry
	res  snapshotResult
}

// render prunes root into indented snapshot lines, assigning refs to
// interactive nodes through refs.
func render(root *axNode, refs *refRegistry, opts renderOptions) snapshotResult {
	r := &renderer{opts: opts, refs: refs}
	for _, o := range r.prune(root, "") {
		r.print(o, 0)
	}
	return r.res
}

func (r *renderer) print(o *outNode, depth int) {
	indent := strings.Repeat("  ", depth)
	if o.n == nil {
		r.res.Lines = append(r.res.Lines, indent+o.text)
		return
	}
	r.res.Lines = append(r.res.Lines, indent+formatLine(o))
	for _, k := range o.kids {
		r.print(k, depth+1)
	}
}

// formatLine renders `[e6] button "Save" (disabled)`.
func formatLine(o *outNode) string {
	n := o.n
	var b strings.Builder
	if o.ref != "" {
		fmt.Fprintf(&b, "[%s] ", o.ref)
	}
	role := n.Role
	if role == "StaticText" {
		role = "text"
	}
	b.WriteString(role)
	name := n.Name
	if o.name != "" {
		name = o.name
	}
	if name := clip(name); name != "" {
		fmt.Fprintf(&b, " %q", name)
	}
	if valueRoles[n.Role] {
		if v := clip(n.Value); v != "" && v != clip(n.Name) {
			fmt.Fprintf(&b, " value=%q", v)
		}
	}
	if states := states(n); len(states) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(states, ", "))
	}
	return b.String()
}

// states lists only non-default states, in a fixed order.
func states(n *axNode) []string {
	var out []string
	if lvl := n.prop("level"); lvl != "" && n.Role == "heading" {
		out = append(out, "level "+lvl)
	}
	if n.prop("disabled") == "true" {
		out = append(out, "disabled")
	}
	switch n.prop("checked") {
	case "true":
		out = append(out, "checked")
	case "mixed":
		out = append(out, "mixed")
	}
	switch n.prop("pressed") {
	case "true":
		out = append(out, "pressed")
	case "mixed":
		out = append(out, "mixed")
	}
	if n.prop("selected") == "true" {
		out = append(out, "selected")
	}
	switch n.prop("expanded") {
	case "true":
		out = append(out, "expanded")
	case "false":
		out = append(out, "collapsed")
	}
	if n.prop("required") == "true" {
		out = append(out, "required")
	}
	if n.prop("readonly") == "true" {
		out = append(out, "readonly")
	}
	if inv := n.prop("invalid"); inv != "" && inv != "false" {
		out = append(out, "invalid")
	}
	if n.prop("focused") == "true" {
		out = append(out, "focused")
	}
	return out
}

// clip collapses whitespace and truncates to maxNameRunes.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxNameRunes {
		return string(r[:maxNameRunes-1]) + "…"
	}
	return s
}

// onScreen reports whether a visible leaf-like node should be printed, and
// records it in the off-viewport hint otherwise.
func (r *renderer) onScreen(n *axNode) bool {
	if r.opts.Unbounded || n.Bounds.intersects(r.opts.Viewport) {
		return true
	}
	above := n.Bounds.Y+n.Bounds.H <= r.opts.Viewport.Y
	control := interactiveRoles[n.Role] || n.Role == "generic" // generics get here only as focusable controls
	switch {
	case control && above:
		r.res.AboveCount++
	case control:
		r.res.BelowCount++
	case n.Role == "heading" && above:
		r.res.AboveHeadings = append(r.res.AboveHeadings, clip(n.Name))
	case n.Role == "heading":
		r.res.BelowHeadings = append(r.res.BelowHeadings, clip(n.Name))
	}
	return false
}

// prune returns the printable nodes for n's subtree. Nodes that are not kept
// hoist their children into the parent. ancestorName suppresses text that an
// enclosing control or heading already names.
func (r *renderer) prune(n *axNode, ancestorName string) []*outNode {
	switch n.Role {
	case "InlineTextBox", "LineBreak":
		return nil
	}
	if n.Ignored {
		return r.pruneChildren(n, ancestorName)
	}

	switch {
	case interactiveRoles[n.Role]:
		if !n.visible() || !r.onScreen(n) {
			return nil
		}
		ref := r.refs.ref(n.BackendID)
		return []*outNode{{n: n, ref: ref, kids: r.pruneChildren(n, n.Name)}}

	case n.Role == "generic" && n.prop("focusable") == "true" && !hasControl(n.Children):
		// A custom control such as <div tabindex=0>Save</div>. Chrome gives a
		// generic no name from its content, so use its visible text.
		name := n.Name
		if strings.TrimSpace(name) == "" {
			name = visibleText(n)
		}
		if name == "" {
			return r.pruneChildren(n, ancestorName)
		}
		if !n.visible() || !r.onScreen(n) {
			return nil
		}
		ref := r.refs.ref(n.BackendID)
		return []*outNode{{n: n, ref: ref, name: name, kids: r.pruneChildren(n, name)}}

	case n.Role == "heading":
		if !n.visible() || !r.onScreen(n) {
			return nil
		}
		return []*outNode{{n: n, kids: r.pruneChildren(n, n.Name)}}

	case n.Role == "StaticText":
		if r.opts.Mode == modeInteractive || !n.visible() {
			return nil
		}
		name := strings.Join(strings.Fields(n.Name), " ")
		if name == "" || (ancestorName != "" && strings.Contains(ancestorName, name)) {
			return nil
		}
		if !r.onScreen(n) {
			return nil
		}
		return []*outNode{{n: n}}

	case n.Role == "image" || n.Role == "img":
		if r.opts.Mode == modeInteractive || !n.visible() || strings.TrimSpace(n.Name) == "" {
			return nil
		}
		if !r.onScreen(n) {
			return nil
		}
		return []*outNode{{n: n}}

	case structuralRoles[n.Role]:
		kids := r.pruneChildren(n, ancestorName)
		if len(kids) == 0 {
			return nil
		}
		if strings.TrimSpace(n.Name) == "" && len(kids) == 1 {
			// Collapse unnamed single-child chains.
			return kids
		}
		return []*outNode{{n: n, kids: kids}}
	}
	return r.pruneChildren(n, ancestorName)
}

// hasControl reports whether any node in nodes or their subtrees is an
// interactive role or focusable. A focusable wrapper around controls (such as
// a tabindex=-1 app root) is not itself a control.
func hasControl(nodes []*axNode) bool {
	for _, c := range nodes {
		if !c.Ignored && (interactiveRoles[c.Role] || c.prop("focusable") == "true") {
			return true
		}
		if hasControl(c.Children) {
			return true
		}
	}
	return false
}

// visibleText joins the visible static text in n's subtree.
func visibleText(n *axNode) string {
	var parts []string
	var walk func(*axNode)
	walk = func(n *axNode) {
		if n.Role == "StaticText" && n.visible() {
			parts = append(parts, n.Name)
			return
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

// pruneChildren prunes each child and collapses long runs of same-role
// siblings to their first repeatShow items.
func (r *renderer) pruneChildren(n *axNode, ancestorName string) []*outNode {
	var out []*outNode
	var groups []int // child index of each produced group, for repeat detection
	var roles []string
	for _, c := range n.Children {
		kids := r.prune(c, ancestorName)
		if len(kids) == 0 {
			continue
		}
		groups = append(groups, len(out))
		roles = append(roles, c.Role)
		out = append(out, kids...)
	}
	out = dropLabelText(out)
	if r.opts.Unbounded || len(roles) <= repeatThreshold {
		return out
	}
	for _, role := range roles[1:] {
		if role != roles[0] {
			return out
		}
	}
	shown := out[:groups[repeatShow]]
	marker := &outNode{text: fmt.Sprintf("… (%d items, showing %d; use mode=full to see all)", len(roles), repeatShow)}
	return append(shown[:len(shown):len(shown)], marker)
}

// dropLabelText removes text lines that only repeat the name of an adjacent
// control, such as a <label> next to the field it names.
func dropLabelText(nodes []*outNode) []*outNode {
	names := func(i int) string {
		if i < 0 || i >= len(nodes) || nodes[i].ref == "" {
			return ""
		}
		return strings.Join(strings.Fields(nodes[i].n.Name), " ")
	}
	out := nodes[:0:0]
	for i, o := range nodes {
		if o.n != nil && o.n.Role == "StaticText" {
			text := strings.Join(strings.Fields(o.n.Name), " ")
			if prev, next := names(i-1), names(i+1); text != "" && (strings.HasPrefix(next, text) || strings.HasPrefix(prev, text)) {
				continue
			}
		}
		out = append(out, o)
	}
	return out
}

// formatSnapshot renders a page of snapshot lines with the off-viewport hint
// and paging footer. page is 1-based; maxLines bounds each page.
func formatSnapshot(res snapshotResult, page, maxLines int) (string, error) {
	lines := res.Lines
	pages := max(1, (len(lines)+maxLines-1)/maxLines)
	if page < 1 {
		page = 1
	}
	if page > pages {
		return "", fmt.Errorf("page %d is out of range: the snapshot has %d page(s)", page, pages)
	}
	start := (page - 1) * maxLines
	end := min(len(lines), start+maxLines)

	var b strings.Builder
	if len(lines) == 0 {
		b.WriteString("(no visible elements in this view)\n")
	}
	for _, l := range lines[start:end] {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if hint := offscreenHint("above", res.AboveCount, res.AboveHeadings); hint != "" {
		b.WriteString(hint)
	}
	if hint := offscreenHint("below", res.BelowCount, res.BelowHeadings); hint != "" {
		b.WriteString(hint)
	}
	if pages > 1 {
		fmt.Fprintf(&b, "-- page %d of %d", page, pages)
		if page < pages {
			fmt.Fprintf(&b, " (pass page=%d for more)", page+1)
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func offscreenHint(where string, count int, headings []string) string {
	if count == 0 && len(headings) == 0 {
		return ""
	}
	var parts []string
	if count > 0 {
		noun := "elements"
		if count == 1 {
			noun = "element"
		}
		parts = append(parts, fmt.Sprintf("%d interactive %s", count, noun))
	}
	if len(headings) > 0 {
		quoted := make([]string, 0, maxHintHeadings)
		for i, h := range headings {
			if i == maxHintHeadings {
				quoted = append(quoted, "…")
				break
			}
			quoted = append(quoted, fmt.Sprintf("%q", h))
		}
		parts = append(parts, "headings: "+strings.Join(quoted, ", "))
	}
	dir := "down"
	if where == "above" {
		dir = "up"
	}
	return fmt.Sprintf("-- %s the viewport: %s (scroll %s to reveal)\n", where, strings.Join(parts, "; "), dir)
}

// findBackend returns the node in root's subtree backed by DOM node id.
func findBackend(root *axNode, id int) *axNode {
	if root.BackendID == id && !root.Ignored {
		return root
	}
	for _, c := range root.Children {
		if found := findBackend(c, id); found != nil {
			return found
		}
	}
	return nil
}
