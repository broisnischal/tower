package browser

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"tower/internal/tmux"
)

type renderMode int

const (
	renderBlocks renderMode = iota
	renderKitty
)

func (r renderMode) String() string {
	if r == renderKitty {
		return "kitty"
	}
	return "blocks"
}

// terminal is what I know about where the viewer draws.
type terminal struct {
	render renderMode
	tmux   bool   // inside tmux: graphics need passthrough
	pane   string // $TMUX_PANE
}

// detectTerminal picks kitty graphics when the terminal I am drawn in
// speaks them, else half blocks. force is "kitty" or "blocks"; anything
// else detects.
func detectTerminal(force string) terminal {
	t := terminal{tmux: os.Getenv("TMUX") != "", pane: os.Getenv("TMUX_PANE")}
	switch force {
	case "kitty":
		t.render = renderKitty
		return t
	case "blocks":
		return t
	}
	if t.tmux {
		if passthrough(t.pane) && kittyTerminal(clientNames(t.pane)...) {
			t.render = renderKitty
		}
		return t
	}
	names := []string{os.Getenv("TERM"), os.Getenv("TERM_PROGRAM")}
	if os.Getenv("KITTY_WINDOW_ID") != "" || os.Getenv("GHOSTTY_RESOURCES_DIR") != "" {
		names = append(names, "kitty")
	}
	if kittyTerminal(names...) {
		t.render = renderKitty
	}
	return t
}

// kittyTerminal reports whether any of the terminal names is one I know
// draws kitty graphics with Unicode placeholders.
func kittyTerminal(names ...string) bool {
	for _, n := range names {
		n = strings.ToLower(n)
		if strings.Contains(n, "kitty") || strings.Contains(n, "ghostty") {
			return true
		}
	}
	return false
}

// clientNames are the terminal name, the type the terminal reported, and
// TERM_PROGRAM from the session environment of the client showing pane.
func clientNames(pane string) []string {
	names := strings.Split(tmux.Run("display", "-p", "-t", pane, "#{client_termname}\t#{client_termtype}"), "\t")
	if env := tmux.Run("show-environment", "-t", pane, "TERM_PROGRAM"); strings.HasPrefix(env, "TERM_PROGRAM=") {
		names = append(names, strings.TrimPrefix(env, "TERM_PROGRAM="))
	}
	return names
}

// passthrough reports whether tmux lets the pane talk to the terminal.
func passthrough(pane string) bool {
	v := tmux.Run("show", "-Apv", "-t", pane, "allow-passthrough")
	return v == "on" || v == "all"
}

// paneVisible reports whether the pane is on screen in some client.
func paneVisible(pane string) bool {
	v := strings.Fields(tmux.Run("display", "-p", "-t", pane, "#{window_active} #{session_attached}"))
	return len(v) == 2 && v[0] == "1" && v[1] != "0"
}

// cellPixels is the size of one cell in pixels: what the client showing
// the pane told tmux, else the tty's window size, or 0, 0 when nothing
// says. Inside tmux the pane's own window size only changes on a resize,
// so it can still hold tmux's default from before a client attached.
func cellPixels(f *os.File, pane string) (w, h int) {
	if pane != "" {
		v := strings.Fields(tmux.Run("display", "-p", "-t", pane, "#{client_cell_width} #{client_cell_height}"))
		if len(v) == 2 {
			w, _ = strconv.Atoi(v[0])
			h, _ = strconv.Atoi(v[1])
			if w > 0 && h > 0 {
				return w, h
			}
		}
	}
	var ws struct{ rows, cols, x, y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno == 0 && ws.cols > 0 && ws.rows > 0 && ws.x > 0 && ws.y > 0 {
		return int(ws.x) / int(ws.cols), int(ws.y) / int(ws.rows)
	}
	return 0, 0
}

// tty is the terminal shared by Bubble Tea and my image uploads. Each write
// holds the lock, so an upload never lands in the middle of a frame of
// text. It passes for a terminal, so Bubble Tea still sizes and resizes.
type tty struct {
	mu sync.Mutex
	f  *os.File
}

func (t *tty) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.f.Write(b)
}

func (t *tty) WriteString(s string) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.f.WriteString(s)
}

func (t *tty) Read(b []byte) (int, error) { return t.f.Read(b) }
func (t *tty) Close() error               { return nil }
func (t *tty) Fd() uintptr                { return t.f.Fd() }
