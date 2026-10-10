package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	return core.WithToolEffectPolicy(domainTool("read_file",
		"Read a text file from the sandboxed working directory and return its content.",
		func(ctx context.Context, p readFileParams) (string, error) {
			if p.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			return readRootFile(root, p.Path)
		}), core.ToolEffectPolicy{Kind: core.ToolEffectReadOnly})
}

// readRootFile reads name strictly inside root, truncating after
// readMaxBytes with a marker.
func readRootFile(root, name string) (string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()

	f, err := r.Open(name)
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
}

// WriteFile returns a "write_file" tool that writes files strictly inside
// root, creating parent directories as needed. Path traversal and symlinks
// that escape the root are rejected (enforced by [os.Root]). Each model call
// is a new write, including restoring older content. Durable admission and
// committed tool outcomes prevent replay; no store-wide semantic guard is used.
func WriteFile(root string) core.Tool {
	tool := core.FuncResult("write_file",
		"Write a file (creating parent directories) inside the sandboxed working directory, replacing any existing content.",
		func(ctx context.Context, p writeFileParams) (core.ToolResult, error) {
			effectError := func(err error, status core.EffectStatus) core.ToolResult {
				result := core.ErrorResult(err.Error())
				result.Effect = core.EffectReport{Status: status}
				return result
			}
			if err := ctx.Err(); err != nil {
				return core.ToolResult{}, err
			}
			if p.Path == "" {
				return effectError(fmt.Errorf("path is required"), core.EffectNotApplied), nil
			}
			if !filepath.IsLocal(p.Path) {
				return effectError(fmt.Errorf("path %q must be relative and stay inside the working directory", p.Path), core.EffectNotApplied), nil
			}
			r, err := os.OpenRoot(root)
			if err != nil {
				return effectError(err, core.EffectNotApplied), nil
			}
			defer r.Close()

			// Failures before the file is opened change no file content: a
			// model typo (a parent that is a file, an escaping symlink) is
			// reported as not applied so the run can recover.
			if dir := filepath.Dir(p.Path); dir != "." {
				if err := r.MkdirAll(dir, 0o755); err != nil {
					return effectError(err, core.EffectNotApplied), nil
				}
			}
			f, err := r.OpenFile(p.Path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return effectError(err, core.EffectNotApplied), nil
			}
			_, err = f.Write([]byte(p.Content))
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				if ctx.Err() != nil {
					return core.ToolResult{Effect: core.EffectReport{Status: core.EffectUnknown}}, ctx.Err()
				}
				return effectError(err, core.EffectUnknown), nil
			}
			digest := sha256.Sum256([]byte(p.Content))
			result := core.TextResult(fmt.Sprintf("wrote %d bytes to %s", len(p.Content), p.Path))
			result.Effect = core.EffectReport{Status: core.EffectApplied, Receipt: hex.EncodeToString(digest[:])}
			return result, nil
		})
	absoluteRoot, _ := filepath.Abs(root)
	return core.WithToolEffectPolicy(tool, core.ToolEffectPolicy{
		Kind:  core.ToolEffectMutating,
		Scope: "filesystem:" + absoluteRoot,
	})
}
