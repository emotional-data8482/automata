package tools

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// AgentsMDFile is the instruction file name [LoadAgentsMD] collects.
const AgentsMDFile = "AGENTS.md"

// DefaultAgentsMDMaxBytes bounds the combined instructions [LoadAgentsMD]
// returns when AgentsMDOptions.MaxBytes is zero.
const DefaultAgentsMDMaxBytes = 32 << 10 // 32 KiB

// AgentsMDOptions configures [LoadAgentsMD].
type AgentsMDOptions struct {
	// Dir is the working directory, relative to the root. Instructions are
	// collected from the root down to Dir. Empty selects the root itself.
	Dir string
	// MaxBytes bounds the combined file contents. Zero selects
	// DefaultAgentsMDMaxBytes.
	MaxBytes int
}

// LoadAgentsMD reads the AGENTS.md files on the path from root down to
// opts.Dir and returns them as one instruction string, suitable for appending
// to core.AgentConfig.SystemPrompt. Files are ordered shallowest first, so
// the instructions closest to Dir come last and take precedence; each is
// headed by its root-relative path. Directories without an AGENTS.md and
// files that are empty after trimming are skipped, and no files yield "".
//
// Access is sandboxed by [os.Root]: Dir must be local to root, and a file or
// directory that escapes it is an error. Exceeding the byte limit is an error
// rather than a truncation, because dropping the deepest (most specific)
// instructions silently would be worse than failing construction.
//
// Load once when building the agent. The result is frozen into the agent's
// system prompt, so a registered revision pins the instructions it ran with;
// register a new revision to pick up edits.
func LoadAgentsMD(root string, opts AgentsMDOptions) (string, error) {
	if opts.MaxBytes < 0 {
		return "", fmt.Errorf("agents.md: negative MaxBytes")
	}
	limit := opts.MaxBytes
	if limit == 0 {
		limit = DefaultAgentsMDMaxBytes
	}
	clean := filepath.Clean(opts.Dir)
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("agents.md: dir %q is not inside the root", opts.Dir)
	}
	dir := filepath.ToSlash(clean)

	r, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()

	info, err := r.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("agents.md: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("agents.md: dir %q is not a directory", opts.Dir)
	}

	var sections []string
	total := 0
	for _, d := range agentsMDDirs(dir) {
		name := path.Join(d, AgentsMDFile)
		content, err := readAgentsMD(r, name, limit, total)
		if err != nil {
			return "", err
		}
		if content == "" {
			continue
		}
		total += len(content)
		sections = append(sections, "# Instructions from "+name+"\n\n"+content)
	}
	return strings.Join(sections, "\n\n"), nil
}

// agentsMDDirs lists dir and its ancestors up to ".", shallowest first.
func agentsMDDirs(dir string) []string {
	dirs := []string{"."}
	if dir == "." {
		return dirs
	}
	parts := strings.Split(dir, "/")
	for i := range parts {
		dirs = append(dirs, strings.Join(parts[:i+1], "/"))
	}
	return dirs
}

// readAgentsMD returns the trimmed content of name, or "" when it is absent
// or not a regular file. used bytes of the limit are already spent.
func readAgentsMD(r *os.Root, name string, limit, used int) (string, error) {
	f, err := r.Open(filepath.FromSlash(name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("agents.md: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("agents.md: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	remaining := limit - used
	data, err := io.ReadAll(io.LimitReader(f, int64(remaining)+1))
	if err != nil {
		return "", fmt.Errorf("agents.md: %w", err)
	}
	if len(data) > remaining {
		return "", fmt.Errorf("agents.md: instructions exceed %d bytes at %s", limit, name)
	}
	return strings.TrimSpace(string(data)), nil
}
