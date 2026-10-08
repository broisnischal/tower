package browser

import (
	"strconv"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// CDP modifier bits.
const (
	modAlt   = 1
	modCtrl  = 2
	modMeta  = 4
	modShift = 8
)

// keyDef is a key as Input.dispatchKeyEvent wants it.
type keyDef struct {
	key, code, text string
	vk              int
}

var (
	keyEnter     = keyDef{"Enter", "Enter", "\r", 13}
	keyBackspace = keyDef{"Backspace", "Backspace", "", 8}
	keyTab       = keyDef{"Tab", "Tab", "", 9}
	keyEscape    = keyDef{"Escape", "Escape", "", 27}
	keyUp        = keyDef{"ArrowUp", "ArrowUp", "", 38}
	keyDown      = keyDef{"ArrowDown", "ArrowDown", "", 40}
	keyLeft      = keyDef{"ArrowLeft", "ArrowLeft", "", 37}
	keyRight     = keyDef{"ArrowRight", "ArrowRight", "", 39}
	keyHome      = keyDef{"Home", "Home", "", 36}
	keyEnd       = keyDef{"End", "End", "", 35}
	keyPgUp      = keyDef{"PageUp", "PageUp", "", 33}
	keyPgDown    = keyDef{"PageDown", "PageDown", "", 34}
	keyDelete    = keyDef{"Delete", "Delete", "", 46}
	keyInsert    = keyDef{"Insert", "Insert", "", 45}
)

// named maps Bubble Tea's key types to page keys plus the modifiers the
// type implies.
var named = map[tea.KeyType]struct {
	k    keyDef
	mods int
}{
	tea.KeyEnter:          {keyEnter, 0},
	tea.KeyBackspace:      {keyBackspace, 0},
	tea.KeyCtrlH:          {keyBackspace, modCtrl}, // what most terminals send for ctrl+backspace
	tea.KeyTab:            {keyTab, 0},
	tea.KeyShiftTab:       {keyTab, modShift},
	tea.KeyEsc:            {keyEscape, 0},
	tea.KeyUp:             {keyUp, 0},
	tea.KeyDown:           {keyDown, 0},
	tea.KeyLeft:           {keyLeft, 0},
	tea.KeyRight:          {keyRight, 0},
	tea.KeyShiftUp:        {keyUp, modShift},
	tea.KeyShiftDown:      {keyDown, modShift},
	tea.KeyShiftLeft:      {keyLeft, modShift},
	tea.KeyShiftRight:     {keyRight, modShift},
	tea.KeyCtrlUp:         {keyUp, modCtrl},
	tea.KeyCtrlDown:       {keyDown, modCtrl},
	tea.KeyCtrlLeft:       {keyLeft, modCtrl},
	tea.KeyCtrlRight:      {keyRight, modCtrl},
	tea.KeyCtrlShiftUp:    {keyUp, modCtrl | modShift},
	tea.KeyCtrlShiftDown:  {keyDown, modCtrl | modShift},
	tea.KeyCtrlShiftLeft:  {keyLeft, modCtrl | modShift},
	tea.KeyCtrlShiftRight: {keyRight, modCtrl | modShift},
	tea.KeyHome:           {keyHome, 0},
	tea.KeyEnd:            {keyEnd, 0},
	tea.KeyShiftHome:      {keyHome, modShift},
	tea.KeyShiftEnd:       {keyEnd, modShift},
	tea.KeyCtrlHome:       {keyHome, modCtrl},
	tea.KeyCtrlEnd:        {keyEnd, modCtrl},
	tea.KeyCtrlShiftHome:  {keyHome, modCtrl | modShift},
	tea.KeyCtrlShiftEnd:   {keyEnd, modCtrl | modShift},
	tea.KeyPgUp:           {keyPgUp, 0},
	tea.KeyPgDown:         {keyPgDown, 0},
	tea.KeyCtrlPgUp:       {keyPgUp, modCtrl},
	tea.KeyCtrlPgDown:     {keyPgDown, modCtrl},
	tea.KeyDelete:         {keyDelete, 0},
	tea.KeyInsert:         {keyInsert, 0},
}

// pageInput turns a key from the terminal into what the page gets: a key
// press, or text to insert for pastes. ok is false for keys I drop.
func pageInput(k tea.KeyMsg) (def keyDef, mods int, text string, ok bool) {
	if k.Alt {
		mods |= modAlt
	}
	if n, found := named[k.Type]; found {
		return n.k, mods | n.mods, "", true
	}
	switch {
	case k.Type == tea.KeySpace:
		return charKey(' '), mods, "", true
	case k.Type == tea.KeyRunes:
		if k.Paste || len(k.Runes) != 1 {
			return keyDef{}, 0, string(k.Runes), len(k.Runes) > 0
		}
		r := k.Runes[0]
		if r < 0x80 && unicode.IsUpper(r) {
			mods |= modShift
		}
		return charKey(r), mods, "", true
	case k.Type <= tea.KeyF1 && k.Type >= tea.KeyF12: // these count down
		n := int(tea.KeyF1-k.Type) + 1
		f := "F" + strconv.Itoa(n)
		return keyDef{f, f, "", 111 + n}, mods, "", true
	case k.Type >= tea.KeyCtrlA && k.Type <= tea.KeyCtrlZ:
		return charKey(rune('a' + k.Type - tea.KeyCtrlA)), mods | modCtrl, "", true
	}
	return keyDef{}, 0, "", false
}

// charKey is the key for one typed character.
func charKey(r rune) keyDef {
	s := string(r)
	switch {
	case r == ' ':
		return keyDef{" ", "Space", " ", 32}
	case r < 0x80 && unicode.IsLetter(r):
		u := unicode.ToUpper(r)
		return keyDef{s, "Key" + string(u), s, int(u)}
	case r >= '0' && r <= '9':
		return keyDef{s, "Digit" + s, s, int(r)}
	}
	return keyDef{s, "", s, 0}
}
