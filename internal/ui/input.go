package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"tower/internal/agent"
	"tower/internal/tmux"
)

// inputBar is the composer docked along the bottom of a window. It writes
// to the agent in the same window unless I pick another with tab or @name.
// As a popup (tower compose) it is addressed to one agent and closes after
// sending.
type inputBar struct {
	c        composer
	popup    bool
	self     string
	agents   []agent.Agent
	target   string // pane id
	pinned   bool   // chosen with tab or by the popup, not by window
	w, h     int
	frame    int
	visible  bool
	spinning bool
	flash    string
	flashAt  time.Time
	refine   string // @tower_refine: review, auto or off
	refining bool
	rseq     int    // a rewrite that comes back after I moved on is dropped
	mine     string // my own words while the box holds a rewrite of them
	asTyped  bool   // enter sends the box as it is, without a rewrite
	rmodel   string // the model and start of the rewrite in flight, and
	rstart   time.Time
	rsend    bool // whether it sends once it is back
	gframe   int  // frame of its generating effect
}

type sentMsg struct {
	note string
	err  error
}

type refinedMsg struct {
	seq  int
	from string // the box when the rewrite started
	text string
	err  error
	send bool // auto mode: send it once it is back
}

// Input runs the input bar.
func Input() error { return runInput(&inputBar{self: os.Getenv("TMUX_PANE")}) }

// Compose runs the input box as a popup addressed to target.
func Compose(target string) error {
	a, err := agent.Resolve(target, agent.Load(true))
	if err != nil {
		return err
	}
	return runInput(&inputBar{popup: true, target: a.Pane, pinned: true})
}

func runInput(b *inputBar) error {
	b.c = newComposer("message the agent here (@name or @all to send elsewhere)")
	b.refine = refineMode()
	b.refresh()
	return run(b, tea.WithAltScreen(), tea.WithFPS(30))
}

func (b *inputBar) Init() tea.Cmd { return tea.Batch(tick(), b.startSpin(), b.c.ta.Focus()) }

func (b *inputBar) startSpin() tea.Cmd {
	a := b.find(b.target)
	if b.spinning || !b.visible || a == nil || a.Status != agent.Working {
		return nil
	}
	b.spinning = true
	return spin()
}

// refresh reloads agents and picks the target. It reports false when the
// bar's window has nothing but tower panes left.
func (b *inputBar) refresh() bool {
	defer func() { onScreen.Store(b.visible) }()
	b.visible = true
	if !b.popup {
		visible, orphan := placement(b.self)
		if orphan {
			return false
		}
		if b.visible = visible; !visible && b.agents != nil {
			return true
		}
	}
	b.agents = agent.Load(false)
	if b.pinned && b.find(b.target) != nil {
		return true
	}
	b.pinned, b.target = false, b.local()
	return true
}

// local is the agent in my window: the pane I came from if it is one,
// else the first agent there.
func (b *inputBar) local() string {
	var first string
	for _, l := range strings.Split(tmux.Run("list-panes", "-t", b.self, "-F", "#{pane_id} #{pane_last}"), "\n") {
		v := strings.Fields(l)
		if len(v) != 2 || b.find(v[0]) == nil {
			continue
		}
		if v[1] == "1" {
			return v[0]
		}
		if first == "" {
			first = v[0]
		}
	}
	return first
}

func (b *inputBar) find(pane string) *agent.Agent {
	for i := range b.agents {
		if b.agents[i].Pane == pane {
			return &b.agents[i]
		}
	}
	return nil
}

func (b *inputBar) cycle(d int) {
	if len(b.agents) == 0 {
		return
	}
	i := 0
	for j, a := range b.agents {
		if a.Pane == b.target {
			i = j
		}
	}
	b.target = b.agents[(i+d+len(b.agents))%len(b.agents)].Pane
	b.pinned = true
}

