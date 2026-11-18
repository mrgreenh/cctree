package main

import (
	"fmt"
	"io"
	"strings"
)

// PrintTree writes the folder tree as plain text: each folder, the sessions
// (or skills) in it, then its subfolders, indented by depth. Skills are
// limited to the person's own, leaving out second checkouts of a project.
func PrintTree(w io.Writer, store Store, isSkills bool) error {
	sessions, err := store.Load()
	if err != nil {
		return err
	}
	var root *Folder
	if isSkills {
		var own []Skill
		for _, k := range store.LoadSkills(cwds(sessions)) {
			if k.IsOwn() && !(k.Source == "project" && inWorktree(k.Folder)) {
				own = append(own, k)
			}
		}
		root = BuildTree(skillRows(own), home())
		sortByTitle(root)
	} else {
		var shown []Session
		for _, s := range sessions {
			if !s.Scripted || s.Live {
				shown = append(shown, s)
			}
		}
		root = BuildTree(shown, home())
	}
	writeTree(w, root, 0)
	return nil
}

// Private

func writeTree(w io.Writer, f *Folder, depth int) {
	indent := strings.Repeat("  ", depth)
	for _, s := range f.Sessions {
		line := indent + "- " + clean(s.Title)
		if s.Live {
			line += " (running)"
		}
		fmt.Fprintln(w, line)
	}
	for _, c := range f.Folders {
		fmt.Fprintln(w, indent+c.Name+"/")
		writeTree(w, c, depth+1)
	}
}

// skillRows wraps each skill in a Session, so the tree treats both alike.
func skillRows(skills []Skill) []Session {
	rows := make([]Session, len(skills))
	for i := range skills {
		sk := &skills[i]
		rows[i] = Session{ID: sk.Path, Cwd: sk.Folder, Title: clean(sk.Name), Prompt: clean(sk.Description), Updated: sk.Updated, Skill: sk}
	}
	return rows
}

// cwds lists the distinct folders the sessions ran in.
func cwds(sessions []Session) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sessions {
		if !seen[s.Cwd] {
			seen[s.Cwd] = true
			out = append(out, s.Cwd)
		}
	}
	return out
}
