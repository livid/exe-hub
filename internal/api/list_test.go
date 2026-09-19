package api

import "testing"

// a list is a block like a heading or a table: the breaks around it and
// one blank line on either side go with it, .first when it opens the
// post and .last when it ends it; an item takes the inline pipeline, and
// a numbered list rides its first number in --n and its widest marker's
// digits in w2/w3 (card.ListAt holds the reading; testdata/lists.json)
func TestRenderTextList(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"- one\n- two", `<ul class="first last"><li>one</li><li>two</li></ul>` + "\n"},
		{"Points:\n- one\n* two\nafter", `Points:<ul><li>one</li><li>two</li></ul>` + "\nafter"},
		{"Points:\n\n- one\n\nafter", `Points:<ul><li>one</li></ul>` + "\nafter"},
		{"1. one\n2. two", `<ol class="first last"><li>one</li><li>two</li></ol>` + "\n"},
		{"1) one", `<ol class="first last"><li>one</li></ol>` + "\n"},
		{"9. nine\n1. ten", `<ol class="w2 first last" style="--n:8"><li>nine</li><li>ten</li></ol>` + "\n"},
		{"0. nought", `<ol class="first last" style="--n:-1"><li>nought</li></ol>` + "\n"},
		{"100. a", `<ol class="w3 first last" style="--n:99"><li>a</li></ol>` + "\n"},
		{"1. one\n\n2. two", `<ol class="first"><li>one</li></ol>` + "\n" + `<ol class="last" style="--n:1"><li>two</li></ol>` + "\n"},
		{"- one\n1. two", `<ul class="first"><li>one</li></ul>` + "\n" + `<ol class="last"><li>two</li></ol>` + "\n"},
		{"- **The bar:** a `field` and [Docs](https://x.y)", `<ul class="first last"><li><strong>The bar:</strong> a <code>field</code> and <a href="https://x.y" title="https://x.y" target="_blank" rel="noopener nofollow">Docs</a></li></ul>` + "\n"},
		{"- <b>&", `<ul class="first last"><li>&lt;b&gt;&amp;</li></ul>` + "\n"},
		{"## T\n- one\n\n| a |\n| - |\n| 1 |", `<h2 class="first">T</h2>` + "\n" + `<ul><li>one</li></ul>` + "\n" + `<div class="tbl last"><table><thead><tr><th>a</th></tr></thead><tbody><tr><td>1</td></tr></tbody></table></div>` + "\n"},
		{"**bold** first\n-5 degrees\n2026. A year", "<strong>bold</strong> first<br>\n-5 degrees<br>\n2026. A year"},
	} {
		if got := string(renderText(c.in)); got != c.want {
			t.Errorf("renderText(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	// the found words of a search are marked inside items, tags untouched
	if got, want := string(markHits(renderText("- apple pie\n- cherry"), "pie li")), `<ul class="first last"><li>apple <mark>pie</mark></li><li>cherry</li></ul>`+"\n"; got != want {
		t.Errorf("markHits over a list\n got %s\nwant %s", got, want)
	}
	if got := excerpt("Three things:\n- one\n- **two**\n1. three", 100); got != "Three things: • one • two 1. three" {
		t.Errorf("excerpt: %q", got)
	}
}
