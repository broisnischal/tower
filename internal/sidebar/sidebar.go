// Package sidebar manages tower's docked panes: the agents sidebar on the
// left and the input bar along the bottom. Each window gets its own copy when
// a kind is on, so switching windows never resizes a pane (a resize makes
// Claude Code redraw its whole screen, which flickers).
package sidebar

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"tower/internal/agent"
	"tower/internal/tmux"
)

// Kind is one sort of docked pane.
type Kind struct {
	Name   string // the tower subcommand it runs
	marker string // pane option set on it
	wanted string // global option: 1 while it should be shown
	split  func() []string
}

var (
	Sidebar = Kind{"sidebar", "@tower_sidebar", "@tower_sidebar_on", func() []string {
		return []string{"-hbf", "-l", tmux.Option("@tower_width", "40")}
	}}
	Input = Kind{"input", "@tower_input", "@tower_input_on", func() []string {
		return []string{"-vf", "-l", tmux.Option("@tower_input_height", "5")}
	}}
	kinds = []Kind{Sidebar, Input}
)

// ByName finds a kind by its name.
func ByName(name string) (Kind, bool) {
	for _, k := range kinds {
		if k.Name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// lock serialises pane creation: one window switch can fire two hooks.
func lock() func() {
	os.MkdirAll(agent.Dir(), 0o700)
	f, err := os.OpenFile(filepath.Join(agent.Dir(), "panes.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
}

func (k Kind) on() bool { return tmux.Option(k.wanted, "") == "1" }

// panes lists this kind's panes as "pane_id window_id".
func (k Kind) panes(target ...string) [][2]string {
	args := []string{"list-panes", "-a"}
	if len(target) > 0 {
		args = []string{"list-panes", "-t", target[0]}
	}
	var out [][2]string
	for _, l := range strings.Split(tmux.Run(append(args, "-F", "#{pane_id} #{window_id} #{"+k.marker+"}")...), "\n") {
		if v := strings.Fields(l); len(v) == 3 && v[2] == "1" {
			out = append(out, [2]string{v[0], v[1]})
		}
	}
	return out
}

// Ensure gives target's window this pane if it has none and returns its id.
func (k Kind) Ensure(target string) string {
	defer lock()()
	win := tmux.Run("display", "-p", "-t", target, "#{window_id}")
	if win == "" {
		return ""
	}
	if p := k.panes(win); len(p) > 0 {
		return p[0][0]
	}
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	// Started in the window's folder, so automatic-rename keeps the window's
	// name when this pane has focus.
	args := append([]string{"split-window", "-d", "-c", "#{pane_current_path}"}, k.split()...)
	args = append(args, "-t", win, "-P", "-F", "#{pane_id}", "exec "+agent.ShellQuote(self)+" "+k.Name)
	id := tmux.Run(args...)
	if id != "" {
		tmux.Run("set", "-p", "-t", id, k.marker, "1")
	}
	return id
}

// Show turns this kind on and adds it to every window.
func (k Kind) Show() {
	tmux.Run("set", "-g", k.wanted, "1")
	for _, w := range strings.Split(tmux.Run("list-windows", "-a", "-F", "#{window_id}"), "\n") {
		if w != "" {
			k.Ensure(w)
		}
	}
}

// Hide turns this kind off and closes its panes.
func (k Kind) Hide() {
	tmux.Run("set", "-g", k.wanted, "0")
	for _, p := range k.panes() {
		tmux.Run("kill-pane", "-t", p[0])
	}
}

// Toggle shows or hides this kind everywhere.
func (k Kind) Toggle() {
	if k.on() {
		k.Hide()
		return
	}
	k.Show()
}

// Focus moves the cursor into this window's pane of this kind, turning it on
// if needed. From inside that pane it goes back to the previous pane.
func (k Kind) Focus(target string) {
	if tmux.Run("display", "-p", "-t", target, "#{"+k.marker+"}") == "1" {
		tmux.Run("last-pane", "-t", target)
		return
	}
	if !k.on() {
		k.Show()
	}
	if id := k.Ensure(target); id != "" {
		tmux.Run("select-pane", "-t", id)
	}
}

// Fit puts the docked panes in target's window back to their set size;
// tmux shares out every window resize among all panes, docked ones too.
func Fit(target string) {
	if target == "all" {
		for _, w := range strings.Split(tmux.Run("list-windows", "-a", "-F", "#{window_id}"), "\n") {
			if w != "" {
				Fit(w)
			}
		}
		return
	}
	if tmux.Run("display", "-p", "-t", target, "#{window_zoomed_flag}") == "1" {
		return // resize-pane would unzoom it
	}
	for _, k := range kinds {
		for _, p := range k.panes(target) {
			if k.Name == "sidebar" {
				tmux.Run("resize-pane", "-t", p[0], "-x", tmux.Option("@tower_width", "40"))
			} else {
				tmux.Run("resize-pane", "-t", p[0], "-y", tmux.Option("@tower_input_height", "5"))
			}
		}
	}
}

// Respawn restarts every docked pane in place with the current binary,
// without touching the layout, so nothing resizes.
func Respawn() {
	self, err := os.Executable()
	if err != nil {
		return
	}
	for _, k := range kinds {
		for _, p := range k.panes() {
			dir := tmux.Run("display", "-p", "-t", p[1], "#{pane_current_path}")
			for _, l := range strings.Split(tmux.Run("list-panes", "-t", p[1], "-F", "#{pane_current_path}\t#{@tower_sidebar}#{@tower_input}"), "\n") {
				if path, marks, _ := strings.Cut(l, "\t"); marks == "" && path != "" {
					dir = path
					break
				}
			}
			tmux.Run("respawn-pane", "-k", "-c", dir, "-t", p[0], "exec "+agent.ShellQuote(self)+" "+k.Name)
		}
	}
}

// Follow runs on every window switch and new window: it adds the docked
// panes that are on to a window missing them, and lets Load mark agents
// there as seen.
func Follow(target string) {
	fresh := map[string]bool{}
	for _, k := range kinds {
		// Ensure also returns a pane that was already there; only one it
		// creates now must be spared the signal.
		if k.on() && len(k.panes(target)) == 0 {
			fresh[k.Ensure(target)] = true
		}
	}
	wake(target, fresh)
	agent.Load(true)
}

// wake tells the docked panes that were already in target's window to
// refresh now: hidden ones only check in every few seconds. Panes created
// this moment are skipped; they may not handle the signal yet.
func wake(target string, skip map[string]bool) {
	for _, l := range strings.Split(tmux.Run("list-panes", "-t", target, "-F", "#{pane_id} #{pane_pid} #{@tower_sidebar}#{@tower_input}"), "\n") {
		v := strings.Fields(l)
		if len(v) != 3 || skip[v[0]] {
			continue
		}
		if pid, err := strconv.Atoi(v[1]); err == nil {
			syscall.Kill(pid, syscall.SIGUSR1)
		}
	}
}
