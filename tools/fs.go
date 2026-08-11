package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/emotional-data8482/automata/core"
)

const readMaxBytes = 256 << 10 // 256 KiB

type readFileParams struct {
	Path string `json:"path" desc:"file path, relative to the sandbox root"`
}

type writeFileParams struct {
	Path    string `json:"path" desc:"file path, relative to the sandbox root"`
	Content string `json:"content" desc:"the full file content to write"`
}

// ReadFile returns a "read_file" tool that reads text files strictly inside
// root. Path traversal and symlinks that escape the root are rejected (the
// sandbox is enforced by [os.Root], not string cleaning). Files are capped at
// 256 KiB with a truncation marker.
func ReadFile(root string) core.Tool {
	return core.Func("read_file",
		"Read a text file from the sandboxed working directory and return its content.",
		func(ctx context.Context, p readFileParams) (string, error) {
			if p.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			r, err := os.OpenRoot(root)
			if err != nil {
				return "", err
			}
			defer r.Close()

			f, err := r.Open(p.Path)
			if err != nil {
				return "", err
			}
			defer f.Close()

			data, err := io.ReadAll(io.LimitReader(f, readMaxBytes+1))
			if err != nil {
				return "", err
			}
			if len(data) > readMaxBytes {
				return string(data[:readMaxBytes]) + "\n\n[truncated: file exceeds 256KB]", nil
			}
			return string(data), nil
		})
}

// WriteFile returns a "write_file" tool that writes files strictly inside
// root, creating parent directories as needed. Path traversal and symlinks
// that escape the root are rejected (enforced by [os.Root]).
func WriteFile(root string) core.Tool {
	return core.Func("write_file",
		"Write a file (creating parent directories) inside the sandboxed working directory, replacing any existing content.",
		func(ctx context.Context, p writeFileParams) (string, error) {
			if p.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			r, err := os.OpenRoot(root)
			if err != nil {
				return "", err
			}
			defer r.Close()

			if dir := filepath.Dir(p.Path); dir != "." && dir != string(filepath.Separator) {
				if err := r.MkdirAll(dir, 0o755); err != nil {
					return "", err
				}
			}
			if err := r.WriteFile(p.Path, []byte(p.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(p.Content), p.Path), nil
		})
}
