package main

import (
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const runningPath = "\x00running"

// allSkillsPath is the skills browser's first tree row: every skill, flat.
const allSkillsPath = "\x00skills"

type pane int

const (
	treePane pane = iota
	listPane
)

// item is one row of the right pane: a subfolder, a session, or one of a
// session's subagents (session set too, so actions reach its parent).
type item struct {
	folder  *Folder
	session *Session
	agent   *Agent
	header  string // a divider naming the folder of the sessions below it
	count   int
}

type treeRow struct {
	folder *Folder
	depth  int
}

type Model struct {
	store   Store
	homeDir string
	now     func() time.Time

	sessions    []Session
	root        *Folder
	running     *Folder
	rows        []treeRow
	expanded    map[string]bool
	shownAgents map[string]bool

	focus      pane
	treeCursor int
	listCursor int

	filter      string
	isFiltering bool
	isLiveOnly  bool
	isScripted  bool // showing headless claude -p / SDK runs
	isLoading   bool

	// Skills mode browses skills instead of sessions. Each skill rides in a
	// Session (Skill set) so the tree, list and filter treat both alike.
	isSkills         bool
	isSkillsLoading  bool
	hasProjectSkills bool // skills were scanned after the sessions were known
	skills           []Session
	allSkills        *Folder
	skillNameWidth   int

	width, height int
	ticks         int
	message       string

	// Resume is set when the person picked a past session: main execs it
	// once the TUI has given the terminal back.
	Resume *Session
}

type loadedMsg struct {
	sessions []Session
	err      error
}

type skillsMsg struct {
	skills       []Skill
	withProjects bool
}

// shownMsg arrives when the pager or editor opened on a skill exits.
type shownMsg struct{ err error }

type liveMsg map[string]LiveSession

type tickMsg struct{}

func NewModel(store Store) Model {
	return Model{store: store, homeDir: home(), now: time.Now, expanded: map[string]bool{"~": true, runningPath: true}, shownAgents: map[string]bool{}, isLoading: true}
}

// InSkills starts the model in the skills browser.
func (m Model) InSkills() Model {
	m.isSkills, m.isSkillsLoading = true, true
	return m
}

func (m Model) Init() tea.Cmd {
	if m.isSkills {
		return tea.Batch(m.load(), m.loadSkills(), tick())
	}
	return tea.Batch(m.load(), tick())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case loadedMsg:
		m.isLoading = false
		if msg.err != nil {
			m.message = "scan failed: " + msg.err.Error()
			return m, nil
		}
		m.sessions = msg.sessions
		m.rebuild()
		if m.isSkills && !m.hasProjectSkills {
			return m, m.loadSkills() // project folders are known now
		}
	case skillsMsg:
		m.isSkillsLoading, m.hasProjectSkills = false, msg.withProjects
		m.skills = make([]Session, len(msg.skills))
		for i := range msg.skills {
			sk := &msg.skills[i]
			m.skills[i] = Session{ID: sk.Path, Cwd: sk.Folder, Title: clean(sk.Name), Prompt: clean(sk.Description), Updated: sk.Updated, Skill: sk}
		}
		isFirst := m.allSkills == nil
		if m.isSkills {
			m.rebuild()
			if isFirst { // open the top folders, so the tree shows where skills live
				for _, f := range m.root.Folders {
					m.expanded[f.Path] = true
				}
				m.flatten()
			}
		}
	case shownMsg:
		if msg.err != nil {
			m.message = "could not open: " + msg.err.Error()
		}
	case liveMsg:
		m.applyLive(msg)
	case tickMsg:
		store := m.store
		m.ticks++
		if m.ticks%5 == 0 && !m.isLoading {
			return m, tea.Batch(tick(), m.load()) // pick up new replies and sessions
		}
		return m, tea.Batch(tick(), func() tea.Msg { return liveMsg(store.Live()) })
	case tea.KeyPressMsg:
		return m.key(msg)
	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelUp {
			m.move(-3)
		} else if msg.Button == tea.MouseWheelDown {
			m.move(3)
		}
	}
	return m, nil
}

func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "cctree"
	return v
}

// Private

