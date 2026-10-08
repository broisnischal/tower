// Package ui is tower's Bubble Tea front end. One model draws the narrow tmux
// sidebar, the wide dashboard (list plus the selected agent's live screen,
// log, diff and last reply) and the grid of every agent at once.
package ui

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"tower/internal/agent"
	"tower/internal/thread"
	"tower/internal/tmux"
)

type view int

const (
	viewLive   view = iota // what the agent is doing, from its transcript
	viewScreen             // its terminal
	viewDiff               // git diff of its folder
	viewLog                // tower's activity log
)

var viewNames = []string{"live", "screen", "diff", "log"}

// liveFeed follows one Claude Code transcript as a chat.
type liveFeed struct {
	feed thread.Feed
	chat chatState
}

type mode int

const (
	modeList mode = iota
	modeSend
	modeBroadcast
	modeForward
	modeNewName
	modeNewTask
	modeNewTree // y/N
	modeRename
	modeKill         // y/N
	modeThreadEngine // c/x
	modeThreadName
	modeThreadTask
	modeThreadTree // y/N
	modeThreadRename
	modeThreadRemove // y/N
	modeDelegate     // "@agent task", from the selected agent
)

// keyModes answer with one key instead of a line of text.
var keyModes = map[mode]bool{modeKill: true, modeNewTree: true, modeThreadEngine: true, modeThreadTree: true, modeThreadRemove: true}

type item struct {
	session tmux.Session
	agent   *agent.Agent   // a tmux agent row
	thread  *thread.Thread // a headless thread row
	header  bool           // the "threads" group row
}

func (it item) key() string {
	switch {
	case it.agent != nil:
		return "a" + it.agent.Pane
	case it.thread != nil:
		return "t" + it.thread.ID
	case it.header:
		return "h"
	}
	return "s" + it.session.Name
}

// rect is a clickable area of the grid.
type rect struct{ x0, y0, x1, y1, item int }

type model struct {
	sidebar bool
	self    string
	w, h    int

	agents   []agent.Agent
	sessions []tmux.Session
	items    []item

	tc        *thread.Client // nil until the daemon is reachable
	threads   []thread.Thread
	chats     map[string]*chatState
	focusChat bool     // keys go to the composer under the selected thread
	comp      composer // that composer
	newT      thread.CreateArgs
	want      string // thread to select once it shows up
	ticks     int
	cur       int
	selKey    string
	usage     map[string]*agent.Usage // by transcript path
	feeds     map[string]*liveFeed    // by transcript path
	comms     []agent.Message         // recent hand-offs between agents
	links     map[string]string       // pane -> open task it gives or works on
	showComms bool

	view     view
	grid     bool
	help     bool
	showLog  bool
	compact  bool
	byNum    []int             // item index of agent or thread number n+1
	logOff   int               // sidebar log panel: lines scrolled up from the newest
	logY     int               // first screen row of the log panel
	frame    int               // spinner frame
	visible  bool              // on screen; hidden sidebars do no work
	spinning bool              // a spin tick is scheduled
	screen   string            // selected agent's screen, for the screen view
	screens  map[string]string // every agent's screen, for the grid
	detail   map[string]string // diff and reply text, loaded off the UI goroutine

	// detail scrolling: dtop < 0 follows the view's anchor (bottom for
	// screen, log and reply; top for diff). dcur, dlen, dh are from the last draw.
	dtop, dcur, dlen, dh int

	mode    mode
	input   textinput.Model
	spawn   agent.SpawnOpts
	fwdFrom agent.Agent

	flash   string
	flashAt time.Time
	top     int         // list scroll offset
	rows    map[int]int // list row -> item index, for clicks
	tiles   []rect
	listW   int
}

type tickMsg time.Time
type spinMsg struct{}
type detailMsg struct{ key, text string }
type flashMsg string
type pushMsg thread.Push
type goneMsg struct{}

func waitPush(c *thread.Client) tea.Cmd {
	return func() tea.Msg {
		p, ok := c.Next()
		if !ok {
			return goneMsg{}
		}
		return pushMsg(p)
	}
}

func tick() tea.Cmd { return tickIn(time.Second) }

func tickIn(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func spin() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return spinMsg{} })
}

// wakeMsg arrives when tower signals a docked pane (SIGUSR1) because its
// window just came on screen: refresh now instead of at the next tick.
type wakeMsg struct{}

func run(model tea.Model, opts ...tea.ProgramOption) error {
	p := tea.NewProgram(model, opts...)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			p.Send(wakeMsg{})
		}
	}()
	go watchState(p)
	_, err := p.Run()
	return err
}

