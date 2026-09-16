package preview

import (
	"bytes"
	"image/png"
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