func (m Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if m.isFiltering {
		switch k {
		case "esc":
			m.isFiltering, m.filter = false, ""
			m.rebuild()
		case "enter", "down", "tab":
			m.isFiltering = false
		case "backspace":
			if r := []rune(m.filter); len(r) > 0 {
				m.filter = string(r[:len(r)-1])
				m.rebuild()
			}
		case "ctrl+c":
			return m, tea.Quit
		default:
			if msg.Text != "" {
				m.filter += msg.Text
				m.rebuild()
			}
		}
		return m, nil
	}

	switch k {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "/":
		m.isFiltering = true
		if m.isSkills { // search every skill, wherever the tree was
			m.treeCursor, m.listCursor, m.focus = 0, 0, listPane
		}
	case "S", "shift+s":
		m.isSkills = !m.isSkills
		m.filter, m.message, m.focus, m.treeCursor, m.listCursor = "", "", treePane, 0, 0
		m.rebuild()
		m.treeCursor = 0
		if m.isSkills && m.skills == nil {
			m.isSkillsLoading = true
			return m, m.loadSkills()
		}
	case "a":
		if !m.isSkills {
			m.isLiveOnly = !m.isLiveOnly
			m.rebuild()
		}
	case "s":
		if !m.isSkills {
			m.isScripted = !m.isScripted
			m.rebuild()
		}
	case "e":
		if s, ok := m.session(); ok && s.Skill != nil && m.focus == listPane {
			return m, showFile(s.Skill.Path, editor()...)
		}
	case "r":
		m.isLoading, m.message = true, ""
		if m.isSkills {
			m.isSkillsLoading = true
			return m, tea.Batch(m.load(), m.loadSkills())
		}
		return m, m.load()
	case "tab":
		m.focus = 1 - m.focus
		m.settle(1)
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup":
		m.move(-m.bodyHeight())
	case "pgdown":
		m.move(m.bodyHeight())
	case "home", "g":
		m.move(-1 << 20)
	case "end", "G":
		m.move(1 << 20)
	case "left", "h":
		if m.focus == treePane {
			m.collapse()
		} else if it, ok := m.current(); ok && it.agent != nil {
			delete(m.shownAgents, it.session.ID)
			m.selectSession(it.session.ID)
		} else if ok && it.session != nil && m.shownAgents[it.session.ID] {
			delete(m.shownAgents, it.session.ID)
		} else {
			m.focus = treePane
		}
	case "esc":
		if m.focus == listPane {
			m.focus = treePane
		} else if m.filter != "" {
			m.filter = ""
			m.rebuild()
		}
	case "right", "l", "space":
		if m.focus == treePane {
			m.expand()
		} else if it, ok := m.current(); ok && it.folder != nil {
			m.enterFolder(it.folder)
		} else if ok && it.agent == nil && it.session != nil && len(it.session.Agents) > 0 {
			m.shownAgents[it.session.ID] = !m.shownAgents[it.session.ID]
		}
	case "enter":
		if m.focus == treePane {
			if f := m.folder(); f != nil && len(f.Folders) > 0 && !m.expanded[f.Path] {
				m.expanded[f.Path] = true
				m.flatten()
			} else {
				m.focus, m.listCursor = listPane, 0
				m.settle(1)
			}
			return m, nil
		}
		if it, ok := m.current(); ok && it.folder != nil {
			m.enterFolder(it.folder)
			return m, nil
		}
		return m.open(false)
	case "o":
		if m.focus == listPane {
			return m.open(true)
		}
	}
	return m, nil
}

func (m Model) open(isNewWindow bool) (tea.Model, tea.Cmd) {
	s, ok := m.session()
	if !ok {
		return m, nil
	}
	if s.Skill != nil {
		if isNewWindow {
			return m, nil
		}
		return m, showFile(s.Skill.Path, "less", "-+F")
	}
	if s.Live {
		m.message = FocusLive(s)
		return m, nil
	}
	if isNewWindow {
		if err := OpenTerminal(s.Cwd, "claude", "--resume", s.ID); err != nil {
			m.message = err.Error()
		} else {
			m.message = "opened " + truncate(s.Title, 40) + " in a new window"
		}
		return m, nil
	}
	m.Resume = &s
	return m, tea.Quit
}

func (m *Model) move(delta int) {
	if m.focus == treePane {
		m.treeCursor = clamp(m.treeCursor+delta, 0, len(m.rows)-1)
		m.listCursor = 0
	} else {
		m.listCursor = clamp(m.listCursor+delta, 0, len(m.list())-1)
		m.settle(map[bool]int{true: 1, false: -1}[delta > 0])
	}
}

// enterFolder selects a subfolder of the current folder in the tree.
func (m *Model) enterFolder(f *Folder) {
	if cur := m.folder(); cur != nil {
		m.expanded[cur.Path] = true
	}
	m.flatten()
	for i, r := range m.rows {
		if r.folder.Path == f.Path {
			m.treeCursor, m.listCursor = i, 0
			return
		}
	}
}

func (m *Model) selectSession(id string) {
	for i, it := range m.list() {
		if it.agent == nil && it.session != nil && it.session.ID == id {
			m.listCursor = i
			return
		}
	}
}

func (m *Model) expand() {
	f := m.folder()
	if f == nil {
		return
	}
	if len(f.Folders) == 0 || m.expanded[f.Path] {
		m.focus, m.listCursor = listPane, 0
		m.settle(1)
		return
	}
	m.expanded[f.Path] = true
	m.flatten()
}

func (m *Model) collapse() {
	if m.treeCursor >= len(m.rows) {
		return
	}
	row := m.rows[m.treeCursor]
	if m.expanded[row.folder.Path] && len(row.folder.Folders) > 0 {
		delete(m.expanded, row.folder.Path)
		m.flatten()
		return
	}
	for i := m.treeCursor - 1; i >= 0; i-- {
		if m.rows[i].depth < row.depth {
			m.treeCursor = i
			return
		}
	}
}

