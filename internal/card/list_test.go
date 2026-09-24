package card

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// the cases the Hub app's listAt and plainWords are run against too
// (~/tools/playwright/exe-hub-list-test.js reads this file)
func TestListAt(t *testing.T) {
	raw, err := os.ReadFile("testdata/lists.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text, Plain string
		N, Start          int
		Ordered           bool
		Items, Boxes      []string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		l, n := ListAt(strings.Split(c.Text, "\n"), 0)
		if n != c.N || (l == nil) != (c.N == 0) {
			t.Errorf("%s: took %d lines (list %v), want %d", c.Name, n, l != nil, c.N)
			continue
		}
		if l != nil && (l.Ordered != c.Ordered || l.Start != c.Start || !reflect.DeepEqual(l.Items, c.Items) || !reflect.DeepEqual(l.Boxes, c.Boxes)) {
			t.Errorf("%s:\n got %v %d %q boxes %q\nwant %v %d %q boxes %q", c.Name, l.Ordered, l.Start, l.Items, l.Boxes, c.Ordered, c.Start, c.Items, c.Boxes)
		}
		if c.Plain == "" {
			continue
		}
		if got := strings.Join(strings.Fields(Unbold(Unlink(Unlist(Untable(c.Text))))), " "); got != c.Plain {
			t.Errorf("%s: plain\n got %q\nwant %q", c.Name, got, c.Plain)
		}
	}
}

// Boxes counts a text's boxes the way a page draws them: across every
// list, a fenced "- [ ]" not among them, a numbered item's brackets
// no box
func TestBoxes(t *testing.T) {
	for _, c := range []struct {
		text string
		n    int
	}{
		{"- [ ] a\n- [x] b", 2},
		{"- [ ] a\n- b\n\nwords\n- [X] c", 2},
		{"```\n- [ ] code\n```\n- [ ] a", 1},
		{"1. [ ] a", 0},
		{"plain - [ ] words", 0},
		{"", 0},
	} {
		if got := Boxes(c.text); got != c.n {
			t.Errorf("Boxes(%q) = %d, want %d", c.text, got, c.n)
		}
	}
}
