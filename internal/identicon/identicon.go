// Package identicon draws the face of a profile that has no picture of
// its own: a 5×5 pattern, mirrored left to right, in one of four Platinum
// blues on a pale ground, all of it read from the profile id.
//
// It is the twin of identicon() in the exe Hub app
// (exe: internal/server/sysapps/hub/index.html), so one person wears one
// face in the app and on the pages; testdata/identicon.json holds faces
// the app's own function drew, and the test here must draw the same.
package identicon

import (
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
)

// N is the pattern's side, in cells.
const N = 5

// Ground is the pale tile behind the pattern; the pages' stylesheet says
// the same colour where it pads the pattern out to a box.
var Ground = color.RGBA{0xe6, 0xe6, 0xf5, 0xff}

var inks = [4]color.RGBA{
	{0x33, 0x33, 0x99, 0xff},
	{0x66, 0x66, 0xcc, 0xff},
	{0x99, 0x99, 0xff, 0xff},
	{0x33, 0x66, 0x99, 0xff},
}

// Valid reports whether id is a profile id: 16 lowercase hex digits.
func Valid(id string) bool {
	if len(id) != 16 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// Face is the pattern for id: the ink, and the cells that wear it,
// on[y][x]. The id's first byte picks the ink; its first fifteen digits,
// odd ones, fill the left three columns row by row, and the right two
// mirror the left two. An id that is not Valid has no cells.
func Face(id string) (ink color.RGBA, on [N][N]bool) {
	if !Valid(id) {
		return inks[0], on
	}
	b, _ := hex.DecodeString(id[:2])
	ink = inks[int(b[0])%len(inks)]
	for i := 0; i < 15; i++ {
		d, _ := hex.DecodeString("0" + id[i:i+1])
		if d[0]%2 == 1 {
			x, y := i%3, i/3
			on[y][x] = true
			on[y][N-1-x] = true
		}
	}
	return ink, on
}

// Rows is the pattern as five strings of '#' and '.', what the tests and
// the fixture compare.
func Rows(id string) [N]string {
	_, on := Face(id)
	var rows [N]string
	for y := range on {
		var b strings.Builder
		for _, c := range on[y] {
			if c {
				b.WriteByte('#')
			} else {
				b.WriteByte('.')
			}
		}
		rows[y] = b.String()
	}
	return rows
}

func hexColor(c color.RGBA) string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

// SVG is the bare pattern, five units a side with crisp edges: whatever
// shows it gives it a whole number of pixels a cell and pads the rest of
// its box with the ground.
func SVG(id string) []byte {
	ink, on := Face(id)
	var d strings.Builder
	for y := range on {
		for x, c := range on[y] {
			if c {
				fmt.Fprintf(&d, "M%d %dh1v1h-1z", x, y)
			}
		}
	}
	return []byte(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><path fill="%s" d="M0 0h%dv%dh-%dz"/><path fill="%s" d="%s"/></svg>`,
		N, N, hexColor(Ground), N, N, N, hexColor(ink), d.String()))
}

// Cell is the side of one cell in a box px wide: about a seventh of it,
// so the pattern stands a cell clear of the box's edge, and the largest
// such that leaves the same whole margin on both sides.
func Cell(px int) int {
	for c := px / (N + 2); c > 1; c-- {
		if (px-N*c)%2 == 0 {
			return c
		}
	}
	return 1
}

// Image is the face as a px-square picture, the pattern centred on the
// ground in whole cells.
func Image(id string, px int) image.Image {
	ink, on := Face(id)
	img := image.NewRGBA(image.Rect(0, 0, px, px))
	draw.Draw(img, img.Bounds(), image.NewUniform(Ground), image.Point{}, draw.Src)
	c := Cell(px)
	m := (px - N*c) / 2
	for y := range on {
		for x, set := range on[y] {
			if set {
				r := image.Rect(m+x*c, m+y*c, m+(x+1)*c, m+(y+1)*c)
				draw.Draw(img, r, image.NewUniform(ink), image.Point{}, draw.Src)
			}
		}
	}
	return img
}
