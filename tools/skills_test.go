package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/emotional-data8482/automata/core"
)

func writeSkill(t *testing.T, dir, name, frontmatter, body string) string {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	writeAgentsMDTree(t, skillDir, map[string]string{SkillFile: "---\n" + frontmatter + "\n---\n" + body})
	return skillDir
}

func TestLoadSkillsCatalogAndTool(t *testing.T) {
	dir := t.TempDir()
	pdf := writeSkill(t, dir, "pdf-forms", "name: pdf-forms\ndescription: Fill PDF\n  forms.\nallowed-tools: Read Bash(python:*)", "\n# PDF\n\nSee references/fields.md.\n")
	writeAgentsMDTree(t, pdf, map[string]string{"references/fields.md": "field list"})
	writeSkill(t, dir, "alpha", "name: alpha\ndescription: First skill.", "Alpha body.")
	writeAgentsMDTree(t, dir, map[string]string{"notes.txt": "not a skill", "empty/README.md": "no SKILL.md"})

	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	list := skills.List()
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "pdf-forms" {
		t.Fatalf("List = %+v", list)
	}
	pdfSkill := list[1]
	if pdfSkill.Description != "Fill PDF forms." || pdfSkill.Body != "# PDF\n\nSee references/fields.md." || pdfSkill.Dir != pdf {
		t.Errorf("pdf skill = %+v", pdfSkill)
	}
	if got := strings.Join(pdfSkill.AllowedTools, ","); got != "Read,Bash(python:*)" {
		t.Errorf("AllowedTools = %q", got)
	}
	list[1].AllowedTools[0] = "mutated"
	if s, _ := skills.Lookup("pdf-forms"); s.AllowedTools[0] != "Read" {
		t.Error("List exposed internal state")
	}

	catalog := skills.Catalog()
	if !strings.Contains(catalog, "load_skill") || !strings.HasSuffix(catalog, "\n- alpha: First skill.\n- pdf-forms: Fill PDF forms.") {
		t.Errorf("Catalog =\n%s", catalog)
	}

	tool := skills.Tool()
	if tool.Definition().Name != "load_skill" {
		t.Errorf("tool name = %q", tool.Definition().Name)
	}
	ctx := context.Background()
	for _, tc := range []struct{ args, want string }{
		{`{"name":"pdf-forms"}`, "# PDF\n\nSee references/fields.md."},
		{`{"name":"pdf-forms","path":"references/fields.md"}`, "field list"},
	} {
		got, err := executeDomain(t, tool, ctx, tc.args)
		if err != nil || got != tc.want {
			t.Errorf("load_skill %s = %q, %v; want %q", tc.args, got, err, tc.want)
		}
	}
	for _, args := range []string{
		`{"name":"missing"}`,
		`{"name":"pdf-forms","path":"../alpha/SKILL.md"}`,
		`{"name":"pdf-forms","path":"references/none.md"}`,
	} {
		if _, err := executeDomain(t, tool, ctx, args); err == nil {
			t.Errorf("load_skill %s: want model-visible error", args)
		}
	}
}

func TestLoadSkillsBodyIsFrozen(t *testing.T) {
	dir := t.TempDir()
	skillDir := writeSkill(t, dir, "frozen", "name: frozen\ndescription: d", "original")
	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, SkillFile), []byte("---\nname: frozen\ndescription: d\n---\nedited"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := executeDomain(t, skills.Tool(), context.Background(), `{"name":"frozen"}`)
	if err != nil || got != "original" {
		t.Errorf("load_skill = %q, %v; want the body read at load", got, err)
	}
}

func TestLoadSkillsToolRejectsEscapingSymlink(t *testing.T) {
	outside := t.TempDir()
	writeAgentsMDTree(t, outside, map[string]string{"secret.md": "secret"})
	dir := t.TempDir()
	skillDir := writeSkill(t, dir, "linky", "name: linky\ndescription: d", "")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(skillDir, "ref.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := executeDomain(t, skills.Tool(), context.Background(), `{"name":"linky","path":"ref.md"}`); err == nil {
		t.Fatalf("load_skill = %q, want sandbox error", got)
	}
}

func TestLoadSkillsFollowsSymlinkedSkillDir(t *testing.T) {
	store := t.TempDir()
	target := writeSkill(t, store, "shared", "name: shared\ndescription: d", "body")
	dir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "shared")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := skills.Lookup("shared"); !ok {
		t.Error("symlinked skill directory not loaded")
	}
}

