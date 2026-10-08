package browser

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/jpeg" // screencast frames for half blocks
	_ "image/png"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// frameEvery caps the frame rate at 20 fps.
const frameEvery = 50 * time.Millisecond

// Options configure the viewer.
type Options struct {
	Render string // "kitty", "blocks", or "" to detect
}

var (
	accent = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	warn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	spin   = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
)

// Columns of the bar's buttons; the address starts at urlCol.
const (
	backCol   = 1
	fwdCol    = 3
	reloadCol = 5
	urlCol    = 7
)

type viewer struct {
	start string // URL to open once Chrome is up
	term  terminal
	out   *tty
	ids   [2]imageID
	send  func(tea.Msg)

	s       *session                // nil until Chrome is up
	sess    atomic.Pointer[session] // for shutdown, set as soon as Chrome runs
	lay     atomic.Pointer[layout]  // read by the frame pump
	started chan struct{}           // closed when the launch is over
	done    chan struct{}           // stops the pump and the visibility poll
	closing atomic.Bool

	w, h         int
	cellW, cellH int
	scale        float64 // display scale Chrome runs at
	zoom         float64 // my zoom on top of it
	resizes      int     // debounces viewport changes

	lines  []string // the page area
	placed map[placeKey][]string

	url, title    string
	loading       bool
	back, forward bool
	edit, fresh   bool
	in            textinput.Model
	dialog        *dialogMsg
	flash         string
	flashAt       time.Time
	spin          int
	spinning      bool

	held                  tea.MouseButton // for drags and the release
	clickAt               time.Time
	clickX, clickY, click int

	err error
}

type placeKey struct {
	id         imageID
	cols, rows int
}

type (
	readyMsg  struct{ s *session }
	errMsg    struct{ err error }
	linesMsg  []string
	kittyMsg  placeKey
	resizeMsg int
	spinMsg   struct{}
	redrawMsg struct{}
)

// flashFor is how long a note stays in the bar.
const flashFor = 4 * time.Second

// Run shows url in this pane until I quit.
func Run(url string, o Options) error {
	v := &viewer{
		start:   url,
		url:     url,
		term:    detectTerminal(o.Render),
		out:     &tty{f: os.Stdout},
		ids:     imageIDs(os.Getpid()),
		started: make(chan struct{}),
		done:    make(chan struct{}),
		zoom:    1,
		placed:  map[placeKey][]string{},
	}
	v.cellW, v.cellH = v.cells()
	v.scale = 1
	if v.term.render == renderKitty {
		// Only kitty shows frames pixel for pixel; half blocks gain nothing
		// from bigger ones.
		v.scale = displayScale(v.cellH)
	}
	v.in = textinput.New()
	v.in.Prompt = ""
	p := tea.NewProgram(v, tea.WithAltScreen(), tea.WithMouseAllMotion(), tea.WithOutput(v.out))
	v.send = p.Send
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP) // the pane closed: still shut Chrome down
	go func() {
		if _, ok := <-hup; ok {
			p.Quit()
		}
	}()
	_, err := p.Run()
	signal.Stop(hup)
	close(hup)
	v.shutdown()
	if err == nil {
		err = v.err
	}
	return err
}

func (v *viewer) Init() tea.Cmd {
	return tea.Batch(v.launch, v.startSpin())
}

// launch starts Chrome and attaches to its first tab.
func (v *viewer) launch() tea.Msg {
	defer close(v.started)
	dir, temp, err := profile(false)
	if err != nil {
		return errMsg{err}
	}
	ch, err := launchChrome(dir, temp, v.scale)
	if err != nil {
		return errMsg{err}
	}
	s := &session{chrome: ch, ui: v.send, frames: newFrames()}
	ws, err := dialWS(ch.ws, 10*time.Second)
	if err != nil {
		ch.kill()
		return errMsg{err}
	}
	s.conn = newCDP(ws, s.handle)
	v.sess.Store(s)
	if v.closing.Load() {
		return nil
	}
	ctx, cancel := timeout(10 * time.Second)
	defer cancel()
	if err := s.conn.call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil); err != nil {
		return errMsg{err}
	}
	target, err := firstPage(s.conn)
	if err != nil {
		return errMsg{err}
	}
	t, err := attach(s.conn, target)
	if err != nil {
		return errMsg{err}
	}
	s.mu.Lock()
	s.tabs = []*tab{t}
	s.mu.Unlock()
	return readyMsg{s}
}

