package browser

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

func fill(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func TestDownsampleBoxFilter(t *testing.T) {
	// One pixel wide black and white stripes: nearest neighbour would pick
	// one of the two, a box filter gives grey.
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := range 8 {
		c := color.RGBA{0, 0, 0, 255}
		if x%2 == 1 {
			c = color.RGBA{255, 255, 255, 255}
		}
		fill(img, image.Rect(x, 0, x+1, 8), c)
	}
	for _, p := range downsample(img, 2, 2) {
		if p.r < 120 || p.r > 135 || p.r != p.g || p.g != p.b {
			t.Fatalf("pixel %v, want mid grey", p)
		}
	}
}

func TestDownsampleKeepsRegions(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 40))
	fill(img, image.Rect(0, 0, 40, 20), color.RGBA{200, 0, 0, 255})
	fill(img, image.Rect(0, 20, 40, 40), color.RGBA{0, 0, 200, 255})
	px := downsample(img, 4, 4)
	if px[0] != (rgb{200, 0, 0}) || px[15] != (rgb{0, 0, 200}) {
		t.Fatalf("corners %v %v", px[0], px[15])
	}
}

func TestDownsampleSubImageAndUpscale(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	fill(img, image.Rect(5, 5, 10, 10), color.RGBA{10, 20, 30, 255})
	sub := img.SubImage(image.Rect(5, 5, 7, 7))
	for _, p := range downsample(sub, 4, 4) { // upscaling repeats pixels
		if p != (rgb{10, 20, 30}) {
			t.Fatalf("pixel %v", p)
		}
	}
}

func TestDownsampleJPEGMatchesGeneric(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := range 32 {
		for x := range 64 {
			src.SetRGBA(x, y, color.RGBA{uint8(x * 4), uint8(y * 8), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := img.(*image.YCbCr); !ok {
		t.Fatalf("decoded %T, want *image.YCbCr", img)
	}
	fast := downsample(img, 8, 4)
	slow := downsample(struct{ image.Image }{img}, 8, 4) // hides the type: generic path
	for i := range fast {
		for _, d := range []int{int(fast[i].r) - int(slow[i].r), int(fast[i].g) - int(slow[i].g), int(fast[i].b) - int(slow[i].b)} {
			if d < -3 || d > 3 {
				t.Fatalf("pixel %d: fast %v, generic %v", i, fast[i], slow[i])
			}
		}
	}
}

func TestHalfBlocks(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	fill(img, image.Rect(0, 0, 2, 1), color.RGBA{255, 0, 0, 255}) // top row red
	fill(img, image.Rect(0, 1, 2, 2), color.RGBA{0, 0, 255, 255}) // bottom row blue
	lines := halfBlocks(img, 2, 1)
	want := "\x1b[48;2;0;0;255m\x1b[38;2;255;0;0m▀▀\x1b[0m"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("got %q, want %q", lines, want)
	}
	if w := ansi.StringWidth(lines[0]); w != 2 {
		t.Fatalf("width %d", w)
	}
}

func TestHalfBlocksSolidUsesSpaces(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	fill(img, img.Rect, color.RGBA{1, 2, 3, 255})
	lines := halfBlocks(img, 3, 1)
	if lines[0] != "\x1b[48;2;1;2;3m   \x1b[0m" {
		t.Fatalf("got %q", lines[0])
	}
}

func TestImageIDs(t *testing.T) {
	for _, pid := range []int{1, 119, 120, 4242, 1 << 22} {
		ids := imageIDs(pid)
		for _, id := range ids {
			if id.color < 16 || id.high == 0 {
				t.Fatalf("pid %d: id %+v", pid, id)
			}
		}
		if ids[0] == ids[1] || ids[0].id()>>24 != uint32(ids[0].high) || ids[0].id()&0xFFFFFF != uint32(ids[0].color) {
			t.Fatalf("pid %d: ids %+v", pid, ids)
		}
	}
	if imageIDs(100) == imageIDs(101) {
		t.Fatal("neighbouring pids share ids")
	}
}

// apcs splits a byte stream into the kitty graphics commands in it.
func apcs(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "\x1b_G")
		if i < 0 {
			return out
		}
		j := strings.Index(s[i:], "\x1b\\")
		out = append(out, s[i+3:i+j])
		s = s[i+j+2:]
	}
}

func TestKittyUploadChunks(t *testing.T) {
	data := strings.Repeat("QUJD", 2500) // 10000 bytes of base64
	id := imageID{color: 42, high: 7}
	cmds := apcs(kittyUpload(id, data, 80, 24, false))
	if len(cmds) != 3 {
		t.Fatalf("%d chunks, want 3", len(cmds))
	}
	first, _, _ := strings.Cut(cmds[0], ";")
	want := "a=T,U=1,f=100,t=d,q=2,i=117440554,c=80,r=24,m=1" // 7<<24 | 42
	if first != want {
		t.Fatalf("first control %q, want %q", first, want)
	}
	var joined strings.Builder
	for i, c := range cmds {
		ctl, payload, _ := strings.Cut(c, ";")
		more := strings.HasSuffix(ctl, "m=1")
		if more != (i < len(cmds)-1) {
			t.Fatalf("chunk %d: %q", i, ctl)
		}
		if i > 0 && ctl != "q=2,m=0" && ctl != "q=2,m=1" {
			t.Fatalf("chunk %d control %q", i, ctl)
		}
		if len(payload) > kitty.MaxChunkSize || (more && len(payload)%4 != 0) {
			t.Fatalf("chunk %d is %d bytes", i, len(payload))
		}
		joined.WriteString(payload)
	}
	if joined.String() != data {
		t.Fatal("chunks do not join back to the data")
	}
}

func TestKittyUploadTmuxPassthrough(t *testing.T) {
	out := kittyUpload(imageID{20, 1}, strings.Repeat("A", 5000), 10, 5, true)
	parts := strings.SplitAfter(out, "\x1b\x1b\\\x1b\\")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) != 2 {
		t.Fatalf("%d passthrough sequences, want one per chunk (2)", len(parts))
	}
	for _, p := range parts {
		if !strings.HasPrefix(p, "\x1bPtmux;\x1b\x1b_G") {
			t.Fatalf("sequence starts %q", p[:12])
		}
		// Inside, every ESC is doubled; the closing ST ends the DCS.
		inner := strings.TrimSuffix(strings.TrimPrefix(p, "\x1bPtmux;"), "\x1b\\")
		if strings.Count(inner, "\x1b") != 2*strings.Count(strings.ReplaceAll(inner, "\x1b\x1b", "\x1b"), "\x1b") {
			t.Fatalf("ESC not doubled in %q", inner[:20])
		}
	}
	if got := kittyDelete(imageID{20, 1}, false); got != "\x1b_Ga=d,d=I,q=2,i=16777236\x1b\\" {
		t.Fatalf("delete %q", got)
	}
}

