package ui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"tower/internal/agent"
	"tower/internal/thread"
	"tower/internal/tmux"
)

// ANSI colors 1-8 so tower follows whatever terminal theme is active.
var (
	accent = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	bold   = lipgloss.NewStyle().Bold(true)
	plain  = lipgloss.NewStyle()
	claude = lipgloss.Color("#D97757") // Claude's orange
	link   = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	badge  = lipgloss.NewStyle().Background(lipgloss.Color("4")).Foreground(lipgloss.Color("0")).Bold(true)
	tone   = map[string]lipgloss.Style{
		agent.Working: lipgloss.NewStyle().Foreground(claude),
		agent.Waiting: lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		agent.Done:    lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		agent.Idle:    dim,
	}
)

// wideMin is the width from which the dashboard shows the detail panel.
const wideMin = 90

func (m *model) View() string {
	if m.w == 0 || m.h == 0 {
		return ""
	}
	switch {
	case m.help:
		return m.helpView()
	case m.grid && !m.sidebar:
		return m.gridView()
	case m.sidebar || m.w < wideMin:
		return m.narrow()
	}
	return m.wide()
}

var (
	spinner    = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"} // tool calls
	claudeSpin = []string{"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"} // what Claude Code shows while it works
	codexSpin  = []string{"◐", "◓", "◑", "◒"}
	glyph      = map[string]string{"claude": "✻", "codex": "⬡"}
)

// agentIcon is an agent's engine mark in its state's colour: ✻ for Claude
// Code (cycling like Claude Code's own spinner while it works), ⬡ for Codex.
func agentIcon(engine, status string, frame int) string {
	g := glyph[engine]
	if g == "" {
		g = "◇"
	}
	switch status {
	case agent.Working:
		frames := claudeSpin
		if engine == "codex" {
			frames = codexSpin
		}
		return tone[agent.Working].Render(frames[frame%len(frames)])
	case agent.Waiting:
		return tone[agent.Waiting].Bold(true).Render(g)
	case agent.Done:
		return tone[agent.Done].Render(g)
	}
	return dim.Render(g)
}

// icon is a tmux agent's mark; those are always Claude Code.
func (m *model) icon(status string) string { return agentIcon("claude", status, m.frame) }

func ansiWidth(s string) int { return ansi.StringWidth(s) }

func (m *model) narrow() string {
	head := m.header(m.w)
	foot := m.footer(m.w)
	var log []string
	switch {
	case m.showComms:
		log = m.commsPanel(m.w, min(14, m.h/3), true)
	case m.showLog:
		log = m.logPanel(m.w, min(12, m.h/3))
	}
	m.listW = m.w
	listH := max(0, m.h-len(head)-len(foot)-len(log))
	m.logY = len(head) + listH
	lines := append(head, m.list(m.w, listH, len(head))...)
	lines = append(lines, log...)
	lines = append(lines, foot...)
	return strings.Join(lines, "\n")
}

func (m *model) wide() string {
	lw := clamp(m.w/3, 34, 46)
	head := m.header(m.w)
	foot := m.footer(m.w)
	var panel []string
	if m.h >= 20 { // too short and the chat would have no room
		panel = m.statusPanel(m.w)
	}
	bodyH := max(0, m.h-len(head)-len(panel)-len(foot))
	m.listW = lw
	left := m.list(lw, bodyH, len(head))
	right := m.detailPanel(m.w-lw-3, bodyH)
	if m.showComms {
		right = m.commsPanel(m.w-lw-3, bodyH, false)
	}
	sep := dim.Render(" │ ")
	lines := head
	for i := range bodyH {
		lines = append(lines, left[i]+sep+right[i])
	}
	lines = append(lines, panel...)
	lines = append(lines, foot...)
	return strings.Join(lines, "\n")
}

