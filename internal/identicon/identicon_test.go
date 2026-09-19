package identicon

import (
	"encoding/json"
	"image/color"
	"os"
	"strings"
	"testing"
)

// The fixture is what the Hub app's identicon() drew for each id
// (testdata/draw.js runs the app's own source); a face here must match.
func TestFaceMatchesTheHubApp(t *testing.T) {
	raw, err := os.ReadFile("testdata/identicon.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID, Ground, Ink string
		Rows            [N]string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty fixture")
	}
	for _, c := range cases {
		ink, _ := Face(c.ID)
		if got := hexColor(ink); got != c.Ink {
			t.Errorf("%s: ink %s, the app draws %s", c.ID, got, c.Ink)
		}
		if got := hexColor(Ground); got != c.Ground {
			t.Errorf("%s: ground %s, the app draws %s", c.ID, got, c.Ground)
		}
		if got := Rows(c.ID); got != c.Rows {
			t.Errorf("%s: rows %v, the app draws %v", c.ID, got, c.Rows)
		}
	}
}

func TestValid(t *testing.T) {
	for id, want := range map[string]bool{
		"fa0fd0d0cbc2e8d1":  true,
		"FA0FD0D0CBC2E8D1":  false,
		"fa0fd0d0cbc2e8d":   false,
		"fa0fd0d0cbc2e8d1a": false,
		"fa0fd0d0cbc2e8dg":  false,
		"":                  false,
	} {
		if Valid(id) != want {
			t.Errorf("Valid(%q) = %v", id, !want)
		}
	}
}

func TestSVG(t *testing.T) {
	s := string(SVG("44314766ad285c2a")) // ..#.. / #.#.# / ..... / #...# / #...#
	for _, want := range []string{
		`viewBox="0 0 5 5"`, `shape-rendering="crispEdges"`,
		`fill="#e6e6f5" d="M0 0h5v5h-5z"`,
		`fill="#333399" d="M2 0h1v1h-1zM0 1h1v1h-1zM2 1h1v1h-1zM4 1h1v1h-1zM0 3h1v1h-1zM4 3h1v1h-1zM0 4h1v1h-1zM4 4h1v1h-1z"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("SVG lacks %s:\n%s", want, s)
		}
	}
}

// Every box the pages and the cards draw a face in: whole cells, the same
// whole margin on both sides, the pattern about a cell clear of the edge.
func TestCell(t *testing.T) {
	for px, want := range map[int]int{14: 2, 16: 2, 18: 2, 30: 4, 46: 6, 62: 8, 96: 12} {
		c := Cell(px)
		if c != want {
			t.Errorf("Cell(%d) = %d, want %d", px, c, want)
		}
		if (px-N*c)%2 != 0 {
			t.Errorf("Cell(%d) = %d leaves an uneven margin", px, c)
		}
	}
}

func TestImage(t *testing.T) {
	const px = 96
	img := Image("44314766ad285c2a", px)
	if img.Bounds().Dx() != px || img.Bounds().Dy() != px {
		t.Fatalf("bounds %v", img.Bounds())
	}
	c := Cell(px)
	m := (px - N*c) / 2
	ink, on := Face("44314766ad285c2a")
	at := func(x, y int) color.RGBA {
		r, g, b, a := img.At(x, y).RGBA()
		return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
	}
	if at(0, 0) != Ground || at(m-1, m-1) != Ground || at(px-1, px-1) != Ground {
		t.Error("the margin is not the ground")
	}
	for y := range on {
		for x, set := range on[y] {
			want := Ground
			if set {
				want = ink
			}
			// both corners of the cell
			if at(m+x*c, m+y*c) != want || at(m+(x+1)*c-1, m+(y+1)*c-1) != want {
				t.Errorf("cell %d,%d is not %v", x, y, want)
			}
		}
	}
}
