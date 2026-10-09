package ui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"tower/internal/agent"
	"tower/internal/branch"
)

// The branches tab is every branch of the selected agent's repo: where each
// is checked out and who works there, how it stands against the base branch
// and the remote, and what is uncommitted. The agent's own branch is marked.

// workers is every agent and thread, for putting names on worktrees. It is
// a copy, so the scan can run off the UI goroutine.
func (m *model) workers() []branch.Who {
	var who []branch.Who
	for _, a := range m.agents {
		who = append(who, branch.Who{Name: a.Name, Dir: a.Cwd})
	}
	for _, t := range m.threads {
		who = append(who, branch.Who{Name: t.Name, Dir: t.Cwd})
	}
	return who
}

func branchView(dir, name string, who []branch.Who) string {
	r, err := branch.Scan(dir, who)
	if err != nil {
		return dim.Render(err.Error())
	}
	now := time.Now().Unix()
	var info []string
	if r.URL != "" {
		info = append(info, branch.Short(r.URL))
	}
	if r.Base != "" {
		info = append(info, "base "+r.Base)
	}
	if r.Remote != "" && r.Fetched > 0 {
		info = append(info, "last fetch "+since(r.Fetched, now))
	}
	count := fmt.Sprintf("%d branches", len(r.Branches))
	if len(r.Branches) == 1 {
		count = "1 branch"
	}
	lines := []string{bold.Render(home(r.Root)) + "  " + dim.Render(strings.Join(info, " · ")) + "  " + dim.Render(count), ""}
	style := map[branch.Level]func(...string) string{branch.Plain: dim.Render, branch.Good: green.Render, branch.Todo: tone[agent.Working].Render}
	for _, b := range r.Branches {
		mark := "  "
		if slices.Contains(b.Agents, name) {
			mark = accent.Render("▸ ")
		}
		top := mark + bold.Render(b.Name)
		if b.Worktree != "" {
			top += "  " + dim.Render(home(b.Worktree))
		}
		if len(b.Agents) > 0 {
			top += "  " + link.Render(strings.Join(b.Agents, ", "))
		}
		lines = append(lines, top)
		if b.Head != "" {
			lines = append(lines, "    "+dim.Render(b.Head+" "+b.Subject+" · "+since(b.When, now)))
		}
		var notes []string
		for _, n := range []func() (string, branch.Level){
			func() (string, branch.Level) { return r.VsBase(b) },
			func() (string, branch.Level) { return r.Pushed(b) },
			b.Uncommitted,
		} {
			if text, lvl := n(); text != "" {
				notes = append(notes, style[lvl](text))
			}
		}
		lines = append(lines, "    "+strings.Join(notes, dim.Render(" · ")), "")
	}
	return strings.Join(lines, "\n")
}

func since(t, now int64) string {
	if a := branch.Age(t, now); a != "now" {
		return a + " ago"
	}
	return "just now"
}
