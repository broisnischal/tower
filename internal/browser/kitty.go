package browser

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

// kitty graphics with Unicode placeholders: I upload each frame as a PNG
// with a virtual placement of cols x rows cells, then fill the pane with
// U+10EEEE cells whose diacritics name the row and column of the image
// they show. The placeholders are ordinary text to tmux, so the image
// stays inside the pane, scrolls with it and disappears when the window
// does. Uploads go through tmux's passthrough.
//
// Frames alternate between two image ids. Re-uploading the id on screen
// would blank it until the last chunk arrives; uploading to the other id
// and then switching the placeholders over never shows a half frame.

// imageID is an image id split the way placeholders carry it: the low byte
// is the 256-colour foreground, the high byte a third diacritic.
type imageID struct{ color, high uint8 }

func (i imageID) id() uint32 { return uint32(i.high)<<24 | uint32(i.color) }

// imageIDs picks my two ids from pid, so two viewers on one terminal do not
// draw each other's frames.
func imageIDs(pid int) [2]imageID {
	c := uint8(16 + (pid%120)*2) // 16..254, and c+1 still fits
	h := uint8(1 + (pid/120)%254)
	return [2]imageID{{c, h}, {c + 1, h}}
}

// maxPlaceholder is how many rows or columns the diacritics can number.
const maxPlaceholder = 297

// kittyUpload is the escape sequence that uploads a base64 PNG (as Chrome
// sends it) as image id, placed virtually over cols x rows cells. The data
// goes in chunks of 4096 bytes; inside tmux each chunk is wrapped on its
// own, since tmux caps one passthrough sequence. q=2 keeps the terminal
// from answering into my input.
func kittyUpload(id imageID, b64 string, cols, rows int, tmux bool) string {
	var sb strings.Builder
	sb.Grow(len(b64) + len(b64)/kitty.MaxChunkSize*48 + 64)
	for i := 0; i == 0 || i < len(b64); i += kitty.MaxChunkSize {
		end := min(i+kitty.MaxChunkSize, len(b64))
		more := "0"
		if end < len(b64) {
			more = "1"
		}
		ctl := "q=2,m=" + more
		if i == 0 {
			ctl = "a=T,U=1,f=100,t=d,q=2,i=" + strconv.FormatUint(uint64(id.id()), 10) +
				",c=" + strconv.Itoa(cols) + ",r=" + strconv.Itoa(rows) + ",m=" + more
		}
		writeAPC(&sb, ctl, b64[i:end], tmux)
	}
	return sb.String()
}

// kittyDelete frees an image and its placements in the terminal.
func kittyDelete(id imageID, tmux bool) string {
	var sb strings.Builder
	writeAPC(&sb, "a=d,d=I,q=2,i="+strconv.FormatUint(uint64(id.id()), 10), "", tmux)
	return sb.String()
}

func writeAPC(sb *strings.Builder, ctl, payload string, tmux bool) {
	seq := "\x1b_G" + ctl
	if payload != "" {
		seq += ";" + payload
	}
	seq += "\x1b\\"
	if tmux {
		seq = ansi.TmuxPassthrough(seq)
	}
	sb.WriteString(seq)
}

// placeholders are the rows of cells that show image id: every cell names
// its row, column and the id's high byte, so each one stands on its own
// when tmux redraws only part of a line.
func placeholders(id imageID, cols, rows int) []string {
	cols, rows = min(cols, maxPlaceholder), min(rows, maxPlaceholder)
	fg := "\x1b[38;5;" + strconv.Itoa(int(id.color)) + "m"
	high := kitty.Diacritic(int(id.high))
	lines := make([]string, rows)
	var sb strings.Builder
	for r := range rows {
		sb.Reset()
		sb.WriteString(fg)
		row := kitty.Diacritic(r)
		for c := range cols {
			sb.WriteRune(kitty.Placeholder)
			sb.WriteRune(row)
			sb.WriteRune(kitty.Diacritic(c))
			sb.WriteRune(high)
		}
		sb.WriteString("\x1b[39m")
		lines[r] = sb.String()
	}
	return lines
}
