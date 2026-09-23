package core_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	readmeGoBlock = regexp.MustCompile("(?s)```go\n(.*?)```")
	readmeExample = regexp.MustCompile(`^// (Example\w*) in core/example_test\.go$`)
)

// TestReadmeExamplesMatch keeps the repository README honest: every Go block
// is either an exact excerpt of a compiled, output-checked example in this
// package or labeled as a fragment.
func TestReadmeExamplesMatch(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("example_test.go")
	if err != nil {
		t.Fatal(err)
	}
	blocks := readmeGoBlock.FindAllStringSubmatch(string(readme), -1)
	if len(blocks) == 0 {
		t.Fatal("README has no Go blocks")
	}
	for _, block := range blocks {
		first, body, _ := strings.Cut(block[1], "\n")
		if first == "// Fragment" {
			continue
		}
		match := readmeExample.FindStringSubmatch(first)
		if match == nil {
			t.Errorf("README Go block must start with %q or %q, got %q", "// Fragment", "// ExampleName in core/example_test.go", first)
			continue
		}
		want, ok := exampleExcerpt(string(source), match[1])
		if !ok {
			t.Errorf("README cites %s, which has no README region in example_test.go", match[1])
			continue
		}
		if got := strings.TrimRight(body, "\n"); got != want {
			t.Errorf("README excerpt of %s drifted from example_test.go.\nREADME:\n%s\n\nexample:\n%s", match[1], got, want)
		}
	}
}

// exampleExcerpt returns the text between the "README:" and "README end"
// markers of the named example function, dedented by one tab.
func exampleExcerpt(source, name string) (string, bool) {
	start := strings.Index(source, "\nfunc "+name+"() {\n")
	if start < 0 {
		return "", false
	}
	function := source[start:]
	if end := strings.Index(function, "\n}\n"); end >= 0 {
		function = function[:end]
	}
	_, rest, ok := strings.Cut(function, "// README: ")
	if !ok {
		return "", false
	}
	_, rest, _ = strings.Cut(rest, "\n")
	region, _, ok := strings.Cut(rest, "\n\t// README end")
	if !ok {
		return "", false
	}
	lines := strings.Split(region, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, "\t")
	}
	return strings.Join(lines, "\n"), true
}