// fsWakeMsg arrives when an agent's state or log changed on disk.
type fsWakeMsg struct{}

// onScreen is whether this pane's window is visible; a hidden pane is not
// woken by changes on disk, only by its slow tick or by tower's signal.
var onScreen atomic.Bool

// watchState turns writes to agent state and logs into fsWakeMsg, at most
// one every 150 ms, so what is on screen changes the moment an agent does.
func watchState(p *tea.Program) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return
	}
	os.MkdirAll(agent.Dir(), 0o700)
	if _, err := syscall.InotifyAddWatch(fd, agent.Dir(), syscall.IN_CLOSE_WRITE|syscall.IN_MOVED_TO|syscall.IN_MODIFY|syscall.IN_DELETE); err != nil {
		return
	}
	var pending atomic.Bool
	buf := make([]byte, 16<<10)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil || n <= 0 {
			return
		}
		relevant := false
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			start := off + syscall.SizeofInotifyEvent
			name := strings.TrimRight(string(buf[start:start+int(ev.Len)]), "\x00")
			if strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".jsonl") {
				relevant = true
			}
			off = start + int(ev.Len)
		}
		if relevant && onScreen.Load() && pending.CompareAndSwap(false, true) {
			time.AfterFunc(150*time.Millisecond, func() {
				pending.Store(false)
				p.Send(fsWakeMsg{})
			})
		}
	}
}

// hiddenTick is how often a docked pane in a window I cannot see checks in.
const hiddenTick = 4 * time.Second

// Options pick how the UI starts.
type Options struct {
	Sidebar bool   // narrow layout for the sidebar pane
	Grid    bool   // start on the grid of every agent
	Thread  string // open on this headless thread, ready to type
}

// Run starts the UI.
func Run(o Options) error {
	in := textinput.New()
	in.Prompt = ""
	in.PromptStyle = accent
	m := &model{
		sidebar: o.Sidebar,
		grid:    o.Grid,
		self:    os.Getenv("TMUX_PANE"),
		usage:   map[string]*agent.Usage{},
		feeds:   map[string]*liveFeed{},
		screens: map[string]string{},
		detail:  map[string]string{},
		input:   in,
		dtop:    -1,
		chats:   map[string]*chatState{},
		comp:    newComposer("message this agent (@name to send elsewhere)"),
		want:    o.Thread,
	}
	m.comp.ta.Blur()
	m.connect(false)
	m.refresh()
	fps := 60
	if o.Sidebar { // a spinner needs about 7 frames a second; idle wakeups cost
		fps = 20
	}
	return run(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithFPS(fps))
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{tick(), m.startSpin(), m.detailCmd()}
	if m.tc != nil {
		cmds = append(cmds, waitPush(m.tc))
	}
	if m.focusChat {
		cmds = append(cmds, m.comp.ta.Focus())
	}
	return tea.Batch(cmds...)
}

// poll refreshes what is on screen. Every window has its own sidebar; only
// the one on screen does any work, and one left alone in its window closes.
func (m *model) poll() tea.Cmd {
	defer func() { onScreen.Store(m.visible) }()
	m.visible = true
	if m.sidebar {
		w := where(m.self)
		if w.orphan {
			return tea.Quit
		}
		if m.visible = w.visible; !w.visible {
			return nil
		}
		// Unless I am driving it, the sidebar points at the agent in the
		// window I am working in.
		if !w.focused && w.local != "" {
			m.selKey = "a" + w.local
		}
	}
	m.ticks++
	m.refresh()
	cmds := []tea.Cmd{m.detailCmd(), m.startSpin()}
	if m.tc == nil && m.ticks%5 == 0 && m.connect(false) { // the daemon came up
		m.rebuild()
		cmds = append(cmds, waitPush(m.tc))
	}
	return tea.Batch(cmds...)
}

func (m *model) anyWorking() bool {
	for _, a := range m.agents {
		if a.Status == agent.Working {
			return true
		}
	}
	for _, t := range m.threads {
		if t.Status == agent.Working {
			return true
		}
	}
	return false
}

// startSpin animates spinners while something on screen is working.
func (m *model) startSpin() tea.Cmd {
	if m.spinning || !m.visible || !m.anyWorking() {
		return nil
	}
	m.spinning = true
	return spin()
}

// connect attaches to the thread daemon, starting it when start is set.
func (m *model) connect(start bool) bool {
	if m.tc != nil {
		return false
	}
	c, err := thread.Dial(start)
	if err != nil {
		return false
	}
	ts, err := c.Watch()
	if err != nil {
		c.Close()
		return false
	}
	m.tc, m.threads, m.chats = c, ts, map[string]*chatState{}
	return true
}

