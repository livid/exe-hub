package preview

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/math/fixed"
)

// TestWrap: words break at spaces, CJK a character at a time with a
// closing mark kept off a line's start, a newline is a break, and the
// overflow ends the last line in an ellipsis.
func TestWrap(t *testing.T) {
	if err := load(); err != nil {
		t.Fatal(err)
	}
	f := face(regular, 36)
	lines := f.wrap("one two three four five six seven eight nine ten eleven twelve", fixed.I(400), 2)
	if len(lines) != 2 || !strings.HasSuffix(lines[1], "…") || f.width(lines[1]) > fixed.I(400) {
		t.Errorf("wrap: %q", lines)
	}
	lines = f.wrap("first\n\nsecond", fixed.I(400), 5)
	if len(lines) != 2 || lines[0] != "first" || lines[1] != "second" {
		t.Errorf("newlines: %q", lines)
	}
	zh := strings.Repeat("今天新增了价格提醒。", 6)
	for _, l := range f.wrap(zh, fixed.I(300), 8) {
		if strings.HasPrefix(l, "。") || f.width(l) > fixed.I(300) {
			t.Errorf("CJK line: %q", l)
		}
	}
	if l := f.wrap(strings.Repeat("x", 200), fixed.I(300), 1); len(l) != 1 || !strings.HasSuffix(l[0], "…") {
		t.Errorf("a word too wide: %q", l)
	}
	if l := f.wrap("", fixed.I(300), 3); len(l) != 0 {
		t.Errorf("empty: %q", l)
	}
}

// TestPNG: the card is a 1200×630 PNG whatever it says, an emoji
// (which no face has) included.
func TestPNG(t *testing.T) {
	b, err := Card{Title: "hub.example", Name: "Livid", Sub: "16 Sep 2026", Body: "exe 功能一览 🚀 done", Foot: "1 reply"}.PNG()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width != W || cfg.Height != H {
		t.Fatalf("%dx%d %v", cfg.Width, cfg.Height, err)
	}
}

