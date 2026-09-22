// Package preview draws the picture a chat app or a social site shows
// when a link to one of the hub's pages is pasted (its OpenGraph
// image): a 1200×630 PNG of one Platinum window — the pages' own
// chrome at twice the size, so its lines stay crisp at the half the
// picture is shown at — holding a post's author and words, a profile,
// or the hub itself. The type is Go Sans, with Droid Sans Fallback
// (Apache 2.0, see FONTS.md) for the CJK characters it lacks; a
// character neither has (an emoji) is left out.
package preview

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"math"
	"strings"
	"sync"
	"unicode"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/webp"
)

// W and H are the picture's size: the 1.91:1 every site's card takes.
const W, H = 1200, 630

//go:embed DroidSansFallback.ttf
var droidTTF []byte

// Pic is the side of a card's picture, before the 1px line around it: a
// picture made at this size goes on untouched.
const Pic = 96

// Card is what one picture says.
type Card struct {
	Title   string      // the window's title: the host
	Picture image.Image // the avatar or the hub icon, square at the left; nil for none
	Crisp   bool        // scale the picture by whole pixels: it is pixel art
	Name    string      // the bold line beside the picture
	Sub     string      // the grey line under the name: a date, an id
	Body    string      // the words, wrapped and cut to fit; newlines break lines
	Foot    string      // the status bar's left text
	Right   string      // and its right one
}

// the Platinum palette, as the page's CSS names it
var (
	desk  = color.RGBA{0xcc, 0xcc, 0xcc, 0xff}
	ink   = color.RGBA{0x26, 0x26, 0x26, 0xff}
	white = color.RGBA{0xff, 0xff, 0xff, 0xff}
	light = color.RGBA{0xdd, 0xdd, 0xdd, 0xff}
	shade = color.RGBA{0x99, 0x99, 0x99, 0xff}
	grey  = color.RGBA{0x66, 0x66, 0x66, 0xff}
	dim   = color.RGBA{0x33, 0x33, 0x33, 0xff}
)

var (
	once                    sync.Once
	regular, bold, fallback *opentype.Font
	parseErr                error
)

func load() error {
	once.Do(func() {
		if regular, parseErr = opentype.Parse(goregular.TTF); parseErr != nil {
			return
		}
		if bold, parseErr = opentype.Parse(gobold.TTF); parseErr != nil {
			return
		}
		fallback, parseErr = opentype.Parse(droidTTF)
	})
	return parseErr
}

// typeface is one size of type: the Go face and the fallback behind it.
type typeface struct {
	main, back font.Face
	size       int
}

func newFace(f *opentype.Font, size int) font.Face {
	face, _ := opentype.NewFace(f, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
	return face
}

func face(f *opentype.Font, size int) typeface {
	return typeface{main: newFace(f, size), back: newFace(fallback, size), size: size}
}

// face is the face that has r: Go Sans first, the fallback behind it,
// nil when neither does.
func (t typeface) face(r rune) font.Face {
	if _, ok := t.main.GlyphAdvance(r); ok {
		return t.main
	}
	if _, ok := t.back.GlyphAdvance(r); ok {
		return t.back
	}
	return nil
}

func (t typeface) advance(r rune) fixed.Int26_6 {
	f := t.face(r)
	if f == nil {
		return 0
	}
	a, _ := f.GlyphAdvance(r)
	return a
}

func (t typeface) width(s string) (w fixed.Int26_6) {
	for _, r := range s {
		w += t.advance(r)
	}
	return w
}

// draw sets s with its baseline at y starting at x, a rune at a time
// so each may come from the face that has it.
func (t typeface) draw(dst draw.Image, x, y int, s string, col color.Color) {
	d := &font.Drawer{Dst: dst, Src: image.NewUniform(col), Dot: fixed.P(x, y)}
	for _, r := range s {
		f := t.face(r)
		if f == nil {
			continue
		}
		d.Face = f
		d.DrawString(string(r))
	}
}

// breaks says a line may break on either side of r: a CJK character
// stands alone where there are no spaces to break at.
func breaks(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r) || unicode.In(r, unicode.Po, unicode.Ps, unicode.Pe) && r > 0x2fff
}

