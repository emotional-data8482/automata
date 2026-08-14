package tools

import (
	"context"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/emotional-data8482/automata/core"
)

const (
	fetchTimeout  = 30 * time.Second
	fetchMaxBytes = 512 << 10 // 512 KiB
)

type fetchParams struct {
	URL string `json:"url" desc:"the http(s) URL to fetch"`
}

// HTTPFetch returns an "http_fetch" tool that GETs a URL and returns its
// readable text: HTML is reduced to text (scripts, styles and tags stripped),
// text and JSON bodies are returned as-is, and other content types are
// rejected. Responses are capped at 512 KiB with a truncation marker.
//
// Security: the model chooses the URL, so this tool will happily fetch
// internal endpoints reachable from the host (SSRF). In production, gate it
// with an [core.Approver] or front it with an egress proxy.
func HTTPFetch() core.Tool {
	client := &http.Client{Timeout: fetchTimeout}
	return core.Func("http_fetch",
		"Fetch a web page over HTTP GET and return its readable text content. Works best on HTML, plain-text, and JSON URLs.",
		func(ctx context.Context, p fetchParams) (string, error) {
			u, err := url.Parse(strings.TrimSpace(p.URL))
			if err != nil {
				return "", fmt.Errorf("invalid url: %w", err)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return "", fmt.Errorf("unsupported scheme %q: only http and https are allowed", u.Scheme)
			}
			if u.Host == "" {
				return "", fmt.Errorf("invalid url %q: missing host", p.URL)
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
			if err != nil {
				return "", err
			}
			req.Header.Set("User-Agent", "automata-tools/0.1 (+https://github.com/emotional-data8482/automata)")

			resp, err := client.Do(req)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				return "", fmt.Errorf("GET %s: %s", u, resp.Status)
			}

			data, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes+1))
			if err != nil {
				return "", fmt.Errorf("read %s: %w", u, err)
			}
			truncated := len(data) > fetchMaxBytes
			if truncated {
				data = data[:fetchMaxBytes]
			}

			mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
			if mediaType == "" {
				mediaType, _, _ = mime.ParseMediaType(http.DetectContentType(data))
			}

			var text string
			switch {
			case mediaType == "text/html" || mediaType == "application/xhtml+xml":
				text = extractText(string(data))
			case strings.HasPrefix(mediaType, "text/"),
				mediaType == "application/json", strings.HasSuffix(mediaType, "+json"),
				mediaType == "application/xml", strings.HasSuffix(mediaType, "+xml"):
				text = string(data)
			default:
				return "", fmt.Errorf("unsupported content type %q for %s", mediaType, u)
			}

			if truncated {
				text += "\n\n[truncated: response exceeded 512KB]"
			}
			return text, nil
		})
}

// skipContainers are elements whose entire content is dropped during text
// extraction. None of them nest in practice, so a scan to the matching close
// tag suffices.
var skipContainers = map[string]bool{
	"script":   true,
	"style":    true,
	"noscript": true,
	"template": true,
	"head":     true,
}

// blockTags are elements that imply a line break around their content.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "ul": true, "ol": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"tr": true, "table": true, "section": true, "article": true,
	"header": true, "footer": true, "blockquote": true, "pre": true,
	"hr": true, "dt": true, "dd": true, "figcaption": true,
}

// extractText reduces an HTML document to readable text using only the
// stdlib: script/style/head/noscript/template content and comments are
// dropped, block-level elements break lines, all other tags are stripped, and
// entities are decoded. It is deliberately "readability-ish" — good enough to
// hand a model the words on the page, not a faithful DOM rendering.
func extractText(doc string) string {
	lower := strings.ToLower(doc)
	var b strings.Builder
	i := 0
	for i < len(doc) {
		if doc[i] != '<' {
			next := strings.IndexByte(doc[i:], '<')
			if next < 0 {
				b.WriteString(doc[i:])
				break
			}
			b.WriteString(doc[i : i+next])
			i += next
			continue
		}

		if strings.HasPrefix(lower[i:], "<!--") {
			end := strings.Index(lower[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}

		end := strings.IndexByte(doc[i:], '>')
		if end < 0 {
			break
		}
		name, closing := tagName(lower[i+1 : i+end])
		i += end + 1

		if !closing && skipContainers[name] {
			// Drop everything through the matching close tag; an unclosed
			// container swallows the rest of the document.
			rest := lower[i:]
			j := strings.Index(rest, "</"+name)
			if j < 0 {
				break
			}
			i += j
			if k := strings.IndexByte(lower[i:], '>'); k >= 0 {
				i += k + 1
			} else {
				i = len(doc)
			}
			continue
		}

		if blockTags[name] {
			b.WriteByte('\n')
		}
	}
	return normalizeText(html.UnescapeString(b.String()))
}

// tagName extracts the element name from raw tag content (already lowercased,
// without the angle brackets) and reports whether it is a closing tag.
func tagName(tag string) (name string, closing bool) {
	tag = strings.TrimSpace(tag)
	if strings.HasPrefix(tag, "/") {
		closing = true
		tag = strings.TrimSpace(tag[1:])
	}
	for i := 0; i < len(tag); i++ {
		switch tag[i] {
		case ' ', '\t', '\n', '\r', '/':
			return tag[:i], closing
		}
	}
	return tag, closing
}

// normalizeText collapses horizontal whitespace within lines and runs of blank
// lines, trimming the result.
func normalizeText(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := true // also suppresses leading blank lines
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, line)
		blank = false
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}