func (m *Model) rebuild() {
	var path string
	if f := m.folder(); f != nil {
		path = f.Path
	}
	if m.isSkills {
		m.rebuildSkills(path)
		return
	}
	m.allSkills = nil
	var shown, live []Session
	for _, s := range m.sessions {
		if m.isLiveOnly && !s.Live {
			continue
		}
		if s.Scripted && !s.Live && !m.isScripted {
			continue
		}
		if m.filter != "" && !matches(s, m.filter, m.homeDir) {
			continue
		}
		shown = append(shown, s)
		if s.Live {
			live = append(live, s)
		}
	}
	m.root = BuildTree(shown, m.homeDir)
	m.running = BuildTree(live, m.homeDir)
	m.running.Path, m.running.Name = runningPath, "all"
	prefixPaths(m.running.Folders, runningPath+"/")
	m.restore(path)
}

// rebuildSkills is rebuild for the skills browser: a flat row of every
// matching skill, then the tree of the folders that hold them.
func (m *Model) rebuildSkills(path string) {
	var shown []Session
	m.skillNameWidth = 0
	for _, s := range m.skills {
		if m.filter != "" && !matches(s, m.filter, m.homeDir) {
			continue
		}
		shown = append(shown, s)
		m.skillNameWidth = max(m.skillNameWidth, lipgloss.Width(s.Title))
	}
	if words := strings.Fields(strings.ToLower(m.filter)); len(words) > 0 {
		// Skills whose name matches come before those matching elsewhere.
		sort.SliceStable(shown, func(i, j int) bool {
			return strings.Contains(strings.ToLower(shown[i].Title), words[0]) && !strings.Contains(strings.ToLower(shown[j].Title), words[0])
		})
	}
	m.root = BuildTree(shown, m.homeDir)
	sortByTitle(m.root)
	m.running = nil
	m.allSkills = &Folder{Path: allSkillsPath, Name: "all skills", Sessions: shown, Total: len(shown)}
	m.restore(path)
}

// restore flattens the tree and puts the cursor back on the folder at path.
func (m *Model) restore(path string) {
	m.flatten()
	m.treeCursor = 0
	for i, r := range m.rows {
		if r.folder.Path == path {
			m.treeCursor = i
		}
	}
	m.listCursor = clamp(m.listCursor, 0, len(m.list())-1)
	m.settle(1)
}

func (m *Model) flatten() {
	m.rows = m.rows[:0]
	if m.root == nil {
		return
	}
	isAllOpen := m.isAllOpen()
	var walk func(f *Folder, depth int, isOpen bool)
	walk = func(f *Folder, depth int, isOpen bool) {
		for _, c := range f.Folders {
			m.rows = append(m.rows, treeRow{folder: c, depth: depth})
			if isOpen || m.expanded[c.Path] {
				walk(c, depth+1, isOpen)
			}
		}
	}
	// The running section is its own tree of just the folders with live
	// sessions, always open beneath its header.
	if m.running != nil && m.running.Total > 0 {
		m.rows = append(m.rows, treeRow{folder: m.running})
		if m.expanded[runningPath] {
			walk(m.running, 1, true)
		}
	}
	if m.allSkills != nil {
		m.rows = append(m.rows, treeRow{folder: m.allSkills})
	}
	walk(m.root, 0, isAllOpen)
	m.treeCursor = clamp(m.treeCursor, 0, len(m.rows)-1)
}

// isAllOpen reports whether the tree is drawn fully expanded, as it is
// while a filter narrows it.
func (m Model) isAllOpen() bool {
	return m.filter != "" || (m.isLiveOnly && !m.isSkills)
}

func (m *Model) applyLive(live map[string]LiveSession) {
	changed := false
	for i := range m.sessions {
		s := &m.sessions[i]
		on, isLive := live[s.ID]
		if s.Live != isLive || (isLive && s.Status != on.Status) {
			s.Live, s.Status, s.PID = isLive, on.Status, on.PID
			changed = true
		}
	}
	if changed {
		m.rebuild()
	}
}

func (m Model) folder() *Folder {
	if m.treeCursor < 0 || m.treeCursor >= len(m.rows) {
		return nil
	}
	return m.rows[m.treeCursor].folder
}

// list is the selected folder's own level: its subfolders, then the
// sessions started in it, each followed by its subagents when shown.
func (m Model) list() []item {
	f := m.folder()
	if f == nil {
		return nil
	}
	var out []item
	add := func(sessions []Session) {
		for i := range sessions {
			s := &sessions[i]
			out = append(out, item{session: s})
			if m.shownAgents[s.ID] {
				for j := range s.Agents {
					out = append(out, item{session: s, agent: &s.Agents[j]})
				}
			}
		}
	}
	if !isRunningPath(f.Path) {
		for _, c := range f.Folders {
			out = append(out, item{folder: c})
		}
		add(f.Sessions)
		return out
	}
	// The running section is its tree flattened: a divider per folder,
	// then the sessions running in it.
	var walk func(*Folder)
	walk = func(n *Folder) {
		if len(n.Sessions) > 0 {
			out = append(out, item{header: strings.TrimPrefix(n.Path, runningPath+"/"), count: len(n.Sessions)})
			add(n.Sessions)
		}
		for _, c := range n.Folders {
			walk(c)
		}
	}
	walk(f)
	return out
}

