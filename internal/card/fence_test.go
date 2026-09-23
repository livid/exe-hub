package card

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// the cases the Hub app's fenceAt and plainWords are run against too
// (~/tools/playwright/exe-hub-fence-test.js reads this file)
func TestFenceAt(t *testing.T) {
	raw, err := os.ReadFile("testdata/fences.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text, Info, Code, Plain string
		N                             int
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	words := func(s string) string { return Unbold(Unlink(Unlist(Untable(s)))) }
	for _, c := range cases {
		f, n := FenceAt(strings.Split(c.Text, "\n"), 0)
		if n != c.N || (f == nil) != (c.N == 0) {
			t.Errorf("%s: took %d lines (fence %v), want %d", c.Name, n, f != nil, c.N)
			continue
		}
		if f != nil && (f.Info != c.Info || f.Code != c.Code) {
			t.Errorf("%s:\n got %q %q\nwant %q %q", c.Name, f.Info, f.Code, c.Info, c.Code)
		}
		if got := strings.Join(strings.Fields(Unfence(c.Text, words)), " "); got != c.Plain {
			t.Errorf("%s: plain\n got %q\nwant %q", c.Name, got, c.Plain)
		}
	}
}