// shutdown runs after Bubble Tea has let go of the terminal.
func (v *viewer) shutdown() {
	v.closing.Store(true)
	close(v.done)
	select {
	case <-v.started:
	case <-time.After(30 * time.Second):
	}
	if s := v.sess.Load(); s != nil {
		s.chrome.close(s.conn)
	}
	if v.term.render == renderKitty {
		v.out.WriteString(kittyDelete(v.ids[0], v.term.tmux) + kittyDelete(v.ids[1], v.term.tmux))
	}
}

func (v *viewer) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return v, v.resize(msg.Width, msg.Height)
	case resizeMsg:
		if int(msg) == v.resizes {
			return v, v.apply()
		}
	case readyMsg:
		return v, v.ready(msg.s)
	case errMsg:
		v.err = msg.err
		return v, tea.Quit
	case closedMsg:
		return v, tea.Quit
	case linesMsg:
		v.lines = msg
	case kittyMsg:
		v.lines = v.placeholders(placeKey(msg))
	case pageMsg:
		if !v.edit {
			v.url = msg.url
		}
		v.title = msg.title
	case loadingMsg:
		v.loading = bool(msg)
		return v, v.startSpin()
	case historyMsg:
		v.back, v.forward = msg.back, msg.forward
	case dialogMsg:
		v.dialog = &msg
	case flashMsg:
		v.flash, v.flashAt = string(msg), time.Now()
		return v, tea.Tick(flashFor, func(time.Time) tea.Msg { return redrawMsg{} })
	case redrawMsg:
	case spinMsg:
		v.spinning = false
		v.spin++
		return v, v.startSpin()
	case tea.KeyMsg:
		return v, v.key(msg)
	case tea.MouseMsg:
		return v, v.mouse(tea.MouseEvent(msg))
	default:
		if v.edit { // cursor blinks
			var cmd tea.Cmd
			v.in, cmd = v.in.Update(msg)
			return v, cmd
		}
	}
	return v, nil
}

// ready starts drawing and loads the first page once the viewport fits.
func (v *viewer) ready(s *session) tea.Cmd {
	v.s = s
	go v.pump(s)
	go v.watchChrome(s)
	if v.term.tmux && v.term.pane != "" {
		go v.watchVisible(s)
	}
	l := v.layout()
	t, url := s.current(), v.start
	return func() tea.Msg {
		if err := s.resize(l); err != nil {
			return flashMsg(err.Error())
		}
		if err := t.navigate(url); err != nil {
			return flashMsg(err.Error())
		}
		return nil
	}
}

// watchChrome ends the viewer if Chrome goes away under it.
func (v *viewer) watchChrome(s *session) {
	select {
	case <-s.conn.done:
		v.send(errMsg{s.conn.err})
	case <-v.done:
	}
}

// startSpin keeps the loading spinner turning, and only while it is needed.
func (v *viewer) startSpin() tea.Cmd {
	if v.spinning || !(v.loading || v.s == nil) {
		return nil
	}
	v.spinning = true
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return spinMsg{} })
}

// layout is the page area for the current pane size and zoom.
func (v *viewer) layout() layout {
	return layout{
		cols: v.w, rows: max(0, v.h-1),
		cellW: v.cellW, cellH: v.cellH,
		scale: v.scale, zoom: v.zoom,
		render: v.term.render,
	}
}

// resize redraws the last frame at the new size right away, and resizes
// the page once the pane has settled.
func (v *viewer) resize(w, h int) tea.Cmd {
	v.w, v.h = w, h
	v.cellW, v.cellH = v.cells()
	l := v.layout()
	v.lay.Store(&l)
	if v.s != nil {
		v.s.frames.redraw()
	}
	v.resizes++
	n := v.resizes
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return resizeMsg(n) })
}

// cells is the cell size in pixels, with a fallback when the terminal
// keeps it to itself.
func (v *viewer) cells() (w, h int) {
	w, h = cellPixels(os.Stdout, v.term.pane)
	if w == 0 || h == 0 {
		return defaultCellW, defaultCellH
	}
	return w, h
}

// apply sends the current layout to Chrome.
func (v *viewer) apply() tea.Cmd {
	if v.s == nil {
		return nil
	}
	s, l := v.s, v.layout()
	v.lay.Store(&l)
	return func() tea.Msg {
		if err := s.resize(l); err != nil {
			return flashMsg(err.Error())
		}
		return nil
	}
}

