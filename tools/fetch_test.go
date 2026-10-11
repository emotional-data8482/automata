package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fetchURL(t *testing.T, url string) (string, error) {
	t.Helper()
	return executeDomain(t, HTTPFetch(), context.Background(), fmt.Sprintf(`{"url":%q}`, url))
}

func TestHTTPFetchExtractsHTML(t *testing.T) {
	const page = `<!DOCTYPE html>
<html>
<head><title>Ignored Title</title><style>body { color: red }</style></head>
<body>
  <!-- a comment -->
  <h1>Hello</h1>
  <p>World &amp; <a href="/x">friends</a>.</p>
  <script>var secret = 1;</script>
  <ul><li>one</li><li>two</li></ul>
</body>
</html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	got, err := fetchURL(t, srv.URL)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	for _, want := range []string{"Hello", "World & friends.", "one", "two"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{"secret", "color: red", "Ignored Title", "<", "a comment"} {
		if strings.Contains(got, banned) {
			t.Errorf("output leaked %q:\n%s", banned, got)
		}
	}
	if strings.Contains(got, "Hello World") {
		t.Errorf("block tags did not break lines:\n%s", got)
	}
}

func TestHTTPFetchPlainTextAndJSONPassThrough(t *testing.T) {
	for _, tc := range []struct{ contentType, body string }{
		{"text/plain; charset=utf-8", "just plain text"},
		{"application/json", `{"k":"v"}`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", tc.contentType)
			fmt.Fprint(w, tc.body)
		}))
		got, err := fetchURL(t, srv.URL)
		srv.Close()
		if err != nil {
			t.Fatalf("%s: Execute: %v", tc.contentType, err)
		}
		if got != tc.body {
			t.Errorf("%s: got %q, want %q", tc.contentType, got, tc.body)
		}
	}
}

func TestHTTPFetchTruncatesLargeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("a", fetchMaxBytes+1000)))
	}))
	defer srv.Close()

	got, err := fetchURL(t, srv.URL)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(got, "[truncated") {
		t.Error("missing truncation marker")
	}
	if len(got) > fetchMaxBytes+100 {
		t.Errorf("output length %d not capped near %d", len(got), fetchMaxBytes)
	}
}

func TestHTTPFetchRejects(t *testing.T) {
	binary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0x00, 0x01, 0x02})
	}))
	defer binary.Close()
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()

	for name, url := range map[string]string{
		"non-http scheme": "ftp://example.com/file",
		"missing host":    "http:///nope",
		"binary body":     binary.URL,
		"non-2xx status":  notFound.URL,
	} {
		if _, err := fetchURL(t, url); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

func TestExtractTextCaseFoldingDoesNotShiftOffsets(t *testing.T) {
	// U+0130 lowercases from two bytes to one, so a lowercased copy of the
	// document is shorter and byte offsets into it no longer line up.
	doc := strings.Repeat("İ", 20) + "<p>hello</p><script>secret()</script>world"
	got := extractText(doc)
	if strings.Contains(got, "secret") {
		t.Errorf("script contents leaked:\n%s", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("text around script missing:\n%q", got)
	}
}

func TestExtractTextUnclosedSkipContainer(t *testing.T) {
	if got := extractText("<p>before</p><script>var x = 1;"); got != "before" {
		t.Errorf("got %q, want %q", got, "before")
	}
}

func TestExtractTextHeadBoundaryIgnoresLiterals(t *testing.T) {
	for name, head := range map[string]string{
		"script":                      `<script>const markup = "<body>"; const secret = "hidden";</script>`,
		"comment":                     `<!-- <body> hidden comment -->`,
		"style":                       `<style>p::before { content: "<body>"; }</style>`,
		"title":                       `<title>Literal <body> title</title>`,
		"attribute":                   `<meta content='<body> hidden'>`,
		"attribute with greater-than": `<meta content='> <body> hidden'>`,
		"tag prefix":                  `<bodyguard>hidden`,
		"head close in script":        `<script>const markup = "</head><body>";</script>`,
		"head close in comment":       `<!-- </head><body> hidden -->`,
		"unicode":                     `<title>İ Hidden title</title><!-- <BODY> -->`,
	} {
		for _, closeHead := range []string{"</head>", ""} {
			t.Run(name+"/"+closeHead, func(t *testing.T) {
				doc := "<html><head>" + head + "<title>Hidden title</title>" + closeHead + "<BODY class='page'><p>hello</p></BODY></html>"
				if got := extractText(doc); got != "hello" {
					t.Fatalf("got %q, want hello", got)
				}
			})
		}
	}
}

func TestExtractTextUnclosedHeadContent(t *testing.T) {
	for _, head := range []string{
		`<script>const markup = "<body>";`,
		`<!-- <body>`,
		`<meta content='<body>`,
		`<title>hidden <body>`,
		`<meta charset="utf-8">`,
	} {
		if got := extractText("<p>before</p><head>" + head); got != "before" {
			t.Errorf("head %q: got %q, want before", head, got)
		}
	}
}

func TestExtractTextHeadWithoutCloseTag(t *testing.T) {
	// </head> is optional in HTML; the head skip must stop at <body> so the
	// page text is not swallowed along with the head.
	doc := "<html><head><title>t</title><body><p>hello</p></body></html>"
	if got := extractText(doc); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}
