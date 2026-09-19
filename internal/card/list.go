package card

import (
	"regexp"
	"strconv"
	"strings"
)

// List is a Markdown list as a post writes it: a run of item lines, one
// item a line. A bulleted item is "- " or "* " and its words; a numbered
// one is up to three digits, "." or ")", a space and its words — three
// digits and no more, so "2026. A year" stays prose. Start is a numbered
// list's first number: the list counts on from there whatever the lines
// under it say, as in Markdown, so "3." alone is item three. The items
// are the post's own words, still to be escaped and set by the inline
// pipeline.
type List struct {
	Ordered bool
	Start   int
	Items   []string
}

var (
	listBullet = regexp.MustCompile(`^[-*] +(\S.*?) *$`)
	listNumber = regexp.MustCompile(`^(\d{1,3})[.)] +(\S.*?) *$`)
)

// listItem reads one line as an item: its kind, its number when it has
// one, and its words.
func listItem(line string) (ordered bool, num int, words string, ok bool) {
	if m := listBullet.FindStringSubmatch(line); m != nil {
		return false, 0, m[1], true
	}
	if m := listNumber.FindStringSubmatch(line); m != nil {
		n, _ := strconv.Atoi(m[1])
		return true, n, m[2], true
	}
	return false, 0, "", false
}

// ListAt reads the list that begins at lines[i], and says how many lines
// it takes; nil when no list begins there. The marker stands at the
// start of the line with a space after it, so "**bold**", "*word*",
// "-5" and "--" are no items. The list runs while the lines are items
// of its own kind: a blank line or a line of prose ends it, as a table
// ends (posts are written tight, and nothing here is hard-wrapped, so an
// item has no continuation lines and there is no nesting), and a
// numbered line under bullets begins a list of its own. The Hub app's
// listAt reads the same way; testdata/lists.json holds the cases both
// are run against.
func ListAt(lines []string, i int) (*List, int) {
	ordered, num, words, ok := listItem(lines[i])
	if !ok {
		return nil, 0
	}
	l := &List{Ordered: ordered, Start: num, Items: []string{words}}
	n := 1
	for ; i+n < len(lines); n++ {
		o, _, w, ok := listItem(lines[i+n])
		if !ok || o != ordered {
			break
		}
		l.Items = append(l.Items, w)
	}
	return l, n
}

// Unlist is text with every bulleted item's marker put to a bullet, "•",
// for the places that show a post's words plain and run its lines
// together — an excerpt, a title, a preview picture, a notification:
// "Three things: • one • two" reads, where "- one - two" would not. A
// numbered item already says what it is and stays as typed.
func Unlist(text string) string {
	if !strings.Contains(text, "- ") && !strings.Contains(text, "* ") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if m := listBullet.FindStringSubmatch(line); m != nil {
			lines[i] = "• " + m[1]
		}
	}
	return strings.Join(lines, "\n")
}
