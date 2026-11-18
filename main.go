package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	tea "charm.land/bubbletea/v2"
)

var version = "dev"

const usage = `cctree: browse every Claude Code session, running and past, as a folder tree.
Run it with no arguments; keys are listed at the bottom of the screen.

  cctree skills          browse your skills instead (S switches between the two)
  cctree --print         print the folder tree of sessions as text and exit
  cctree skills --print  print the folder tree of skills as text and exit`

func main() {
	isSkills, isPrint := false, false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-v", "--version", "version":
			fmt.Println("cctree", version)
			return
		case "-h", "--help", "help":
			fmt.Println(usage)
			return
		case "skills", "--skills":
			isSkills = true
		case "-p", "--print":
			isPrint = true
		default:
			fmt.Fprintf(os.Stderr, "cctree: unknown argument %q\n%s\n", arg, usage)
			os.Exit(2)
		}
	}
	store := DefaultStore()
	if isPrint {
		if err := PrintTree(os.Stdout, store, isSkills); err != nil {
			fmt.Fprintln(os.Stderr, "cctree:", err)
			os.Exit(1)
		}
		return
	}
	model := NewModel(store)
	if isSkills {
		model = model.InSkills()
	}
	final, err := tea.NewProgram(model).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cctree:", err)
		os.Exit(1)
	}
	if m, ok := final.(Model); ok && m.Resume != nil {
		resume(*m.Resume)
	}
}

// resume replaces cctree with `claude --resume <id>` in the session's folder.
func resume(s Session) {
	if err := os.Chdir(s.Cwd); err != nil {
		fmt.Fprintf(os.Stderr, "cctree: %s is gone; resuming from %s\n", s.Cwd, mustGetwd())
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cctree: claude not found on PATH")
		os.Exit(1)
	}
	err = syscall.Exec(claude, []string{"claude", "--resume", s.ID}, os.Environ())
	fmt.Fprintln(os.Stderr, "cctree:", err)
	os.Exit(1)
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}
