package browser

import "math"

// Fallback cell size when the terminal does not report pixels.
const defaultCellW, defaultCellH = 8, 16

// layout is the page area of the pane and what it means in pixels. The
// page's CSS viewport is the area's pixel size divided by the display
// scale and my zoom. Chrome runs at the display scale, so at zoom 1 a
// frame has one pixel per screen pixel.
type layout struct {
	cols, rows   int     // cells under the bar
	cellW, cellH int     // pixels per cell
	scale        float64 // display scale, fixed when Chrome starts
	zoom         float64 // mine, alt+= and alt+-
	render       renderMode
}

// displayScale guesses the screen's scale from the cell height: a cell
// taller than about 30 pixels means a HiDPI screen, where a CSS pixel
// should be two device pixels.
func displayScale(cellH int) float64 {
	return math.Max(1, math.Round(float64(cellH)/20))
}

// dsf is the device scale factor the page sees.
func (l layout) dsf() float64 { return l.scale * l.zoom }

func (l layout) ok() bool { return l.cols > 0 && l.rows > 0 }

// imageCells is the part of the area the image covers. Kitty placeholders
// can number at most 297 rows and columns.
func (l layout) imageCells() (cols, rows int) {
	if l.render == renderKitty {
		return min(l.cols, maxPlaceholder), min(l.rows, maxPlaceholder)
	}
	return l.cols, l.rows
}

func (l layout) pixels() (w, h int) {
	c, r := l.imageCells()
	return c * l.cellW, r * l.cellH
}

// css is the page's viewport in CSS pixels.
func (l layout) css() (w, h int) {
	pw, ph := l.pixels()
	return max(1, int(float64(pw)/l.dsf())), max(1, int(float64(ph)/l.dsf()))
}

// frameMax bounds the screencast frames, which come at the display scale
// whatever the page's zoom. Kitty shows them pixel for pixel; half blocks
// only need a few source pixels per output pixel for the box filter, so
// Chrome sends small frames.
func (l layout) frameMax() (w, h int) {
	pw, ph := l.pixels()
	if l.render == renderKitty {
		return pw, ph
	}
	c, r := l.imageCells()
	return min(pw, c*4), min(ph, r*2*4)
}

// format is the screencast image format: kitty takes Chrome's PNG as is,
// half blocks decode JPEG faster.
func (l layout) format() string {
	if l.render == renderKitty {
		return "png"
	}
	return "jpeg"
}

// toPage maps a cell of the page area to the CSS pixel at its centre.
func (l layout) toPage(col, row int) (x, y float64) {
	c, r := l.imageCells()
	w, h := l.css()
	return (float64(col) + 0.5) * float64(w) / float64(c), (float64(row) + 0.5) * float64(h) / float64(r)
}