func (m *model) header(w int) []string {
	n := map[string]int{}
	out := 0
	for _, a := range m.agents {
		n[a.Status]++
		if u := m.usage[a.Transcript]; u != nil {
			out += u.Out
		}
	}
	for _, t := range m.threads {
		n[t.Status]++
		out += t.Usage.Output
	}
	cost := 0.0
	for _, t := range m.threads {
		cost += t.Usage.CostUSD
	}
	total := len(m.agents) + len(m.threads)
	var headline string
	switch {
	case n[agent.Waiting] > 0:
		headline = tone[agent.Waiting].Bold(true).Render(fmt.Sprintf("◐ %d need you", n[agent.Waiting]))
	case n[agent.Working] > 0:
		headline = tone[agent.Working].Render(fmt.Sprintf("%s %d working", claudeSpin[m.frame%len(claudeSpin)], n[agent.Working]))
	case n[agent.Done] > 0:
		headline = tone[agent.Done].Render(fmt.Sprintf("✓ %d done", n[agent.Done]))
	case total > 0:
		headline = dim.Render("all idle")
	}
	var counts []string
	for _, s := range []string{agent.Working, agent.Waiting, agent.Done, agent.Idle} {
		if n[s] > 0 {
			counts = append(counts, tone[s].Render(fmt.Sprintf("%s %d", map[string]string{
				agent.Working: "✻", agent.Waiting: "◐", agent.Done: "✓", agent.Idle: "○"}[s], n[s])))
		}
	}
	stats := " " + strings.Join(counts, "  ")
	totals := fmt.Sprintf(" %s out", tokens(out))
	if cost > 0 {
		totals += fmt.Sprintf(" · $%.2f", cost)
	}
	stats += dim.Render(totals + " ")
	return []string{
		fit(row(w, " "+badge.Render(" ✻ tower ")+" "+dim.Render(plural(total, "agent")), headline+" "), w),
		fit(stats+dim.Render(strings.Repeat("─", max(0, w-ansi.StringWidth(stats)))), w),
	}
}

// list draws the session and agent rows, scrolled so the selection is
// visible, and records which screen row belongs to which item.
func (m *model) list(w, height, y0 int) []string {
	type line struct {
		text string
		item int
	}
	var ls []line
	now := time.Now().Unix()
	for i, it := range m.items {
		mark := " "
		if i == m.cur {
			mark = accent.Render("▌")
		}
		if it.header {
			ls = append(ls, line{row(w, mark+bold.Render("threads"), dim.Render("headless ")), i})
			continue
		}
		num := dim.Render(fmt.Sprint(m.number(i)))
		if t := it.thread; t != nil {
			name := t.Name
			if i == m.cur || t.Status == agent.Waiting || t.Status == agent.Done {
				name = bold.Render(name)
			}
			ls = append(ls, line{row(w, mark+num+" "+agentIcon(t.Engine, t.Status, m.frame)+" "+name, threadTimer(t, now)+" "), i})
			act := dim
			if t.Status == agent.Waiting {
				act = tone[agent.Waiting]
			}
			ls = append(ls, line{mark + "   " + activity(act, t.Activity, t.RetryAt, now), i})
			if m.compact {
				continue
			}
			if t.Prompt != "" {
				ls = append(ls, line{mark + dim.Render("   › "+t.Prompt), i})
			}
			ls = append(ls, line{mark + dim.Render("   "+threadMeta(t)), i})
			continue
		}
		if it.agent == nil {
			if len(ls) > 0 {
				ls = append(ls, line{"", -1})
			}
			name := bold.Render(it.session.Name)
			if it.session.Attached {
				name = accent.Bold(true).Render(it.session.Name)
			}
			ls = append(ls, line{row(w, mark+name, dim.Render(it.session.Windows+"w ")), i})
			continue
		}
		a := it.agent
		name := a.Name
		if i == m.cur || a.Status == agent.Waiting || a.Status == agent.Done {
			name = bold.Render(name)
		}
		ls = append(ls, line{row(w, mark+num+" "+m.icon(a.Status)+" "+name, timer(a, now)+" "), i})
		act := dim
		if a.Status == agent.Waiting {
			act = tone[agent.Waiting]
		}
		ls = append(ls, line{mark + "   " + activity(act, a.Activity, a.RetryAt, now), i})
		if l := m.links[a.Pane]; l != "" {
			ls = append(ls, line{mark + "   " + link.Render(l), i})
		}
		if m.compact {
			continue
		}
		if a.Prompt != "" {
			ls = append(ls, line{mark + dim.Render("   › "+a.Prompt), i})
		}
		meta := "   " + a.Win.WindowIndex + "." + a.Win.Index
		if u := m.usage[a.Transcript]; u != nil && u.Ctx > 0 {
			meta += " · " + tokens(u.Ctx) + " ctx · " + tokens(u.Out) + " out"
		}
		ls = append(ls, line{mark + dim.Render(meta), i})
	}
	if len(ls) == 0 {
		ls = append(ls, line{dim.Render(" no tmux sessions"), -1})
	}

	first, last := -1, -1
	for j, l := range ls {
		if l.item == m.cur {
			if first < 0 {
				first = j
			}
			last = j
		}
	}
	if first >= 0 {
		if first < m.top {
			m.top = first
		}
		if last >= m.top+height {
			m.top = last - height + 1
		}
	}
	m.top = clamp(m.top, 0, max(0, len(ls)-height))

	m.rows = map[int]int{}
	var out []string
	for j := m.top; j < len(ls) && len(out) < height; j++ {
		if ls[j].item >= 0 {
			m.rows[y0+len(out)] = ls[j].item
		}
		out = append(out, ls[j].text)
	}
	return pad(out, w, height)
}