func (b *inputBar) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		b.w, b.h = msg.Width, msg.Height
		return b, nil
	case fsWakeMsg:
		if !b.visible {
			return b, nil
		}
		b.refresh()
		return b, b.startSpin()
	case tickMsg, wakeMsg:
		if !b.refresh() {
			return b, tea.Quit
		}
		cmd := b.startSpin()
		if _, ok := msg.(wakeMsg); ok {
			return b, cmd
		}
		next := time.Second
		if !b.visible {
			next = hiddenTick
		}
		return b, tea.Batch(cmd, tickIn(next))
	case spinMsg:
		if a := b.find(b.target); !b.visible || a == nil || a.Status != agent.Working {
			b.spinning = false
			return b, nil
		}
		b.frame++
		return b, spin()
	case sentMsg:
		if msg.err != nil {
			b.flash, b.flashAt = msg.err.Error(), time.Now()
			return b, nil
		}
		b.c.Reset()
		b.mine, b.asTyped = "", false
		if b.popup {
			return b, tea.Quit
		}
		b.flash, b.flashAt = msg.note, time.Now()
		return b, nil
	case refinedMsg:
		if msg.seq != b.rseq || !b.refining {
			return b, nil
		}
		b.refining = false
		if msg.err != nil {
			b.asTyped = true
			b.flash, b.flashAt = msg.err.Error()+" · enter sends it as typed", time.Now()
			return b, nil
		}
		if b.c.ta.Value() != msg.from {
			b.flash, b.flashAt = "the message changed during the rewrite · ctrl+o rewrites it again", time.Now()
			return b, nil
		}
		if b.mine == "" {
			b.mine = msg.from
		}
		b.asTyped = false
		b.c.ta.SetValue(msg.text)
		if msg.send {
			return b, b.send()
		}
		return b, nil
	case genTickMsg:
		if !b.refining || int(msg) != b.rseq {
			return b, nil
		}
		b.gframe++
		return b, genTick(b.rseq)
	case tea.KeyMsg:
		if b.refining && msg.String() != "ctrl+c" && msg.String() != "esc" {
			return b, nil // the box is drawn as the effect: typing would go in unseen
		}
		switch msg.String() {
		case "tab":
			b.cycle(1)
			return b, nil
		case "shift+tab":
			b.cycle(-1)
			return b, nil
		case "esc":
			if b.popup {
				return b, tea.Quit
			}
			tmux.Run("last-pane", "-t", b.self)
			return b, nil
		case "ctrl+c":
			if b.popup {
				return b, tea.Quit
			}
			b.c.Reset()
			b.refining, b.mine, b.asTyped = false, "", false
			return b, nil
		case "ctrl+o":
			if b.refining || b.c.Empty() {
				return b, nil
			}
			return b, b.rewrite(false)
		case "ctrl+z":
			if b.mine != "" {
				b.c.ta.SetValue(b.mine)
				b.mine, b.asTyped = "", true
			}
			return b, nil
		}
	}
	submit, cmd := b.c.Update(msg)
	if b.c.Empty() {
		b.mine, b.asTyped = "", false
	}
	if submit && !b.c.Empty() && !b.refining {
		b.refine = refineMode()
		if b.refine != "off" && b.mine == "" && !b.asTyped && agent.NeedsRefine(b.c.ta.Value()) {
			return b, b.rewrite(b.refine == "auto")
		}
		return b, b.send()
	}
	return b, cmd
}

// refineMode is @tower_refine: auto (enter rewrites and sends), review
// (enter rewrites the message into the box, enter again sends it) or off.
func refineMode() string {
	switch m := tmux.Option("@tower_refine", "auto"); m {
	case "review", "off":
		return m
	}
	return "auto"
}

// rewrite turns the message into a fuller prompt for the agent it goes to.
// Asked again, it starts over from my own words, not from the last rewrite.
func (b *inputBar) rewrite(send bool) tea.Cmd {
	from := b.c.ta.Value()
	src := or(b.mine, from)
	var def *agent.Agent
	if a := b.find(b.target); a != nil {
		c := *a
		def = &c
	}
	agents := append([]agent.Agent(nil), b.agents...)
	b.rseq++
	seq := b.rseq
	model := tmux.Option("@tower_refine_model", "haiku")
	b.refining, b.rmodel, b.rstart, b.rsend, b.gframe = true, model, time.Now(), send, 0
	return tea.Batch(func() tea.Msg {
		ctx := def
		if to, _, err := agent.Route(src, nil, agents); err == nil && len(to) == 1 {
			ctx = &to[0]
		}
		text, err := agent.Refine(src, ctx, model)
		return refinedMsg{seq: seq, from: from, text: text, err: err, send: send}
	}, genTick(seq))
}

func (b *inputBar) send() tea.Cmd {
	text, _ := b.c.Message(true)
	var def []agent.Agent
	if a := b.find(b.target); a != nil {
		def = []agent.Agent{*a}
	}
	agents := append([]agent.Agent(nil), b.agents...)
	return func() tea.Msg {
		to, body, err := agent.Route(text, def, agents)
		if err != nil {
			return sentMsg{err: err}
		}
		if len(to) == 0 {
			return sentMsg{err: errNoTarget}
		}
		var names []string
		for _, a := range to {
			if err := agent.Send(a, body, false); err != nil {
				return sentMsg{err: err}
			}
			names = append(names, a.Name)
			agent.LogMessage(agent.Message{Kind: "message", From: "me", To: a.Name, ToPane: a.Pane, Text: body})
		}
		return sentMsg{note: "sent to " + strings.Join(names, ", ")}
	}
}

type inputErr string

