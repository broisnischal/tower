package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"tower/internal/agent"
)

// The status panel is docked along the bottom of the dashboard, under the
// list and the chat: who needs me, what is running, the selected agent's
// tokens, and the plan's usage limits with the fleet totals. It keeps the
// same height whatever it holds, so the chat above never jumps.

const statusH = 5 // a rule and four rows

// loadStatus reads what the panel needs from disk. It runs on refresh, not
// on every frame.
func (m *model) loadStatus() {
	m.limits, m.hasLimits = agent.ReadLimits()
	m.live = map[string]agent.Live{}
	for _, a := range m.agents {
		if l, ok := agent.ReadLive(a.SessionID); ok {
			m.live[a.SessionID] = l
		}
	}
}

func (m *model) statusPanel(w int) []string {
	now := time.Now().Unix()
	row := func(icon, body string) string { return fit(" "+icon+" "+body, w) }
	return []string{
		rule(w, dim.Render(" status "), ""),
		row(m.needsYou(now)),
		row(m.running(now)),
		row(m.tokenRow()),
		row(m.limitsRow(now)),
	}
}

// needsYou names every agent waiting on me and what for, in red.
func (m *model) needsYou(now int64) (string, string) {
	var who []string
	for _, a := range m.agents {
		if a.Status == agent.Waiting {
			who = append(who, bold.Render(a.Name)+": "+a.Activity+" "+dim.Render(ago(now-a.Updated)))
		}
	}
	for _, t := range m.threads {
		if t.Status == agent.Waiting {
			text := t.Activity
			if t.Pending > 1 {
				text += fmt.Sprintf(" (+%d more)", t.Pending-1)
			}
			who = append(who, bold.Render(t.Name)+": "+text)
		}
	}
	red := tone[agent.Waiting]
	if len(who) == 0 {
		return dim.Render("◐"), dim.Render("nobody needs me")
	}
	verb := " need me"
	if len(who) == 1 {
		verb = " needs me"
	}
	return red.Bold(true).Render("◐"), red.Bold(true).Render(plural(len(who), "agent")+verb) + "  " +
		red.Render(strings.Join(who, red.Render(" │ ")))
}

// running is each working agent, what it is doing and for how long. Each
// gets an equal share of the row, so one long command can't push the rest
// off the edge.
func (m *model) running(now int64) (string, string) {
	type entry struct {
		name, act, timer string
		retryAt          int64
	}
	var es []entry
	for _, a := range m.agents {
		if a.Status == agent.Working {
			es = append(es, entry{a.Name, a.Activity, timer(&a, now), a.RetryAt})
		}
	}
	for _, t := range m.threads {
		if t.Status == agent.Working {
			es = append(es, entry{t.Name, t.Activity, threadTimer(&t, now), t.RetryAt})
		}
	}
	var parts []string
	for _, e := range es {
		room := max(12, (m.w-4)/max(1, len(es))-ansi.StringWidth(e.name)-ansi.StringWidth(e.timer)-7)
		parts = append(parts, bold.Render(e.name)+" "+activity(dim, ansi.Truncate(e.act, room, "…"), e.retryAt, now)+" "+e.timer)
	}
	if len(parts) == 0 {
		return dim.Render("✻"), dim.Render("nothing running")
	}
	return tone[agent.Working].Render(claudeSpin[m.frame%len(claudeSpin)]), strings.Join(parts, dim.Render("  │  "))
}