// refresh reloads agents, sessions and screens.
func (m *model) refresh() {
	m.agents = agent.Load(true)
	m.comms = agent.Messages(200)
	m.links = map[string]string{}
	for _, t := range agent.OpenTasks(m.comms) {
		m.links[t.ToPane] = "⇠ task from " + t.From + ": " + t.Text
		if t.FromPane != "" {
			m.links[t.FromPane] = "⇢ waiting on " + t.To + ": " + t.Text
		}
	}
	m.sessions = tmux.Sessions()
	m.rebuild()
	m.capture()
}

// rebuild lays out the rows: headless threads first, then each tmux session
// with its agents, keeping the selection on the same row.
func (m *model) rebuild() {
	m.items = m.items[:0]
	for _, s := range m.sessions {
		m.items = append(m.items, item{session: s})
		for i := range m.agents {
			a := &m.agents[i]
			if a.Win.Session != s.Name {
				continue
			}
			m.items = append(m.items, item{session: s, agent: a})
			u := m.usage[a.Transcript]
			if u == nil {
				u = &agent.Usage{}
				m.usage[a.Transcript] = u
			}
			u.Update(a.Transcript)
		}
	}
	if len(m.threads) > 0 {
		m.items = append(m.items, item{header: true})
		for i := range m.threads {
			m.items = append(m.items, item{thread: &m.threads[i]})
		}
	}
	// Agents and threads are numbered in list order, so 1-9 jump to them
	// (and tower jump N, prefix j N reach the same agent).
	m.byNum = m.byNum[:0]
	for i, it := range m.items {
		if it.agent != nil || it.thread != nil {
			m.byNum = append(m.byNum, i)
		}
	}
	if m.want != "" {
		for _, it := range m.items {
			if it.thread != nil && (it.thread.ID == m.want || it.thread.Name == m.want) {
				m.selKey, m.want, m.focusChat = it.key(), "", true
			}
		}
	}
	found := false
	for i, it := range m.items {
		if it.key() == m.selKey {
			m.cur, found = i, true
			break
		}
	}
	if !found && (m.selKey == "" || m.grid) { // first load, or the grid lost its agent
		m.cur = m.firstAgent()
	}
	m.cur = clamp(m.cur, 0, len(m.items)-1)
	m.syncKey()
	if t := m.selThread(); t != nil {
		m.loadChat(t.ID)
	}
}

func (m *model) selThread() *thread.Thread {
	it, _ := m.sel()
	return it.thread
}

// loadChat fetches a thread's history the first time it is shown; pushes
// keep it current after that.
func (m *model) loadChat(id string) *chatState {
	if c := m.chats[id]; c != nil || m.tc == nil {
		return c
	}
	var ev []thread.Event
	if m.tc.Call("history", thread.Ref(id), &ev) != nil {
		return nil
	}
	c := &chatState{}
	for _, e := range ev {
		c.apply(e)
	}
	m.chats[id] = c
	return c
}

func (m *model) firstAgent() int {
	for i, it := range m.items {
		if it.agent != nil || it.thread != nil {
			return i
		}
	}
	return 0
}

func (m *model) sel() (item, bool) {
	if m.cur >= 0 && m.cur < len(m.items) {
		return m.items[m.cur], true
	}
	return item{}, false
}

func (m *model) selAgent() *agent.Agent {
	it, _ := m.sel()
	return it.agent
}

func (m *model) syncKey() {
	if it, ok := m.sel(); ok {
		m.selKey = it.key()
	}
}

func (m *model) capture() {
	m.screen = ""
	if m.sidebar {
		return
	}
	if m.grid {
		for i := range m.agents {
			if m.follow(&m.agents[i]) == nil {
				m.screens[m.agents[i].Pane] = tmux.Run("capture-pane", "-p", "-e", "-t", m.agents[i].Pane)
			}
		}
		return
	}
	if a := m.selAgent(); a != nil && m.view == viewLive {
		m.follow(a)
	}
	if a := m.selAgent(); a != nil && m.view == viewScreen {
		m.screen = tmux.Run("capture-pane", "-p", "-e", "-t", a.Pane)
	}
}

