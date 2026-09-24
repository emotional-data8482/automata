package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/emotional-data8482/automata/core"
)

// SkillFile is the file that marks a directory as an Agent Skill.
const SkillFile = "SKILL.md"

// Skill is one parsed Agent Skill (https://agentskills.io): a directory
// holding a SKILL.md whose YAML frontmatter names and describes it.
type Skill struct {
	Name          string
	Description   string
	License       string
	Compatibility string
	// AllowedTools is the frontmatter's space-delimited allowed-tools list.
	// It names tools of other harnesses and is exposed, not enforced;
	// use core.ToolPolicy and approvals to bound what an agent may run.
	AllowedTools []string
	// Dir is the skill's absolute directory. Files the skill references are
	// read relative to it.
	Dir string
	// Body is the SKILL.md content after the frontmatter, trimmed.
	Body string
}

// Skills is an immutable set of skills loaded by [LoadSkills]. It is safe
// for concurrent use.
type Skills struct {
	list   []Skill // sorted by name
	byName map[string]int
}

// LoadSkills loads every skill found directly under the given directories:
// each subdirectory containing a SKILL.md is one skill, and other entries
// are ignored. Every directory must exist.
//
// A skill's frontmatter must carry a name (1-64 lowercase letters, digits,
// and single hyphens, matching its directory name) and a description (at
// most 1024 characters). Frontmatter is parsed as a YAML subset: top-level
// plain, quoted, and block scalars are read; nested mappings such as
// metadata are skipped. A name defined twice, including across
// directories, is an error, so no skill silently shadows another.
//
// SKILL.md bodies are read once here and frozen, so an agent built from the
// set pins the instructions a registered revision runs with. Files a skill
// references are read on demand by [Skills.Tool].
func LoadSkills(dirs ...string) (*Skills, error) {
	s := &Skills{byName: make(map[string]int)}
	for _, dir := range dirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("skills: %w", err)
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return nil, fmt.Errorf("skills: %w", err)
		}
		for _, entry := range entries {
			skillDir := filepath.Join(abs, entry.Name())
			if info, err := os.Stat(skillDir); err != nil || !info.IsDir() {
				continue // follows symlinked skill directories; skips files and dangling links
			}
			skill, ok, err := loadSkill(skillDir)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if i, dup := s.byName[skill.Name]; dup {
				return nil, fmt.Errorf("skills: %q defined in both %s and %s", skill.Name, s.list[i].Dir, skill.Dir)
			}
			s.byName[skill.Name] = len(s.list)
			s.list = append(s.list, skill)
		}
	}
	slices.SortFunc(s.list, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	for i, skill := range s.list {
		s.byName[skill.Name] = i
	}
	return s, nil
}

// loadSkill parses dir/SKILL.md. ok is false when dir has no SKILL.md.
func loadSkill(dir string) (skill Skill, ok bool, err error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return Skill{}, false, fmt.Errorf("skills: %w", err)
	}
	defer r.Close()
	f, err := r.Open(SkillFile)
	if errors.Is(err, fs.ErrNotExist) {
		return Skill{}, false, nil
	}
	if err != nil {
		return Skill{}, false, fmt.Errorf("skills: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, readMaxBytes+1))
	if err != nil {
		return Skill{}, false, fmt.Errorf("skills: %w", err)
	}
	where := filepath.Join(dir, SkillFile)
	if len(data) > readMaxBytes {
		return Skill{}, false, fmt.Errorf("skills: %s exceeds %d bytes", where, readMaxBytes)
	}

	fields, body, err := parseSkillFrontmatter(string(data))
	if err != nil {
		return Skill{}, false, fmt.Errorf("skills: %s: %w", where, err)
	}
	skill = Skill{
		Name:          fields["name"],
		Description:   fields["description"],
		License:       fields["license"],
		Compatibility: fields["compatibility"],
		AllowedTools:  strings.Fields(fields["allowed-tools"]),
		Dir:           dir,
		Body:          body,
	}
	if err := validSkillName(skill.Name); err != nil {
		return Skill{}, false, fmt.Errorf("skills: %s: %w", where, err)
	}
	if base := filepath.Base(dir); skill.Name != base {
		return Skill{}, false, fmt.Errorf("skills: %s: name %q does not match directory %q", where, skill.Name, base)
	}
	if skill.Description == "" || len(skill.Description) > 1024 {
		return Skill{}, false, fmt.Errorf("skills: %s: description must be 1-1024 characters", where)
	}
	return skill, true, nil
}

func validSkillName(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("name must be 1-64 characters")
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(name)-1 && name[i-1] != '-':
		default:
			return fmt.Errorf("name %q must be lowercase letters, digits, and single inner hyphens", name)
		}
	}
	return nil
}

// List returns the skills sorted by name.
func (s *Skills) List() []Skill {
	out := make([]Skill, len(s.list))
	for i, skill := range s.list {
		skill.AllowedTools = slices.Clone(skill.AllowedTools)
		out[i] = skill
	}
	return out
}

// Lookup returns the named skill.
func (s *Skills) Lookup(name string) (Skill, bool) {
	i, ok := s.byName[name]
	if !ok {
		return Skill{}, false
	}
	skill := s.list[i]
	skill.AllowedTools = slices.Clone(skill.AllowedTools)
	return skill, true
}