// activity is an agent's current-activity line; while a retry is due it is
// orange with a countdown.
func activity(st lipgloss.Style, text string, retryAt, now int64) string {
	if retryAt > now {
		return tone[agent.Working].Render(text + " · in " + ago(retryAt-now))
	}
	return st.Render(text)
}

// number is the 1-based number of an agent or thread row, 0 for others.
func (m *model) number(item int) int {
	for n, i := range m.byNum {
		if i == item {
			return n + 1
		}
	}
	return 0
}

func threadTimer(t *thread.Thread, now int64) string {
	switch {
	case t.Status == agent.Working && t.TurnStarted > 0:
		return tone[agent.Working].Render(ago(now - t.TurnStarted))
	case t.Status == agent.Waiting:
		return tone[agent.Waiting].Render(ago(now - t.Updated))
	case t.Status == agent.Done || t.Status == agent.Idle:
		return dim.Render(ago(now - t.Updated))
	}
	return ""
}

// threadMeta is the engine, and once known the context size and cost.
func threadMeta(t *thread.Thread) string {
	parts := []string{t.Engine}
	if !t.Live {
		parts[0] += " (stopped)"
	}
	if t.Usage.Context > 0 {
		parts = append(parts, tokens(t.Usage.Context)+" ctx")
	}
	if t.Usage.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", t.Usage.CostUSD))
	}
	return strings.Join(parts, " · ")
}

func timer(a *agent.Agent, now int64) string {
	switch {
	case a.Status == agent.Working && a.TurnStarted > 0:
		return tone[agent.Working].Render(ago(now - a.TurnStarted))
	case a.Status == agent.Waiting:
		return tone[agent.Waiting].Render(ago(now - a.Updated))
	case a.Finished > 0:
		return dim.Render(ago(now - a.Finished))
	}
	return ""
}