func (m *model) say(s string) { m.flash, m.flashAt = s, time.Now() }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		cmd := m.poll()
		next := time.Second
		if !m.visible {
			next = hiddenTick
		}
		return m, tea.Batch(cmd, tickIn(next))
	case wakeMsg:
		return m, m.poll()
	case fsWakeMsg:
		if !m.visible {
			return m, nil
		}
		return m, m.poll()
	case pushMsg:
		m.onPush(thread.Push(msg))
		return m, waitPush(m.tc)
	case goneMsg:
		m.tc, m.threads = nil, nil
		m.rebuild()
		return m, nil
	case attachMsg, voiceMsg:
		_, cmd := m.comp.Update(msg)
		return m, cmd
	case spinMsg:
		if !m.visible || !m.anyWorking() {
			m.spinning = false
			return m, nil
		}
		m.frame++
		return m, spin()
	case selectMsg:
		m.want = string(msg)
		m.rebuild()
		if m.focusChat {
			return m, m.comp.ta.Focus()
		}
	case detailMsg:
		m.detail[msg.key] = msg.text
	case flashMsg:
		m.say(string(msg))
	case tea.MouseMsg:
		return m, m.onMouse(msg)
	case tea.KeyMsg:
		if m.help {
			m.help = false
			return m, nil
		}
		if m.mode != modeList {
			return m, m.onInput(msg)
		}
		if m.focusChat {
			return m, m.onChatKey(msg)
		}
		if m.grid {
			if cmd, ok := m.onGridKey(msg); ok {
				return m, cmd
			}
		}
		return m, m.onKey(msg)
	}
	return m, nil
}

func (m *model) onPush(p thread.Push) {
	switch p.Kind {
	case "thread":
		found := false
		for i := range m.threads {
			if m.threads[i].ID == p.Thread.ID {
				m.threads[i], found = *p.Thread, true
			}
		}
		if !found {
			m.threads = append(m.threads, *p.Thread)
		}
		m.rebuild()
	case "removed":
		for i := range m.threads {
			if m.threads[i].ID == p.ThreadID {
				m.threads = append(m.threads[:i], m.threads[i+1:]...)
				break
			}
		}
		delete(m.chats, p.ThreadID)
		m.rebuild()
	case "event":
		if c := m.chats[p.ThreadID]; c != nil && p.Event != nil {
			c.apply(*p.Event)
		}
	}
}

// onChatKey handles keys while I am typing to the selected thread.
func (m *model) onChatKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.focusChat = false
		m.comp.ta.Blur()
		return nil
	case "ctrl+c":
		return tea.Quit
	}
	submit, cmd := m.comp.Update(k)
	if !submit || m.comp.Empty() {
		return cmd
	}
	t := m.selThread()
	if t == nil || m.tc == nil {
		return nil
	}
	text, images := m.comp.Message(false)
	m.comp.Reset()
	return m.threadCall("send", map[string]any{"thread": t.ID, "text": text, "images": images}, "")
}

// threadCall runs a daemon call in the background and flashes its outcome.
func (m *model) threadCall(op string, args any, ok string) tea.Cmd {
	c := m.tc
	if c == nil {
		return func() tea.Msg { return flashMsg("the tower daemon is not running") }
	}
	return func() tea.Msg {
		if err := c.Call(op, args, nil); err != nil {
			return flashMsg(err.Error())
		}
		if ok == "" {
			return nil
		}
		return flashMsg(ok)
	}
}

// answer resolves the selected thread's oldest pending approval.
func (m *model) answer(t *thread.Thread, d thread.Decision) tea.Cmd {
	c := m.loadChat(t.ID)
	if c == nil {
		return nil
	}
	p, ok := c.PendingApproval()
	if !ok {
		m.say(t.Name + " is not waiting on anything")
		return nil
	}
	return m.threadCall("answer", map[string]any{"thread": t.ID, "approval": p.ID, "decision": d}, string(d)+" "+p.Tool+" "+p.Input)
}

func (m *model) onThreadKey(t *thread.Thread, k string) (tea.Cmd, bool) {
	switch k {
	case "enter", "i", "s":
		if m.sidebar { // too narrow to chat: open it in the dashboard
			popup("tower", "92%", "88%", "ui", "--thread", t.ID)
			return nil, true
		}
		m.focusChat = true
		return m.comp.ta.Focus(), true
	case "y":
		return m.answer(t, thread.Allow), true
	case "A":
		return m.answer(t, thread.Always), true
	case "d":
		return m.answer(t, thread.Deny), true
	case "x":
		return m.threadCall("interrupt", thread.Ref(t.ID), "interrupted "+t.Name), true
	case "X":
		return m.ask(modeThreadRemove, "remove thread "+t.Name+" and its history? y/N ", ""), true
	case "r":
		return m.ask(modeThreadRename, "rename: ", t.Name), true
	}
	return nil, false
}

