package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Skill is one SKILL.md: a user skill under ~/.claude/skills, a plugin's,
// or a project's under <project>/.claude/skills.
type Skill struct {
	Name        string
	Description string
	Path        string // the SKILL.md file
	Folder      string // the folder holding the skill's own directory
	Source      string // "user", "project", "plugin" or "bundled" (synced from Anthropic)
	Body        string // the start of the instructions, for the preview
	Updated     time.Time
}

// LoadSkills finds every skill under the Claude directory's skills and
// plugins folders, and under .claude/skills of each project folder and its
// parents. Sorted by name, then path.
func (s Store) LoadSkills(projects []string) []Skill {
	seen := map[string]bool{}
	var skills []Skill
	scan := func(root, source string) {
		walkSkills(root, 0, seen, func(path string) {
			if skill, ok := readSkill(path); ok {
				skill.Source = source
				skills = append(skills, skill)
			}
		})
	}
	userRoot := filepath.Join(s.ClaudeDir, "skills")
	scan(userRoot, "user")
	manifests := map[string]map[string]string{}
	for i := range skills {
		skills[i].Source = userSkillSource(skills[i], userRoot, manifests)
	}
	scan(filepath.Join(s.ClaudeDir, "plugins"), "plugin")
	for _, dir := range projectSkillDirs(append(s.projectFolders(), projects...), filepath.Dir(s.ClaudeDir)) {
		scan(dir, "project")
	}
	sort.SliceStable(skills, func(i, j int) bool {
		a, b := strings.ToLower(skills[i].Name), strings.ToLower(skills[j].Name)
		if a != b {
			return a < b
		}
		return skills[i].Path < skills[j].Path
	})
	return skills
}

// Private

const skillBodyBytes = 64 * 1024

// walkSkills calls found for each SKILL.md under root, following symlinked
// folders and skipping trash, VCS and dependency folders.
func walkSkills(root string, depth int, seen map[string]bool, found func(string)) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil || seen[real] || depth > 12 {
		return
	}
	seen[real] = true
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(root, name)
		if name == "SKILL.md" {
			found(path)
			continue
		}
		if name == ".trash" || name == ".git" || name == "node_modules" {
			continue
		}
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			st, err := os.Stat(path)
			isDir = err == nil && st.IsDir()
		}
		if isDir {
			walkSkills(path, depth+1, seen, found)
		}
	}
}

// IsOwn reports whether the person wrote the skill, rather than it coming
// with a plugin or from Anthropic.
func (k Skill) IsOwn() bool {
	return k.Source == "user" || k.Source == "project"
}

// userSkillSource tells apart what sits under the user's skills folder: a
// plugin installed there (it carries a plugin manifest), a skill synced from
// claude.ai that Anthropic wrote, and the person's own.
func userSkillSource(k Skill, root string, manifests map[string]map[string]string) string {
	for dir := filepath.Dir(k.Path); dir != root && len(dir) > len(root); dir = filepath.Dir(dir) {
		for _, mark := range []string{"plugin.json", ".claude-plugin"} {
			if _, err := os.Stat(filepath.Join(dir, mark)); err == nil {
				return "plugin"
			}
		}
	}
	creators, ok := manifests[k.Folder]
	if !ok {
		creators = syncedCreators(filepath.Join(k.Folder, "manifest.json"))
		manifests[k.Folder] = creators
	}
	if creators != nil && creators[filepath.Base(filepath.Dir(k.Path))] != "user" {
		return "bundled"
	}
	return "user"
}