// tokenRow is the selected agent's tokens. A tmux agent's context and output
// come from its transcript; the share of the window and the cost come from
// its status line, when that has run. A headless thread's figures are as of
// its last finished turn.
func (m *model) tokenRow() (string, string) {
	icon := accent.Render("▤")
	if t := m.selThread(); t != nil {
		parts := []string{bold.Render(t.Name)}
		if t.Usage.Context > 0 {
			parts = append(parts, tokens(t.Usage.Context)+" ctx")
		}
		parts = append(parts, tokens(t.Usage.Output)+" out")
		if t.Usage.CostUSD > 0 {
			parts = append(parts, fmt.Sprintf("$%.2f", t.Usage.CostUSD))
		}
		if t.Model != "" {
			parts = append(parts, dim.Render(t.Model))
		}
		return icon, strings.Join(parts, " · ") + dim.Render("  (as of its last turn)")
	}
	a := m.selAgent()
	if a == nil {
		return dim.Render("▤"), dim.Render("select an agent for its tokens")
	}
	parts := []string{bold.Render(a.Name)}
	u := m.usage[a.Transcript]
	l, live := m.live[a.SessionID]
	ctx := ""
	if u != nil && u.Ctx > 0 {
		ctx = tokens(u.Ctx) + " ctx"
	}
	if live && l.CtxPct >= 0 && l.CtxSize > 0 {
		ctx = strings.TrimSpace(ctx + fmt.Sprintf(" (%.0f%% of %s)", l.CtxPct, tokens(l.CtxSize)))
	}
	if ctx != "" {
		parts = append(parts, ctx)
	}
	if u != nil {
		parts = append(parts, tokens(u.Out)+" out")
	}
	model := ""
	if u != nil {
		model = u.Model
	}
	if live {
		parts = append(parts, fmt.Sprintf("$%.2f", l.Cost))
		model = or(l.Model, model)
	}
	if model != "" {
		parts = append(parts, dim.Render(model))
	}
	line := strings.Join(parts, " · ")
	if !live {
		line += dim.Render("  (window share and cost show once its status line runs)")
	}
	return icon, line
}

// limitsRow is the plan's usage limits, which every agent shares, then the
// fleet: counts, total output, headless cost and open tasks.
func (m *model) limitsRow(now int64) (string, string) {
	var lim string
	switch {
	case !m.hasLimits:
		lim = dim.Render("plan limits: no report yet")
	default:
		var ws []string
		for _, w := range []struct {
			name string
			win  *agent.Window
		}{{"5h", m.limits.FiveHour}, {"7d", m.limits.SevenDay}} {
			if w.win == nil {
				continue
			}
			st := plain
			switch {
			case w.win.Used >= 90:
				st = tone[agent.Waiting].Bold(true)
			case w.win.Used >= 70:
				st = tone[agent.Working]
			}
			s := st.Render(fmt.Sprintf("%s %.0f%%", w.name, w.win.Used))
			if w.win.ResetsAt > now {
				s += dim.Render(" · resets " + resetTime(w.win.ResetsAt, now))
			} else if w.win.ResetsAt > 0 {
				s += dim.Render(" · reset " + resetTime(w.win.ResetsAt, now) + ", no report since")
			}
			ws = append(ws, s)
		}
		lim = strings.Join(ws, dim.Render(" │ "))
		if age := now - m.limits.Updated; age > 10*60 {
			lim += dim.Render(" (as of " + ago(age) + " ago)")
		}
	}

	n := map[string]int{}
	out := 0
	for _, a := range m.agents {
		n[a.Status]++
		if u := m.usage[a.Transcript]; u != nil {
			out += u.Out
		}
	}
	cost := 0.0
	for _, t := range m.threads {
		n[t.Status]++
		out += t.Usage.Output
		cost += t.Usage.CostUSD
	}
	var fleet []string
	for _, s := range []string{agent.Working, agent.Waiting, agent.Done} {
		if n[s] > 0 {
			fleet = append(fleet, tone[s].Render(fmt.Sprintf("%d %s", n[s], s)))
		}
	}
	fleet = append(fleet, dim.Render(tokens(out)+" out"))
	if cost > 0 {
		fleet = append(fleet, dim.Render(fmt.Sprintf("$%.2f headless", cost)))
	}
	if open := len(agent.OpenTasks(m.comms)); open > 0 {
		fleet = append(fleet, accent.Render(fmt.Sprintf("⇄ %d open", open)))
	}
	return accent.Render("⏱"), lim + dim.Render("   ") + strings.Join(fleet, dim.Render(" · "))
}

// resetTime is a reset as a clock time today, or a date after that.
func resetTime(at, now int64) string {
	t, n := time.Unix(at, 0), time.Unix(now, 0)
	if t.YearDay() == n.YearDay() && t.Year() == n.Year() {
		return t.Format("3:04pm")
	}
	return t.Format("Jan 2 3:04pm")
}
