package api

import "testing"

// a fenced code block is a block like a table: the breaks around it and
// one blank line on either side go with it, .first when it opens the
// post and .last when it ends it; the code is escaped and nothing else —
// no spans, links, marks of a heading or an item — and a browser drops
// the newline after <pre>, so one is written for it (card.FenceAt holds
// the reading; testdata/fences.json)
func TestRenderTextFence(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"```\ncode\n```", "<pre class=\"first last\">\ncode</pre>\n"},
		{"Entry:\n```\ngping 8.8.8.8  terminal gping 8.8.8.8\n```\nafter", "Entry:<pre>\ngping 8.8.8.8  terminal gping 8.8.8.8</pre>\nafter"},
		{"Entry:\n\n```go\nx\n```\n\nafter", "Entry:<pre>\nx</pre>\nafter"},
		{"```\n\nblank first\n```", "<pre class=\"first last\">\n\nblank first</pre>\n"},
		{"```\n# h\n- i\n**b** `c` [w](https://x.y) https://x.y\n```", "<pre class=\"first last\">\n# h\n- i\n**b** `c` [w](https://x.y) https://x.y</pre>\n"},
		{"```\n<b>&amp;\n```", "<pre class=\"first last\">\n&lt;b&gt;&amp;amp;</pre>\n"},
		{"```\nopen", "<pre class=\"first last\">\nopen</pre>\n"},
		{"```\n```", "<pre class=\"first last\">\n</pre>\n"},
		{"the ``` lines\n`a`", "the ``` lines<br>\n<code>a</code>"},
		{"- one\n```\ntwo\n```", "<ul class=\"first\"><li>one</li></ul>\n<pre class=\"last\">\ntwo</pre>\n"},
		{"## T\n```\nx\n```\n| a |\n| - |", "<h2 class=\"first\">T</h2>\n<pre>\nx</pre>\n<div class=\"tbl last\"><table><thead><tr><th>a</th></tr></thead></table></div>\n"},
	} {
		if got := string(renderText(c.in)); got != c.want {
			t.Errorf("renderText(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	// the found words of a search are marked inside the code, tags untouched
	if got, want := string(markHits(renderText("```\napple pie\n```"), "pie")), "<pre class=\"first last\">\napple <mark>pie</mark></pre>\n"; got != want {
		t.Errorf("markHits over a fence\n got %s\nwant %s", got, want)
	}
	// an id inside a fence is code, not a mention
	names := map[string]string{"0123456789abcdef": "Ann"}
	if got, want := string(renderPost("```\n@0123456789abcdef\n```\n@0123456789abcdef", names, "")), "<pre class=\"first\">\n@0123456789abcdef</pre>\n<a class=\"mention\" href=\"/u/0123456789abcdef\">@Ann</a>"; got != want {
		t.Errorf("renderPost over a fence\n got %s\nwant %s", got, want)
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