func (m *model) onKey(k tea.KeyMsg) tea.Cmd {
	it, _ := m.sel()
	a := it.agent
	if it.thread != nil {
		if cmd, ok := m.onThreadKey(it.thread, k.String()); ok {
			return cmd
		}
	}
	switch k.String() {
	case "N":
		m.newT = thread.CreateArgs{Cwd: m.dirFor(it), Mode: "default"}
		return m.ask(modeThreadEngine, "new headless agent: [c]laude or code[x]? ", "")
	case "ctrl+c":
		return tea.Quit
	case "?":
		m.help = true
	case "q":
		if m.sidebar {
			tmux.Run("set", "-g", "@tower_sidebar_on", "0")
		}
		return tea.Quit
	case "esc":
		if !m.sidebar {
			return tea.Quit
		}
		tmux.Run("last-pane", "-t", m.self)
	case "tab":
		if m.sidebar {
			tmux.Run("last-pane", "-t", m.self)
			return nil
		}
		return m.setView((m.view + 1) % 4)
	case "shift+tab":
		return m.setView((m.view + 3) % 4)
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if n := int(k.String()[0] - '1'); n < len(m.byNum) {
			m.move(m.byNum[n] - m.cur)
			return m.jump(m.items[m.byNum[n]])
		}
	case "j", "down":
		return m.move(1)
	case "k", "up":
		return m.move(-1)
	case "g", "home":
		return m.move(-len(m.items))
	case "G", "end":
		return m.move(len(m.items))
	case "a":
		return m.nextAttention()
	case "pgup", "ctrl+u":
		if m.sidebar {
			m.logOff += 5
			return nil
		}
		m.scroll(-max(1, m.dh/2))
	case "pgdown", "ctrl+d":
		if m.sidebar {
			m.logOff = max(0, m.logOff-5)
			return nil
		}
		m.scroll(max(1, m.dh/2))
	case "enter":
		return m.jump(it)
	case "v":
		if m.sidebar {
			popup("agents", "95%", "90%", "grid")
			return nil
		}
		m.grid = !m.grid
		m.capture()
	case "o":
		if m.sidebar {
			popup("tower", "92%", "88%", "ui")
		}
	case "s":
		if a == nil {
			return nil
		}
		if m.sidebar { // 40 columns is too narrow to write in: open the editor
			popup("message "+a.Name, "80%", "12", "compose", a.Pane)
			return nil
		}
		return m.ask(modeSend, "→ "+a.Name+" (@name to redirect): ", "")
	case "b":
		return m.ask(modeBroadcast, "→ every agent: ", "")
	case "f":
		if a != nil {
			m.fwdFrom = *a
			return m.ask(modeForward, "forward "+a.Name+"'s last reply to: ", "")
		}
	case "n":
		m.spawn = agent.SpawnOpts{Session: it.session.Name, Dir: m.dirFor(it)}
		return m.ask(modeNewName, "name: ", "")
	case "r":
		if a != nil {
			return m.ask(modeRename, "rename: ", a.Name)
		}
	case "y":
		if a != nil && a.Status == agent.Waiting {
			tmux.Run("send-keys", "-t", a.Pane, "Enter")
			m.say("approved " + a.Name)
		}
	case "x":
		if a != nil {
			tmux.Run("send-keys", "-t", a.Pane, "Escape")
			m.say("sent Esc to " + a.Name)
		}
	case "X":
		if a != nil {
			return m.ask(modeKill, "close "+a.Name+"? y/N ", "")
		}
	case "l":
		m.showLog, m.showComms = !m.showLog, false
	case "m":
		m.showComms = !m.showComms
		m.showLog = m.showComms && m.sidebar
	case "D":
		if a != nil {
			return m.ask(modeDelegate, "assign from "+a.Name+" → @agent task: ", "@")
		}
	case "c":
		m.compact = !m.compact
	}
	return nil
}

// onGridKey handles the keys that mean something else on the grid. It
// reports false for keys the list handles the same way.
func (m *model) onGridKey(k tea.KeyMsg) (tea.Cmd, bool) {
	cols := max(1, m.gridCols())
	switch k.String() {
	case "h", "left":
		return m.moveAgent(-1), true
	case "l", "right":
		return m.moveAgent(1), true
	case "j", "down":
		return m.moveAgent(cols), true
	case "k", "up":
		return m.moveAgent(-cols), true
	}
	return nil, false
}