// pump draws screencast frames as they come, at most one per frameEvery.
// Chrome waits for my acks before sending more, so acking after the pause
// is what holds the rate down; a still page sends nothing at all.
func (v *viewer) pump(s *session) {
	n := 0
	for {
		select {
		case <-s.frames.ready:
		case <-v.done:
			return
		}
		start := time.Now()
		fr, acks := s.frames.take()
		if fr != nil && v.draw(fr, n) {
			n++
		}
		select {
		case <-time.After(frameEvery - time.Since(start)):
		case <-v.done:
			return
		}
		for _, a := range acks {
			s.conn.send(a.session, "Page.screencastFrameAck", map[string]any{"sessionId": a.id})
		}
	}
}

// draw turns a frame into what the page area shows.
func (v *viewer) draw(fr *screencastFrame, n int) bool {
	l := v.lay.Load()
	if l == nil || !l.ok() {
		return false
	}
	cols, rows := l.imageCells()
	if l.render == renderKitty {
		id := v.ids[n%2]
		v.out.WriteString(kittyUpload(id, fr.Data, cols, rows, v.term.tmux))
		v.send(kittyMsg{id, cols, rows})
		return true
	}
	b, err := base64.StdEncoding.DecodeString(fr.Data)
	if err != nil {
		return false
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return false
	}
	v.send(linesMsg(halfBlocks(img, cols, rows)))
	return true
}

func (v *viewer) placeholders(k placeKey) []string {
	if p, ok := v.placed[k]; ok {
		return p
	}
	if len(v.placed) > 4 {
		clear(v.placed)
	}
	p := placeholders(k.id, k.cols, k.rows)
	v.placed[k] = p
	return p
}

// watchVisible stops the frames while the pane is off screen and brings
// them back when it shows again. With kitty graphics that also uploads the
// picture again, since tmux drops passthrough for hidden panes.
func (v *viewer) watchVisible(s *session) {
	seen, visible := false, true
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-v.done:
			return
		case <-tick.C:
		}
		now := paneVisible(v.term.pane)
		seen = seen || now
		if now == visible || !seen {
			continue
		}
		visible = now
		if !now {
			s.pause()
			continue
		}
		if l := v.lay.Load(); l != nil {
			s.resize(*l)
		}
		s.frames.redraw()
	}
}

func (v *viewer) View() string {
	if v.w == 0 || v.h == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(v.bar())
	for i := range v.h - 1 {
		sb.WriteByte('\n')
		if i < len(v.lines) {
			sb.WriteString(v.lines[i])
		}
	}
	return sb.String()
}

// bar is the one row of browser chrome: back, forward, reload (stop while
// loading), the address or the dialog the page opened, the title, and a
// spinner while the page loads.
func (v *viewer) bar() string {
	btn := func(s string, on bool) string {
		if on {
			return accent.Render(s)
		}
		return dim.Render(s)
	}
	reload := "↻"
	if v.loading {
		reload = "✕"
	}
	left := " " + btn("←", v.back) + " " + btn("→", v.forward) + " " + btn(reload, v.s != nil) + "  "

	var right string
	switch {
	case v.flash != "" && time.Since(v.flashAt) < flashFor:
		right = warn.Render(ansi.Truncate(v.flash, max(10, v.w/2), "…")) + " "
	case v.loading || v.s == nil:
		right = accent.Render(spin[v.spin%len(spin)]) + " "
	}
	room := max(0, v.w-urlCol-lipgloss.Width(right)-1)

	var mid string
	switch {
	case v.dialog != nil:
		mid = warn.Render(v.dialog.kind+": ") + oneLine(v.dialog.text) + dim.Render("  enter ok · esc cancel")
	case v.edit:
		v.in.Width = max(1, room-1)
		mid = v.in.View()
	case v.s == nil:
		mid = dim.Render("starting Chrome for " + v.url)
	default:
		mid = v.url
		if v.title != "" && v.title != v.url {
			mid += "  " + dim.Render(oneLine(v.title))
		}
	}
	mid = ansi.Truncate(mid, room, "…")
	pad := max(0, v.w-urlCol-lipgloss.Width(mid)-lipgloss.Width(right))
	return left + mid + strings.Repeat(" ", pad) + right
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