func (m *model) detailPanel(w, h int) []string {
	it, ok := m.sel()
	if !ok {
		return pad(nil, w, h)
	}
	switch {
	case it.thread != nil:
		return m.threadPanel(it.thread, w, h)
	case it.header:
		return pad([]string{
			bold.Render("headless threads"), "",
			"Agents tower runs itself, with no terminal of their own: Claude Code or Codex",
			"talking to tower directly, so every tool call, approval and reply shows up here.",
			"", accent.Render("N") + " starts one   " + accent.Render("i") + " types to the selected one",
		}, w, h)
	case it.agent == nil:
		return pad(m.sessionPanel(it.session, h), w, h)
	}
	a := it.agent
	info := []string{a.Where(), home(a.Cwd)}
	if u := m.usage[a.Transcript]; u != nil && u.Model != "" {
		info = append(info, strings.TrimPrefix(u.Model, "claude-"))
	}
	lines := []string{
		m.icon(a.Status) + " " + bold.Render(a.Name) + "  " + dim.Render(strings.Join(info, " · ")),
		m.tabs(),
		"",
	}
	bodyH := max(0, h-len(lines))
	var body []string
	switch m.view {
	case viewScreen:
		body = m.window(trimBlank(strings.Split(m.screen, "\n")), bodyH, true)
	case viewLog:
		body = m.window(logLines(agent.Tail(a.SessionID, 500), false), bodyH, true)
	case viewDiff:
		body = m.window(strings.Split(m.cached(viewDiff, a.Pane), "\n"), bodyH, false)
	case viewLive:
		var lines []string
		if f := m.feeds[a.Transcript]; f != nil {
			lines = renderChat(f.chat.Items, w, m.frame)
		}
		if len(lines) == 0 {
			lines = []string{dim.Render("nothing in its transcript yet")}
		}
		body = m.window(lines, bodyH, true)
	}
	return pad(append(lines, body...), w, h)
}

// commsPanel is the timeline of hand-offs between agents, newest last,
// with tasks still being worked on marked.
func (m *model) commsPanel(w, h int, short bool) []string {
	open := map[string]bool{}
	for _, t := range agent.OpenTasks(m.comms) {
		open[t.ID] = true
	}
	format := "15:04:05"
	if short {
		format = "15:04"
	}
	var lines []string
	for _, c := range m.comms {
		kind := map[string]lipgloss.Style{"task": link, "reply": green, "forward": accent}[c.Kind]
		if c.Kind == "message" || c.Failed {
			kind = dim
		}
		if c.Failed {
			kind = red
		}
		state := ""
		if open[c.ID] {
			state = " " + tone[agent.Working].Render(claudeSpin[m.frame%len(claudeSpin)]+" working")
		}
		head := " " + dim.Render(time.Unix(c.Time, 0).Format(format)) + " " + bold.Render(c.From) + dim.Render(" ─▶ ") + bold.Render(c.To) + " " + kind.Render(c.Kind) + state
		lines = append(lines, head)
		text := strings.Join(strings.Fields(c.Text), " ")
		if short {
			lines = append(lines, "   "+dim.Render(text))
			continue
		}
		for _, l := range strings.Split(lipgloss.NewStyle().Width(max(10, w-4)).Render(text), "\n") {
			lines = append(lines, "   "+dim.Render(l))
		}
	}
	if len(lines) == 0 {
		lines = []string{dim.Render(" no hand-offs yet: D assigns a task from the selected agent to another")}
	}
	title := " agents talking (" + plural(len(open), "open task") + ") "
	head := dim.Render("─" + title + strings.Repeat("─", max(0, w-ansi.StringWidth(title)-1)))
	room := max(0, h-1)
	if len(lines) > room {
		lines = lines[len(lines)-room:]
	}
	return pad(append([]string{head}, lines...), w, h)
}