// moveAgent moves the selection d agents along, skipping session rows.
func (m *model) moveAgent(d int) tea.Cmd {
	var idx []int
	pos := 0
	for i, it := range m.items {
		if it.agent != nil {
			if i == m.cur {
				pos = len(idx)
			}
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return nil
	}
	return m.move(idx[clamp(pos+d, 0, len(idx)-1)] - m.cur)
}

func (m *model) onMouse(e tea.MouseMsg) tea.Cmd {
	if m.grid {
		if e.Button == tea.MouseButtonLeft && e.Action == tea.MouseActionPress {
			for _, r := range m.tiles {
				if e.X >= r.x0 && e.X < r.x1 && e.Y >= r.y0 && e.Y < r.y1 {
					m.move(r.item - m.cur)
					return m.jump(m.items[r.item])
				}
			}
		}
		return nil
	}
	inDetail := !m.sidebar && e.X >= m.listW
	inLog := m.sidebar && m.showLog && e.Y >= m.logY
	switch {
	case inLog && e.Button == tea.MouseButtonWheelUp:
		m.logOff += 3
		return nil
	case inLog && e.Button == tea.MouseButtonWheelDown:
		m.logOff = max(0, m.logOff-3)
		return nil
	case e.Button == tea.MouseButtonWheelUp:
		if inDetail {
			m.scroll(-3)
			return nil
		}
		return m.move(-1)
	case e.Button == tea.MouseButtonWheelDown:
		if inDetail {
			m.scroll(3)
			return nil
		}
		return m.move(1)
	case e.Button == tea.MouseButtonLeft && e.Action == tea.MouseActionPress:
		i, ok := m.rows[e.Y]
		if !ok || inDetail {
			return nil
		}
		m.move(i - m.cur)
		return m.jump(m.items[i])
	}
	return nil
}

func (m *model) move(d int) tea.Cmd {
	m.cur = clamp(m.cur+d, 0, len(m.items)-1)
	m.dtop = -1
	m.syncKey()
	if !m.grid {
		m.capture()
	}
	if t := m.selThread(); t != nil {
		m.loadChat(t.ID)
	}
	return m.detailCmd()
}

// nextAttention selects the next agent that is waiting on me or finished.
func (m *model) nextAttention() tea.Cmd {
	for i := 1; i <= len(m.items); i++ {
		j := (m.cur + i) % len(m.items)
		if a := m.items[j].agent; a != nil && (a.Status == agent.Waiting || a.Status == agent.Done) {
			return m.move(j - m.cur)
		}
	}
	return nil
}

func (m *model) setView(v view) tea.Cmd {
	m.view, m.dtop = v, -1
	m.capture()
	return m.detailCmd()
}

func (m *model) scroll(d int) {
	top := m.dcur + d
	last := max(0, m.dlen-m.dh)
	if top >= last && m.view != viewDiff {
		m.dtop = -1 // back to following the bottom
		return
	}
	m.dtop = clamp(top, 0, last)
}

func (m *model) jump(it item) tea.Cmd {
	switch {
	case it.thread != nil:
		if m.sidebar {
			popup("tower", "92%", "88%", "ui", "--thread", it.thread.ID)
			return nil
		}
		m.grid, m.focusChat = false, true
		return m.comp.ta.Focus()
	case it.agent != nil:
		agent.Jump(it.agent.Pane)
	case it.session.Name != "":
		tmux.Run("switch-client", "-t", "="+it.session.Name)
	default:
		return nil
	}
	if m.sidebar {
		return nil
	}
	return tea.Quit // the dashboard is a popup: get out of the way
}

// popup opens another tower command in a tmux popup without waiting for it.
func popup(title, w, h string, args ...string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := agent.ShellQuote(self)
	for _, a := range args {
		cmd += " " + agent.ShellQuote(a)
	}
	exec.Command("tmux", "display-popup", "-E", "-w", w, "-h", h, "-T", " "+title+" ", cmd).Start()
}

func (m *model) ask(md mode, prompt, value string) tea.Cmd {
	m.mode = md
	m.input.Prompt = prompt
	m.input.SetValue(value)
	m.input.CursorEnd()
	return m.input.Focus()
}

func (m *model) endInput() {
	m.mode = modeList
	m.input.Blur()
	m.input.SetValue("")
}

func (m *model) onInput(k tea.KeyMsg) tea.Cmd {
	md := m.mode
	if keyModes[md] {
		m.endInput()
		if md == modeThreadEngine {
			switch k.String() {
			case "c", "C":
				m.newT.Engine = "claude"
			case "x", "X":
				m.newT.Engine = "codex"
			default:
				return nil
			}
			return m.ask(modeThreadName, "name ("+m.newT.Engine+"): ", "")
		}
		return m.confirm(md, k.String() == "y" || k.String() == "Y")
	}
	switch k.String() {
	case "esc", "ctrl+c":
		m.endInput()
		return nil
	case "enter":
		v := strings.TrimSpace(m.input.Value())
		m.endInput()
		return m.submit(md, v)
	case "ctrl+v":
		if p, err := saveClipboard("image/png"); err == nil {
			m.input.SetValue(m.input.Value() + p + " ")
			m.input.CursorEnd()
			return nil
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return cmd
}

func (m *model) submit(md mode, v string) tea.Cmd {
	it, _ := m.sel()
	switch md {
	case modeSend, modeBroadcast:
		if v == "" {
			return nil
		}
		def := m.agents
		if md == modeSend {
			if it.agent == nil {
				return nil
			}
			def = []agent.Agent{*it.agent}
		}
		return sendCmd(v, def, append([]agent.Agent(nil), m.agents...))
	case modeForward:
		from := m.fwdFrom
		agents := append([]agent.Agent(nil), m.agents...)
		return func() tea.Msg {
			reply := agent.LastReply(from.Transcript)
			if reply == "" {
				return flashMsg(from.Name + " has no reply to forward yet")
			}
			to, _, err := agent.Route(mentions(v)+" x", nil, agents)
			if err != nil {
				return flashMsg(err.Error())
			}
			if len(to) == 0 {
				return flashMsg("forward to whom? type one or more agent names")
			}
			return sendAs(from.Name, "forward", fmt.Sprintf("[forwarded from agent %q]\n%s", from.Name, reply), to)
		}
	case modeNewName:
		m.spawn.Name = v
		return m.ask(modeNewTask, "task (optional): ", "")
	case modeNewTask:
		m.spawn.Prompt = v
		if agent.IsGitRepo(m.spawn.Dir) {
			return m.ask(modeNewTree, "own git worktree? y/N ", "")
		}
		return m.startSpawn()
	case modeRename:
		if it.agent != nil && v != "" {
			agent.Rename(it.agent.Pane, v)
			m.refresh()
		}
	case modeDelegate:
		from := it.agent
		if from == nil {
			return nil
		}
		to, task, err := agent.Route(v, nil, m.agents)
		switch {
		case err != nil:
			return func() tea.Msg { return flashMsg(err.Error()) }
		case len(to) != 1 || task == "":
			return func() tea.Msg { return flashMsg("write it as: @agent the task") }
		case to[0].Pane == from.Pane:
			return func() tea.Msg { return flashMsg("pick another agent") }
		}
		fromPane, toPane, toName := from.Pane, to[0].Pane, to[0].Name
		return func() tea.Msg {
			c, err := thread.Dial(true)
			if err != nil {
				return flashMsg("tower daemon: " + err.Error())
			}
			defer c.Close()
			if err := c.Call("relay", thread.RelayArgs{From: fromPane, To: toPane, Text: task}, nil); err != nil {
				return flashMsg(err.Error())
			}
			return flashMsg("assigned to " + toName + "; the answer goes back to " + from.Name)
		}
	case modeThreadName:
		m.newT.Name = v
		return m.ask(modeThreadTask, "task (optional): ", "")
	case modeThreadTask:
		m.newT.Prompt = v
		if agent.IsGitRepo(m.newT.Cwd) {
			return m.ask(modeThreadTree, "own git worktree? y/N ", "")
		}
		return m.createThread()
	case modeThreadRename:
		if t := m.selThread(); t != nil && v != "" {
			return m.threadCall("rename", map[string]string{"thread": t.ID, "name": v}, "")
		}
	}
	return nil
}

// createThread starts a headless thread, starting the daemon if needed, and
// selects it ready to type.
func (m *model) createThread() tea.Cmd {
	fresh := m.connect(true)
	if m.tc == nil {
		m.say("could not start the tower daemon")
		return nil
	}
	a, c := m.newT, m.tc
	m.say("starting " + a.Engine)
	var cmds []tea.Cmd
	if fresh {
		cmds = append(cmds, waitPush(c))
	}
	cmds = append(cmds, func() tea.Msg {
		var t thread.Thread
		if err := c.Call("create", a, &t); err != nil {
			return flashMsg(err.Error())
		}
		return selectMsg(t.ID)
	})
	return tea.Batch(cmds...)
}

type selectMsg string

// mentions turns "api docs" or "@api @docs" into "@api @docs".
func mentions(s string) string {
	var out []string
	for _, f := range strings.Fields(s) {
		out = append(out, "@"+strings.TrimPrefix(f, "@"))
	}
	return strings.Join(out, " ")
}

// sendCmd routes text by its leading @mentions (default def) and sends it.
func sendCmd(text string, def, agents []agent.Agent) tea.Cmd {
	return func() tea.Msg {
		to, body, err := agent.Route(text, def, agents)
		if err != nil {
			return flashMsg(err.Error())
		}
		if body == "" {
			return flashMsg("nothing to send")
		}
		return send(body, to)
	}
}

func send(text string, to []agent.Agent) tea.Msg { return sendAs("me", "message", text, to) }

func sendAs(from, kind, text string, to []agent.Agent) tea.Msg {
	var sent, failed []string
	for _, a := range to {
		if err := agent.Send(a, text, false); err != nil {
			failed = append(failed, err.Error())
		} else {
			sent = append(sent, a.Name)
			agent.LogMessage(agent.Message{Kind: kind, From: from, To: a.Name, ToPane: a.Pane, Text: text})
		}
	}
	switch {
	case len(failed) > 0 && len(sent) == 0:
		return flashMsg(strings.Join(failed, "; "))
	case len(failed) > 0:
		return flashMsg("sent to " + strings.Join(sent, ", ") + "; " + strings.Join(failed, "; "))
	}
	return flashMsg("sent to " + strings.Join(sent, ", "))
}

func (m *model) confirm(md mode, yes bool) tea.Cmd {
	switch md {
	case modeKill:
		if a := m.selAgent(); yes && a != nil {
			tmux.Run("kill-pane", "-t", a.Pane)
			m.say("closed " + a.Name)
		}
	case modeNewTree:
		m.spawn.Worktree = yes
		return m.startSpawn()
	case modeThreadTree:
		m.newT.Worktree = yes
		return m.createThread()
	case modeThreadRemove:
		if t := m.selThread(); yes && t != nil {
			return m.threadCall("remove", thread.Ref(t.ID), "removed "+t.Name)
		}
	}
	return nil
}

func (m *model) startSpawn() tea.Cmd {
	o := m.spawn
	m.say("starting " + o.Name)
	return func() tea.Msg {
		_, name, err := agent.Spawn(o)
		if err != nil {
			return flashMsg(err.Error())
		}
		return flashMsg("started " + name)
	}
}

// dirFor picks where a new agent starts: next to the selected agent, or in
// the selected session's current pane.
func (m *model) dirFor(it item) string {
	if it.agent != nil && it.agent.Cwd != "" {
		return it.agent.Cwd
	}
	if it.thread != nil {
		return it.thread.Cwd
	}
	if it.session.Name != "" {
		if d := tmux.Run("display", "-p", "-t", "="+it.session.Name+":", "#{pane_current_path}"); d != "" {
			return d
		}
	}
	d, _ := os.Getwd()
	return d
}

func detailKey(v view, pane string) string { return strconv.Itoa(int(v)) + pane }

// detailCmd loads the diff or reply view in the background.
func (m *model) detailCmd() tea.Cmd {
	if t := m.selThread(); t != nil && !m.sidebar && !m.grid && m.view == viewDiff {
		key, dir := detailKey(viewDiff, "t"+t.ID), t.Cwd
		return func() tea.Msg { return detailMsg{key, gitDiff(dir)} }
	}
	a := m.selAgent()
	if m.sidebar || m.grid || a == nil {
		return nil
	}
	key := detailKey(m.view, a.Pane)
	switch m.view {
	case viewDiff:
		dir := a.Cwd
		return func() tea.Msg { return detailMsg{key, gitDiff(dir)} }
	}
	return nil
}

// follow brings an agent's live chat up to date with its transcript.
func (m *model) follow(a *agent.Agent) *liveFeed {
	if a == nil || a.Transcript == "" {
		return nil
	}
	f := m.feeds[a.Transcript]
	if f == nil {
		f = &liveFeed{}
		m.feeds[a.Transcript] = f
	}
	for _, e := range f.feed.Read(a.Transcript) {
		f.chat.Apply(e)
	}
	if n := len(f.chat.Items); n > 1200 { // keep the recent part; long sessions run to thousands
		var c thread.Chat
		for _, e := range f.chat.Items[n-800:] {
			c.Apply(e)
		}
		f.chat.Chat = c
	}
	return f
}

func gitDiff(dir string) string {
	if !agent.IsGitRepo(dir) {
		return dim.Render("not a git repo: " + dir)
	}
	git := func(args ...string) string {
		out, _ := exec.Command("git", append([]string{"-C", dir, "-c", "color.ui=always"}, args...)...).Output()
		return strings.TrimRight(string(out), "\n")
	}
	parts := []string{git("status", "-sb")}
	d := git("diff", "HEAD")
	if d == "" {
		d = git("diff") // no commits yet
	}
	if d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, "\n\n")
}
