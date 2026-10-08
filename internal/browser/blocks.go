package browser

import (
	"image"
	"image/color"
	"strconv"
	"strings"
)

// Half blocks: each cell shows two pixels, the top one as the foreground
// of ▀ and the bottom one as the background. Works in any true colour
// terminal.

type rgb struct{ r, g, b uint8 }

// downsample shrinks img to w x h with a box filter: every output pixel is
// the mean of the source pixels it covers, so thin text turns into grey
// instead of flickering in and out the way nearest neighbour does.
func downsample(img image.Image, w, h int) []rgb {
	out := make([]rgb, w*h)
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || sw <= 0 || sh <= 0 {
		return out
	}
	sum := sampler(img)
	for oy := range h {
		y0, y1 := span(oy, h, sh)
		for ox := range w {
			x0, x1 := span(ox, w, sw)
			out[oy*w+ox] = sum(b.Min.X+x0, b.Min.Y+y0, b.Min.X+x1, b.Min.Y+y1)
		}
	}
	return out
}

// span is the source range [lo, hi) that output index i of n covers in a
// source of size src. It is never empty, so upscaling repeats pixels.
func span(i, n, src int) (lo, hi int) {
	lo, hi = i*src/n, (i+1)*src/n
	if hi <= lo {
		hi = lo + 1
	}
	return lo, min(hi, src)
}

// sampler returns a function that averages a rectangle of img, with fast
// paths for what Chrome sends: JPEG decodes to YCbCr, PNG to (N)RGBA.
func sampler(img image.Image) func(x0, y0, x1, y1 int) rgb {
	switch m := img.(type) {
	case *image.YCbCr:
		return func(x0, y0, x1, y1 int) rgb {
			// YCbCr to RGB is affine, so the mean of the converted pixels
			// is the conversion of the mean (up to clamping).
			var sy, sb, sr, n int
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					c := m.COffset(x, y)
					sy += int(m.Y[m.YOffset(x, y)])
					sb += int(m.Cb[c])
					sr += int(m.Cr[c])
					n++
				}
			}
			r, g, b := color.YCbCrToRGB(uint8(sy/n), uint8(sb/n), uint8(sr/n))
			return rgb{r, g, b}
		}
	case *image.RGBA:
		return rgbaMean(m.Pix, m.Stride, m.Rect.Min)
	case *image.NRGBA:
		return rgbaMean(m.Pix, m.Stride, m.Rect.Min) // Chrome's frames are opaque
	}
	return func(x0, y0, x1, y1 int) rgb {
		var sr, sg, sb, n uint32
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				r, g, b, _ := img.At(x, y).RGBA()
				sr, sg, sb, n = sr+r>>8, sg+g>>8, sb+b>>8, n+1
			}
		}
		return rgb{uint8(sr / n), uint8(sg / n), uint8(sb / n)}
	}
}

func rgbaMean(pix []uint8, stride int, o image.Point) func(x0, y0, x1, y1 int) rgb {
	return func(x0, y0, x1, y1 int) rgb {
		var sr, sg, sb, n int
		for y := y0; y < y1; y++ {
			row := (y-o.Y)*stride - o.X*4
			for x := x0; x < x1; x++ {
				i := row + x*4
				sr += int(pix[i])
				sg += int(pix[i+1])
				sb += int(pix[i+2])
				n++
			}
		}
		return rgb{uint8(sr / n), uint8(sg / n), uint8(sb / n)}
	}
}

// halfBlocks draws img into rows lines of cols cells.
func halfBlocks(img image.Image, cols, rows int) []string {
	px := downsample(img, cols, rows*2)
	lines := make([]string, rows)
	var sb strings.Builder
	for y := range rows {
		sb.Reset()
		top, bot := px[2*y*cols:(2*y+1)*cols], px[(2*y+1)*cols:(2*y+2)*cols]
		blockRow(&sb, top, bot)
		lines[y] = sb.String()
	}
	return lines
}

// blockRow writes one row of cells, emitting colours only when they change.
// Where both pixels match, a space on the background says it in fewer bytes.
func blockRow(sb *strings.Builder, top, bot []rgb) {
	var fg, bg rgb
	fgSet, bgSet := false, false
	for x := range top {
		t, b := top[x], bot[x]
		if !bgSet || b != bg {
			sgr(sb, 48, b)
			bg, bgSet = b, true
		}
		if t == b {
			sb.WriteByte(' ')
			continue
		}
		if !fgSet || t != fg {
			sgr(sb, 38, t)
			fg, fgSet = t, true
		}
		sb.WriteString("▀")
	}
	sb.WriteString("\x1b[0m")
}

func sgr(sb *strings.Builder, kind int, c rgb) {
	sb.WriteString("\x1b[")
	sb.WriteString(strconv.Itoa(kind))
	sb.WriteString(";2;")
	sb.WriteString(strconv.Itoa(int(c.r)))
	sb.WriteByte(';')
	sb.WriteString(strconv.Itoa(int(c.g)))
	sb.WriteByte(';')
	sb.WriteString(strconv.Itoa(int(c.b)))
	sb.WriteByte('m')
}
