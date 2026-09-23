package api

import (
	"strings"
	"testing"
)

// BTN is the Copy button writeFence sets beside a pre, in English
const BTN = "<button type=\"button\" class=\"btn copy\">" + webCopyGlyphs + "<span>Copy</span><span>Copied</span></button>"

// a fenced code block is a block like a table: the breaks around it and
// one blank line on either side go with it, .first when it opens the
// post and .last when it ends it; the code is escaped and nothing else —
// no spans, links, marks of a heading or an item — and a browser drops
// the newline after <pre>, so one is written for it (card.FenceAt holds
// the reading; testdata/fences.json). Beside the pre stands the Copy
// button with both its words, the page's language's.
func TestRenderTextFence(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"```\ncode\n```", "<div class=\"code one first last\"><pre>\ncode</pre>" + BTN + "</div>\n"},
		{"Entry:\n```\ngping 8.8.8.8  terminal gping 8.8.8.8\n```\nafter", "Entry:<div class=\"code one\"><pre>\ngping 8.8.8.8  terminal gping 8.8.8.8</pre>" + BTN + "</div>\nafter"},
		{"Entry:\n\n```go\nx\n```\n\nafter", "Entry:<div class=\"code one\"><pre>\nx</pre>" + BTN + "</div>\nafter"},
		{"```\n\nblank first\n```", "<div class=\"code first last\"><pre>\n\nblank first</pre>" + BTN + "</div>\n"},
		{"```\n# h\n- i\n**b** `c` [w](https://x.y) https://x.y\n```", "<div class=\"code first last\"><pre>\n# h\n- i\n**b** `c` [w](https://x.y) https://x.y</pre>" + BTN + "</div>\n"},
		{"```\n<b>&amp;\n```", "<div class=\"code one first last\"><pre>\n&lt;b&gt;&amp;amp;</pre>" + BTN + "</div>\n"},
		{"```\nopen", "<div class=\"code one first last\"><pre>\nopen</pre>" + BTN + "</div>\n"},
		{"```\n```", "<div class=\"code one first last\"><pre>\n</pre>" + BTN + "</div>\n"},
		{"the ``` lines\n`a`", "the ``` lines<br>\n<code>a</code>"},
		{"- one\n```\ntwo\n```", "<ul class=\"first\"><li>one</li></ul>\n<div class=\"code one last\"><pre>\ntwo</pre>" + BTN + "</div>\n"},
		{"## T\n```\nx\n```\n| a |\n| - |", "<h2 class=\"first\">T</h2>\n<div class=\"code one\"><pre>\nx</pre>" + BTN + "</div>\n<div class=\"tbl last\"><table><thead><tr><th>a</th></tr></thead></table></div>\n"},
	} {
		if got := string(renderText(c.in, nil)); got != c.want {
			t.Errorf("renderText(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	// the found words of a search are marked inside the code, tags untouched
	if got, want := string(markHits(renderText("```\napple pie\n```", nil), "pie")), "<div class=\"code one first last\"><pre>\napple <mark>pie</mark></pre>"+BTN+"</div>\n"; got != want {
		t.Errorf("markHits over a fence\n got %s\nwant %s", got, want)
	}
	// an id inside a fence is code, not a mention
	names := map[string]string{"0123456789abcdef": "Ann"}
	if got, want := string(renderPost("```\n@0123456789abcdef\n```\n@0123456789abcdef", names, webReading{})), "<div class=\"code one first\"><pre>\n@0123456789abcdef</pre>"+BTN+"</div>\n<a class=\"mention\" href=\"/u/0123456789abcdef\">@Ann</a>"; got != want {
		t.Errorf("renderPost over a fence\n got %s\nwant %s", got, want)
	}
	// the search's marks stop at the button: its words are the page's
	if got, want := string(markHits(renderText("```\ncopy\n```", nil), "copy")), "<div class=\"code one first last\"><pre>\n<mark>copy</mark></pre>"+BTN+"</div>\n"; got != want {
		t.Errorf("markHits over the button\n got %s\nwant %s", got, want)
	}
	// the phone's glyphs: a box on another, and a check mark
	if strings.Count(webCopyGlyphs, `<svg class="gl `) != 2 || !strings.Contains(webCopyGlyphs, `class="gl box"`) || !strings.Contains(webCopyGlyphs, `class="gl ok"`) || strings.Count(webCopyGlyphs, `<path class="w"`) != 1 {
		t.Errorf("webCopyGlyphs: %s", webCopyGlyphs)
	}
	// the button's words are the page's language's
	if got := string(renderText("```\nx\n```", webLocales["ja"])); !strings.Contains(got, "<span>コピー</span><span>コピー済み</span>") {
		t.Errorf("renderText in Japanese: %s", got)
	}
	// plain words: the fences go, the code stays as typed
	if got := excerpt("The entry:\n```\ngping 8.8.8.8  terminal gping 8.8.8.8\n```\nThe **two** spaces.", 100); got != "The entry: gping 8.8.8.8 terminal gping 8.8.8.8 The two spaces." {
		t.Errorf("excerpt: %q", got)
	}
	if got := excerpt("```\n# not a heading\n- not an item\n```\n- item", 100); got != "# not a heading - not an item • item" {
		t.Errorf("excerpt of code: %q", got)
	}
	if got := opening("```\ncode line. more\n```"); got != "code line" {
		t.Errorf("opening: %q", got)
	}
}
