package card

import (
	"regexp"
	"strings"
)

// Fence is a fenced code block as a post writes it: a line of three
// backticks opens it and the next line of three backticks alone closes
// it, and the lines between are the code, kept as typed — spaces, blank
// lines and all — with nothing in them read as a heading, an item, a
// row or a span. Info is what follows the opening backticks, the
// language name Markdown puts there; it is read and kept but not shown.
// A fence never closed runs to the end of the post, as in Markdown.
type Fence struct {
	Info string
	Code string
}

var (
	fenceOpen  = regexp.MustCompile("^```+ *([^`]*?) *$")
	fenceClose = regexp.MustCompile("^```+ *$")
)

// FenceAt reads the fenced block that begins at lines[i], and says how
// many lines it takes, the fences counted; nil when no fence opens
// there. The backticks stand at the start of the line, as a list's
// marker does: indented ones, and the ``` of a sentence about fences,
// are no fence, and neither is a `code` span. Stricter than Markdown on
// purpose: backticks only, no ~~~, and any line of backticks alone
// closes the block, however many opened it. The Hub app's fenceAt reads
// the same way; testdata/fences.json holds the cases both are run
// against.
func FenceAt(lines []string, i int) (*Fence, int) {
	m := fenceOpen.FindStringSubmatch(lines[i])
	if m == nil {
		return nil, 0
	}
	f := &Fence{Info: m[1]}
	j := i + 1
	for ; j < len(lines); j++ {
		if fenceClose.MatchString(lines[j]) {
			break
		}
	}
	f.Code = strings.Join(lines[i+1:j], "\n")
	if j < len(lines) {
		j++ // the closing fence
	}
	return f, j - i
}

// Unfence is text for the places that show a post's words plain — an
// excerpt, a title, a preview picture, a notification: every fence's
// lines dropped and its code kept as it is, the words outside the
// fences put through words, the plain-words chain (Untable, Unlist,
// Unlink, Unbold), so that a "- " or a "**" inside the code is left the
// code it is.
func Unfence(text string, words func(string) string) string {
	if !strings.Contains(text, "```") {
		return words(text)
	}
	lines := strings.Split(text, "\n")
	var out, run []string
	flush := func() {
		if len(run) > 0 {
			out = append(out, words(strings.Join(run, "\n")))
			run = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		if f, n := FenceAt(lines, i); f != nil {
			flush()
			if f.Code != "" {
				out = append(out, f.Code)
			}
			i += n - 1
			continue
		}
		run = append(run, lines[i])
	}
	flush()
	return strings.Join(out, "\n")
}
