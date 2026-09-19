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
		Items             []string
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
		if l != nil && (l.Ordered != c.Ordered || l.Start != c.Start || !reflect.DeepEqual(l.Items, c.Items)) {
			t.Errorf("%s:\n got %v %d %q\nwant %v %d %q", c.Name, l.Ordered, l.Start, l.Items, c.Ordered, c.Start, c.Items)
		}
		if c.Plain == "" {
			continue
		}
		if got := strings.Join(strings.Fields(Unbold(Unlink(Unlist(Untable(c.Text))))), " "); got != c.Plain {
			t.Errorf("%s: plain\n got %q\nwant %q", c.Name, got, c.Plain)
		}
	}
}