// threadPanel is a headless thread: its chat (or git diff) with the
// composer docked underneath.
func (m *model) threadPanel(t *thread.Thread, w, h int) []string {
	info := []string{t.Engine}
	if t.Model != "" {
		info = append(info, t.Model)
	}
	info = append(info, home(t.Cwd))
	if t.Branch != "" {
		info = append(info, t.Branch)
	}
	if t.Mode != "" && t.Mode != "default" {
		info = append(info, t.Mode)
	}
	if note := strings.TrimPrefix(usageNote(&t.Usage), " · "); note != "" {
		info = append(info, note)
	}
	on, off := accent.Bold(true).Underline(true), dim
	tabs := on.Render(" chat ") + off.Render(" diff ") + dim.Render("  tab switches")
	if m.view == viewDiff {
		tabs = off.Render(" chat ") + on.Render(" diff ") + dim.Render("  tab switches")
	}
	head := []string{agentIcon(t.Engine, t.Status, m.frame) + " " + bold.Render(t.Name) + "  " + dim.Render(strings.Join(info, " · ")), tabs}

	compH := 3
	label := dim.Render(" i to type ")
	if m.focusChat {
		label = accent.Render(" enter send · alt+enter new line · ctrl+v image/video · ctrl+r voice · esc list ")
	}
	comp := []string{rule(w, label, "")}
	comp = append(comp, strings.Split(m.comp.View(w, compH), "\n")...)
	if st := m.comp.Status(); st != "" {
		comp = append(comp, fit(" "+st, w))
	}

	bodyH := max(0, h-len(head)-len(comp))
	var body []string
	if m.view == viewDiff {
		body = m.window(strings.Split(m.cached(viewDiff, "t"+t.ID), "\n"), bodyH, false)
	} else {
		var lines []string
		if c := m.chats[t.ID]; c != nil {
			lines = renderChat(c.Items, w, m.frame)
		}
		if len(lines) == 0 {
			lines = []string{dim.Render("no messages yet: press i and say what to do")}
		}
		body = m.window(lines, bodyH, true)
	}
	return pad(append(append(head, pad(body, w, bodyH)...), comp...), w, h)
}

// window picks the visible slice of a detail view, anchored at the bottom or
// the top until I scroll.
func (m *model) window(lines []string, h int, bottom bool) []string {
	last := max(0, len(lines)-h)
	top := m.dtop
	if top < 0 {
		top = 0
		if bottom {
			top = last
		}
	}
	top = clamp(top, 0, last)
	m.dcur, m.dlen, m.dh = top, len(lines), h
	return lines[top:min(len(lines), top+h)]
}

func (m *model) cached(v view, pane string) string {
	s, ok := m.detail[detailKey(v, pane)]
	switch {
	case !ok:
		return dim.Render("loading")
	case s == "":
		return dim.Render("(nothing yet)")
	}
	return s
}

func (m *model) tabs() string {
	var b strings.Builder
	for i, n := range viewNames {
		label := " " + n + " "
		if view(i) == m.view {
			b.WriteString(accent.Bold(true).Underline(true).Render(label))
		} else {
			b.WriteString(dim.Render(label))
		}
	}
	return b.String()
}

// sessionPanel shows a session's windows and the merged activity of its agents.
func (m *model) sessionPanel(s tmux.Session, h int) []string {
	lines := []string{bold.Render(s.Name) + "  " + dim.Render(s.Windows+" windows"), ""}
	for _, l := range strings.Split(tmux.Run("list-windows", "-t", "="+s.Name, "-F", "#{window_index}\t#{window_name}\t#{window_panes}\t#{?window_active, *,}"), "\n") {
		v := strings.Split(l, "\t")
		if len(v) == 4 {
			lines = append(lines, " "+accent.Render(v[0])+" "+v[1]+v[3]+" "+dim.Render(plural(atoi(v[2]), "pane")))
		}
	}
	var feed []agent.LogLine
	for _, a := range m.agents {
		if a.Win.Session != s.Name {
			continue
		}
		for _, l := range agent.Tail(a.SessionID, h) {
			l.Agent = a.Name
			feed = append(feed, l)
		}
	}
	if len(feed) == 0 {
		return lines
	}
	sort.SliceStable(feed, func(i, j int) bool { return feed[i].Time.Before(feed[j].Time) })
	room := max(0, h-len(lines)-2)
	if len(feed) > room {
		feed = feed[len(feed)-room:]
	}
	lines = append(lines, "", dim.Render("recent activity"))
	return append(lines, logLines(feed, false)...)
}

