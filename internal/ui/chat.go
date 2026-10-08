package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"tower/internal/agent"
	"tower/internal/thread"
)

var (
	green  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	red    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	italic = dim.Italic(true)
)

// chatState is a thread's chat plus the last event folded into it, so
// pushes that the history already covered are not applied twice.
type chatState struct {
	thread.Chat
	seq int64
}

func (c *chatState) apply(e thread.Event) {
	if e.Seq > 0 && e.Seq <= c.seq && e.Kind != thread.KindDelta {
		return
	}
	c.Apply(e)
	c.seq = max(c.seq, e.Seq)
}

// renderChat draws a thread's chat at width w: my messages, the agent's
// replies, each tool call as a card with its state, diffs, approvals waiting
// on me, and a summary line per finished turn.
func renderChat(items []thread.Event, w, frame int) []string {
	wrap := func(s string, width int) []string {
		return strings.Split(lipgloss.NewStyle().Width(max(10, width)).Render(s), "\n")
	}
	var out []string
	for _, it := range items {
		switch it.Kind {
		case thread.KindUser:
			out = append(out, "")
			for i, l := range wrap(it.Text, w-2) {
				p := "  "
				if i == 0 {
					p = accent.Bold(true).Render("❯ ")
				}
				out = append(out, p+bold.Render(l))
			}
			out = append(out, "")
		case thread.KindText:
			if it.Role == "reasoning" {
				ls := wrap(it.Text, w-2)
				if len(ls) > 3 {
					ls = append(ls[:3], "…")
				}
				for i, l := range ls {
					p := "  "
					if i == 0 {
						p = dim.Render("✻ ")
					}
					out = append(out, p+italic.Render(l))
				}
				continue
			}
			for i, l := range wrap(it.Text, w-2) {
				p := "  "
				if i == 0 {
					p = "● "
				}
				out = append(out, p+l)
			}
		case thread.KindTool:
			out = append(out, toolCard(it, w, frame)...)
		case thread.KindApproval:
			if it.Status != thread.Pending {
				continue
			}
			out = append(out, tone[agent.Waiting].Bold(true).Render("◐ approve "+it.Tool)+" "+it.Input)
			if it.Text != "" {
				out = append(out, "  "+dim.Render(it.Text))
			}
			out = append(out, "  "+tone[agent.Waiting].Render("y allow · A allow for this session · d deny"))
		case thread.KindTurn:
			switch it.Status {
			case thread.TurnDone:
				out = append(out, dim.Render("── done"+usageNote(it.Usage)+" ──"), "")
			case thread.TurnInterrupted:
				out = append(out, dim.Render("── interrupted ──"), "")
			case thread.TurnFailed:
				out = append(out, red.Render("── failed: "+it.Text+" ──"), "")
			}
		case thread.KindInfo:
			if it.Status == "error" {
				out = append(out, red.Render("! "+it.Text))
			} else {
				out = append(out, dim.Render("· "+it.Text))
			}
		}
	}
	return out
}

func toolCard(it thread.Event, w, frame int) []string {
	var icon string
	switch it.Status {
	case thread.ToolRunning:
		icon = tone[agent.Working].Render(spinner[frame%len(spinner)])
	case thread.ToolOK:
		icon = green.Render("✓")
	case thread.ToolFailed:
		icon = red.Render("✗")
	default:
		icon = dim.Render("⊘")
	}
	out := []string{icon + " " + bold.Render(it.Tool) + " " + it.Input}
	if it.Status == thread.ToolDeclined {
		out[0] += dim.Render("  declined")
	}
	if it.Diff != "" {
		ls := strings.Split(strings.TrimRight(it.Diff, "\n"), "\n")
		if len(ls) > 14 {
			ls = append(ls[:14], fmt.Sprintf("… %d more lines", len(ls)-14))
		}
		for _, l := range ls {
			st := dim
			switch {
			case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "@@"):
			case strings.HasPrefix(l, "+"):
				st = green
			case strings.HasPrefix(l, "-"):
				st = red
			}
			out = append(out, "    "+st.Render(l))
		}
	}
	if it.Output != "" && it.Diff == "" {
		ls := strings.Split(it.Output, "\n")
		if len(ls) > 4 {
			ls = append(ls[:4], fmt.Sprintf("… %d more lines", len(ls)-4))
		}
		for i, l := range ls {
			p := "    "
			if i == 0 {
				p = "  ⎿ "
			}
			out = append(out, dim.Render(p+l))
		}
	}
	return out
}

func usageNote(u *thread.Usage) string {
	if u == nil {
		return ""
	}
	var parts []string
	if u.Output > 0 {
		parts = append(parts, tokens(u.Output)+" out")
	}
	if u.Context > 0 {
		parts = append(parts, tokens(u.Context)+" ctx")
	}
	if u.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.3f", u.CostUSD))
	}
	if len(parts) == 0 {
		return ""
	}
	return " · " + strings.Join(parts, " · ")
}