// settle moves the list cursor off divider rows, in direction dir.
func (m *Model) settle(dir int) {
	list := m.list()
	for m.listCursor >= 0 && m.listCursor < len(list) && list[m.listCursor].header != "" {
		m.listCursor += dir
	}
	if m.listCursor >= len(list) || m.listCursor < 0 {
		m.listCursor = clamp(m.listCursor, 0, len(list)-1)
		if dir > 0 {
			m.settle(-1)
		} else if len(list) > 0 && list[m.listCursor].header != "" {
			m.settle(1)
		}
	}
}

func (m Model) current() (item, bool) {
	list := m.list()
	if m.listCursor < 0 || m.listCursor >= len(list) {
		return item{}, false
	}
	return list[m.listCursor], true
}

func (m Model) session() (Session, bool) {
	it, ok := m.current()
	if !ok || it.session == nil {
		return Session{}, false
	}
	return *it.session, true
}

// loadSkills scans for skills, including those of every folder a session
// ran in once the sessions are known.
func (m Model) loadSkills() tea.Cmd {
	store := m.store
	withProjects := m.sessions != nil || !m.isLoading
	seen := map[string]bool{}
	var projects []string
	for _, s := range m.sessions {
		if !seen[s.Cwd] {
			seen[s.Cwd] = true
			projects = append(projects, s.Cwd)
		}
	}
	return func() tea.Msg {
		return skillsMsg{store.LoadSkills(projects), withProjects}
	}
}

func (m Model) load() tea.Cmd {
	store := m.store
	return func() tea.Msg {
		sessions, err := store.Load()
		return loadedMsg{sessions, err}
	}
}

// Drawing

var (
	accent   = lipgloss.Color("4")
	liveCol  = lipgloss.Color("2")
	busyCol  = lipgloss.Color("3")
	skillCol = lipgloss.Color("5")
	muted    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	green    = lipgloss.NewStyle().Foreground(liveCol)
	bold     = lipgloss.NewStyle().Bold(true)
	selected = lipgloss.NewStyle().Reverse(true)
	dimSel   = lipgloss.NewStyle().Bold(true).Foreground(accent)
)

func (m Model) bodyHeight() int {
	top, _ := m.panelHeights()
	return max(3, top-2)
}

func (m Model) render() string {
	if m.width < 60 || m.height < 18 {
		return fmt.Sprintf("cctree needs at least 60×18 (now %d×%d)", m.width, m.height)
	}
	leftW := clamp(m.width/3, 30, 52)
	rightW := m.width - leftW
	topH, previewH := m.panelHeights()

	runN := m.runningRows()
	isInRunning := m.treeCursor < runN
	tree := ""
	foldersH := topH
	if runN > 0 {
		runH := min(runN+2, topH/2)
		foldersH = topH - runH
		tree = panel("Running", fmt.Sprint(m.running.Live), m.renderRows(0, runN, leftW-4, runH-2), leftW, runH, m.focus == treePane && isInRunning, liveCol) + "\n"
	}
	total := 0
	if m.root != nil {
		total = m.root.Total
	}
	tree += panel(map[bool]string{true: "Skill folders", false: "Folders"}[m.isSkills], fmt.Sprint(total), m.renderRows(runN, len(m.rows), leftW-4, foldersH-2), leftW, foldersH, m.focus == treePane && !isInRunning)
	title, count := m.listTitle()
	list := panel(title, count, m.renderList(rightW-4, topH-2), rightW, topH, m.focus == listPane)
	ptitle, pright, pbody := m.preview(m.width - 4)
	prev := panel(ptitle, pright, pbody, m.width, previewH, false)

	return strings.Join([]string{m.header(), lipgloss.JoinHorizontal(lipgloss.Top, tree, list), prev, m.footer()}, "\n")
}

// panelHeights splits the body between the browsing panels and the preview,
// which takes about two fifths.
func (m Model) panelHeights() (top, preview int) {
	body := m.height - 2
	preview = clamp(body*2/5, 9, 24)
	return body - preview, preview
}