// Catalog returns the skill names and descriptions as system prompt text
// that directs the model to the tool from [Skills.Tool]. An empty set
// yields "".
func (s *Skills) Catalog() string {
	if len(s.list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Skills\n\n")
	b.WriteString("Skills hold specialized instructions. When a task matches a skill's description, ")
	b.WriteString("call load_skill with its name before starting and follow the instructions it returns. ")
	b.WriteString("To read a file the skill references, call load_skill with the name and the file's path relative to the skill.\n")
	for _, skill := range s.list {
		b.WriteString("\n- " + skill.Name + ": " + strings.Join(strings.Fields(skill.Description), " "))
	}
	return b.String()
}

type loadSkillParams struct {
	Name string `json:"name" desc:"skill name from the catalog"`
	Path string `json:"path,omitempty" desc:"file path relative to the skill directory, such as references/api.md; omit to load the skill's instructions"`
}

// Tool returns a read-only "load_skill" tool. Called with a name, it returns
// the frozen SKILL.md body. Called with a name and a path, it reads that
// file strictly inside the skill's directory ([os.Root]), truncated after
// 256 KiB. Unknown skills and unreadable paths are model-visible errors.
// Scripts a skill ships are never executed; running one requires a tool
// such as [Shell].
func (s *Skills) Tool() core.Tool {
	return core.WithToolEffectPolicy(domainTool("load_skill",
		"Load a skill's instructions by name, or a file the skill references by name and relative path.",
		func(ctx context.Context, p loadSkillParams) (string, error) {
			i, ok := s.byName[p.Name]
			if !ok {
				return "", fmt.Errorf("unknown skill %q", p.Name)
			}
			skill := s.list[i]
			if p.Path == "" {
				return skill.Body, nil
			}
			return readRootFile(skill.Dir, p.Path)
		}), core.ToolEffectPolicy{Kind: core.ToolEffectReadOnly})
}

// parseSkillFrontmatter splits a SKILL.md into its top-level frontmatter
// scalars and trimmed body. It supports the YAML subset skills use: plain
// scalars (with comments and indented continuation lines), single- and
// double-quoted scalars on one line, and literal or folded block scalars.
// Indented lines under a key with no inline value (nested mappings and
// sequences) are skipped.
func parseSkillFrontmatter(content string) (map[string]string, string, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.TrimPrefix(content, "\uFEFF")
	rest, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		return nil, "", fmt.Errorf("missing frontmatter")
	}
	var header, body string
	if h, b, found := strings.Cut(rest, "\n---\n"); found {
		header, body = h, b
	} else if h, found := strings.CutSuffix(strings.TrimRight(rest, "\n"), "\n---"); found {
		header = h
	} else {
		return nil, "", fmt.Errorf("unterminated frontmatter")
	}

	fields := make(map[string]string)
	lines := strings.Split(header, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			return nil, "", fmt.Errorf("line %d: unexpected indentation", i+1)
		}
		key, value, found := strings.Cut(line, ":")
		if !found || strings.ContainsAny(key, " \t") {
			return nil, "", fmt.Errorf("line %d: expected key: value", i+1)
		}
		if _, dup := fields[key]; dup {
			return nil, "", fmt.Errorf("duplicate key %q", key)
		}
		value = strings.TrimSpace(value)

		// Indented lines that follow belong to this key.
		var nested []string
		for i+1 < len(lines) && (strings.TrimSpace(lines[i+1]) == "" || lines[i+1][0] == ' ' || lines[i+1][0] == '\t') {
			i++
			nested = append(nested, lines[i])
		}
		switch {
		case value == "":
			// A nested mapping or sequence: not a scalar field.
			continue
		case value[0] == '|' || value[0] == '>':
			if strings.Trim(value[1:], "+-") != "" {
				return nil, "", fmt.Errorf("key %q: unsupported block scalar header %q", key, value)
			}
			fields[key] = blockScalar(nested, value[0] == '>')
		case value[0] == '"':
			if !blank(nested) {
				return nil, "", fmt.Errorf("key %q: multi-line quoted scalars are unsupported", key)
			}
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return nil, "", fmt.Errorf("key %q: invalid double-quoted scalar", key)
			}
			fields[key] = unquoted
		case value[0] == '\'':
			if !blank(nested) {
				return nil, "", fmt.Errorf("key %q: multi-line quoted scalars are unsupported", key)
			}
			if len(value) < 2 || value[len(value)-1] != '\'' {
				return nil, "", fmt.Errorf("key %q: invalid single-quoted scalar", key)
			}
			fields[key] = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
		default:
			words := []string{stripComment(value)}
			for _, l := range nested {
				if l = strings.TrimSpace(l); l != "" {
					words = append(words, stripComment(l))
				}
			}
			fields[key] = strings.Join(words, " ")
		}
	}
	return fields, strings.TrimSpace(body), nil
}

// blockScalar joins an indented block, removing its common indentation.
// Literal blocks keep line breaks; folded blocks join lines with spaces and
// keep blank lines as breaks. Trailing line breaks are dropped.
func blockScalar(lines []string, folded bool) string {
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, " \t")); indent < 0 || n < indent {
			indent = n
		}
	}
	var out strings.Builder
	prevBlank := true
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			out.WriteString("\n")
			prevBlank = true
			continue
		}
		l = l[indent:]
		switch {
		case !folded && out.Len() > 0:
			out.WriteString("\n")
		case folded && !prevBlank:
			out.WriteString(" ")
		}
		out.WriteString(l)
		prevBlank = false
	}
	return strings.TrimRight(out.String(), "\n")
}

func stripComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func blank(lines []string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}
