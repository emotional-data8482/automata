package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadWriteRoundtrip(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	out, err := WriteFile(root).Execute(ctx, `{"path":"notes/a.txt","content":"hello sandbox"}`)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(out, "notes/a.txt") {
		t.Errorf("write result = %q, want it to mention the path", out)
	}

	got, err := ReadFile(root).Execute(ctx, `{"path":"notes/a.txt"}`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "hello sandbox" {
		t.Errorf("read = %q, want %q", got, "hello sandbox")
	}
}

func TestReadFileTruncatesLargeFile(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("b", readMaxBytes+10)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFile(root).Execute(context.Background(), `{"path":"big.txt"}`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(got, "[truncated") {
		t.Error("missing truncation marker")
	}
}

func TestSandboxRejectsEscapes(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	escapes := map[string]string{
		"dot-dot read":  `{"path":"../secret.txt"}`,
		"symlink read":  `{"path":"link"}`,
		"absolute read": fmt.Sprintf(`{"path":%q}`, secret),
	}
	for name, args := range escapes {
		if out, err := ReadFile(root).Execute(ctx, args); err == nil {
			t.Errorf("%s: escaped the sandbox, read %q", name, out)
		}
	}

	if _, err := WriteFile(root).Execute(ctx, `{"path":"../evil.txt","content":"x"}`); err == nil {
		t.Error("dot-dot write escaped the sandbox")
	}
	if _, err := os.Stat(filepath.Join(parent, "evil.txt")); err == nil {
		t.Error("dot-dot write actually created a file outside the root")
	}
}

func TestFSRequiresPath(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	if _, err := ReadFile(root).Execute(ctx, `{}`); err == nil {
		t.Error("read with no path: expected error")
	}
	if _, err := WriteFile(root).Execute(ctx, `{"content":"x"}`); err == nil {
		t.Error("write with no path: expected error")
	}
}