func (m *model) logPanel(w, h int) []string {
	title := " log "
	body := []string{dim.Render(" select an agent")}
	if a := m.selAgent(); a != nil {
		all := logLines(agent.Tail(a.SessionID, 500), true)
		m.logOff = clamp(m.logOff, 0, max(0, len(all)-(h-1)))
		end := len(all) - m.logOff
		body = all[max(0, end-(h-1)):end]
		title = " log · " + a.Name + " "
		if m.logOff > 0 {
			title += fmt.Sprintf("(%d newer below) ", m.logOff)
		}
	}
	rule := dim.Render("─" + title + strings.Repeat("─", max(0, w-ansi.StringWidth(title)-1)))
	return pad(append([]string{rule}, body...), w, h)
}

func logLines(ls []agent.LogLine, short bool) []string {
	format := "15:04:05"
	if short {
		format = "15:04"
	}
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		st := plain
		switch {
		case strings.HasPrefix(l.Text, "›"):
			st = accent
		case strings.HasPrefix(l.Text, "!"), strings.HasPrefix(l.Text, "✗"):
			st = tone[agent.Waiting]
		case strings.HasPrefix(l.Text, "✓"):
			st = tone[agent.Done]
		}
		who := ""
		if l.Agent != "" {
			who = bold.Render(l.Agent) + " "
		}
		out = append(out, " "+dim.Render(l.Time.Format(format))+" "+who+st.Render(l.Text))
	}
	return out
}

func (m *model) footer(w int) []string {
	narrow := m.sidebar || m.w < wideMin
	if m.mode != modeList {
		m.input.Width = max(1, w-ansi.StringWidth(m.input.Prompt)-3)
		lines := []string{fit(" "+m.input.View(), w)}
		if narrow {
			lines = append(lines, fit(dim.Render(" enter ok · esc cancel"), w))
		}
		return lines
	}
	flash := ""
	if m.flash != "" && time.Since(m.flashAt) < 5*time.Second {
		flash = accent.Render(" " + m.flash)
	}
	if narrow {
		l1 := dim.Render(" click/⏎ go  1-9 jump  s send  y ok")
		if flash != "" {
			l1 = flash
		}
		return []string{fit(l1, w), fit(dim.Render(" v grid  o board  l log  m comms  ? keys"), w)}
	}
	l := dim.Render(" click/⏎ go  1-9 jump  v grid  tab view  s send  D assign  m comms  f forward  b all  n new  N headless  y approve  x esc  ? keys  q quit")
	if m.selThread() != nil {
		l = dim.Render(" i type  y allow  A always  d deny  x stop  X remove  r rename  tab chat/diff  1-9 jump  N new headless  v grid  ? keys  q quit")
	}
	if m.grid {
		l = dim.Render(" ⏎ jump  hjkl move  v list  s send  f forward  b all  n new  y approve  x esc  a next  ? keys  q quit")
	}
	if flash != "" {
		l = flash
	}
	return []string{fit(l, w)}
}

// row lays out left and right on one line of width w, cutting left to fit.
func row(w int, left, right string) string {
	rw := ansi.StringWidth(right)
	left = ansi.Truncate(left, max(0, w-rw-1), "…")
	return left + strings.Repeat(" ", max(0, w-ansi.StringWidth(left)-rw)) + right
}

// fit cuts or pads s to exactly w cells and resets any color it leaves open.
func fit(s string, w int) string {
	s = ansi.Truncate(s, w, "")
	return s + "\x1b[0m" + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}

func pad(lines []string, w, h int) []string {
	out := make([]string, h)
	for i := range out {
		if i < len(lines) {
			out[i] = fit(lines[i], w)
		} else {
			out[i] = strings.Repeat(" ", w)
		}
	}
	return out
}

func trimBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func ago(sec int64) string {
	sec = max(0, sec)
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	case sec < 86400:
		return fmt.Sprintf("%dh%02dm", sec/3600, sec%3600/60)
	}
	return fmt.Sprintf("%dd", sec/86400)
}

func tokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1e6)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func home(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		return "~" + p[len(h):]
	}
	return p
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

func atoi(s string) int {
	var n int
	fmt.Sscan(s, &n)
	return n
}

// gridCols is how many agent tiles fit side by side.
func (m *model) gridCols() int { return clamp(m.w/64, 1, max(1, len(m.agents)+len(m.threads))) }

// gridView shows every agent at once: a tile per agent with its state,
// what it is doing, and the live tail of its screen.
func (m *model) gridView() string {
	head := m.header(m.w)
	foot := m.footer(m.w)
	bodyH := max(0, m.h-len(head)-len(foot))
	m.tiles = m.tiles[:0]
	var tiles []int // item indexes of agents and threads, in list order
	for i, it := range m.items {
		if it.agent != nil || it.thread != nil {
			tiles = append(tiles, i)
		}
	}
	if len(tiles) == 0 {
		body := pad([]string{"", dim.Render("  no agents yet: start one with n")}, m.w, bodyH)
		return strings.Join(append(append(head, body...), foot...), "\n")
	}
	cols := m.gridCols()
	rowsN := (len(tiles) + cols - 1) / cols
	tileH := max(7, bodyH/rowsN)
	visible := max(1, bodyH/tileH) // tile rows that fit; scroll to the selection
	selRow := 0
	for k, i := range tiles {
		if i == m.cur {
			selRow = k / cols
		}
	}
	first := clamp(selRow-visible+1, 0, max(0, rowsN-visible))
	var body []string
	for r := first; r < rowsN && r < first+visible; r++ {
		var row []string
		for c := 0; c < cols; c++ {
			k := r*cols + c
			w := m.w / cols
			if c == cols-1 {
				w = m.w - (cols-1)*(m.w/cols)
			}
			if k >= len(tiles) {
				row = append(row, strings.Join(pad(nil, w, tileH), "\n"))
				continue
			}
			x0, y0 := c*(m.w/cols), len(head)+len(body)
			m.tiles = append(m.tiles, rect{x0, y0, x0 + w, y0 + tileH, tiles[k]})
			if t := m.items[tiles[k]].thread; t != nil {
				row = append(row, m.threadTile(t, w, tileH, tiles[k] == m.cur))
			} else {
				row = append(row, m.tile(m.items[tiles[k]].agent, w, tileH, tiles[k] == m.cur))
			}
		}
		body = append(body, strings.Split(lipgloss.JoinHorizontal(lipgloss.Top, row...), "\n")...)
	}
	return strings.Join(append(append(head, pad(body, m.w, bodyH)...), foot...), "\n")
}

