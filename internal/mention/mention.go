// Package mention reads the mentions a post's text holds (see PLAN.md,
// Mentions). A mention is "@" and a profile id — the 16 hex characters
// of a key's fingerprint, the one thing about a writer that never
// changes — and the name it shows under is looked up when the post is
// drawn, so a changed nickname shows in every post ever written.
//
// The package imports nothing of the hub's, so the store may use it.
package mention

import "regexp"

var token = regexp.MustCompile(`@[0-9a-f]{16}`)

// word says c is a letter, a digit or "_" of ASCII: what may not stand
// on either side of a mention. "mail@0123456789abcdef" is an address and
// "@0123456789abcdef0" a longer run of hex, and neither is a mention;
// anything else may touch one — "你好@…" is how Chinese is written.
func word(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// At is the mentions of s as [from, to] pairs, the id s[from+1:to]. It
// knows nothing of code spans or links: card.Mentions leaves out the
// ones those hold.
func At(s string) [][]int {
	var out [][]int
	for _, m := range token.FindAllStringIndex(s, -1) {
		if m[0] > 0 && word(s[m[0]-1]) {
			continue
		}
		if m[1] < len(s) && word(s[m[1]]) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// IDs is the profile ids texts mention, each once, in the order they
// come — every token, a code span's too: it is what to look names up
// for, and a name nobody shows costs nothing.
func IDs(texts ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, text := range texts {
		for _, m := range At(text) {
			if id := text[m[0]+1 : m[1]]; !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}