func TestLoadSkillsRejectsInvalidSkills(t *testing.T) {
	for _, tc := range []struct {
		name, dir, content string
	}{
		{"no frontmatter", "a", "# just markdown"},
		{"unterminated", "a", "---\nname: a\ndescription: d\n"},
		{"missing name", "a", "---\ndescription: d\n---\n"},
		{"missing description", "a", "---\nname: a\n---\n"},
		{"dir mismatch", "a", "---\nname: b\ndescription: d\n---\n"},
		{"uppercase", "Bad", "---\nname: Bad\ndescription: d\n---\n"},
		{"double hyphen", "a--b", "---\nname: a--b\ndescription: d\n---\n"},
		{"leading hyphen", "-a", "---\nname: -a\ndescription: d\n---\n"},
		{"long description", "a", "---\nname: a\ndescription: " + strings.Repeat("x", 1025) + "\n---\n"},
		{"duplicate key", "a", "---\nname: a\nname: a\ndescription: d\n---\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAgentsMDTree(t, dir, map[string]string{tc.dir + "/" + SkillFile: tc.content})
			if _, err := LoadSkills(dir); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestLoadSkillsRejectsDuplicateNamesAndMissingDirs(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeSkill(t, a, "same", "name: same\ndescription: d", "")
	writeSkill(t, b, "same", "name: same\ndescription: d", "")
	if _, err := LoadSkills(a, b); err == nil || !strings.Contains(err.Error(), `"same"`) {
		t.Errorf("duplicate: err = %v", err)
	}
	if _, err := LoadSkills(filepath.Join(a, "missing")); err == nil {
		t.Error("missing dir: want error")
	}
	empty, err := LoadSkills()
	if err != nil || empty.Catalog() != "" || len(empty.List()) != 0 {
		t.Errorf("no dirs = %v, %v", empty, err)
	}
}

func TestParseSkillFrontmatter(t *testing.T) {
	content := "\uFEFF---\r\n" +
		"# comment\r\n" +
		"name: demo # trailing comment\r\n" +
		"description: >-\r\n" +
		"  Folded line one\r\n" +
		"  line two.\r\n" +
		"\r\n" +
		"  New paragraph.\r\n" +
		"license: 'it''s MIT'\r\n" +
		"compatibility: \"Go 1.26+ \\\"stdlib\\\"\"\r\n" +
		"metadata:\r\n" +
		"  package: example\r\n" +
		"  nested: true\r\n" +
		"notes: |\r\n" +
		"  keep\r\n" +
		"    indent\r\n" +
		"---"
	fields, body, err := parseSkillFrontmatter(content)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"name":          "demo",
		"description":   "Folded line one line two.\nNew paragraph.",
		"license":       "it's MIT",
		"compatibility": `Go 1.26+ "stdlib"`,
		"notes":         "keep\n  indent",
	}
	if len(fields) != len(want) {
		t.Errorf("fields = %q", fields)
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("%s = %q, want %q", k, fields[k], v)
		}
	}
	if body != "" {
		t.Errorf("body = %q", body)
	}

	for _, bad := range []string{
		"---\n  indented: x\n---\n",
		"---\nno colon\n---\n",
		"---\nname: \"open\n  quote\"\n---\n",
		"---\nname: 'unterminated\n---\n",
		"---\nname: |2\n  x\n---\n",
	} {
		if _, _, err := parseSkillFrontmatter(bad); err == nil {
			t.Errorf("parse %q: want error", bad)
		}
	}
}

func TestLoadSkillsRepositorySkills(t *testing.T) {
	var dirs []string
	for _, dir := range []string{"../.agents/skills", "../.codex/skills"} {
		if _, err := os.Stat(dir); err == nil {
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		t.Skip("repository skills unavailable")
	}
	skills, err := LoadSkills(dirs...)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := skills.Lookup("automata-go")
	if !ok || s.License != "Apache-2.0" || !strings.HasPrefix(s.Compatibility, "Automata v0.5.0+") || !strings.HasPrefix(s.Body, "# ") {
		t.Errorf("automata-go = %+v, %v", s, ok)
	}
}

func TestSkillsToolConcurrentUse(t *testing.T) {
	dir := t.TempDir()
	skillDir := writeSkill(t, dir, "conc", "name: conc\ndescription: d", "body")
	writeAgentsMDTree(t, skillDir, map[string]string{"ref.md": "ref"})
	skills, err := LoadSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	tool := skills.Tool()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			args := `{"name":"conc"}`
			if i%2 == 1 {
				args = `{"name":"conc","path":"ref.md"}`
			}
			if _, err := executeDomain(t, tool, context.Background(), args); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func ExampleLoadSkills() {
	dir, _ := os.MkdirTemp("", "skills")
	defer os.RemoveAll(dir)
	os.MkdirAll(filepath.Join(dir, "release-notes"), 0o755)
	os.WriteFile(filepath.Join(dir, "release-notes", "SKILL.md"), []byte(`---
name: release-notes
description: Draft release notes from merged changes.
---
# Release notes

Group changes by module.
`), 0o644)

	skills, err := LoadSkills(dir)
	if err != nil {
		panic(err)
	}
	// Append the catalog to core.AgentConfig.SystemPrompt and register the tool.
	config := core.AgentConfig{
		SystemPrompt: "You are a release assistant.\n\n" + skills.Catalog(),
		Tools:        []core.Tool{skills.Tool()},
	}
	fmt.Println(config.SystemPrompt)
	// Output:
	// You are a release assistant.
	//
	// # Skills
	//
	// Skills hold specialized instructions. When a task matches a skill's description, call load_skill with its name before starting and follow the instructions it returns. To read a file the skill references, call load_skill with the name and the file's path relative to the skill.
	//
	// - release-notes: Draft release notes from merged changes.
}