func (m Model) header() string {
	live, scripted := 0, 0
	for _, s := range m.sessions {
		if s.Live {
			live++
		} else if s.Scripted {
			scripted++
		}
	}
	shown := len(m.sessions)
	if !m.isScripted {
		shown -= scripted
	}
	parts := []string{bold.Render("cctree"), muted.Render(fmt.Sprintf("%d sessions", shown)), lipgloss.NewStyle().Foreground(liveCol).Render(fmt.Sprintf("%d running", live))}
	if scripted > 0 {
		parts = append(parts, muted.Render(fmt.Sprintf("%d scripted %s", scripted, map[bool]string{true: "shown", false: "hidden"}[m.isScripted])))
	}
	if m.isSkills {
		parts = []string{bold.Render("cctree"), lipgloss.NewStyle().Foreground(skillCol).Render("skills"), muted.Render(fmt.Sprintf("%d skills", len(m.skills)))}
		if m.filter != "" && m.allSkills != nil {
			parts = append(parts, muted.Render(fmt.Sprintf("%d match", m.allSkills.Total)))
		}
	}
	if m.isLoading || (m.isSkills && m.isSkillsLoading) {
		parts = append(parts, muted.Render("scanning…"))
	}
	switch {
	case m.isFiltering:
		parts = append(parts, lipgloss.NewStyle().Foreground(accent).Render("/"+m.filter+"█"))
	case m.filter != "":
		parts = append(parts, lipgloss.NewStyle().Foreground(accent).Render("filter: "+m.filter))
	}
	if m.isLiveOnly && !m.isSkills {
		parts = append(parts, lipgloss.NewStyle().Foreground(liveCol).Render("running only"))
	}
	line := muted.Render("── ") + strings.Join(parts, muted.Render(" · ")) + " "
	return line + muted.Render(strings.Repeat("─", max(0, m.width-lipgloss.Width(line))))
}

// renderRows draws tree rows [from, to) into a w×h box, scrolled to keep
// the cursor in view when it is inside the range.
func (m Model) renderRows(from, to, w, h int) string {
	cursor := m.treeCursor - from
	if cursor < 0 || cursor >= to-from {
		cursor = 0
	}
	top := from + scrollTop(cursor, h, to-from)
	var lines []string
	for i := top; i < min(to, top+h); i++ {
		row := m.rows[i]
		f := row.folder
		isRunning := isRunningPath(f.Path)
		marker := "  "
		if len(f.Folders) > 0 {
			marker = "▸ "
			if m.expanded[f.Path] || m.isAllOpen() || (isRunning && f.Path != runningPath) {
				marker = "▾ "
			}
		}
		name := strings.Repeat("  ", row.depth) + marker + f.Name
		count := fmt.Sprint(f.Total)
		if isRunning {
			count = fmt.Sprint(f.Live)
		}
		name = ansi.Truncate(name, w-len(count)-1, "…")
		pad := strings.Repeat(" ", max(1, w-lipgloss.Width(name)-len(count)))
		switch {
		case i == m.treeCursor && m.focus == treePane:
			lines = append(lines, selected.Render(name+pad+count))
		case i == m.treeCursor:
			lines = append(lines, dimSel.Render(name+pad+count))
		case isRunning:
			lines = append(lines, name+pad+green.Render(count))
		default:
			lines = append(lines, name+pad+muted.Render(count))
		}
	}
	if to == from {
		lines = append(lines, muted.Render(map[bool]string{true: "scanning…", false: "no " + m.noun()}[m.isLoading || (m.isSkills && m.isSkillsLoading)]))
	}
	return fill(lines, w, h)
}

// runningRows is how many tree rows belong to the running section, which
// always comes first.
func (m Model) runningRows() int {
	n := 0
	for n < len(m.rows) && isRunningPath(m.rows[n].folder.Path) {
		n++
	}
	return n
}

func (m Model) renderList(w, h int) string {
	f := m.folder()
	list := m.list()
	var lines []string
	cursorLine := 0
	for i := range list {
		if i == m.listCursor {
			cursorLine = len(lines)
		}
		if it := list[i]; it.header != "" {
			label := " " + bold.Render(ansi.Truncate(it.header, w-12, "…")) + " "
			count := " " + fmt.Sprint(it.count)
			rule := strings.Repeat("─", max(0, w-2-lipgloss.Width(label)-lipgloss.Width(count)))
			if i > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, muted.Render("──")+label+muted.Render(rule)+green.Render(count))
			continue
		}
		if s := list[i].session; s != nil && s.Skill != nil {
			plain, styled := m.skillRow(s, w, f != nil && f.Path == allSkillsPath)
			switch {
			case i == m.listCursor && m.focus == listPane:
				lines = append(lines, selected.Render(plain))
			case i == m.listCursor:
				lines = append(lines, dimSel.Render(plain))
			default:
				lines = append(lines, styled)
			}
			continue
		}
		left, right, markCol, isDim := m.itemText(f, list[i])
		room := w - 1 - len([]rune(right))
		left = ansi.Truncate(left, room, "…")
		pad := strings.Repeat(" ", max(1, w-lipgloss.Width(left)-len([]rune(right))))
		switch {
		case i == m.listCursor && m.focus == listPane:
			lines = append(lines, selected.Render(left+pad+right))
		case i == m.listCursor:
			lines = append(lines, dimSel.Render(left+pad+right))
		default:
			mark, rest, _ := strings.Cut(left, " ")
			if strings.HasPrefix(left, " ") {
				mark, rest = "", left
			}
			style := lipgloss.NewStyle()
			if isDim {
				style = style.Foreground(lipgloss.Color("7"))
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(markCol).Render(mark)+map[bool]string{true: " ", false: ""}[mark != ""]+style.Render(rest)+pad+muted.Render(right))
		}
	}
	if len(list) == 0 && f != nil {
		lines = append(lines, muted.Render(map[bool]string{true: "no skills match", false: "no " + m.noun() + " at this level"}[f.Path == allSkillsPath]))
	}
	top := scrollTop(cursorLine, h, len(lines))
	return fill(lines[top:], w, h)
}