// closing says r is a CJK mark no line may open with — a full stop, a
// comma, a closing bracket — so it stays with the character before it.
func closing(r rune) bool {
	return r > 0x2fff && (unicode.In(r, unicode.Pe, unicode.Pf) || strings.ContainsRune("。，、；：？！…", r))
}

// tokens splits a paragraph into the pieces a line is made of: a run
// of spaces, a word, or one CJK character.
func tokens(s string) []string {
	var out []string
	var cur []rune
	kind := -1 // 0 space, 1 word
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			if kind != 0 {
				flush()
			}
			kind = 0
			cur = append(cur, ' ')
		case breaks(r):
			flush()
			if closing(r) && len(out) > 0 && out[len(out)-1] != " " {
				out[len(out)-1] += string(r)
			} else {
				out = append(out, string(r))
			}
			kind = -1
		default:
			if kind != 1 {
				flush()
			}
			kind = 1
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// wrap sets text in at most max lines of width w, a word at a time
// where spaces separate words and a character at a time where a CJK
// run has none; a newline ends a line, a blank line is dropped. When
// the text runs past the last line, that line ends in an ellipsis.
func (t typeface) wrap(text string, w fixed.Int26_6, max int) []string {
	var lines []string
	cut := false
	push := func(line string) bool {
		line = strings.TrimRight(line, " ")
		if line == "" {
			return true
		}
		if len(lines) == max {
			cut = true
			return false
		}
		lines = append(lines, line)
		return true
	}
	for _, para := range strings.Split(text, "\n") {
		line := ""
		for _, tok := range tokens(para) {
			if tok == " " && line == "" {
				continue
			}
			if t.width(line+tok) <= w {
				line += tok
				continue
			}
			if line != "" && !push(line) {
				break
			}
			line = ""
			if tok == " " {
				continue
			}
			// a word wider than the line breaks where it must
			for _, r := range tok {
				if t.width(line+string(r)) > w && line != "" {
					if !push(line) {
						break
					}
					line = ""
				}
				line += string(r)
			}
		}
		if cut {
			break
		}
		if !push(line) {
			break
		}
	}
	if cut && len(lines) > 0 {
		last := []rune(strings.TrimRight(lines[len(lines)-1], " "))
		for len(last) > 0 && t.width(string(last)+"…") > w {
			last = last[:len(last)-1]
		}
		lines[len(lines)-1] = strings.TrimRight(string(last), " ") + "…"
	}
	return lines
}

func fill(dst draw.Image, r image.Rectangle, c color.Color) {
	draw.Draw(dst, r, image.NewUniform(c), image.Point{}, draw.Src)
}

// box draws a rectangle's n-pixel border inside r.
func box(dst draw.Image, r image.Rectangle, n int, c color.Color) {
	fill(dst, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+n), c)
	fill(dst, image.Rect(r.Min.X, r.Max.Y-n, r.Max.X, r.Max.Y), c)
	fill(dst, image.Rect(r.Min.X, r.Min.Y, r.Min.X+n, r.Max.Y), c)
	fill(dst, image.Rect(r.Max.X-n, r.Min.Y, r.Max.X, r.Max.Y), c)
}

// bevel is the inset highlight every Platinum box wears: n pixels of
// hi along the inner top and left, of lo along the inner bottom and
// right.
func bevel(dst draw.Image, r image.Rectangle, n int, hi, lo color.Color) {
	fill(dst, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+n), hi)
	fill(dst, image.Rect(r.Min.X, r.Min.Y, r.Min.X+n, r.Max.Y), hi)
	fill(dst, image.Rect(r.Min.X, r.Max.Y-n, r.Max.X, r.Max.Y), lo)
	fill(dst, image.Rect(r.Max.X-n, r.Min.Y, r.Max.X, r.Max.Y), lo)
}