func (m *model) tile(a *agent.Agent, w, h int, sel bool) string {
	inner, lines := max(1, w-2), max(1, h-2)
	border := dim.GetForeground()
	switch {
	case sel:
		border = accent.GetForeground()
	case a.Status == agent.Waiting:
		border = tone[agent.Waiting].GetForeground()
	}
	act := dim
	if a.Status == agent.Waiting {
		act = tone[agent.Waiting]
	}
	meta := a.Where()
	if u := m.usage[a.Transcript]; u != nil && u.Ctx > 0 {
		meta += " · " + tokens(u.Ctx) + " ctx"
	}
	body := []string{
		row(inner, " "+m.icon(a.Status)+" "+bold.Render(a.Name)+" "+dim.Render(meta), timer(a, time.Now().Unix())+" "),
		" " + act.Render(a.Activity),
	}
	var screen []string
	if f := m.feeds[a.Transcript]; f != nil && len(f.chat.Items) > 0 {
		screen = renderChat(f.chat.Items[max(0, len(f.chat.Items)-60):], inner-1, m.frame)
	} else {
		screen = trimBlank(claudeBody(strings.Split(m.screens[a.Pane], "\n")))
	}
	if room := lines - len(body); len(screen) > room {
		screen = screen[len(screen)-room:]
	}
	for _, l := range screen {
		body = append(body, " "+l)
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Render(strings.Join(pad(body, inner, lines), "\n"))
}

func (m *model) threadTile(t *thread.Thread, w, h int, sel bool) string {
	inner, lines := max(1, w-2), max(1, h-2)
	border := dim.GetForeground()
	switch {
	case sel:
		border = accent.GetForeground()
	case t.Status == agent.Waiting:
		border = tone[agent.Waiting].GetForeground()
	}
	act := dim
	if t.Status == agent.Waiting {
		act = tone[agent.Waiting]
	}
	body := []string{
		row(inner, " "+agentIcon(t.Engine, t.Status, m.frame)+" "+bold.Render(t.Name)+" "+dim.Render(threadMeta(t)), threadTimer(t, time.Now().Unix())+" "),
		" " + act.Render(t.Activity),
	}
	if c := m.loadChat(t.ID); c != nil {
		chat := renderChat(c.Items, inner-1, m.frame)
		if room := lines - len(body); len(chat) > room {
			chat = chat[len(chat)-room:]
		}
		for _, l := range chat {
			body = append(body, " "+l)
		}
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Render(strings.Join(pad(body, inner, lines), "\n"))
}

// claudeBody drops Claude Code's input box and status line from the bottom
// of a captured screen, leaving the conversation.
func claudeBody(lines []string) []string {
	for i := len(lines) - 1; i > 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(ansi.Strip(lines[i])), "❯") && isRule(lines[i-1]) {
			return lines[:i-1]
		}
	}
	return lines
}

func isRule(s string) bool {
	s = strings.TrimSpace(ansi.Strip(s))
	return s != "" && strings.Trim(s, "─") == ""
}

var keyHelp = [][2]string{
	{"j k ↑ ↓", "move (h l too on the grid)"},
	{"click, enter", "go to the agent (a thread opens its chat)"},
	{"1-9", "go to agent number 1-9; prefix j then 1-9 from anywhere"},
	{"a", "next agent waiting on me or done"},
	{"v", "grid of every agent / back to the list"},
	{"s", "send a message; start it with @name or @all to redirect"},
	{"f", "forward this agent's last reply to other agents"},
	{"D", "assign a task from this agent to another (@agent task); the answer comes back to it"},
	{"m", "agents talking: every hand-off, with tasks still being worked on"},
	{"b", "send to every agent"},
	{"ctrl+v", "while typing: attach the clipboard image"},
	{"y", "approve the prompt the agent is waiting on"},
	{"x / X", "interrupt (Esc) / close the pane"},
	{"n", "new agent in a tmux window: name, task, optional git worktree"},
	{"N", "new headless agent (Claude Code or Codex) that tower runs itself"},
	{"i", "headless thread: type to it (esc back to the list)"},
	{"y / A / d", "headless thread: allow / allow for the session / deny"},
	{"r", "rename"},
	{"tab", "dashboard view: screen, log, diff, reply (chat, diff for threads)"},
	{"pgup pgdn", "scroll the view; in the sidebar, the log panel (wheel too)"},
	{"l / c", "sidebar log panel / compact rows"},
	{"o", "sidebar: open the dashboard"},
	{"esc", "sidebar: back to my pane; dashboard: close"},
	{"q", "sidebar: hide everywhere; dashboard: close"},
}

func (m *model) helpView() string {
	lines := []string{accent.Bold(true).Render(" tower keys"), ""}
	for _, k := range keyHelp {
		lines = append(lines, " "+accent.Render(fmt.Sprintf("%-13s", k[0]))+k[1])
	}
	lines = append(lines, "", dim.Render(" any key to close"))
	return strings.Join(pad(lines, m.w, m.h), "\n")
}