func (e inputErr) Error() string { return string(e) }

const errNoTarget = inputErr("no agent in this window: tab picks one")

func (b *inputBar) View() string {
	if b.w == 0 {
		return ""
	}
	var label string
	if names, ok := b.recipients(); ok {
		label = " → " + accent.Bold(true).Render(names) + " "
	} else if a := b.find(b.target); a != nil {
		label = " → " + agentIcon("claude", a.Status, b.frame) + " " + bold.Render(a.Name) + " " + tone[a.Status].Render(a.Status)
		if a.Activity != "" {
			label += " " + activity(dim, "· "+a.Activity, a.RetryAt, time.Now().Unix())
		}
		label += " "
	} else {
		label = dim.Render(" " + string(errNoTarget) + " ")
	}
	top := rule(b.w, label, dim.Render(" tab: next agent "))
	if b.refining {
		box := generatingBox(b.c.ta.Value(), b.w, max(1, b.h-2), b.gframe)
		status := generatingStatus(b.gframe, b.rstart, b.rmodel, b.rsend)
		return strings.Join([]string{top, box, fit(" "+status, b.w)}, "\n")
	}
	box := b.c.View(b.w, max(1, b.h-2))
	status := b.c.Status()
	if b.mine != "" {
		status = strings.TrimSpace(status + " " + accent.Render("✦ rewritten") + dim.Render(" · enter send · ctrl+z my words · ctrl+o again"))
	}
	if b.flash != "" && time.Since(b.flashAt) < 5*time.Second {
		status = strings.TrimSpace(status + " " + accent.Render(b.flash))
	}
	if status == "" {
		enter := map[string]string{"review": "enter rewrite, again to send", "auto": "enter rewrite and send"}[b.refine]
		if b.asTyped {
			enter = "enter send as typed"
		}
		status = dim.Render(or(enter, "enter send") + " · alt+enter new line · ctrl+o rewrite · ctrl+v paste image/video/files · ctrl+r voice · ↑ history · esc back")
	}
	return strings.Join([]string{top, box, fit(" "+status, b.w)}, "\n")
}

// recipients previews who the message goes to when it starts with
// @mentions. It only matches names, so drawing never calls tmux.
func (b *inputBar) recipients() (string, bool) {
	rest := strings.TrimSpace(b.c.ta.Value())
	if !strings.HasPrefix(rest, "@") {
		return "", false
	}
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for strings.HasPrefix(rest, "@") {
		word, after, _ := strings.Cut(rest, " ")
		name := word[1:]
		switch {
		case name == "all":
			for _, a := range b.agents {
				add(a.Name)
			}
		case b.named(name):
			add(name)
		case name != "":
			add(tone[agent.Waiting].Render(name + "?"))
		}
		rest = strings.TrimSpace(after)
	}
	if len(names) == 0 {
		return "", false
	}
	return strings.Join(names, ", ") + dim.Render(fmt.Sprintf("  (%s)", plural(len(names), "agent"))), true
}

func (b *inputBar) named(name string) bool {
	for _, a := range b.agents {
		if a.Name == name {
			return true
		}
	}
	return false
}

// rule is a horizontal line with labels at both ends.
func rule(w int, left, right string) string {
	fill := max(0, w-2-ansiWidth(left)-ansiWidth(right))
	return fit(dim.Render("─")+left+dim.Render(strings.Repeat("─", fill))+right+dim.Render("─"), w)
}

// placement reports whether a pane's window is on screen, and whether only
// tower panes (sidebar, input bar) are left in it.
func placement(pane string) (visible, orphan bool) {
	w := where(pane)
	return w.visible, w.orphan
}

// spot is where a docked tower pane sits.
type spot struct {
	visible bool   // its window is on screen
	orphan  bool   // only tower panes are left in the window
	focused bool   // the cursor is in it
	local   string // the window's working pane: the focused one, else the last focused
}

func where(pane string) spot {
	s := spot{orphan: pane != ""}
	var last, first string
	for _, l := range strings.Split(tmux.Run("list-panes", "-t", pane, "-F",
		"#{pane_id} #{pane_active} #{pane_last} #{&&:#{window_active},#{session_attached}} #{@tower_sidebar}#{@tower_input}"), "\n") {
		v := strings.Fields(l)
		if len(v) < 4 {
			continue
		}
		s.visible = v[3] == "1"
		if v[0] == pane {
			s.focused = v[1] == "1"
		}
		if len(v) > 4 { // a tower pane
			continue
		}
		s.orphan = false
		switch {
		case v[1] == "1":
			s.local = v[0]
		case v[2] == "1":
			last = v[0]
		case first == "":
			first = v[0]
		}
	}
	if s.local == "" {
		s.local = or(last, first)
	}
	return s
}

func or(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
