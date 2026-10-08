package browser

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestResolve(t *testing.T) {
	cases := map[string]string{
		"":                           "",
		"  https://example.com/a  ":  "https://example.com/a",
		"http://localhost:3000":      "http://localhost:3000",
		"example.com":                "https://example.com",
		"go.dev/doc/effective_go":    "https://go.dev/doc/effective_go",
		"sub.example.co.uk:8443/x?y": "https://sub.example.co.uk:8443/x?y",
		"localhost":                  "http://localhost",
		"localhost:3000/login":       "http://localhost:3000/login",
		"127.0.0.1:8080":             "http://127.0.0.1:8080",
		"[::1]:5173":                 "http://[::1]:5173",
		":3000":                      "http://localhost:3000",
		":5173/app":                  "http://localhost:5173/app",
		"about:blank":                "about:blank",
		"data:text/html,<b>hi</b>":   "data:text/html,<b>hi</b>",
		"view-source:example.com":    "view-source:example.com",
		"/tmp/page.html":             "file:///tmp/page.html",
		"golang":                     searchURL + "golang",
		"how do tabs work":           searchURL + "how+do+tabs+work",
		"example.com is down":        searchURL + "example.com+is+down",
		"c++ & go":                   searchURL + "c%2B%2B+%26+go",
		"v1.2.3":                     searchURL + "v1.2.3",
		"3000":                       searchURL + "3000",
	}
	for in, want := range cases {
		if got := Resolve(in); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveArgFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "index.html")
	os.WriteFile(p, []byte("<p>hi"), 0o644)
	t.Chdir(dir)
	if got := ResolveArg("index.html"); got != "file://"+p {
		t.Fatalf("ResolveArg = %q", got)
	}
	if got := ResolveArg("localhost:3000"); got != "http://localhost:3000" {
		t.Fatalf("ResolveArg(localhost) = %q", got)
	}
}

func TestHost(t *testing.T) {
	cases := map[string]string{
		"http://localhost:3000/x": "localhost:3000",
		"https://example.com":     "example.com",
		"about:blank":             "blank",
		"file:///tmp/site/a.html": "a.html",
	}
	for in, want := range cases {
		if got := Host(in); got != want {
			t.Errorf("Host(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPageInput(t *testing.T) {
	cases := []struct {
		msg  tea.KeyMsg
		key  string
		text string // typed text the key event carries
		mods int
		ins  string // inserted text instead of a key
	}{
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}, "a", "a", 0, ""},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("A")}, "A", "A", modShift, ""},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("é")}, "é", "é", 0, ""},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x"), Alt: true}, "x", "x", modAlt, ""},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello"), Paste: true}, "", "", 0, "hello"},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi")}, "", "", 0, "hi"},
		{tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}, " ", " ", 0, ""},
		{tea.KeyMsg{Type: tea.KeyEnter}, "Enter", "\r", 0, ""},
		{tea.KeyMsg{Type: tea.KeyBackspace}, "Backspace", "", 0, ""},
		{tea.KeyMsg{Type: tea.KeyTab}, "Tab", "", 0, ""},
		{tea.KeyMsg{Type: tea.KeyShiftTab}, "Tab", "", modShift, ""},
		{tea.KeyMsg{Type: tea.KeyEsc}, "Escape", "", 0, ""},
		{tea.KeyMsg{Type: tea.KeyLeft}, "ArrowLeft", "", 0, ""},
		{tea.KeyMsg{Type: tea.KeyCtrlShiftRight}, "ArrowRight", "", modCtrl | modShift, ""},
		{tea.KeyMsg{Type: tea.KeyPgDown}, "PageDown", "", 0, ""},
		{tea.KeyMsg{Type: tea.KeyCtrlA}, "a", "a", modCtrl, ""},
		{tea.KeyMsg{Type: tea.KeyF5}, "F5", "", 0, ""},
		{tea.KeyMsg{Type: tea.KeyF12}, "F12", "", 0, ""},
	}
	for _, c := range cases {
		def, mods, ins, ok := pageInput(c.msg)
		if !ok || def.key != c.key || def.text != c.text || mods != c.mods || ins != c.ins {
			t.Errorf("%q: got key %q text %q mods %d insert %q ok %v", c.msg.String(), def.key, def.text, mods, ins, ok)
		}
	}
	if def, _, _, _ := pageInput(tea.KeyMsg{Type: tea.KeyF5}); def.vk != 116 {
		t.Errorf("F5 virtual key %d, want 116", def.vk)
	}
	if def := charKey('q'); def.code != "KeyQ" || def.vk != 'Q' {
		t.Errorf("charKey(q) = %+v", def)
	}
}

