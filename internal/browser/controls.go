package browser

import (
	"math"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// What my keys and mouse do in the viewer: shortcuts and the address bar
// first, then everything else goes to the page.

// key handles my shortcuts and sends the rest to the page.
func (v *viewer) key(k tea.KeyMsg) tea.Cmd {
	if k.String() == "ctrl+q" {
		return tea.Quit
	}
	if v.dialog != nil {
		return v.dialogKey(k)
	}
	if v.edit {
		return v.editKey(k)
	}
	t := v.tab()
	if t == nil {
		return nil
	}
	switch k.String() {
	case "ctrl+l":
		return v.startEdit()
	case "ctrl+r", "f5":
		t.send("Page.reload", nil)
		return nil
	case "alt+left":
		return v.step(-1)
	case "alt+right":
		return v.step(1)
	case "alt+=", "alt++":
		return v.setZoom(v.zoom * 1.25)
	case "alt+-":
		return v.setZoom(v.zoom / 1.25)
	case "alt+0":
		return v.setZoom(1)
	}
	def, mods, text, ok := pageInput(k)
	if !ok {
		return nil
	}
	if text != "" {
		t.insertText(text)
	} else {
		t.key(def, mods)
	}
	return nil
}

// tab is the tab on screen, or nil before Chrome is up and after the last
// tab closed.
func (v *viewer) tab() *tab {
	if v.s == nil {
		return nil
	}
	return v.s.current()
}

func (v *viewer) setZoom(z float64) tea.Cmd {
	v.zoom = math.Min(4, math.Max(0.25, z))
	if math.Abs(v.zoom-1) < 0.01 {
		v.zoom = 1
	}
	return v.apply()
}

func (v *viewer) step(d int) tea.Cmd {
	t := v.tab()
	if t == nil {
		return nil
	}
	return func() tea.Msg {
		if err := t.step(d); err != nil {
			return flashMsg(err.Error())
		}
		return nil
	}
}

// startEdit focuses the address bar with the URL in it; like Chrome, the
// first character I type replaces it.
func (v *viewer) startEdit() tea.Cmd {
	v.edit, v.fresh = true, true
	v.in.SetValue(v.url)
	v.in.CursorEnd()
	return v.in.Focus()
}

func (v *viewer) editKey(k tea.KeyMsg) tea.Cmd {
	switch k.Type {
	case tea.KeyEsc:
		v.edit = false
		v.in.Blur()
		return nil
	case tea.KeyEnter:
		v.edit = false
		v.in.Blur()
		return v.navigate(Resolve(v.in.Value()))
	case tea.KeyRunes, tea.KeySpace:
		if v.fresh {
			v.in.SetValue("")
		}
	case tea.KeyBackspace, tea.KeyDelete:
		if v.fresh {
			v.in.SetValue("")
			v.fresh = false
			return nil
		}
	}
	v.fresh = false
	var cmd tea.Cmd
	v.in, cmd = v.in.Update(k)
	return cmd
}

func (v *viewer) navigate(u string) tea.Cmd {
	t := v.tab()
	if u == "" || t == nil {
		return nil
	}
	v.url = u
	return func() tea.Msg {
		if err := t.navigate(u); err != nil {
			return flashMsg(err.Error())
		}
		return nil
	}
}

func (v *viewer) dialogKey(k tea.KeyMsg) tea.Cmd {
	switch k.Type {
	case tea.KeyEnter:
		v.s.answer(*v.dialog, true)
	case tea.KeyEsc:
		v.s.answer(*v.dialog, false)
	default:
		return nil
	}
	v.dialog = nil
	return nil
}

// mouse handles clicks on the bar and forwards the rest to the page, with
// cells mapped to CSS pixels.
func (v *viewer) mouse(m tea.MouseEvent) tea.Cmd {
	t := v.tab()
	if t == nil {
		return nil
	}
	if m.Y == 0 {
		if m.Action == tea.MouseActionPress && m.Button == tea.MouseButtonLeft {
			return v.barClick(m.X)
		}
		return nil
	}
	l := v.layout()
	cols, rows := l.imageCells()
	col, row := m.X, m.Y-1
	if col >= cols || row >= rows || v.dialog != nil {
		return nil
	}
	x, y := l.toPage(col, row)
	mods := mouseMods(m)
	switch {
	case m.IsWheel():
		dx, dy := wheelDelta(m.Button)
		t.wheel(x, y, dx, dy, mods)
	case m.Action == tea.MouseActionPress:
		v.held = m.Button
		v.countClick(col, row)
		t.mouse("mousePressed", x, y, buttonName(m.Button), buttonMask(m.Button), v.click, mods)
	case m.Action == tea.MouseActionRelease:
		b := v.held
		v.held = tea.MouseButtonNone
		t.mouse("mouseReleased", x, y, buttonName(b), 0, v.click, mods)
	case m.Action == tea.MouseActionMotion:
		t.mouse("mouseMoved", x, y, buttonName(v.held), buttonMask(v.held), 0, mods)
	}
	return nil
}

// countClick tells single, double and triple clicks apart.
func (v *viewer) countClick(col, row int) {
	if time.Since(v.clickAt) < 500*time.Millisecond && col == v.clickX && row == v.clickY && v.click < 3 {
		v.click++
	} else {
		v.click = 1
	}
	v.clickAt, v.clickX, v.clickY = time.Now(), col, row
}

func (v *viewer) barClick(x int) tea.Cmd {
	switch {
	case x == backCol:
		return v.step(-1)
	case x == fwdCol:
		return v.step(1)
	case x == reloadCol && v.loading:
		v.tab().send("Page.stopLoading", nil)
	case x == reloadCol:
		v.tab().send("Page.reload", nil)
	case x >= urlCol && !v.edit && v.dialog == nil:
		return v.startEdit()
	}
	return nil
}

func mouseMods(m tea.MouseEvent) int {
	mods := 0
	if m.Alt {
		mods |= modAlt
	}
	if m.Ctrl {
		mods |= modCtrl
	}
	if m.Shift {
		mods |= modShift
	}
	return mods
}

// wheelDelta is one notch of the wheel in CSS pixels, the way Chrome
// scrolls for a mouse wheel.
func wheelDelta(b tea.MouseButton) (dx, dy float64) {
	switch b {
	case tea.MouseButtonWheelUp:
		return 0, -100
	case tea.MouseButtonWheelDown:
		return 0, 100
	case tea.MouseButtonWheelLeft:
		return -100, 0
	case tea.MouseButtonWheelRight:
		return 100, 0
	}
	return 0, 0
}

func buttonName(b tea.MouseButton) string {
	switch b {
	case tea.MouseButtonLeft:
		return "left"
	case tea.MouseButtonMiddle:
		return "middle"
	case tea.MouseButtonRight:
		return "right"
	case tea.MouseButtonBackward:
		return "back"
	case tea.MouseButtonForward:
		return "forward"
	}
	return "none"
}

// buttonMask is the CDP buttons bit field for a held button.
func buttonMask(b tea.MouseButton) int {
	switch b {
	case tea.MouseButtonLeft:
		return 1
	case tea.MouseButtonRight:
		return 2
	case tea.MouseButtonMiddle:
		return 4
	case tea.MouseButtonBackward:
		return 8
	case tea.MouseButtonForward:
		return 16
	}
	return 0
}
