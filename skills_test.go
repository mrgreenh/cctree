package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func writeSkill(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseFrontmatterReadsPlainQuotedAndBlockValues(t *testing.T) {
	meta, body := parseFrontmatter([]byte("---\nname: \"deploy\"\ndescription: >-\n  Ship the app\n  to production.\nallowed-tools: Bash\n---\n\n# Deploy\nsteps\n"))
	if meta["name"] != "deploy" || meta["description"] != "Ship the app to production." || meta["allowed-tools"] != "Bash" {
		t.Fatalf("meta: %+v", meta)
	}
	if body != "\n# Deploy\nsteps" {
		t.Fatalf("body: %q", body)
	}
	if meta, body := parseFrontmatter([]byte("# No frontmatter\ntext")); len(meta) != 0 || body != "# No frontmatter\ntext" {
		t.Fatalf("no frontmatter: %+v %q", meta, body)
	}
}

func TestLoadSkillsFindsUserPluginAndProjectSkills(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	project := filepath.Join(home, "work", "api")
	writeSkill(t, filepath.Join(claude, "skills", "deslop"), "---\nname: deslop\ndescription: Remove slop\n---\nbody")
	writeSkill(t, filepath.Join(claude, "skills", ".trash", "old"), "---\nname: old\n---\n")
	writeSkill(t, filepath.Join(claude, "plugins", "cache", "stripe", "skills", "pay"), "---\ndescription: Take payments\n---\n")
	writeSkill(t, filepath.Join(home, "work", ".claude", "skills", "release"), "---\nname: release\n---\n")
	writeSkill(t, filepath.Join(claude, "skills", "vendor", "skills", "tool"), "---\nname: tool\n---\n")
	synced := filepath.Join(claude, "skills", "synced", "bucket")
	writeSkill(t, filepath.Join(synced, "pdf"), "---\nname: pdf\n---\n")
	writeSkill(t, filepath.Join(synced, "mine"), "---\nname: mine\n---\n")
	for path, content := range map[string]string{
		filepath.Join(claude, "skills", "vendor", "plugin.json"): "{}",
		filepath.Join(synced, "manifest.json"):                   `{"skills":[{"name":"pdf"},{"name":"mine","creatorType":"user"}]}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill(t, filepath.Join(project, "tools", "oracle", ".claude", "skills", "oracle"), "---\nname: oracle\n---\n")
	// A project whose sessions are not listed still counts, from its key.
	other := filepath.Join(home, "side")
	writeSkill(t, filepath.Join(other, ".claude", "skills", "side"), "---\nname: side\n---\n")
	if err := os.MkdirAll(filepath.Join(claude, "projects", strings.ReplaceAll(other, "/", "-")), 0o755); err != nil {
		t.Fatal(err)
	}

	skills := Store{ClaudeDir: claude}.LoadSkills([]string{project, "?"})
	got := map[string]string{}
	for _, s := range skills {
		got[s.Name] = s.Source
	}
	if got["tool"] != "plugin" || got["pdf"] != "bundled" || got["mine"] != "user" {
		t.Fatalf("sources under the user folder: %+v", got)
	}
	if len(skills) != 8 || got["deslop"] != "user" || got["pay"] != "plugin" || got["release"] != "project" || got["oracle"] != "project" || got["side"] != "project" {
		t.Fatalf("skills: %+v", got)
	}
	if skills[0].Name != "deslop" || skills[0].Description != "Remove slop" || skills[0].Folder != filepath.Join(claude, "skills") || skills[0].Body != "body" {
		t.Fatalf("first skill: %+v", skills[0])
	}
}

func TestSkillsModeSearchesEverySkillAndBrowsesByFolder(t *testing.T) {
	skills := []Skill{
		{Name: "deploy", Description: "Ship the app", Path: "/home/me/.claude/skills/deploy/SKILL.md", Folder: "/home/me/.claude/skills", Source: "user", Body: "# Deploy\nrun the script"},
		{Name: "deslop", Description: "Remove slop", Path: "/home/me/.claude/skills/deslop/SKILL.md", Folder: "/home/me/.claude/skills", Source: "user"},
		{Name: "pay", Description: "Take payments \x1b[31mnow", Path: "/home/me/.claude/plugins/stripe/skills/pay/SKILL.md", Folder: "/home/me/.claude/plugins/stripe/skills", Source: "plugin"},
	}
	m := NewModel(Store{}).InSkills()
	m.homeDir = "/home/me"
	next, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	next, _ = next.Update(skillsMsg{skills: skills, withProjects: true})
	m = next.(Model)

	view := ansi.Strip(m.render())
	for _, want := range []string{"All skills", "all skills", "deploy", "Ship the app", "plugins/stripe/skills", "Take payments [31mnow"} {
		if !contains(view, want) {
			t.Fatalf("first view lacks %q:\n%s", want, view)
		}
	}

	m = gotoRow(m, "~/.claude/plugins/stripe/skills")
	m = press(m, "/", "s", "l", "o", "p")
	if f := m.folder(); f == nil || f.Path != allSkillsPath || len(m.list()) != 1 {
		t.Fatalf("search should list matches from every folder, got %+v", m.folder())
	}
	view = ansi.Strip(m.render())
	if !contains(view, "Skills matching “slop”") || !contains(view, "user skill") {
		t.Fatalf("search view:\n%s", view)
	}

	m = press(m, "esc")
	m = gotoRow(m, "~/.claude/skills")
	if list := m.list(); len(list) != 2 || list[0].session.Title != "deploy" || list[1].session.Title != "deslop" {
		t.Fatalf("folder lists its own skills by name: %+v", list)
	}
	if m.focus != listPane {
		t.Fatal("searching should leave the list focused")
	}
	if view = ansi.Strip(m.render()); !contains(view, "run the script") || !contains(view, "e edit") {
		t.Fatalf("preview shows the skill's instructions:\n%s", view)
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd == nil {
		t.Fatal("enter on a skill should open it")
	}

	m = press(m, "S")
	if m.isSkills || m.allSkills != nil {
		t.Fatal("S should switch back to sessions")
	}
}
