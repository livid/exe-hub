package lang

import (
	"regexp"
	"strings"
	"unicode"

	"exehub/internal/card"
)

// the half-width marks Chinese sets full-width, and the ones Japanese
// does: Japanese has its own comma, 、, which the model writes itself,
// so a half-width comma against Japanese is left as it is
var (
	fullWidth   = map[rune]rune{',': '，', ';': '；', ':': '：', '!': '！', '?': '？'}
	fullWidthJa = map[rune]rune{';': '；', ':': '：', '!': '！', '?': '？'}
)

// Tidy is the punctuation rule for a translation into to: FullWidth for
// Chinese, FullWidthJa for Japanese, and the text as it is for any
// other language.
func Tidy(to, text string) string {
	switch {
	case strings.HasPrefix(to, "zh"):
		return FullWidth(text)
	case to == "ja":
		return FullWidthJa(text)
	}
	return text
}

// FullWidthJa is FullWidth for a Japanese translation (2026-09-22): the
// model's habit there is one mark, a half-width colon set straight
// against Japanese — "第 2 層:黙って", "影響なし:onmessage", 13 in 6 of
// the first 22 kept — so : ; ! ? become full-width when Han or kana
// stands directly before or after them, or after the one space; the
// comma is left alone, since Japanese sets its own, 、, and the model
// does.
func FullWidthJa(text string) string {
	return fullWidthWith(text, fullWidthJa, func(r rune) bool {
		return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r)
	})
}

// FullWidth sets a Chinese translation's punctuation by rule (see
// PLAN.md, Translations). The model's one habit is to stay in ASCII for
// one more character after a code span, a link or a Latin word —
// "`zh`,66 次", "true;在中文前" — so a half-width , ; : ! ? becomes the
// full-width mark when a Han character stands directly before it,
// directly after it, or after the one space it needed; and a , or ;
// jammed between a code span or a link and whatever follows does too,
// Chinese or not, since English never sets one so. The space after a
// mark that turns goes with it, a full-width mark carrying its own.
// Code spans and links, as the pages find them, are never touched, and
// a mark with no Chinese against it is left as it is: 10,000, 3:30, a
// table's :---, a,b between Latin words, which may be a literal, and
// the punctuation of an English phrase quoted inside the post. A :)
// stays one. The rule is its own fixed point: FullWidth(FullWidth(s))
// is FullWidth(s).
func FullWidth(text string) string {
	return fullWidthWith(text, fullWidth, func(r rune) bool { return unicode.Is(unicode.Han, r) })
}

// fullWidthWith is the rule with its marks and its script given: marks
// is the half-width mark to the full-width one, and script says a rune
// is of the language, the one a mark turns against.
func fullWidthWith(text string, marks map[rune]rune, script func(rune) bool) string {
	kept := make([]bool, len(text)) // the bytes of code spans and links
	for _, re := range []*regexp.Regexp{card.Code, card.URL} {
		for _, m := range re.FindAllStringIndex(text, -1) {
			for i := m[0]; i < m[1]; i++ {
				kept[i] = true
			}
		}
	}
	type at struct {
		r    rune
		kept bool
	}
	rs := make([]at, 0, len(text))
	for i, r := range text {
		rs = append(rs, at{r, kept[i]})
	}
	han := func(i int) bool { return i >= 0 && i < len(rs) && script(rs[i].r) }
	space := func(i int) bool { return i < len(rs) && rs[i].r == ' ' && !rs[i].kept }
	// turns says the mark at i is Chinese punctuation typed in ASCII
	turns := func(i int) bool {
		if han(i-1) || han(i+1) || (space(i+1) && han(i+2)) {
			return true
		}
		jammed := i+1 < len(rs) && !unicode.IsSpace(rs[i+1].r)
		return (rs[i].r == ',' || rs[i].r == ';') && i > 0 && rs[i-1].kept && jammed
	}
	var b strings.Builder
	b.Grow(len(text) + 16)
	for i := 0; i < len(rs); i++ {
		wide, mark := marks[rs[i].r]
		if !mark || rs[i].kept || !turns(i) {
			b.WriteRune(rs[i].r)
			continue
		}
		if (rs[i].r == ':' || rs[i].r == ';') && i+1 < len(rs) && strings.ContainsRune(")(-", rs[i+1].r) {
			b.WriteRune(rs[i].r) // a face, not a colon
			continue
		}
		b.WriteRune(wide)
		if space(i + 1) {
			i++
		}
	}
	return b.String()
}