func TestLayout(t *testing.T) {
	l := layout{cols: 100, rows: 40, cellW: 10, cellH: 20, scale: 1, zoom: 1, render: renderBlocks}
	if w, h := l.css(); w != 1000 || h != 800 {
		t.Fatalf("css %dx%d", w, h)
	}
	if w, h := l.frameMax(); w != 400 || h != 320 {
		t.Fatalf("blocks frames %dx%d", w, h)
	}
	if x, y := l.toPage(0, 0); x != 5 || y != 10 {
		t.Fatalf("toPage(0,0) = %v,%v", x, y)
	}
	if x, y := l.toPage(99, 39); x != 995 || y != 790 {
		t.Fatalf("toPage(99,39) = %v,%v", x, y)
	}

	l.zoom = 2 // CSS pixels twice as big: half the viewport, same frame size
	if w, h := l.css(); w != 500 || h != 400 || l.dsf() != 2 {
		t.Fatalf("zoomed css %dx%d dsf %v", w, h, l.dsf())
	}
	if x, y := l.toPage(99, 39); x != 497.5 || y != 395 {
		t.Fatalf("zoomed toPage = %v,%v", x, y)
	}

	// A HiDPI screen: 20x40 pixel cells, two device pixels per CSS pixel.
	hi := layout{cols: 100, rows: 40, cellW: 20, cellH: 40, scale: 2, zoom: 1.25, render: renderKitty}
	if w, h := hi.css(); w != 800 || h != 640 || hi.dsf() != 2.5 {
		t.Fatalf("hidpi css %dx%d dsf %v", w, h, hi.dsf())
	}
	if w, h := hi.frameMax(); w != 2000 || h != 1600 {
		t.Fatalf("hidpi frames %dx%d", w, h)
	}

	k := layout{cols: 400, rows: 50, cellW: 10, cellH: 20, scale: 1, zoom: 1, render: renderKitty}
	if c, r := k.imageCells(); c != maxPlaceholder || r != 50 {
		t.Fatalf("kitty cells %dx%d", c, r)
	}
	if w, h := k.frameMax(); w != maxPlaceholder*10 || h != 1000 {
		t.Fatalf("kitty frames %dx%d", w, h)
	}
	if k.format() != "png" || l.format() != "jpeg" {
		t.Fatal("formats")
	}
}

func TestDisplayScale(t *testing.T) {
	for h, want := range map[int]float64{0: 1, 16: 1, 19: 1, 25: 1, 32: 2, 40: 2, 60: 3} {
		if got := displayScale(h); got != want {
			t.Errorf("displayScale(%d) = %v, want %v", h, got, want)
		}
	}
}

func TestKittyTerminal(t *testing.T) {
	yes := [][]string{{"xterm-kitty"}, {"xterm-256color", "kitty(0.42.1)"}, {"xterm-ghostty", ""}, {"tmux-256color", "ghostty 1.2.0"}}
	no := [][]string{{"alacritty", ""}, {"xterm-256color", "foot"}, {"", ""}, {"tmux-256color"}}
	for _, n := range yes {
		if !kittyTerminal(n...) {
			t.Errorf("%q: want kitty", n)
		}
	}
	for _, n := range no {
		if kittyTerminal(n...) {
			t.Errorf("%q: want blocks", n)
		}
	}
}

func TestDevtoolsURL(t *testing.T) {
	u, ok := devtoolsURL("\nDevTools listening on ws://127.0.0.1:41235/devtools/browser/0c5e-41f1\n")
	if !ok || u != "ws://127.0.0.1:41235/devtools/browser/0c5e-41f1" {
		t.Fatalf("got %q %v", u, ok)
	}
	if _, ok := devtoolsURL("[1008/101:ERROR:bus.cc] Failed to connect to the bus"); ok {
		t.Fatal("matched a log line")
	}
}

func TestProfileLocked(t *testing.T) {
	dir := t.TempDir()
	if profileLocked(dir) {
		t.Fatal("no lock, but locked")
	}
	lock := filepath.Join(dir, "SingletonLock")
	os.Symlink("myhost-"+strconv.Itoa(os.Getpid()), lock)
	if !profileLocked(dir) {
		t.Fatal("live pid, but not locked")
	}
	os.Remove(lock)
	os.Symlink("myhost-2147483646", lock)
	if profileLocked(dir) {
		t.Fatal("dead pid, but locked")
	}
}

func TestFramesKeepNewestAndAckAll(t *testing.T) {
	f := newFrames()
	f.put(screencastFrame{Data: "a", SessionID: 1}, "s")
	f.put(screencastFrame{Data: "b", SessionID: 2}, "s")
	<-f.ready
	fr, acks := f.take()
	if fr.Data != "b" || len(acks) != 2 || acks[0].id != 1 || acks[1].id != 2 {
		t.Fatalf("took %q with acks %+v", fr.Data, acks)
	}
	f.redraw()
	<-f.ready
	if fr, acks := f.take(); fr == nil || fr.Data != "b" || len(acks) != 0 {
		t.Fatalf("redraw took %v %+v", fr, acks)
	}
}
