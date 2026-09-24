package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAgentsMDTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadAgentsMDOrdersShallowestFirst(t *testing.T) {
	root := t.TempDir()
	writeAgentsMDTree(t, root, map[string]string{
		"AGENTS.md":           "root rules\n",
		"core/AGENTS.md":      "core rules",
		"core/sub/AGENTS.md":  "  \n\t", // blank: skipped
		"core/sub/deep/x.go":  "package deep",
		"other/AGENTS.md":     "not on the path",
		"core/sub/deep/.keep": "",
	})

	got, err := LoadAgentsMD(root, AgentsMDOptions{Dir: "core/sub/deep"})
	if err != nil {
		t.Fatal(err)
	}
	want := "# Instructions from AGENTS.md\n\nroot rules\n\n" +
		"# Instructions from core/AGENTS.md\n\ncore rules"
	if got != want {
		t.Errorf("LoadAgentsMD =\n%q\nwant\n%q", got, want)
	}
}

func TestLoadAgentsMDDefaultsToRoot(t *testing.T) {
	root := t.TempDir()
	writeAgentsMDTree(t, root, map[string]string{
		"AGENTS.md":      "root rules",
		"core/AGENTS.md": "core rules",
	})

	got, err := LoadAgentsMD(root, AgentsMDOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "# Instructions from AGENTS.md\n\nroot rules" {
		t.Errorf("LoadAgentsMD = %q", got)
	}
}

func TestLoadAgentsMDNoFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory named AGENTS.md is not an instruction file.
	if err := os.Mkdir(filepath.Join(root, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := LoadAgentsMD(root, AgentsMDOptions{Dir: "pkg"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("LoadAgentsMD = %q, want empty", got)
	}
}

func TestLoadAgentsMDRejectsInvalidDir(t *testing.T) {
	root := t.TempDir()
	writeAgentsMDTree(t, root, map[string]string{"file.txt": "x"})

	for _, dir := range []string{"..", "../elsewhere", "/abs", "missing", "file.txt"} {
		if _, err := LoadAgentsMD(root, AgentsMDOptions{Dir: dir}); err == nil {
			t.Errorf("Dir %q: want error", dir)
		}
	}
	if _, err := LoadAgentsMD(root, AgentsMDOptions{MaxBytes: -1}); err == nil {
		t.Error("negative MaxBytes: want error")
	}
}

func TestLoadAgentsMDRejectsEscapingSymlink(t *testing.T) {
	outside := t.TempDir()
	writeAgentsMDTree(t, outside, map[string]string{"secret.md": "outside the sandbox"})
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "AGENTS.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := LoadAgentsMD(root, AgentsMDOptions{})
	if err == nil {
		t.Fatalf("LoadAgentsMD = %q, want sandbox error", got)
	}
}

func TestLoadAgentsMDEnforcesCombinedLimit(t *testing.T) {
	root := t.TempDir()
	writeAgentsMDTree(t, root, map[string]string{
		"AGENTS.md":     strings.Repeat("a", 6),
		"pkg/AGENTS.md": strings.Repeat("b", 5),
	})

	if _, err := LoadAgentsMD(root, AgentsMDOptions{Dir: "pkg", MaxBytes: 11}); err != nil {
		t.Fatalf("at limit: %v", err)
	}
	_, err := LoadAgentsMD(root, AgentsMDOptions{Dir: "pkg", MaxBytes: 10})
	if err == nil || !strings.Contains(err.Error(), "pkg/AGENTS.md") {
		t.Fatalf("over limit: err = %v, want error naming pkg/AGENTS.md", err)
	}
}

func ExampleLoadAgentsMD() {
	root, _ := os.MkdirTemp("", "agentsmd")
	defer os.RemoveAll(root)
	os.MkdirAll(filepath.Join(root, "core"), 0o755)
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Run go test before finishing."), 0o644)
	os.WriteFile(filepath.Join(root, "core", "AGENTS.md"), []byte("Keep core dependency-free."), 0o644)

	instructions, err := LoadAgentsMD(root, AgentsMDOptions{Dir: "core"})
	if err != nil {
		panic(err)
	}
	// Append to the prompt passed as core.AgentConfig.SystemPrompt.
	fmt.Println("You are a careful coding agent.\n\n" + instructions)
	// Output:
	// You are a careful coding agent.
	//
	// # Instructions from AGENTS.md
	//
	// Run go test before finishing.
	//
	// # Instructions from core/AGENTS.md
	//
	// Keep core dependency-free.
}
