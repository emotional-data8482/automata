package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/emotional-data8482/automata/core"
	"github.com/emotional-data8482/automata/core/coretest"
)

var update = flag.Bool("update", false, "rewrite golden snapshot files")

// shared is the one browser process a test run launches. Each test gets its
// own page and Session on it.
var shared struct {
	once sync.Once
	proc *process
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.proc != nil {
		_ = shared.proc.close()
	}
	os.Exit(code)
}

// newSession opens a page on the run's shared headless browser. The browser
// is launched at most once per run; if that launch fails, every browser test
// skips instead of retrying. AUTOMATA_BROWSER_PATH selects the executable.
func newSession(t *testing.T, cfg Config) *Session {
	t.Helper()
	if testing.Short() {
		t.Skip("browser tests launch Chrome; skipped in -short mode")
	}
	shared.once.Do(func() {
		launchCfg, _ := normalize(Config{BrowserPath: os.Getenv("AUTOMATA_BROWSER_PATH")})
		shared.proc, shared.err = launch(context.Background(), launchCfg)
	})
	if errors.Is(shared.err, ErrNoBrowser) {
		t.Skip("browser tests need Chrome or Chromium and none was found; install one or set AUTOMATA_BROWSER_PATH")
	}
	if shared.err != nil {
		t.Skipf("browser tests skipped: the browser failed to start (one attempt per run, no retry): %v", shared.err)
	}
	if cfg.AllowedHosts == nil {
		cfg.AllowedHosts = []string{"127.0.0.1"} // the httptest fixtures' host
	}
	cfg, err := normalize(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := attach(t.Context(), shared.proc.browser, cfg)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// fixtures serves testdata. links.html points its "away" link at other.
type fixtures struct {
	*httptest.Server
	hits atomic.Int64 // requests to /secret
}

func newFixtures(t *testing.T, other string) *fixtures {
	t.Helper()
	f := &fixtures{}
	files := http.FileServer(http.Dir("testdata"))
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/secret":
			f.hits.Add(1)
			_, _ = w.Write([]byte("<title>Secret</title>secret"))
		case "/redirect":
			http.Redirect(w, r, other+"/secret", http.StatusFound)
		case "/links.html":
			data, err := os.ReadFile("testdata/links.html")
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(bytes.ReplaceAll(data, []byte("OTHER"), []byte(other)))
		default:
			files.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func tool(t *testing.T, s *Session, name string) core.Tool {
	t.Helper()
	for _, tl := range s.Tools() {
		if tl.Definition().Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %q", name)
	return nil
}

func run(t *testing.T, s *Session, name string, args any) core.ToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool(t, s, name).Execute(t.Context(), raw)
	if err != nil {
		t.Fatalf("%s: fatal error: %v", name, err)
	}
	return result
}

func navigate(t *testing.T, s *Session, url string) string {
	t.Helper()
	result := run(t, s, "browser_navigate", map[string]any{"url": url})
	if result.IsError {
		t.Fatalf("navigate %s: %s", url, result.Text())
	}
	return result.Text()
}

// golden compares got (with the server URL normalized) to testdata/golden/name.
func golden(t *testing.T, name, base, got string) {
	t.Helper()
	got = strings.ReplaceAll(got, base, "http://fixture")
	path := filepath.Join("testdata", "golden", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run with -update to accept)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// refFor returns the ref of the first snapshot line containing needle.
func refFor(t *testing.T, snapshot, needle string) string {
	t.Helper()
	for line := range strings.SplitSeq(snapshot, "\n") {
		if strings.Contains(line, needle) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				return line[1:strings.Index(line, "]")]
			}
		}
	}
	t.Fatalf("no ref for %q in:\n%s", needle, snapshot)
	return ""
}

func TestSnapshotGolden(t *testing.T) {
	s := newSession(t, Config{})
	srv := newFixtures(t, "http://unused.invalid")

	golden(t, "form_navigate", srv.URL, navigate(t, s, srv.URL+"/form.html"))
	golden(t, "form_content", srv.URL, run(t, s, "browser_snapshot", map[string]any{"mode": "content"}).Text())

	golden(t, "long_interactive", srv.URL, navigate(t, s, srv.URL+"/long.html"))
	golden(t, "long_content", srv.URL, run(t, s, "browser_snapshot", map[string]any{"mode": "content"}).Text())
	golden(t, "long_full", srv.URL, run(t, s, "browser_snapshot", map[string]any{"mode": "full"}).Text())

	golden(t, "list_interactive", srv.URL, navigate(t, s, srv.URL+"/list.html"))
}

func TestSnapshotPagingAndWithin(t *testing.T) {
	s := newSession(t, Config{MaxNodes: 10})
	srv := newFixtures(t, "http://unused.invalid")
	navigate(t, s, srv.URL+"/list.html")

	full := run(t, s, "browser_snapshot", map[string]any{"mode": "full", "page": 2})
	if full.IsError || !strings.Contains(full.Text(), "-- page 2 of 4 (pass page=3 for more)") {
		t.Fatalf("page 2: %s", full.Text())
	}
	if r := run(t, s, "browser_snapshot", map[string]any{"mode": "full", "page": 9}); !r.IsError {
		t.Fatalf("out-of-range page should fail: %s", r.Text())
	}

	first := run(t, s, "browser_snapshot", nil).Text()
	ref := refFor(t, first, `link "Result 1"`)
	within := run(t, s, "browser_snapshot", map[string]any{"within": ref})
	if within.IsError || !strings.Contains(within.Text(), "["+ref+`] link "Result 1"`) || strings.Contains(within.Text(), "Result 2") {
		t.Fatalf("within %s: %s", ref, within.Text())
	}
}

func TestActTypeSelectClick(t *testing.T) {
	s := newSession(t, Config{})
	srv := newFixtures(t, "http://unused.invalid")
	snap := navigate(t, s, srv.URL+"/form.html")

	result := run(t, s, "browser_act", map[string]any{"actions": []map[string]string{
		{"op": "type", "ref": refFor(t, snap, `textbox "Email"`), "text": "ada@example.com"},
		{"op": "type", "ref": refFor(t, snap, `textbox "Name"`), "text": "Grace"},
		{"op": "select", "ref": refFor(t, snap, `combobox "Plan"`), "text": "Pro"},
		{"op": "click", "ref": refFor(t, snap, `button "Continue`)},
	}})
	if result.IsError {
		t.Fatalf("act failed: %s", result.Text())
	}
	if result.Effect.Status != core.EffectApplied {
		t.Fatalf("effect = %q, want applied", result.Effect.Status)
	}
	text := result.Text()
	for _, want := range []string{"completed 4 action(s).", `value="ada@example.com"`, `value="Grace"`, `combobox "Plan" value="Pro"`} {
		if !strings.Contains(text, want) {
			t.Errorf("act result missing %q:\n%s", want, text)
		}
	}
	content := run(t, s, "browser_snapshot", map[string]any{"mode": "content"}).Text()
	if !strings.Contains(content, `text "Clicked with ada@example.com on Pro"`) {
		t.Errorf("click handler did not run:\n%s", content)
	}

	// The same refs survive the re-snapshot of an unchanged document.
	if got, want := refFor(t, text, `textbox "Email"`), refFor(t, snap, `textbox "Email"`); got != want {
		t.Errorf("ref changed across snapshots: %s -> %s", want, got)
	}

	press := run(t, s, "browser_act", map[string]any{"actions": []map[string]string{
		{"op": "type", "ref": refFor(t, snap, `textbox "Name"`), "text": ""},
		{"op": "press", "ref": refFor(t, snap, `textbox "Name"`), "text": "Shift+a"},
	}})
	if press.IsError || !strings.Contains(press.Text(), `textbox "Name" value="A" (focused)`) {
		t.Fatalf("clear + press: %s", press.Text())
	}
}

func TestActStaleAndInvalidRefs(t *testing.T) {
	s := newSession(t, Config{})
	srv := newFixtures(t, "http://unused.invalid")
	snap := navigate(t, s, srv.URL+"/form.html")
	cont := refFor(t, snap, `button "Continue`)

	unknown := run(t, s, "browser_act", map[string]any{"actions": []map[string]string{
		{"op": "click", "ref": refFor(t, snap, `button "Remove continue"`)},
		{"op": "click", "ref": "e999"},
	}})
	if !unknown.IsError || unknown.Effect.Status != core.EffectNotApplied || !strings.Contains(unknown.Text(), "actions[1]") {
		t.Fatalf("unissued ref: effect=%q %s", unknown.Effect.Status, unknown.Text())
	}

	stale := run(t, s, "browser_act", map[string]any{"actions": []map[string]string{
		{"op": "click", "ref": refFor(t, snap, `button "Remove continue"`)},
		{"op": "click", "ref": cont},
	}})
	if !stale.IsError || stale.Effect.Status != core.EffectApplied {
		t.Fatalf("stale ref: effect=%q %s", stale.Effect.Status, stale.Text())
	}
	if !strings.Contains(stale.Text(), "actions[1] (click "+cont+") failed: ref "+cont+" is stale") ||
		!strings.Contains(stale.Text(), "actions[0..0] completed.") {
		t.Fatalf("stale ref message: %s", stale.Text())
	}

	only := run(t, s, "browser_act", map[string]any{"actions": []map[string]string{{"op": "hover", "ref": cont}}})
	if !only.IsError || only.Effect.Status != core.EffectNotApplied {
		t.Fatalf("stale-only batch: effect=%q %s", only.Effect.Status, only.Text())
	}

	// Refs from an earlier document are stale after navigation.
	navigate(t, s, srv.URL+"/long.html")
	old := run(t, s, "browser_act", map[string]any{"actions": []map[string]string{{"op": "click", "ref": refFor(t, snap, `textbox "Email"`)}}})
	if !old.IsError || !strings.Contains(old.Text(), "is stale") || old.Effect.Status != core.EffectNotApplied {
		t.Fatalf("ref from previous page: effect=%q %s", old.Effect.Status, old.Text())
	}
}

func TestNavigationPolicy(t *testing.T) {
	other := newFixtures(t, "")
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	srv := newFixtures(t, otherURL)
	s := newSession(t, Config{AllowedHosts: []string{"127.0.0.1"}})

	for _, url := range []string{"file:///etc/passwd", "javascript:alert(1)", "data:text/html,hi", otherURL + "/secret", "http://"} {
		r := run(t, s, "browser_navigate", map[string]any{"url": url})
		if !r.IsError || r.Effect.Status != core.EffectNotApplied {
			t.Errorf("navigate %q: want not-applied error, got effect=%q %s", url, r.Effect.Status, r.Text())
		}
	}

	if r := run(t, s, "browser_navigate", map[string]any{"url": srv.URL + "/redirect"}); !r.IsError || !strings.Contains(r.Text(), "blocked") {
		t.Errorf("redirect to a disallowed host: %s", r.Text())
	}

	snap := navigate(t, s, srv.URL+"/links.html")
	run(t, s, "browser_act", map[string]any{"actions": []map[string]string{{"op": "click", "ref": refFor(t, snap, "Leave the allowed hosts")}}})
	if n := other.hits.Load(); n != 0 {
		t.Fatalf("disallowed host received %d request(s)", n)
	}

	snap = navigate(t, s, srv.URL+"/links.html")
	r := run(t, s, "browser_act", map[string]any{"actions": []map[string]string{{"op": "click", "ref": refFor(t, snap, `link "Stay"`)}}})
	if r.IsError || !strings.Contains(r.Text(), "title: Sign up") {
		t.Fatalf("allowed link click: %s", r.Text())
	}
}

func TestScreenshot(t *testing.T) {
	s := newSession(t, Config{})
	srv := newFixtures(t, "http://unused.invalid")
	navigate(t, s, srv.URL+"/long.html")

	size := func(t *testing.T, r core.ToolResult) (int, int) {
		t.Helper()
		if r.IsError {
			t.Fatalf("screenshot: %s", r.Text())
		}
		img, ok := r.Blocks[len(r.Blocks)-1].(core.ImageBlock)
		if !ok || img.MediaType != "image/png" {
			t.Fatalf("want a PNG image block, got %#v", r.Blocks)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(img.Data))
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Width, cfg.Height
	}

	if w, h := size(t, run(t, s, "browser_screenshot", nil)); w != 1024 || h != 640 {
		t.Errorf("viewport screenshot = %dx%d, want 1024x640", w, h)
	}
	if w, h := size(t, run(t, s, "browser_screenshot", map[string]any{"max_px": 200})); w != 200 || h != 125 {
		t.Errorf("max_px screenshot = %dx%d, want 200x125", w, h)
	}
	if w, h := size(t, run(t, s, "browser_screenshot", map[string]any{"full_page": true})); h != 1024 || w >= h {
		t.Errorf("full-page screenshot = %dx%d, want a 1024px-tall portrait image", w, h)
	}
	ask := refFor(t, run(t, s, "browser_snapshot", map[string]any{"mode": "full"}).Text(), `button "Ask a question"`)
	if w, h := size(t, run(t, s, "browser_screenshot", map[string]any{"ref": ask})); w < 40 || w > 300 || h < 10 || h > 60 {
		t.Errorf("element screenshot = %dx%d, want roughly button-sized", w, h)
	}
	if r := run(t, s, "browser_screenshot", map[string]any{"ref": ask, "full_page": true}); !r.IsError {
		t.Errorf("ref with full_page should fail")
	}
}

// TestToolsRunUnderRuntime checks that the tools' effect policies are valid
// and that a navigate/act/screenshot sequence completes a durable run.
func TestToolsRunUnderRuntime(t *testing.T) {
	s := newSession(t, Config{})
	srv := newFixtures(t, "http://unused.invalid")
	snap := navigate(t, s, srv.URL+"/form.html")

	provider := coretest.NewProvider(
		coretest.ToolCall("1", "browser_navigate", map[string]any{"url": srv.URL + "/form.html"}),
		coretest.ToolCall("2", "browser_act", map[string]any{"actions": []map[string]string{{"op": "click", "ref": refFor(t, snap, `button "Continue`)}}}),
		coretest.ToolCall("3", "browser_screenshot", map[string]any{"max_px": 64}),
		coretest.Text("done"),
	)
	runtime := coretest.NewRuntime(t, core.RuntimeConfig{})
	agent := coretest.NewAgent(t, provider, core.AgentConfig{Tools: s.Tools(), MaxTurns: 6})
	def := coretest.Register(t, runtime, "browser", "v1", agent)
	result, err := runtime.Run(t.Context(), def, "click continue")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Output != "done" {
		t.Fatalf("run output = %q", result.Output)
	}
}

// TestConcurrentCalls runs tools from parallel goroutines, as a tool batch
// may; the session serializes them.
func TestConcurrentCalls(t *testing.T) {
	s := newSession(t, Config{})
	srv := newFixtures(t, "http://unused.invalid")
	navigate(t, s, srv.URL+"/form.html")

	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := range 8 {
		wg.Go(func() {
			name, args := "browser_snapshot", map[string]any{"mode": "content"}
			if i%2 == 1 {
				name, args = "browser_screenshot", map[string]any{"max_px": 64}
			}
			raw, _ := json.Marshal(args)
			r, err := tool(t, s, name).Execute(t.Context(), raw)
			if err != nil || r.IsError {
				errs <- fmt.Sprintf("%s: %v %s", name, err, r.Text())
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if r := run(t, s, "browser_snapshot", nil); !r.IsError || r.Effect.Status != core.EffectNotApplied {
		t.Fatalf("call after Close: %+v", r)
	}
}

// TestEmptyAllowedHostsBlocksNavigation checks that navigation fails closed.
func TestEmptyAllowedHostsBlocksNavigation(t *testing.T) {
	s := newSession(t, Config{AllowedHosts: []string{}})
	srv := newFixtures(t, "http://unused.invalid")
	r := run(t, s, "browser_navigate", map[string]any{"url": srv.URL + "/form.html"})
	if !r.IsError || r.Effect.Status != core.EffectNotApplied || !strings.Contains(r.Text(), "AllowedHosts") {
		t.Fatalf("navigate with no allowed hosts: effect=%q %s", r.Effect.Status, r.Text())
	}
}
