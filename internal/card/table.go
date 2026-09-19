package card

import (
	"regexp"
	"strings"
)

// Table is a Markdown table as a post writes it — GFM's pipe table: a
// header row, a delimiter row of dashes that may carry a colon at either
// end, and the rows under it. Align has one entry per column: "" (as it
// falls), "l", "c" or "r". Every row has exactly the header's cells —
// a short row is filled with empty cells and a long one cut, as GFM
// does. The cells are the post's own words, still to be escaped and set
// by the inline pipeline; an escaped pipe, \|, is already a plain | in
// them (in a code span too, as in GFM).
type Table struct {
	Align []string
	Head  []string
	Rows  [][]string
}

var (
	tableRule    = regexp.MustCompile(`^:?-+:?$`)
	tableHeading = regexp.MustCompile(`^#{1,3} +\S`)
)

// TableAt reads the table that begins at lines[i], and says how many
// lines it takes; nil when no table begins there. The header needs a
// pipe, the delimiter row a pipe of its own and as many cells as the
// header — anything looser stays the text it was. The table runs to the
// first line that is blank, has no pipe, or is a heading: prose set
// right under a table is not a row of it (stricter than GFM, which
// takes any line up to a blank one, since posts are written tight).
// The Hub app's tableAt reads the same way; testdata/tables.json holds
// the cases both are run against.
func TableAt(lines []string, i int) (*Table, int) {
	if i+1 >= len(lines) {
		return nil, 0
	}
	head, ok := tableCells(lines[i])
	if !ok || tableHeading.MatchString(lines[i]) {
		return nil, 0
	}
	rule, ok := tableCells(lines[i+1])
	if !ok || len(rule) != len(head) {
		return nil, 0
	}
	t := &Table{Head: head, Align: make([]string, len(rule)), Rows: [][]string{}}
	for k, c := range rule {
		if !tableRule.MatchString(c) {
			return nil, 0
		}
		l, r := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case l && r:
			t.Align[k] = "c"
		case r:
			t.Align[k] = "r"
		case l:
			t.Align[k] = "l"
		}
	}
	j := i + 2
	for ; j < len(lines); j++ {
		cells, ok := tableCells(lines[j])
		if !ok || strings.TrimSpace(lines[j]) == "" || tableHeading.MatchString(lines[j]) {
			break
		}
		row := make([]string, len(head))
		copy(row, cells)
		t.Rows = append(t.Rows, row)
	}
	return t, j - i
}

// tableCells cuts a line at its pipes, and says whether it had one: the
// pipe that opens the line and the one that closes it are the table's
// frame, not cells; \| is a pipe inside a cell; each cell is trimmed.
func tableCells(line string) ([]string, bool) {
	var cells []string
	var cur strings.Builder
	piped := false
	for k := 0; k < len(line); k++ {
		switch {
		case line[k] == '\\' && k+1 < len(line) && line[k+1] == '|':
			cur.WriteByte('|')
			k++
		case line[k] == '|':
			piped = true
			cells = append(cells, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(line[k])
		}
	}
	cells = append(cells, cur.String())
	if !piped {
		return nil, false
	}
	if strings.TrimSpace(cells[0]) == "" {
		cells = cells[1:]
	}
	if n := len(cells); n > 0 && strings.TrimSpace(cells[n-1]) == "" {
		cells = cells[:n-1]
	}
	for k := range cells {
		cells[k] = strings.TrimSpace(cells[k])
	}
	return cells, len(cells) > 0
}

// Untable is text with every table put back to words, for the places
// that show a post's words plain — an excerpt, a title, a preview
// picture, a notification: the delimiter row goes, and each row's cells
// stand on one line with " · " between them, the empty ones left out.
func Untable(text string) string {
	if !strings.Contains(text, "|") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		t, n := TableAt(lines, i)
		if t == nil {
			out = append(out, lines[i])
			continue
		}
		for _, row := range append([][]string{t.Head}, t.Rows...) {
			var words []string
			for _, c := range row {
				if c != "" {
					words = append(words, c)
				}
			}
			out = append(out, strings.Join(words, " · "))
		}
		i += n - 1
	}
	return strings.Join(out, "\n")
}