// skillRow lays out a skill as columns: name, description and, in the flat
// list of every skill, the folder it lives in.
func (m Model) skillRow(s *Session, w int, isAll bool) (plain, styled string) {
	nameW := clamp(m.skillNameWidth, 8, max(8, min(28, w/3)))
	name := ansi.Truncate(s.Title, nameW, "…")
	name += strings.Repeat(" ", nameW-lipgloss.Width(name))
	folder := ""
	if isAll {
		folder = "  " + tail(DisplayPath(s.Skill.Folder, m.homeDir), max(12, w/3))
	}
	room := max(0, w-2-nameW-2-lipgloss.Width(folder))
	desc := ansi.Truncate(s.Prompt, room, "…")
	desc += strings.Repeat(" ", room-lipgloss.Width(desc))
	plain = "◆ " + name + "  " + desc + folder
	styled = lipgloss.NewStyle().Foreground(skillCol).Render("◆") + " " + name + "  " +
		lipgloss.NewStyle().Foreground(lipgloss.Color("7")).Render(desc) + muted.Render(folder)
	return plain, styled
}

// skillPreview shows a skill's description and the start of its
// instructions.
func (m Model) skillPreview(s *Session, w, h int) (title, right, body string) {
	sk := s.Skill
	label := lipgloss.NewStyle().Foreground(accent).Bold(true)
	lines := []string{muted.Render(tail(DisplayPath(sk.Path, m.homeDir), w))}
	if m.message != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(accent).Render(ansi.Truncate(m.message, w, "…")))
	}
	lines = append(lines, "")
	about := wrap(firstOf(s.Prompt, "(no description)"), w-8, max(2, h/3))
	lines = append(lines, label.Render("About   ")+about[0])
	for _, l := range about[1:] {
		lines = append(lines, "        "+l)
	}
	lines = append(lines, "")
	for _, l := range strings.Split(sk.Body, "\n") {
		if len(lines) >= h {
			break
		}
		lines = append(lines, ansi.Truncate(clean(strings.ReplaceAll(l, "\t", "    ")), w, "…"))
	}
	return s.Title, muted.Render(sk.Source + " skill · changed " + agoPhrase(sk.Updated, m.now())), fill(lines, w, h)
}

// noun names what the browser is listing.
func (m Model) noun() string {
	if m.isSkills {
		return "skills"
	}
	return "sessions"
}

// itemText lays out one right-pane row: the text, the right-hand column,
// the color of its leading mark and whether it is drawn dim.
func (m Model) itemText(f *Folder, it item) (left, right string, markCol color.Color, isDim bool) {
	now := m.now()
	switch {
	case it.folder != nil:
		right = fmt.Sprint(it.folder.Total)
		if it.folder.Live > 0 {
			right = fmt.Sprintf("●%d %s", it.folder.Live, right)
		}
		return "▸ " + it.folder.Name + "/", right, accent, false
	case it.agent != nil:
		a := it.agent
		label := firstOf(a.Name, a.Type, a.ID)
		if a.Description != "" && a.Description != label {
			label += " — " + a.Description
		}
		mark, col := "↳", lipgloss.Color("8")
		right = ago(a.Updated, now)
		if it.session.Live && a.IsActive(now) {
			mark, col, right = "◉", busyCol, "working"
		}
		return strings.Repeat("  ", a.Depth+1) + mark + " " + label, right, col, true
	default:
		s := it.session
		mark, col, when := "○", lipgloss.Color("8"), ago(s.Updated, now)
		if s.Live {
			mark, col, when = "●", liveCol, s.Status
			if s.Status == "busy" {
				col = busyCol
			}
		}
		text := s.Title
		if n := agentCount(s.Agents); n > 0 {
			fold := "▸"
			if m.shownAgents[s.ID] {
				fold = "▾"
			}
			when = fmt.Sprintf("%s %d agent%s  %s", fold, n, map[bool]string{true: "", false: "s"}[n == 1], when)
		}
		return mark + " " + text, when, col, !s.Live
	}
}