// TestChromeOut writes the pictures a browser check diffs against
// chrome.css rendered at 2x when PREVIEW_OUT names a directory:
// chrome.png, a card with nothing on it but the chrome, and card.png,
// the sample card, to look at.
func TestChromeOut(t *testing.T) {
	dir := os.Getenv("PREVIEW_OUT")
	if dir == "" {
		t.Skip("PREVIEW_OUT unset")
	}
	for name, c := range map[string]Card{
		"chrome": {},
		"card":   {Title: "100.116.32.57:7788", Name: "Livid", Sub: "22 Sep 2026 · 14:03 UTC", Body: "exe-hub: the og picture it generated for posts has our Mac OS 9 Chrome, but it seems not pixel-accurate. Make it perfect.", Foot: "No replies yet"},
	} {
		b, err := c.PNG()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".png"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// legend reads a region of the picture as one character per CSS pixel
// (its top-left device pixel), each colour by the letter the palette
// gives it and any other by ?, so a region can be checked against a
// drawing.
func legend(img *image.RGBA, x0, y0, x1, y1 int, palette map[color.RGBA]byte) string {
	var sb strings.Builder
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			k, ok := palette[img.RGBAAt(x*u, y*u)]
			if !ok {
				k = '?'
			}
			sb.WriteByte(k)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// TestChrome pins the chrome to chrome.css as a browser paints it at
// 2x (measured in Chromium, 2026-09-22): the close box, the stripes'
// end columns, the window bevel's blended corners, the frame's shadows
// and the status strip's edges. The letters: # the ink, A white, B
// #ccc (the desk, the bar, and white at half over #999 at the bevel's
// corners), C #808080, D the stripes' #777, F the #999 shade, L #ddd
// (the strip, and white at 60% over #aaa at its corners), H white at
// 60% over #ddd, S #aaa; ? anything else, the well's gradient.
func TestChrome(t *testing.T) {
	if err := load(); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	Card{}.draw(img)
	palette := map[color.RGBA]byte{ink: '#', white: 'A', desk: 'B', g700: 'C', stripeDark: 'D', shade: 'F', light: 'L', w60dd: 'H', sedge: 'S'}
	if w50g6 != desk || w60aa != light {
		t.Fatal("the blends are no longer the desk's and the strip's greys: give them letters")
	}
	x1, y1 := W/u-18, H/u-18
	for _, c := range []struct {
		name           string
		x0, y0, x1, y1 int
		want           string
	}{
		{"top-left: border, bevel, close box, gap, stripe", 18, 18, 48, 40, `##############################
#AAAAAAAAAAAAAAAAAAAAAAAAAAAAA
#ABBBBBBBBBBBBBBBBBBBBBBBBBBBB
#ABBBCCCCCCCCCCCCCBBBBBBBBBBBB
#ABBBC###########ABBBBAAAAAAAA
#ABBBC#AAAAAAAAA#ABBBBBDDDDDDD
#ABBBC#A???????C#ABBBBAAAAAAAA
#ABBBC#A???????C#ABBBBBDDDDDDD
#ABBBC#A???????C#ABBBBAAAAAAAA
#ABBBC#A???????C#ABBBBBDDDDDDD
#ABBBC#A???????C#ABBBBAAAAAAAA
#ABBBC#A???????C#ABBBBBDDDDDDD
#ABBBC#A???????C#ABBBBAAAAAAAA
#ABBBC#ACCCCCCCC#ABBBBBDDDDDDD
#ABBBC###########ABBBBAAAAAAAA
#ABBBCAAAAAAAAAAAABBBBBDDDDDDD
#ABBBBBBBBBBBBBBBBBBBBBBBBBBBB
#ABBFFFFFFFFFFFFFFFFFFFFFFFFFF
#ABBF#########################
#ABBF#AAAAAAAAAAAAAAAAAAAAAAAA
#ABBF#AAAAAAAAAAAAAAAAAAAAAAAA
#ABBF#AAAAAAAAAAAAAAAAAAAAAAAA
`},
		{"top-right: stripe end, bar padding, bevel corner, shadow", x1 - 12, 18, x1 + 3, 40, `############BBB
AAAAAAAAAAB#BBB
BBBBBBBBBBF###B
BBBBBBBBBBF###B
AAAAAABBBBF###B
DDDDDDDBBBF###B
AAAAAABBBBF###B
DDDDDDDBBBF###B
AAAAAABBBBF###B
DDDDDDDBBBF###B
AAAAAABBBBF###B
DDDDDDDBBBF###B
AAAAAABBBBF###B
DDDDDDDBBBF###B
AAAAAABBBBF###B
DDDDDDDBBBF###B
BBBBBBBBBBF###B
FFFFFFBBBBF###B
#######BBBF###B
AAAAAA#ABBF###B
AAAAAA#ABBF###B
AAAAAA#ABBF###B
`},
		{"bottom-right: status strip corner, frame, bevel, shadow", x1 - 12, y1 - 31, x1 + 3, y1 + 3, `AAAAAA#ABBF###B
AAAAAA#ABBF###B
#######ABBF###B
HHHHHL#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
LLLLLS#ABBF###B
SSSSSS#ABBF###B
#######ABBF###B
AAAAAAAABBF###B
BBBBBBBBBBF###B
BBBBBBBBBBF###B
FFFFFFFFFFF###B
##############B
##############B
##############B
BBBBBBBBBBBBBBB
`},
		{"bottom-left: status strip corner, frame shadows, bevel", 18, y1 - 31, 33, y1 + 3, `#ABBF#AAAAAAAAA
#ABBF#AAAAAAAAA
#ABBF##########
#ABBF#HHHHHHHHH
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#HLLLLLLLL
#ABBF#LSSSSSSSS
#ABBB##########
#ABBBBAAAAAAAAA
#ABBBBBBBBBBBBB
#ABBBBBBBBBBBBB
#BFFFFFFFFFFFFF
###############
BB#############
BB#############
BBBBBBBBBBBBBBB
`},
	} {
		if got := legend(img, c.x0, c.y0, c.x1, c.y1, palette); got != c.want {
			t.Errorf("%s:\n%s", c.name, got)
		}
	}
	// the well shades from #a6 at its top-left device pixel to #e5 at
	// its bottom-right, evenly along the diagonal
	if c := img.RGBAAt(26*u, 24*u); c.R != 0xa6 {
		t.Errorf("well top-left %v", c)
	}
	if c := img.RGBAAt(32*u+1, 30*u+1); c.R != 0xe5 {
		t.Errorf("well bottom-right %v", c)
	}
}
