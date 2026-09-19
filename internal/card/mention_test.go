package card

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// MentionCase is one case of testdata/mentions.json: the names a post's
// mentions show under, in order, and its words with them set plain. The
// page's renderer (api) and the Hub app's (exe-hub-mention-test.js) are
// run against the same file.
type MentionCase struct {
	Name     string            `json:"name"`
	Text     string            `json:"text"`
	Names    map[string]string `json:"names"`
	Mentions []string          `json:"mentions"`
	Plain    string            `json:"plain"`
}

func MentionCases(t *testing.T, path string) []MentionCase {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []MentionCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	return cases
}

func TestMentions(t *testing.T) {
	for _, c := range MentionCases(t, "testdata/mentions.json") {
		got := []string{}
		for _, m := range Mentions(c.Text) {
			if name := c.Names[c.Text[m[0]+1:m[1]]]; name != "" {
				got = append(got, name)
			}
		}
		if !slices.Equal(got, c.Mentions) {
			t.Errorf("%s: mentions %q, want %q", c.Name, got, c.Mentions)
		}
	}
}

func TestNameMentionsLeavesTheRest(t *testing.T) {
	names := map[string]string{"0123456789abcdef": "Livid"}
	for text, want := range map[string]string{
		"no mention at all":                     "no mention at all",
		"a@b.c and @0123456789abcdef":           "a@b.c and @Livid",
		"`@0123456789abcdef` @0123456789abcdef": "`@0123456789abcdef` @Livid",
	} {
		if got := NameMentions(text, names); got != want {
			t.Errorf("NameMentions(%q) = %q, want %q", text, got, want)
		}
	}
	if got := NameMentions("@0123456789abcdef", nil); got != "@0123456789abcdef" {
		t.Errorf("without names: %q", got)
	}
}
