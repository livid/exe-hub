package card

import (
	"strings"

	"exehub/internal/mention"
)

// Mentions is the mentions of a run of words that show as mentions, as
// [from, to] pairs: mention.At's, less those a code span, a Markdown
// link or a bare URL holds — `@0123456789abcdef` in backticks is code,
// and a link keeps its words and its address whole. The Hub app's
// mentionsOf reads the same way; testdata/mentions.json holds the cases
// both are run against.
func Mentions(text string) [][]int {
	at := mention.At(text)
	if len(at) == 0 {
		return nil
	}
	claims := Code.FindAllStringIndex(text, -1)
	claims = append(claims, Link.FindAllStringIndex(text, -1)...)
	claims = append(claims, URL.FindAllStringIndex(text, -1)...)
	var out [][]int
	for _, m := range at {
		free := true
		for _, c := range claims {
			if c[0] < m[1] && c[1] > m[0] {
				free = false
				break
			}
		}
		if free {
			out = append(out, m)
		}
	}
	return out
}

// NameMentions is text with each mention that shows set as "@" and the
// name its profile goes by today, for where no markup shows: an excerpt,
// a title, a notification. An id names holds no name for stays as it
// was written.
func NameMentions(text string, names map[string]string) string {
	if len(names) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range Mentions(text) {
		name := names[text[m[0]+1:m[1]]]
		if name == "" {
			continue
		}
		b.WriteString(text[last:m[0]])
		b.WriteString("@" + name)
		last = m[1]
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}
