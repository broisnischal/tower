// Package tmux wraps the tmux CLI.
package tmux

import (
	"os/exec"
	"strings"
)

// Run executes a tmux command and returns its stdout without the trailing
// newline. Errors come back as empty output, which every caller treats as
// "not there".
func Run(args ...string) string {
	out, _ := exec.Command("tmux", args...).Output()
	return strings.TrimRight(string(out), "\n")
}

// RunInput is Run with input on stdin.
func RunInput(input string, args ...string) error {
	cmd := exec.Command("tmux", args...)
	cmd.Stdin = strings.NewReader(input)
	return cmd.Run()
}

// Option reads a global option, falling back to def when it is unset.
func Option(name, def string) string {
	if v := Run("show", "-gqv", name); v != "" {
		return v
	}
	return def
}

// Pane is one row of list-panes -a.
type Pane struct {
	ID, Session, SessionAttached      string
	WindowID, WindowIndex, WindowName string
	WindowActive, Index, Active       string
	Command, Path                     string
	AgentName, AgentState, Sidebar    string // @tower_name, @tower_state, @tower_sidebar
}

var paneFields = []string{
	"pane_id", "session_name", "session_attached",
	"window_id", "window_index", "window_name",
	"window_active", "pane_index", "pane_active",
	"pane_current_command", "pane_current_path",
	"@tower_name", "@tower_state", "@tower_sidebar",
}

// Panes lists every pane on the server, keyed by pane id.
func Panes() map[string]Pane {
	f := make([]string, len(paneFields))
	for i, k := range paneFields {
		f[i] = "#{" + k + "}"
	}
	ps := map[string]Pane{}
	for _, line := range strings.Split(Run("list-panes", "-a", "-F", strings.Join(f, "\t")), "\n") {
		v := strings.Split(line, "\t")
		if len(v) != len(paneFields) {
			continue
		}
		ps[v[0]] = Pane{
			ID: v[0], Session: v[1], SessionAttached: v[2],
			WindowID: v[3], WindowIndex: v[4], WindowName: v[5],
			WindowActive: v[6], Index: v[7], Active: v[8],
			Command: v[9], Path: v[10],
			AgentName: v[11], AgentState: v[12], Sidebar: v[13],
		}
	}
	return ps
}

// Visible reports whether the pane sits in the current window of a session
// someone is attached to.
func (p Pane) Visible() bool {
	return p.WindowActive == "1" && p.SessionAttached != "" && p.SessionAttached != "0"
}

// Session is one row of list-sessions.
type Session struct {
	Name     string
	Windows  string
	Attached bool
}

// Sessions lists sessions in tmux's order.
func Sessions() []Session {
	var out []Session
	for _, line := range strings.Split(Run("list-sessions", "-F", "#{session_name}\t#{session_windows}\t#{session_attached}"), "\n") {
		v := strings.Split(line, "\t")
		if len(v) != 3 {
			continue
		}
		out = append(out, Session{Name: v[0], Windows: v[1], Attached: v[2] != "0"})
	}
	return out
}
