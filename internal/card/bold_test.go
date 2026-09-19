package card

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// BoldCase is one case of testdata/bold.json: the bold stretches a
// renderer must set (Strong, each one's words as they read), the links'
// words where the case has any, and the text as plain words (Plain;
// empty where the case says nothing of it). The hub's renderText
// (internal/api) and the Hub app's formatText and plainWords
// (~/tools/playwright/exe-hub-bold-test.js reads this file) are run
// against the same cases.
type BoldCase struct {
	Name, Text, Plain string
	Strong, Links     []string
}

func BoldCases(t *testing.T, path string) []BoldCase {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []BoldCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestUnbold(t *testing.T) {
	for _, c := range BoldCases(t, "testdata/bold.json") {
		if c.Plain == "" {
			continue
		}
		if got := strings.Join(strings.Fields(Unbold(Unlink(Untable(c.Text)))), " "); got != c.Plain {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, c.Plain)
		}
	}
}
