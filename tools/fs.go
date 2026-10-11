package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/emotional-data8482/automata/core"
)

const readMaxBytes = 256 << 10 // 256 KiB

// FileSystem is where [ReadFileFS] and [WriteFileFS] read and write. [LocalFS]
// is the host implementation; a sandbox backend (a container, a VM, a remote
// workspace) implements it to run the same tools against its own files.
//
// Names are model-supplied paths relative to the filesystem's root. The tools
// reject empty, absolute and parent-escaping names (see [filepath.IsLocal])
// before calling an implementation, but an implementation must still keep
// every access inside its root, including through symlinks.
//
// Methods are called concurrently when a model requests several tool calls
// at once. ctx carries the run's [core.ToolOperation], so an implementation
// shared by many runs can resolve the run's own sandbox per call.
//
// An implementation may also define EffectScope() string; [WriteFileFS]
// declares it as the write tool's effect scope (see [core.ToolEffectPolicy]).
type FileSystem interface {
	// ReadFile returns at most limit bytes from the start of the named file.
	// A longer file is not an error.
	ReadFile(ctx context.Context, name string, limit int64) ([]byte, error)
	// WriteFile replaces the named file's content with data, creating parent
	// directories as needed. An error that wraps [ErrNotWritten] means the
	// file was not changed; any other error leaves the write's outcome
	// unknown.
	WriteFile(ctx context.Context, name string, data []byte) error
}

// ErrNotWritten marks a [FileSystem.WriteFile] failure that happened before
// the file was changed, such as an invalid path, a parent that is a file, or
// an unavailable sandbox. [WriteFileFS] reports it as
// [core.EffectNotApplied] so the run can recover; without it a failed write
// is [core.EffectUnknown].
var ErrNotWritten = errors.New("file not written")

// LocalFS returns a [FileSystem] rooted at the host directory root. Path
// traversal and symlinks that escape the root are rejected by [os.Root], not
// by string cleaning. Its EffectScope is "filesystem:" plus the absolute root.
func LocalFS(root string) FileSystem { return localFS{root: root} }

type localFS struct{ root string }

func (l localFS) EffectScope() string {
	absoluteRoot, _ := filepath.Abs(l.root)
	return "filesystem:" + absoluteRoot
}

func (l localFS) ReadFile(_ context.Context, name string, limit int64) ([]byte, error) {
	r, err := os.OpenRoot(l.root)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

func (l localFS) WriteFile(_ context.Context, name string, data []byte) error {
	r, err := os.OpenRoot(l.root)
	if err != nil {
		return notWritten(err)
	}
	defer r.Close()

	// Failures before the file is opened change no file content: a model typo
	// (a parent that is a file, an escaping symlink) is not applied.
	if dir := filepath.Dir(name); dir != "." {
		if err := r.MkdirAll(dir, 0o755); err != nil {
			return notWritten(err)
		}
	}
	f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return notWritten(err)
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func notWritten(err error) error { return fmt.Errorf("%w: %w", ErrNotWritten, err) }

type readFileParams struct {
	Path string `json:"path" desc:"file path, relative to the sandbox root"`
}

type writeFileParams struct {
	Path    string `json:"path" desc:"file path, relative to the sandbox root"`
	Content string `json:"content" desc:"the full file content to write"`
}

// ReadFile returns a "read_file" tool that reads text files strictly inside
// the host directory root. It is [ReadFileFS] over [LocalFS].
func ReadFile(root string) core.Tool { return ReadFileFS(LocalFS(root)) }

// ReadFileFS returns a "read_file" tool that reads text files from fsys.
// Files are capped at 256 KiB with a truncation marker.
func ReadFileFS(fsys FileSystem) core.Tool {
	return core.WithToolEffectPolicy(domainTool("read_file",
		"Read a text file from the sandboxed working directory and return its content.",
		func(ctx context.Context, p readFileParams) (string, error) {
			if p.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if !filepath.IsLocal(p.Path) {
				return "", fmt.Errorf("path %q must be relative and stay inside the working directory", p.Path)
			}
			return readText(ctx, fsys, p.Path)
		}), core.ToolEffectPolicy{Kind: core.ToolEffectReadOnly})
}

// readText reads name from fsys, truncating after readMaxBytes with a marker.
func readText(ctx context.Context, fsys FileSystem, name string) (string, error) {
	data, err := fsys.ReadFile(ctx, name, readMaxBytes+1)
	if err != nil {
		return "", err
	}
	if len(data) > readMaxBytes {
		return string(data[:readMaxBytes]) + "\n\n[truncated: file exceeds 256KB]", nil
	}
	return string(data), nil
}

// WriteFile returns a "write_file" tool that writes files strictly inside the
// host directory root, creating parent directories as needed. It is
// [WriteFileFS] over [LocalFS].
func WriteFile(root string) core.Tool { return WriteFileFS(LocalFS(root)) }

// WriteFileFS returns a "write_file" tool that writes files to fsys. Each
// model call is a new write, including restoring older content. Durable
// admission and committed tool outcomes prevent replay; no store-wide
// semantic guard is used. A successful write reports [core.EffectApplied]
// with a content-digest receipt.
func WriteFileFS(fsys FileSystem) core.Tool {
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
			if err := fsys.WriteFile(ctx, p.Path, []byte(p.Content)); err != nil {
				if errors.Is(err, ErrNotWritten) {
					return effectError(err, core.EffectNotApplied), nil
				}
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
	policy := core.ToolEffectPolicy{Kind: core.ToolEffectMutating}
	if scoped, ok := fsys.(interface{ EffectScope() string }); ok {
		policy.Scope = scoped.EffectScope()
	}
	return core.WithToolEffectPolicy(tool, policy)
}
