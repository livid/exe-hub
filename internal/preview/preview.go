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
	desk   = color.RGBA{0xcc, 0xcc, 0xcc, 0xff}
	ink    = color.RGBA{0x26, 0x26, 0x26, 0xff}
	white  = color.RGBA{0xff, 0xff, 0xff, 0xff}
	light  = color.RGBA{0xdd, 0xdd, 0xdd, 0xff}
	shade  = color.RGBA{0x99, 0x99, 0x99, 0xff}
	stripe = color.RGBA{0x77, 0x77, 0x77, 0xff}
	grey   = color.RGBA{0x66, 0x66, 0x66, 0xff}
	dim    = color.RGBA{0x33, 0x33, 0x33, 0xff}
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

// draw is the page's window at 2x: the CSS's 1px lines are 2px here,
// its 2px hard shadow 4px, the title bar's 1px stripes 2px.
func (c Card) draw(img *image.RGBA) {
	const u = 2 // one CSS pixel
	fill(img, img.Bounds(), desk)

	// the window: hard shadow, border, inset bevel
	win := image.Rect(36, 36, W-36, H-36)
	fill(img, win.Add(image.Pt(2*u, 2*u)), ink)
	fill(img, win, desk)
	box(img, win, u, ink)
	in := win.Inset(u)
	bevel(img, in, u, white, shade)

	// the title bar: padding 2px 4px, the close box, the stripes, the
	// title on its own light box between them
	title := face(bold, 26)
	barH := 22 * u
	bar := image.Rect(in.Min.X+4*u, in.Min.Y+2*u, in.Max.X-4*u, in.Min.Y+2*u+barH)
	stripes := image.Rect(bar.Min.X, bar.Min.Y+(barH-12*u)/2, bar.Max.X, bar.Min.Y+(barH-12*u)/2+12*u)
	fill(img, stripes, light)
	for y := stripes.Min.Y; y < stripes.Max.Y; y += 2 * u {
		fill(img, image.Rect(stripes.Min.X, y, stripes.Max.X, y+u), white)
		fill(img, image.Rect(stripes.Min.X, y+u, stripes.Max.X, y+2*u), stripe)
	}
	tbox := image.Rect(bar.Min.X, bar.Min.Y+(barH-11*u)/2, bar.Min.X+11*u, bar.Min.Y+(barH-11*u)/2+11*u)
	fill(img, tbox, light)
	box(img, tbox, u, ink)
	bevel(img, tbox.Inset(u), u, white, color.RGBA{0x88, 0x88, 0x88, 0xff})
	tw := title.width(c.Title).Ceil()
	tx := (bar.Min.X+bar.Max.X)/2 - tw/2
	fill(img, image.Rect(tx-4*u, stripes.Min.Y, tx+tw+4*u, stripes.Max.Y), light)
	title.draw(img, tx, stripes.Min.Y+(12*u+18)/2, c.Title, ink)

	// the sunken white frame under the bar: margin 0 4px 4px
	frame := image.Rect(in.Min.X+4*u, bar.Max.Y+2*u, in.Max.X-4*u, in.Max.Y-4*u)
	fill(img, image.Rect(frame.Min.X-1*u, frame.Min.Y-1*u, frame.Max.X, frame.Max.Y), shade)
	fill(img, image.Rect(frame.Min.X, frame.Min.Y, frame.Max.X+1*u, frame.Max.Y+1*u), white)
	fill(img, frame, white)
	box(img, frame, u, ink)
	inner := frame.Inset(u)

	// the status bar along the frame's foot
	foot := face(regular, 22)
	statusH := 15 * u
	status := image.Rect(inner.Min.X, inner.Max.Y-statusH, inner.Max.X, inner.Max.Y)
	fill(img, status, light)
	fill(img, image.Rect(status.Min.X, status.Min.Y, status.Max.X, status.Min.Y+u), ink)
	bevel(img, image.Rect(status.Min.X, status.Min.Y+u, status.Max.X, status.Max.Y), u, color.RGBA{0xf3, 0xf3, 0xf3, 0xff}, color.RGBA{0xaa, 0xaa, 0xaa, 0xff})
	base := status.Min.Y + u + (statusH-u+16)/2
	foot.draw(img, status.Min.X+8*u, base, c.Foot, dim)
	if c.Right != "" {
		foot.draw(img, status.Max.X-8*u-foot.width(c.Right).Ceil(), base, c.Right, dim)
	}

	// the content: the picture beside the name and its line, the words
	// below, all inside a margin
	const pad = 40
	area := image.Rect(inner.Min.X+pad, inner.Min.Y+28, inner.Max.X-pad, status.Min.Y-24)
	x := area.Min.X
	const pic = 96
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
}

// Decode reads a picture for a card: whatever the store holds (PNG,
// JPEG, GIF, WebP).
func Decode(b []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	return img, err
}