// PNG draws the card.
func (c Card) PNG() ([]byte, error) {
	if err := load(); err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	c.draw(img)
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// u is the picture's scale: device pixels per CSS pixel. The page's
// chrome is laid out here in CSS pixels, as chrome.css lays it out,
// and every edge lands on a device pixel — the one half-pixel offset
// in the page, the stripes' centring in the bar, is a whole one at 2x.
const u = 2

// css is the device rectangle of a box given by its CSS-pixel edges.
func css(x0, y0, x1, y1 int) image.Rectangle {
	return image.Rect(x0*u, y0*u, x1*u, y1*u)
}

// snap is a device coordinate laid out at a fraction of a CSS pixel,
// moved to the whole CSS pixel the browser paints it at.
func snap(v float64) int {
	return int(math.Floor(v/u+0.5)) * u
}

// px paints one CSS pixel.
func px(dst draw.Image, x, y int, c color.Color) {
	fill(dst, css(x, y, x+1, y+1), c)
}

// the chrome's colours, as chrome.css names them
var (
	g700   = color.RGBA{0x80, 0x80, 0x80, 0xff} // the boxes' outer bevel and inner well edge
	w60dd  = color.RGBA{0xf1, 0xf1, 0xf1, 0xff} // white at 60% over the status strip's #ddd
	w60aa  = color.RGBA{0xdd, 0xdd, 0xdd, 0xff} // white at 60% over its #aaa edge: the strip's corners
	w50g6  = color.RGBA{0xcc, 0xcc, 0xcc, 0xff} // white at 50% over #999: the window bevel's corners
	sedge  = color.RGBA{0xaa, 0xaa, 0xaa, 0xff} // the status strip's bottom and right edge
	wellLo = 0x9a                               // the close box's well: a 135° gradient, #9a9a9a…
	wellHi = 0xf1                               // …to #f1f1f1 across its 9px square
)

// closeBox draws .tbox with its top-left corner at CSS (x, y): 13px
// square, #808080 along the outer top and left and white along the
// outer bottom and right, a 1px black ring inside that, a white then
// #808080 bevel inside the ring, and the 7×7 well between: the 135°
// gradient a 9px square wears centred in the box, of which the bevel
// leaves the middle 7 showing. At 2x the well's shade is computed per
// device pixel along the diagonal, as the browser computes it.
func closeBox(dst draw.Image, x, y int) {
	fill(dst, css(x, y, x+13, y+13), g700)      // outer top and left
	fill(dst, css(x+1, y+1, x+13, y+13), white) // outer bottom and right
	fill(dst, css(x+1, y+1, x+12, y+12), ink)   // the ring
	fill(dst, css(x+2, y+2, x+11, y+11), white) // inner top and left
	fill(dst, css(x+3, y+3, x+11, y+11), g700)  // inner bottom and right
	// the well: the gradient's square is CSS (x+2, y+2) to (x+11, y+11),
	// 9u device pixels a side, its line running corner to corner
	side := 9 * u
	for dy := 0; dy < 7*u; dy++ {
		for dx := 0; dx < 7*u; dx++ {
			// the pixel centre's way along the diagonal, in half pixels
			k := dx + u + dy + u + 1
			c := uint8(wellLo + ((wellHi-wellLo)*k+side)/(2*side))
			dst.Set((x+3)*u+dx, (y+3)*u+dy, color.RGBA{c, c, c, 0xff})
		}
	}
}

// stripe draws .titlebar .stripe between device columns x0 and x1,
// its top at device row y: 12 CSS px of 1px rows, white then #777,
// with a 1px column at the left of white then #ccc and at the right
// of #ccc then #777 (the ::before and ::after).
func stripe(dst draw.Image, x0, x1, y int) {
	for i := 0; i < 12*u; i++ {
		hi := (i/u)%2 == 0
		row, left, right := stripeDark, light4, stripeDark
		if hi {
			row, left, right = white, white, light4
		}
		fill(dst, image.Rect(x0, y+i, x1, y+i+1), row)
		fill(dst, image.Rect(x0, y+i, x0+u, y+i+1), left)
		fill(dst, image.Rect(x1-u, y+i, x1, y+i+1), right)
	}
}

var (
	stripeDark = color.RGBA{0x77, 0x77, 0x77, 0xff}
	light4     = color.RGBA{0xcc, 0xcc, 0xcc, 0xff} // --g400, the bar's own grey
)

// baseline is where a line of text's baseline goes when the browser
// centres the face's ascent and descent in a line box h device pixels
// tall whose top is at top: half the leading above, the ascent below.
func (t typeface) baseline(top, h int) int {
	m := t.main.Metrics()
	return top + (fixed.I(h)-(m.Ascent+m.Descent)).Round()/2 + m.Ascent.Round()
}

// draw is the page's window at 2x: chrome.css's window, title bar,
// frame and status strip laid out in CSS pixels and painted u device
// pixels to each, so the picture's chrome is the page's own, edge for
// edge; the card's content sits inside.
func (c Card) draw(img *image.RGBA) {
	fill(img, img.Bounds(), desk)

	// the window: 18px in from the picture's edges, a 1px #262626
	// border on #ccc, a hard shadow 2px down and right
	x0, y0, x1, y1 := 18, 18, W/u-18, H/u-18
	fill(img, css(x0+2, y0+2, x1+2, y1+2), ink)
	fill(img, css(x0, y0, x1, y1), desk)
	box(img, css(x0, y0, x1, y1), u, ink)

	// the title bar: 17px, padding 2px 4px, a 4px gap between its
	// parts — the close box, a stripe, the title on the bar's grey with
	// 2px of padding, a stripe. The stripes share the width the title
	// leaves, so their edges fall on half pixels, and the 12px stripes
	// sit half a pixel down the 13px row: the browser snaps every edge
	// to a whole CSS pixel, halves rounding up, so the picture does too.
	closeBox(img, x0+5, y0+3)
	title := face(bold, 12*u)
	bx0, bx1 := (x0+22)*u, (x1-5)*u // the bar's content past the close box and its gap
	maxTitle := (x1-x0-10)*u*7/10 - 4*u
	var tw int
	if l := title.wrap(c.Title, fixed.I(maxTitle), 1); len(l) > 0 {
		c.Title = l[0]
		tw = title.width(c.Title).Ceil()
	} else {
		c.Title = ""
	}
	tbox := float64(tw + 4*u) // the title's box: 2px padding either side
	half := (float64(bx1-bx0) - 8*u - tbox) / 2
	l1 := snap(float64(bx0) + half)
	t0 := snap(float64(bx0) + half + 4*u)
	r0 := snap(float64(bx0) + half + 4*u + tbox + 4*u)
	sy := (y0 + 4) * u
	stripe(img, bx0, l1, sy)
	stripe(img, r0, bx1, sy)
	title.draw(img, t0+2*u, title.baseline((y0+3)*u, 13*u), c.Title, ink)

	// the frame: margin 0 4px 4px, a 1px black border, white inside;
	// its #999 shadow 1px up and left, its white one 1px down and right
	fx0, fy0, fx1, fy1 := x0+5, y0+18, x1-5, y1-5
	fill(img, css(fx0-1, fy0-1, fx1-1, fy0), shade)
	fill(img, css(fx0-1, fy0-1, fx0, fy1-1), shade)
	fill(img, css(fx0+1, fy1, fx1+1, fy1+1), white)
	fill(img, css(fx1, fy0+1, fx1+1, fy1+1), white)
	fill(img, css(fx0, fy0, fx1, fy1), white)
	box(img, css(fx0, fy0, fx1, fy1), u, ink)
	ix0, iy0, ix1, iy1 := fx0+1, fy0+1, fx1-1, fy1-1

	// the status strip along the frame's foot: a 1px black rule, then
	// 22px of #ddd — 3px of padding round the 11px type's 16px line —
	// with white at 60% along its top and left, #aaa along its bottom
	// and right, the two blended where they meet
	sy0 := iy1 - 23
	fill(img, css(ix0, sy0, ix1, sy0+1), ink)
	fill(img, css(ix0, sy0+1, ix1, iy1), light)
	fill(img, css(ix0, sy0+1, ix1, sy0+2), w60dd)
	fill(img, css(ix0, sy0+1, ix0+1, iy1), w60dd)
	fill(img, css(ix0, iy1-1, ix1, iy1), sedge)
	fill(img, css(ix1-1, sy0+1, ix1, iy1), sedge)
	px(img, ix1-1, sy0+1, w60aa)
	px(img, ix0, iy1-1, w60aa)
	foot := face(regular, 11*u)
	base := foot.baseline((sy0+4)*u, 16*u)
	foot.draw(img, (ix0+8)*u, base, c.Foot, dim)
	if c.Right != "" {
		foot.draw(img, (ix1-8)*u-foot.width(c.Right).Ceil(), base, c.Right, dim)
	}

	// the content: the picture beside the name and its line, the words
	// below, all inside a margin
	const pad = 40
	area := image.Rect(ix0*u+pad, iy0*u+28, ix1*u-pad, sy0*u-24)
	x := area.Min.X
	const pic = Pic
	if c.Picture != nil {
		dst := image.NewRGBA(image.Rect(0, 0, pic, pic))
		var scaler xdraw.Scaler = xdraw.CatmullRom
		if c.Crisp {
			scaler = xdraw.NearestNeighbor
		}
		scaler.Scale(dst, dst.Bounds(), c.Picture, c.Picture.Bounds(), xdraw.Src, nil)
		at := image.Rect(x, area.Min.Y, x+pic+2*u, area.Min.Y+pic+2*u)
		fill(img, at, light)
		draw.Draw(img, at.Inset(u), dst, image.Point{}, draw.Over)
		box(img, at, u, ink)
		x += pic + 2*u + 24
	}
	name := face(bold, 40)
	sub := face(regular, 26)
	nameLines := name.wrap(c.Name, fixed.I(area.Max.X-x), 1)
	if len(nameLines) > 0 {
		name.draw(img, x, area.Min.Y+40, nameLines[0], ink)
	}
	if c.Sub != "" {
		if l := sub.wrap(c.Sub, fixed.I(area.Max.X-x), 1); len(l) > 0 {
			sub.draw(img, x, area.Min.Y+84, l[0], grey)
		}
	}
	body := face(regular, 36)
	const lineH = 48
	top := area.Min.Y + pic + 2*u + 20
	n := (area.Max.Y - top + 14) / lineH
	if n < 1 {
		n = 1
	}
	for i, line := range body.wrap(c.Body, fixed.I(area.Dx()), n) {
		body.draw(img, area.Min.X, top+36+i*lineH, line, ink)
	}

	// the window's inset bevel, over everything as the page's is
	// (.window::before): white along the inner top and left, #999 along
	// the inner bottom and right, and at the two corners where they
	// meet the white at half strength over the #999
	fill(img, css(x0+1, y0+1, x1-1, y0+2), white)
	fill(img, css(x0+1, y0+1, x0+2, y1-1), white)
	fill(img, css(x1-2, y0+1, x1-1, y1-1), shade)
	fill(img, css(x0+1, y1-2, x1-1, y1-1), shade)
	px(img, x1-2, y0+1, w50g6)
	px(img, x0+1, y1-2, w50g6)
}

// Decode reads a picture for a card: whatever the store holds (PNG,
// JPEG, GIF, WebP).
func Decode(b []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	return img, err
}
