package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// While a rewrite runs, the message in the box is shown being worked over: a
// band of ASCII noise sweeps through it, the text ahead of the band dimmed
// and the text behind it back to full brightness, and the status line
// carries a bouncing scanner with the elapsed time.

const genFrame = 70 * time.Millisecond

// genTickMsg advances the effect of one rewrite, named by its sequence number.
type genTickMsg int

func genTick(seq int) tea.Cmd {
	return tea.Tick(genFrame, func(time.Time) tea.Msg { return genTickMsg(seq) })
}

// noise runs from light to dense.
var noise = []rune(".:-=+*#%@")

const (
	genBand  = 4  // half width of the noise band, in characters
	genSweep = 18 // frames per pass, whatever the length of the message
)

// generatingBox draws text as the composer would (w by h, "❯ " on the first
// line, the end of a long message in view) with the noise band at frame.
func generatingBox(text string, w, h, frame int) string {
	lines := strings.Split(ansi.Wrap(text, max(1, w-2), ""), "\n")
	if len(lines) > h {
		lines = lines[len(lines)-h:]
	}
	total := 0
	for _, l := range lines {
		total += len([]rune(l)) + 1
	}
	period := total + 2*genBand
	head := (frame*max(1, period/genSweep))%period - genBand
	hot := accent.Bold(true)

	out := make([]string, h)
	idx := 0
	for i := range out {
		prefix := "  "
		if i == 0 {
			prefix = accent.Render("❯ ")
		}
		if i >= len(lines) {
			out[i] = fit(prefix, w)
			continue
		}
		var b strings.Builder
		for _, r := range lines[i] {
			switch d := abs(idx - head); {
			case d <= genBand:
				// denser toward the middle of the band, flickering a step either way
				n := (len(noise) - 1) * (genBand - d) / genBand
				n = clamp(n+(idx*7919+frame*104729)%3-1, 0, len(noise)-1)
				style := accent
				if d <= 1 {
					style = hot
				}
				b.WriteString(style.Render(string(noise[n])))
			case idx < head:
				b.WriteRune(r)
			default:
				b.WriteString(dim.Render(string(r)))
			}
			idx++
		}
		idx++ // the line break
		out[i] = fit(prefix+b.String(), w)
	}
	return strings.Join(out, "\n")
}

// generatingStatus is the status line under the box during a rewrite.
func generatingStatus(frame int, since time.Time, model string, send bool) string {
	const width, comet = 12, "-=#=-"
	span := width - len(comet)
	p := frame % (2 * span)
	if p > span {
		p = 2*span - p
	}
	bar := "[" + strings.Repeat(" ", p) + comet + strings.Repeat(" ", span-p) + "]"
	what := "rewriting"
	if send {
		what = "rewriting, then sending"
	}
	return accent.Bold(true).Render("✦ generating") + " " + accent.Render(bar) + " " +
		dim.Render(fmt.Sprintf("%.1fs · %s · %s · ctrl+c cancel", time.Since(since).Seconds(), what, model))
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