func TestPlaceholders(t *testing.T) {
	id := imageID{color: 200, high: 3}
	lines := placeholders(id, 5, 3)
	if len(lines) != 3 {
		t.Fatalf("%d lines", len(lines))
	}
	for r, l := range lines {
		if !strings.HasPrefix(l, "\x1b[38;5;200m") || !strings.HasSuffix(l, "\x1b[39m") {
			t.Fatalf("row %d colours: %q", r, l)
		}
		// Bubble Tea measures and truncates lines: each cell must be one
		// column wide or the frame would be cut short.
		if w := ansi.StringWidth(l); w != 5 {
			t.Fatalf("row %d width %d, want 5", r, w)
		}
		body := strings.TrimSuffix(strings.TrimPrefix(l, "\x1b[38;5;200m"), "\x1b[39m")
		runes := []rune(body)
		if len(runes) != 5*4 {
			t.Fatalf("row %d has %d runes", r, len(runes))
		}
		for c := range 5 {
			cell := runes[c*4 : c*4+4]
			want := []rune{kitty.Placeholder, kitty.Diacritic(r), kitty.Diacritic(c), kitty.Diacritic(3)}
			for i := range cell {
				if cell[i] != want[i] {
					t.Fatalf("row %d col %d: %U, want %U", r, c, cell, want)
				}
			}
		}
	}
	if !utf8.ValidString(lines[0]) {
		t.Fatal("invalid UTF-8")
	}
	// The diacritics number at most 297 rows and columns.
	if l := placeholders(id, 400, 1); ansi.StringWidth(l[0]) != maxPlaceholder {
		t.Fatalf("width %d, want %d", ansi.StringWidth(l[0]), maxPlaceholder)
	}
}