// preview describes the selected row: its title, a right-hand status and
// the body lines, at most w cells wide.
func (m Model) preview(w int) (title, right, body string) {
	_, h := m.panelHeights()
	h -= 2
	label := lipgloss.NewStyle().Foreground(accent).Bold(true)
	it, ok := m.current()
	switch {
	case !ok:
		return "Preview", "", fill([]string{muted.Render(firstOf(m.message, "nothing selected"))}, w, h)
	case it.folder != nil:
		f := it.folder
		summary := fmt.Sprintf("%d sessions · %d running · last active %s", f.Total, f.Live, agoPhrase(f.Latest, m.now()))
		if m.isSkills {
			summary = fmt.Sprintf("%d skills · last changed %s", f.Total, agoPhrase(f.Latest, m.now()))
		}
		lines := []string{muted.Render(summary), ""}
		for _, sub := range f.Folders {
			lines = append(lines, fmt.Sprintf("  ▸ %-24s %s", sub.Name+"/", muted.Render(fmt.Sprintf("%d %s", sub.Total, m.noun()))))
		}
		return strings.TrimPrefix(f.Path, runningPath+"/"), "", fill(lines, w, h)
	case it.agent != nil:
		a := it.agent
		kind := firstOf(a.Type, "agent")
		if a.Workflow != "" {
			kind += " · " + a.Workflow
		}
		lines := []string{
			muted.Render(ansi.Truncate("subagent of “"+it.session.Title+"” · "+kind, w, "…")),
			"",
			label.Render("Task"),
		}
		lines = append(lines, wrap(firstOf(a.Description, "(no description)"), w, 3)...)
		lines = append(lines, "", muted.Render(ansi.Truncate(DisplayPath(a.Path, m.homeDir), w, "…")))
		return firstOf(a.Name, a.ID), agoPhrase(a.Updated, m.now()), fill(lines, w, h)
	}

	s := it.session
	if s.Skill != nil {
		return m.skillPreview(s, w, h)
	}
	right = muted.Render(agoPhrase(s.Updated, m.now()))
	if s.Live {
		right = lipgloss.NewStyle().Foreground(liveCol).Render("● " + s.Status)
	}
	meta := DisplayPath(s.Cwd, m.homeDir) + " · " + s.ID[:min(8, len(s.ID))]
	if n := agentCount(s.Agents); n > 0 {
		meta += fmt.Sprintf(" · %d agents", n)
	}
	lines := []string{muted.Render(ansi.Truncate(meta, w, "…"))}
	if m.message != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(accent).Render(ansi.Truncate(m.message, w, "…")))
	}
	lines = append(lines, "")
	room := h - len(lines)
	you := wrap(firstOf(s.LastPrompt, s.Prompt, "(no prompt yet)"), w-8, max(1, room/3))
	lines = append(lines, label.Render("You     ")+you[0])
	for _, l := range you[1:] {
		lines = append(lines, "        "+l)
	}
	if s.LastReply != "" {
		reply := wrap(s.LastReply, w-8, max(1, h-len(lines)-1))
		lines = append(lines, "", lipgloss.NewStyle().Foreground(liveCol).Bold(true).Render("Claude  ")+reply[0])
		for _, l := range reply[1:] {
			lines = append(lines, "        "+l)
		}
	}
	return s.Title, right, fill(lines, w, h)
}

// listTitle is the right panel's border title: where you are and how many.
func (m Model) listTitle() (string, string) {
	f := m.folder()
	if f == nil {
		return map[bool]string{true: "Skills", false: "Sessions"}[m.isSkills], ""
	}
	if f.Path == allSkillsPath {
		if m.filter != "" {
			return "Skills matching “" + m.filter + "”", fmt.Sprint(f.Total)
		}
		return "All skills", fmt.Sprint(f.Total)
	}
	if isRunningPath(f.Path) {
		label := "Running now"
		if f.Path != runningPath {
			label = "Running in " + strings.TrimPrefix(f.Path, runningPath+"/")
		}
		return label, fmt.Sprint(f.Live)
	}
	count := fmt.Sprintf("%d here", len(f.Sessions))
	if len(f.Folders) > 0 {
		count += fmt.Sprintf(" · %d below", f.Total-len(f.Sessions))
	}
	return f.Path, count
}

func (m Model) footer() string {
	if m.isSkills {
		keys := "↑↓ move  ←→ fold  tab switch  enter open"
		if it, ok := m.current(); m.focus == listPane && ok && it.session != nil {
			keys = "↑↓ move  ←→ fold  tab switch  enter read  e edit"
		}
		return muted.Render(ansi.Truncate(keys+"  / search all skills  r rescan  S sessions  q quit", m.width, "…"))
	}
	keys := "↑↓ move  ←→ fold  tab switch  enter "
	if it, ok := m.current(); m.focus == listPane && ok && it.folder != nil {
		keys += "open folder"
	} else if m.focus == listPane {
		if s, ok := m.session(); ok && s.Live {
			keys += "focus window"
		} else {
			keys += "resume here  o new window"
		}
	} else {
		keys += "open"
	}
	keys += "  / filter  a running only  s scripted  S skills  r rescan  q quit"
	return muted.Render(ansi.Truncate(keys, m.width, "…"))
}

// Helpers