// syncedCreators reads a synced folder's manifest: who created each skill,
// by name. Nil when the folder has no manifest.
func syncedCreators(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var manifest struct {
		Skills []struct {
			Name        string `json:"name"`
			CreatorType string `json:"creatorType"`
		} `json:"skills"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return nil
	}
	creators := map[string]string{}
	for _, k := range manifest.Skills {
		creators[k.Name] = k.CreatorType
	}
	return creators
}

// inWorktree reports whether dir is inside a linked git worktree, whose
// .git is a file: a second checkout of a repository found elsewhere.
func inWorktree(dir string) bool {
	for ; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if st, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return !st.IsDir()
		}
	}
	return false
}

// projectFolders decodes every project key under projects/ back to its
// folder, so a project counts even when none of its sessions is listed.
func (s Store) projectFolders() []string {
	entries, _ := os.ReadDir(filepath.Join(s.ClaudeDir, "projects"))
	resolver := newFolderResolver()
	var out []string
	for _, e := range entries {
		if dir := resolver.decode(e.Name()); dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

// nestedSkillDirs finds .claude/skills in the folders up to depth levels
// below dir, for a package or tool that keeps its own inside a project.
func nestedSkillDirs(dir string, depth int, found func(string)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "target" || name == "dist" || name == "build" {
			continue
		}
		sub := filepath.Join(dir, name)
		if st, err := os.Stat(filepath.Join(sub, ".claude", "skills")); err == nil && st.IsDir() {
			found(filepath.Join(sub, ".claude", "skills"))
		}
		if depth > 1 {
			nestedSkillDirs(sub, depth-1, found)
		}
	}
}

// projectSkillDirs lists the .claude/skills folders of each project folder,
// its parents and the folders just below it, stopping at stop (home, whose
// own is the user's).
func projectSkillDirs(projects []string, stop string) []string {
	checked := map[string]bool{}
	scanned := map[string]bool{}
	var dirs []string
	for _, p := range projects {
		// Home and anything above it is not a project to search through.
		if p = filepath.Clean(p); filepath.IsAbs(p) && !scanned[p] && p != stop && !strings.HasPrefix(stop, strings.TrimSuffix(p, "/")+"/") {
			scanned[p] = true
			nestedSkillDirs(p, 2, func(d string) { dirs = append(dirs, d) })
		}
		for dir := filepath.Clean(p); filepath.IsAbs(dir) && dir != stop && !checked[dir]; dir = filepath.Dir(dir) {
			checked[dir] = true
			candidate := filepath.Join(dir, ".claude", "skills")
			if st, err := os.Stat(candidate); err == nil && st.IsDir() {
				dirs = append(dirs, candidate)
			}
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	sort.Strings(dirs)
	return slices.Compact(dirs)
}

func readSkill(path string) (Skill, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Skill{}, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return Skill{}, false
	}
	data, err := io.ReadAll(io.LimitReader(f, skillBodyBytes))
	if err != nil {
		return Skill{}, false
	}
	dir := filepath.Dir(path)
	meta, body := parseFrontmatter(data)
	return Skill{
		Name:        firstOf(oneLine(meta["name"]), filepath.Base(dir)),
		Description: oneLine(meta["description"]),
		Path:        path,
		Folder:      filepath.Dir(dir),
		Body:        strings.TrimSpace(body),
		Updated:     st.ModTime(),
	}, true
}

// parseFrontmatter reads the top-level keys of a leading "---" block (plain,
// quoted, and folded or literal block values) and returns the text after it.
func parseFrontmatter(data []byte) (map[string]string, string) {
	meta := map[string]string{}
	text := strings.TrimPrefix(string(data), "\ufeff")
	if !strings.HasPrefix(text, "---") {
		return meta, text
	}
	scanner := bufio.NewScanner(bytes.NewReader([]byte(text)))
	scanner.Buffer(make([]byte, 0, 64*1024), len(text)+1)
	scanner.Scan() // the opening ---
	var key string
	var body []string
	isClosed := false
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if isClosed {
			body = append(body, line)
			continue
		}
		if strings.TrimSpace(line) == "---" {
			isClosed = true
			continue
		}
		if line != "" && (line[0] == ' ' || line[0] == '\t') {
			if key != "" {
				meta[key] = strings.TrimSpace(meta[key] + " " + strings.TrimSpace(line))
			}
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		key = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, ">") || strings.HasPrefix(v, "|") {
			v = "" // block value: the indented lines that follow
		}
		meta[key] = unquote(v)
	}
	if !isClosed {
		return map[string]string{}, text
	}
	return meta, strings.Join(body, "\n")
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		inner := v[1 : len(v)-1]
		if v[0] == '"' {
			return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(inner)
		}
		return strings.ReplaceAll(inner, "''", "'")
	}
	return v
}