// panel draws body inside a rounded border with title set into the top
// edge and right at its end; focus colours the border.
func panel(title, right, body string, w, h int, isFocused bool, tone ...color.Color) string {
	border, titleStyle := muted, bold
	switch {
	case len(tone) > 0 && isFocused:
		border, titleStyle = lipgloss.NewStyle().Foreground(tone[0]).Bold(true), bold.Foreground(tone[0])
	case len(tone) > 0:
		border, titleStyle = lipgloss.NewStyle().Foreground(tone[0]), bold.Foreground(tone[0])
	case isFocused:
		border, titleStyle = lipgloss.NewStyle().Foreground(accent), bold.Foreground(accent)
	}
	inner := w - 2
	t := ""
	if title != "" {
		t = " " + titleStyle.Render(ansi.Truncate(title, max(1, inner-lipgloss.Width(right)-8), "…")) + " "
	}
	r := ""
	if right != "" {
		r = " " + right + " "
	}
	gap := max(0, inner-1-lipgloss.Width(t)-lipgloss.Width(r)-1)
	top := border.Render("╭─") + t + border.Render(strings.Repeat("─", gap)) + map[bool]lipgloss.Style{true: titleStyle.UnsetBold(), false: muted}[len(tone) > 0].Render(r) + border.Render("─╮")
	lines := []string{top}
	for _, l := range strings.Split(fill(strings.Split(body, "\n"), inner-2, h-2), "\n") {
		lines = append(lines, border.Render("│")+" "+l+" "+border.Render("│"))
	}
	lines = append(lines, border.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(lines, "\n")
}

// wrap word-wraps text to width, keeping at most n lines (the last one
// ending in … when cut). It always returns at least one line.
func wrap(text string, width, n int) []string {
	var out []string
	for _, para := range strings.Split(strings.TrimSpace(text), "\n") {
		para = strings.Join(strings.Fields(para), " ")
		if para == "" {
			continue
		}
		out = append(out, strings.Split(lipgloss.NewStyle().Width(max(10, width)).Render(para), "\n")...)
	}
	if len(out) == 0 {
		return []string{""}
	}
	if len(out) > n {
		out = out[:n]
		out[n-1] = ansi.Truncate(strings.TrimRight(out[n-1], " ")+" …", width, "…")
	}
	for i := range out {
		out[i] = strings.TrimRight(out[i], " ")
	}
	return out
}

// agoPhrase reads as prose: "just now", "5m ago", "on Aug 20".
func agoPhrase(t, now time.Time) string {
	switch a := ago(t, now); {
	case a == "now":
		return "just now"
	case now.Sub(t) < 7*24*time.Hour:
		return a + " ago"
	default:
		return "on " + a
	}
}

// showFile hands the terminal to a command run on path (a pager or an
// editor) and returns to cctree when it exits.
func showFile(path string, argv ...string) tea.Cmd {
	cmd := exec.Command(argv[0], append(argv[1:], path)...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return shownMsg{err} })
}

// editor is the person's editor command: $VISUAL, then $EDITOR, then vi.
func editor() []string {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if argv := strings.Fields(os.Getenv(name)); len(argv) > 0 {
			return argv
		}
	}
	return []string{"vi"}
}

// sortByTitle orders every folder's rows by name rather than by date.
func sortByTitle(f *Folder) {
	sort.SliceStable(f.Sessions, func(i, j int) bool {
		return strings.ToLower(f.Sessions[i].Title) < strings.ToLower(f.Sessions[j].Title)
	})
	for _, c := range f.Folders {
		sortByTitle(c)
	}
}

// tail keeps the end of s, at most n cells wide, marking the cut with ….
func tail(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > n-1 {
		r = r[1:]
	}
	return "…" + string(r)
}

// clean drops control characters, so text read from a file cannot send
// escape sequences to the terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

func isRunningPath(path string) bool {
	return path == runningPath || strings.HasPrefix(path, runningPath+"/")
}

// prefixPaths moves a tree's folder paths under prefix, so the running
// section's folders never share a key with the full tree's.
func prefixPaths(folders []*Folder, prefix string) {
	for _, f := range folders {
		f.Path = prefix + f.Path
		prefixPaths(f.Folders, prefix)
	}
}

func agentCount(agents []Agent) int {
	n := 0
	for _, a := range agents {
		if a.Type != "workflow" {
			n++
		}
	}
	return n
}

func tick() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func matches(s Session, query, homeDir string) bool {
	hay := strings.ToLower(s.Title + " " + s.Prompt + " " + DisplayPath(s.Cwd, homeDir) + " " + s.ID)
	for _, word := range strings.Fields(strings.ToLower(query)) {
		if !strings.Contains(hay, word) {
			return false
		}
	}
	return true
}

func ago(t time.Time, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("Jan 2006")
	}
}

// scrollTop keeps the cursor in the middle of a list taller than its box.
func scrollTop(cursor, height, total int) int {
	if total <= height {
		return 0
	}
	return clamp(cursor-height/2, 0, total-height)
}

func fill(lines []string, w, h int) string {
	for len(lines) < h {
		lines = append(lines, "")
	}
	lines = lines[:h]
	for i, l := range lines {
		if gap := w - lipgloss.Width(l); gap > 0 {
			lines[i] = l + strings.Repeat(" ", gap)
		}
	}
	return strings.Join(lines, "\n")
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return max(lo, min(v, hi))
}
