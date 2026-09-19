package api

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/events"
	"exehub/internal/identicon"
	"exehub/internal/identity"
	"exehub/internal/preview"
	"exehub/internal/push"
	"exehub/internal/store"
)

// ingest signs and stores one envelope for the test key, returning its
// message id (the post id for post.create).
func ingest(t *testing.T, s *Server, priv ed25519.PrivateKey, pub ed25519.PublicKey, seq int64, typ string, body map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"type": typ, "author": base64.StdEncoding.EncodeToString(pub),
		"seq": seq, "ts": 1756500000000 + seq, "body": body,
	})
	e, err := envelope.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	op, err := e.Op()
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.St.Ingest(raw, ed25519.Sign(priv, raw), e, op)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func get(t *testing.T, h http.Handler, path string, hdr ...string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "http://hub.example"+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestWebHome: the feed page renders posts through the escaping
// pipeline, carries the join block for the live gate, and 404s the rest.
func TestWebHome(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "profile.set", map[string]any{"name": "Ann"})
	ingest(t, s, priv, pub, 2, "post.create", map[string]any{
		"text": "see https://example.com/a. and <b>bold</b> and `x <i>y</i>` too\nline two"})
	h := s.Handler()

	code, body := get(t, h, "/", "X-Forwarded-Proto", "https")
	if code != 200 {
		t.Fatalf("GET / = %d", code)
	}
	for _, want := range []string{
		`<a href="https://example.com/a" target="_blank" rel="noopener nofollow">https://example.com/a</a>.`, // the period stays outside
		"&lt;b&gt;bold&lt;/b&gt;", // markup in a post is text
		"<code>x &lt;i&gt;y&lt;/i&gt;</code>",
		"too<br>\nline two",
		"<b>Ann</b>",
		"<b>Gate:</b> open",
		"One post per 60 seconds",
		"<code>https://hub.example</code>", // the base from Host + X-Forwarded-Proto
		`href="/skill.md"`,
		"1 members · 1 posts",
		// the post's time: a UTC stamp inside <time datetime>, which the
		// page's script turns into the reader's local time
		`<time datetime="2025-08-29T20:40:00Z">2025-08-29 20:40 UTC</time>`,
		"function localTimes(root)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home lacks %q\n%s", want, body)
		}
	}
	if strings.Contains(body, `class="btn prev"`) || strings.Contains(body, `class="btn next"`) {
		t.Error("a paging button on a one-page feed")
	}
	if strings.Contains(body, "<b>bold</b>") {
		t.Error("post markup reached the page unescaped")
	}
	if code, _ := get(t, h, "/nothing"); code != 404 {
		t.Errorf("GET /nothing = %d, want 404", code)
	}

	// a Chinese browser reads the join block in Chinese, the rest of
	// the page as it is; the response varies on the language
	req := httptest.NewRequest("GET", "http://hub.example/", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body = rec.Body.String()
	for _, want := range []string{
		`<div class="window join" id="join" lang="zh-Hans">`,
		`<span class="title">加入这个 hub</span>`,
		"<b>发帖条件：</b>没有限制，任何密钥都可以发帖。每 60 秒最多发一帖。",
		"<code>https://hub.example</code>",
		"<b>Ann</b>", // the feed stays as it is
		`<html lang="en">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Chinese home lacks %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "Join this hub") {
		t.Error("the English join block beside the Chinese one")
	}
	if v := rec.Header().Get("Vary"); v != "Accept-Language" {
		t.Errorf("Vary = %q", v)
	}
	// English first, Chinese further down: English
	if _, body := get(t, h, "/", "Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8"); !strings.Contains(body, "Join this hub") || strings.Contains(body, "加入这个 hub") {
		t.Error("an English browser with Chinese further down got the Chinese join block")
	}
	// ?lang= overrides the browser either way
	if _, body := get(t, h, "/?lang=zh", "Accept-Language", "en-US"); !strings.Contains(body, "加入这个 hub") || strings.Contains(body, "Join this hub") {
		t.Error("?lang=zh did not give an English browser the Chinese join block")
	}
	if _, body := get(t, h, "/?lang=en", "Accept-Language", "zh-CN"); !strings.Contains(body, "Join this hub") || strings.Contains(body, "加入这个 hub") {
		t.Error("?lang=en did not give a Chinese browser the English join block")
	}
}

// TestWebLang: ?lang= is zh, en, or nothing.
func TestWebLang(t *testing.T) {
	for path, want := range map[string]string{
		"/": "", "/?lang=zh": "zh", "/?lang=ZH-TW": "zh", "/?lang=en": "en", "/?lang=en-GB": "en",
		"/?lang=fr": "", "/?lang=": "", "/?lang=zho": "",
	} {
		req := httptest.NewRequest("GET", "http://hub.example"+path, nil)
		req.Header.Set("Accept-Language", "zh-CN")
		if got := webLang(req); got != want {
			t.Errorf("webLang(%q) = %q, want %q", path, got, want)
		}
		if got := webChinese(req); got != (want != "en") { // the browser is Chinese: only ?lang=en says no
			t.Errorf("webChinese(%q, zh browser) = %v", path, got)
		}
	}
}

// TestWebChinese: the browser's language is its highest-q tag.
func TestWebChinese(t *testing.T) {
	for header, want := range map[string]bool{
		"":                                  false,
		"en-US,en;q=0.9":                    false,
		"zh-CN,zh;q=0.9,en;q=0.8":           true,
		"zh-TW":                             true,
		"zh-Hant-HK,zh-Hant;q=0.9,zh;q=0.8": true,
		"zh":                                true,
		"ZH-cn":                             true,
		"en;q=0.8, zh;q=0.9":                true,  // q wins over order
		"en,zh":                             false, // a tie keeps the browser's order
		"zh;q=0,en":                         false, // q=0 is a refusal
		"*":                                 false,
		"zho":                               false, // not a zh- tag
	} {
		req := httptest.NewRequest("GET", "http://hub.example/", nil)
		if header != "" {
			req.Header.Set("Accept-Language", header)
		}
		if got := webChinese(req); got != want {
			t.Errorf("webChinese(%q) = %v, want %v", header, got, want)
		}
	}
}

// TestWebThreadProfile: a thread page shows the post and its replies, a
// profile page the author's posts; unknown ids render the 404 page.
func TestWebThreadProfile(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	root := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "root post"})
	reply := ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "the reply", "reply_to": root})
	h := s.Handler()

	code, body := get(t, h, "/p/"+root)
	if code != 200 || !strings.Contains(body, "root post") || !strings.Contains(body, "the reply") || !strings.Contains(body, "1 reply<") {
		t.Errorf("thread page: %d\n%s", code, body)
	}
	// the way back to the feed is a push button on the strip heading the
	// page, above the post; the status line along the bottom is text only
	if top := topStrip(body); !strings.HasPrefix(top, `<div class="pager top"><a class="btn prev back" href="/"><svg class="gl" viewBox="0 0 11 9"`) ||
		!strings.HasSuffix(top, `</svg><span>Feed</span></a></div>`) || strings.Index(body, `class="pager top"`) > strings.Index(body, `class="post"`) {
		t.Errorf("thread page strip: %q", top)
	}
	if !strings.Contains(body, `<div class="statusbar"><span>1 reply</span></div>`) {
		t.Error("thread status line is not the count alone")
	}
	// a reply to the reply shows on the root's page, indented under it, and
	// names the reply it answers with an in-page link; the count is the tree
	deep := ingest(t, s, priv, pub, 3, "post.create", map[string]any{"text": "deeper still", "reply_to": reply})
	code, body = get(t, h, "/p/"+root)
	if code != 200 || !strings.Contains(body, "deeper still") || !strings.Contains(body, "2 replies<") ||
		!strings.Contains(body, `style="--d:2"`) || !strings.Contains(body, `<a href="#`+reply+`">in reply to `) {
		t.Errorf("nested thread page: %d\n%s", code, body)
	}
	if strings.Contains(body, `<a href="/p/`+root+`">in reply to</a>`) {
		t.Error("a direct reply on the thread page still links back to the same page")
	}
	// every reply carries a Reply link, which the page's script turns into
	// the Reply window's aim and which by itself leads to the reply's own
	// page; the post heading the page has none (the window answers it as
	// it stands), and the window has the line that names the reply, hidden
	for _, id := range []string{reply, deep} {
		if !strings.Contains(body, `<div class="foot re"><a class="rlink" role="button" href="/p/`+id+`">Reply</a></div>`) {
			t.Errorf("reply %s lacks its Reply link", id)
		}
	}
	if strings.Contains(body, `class="rlink" role="button" href="/p/`+root+`"`) || strings.Count(body, `class="rlink"`) != 2 {
		t.Error("the thread's head carries a Reply link, or the links are not one a reply")
	}
	// only someone signed in sees the links: they show under the wallet class alone
	if !strings.Contains(body, ".foot.re { display: none; }\n.wallet .foot.re { display: flex; }\n") || strings.Contains(body, ".js .foot.re") {
		t.Error("the Reply links are not kept to a signed-in wallet")
	}
	if !strings.Contains(body, `<div class="row re-row" hidden><span class="grow re-q"></span><button type="button" class="btn re-clear"`) {
		t.Error("the Reply window lacks the line naming the reply it answers")
	}
	// nowhere else: a feed or a profile has no Reply window to aim
	for _, path := range []string{"/", "/u/" + identity.Fingerprint(pub)} {
		if _, other := get(t, h, path); strings.Contains(other, `class="rlink"`) || strings.Contains(other, `class="row re-row"`) {
			t.Errorf("%s carries a Reply link or the reply line", path)
		}
	}
	// the JSON keeps the one-level replies and adds the tree with depths
	if code, body := get(t, h, "/v1/post/"+root); code != 200 || !strings.Contains(body, `"thread":[`) || !strings.Contains(body, `"depth":2`) {
		t.Errorf("post JSON lacks the thread: %d %s", code, body)
	}
	if !strings.Contains(body, `<meta property="og:description" content="root post">`) {
		t.Error("thread page lacks the OpenGraph description")
	}
	if code, body := get(t, h, "/p/"+strings.Repeat("0", 64)); code != 404 || !strings.Contains(body, "No such post.") {
		t.Errorf("unknown post: %d\n%s", code, body)
	}
	// the profile id is the author's fingerprint, as the feed reports it
	posts, _ := s.St.Feed("", 10, true)
	author := posts[0].Author

	_, home := get(t, h, "/")
	if strings.Contains(home, `class="post reply"`) {
		t.Error("a reply row leaked into the home feed")
	}
	// the count is the whole conversation — the nested reply counts too
	if !strings.Contains(home, "2 replies ▸") {
		t.Error("home lacks the tree reply count link")
	}
	// the foot says what was said last — the newest reply in the tree,
	// its link landing on that reply in the thread
	if !strings.Contains(home, `<a class="latest" href="/p/`+root+`#`+deep+`"><b>`+author+`</b> deeper still</a>`) {
		t.Errorf("home foot lacks the thread's newest reply\n%s", home)
	}

	code, body = get(t, h, "/u/"+author)
	if code != 200 || !strings.Contains(body, "root post") || !strings.Contains(body, "the reply") || !strings.Contains(body, "<h1>"+author+"</h1>") {
		t.Errorf("profile page: %d\n%s", code, body)
	}
	// with a profile.set the page gains the name, count and date
	ingest(t, s, priv, pub, 4, "profile.set", map[string]any{"name": "Ann", "bio": "hi <there>"})
	code, body = get(t, h, "/u/"+author)
	if code != 200 || !strings.Contains(body, "<h1>Ann</h1>") || !strings.Contains(body, `· since <time datetime="20`) || !strings.Contains(body, `Z" data-date>20`) || !strings.Contains(body, `<span class="stats">3 posts</span>`) || !strings.Contains(body, "hi &lt;there&gt;") {
		t.Errorf("named profile page: %d %q", code, statusLine(body))
	}
	if strings.Contains(body, `class="pager top"`) || strings.Contains(home, `class="pager top"`) {
		t.Error("a one-page list is headed by a pager")
	}
	if code, _ := get(t, h, "/u/nobody"); code != 404 {
		t.Errorf("unknown profile = %d", code)
	}
}

// TestWebProfileQuotes: on a profile page a reply carries the post it
// answers, quoted above it as the head of its card, and a run of
// replies under one parent shares one quote.
func TestWebProfileQuotes(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	apub, apriv, _ := ed25519.GenerateKey(nil)
	bpub, bpriv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, apriv, apub, 1, "profile.set", map[string]any{"name": "Ann"})
	root := ingest(t, s, apriv, apub, 2, "post.create", map[string]any{"text": "the idea post"})
	other := ingest(t, s, apriv, apub, 3, "post.create", map[string]any{"text": "a second thread"})
	ingest(t, s, bpriv, bpub, 1, "post.create", map[string]any{"text": "first answer", "reply_to": root})
	ingest(t, s, bpriv, bpub, 2, "post.create", map[string]any{"text": "second answer", "reply_to": root})
	ingest(t, s, bpriv, bpub, 3, "post.create", map[string]any{"text": "on the other", "reply_to": other})
	h := s.Handler()

	// the profile id is the author's fingerprint, as the feed reports it
	feed, _ := s.St.Feed("", 10, true)
	var bob string
	for _, p := range feed {
		if p.Text == "first answer" {
			bob = p.Author
		}
	}
	code, body := get(t, h, "/u/"+bob)
	if code != 200 {
		t.Fatalf("GET /u/%s = %d", bob, code)
	}
	// newest first: "on the other" quotes its thread, "second answer"
	// quotes the root, and "first answer" — same parent — shares that
	// quote as the run under it
	for _, want := range []string{
		`<a class="quote" href="/p/` + root + `"><b>Ann</b> the idea post</a>`,
		`<a class="quote" href="/p/` + other + `"><b>Ann</b> a second thread</a>`,
		`class="post reply qrun"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("profile page lacks %q\n%s", want, body)
		}
	}
	if n := strings.Count(body, `class="quote"`); n != 2 {
		t.Errorf("profile page carries %d quotes, want 2", n)
	}
	if strings.Contains(body, `">in reply to</a>`) {
		t.Error("a quoted reply still wears the bare in-reply-to link")
	}
	// the thread page keeps its own rendering: no quotes there
	if _, tb := get(t, h, "/p/"+root); strings.Contains(tb, `class="quote"`) {
		t.Error("a quote leaked onto the thread page")
	}
}

// TestWebJoinToken: a token-gated hub's join block names the holding
// — raw base units until the RPC has told it the mint's decimals — and
// the mint is a link to Jupiter's swap page, set to buy it with SOL.
func TestWebJoinToken(t *testing.T) {
	zero := 0
	s := testServer(t, &config.Config{
		Gate: config.Gate{Mode: "token", Token: config.TokenGate{
			Mints:   []config.MintReq{{Mint: "9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump", MinAmount: "10000000000", MinRaw: 10000000000}},
			Recheck: "10m"}},
		Cooldown: &zero,
	})
	_, body := get(t, s.Handler(), "/")
	const buy = `<a href="https://jup.ag/swap?sell=So11111111111111111111111111111111111111112&amp;buy=9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump" target="_blank" rel="noopener"`
	if !strings.Contains(body, "<b>10000000000 raw base units</b> of mint "+buy+` title="Buy with SOL on jup.ag"><code>9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump</code></a>.`) {
		t.Errorf("token join block wrong:\n%s", body)
	}
	if strings.Contains(body, "One post per") {
		t.Error("cooldown line shown for a hub with cooldown 0")
	}
	_, body = get(t, s.Handler(), "/", "Accept-Language", "zh-TW")
	if !strings.Contains(body, "需要持有至少 <b>10000000000</b> 个最小单位（mint "+buy+` title="在 jup.ag 用 SOL 购买"><code>9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump</code></a>）。`) {
		t.Errorf("Chinese token join block wrong:\n%s", body)
	}
	if strings.Contains(body, "秒最多发一帖") {
		t.Error("Chinese cooldown line shown for a hub with cooldown 0")
	}
	// the config asks a mint only to be non-empty: whatever it says stays
	// inside the buy parameter
	if got, want := (webMint{Mint: "a&sell=b c"}).Buy(), "https://jup.ag/swap?sell="+wSOL+"&buy=a%26sell%3Db+c"; got != want {
		t.Errorf("Buy() = %s, want %s", got, want)
	}
}

func TestRenderText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a <b>c</b>", "a &lt;b&gt;c&lt;/b&gt;"},
		{"go to https://x.y/z?q=1, now", `go to <a href="https://x.y/z?q=1" target="_blank" rel="noopener nofollow">https://x.y/z?q=1</a>, now`},
		{"(see https://x.y/)", `(see <a href="https://x.y/" target="_blank" rel="noopener nofollow">https://x.y/</a>)`},
		{"来自小虾平台（https://xiaoxia.app）的助手", `来自小虾平台（<a href="https://xiaoxia.app" target="_blank" rel="noopener nofollow">https://xiaoxia.app</a>）的助手`},
		{"详见https://x.y/z的说明。", `详见<a href="https://x.y/z" target="_blank" rel="noopener nofollow">https://x.y/z</a>的说明。`},
		{"https://x.y/café, oui", `<a href="https://x.y/café" target="_blank" rel="noopener nofollow">https://x.y/café</a>, oui`},
		{"“https://x.y/z” en", `“<a href="https://x.y/z" target="_blank" rel="noopener nofollow">https://x.y/z</a>” en`},
		{"`https://x.y` literal", "<code>https://x.y</code> literal"},
		{"a\nb", "a<br>\nb"},
		{"`unterminated", "`unterminated"},
		{"ftp://no.link", "ftp://no.link"},
		// heading lines: h1–h3, the breaks around one and a blank line on
		// either side go with it, the first in a post is marked, its words
		// take the inline pipeline
		{"## Head\nbody", `<h2 class="first">Head</h2>` + "\nbody"},
		{"p\n\n# T  \nq\n", "p<h1>T</h1>\nq<br>\n"},
		{"# T\n\nbody\n\n## S\n\nmore", `<h1 class="first">T</h1>` + "\nbody<h2>S</h2>\nmore"},
		{"# A\n## B\n\n\nc", `<h1 class="first">A</h1>` + "\n<h2>B</h2>\n<br>\nc"},
		{"### `code` and https://x.y/", `<h3 class="first"><code>code</code> and <a href="https://x.y/" target="_blank" rel="noopener nofollow">https://x.y/</a></h3>` + "\n"},
		{"# <b>", `<h1 class="first">&lt;b&gt;</h1>` + "\n"},
		{"#nospace\n#### four\n ## indented\n## ", "#nospace<br>\n#### four<br>\n ## indented<br>\n## "},
		// [words](url): a link on its words, the address on hover; http(s)
		// and only an address the bare matcher takes whole; a code span
		// binds tighter; the words take code spans and no link of their own
		{"see [MDN on order](https://developer.mozilla.org/en-US/docs/Web/CSS/order#accessibility).", `see <a href="https://developer.mozilla.org/en-US/docs/Web/CSS/order#accessibility" title="https://developer.mozilla.org/en-US/docs/Web/CSS/order#accessibility" target="_blank" rel="noopener nofollow">MDN on order</a>.`},
		{"[a](https://x.y/?a=1&b=2) and https://z.w", `<a href="https://x.y/?a=1&amp;b=2" title="https://x.y/?a=1&amp;b=2" target="_blank" rel="noopener nofollow">a</a> and <a href="https://z.w" target="_blank" rel="noopener nofollow">https://z.w</a>`},
		{"[the `order` property](https://x.y)", `<a href="https://x.y" title="https://x.y" target="_blank" rel="noopener nofollow">the <code>order</code> property</a>`},
		{"[https://bank.example](https://x.y)", `<a href="https://x.y" title="https://x.y" target="_blank" rel="noopener nofollow">https://bank.example</a>`},
		{"[<b>](https://x.y)", `<a href="https://x.y" title="https://x.y" target="_blank" rel="noopener nofollow">&lt;b&gt;</a>`},
		{"## [Head](https://x.y)", `<h2 class="first"><a href="https://x.y" title="https://x.y" target="_blank" rel="noopener nofollow">Head</a></h2>` + "\n"},
		{"`[a](https://x.y)` literal", "<code>[a](https://x.y)</code> literal"},
		{"[a `b](https://x.y) c` d", `[a <code>b](https://x.y) c</code> d`},
		{"[a](javascript:alert(1))", "[a](javascript:alert(1))"},
		{"[a](ftp://x.y)", "[a](ftp://x.y)"},
		{"[a] (https://x.y)", `[a] (<a href="https://x.y" target="_blank" rel="noopener nofollow">https://x.y</a>)`},
		{"[a\nb](https://x.y)", `[a<br>` + "\n" + `b](<a href="https://x.y" target="_blank" rel="noopener nofollow">https://x.y</a>)`},
		{`[a](https://x.y/"onmouseover="x)`, `[a](<a href="https://x.y/" target="_blank" rel="noopener nofollow">https://x.y/</a>&#34;onmouseover=&#34;x)`},
		{"[a](https://x.y/w_(z))", `[a](<a href="https://x.y/w_" target="_blank" rel="noopener nofollow">https://x.y/w_</a>(z))`},
		{"前文[说明](https://x.y/z)后文", `前文<a href="https://x.y/z" title="https://x.y/z" target="_blank" rel="noopener nofollow">说明</a>后文`},
		// a table (card.TableAt): a block like a heading — the breaks
		// around it and a blank line on either side go with it, .first
		// when it opens the post, .last when it ends it; a column's
		// alignment is a class on its cells; a cell takes the inline
		// pipeline, so its link is a link and nothing in it is markup
		{"Leaders:\n\n| Fund | Return |\n| --- | ---: |\n| [MRNY](https://x.y/) | +344% |\n\nafter", `Leaders:<div class="tbl"><table><thead><tr><th>Fund</th><th class="r">Return</th></tr></thead><tbody><tr><td><a href="https://x.y/" title="https://x.y/" target="_blank" rel="noopener nofollow">MRNY</a></td><td class="r">+344%</td></tr></tbody></table></div>` + "\nafter"},
		{"a | b\n:- | :-:\n`x\\|y` | <i>", `<div class="tbl first last"><table><thead><tr><th class="l">a</th><th class="c">b</th></tr></thead><tbody><tr><td class="l"><code>x|y</code></td><td class="c">&lt;i&gt;</td></tr></tbody></table></div>` + "\n"},
		{"| a |\n| - |\n", `<div class="tbl first last"><table><thead><tr><th>a</th></tr></thead></table></div>` + "\n"},
		{"## T\n| a | b |\n| - | - |\n| 1 | 2 |\nwords after\n", `<h2 class="first">T</h2>` + "\n" + `<div class="tbl"><table><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table></div>` + "\nwords after<br>\n"},
		{"this | that\nno rule", "this | that<br>\nno rule"},
		{"| a | b |\n| --- |\n| 1 | 2 |", "| a | b |<br>\n| --- |<br>\n| 1 | 2 |"},
	}
	for _, c := range cases {
		if got := string(renderText(c.in)); got != c.want {
			t.Errorf("renderText(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

// the cases the Hub app's markHits is run against too
// (card/testdata/marks.json, read by ~/tools/playwright/exe-hub-find-test.js):
// which stretches of a search result stand in a <mark>
func TestMarkHits(t *testing.T) {
	raw, err := os.ReadFile("../card/testdata/marks.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text, Q string
		Marks         []string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	mark := regexp.MustCompile(`(?s)<mark>(.*?)</mark>`)
	tags := regexp.MustCompile(`<[^>]*>`)
	for _, c := range cases {
		plain := string(renderText(c.Text))
		page := string(markHits(template.HTML(plain), c.Q))
		got := []string{}
		for _, m := range mark.FindAllStringSubmatch(page, -1) {
			got = append(got, html.UnescapeString(m[1]))
		}
		if !reflect.DeepEqual(got, c.Marks) {
			t.Errorf("%s: marks %q, want %q\n%s", c.Name, got, c.Marks, page)
		}
		// the marks are all that is added: take them out and the page is as it was
		if back := strings.NewReplacer("<mark>", "", "</mark>", "").Replace(page); back != plain {
			t.Errorf("%s: more than marks changed\n got %s\nwant %s", c.Name, back, plain)
		}
		if strings.Contains(tags.ReplaceAllString(page, ""), "<") {
			t.Errorf("%s: raw markup in the words\n%s", c.Name, page)
		}
	}
	// the markup itself: tags and attributes pass through, entities stay whole
	for _, c := range []struct{ in, q, want string }{
		{"camp & tea", "amp", "c<mark>amp</mark> &amp; tea"},
		{"[pie shop](https://x.y/pie)", "pie", `<a href="https://x.y/pie" title="https://x.y/pie" target="_blank" rel="noopener nofollow"><mark>pie</mark> shop</a>`},
		{"a <b> pie", "<b>", "a <mark>&lt;b&gt;</mark> pie"},
		{"see https://x.y/pie now", "pie", `see <a href="https://x.y/pie" target="_blank" rel="noopener nofollow">https://x.y/<mark>pie</mark></a> now`},
		{"`pie()` and **Pie**", "PIE", "<code><mark>pie</mark>()</code> and <strong><mark>Pie</mark></strong>"},
		{"target nofollow noopener", "target", "<mark>target</mark> nofollow noopener"},
	} {
		if got := string(markHits(renderText(c.in), c.q)); got != c.want {
			t.Errorf("markHits(%q, %q)\n got %s\nwant %s", c.in, c.q, got, c.want)
		}
	}
}

// the cases the Hub app's formatText is run against too
// (card/testdata/bold.json): which stretches come out bold, and which
// words the links hold
func TestRenderTextBold(t *testing.T) {
	raw, err := os.ReadFile("../card/testdata/bold.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text    string
		Strong, Links []string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	tags := regexp.MustCompile(`<[^>]+>`)
	words := func(page, tag string) []string {
		out := []string{}
		for _, m := range regexp.MustCompile(`(?s)<`+tag+`[ >].*?</`+tag+`>`).FindAllString(page, -1) {
			out = append(out, html.UnescapeString(tags.ReplaceAllString(m, "")))
		}
		return out
	}
	for _, c := range cases {
		page := string(renderText(c.Text))
		if got := words(page, "strong"); !reflect.DeepEqual(got, c.Strong) {
			t.Errorf("%s: bold %q, want %q\n%s", c.Name, got, c.Strong, page)
		}
		if got := words(page, "a"); c.Links != nil && !reflect.DeepEqual(got, c.Links) {
			t.Errorf("%s: links %q, want %q\n%s", c.Name, got, c.Links, page)
		}
	}
	// the markup itself, where the nesting matters
	for _, c := range []struct{ in, want string }{
		{"A **bold** word.", "A <strong>bold</strong> word."},
		{"**[Docs](https://x.y)**", `<strong><a href="https://x.y" title="https://x.y" target="_blank" rel="noopener nofollow">Docs</a></strong>`},
		{"[**Docs** page](https://x.y)", `<a href="https://x.y" title="https://x.y" target="_blank" rel="noopener nofollow"><strong>Docs</strong> page</a>`},
		{"**run `make`**", "<strong>run <code>make</code></strong>"},
		{"`**kwargs**`", "<code>**kwargs**</code>"},
		{"**<b>&amp;**", "<strong>&lt;b&gt;&amp;amp;</strong>"},
		{"## The **new** bar", `<h2 class="first">The <strong>new</strong> bar</h2>` + "\n"},
	} {
		if got := string(renderText(c.in)); got != c.want {
			t.Errorf("renderText(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	if got := excerpt("## Head\n**The bar:** a [field](https://x.y) and `**code**`", 100); got != "Head The bar: a field and `**code**`" {
		t.Errorf("excerpt: %q", got)
	}
}

func TestExcerpt(t *testing.T) {
	if got := excerpt("one  two\nthree", 100); got != "one two three" {
		t.Errorf("excerpt = %q", got)
	}
	if got := excerpt("## Title\nbody # not a heading", 100); got != "Title body # not a heading" {
		t.Errorf("excerpt drops heading marks: %q", got)
	}
	if got := excerpt("one two three four", 10); got != "one two…" {
		t.Errorf("excerpt cut = %q", got)
	}
	if got := excerpt("see [MDN on order](https://x.y/#a), then https://z.w", 100); got != "see MDN on order, then https://z.w" {
		t.Errorf("excerpt puts a Markdown link back to its words: %q", got)
	}
	if got := excerpt("Leaders:\n\n| Fund | Return |\n| --- | ---: |\n| [MRNY](https://x.y/) | +344% |", 100); got != "Leaders: Fund · Return MRNY · +344%" {
		t.Errorf("excerpt puts a table back to its cells' words: %q", got)
	}
}

// TestWebPaging: 32 posts make two pages. The first page has Next only,
// the last has Prev only, and Prev from the last page redirects to the
// bare first page (never a full copy of it under ?after=, which would
// not be live); a newer page with more beyond it is a static middle
// page with both buttons, and a short newer page redirects to the top.
func TestWebPaging(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "profile.set", map[string]any{"name": "Ann"})
	var ids []string // oldest first
	for i := int64(1); i <= webPage+2; i++ {
		ids = append(ids, ingest(t, s, priv, pub, i+1, "post.create", map[string]any{"text": fmt.Sprintf("post %d", i)}))
	}
	h := s.Handler()
	oldestOnFirst := ids[2] // 32 posts newest-first: the first page ends at post 3
	redirect := func(path string) (int, string) {
		req := httptest.NewRequest("GET", "http://hub.example"+path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code, w.Header().Get("Location")
	}

	_, body := get(t, h, "/")
	if strings.Contains(body, "Prev") || !strings.Contains(body, `<a class="btn next fwd" href="/?before=`+oldestOnFirst+`"><span>Next</span><svg`) {
		t.Errorf("first page: %q", statusLine(body))
	}
	if !strings.Contains(body, `<span class="stats">1 members · 32 posts</span>`) {
		t.Errorf("stats: %q", statusLine(body))
	}
	// Next points on: the Hub app's back arrow mirrored
	if !strings.Contains(body, `<span>Next</span><svg class="gl" viewBox="0 0 11 9" width="11" height="9" aria-hidden="true"><path class="k" d="M6 0h1v1h-1z`) {
		t.Errorf("Next arrow: %q", statusLine(body))
	}
	// a list past one page is headed by the same strip, under the find strip
	if top := topStrip(body); strings.TrimSuffix(strings.TrimPrefix(top, `<div class="pager top">`), "</div>") != strings.TrimSuffix(strings.TrimPrefix(statusLine(body), `<div class="pager">`), "</div>") {
		t.Errorf("top strip differs from the bottom one: %q vs %q", top, statusLine(body))
	}
	if i, j := strings.Index(body, `id="find"`), strings.Index(body, `class="pager top"`); !(i < j && j < strings.Index(body, `class="post"`)) {
		t.Error("the top strip is not between the find strip and the first post")
	}

	_, body = get(t, h, "/?before="+oldestOnFirst)
	if !strings.Contains(body, "post 2<") || !strings.Contains(body, "post 1<") || strings.Contains(body, "post 3<") {
		t.Error("second page should hold posts 2 and 1 only")
	}
	if strings.Contains(body, "Next") || !strings.Contains(body, `<a class="btn prev back" href="/?after=`+ids[1]+`"><svg class="gl" viewBox="0 0 11 9" width="11" height="9" aria-hidden="true"><path class="k" d="M4 0h1v1h-1z`) {
		t.Errorf("last page: %q", statusLine(body))
	}
	if strings.Count(body, `<a class="btn prev back" href="/?after=`+ids[1]+`"><svg`) != 2 {
		t.Errorf("last page lacks Prev at the top: %q", topStrip(body))
	}

	// Prev from the last page: the 30 posts newer than post 2 are exactly
	// the first page — back to its bare URL, where the feed is live
	if code, loc := redirect("/?after=" + ids[1]); code != 302 || loc != "/" {
		t.Errorf("full newer page at the top: %d %s", code, loc)
	}

	// 31 posts newer than post 1: the 30 nearest make a middle page, posts 31..2
	_, body = get(t, h, "/?after="+ids[0])
	if !strings.Contains(body, "post 31<") || !strings.Contains(body, "post 2<") || strings.Contains(body, "post 32<") || strings.Contains(body, "post 1<") ||
		!strings.Contains(body, `<a class="btn prev back" href="/?after=`+ids[30]+`"><svg`) ||
		!strings.Contains(body, `<a class="btn next fwd" href="/?before=`+ids[1]+`"><span>Next</span><svg`) {
		t.Errorf("middle newer page: %q", statusLine(body))
	}

	// only a few posts newer than post 20: back to the top page instead of a short one
	if code, loc := redirect("/?after=" + ids[19]); code != 302 || loc != "/" {
		t.Errorf("short newer page: %d %s", code, loc)
	}
	if code, _ := get(t, h, "/?before="+strings.Repeat("0", 64)); code != 404 {
		t.Errorf("unknown cursor = %d", code)
	}
	// ?lang= rides the pager links and the redirect to the top
	_, body = get(t, h, "/?lang=zh")
	if !strings.Contains(body, "加入这个 hub") || !strings.Contains(body, `<a class="btn next fwd" href="/?lang=zh&amp;before=`+oldestOnFirst+`">`) {
		t.Errorf("?lang=zh first page: %q", statusLine(body))
	}
	_, body = get(t, h, "/?lang=zh&before="+oldestOnFirst)
	if !strings.Contains(body, `<a class="btn prev back" href="/?lang=zh&amp;after=`) {
		t.Errorf("?lang=zh last page: %q", statusLine(body))
	}
	if code, loc := redirect("/?lang=zh&after=" + ids[len(ids)-1]); code != 302 || loc != "/?lang=zh" {
		t.Errorf("short newer page with ?lang=zh: %d %q, want 302 /?lang=zh", code, loc)
	}

}

// TestWebSearch: the home page carries the find strip; /search?q= is
// the posts holding every word (replies too), the query escaped in the
// field, the title and the cursor links, paged like the feed.
func TestWebSearch(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "profile.set", map[string]any{"name": "Ann"})
	p1 := ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "apple pie"})
	ingest(t, s, priv, pub, 3, "post.create", map[string]any{"text": "Apple tart <b>x</b>"})
	ingest(t, s, priv, pub, 4, "post.create", map[string]any{"text": "cherry pie"})
	ingest(t, s, priv, pub, 5, "post.create", map[string]any{"text": "pie again", "reply_to": p1})
	h := s.Handler()
	const strip = `<form class="find" id="find" action="/search" method="get"><input type="text" name="q" value="`

	_, body := get(t, h, "/")
	if !strings.Contains(body, strip+`"`) || !strings.Contains(body, `<button class="btn" type="submit">Search</button>`) {
		t.Error("home page lacks the find strip")
	}
	if strings.Contains(body, `name="robots"`) {
		t.Error("the feed is noindex")
	}

	code, body := get(t, h, "/search?q=apple")
	// the found word stands in a <mark>, capitals or not
	if code != 200 || !strings.Contains(body, "<mark>apple</mark> pie<") || !strings.Contains(body, "<mark>Apple</mark> tart") || strings.Contains(body, "cherry") {
		t.Errorf("apple: %d, hits wrong", code)
	}
	for _, want := range []string{
		strip + `apple"`, `<title>Search: apple · hub.example</title>`, `<meta name="robots" content="noindex">`,
		`<span class="stats">2 posts match</span>`, `<span class="title">Search</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("search page lacks %q", want)
		}
	}
	if strings.Contains(body, `class="btn prev"`) || strings.Contains(body, `class="btn next"`) || strings.Contains(body, "EventSource") {
		t.Error("a paging button or the live script on a one-page search")
	}

	// every word must appear; whitespace is normalised into the field
	_, body = get(t, h, "/search?q=+pie++APPLE+")
	if !strings.Contains(body, strip+`pie APPLE"`) || !strings.Contains(body, "<mark>apple</mark> <mark>pie</mark><") || strings.Contains(body, "cherry") || strings.Contains(body, "</mark> tart") ||
		!strings.Contains(body, `<span class="stats">1 post matches</span>`) {
		t.Error("two words: not the one post holding both")
	}

	// replies are found too, and shown as replies
	_, body = get(t, h, "/search?q=pie")
	if !strings.Contains(body, "<mark>pie</mark> again<") || !strings.Contains(body, `class="post reply"`) || !strings.Contains(body, `<span class="stats">3 posts match</span>`) {
		t.Error("the reply is missing from the pie results")
	}

	// the query is escaped wherever it lands, and matched literally
	_, body = get(t, h, "/search?q=%3Cb%3Ex")
	if !strings.Contains(body, strip+`&lt;b&gt;x"`) || !strings.Contains(body, `<title>Search: &lt;b&gt;x · hub.example</title>`) ||
		strings.Contains(body, "<b>x") || !strings.Contains(body, "Apple tart <mark>&lt;b&gt;x</mark>&lt;/b&gt;<") || !strings.Contains(body, "1 post matches") {
		t.Errorf("markup in the query: %s", body)
	}

	_, body = get(t, h, "/search?q=zzz")
	if !strings.Contains(body, "No post matches.") || !strings.Contains(body, `<span class="stats">0 posts match</span>`) {
		t.Error("no hits: message or count missing")
	}
	code, body = get(t, h, "/search")
	if code != 200 || !strings.Contains(body, "Type a word or two") || strings.Contains(body, `class="pager"`) || !strings.Contains(body, `<title>Search · hub.example</title>`) {
		t.Errorf("empty query: %d", code)
	}
	long := strings.Repeat("ab ", 120)
	_, body = get(t, h, "/search?q="+strings.ReplaceAll(long, " ", "+"))
	if !strings.Contains(body, strip+strings.TrimSpace(long[:200])+`"`) {
		t.Error("a long query is not cut at 200 characters")
	}
}

// TestWebSearchPaging: a search pages like the feed, the query carried
// URL-encoded in the cursor links; Prev from the second page lands on
// the bare query.
func TestWebSearchPaging(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	var ids []string // oldest first
	for i := int64(1); i <= webPage+2; i++ {
		ids = append(ids, ingest(t, s, priv, pub, i, "post.create", map[string]any{"text": fmt.Sprintf("hit me %d", i)}))
	}
	ingest(t, s, priv, pub, webPage+3, "post.create", map[string]any{"text": "miss 99"})
	h := s.Handler()
	oldestOnFirst := ids[2]

	_, body := get(t, h, "/search?q=hit+me")
	if strings.Contains(body, "Prev") || !strings.Contains(body, `<a class="btn next fwd" href="/search?q=hit%20me&amp;before=`+oldestOnFirst+`"><span>Next</span><svg`) {
		t.Errorf("first page: %q", statusLine(body))
	}
	if !strings.Contains(body, `<span class="stats">32 posts match</span>`) || strings.Contains(body, "miss 99") {
		t.Errorf("stats: %q", statusLine(body))
	}
	if strings.Count(body, `href="/search?q=hit%20me&amp;before=`+oldestOnFirst+`"`) != 2 {
		t.Errorf("first page lacks Next at the top: %q", topStrip(body))
	}
	if _, body := get(t, h, "/search?q=miss"); strings.Contains(body, `class="pager top"`) || !strings.Contains(body, `<span class="stats">1 post matches</span>`) {
		t.Errorf("a one-page search is headed by a pager: %q", topStrip(body))
	}
	_, body = get(t, h, "/search?q=hit+me&before="+oldestOnFirst)
	if !strings.Contains(body, "<mark>hit</mark> <mark>me</mark> 2<") || !strings.Contains(body, "<mark>hit</mark> <mark>me</mark> 1<") || strings.Contains(body, "</mark> 3<") ||
		strings.Contains(body, "Next") || !strings.Contains(body, `<a class="btn prev back" href="/search?q=hit%20me&amp;after=`+ids[1]+`"><svg`) {
		t.Errorf("last page: %q", statusLine(body))
	}
	req := httptest.NewRequest("GET", "http://hub.example/search?q=hit+me&after="+ids[1], nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 302 || w.Header().Get("Location") != "/search?q=hit+me" {
		t.Errorf("newer page at the top: %d %s", w.Code, w.Header().Get("Location"))
	}
	if code, _ := get(t, h, "/search?q=hit+me&before="+strings.Repeat("0", 64)); code != 404 {
		t.Errorf("unknown cursor = %d", code)
	}
}

// TestSearchAPI: /v1/search is the page's query as JSON — normalised
// query echoed, replies included, the total beside the page, the feed's
// pagination and errors.
func TestSearchAPI(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	p1 := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "apple pie"})
	p2 := ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "cherry pie"})
	r1 := ingest(t, s, priv, pub, 3, "post.create", map[string]any{"text": "PIE again", "reply_to": p1})
	h := s.Handler()
	var out struct {
		Query string
		Posts []struct{ ID, Text string }
		Total int
	}
	code, body := get(t, h, "/v1/search?q=+pie++&limit=2")
	if err := json.Unmarshal([]byte(body), &out); err != nil || code != 200 {
		t.Fatalf("search: %d %s", code, body)
	}
	if out.Query != "pie" || out.Total != 3 || len(out.Posts) != 2 || out.Posts[0].ID != r1 || out.Posts[1].ID != p2 {
		t.Errorf("first page: %+v", out)
	}
	code, body = get(t, h, "/v1/search?q=pie&limit=2&before="+p2)
	if err := json.Unmarshal([]byte(body), &out); err != nil || code != 200 || len(out.Posts) != 1 || out.Posts[0].ID != p1 || out.Total != 3 {
		t.Errorf("second page: %d %+v", code, out)
	}
	if code, _ := get(t, h, "/v1/search?q=pie&before="+strings.Repeat("0", 64)); code != 404 {
		t.Errorf("unknown cursor = %d", code)
	}
	if code, body := get(t, h, "/v1/search?q=+"); code != 400 || !strings.Contains(body, "q is required") {
		t.Errorf("empty query = %d %s", code, body)
	}
	if code, body := get(t, h, "/v1/search?q=nothing"); code != 200 || !strings.Contains(body, `"posts":[]`) || !strings.Contains(body, `"total":0`) {
		t.Errorf("no hits = %d %s", code, body)
	}
}

// statusLine is the page's pager strip along the frame's bottom, for
// short failure messages; topStrip the one heading a list past one page.
func statusLine(body string) string { return strip(body, `<div class="pager">`) }
func topStrip(body string) string   { return strip(body, `<div class="pager top">`) }

func strip(body, open string) string {
	i := strings.Index(body, open)
	if i < 0 {
		return "(none)"
	}
	j := strings.Index(body[i:], "</div>")
	return body[i : i+j+6]
}

// TestWebPicture: a picture embed renders as a viewer link carrying its
// name, and the page ships the viewer script.
func TestWebPicture(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	const cid = "bafybeieeqqg3m4keu6lt3bwmo6s2gofxyrrn3sbyayw63kvxzrq4p5rdsy"
	if err := s.St.AddPin(cid, 1234, "image/jpeg", false); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "look",
		"embeds": []map[string]any{{"cid": cid, "mime": "image/jpeg", "filename": "cat.jpg", "alt": "a cat"}}})
	_, body := get(t, s.Handler(), "/")
	if !strings.Contains(body, `<a class="pic" href="/v1/embed/`+cid+`" data-name="cat.jpg"><img src="/v1/embed/`+cid+`" alt="a cat"></a>`) {
		t.Errorf("picture embed wrong:\n%s", body[strings.Index(body, `<div class="embeds">`):][:300])
	}
	if !strings.Contains(body, `closest("a.pic, a.page")`) {
		t.Error("viewer script missing")
	}
}

// TestWebMedia: a video is a player in a box of its final size, with its
// poster and no download until played; a looping animation plays muted
// without controls; a sound shows its waveform, player, name and length;
// a video without facts gets a 16:9 box; the thread page's preview
// image is the video's frame.
func TestWebMedia(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	const vid, poster, gif, gposter, snd, wave, bare = "bafybeivid", "bafybeivposter", "bafybeigif", "bafybeigposter", "bafybeisnd", "bafybeiwave", "bafybeibare"
	for cid, mime := range map[string]string{vid: "video/mp4", poster: "image/jpeg", gif: "video/mp4", gposter: "image/jpeg", snd: "audio/mp4", wave: "image/png", bare: "video/mp4"} {
		if err := s.St.AddPin(cid, 1000, mime, false); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	id := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "the beach", "embeds": []map[string]any{
		{"cid": vid, "mime": "video/mp4", "poster": poster, "width": 1080, "height": 1920, "duration": 12.052},
		{"cid": gif, "mime": "video/mp4", "poster": gposter, "width": 480, "height": 270, "loop": true},
		{"cid": snd, "mime": "audio/mp4", "poster": wave, "duration": 83.4, "filename": "waves.m4a"},
		{"cid": bare, "mime": "video/mp4", "alt": "a clip"},
	}})
	_, body := get(t, s.Handler(), "/p/"+id)
	for _, want := range []string{
		`<div class="vid" style="width: 236px; aspect-ratio: 236 / 420"><video src="/v1/embed/` + vid + `" poster="/v1/embed/` + poster + `" controls playsinline preload="none"></video></div>`,
		`<div class="vid" data-loop style="width: 480px; aspect-ratio: 480 / 270"><video src="/v1/embed/` + gif + `" poster="/v1/embed/` + gposter + `" autoplay loop muted playsinline preload="auto"></video></div>`,
		`<div class="aud"><img class="wave" src="/v1/embed/` + wave + `" alt=""><audio controls preload="none" src="/v1/embed/` + snd + `"></audio><span class="afoot">waves.m4a · 1:23</span></div>`,
		`<div class="vid" style="width: 100%; aspect-ratio: 16 / 9"><video src="/v1/embed/` + bare + `" controls playsinline preload="metadata" aria-label="a clip"></video></div>`,
		`<meta property="og:image" content="http://hub.example/v1/embed/` + poster + `">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\nin %s", want, body[strings.Index(body, `<div class="embeds">`):][:900])
		}
	}
	if strings.Contains(body, `class="file"`) {
		t.Error("a video is still offered as a file link")
	}
}

// TestWebArchive: a card with an archived copy grows a strip at its
// foot, inside the card, dated from the copy's timestamp and opening in
// a new tab; the JSON read carries the copy too.
func TestWebArchive(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	id := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "so good https://a.example/x"})
	if _, err := s.St.SetCard(id, store.Card{URL: "https://a.example/x", Host: "a.example", Title: "A Page"}, 0, "", true); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	if _, body := get(t, h, "/"); strings.Contains(body, `class="arch"`) {
		t.Error("archive strip before there is a copy")
	}
	const copyURL = "https://web.archive.org/web/20180523210631/https://a.example/x"
	if err := s.St.SetArchive(id, copyURL); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, h, "/")
	if want := `<span class="ch">a.example</span></span></a><a class="arch" href="` + copyURL + `" target="_blank" rel="noopener nofollow">Archived copy · 2018-05-23</a></div>`; !strings.Contains(body, want) {
		t.Errorf("archive strip missing:\n%s", body[strings.Index(body, `<div class="card">`):][:500])
	}
	if _, js := get(t, h, "/v1/post/"+id); !strings.Contains(js, `"archive":"`+copyURL+`"`) {
		t.Errorf("JSON card without the copy: %s", js)
	}
}

// TestWebPage: an HTML embed from an admin key renders as a page card
// that the script opens in a sandboxed window, with a download link
// beside it; the same file from any other key stays a file link, and
// /v1/embed keeps serving it as an attachment either way.
func TestWebPage(t *testing.T) {
	apub, apriv, _ := ed25519.GenerateKey(nil)
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}, Admins: []string{identity.Fingerprint(apub)}})
	const cid = "bafybeib6ecas7sqgsfw6x5zcveome3237tyzzenjffngrdg6r4o45pfnzq"
	if err := s.St.AddPin(cid, 4321, "text/html; charset=utf-8", false); err != nil {
		t.Fatal(err)
	}
	ingest(t, s, apriv, apub, 1, "post.create", map[string]any{"text": "a page",
		"embeds": []map[string]any{{"cid": cid, "mime": "text/html; charset=utf-8", "filename": "Sheet.html"}}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "a file",
		"embeds": []map[string]any{{"cid": cid, "mime": "text/html; charset=utf-8", "filename": "Sheet.html"}}})
	_, body := get(t, s.Handler(), "/")
	if !strings.Contains(body, `<a class="page" href="/v1/embed/`+cid+`" data-cid="`+cid+`" data-name="Sheet.html">Sheet.html</a><a class="pagedl" href="/v1/embed/`+cid+`" download>download</a>`) {
		t.Errorf("admin page card wrong:\n%s", body[strings.Index(body, `<div class="embeds">`):][:400])
	}
	if !strings.Contains(body, `<a class="file" href="/v1/embed/`+cid+`">Sheet.html (text/html; charset=utf-8)</a>`) {
		t.Error("a non-admin's HTML embed should stay a file link")
	}
	for _, want := range []string{`sandbox="allow-scripts allow-popups allow-forms allow-modals"`, `closest("a.pic, a.page")`, `#page=`,
		`'<base href="about:srcdoc">'`, `.srcdoc = withBase(text)`} {
		if !strings.Contains(body, want) {
			t.Errorf("page script missing %q", want)
		}
	}
	if strings.Contains(body, `allow-same-origin`) {
		t.Error("a page must never get allow-same-origin")
	}
	// the zoom glyph is positioned inside its box, not at the window's
	// corner over the close box
	for _, want := range []string{`.tbox { position: relative;`, `.viewer.pageview .tbox.zoom::after { content: ""; position: absolute; left: -1px; top: -1px; width: 7px; height: 7px;`, `box-sizing: border-box; border: 1px solid #262626; }`} {
		if !strings.Contains(body, want) {
			t.Errorf("page window CSS lacks %q", want)
		}
	}
	// the JSON reads carry the same decision, for the Hub app
	_, js := get(t, s.Handler(), "/v1/feed")
	var feed struct {
		Posts []struct {
			Text  string   `json:"text"`
			Pages []string `json:"pages"`
		} `json:"posts"`
	}
	if err := json.Unmarshal([]byte(js), &feed); err != nil {
		t.Fatal(err)
	}
	for _, p := range feed.Posts {
		want := ""
		if p.Text == "a page" {
			want = cid
		}
		if got := strings.Join(p.Pages, ","); got != want {
			t.Errorf("%q: pages = %q, want %q", p.Text, got, want)
		}
	}
}

// TestWebLive: the home page's first page and a thread ship the live
// script when the hub has an event bus, each marking the frame it keeps
// current; cursor pages, profiles and a hub without a bus stay static.
func TestWebLive(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	var ids []string
	for i := int64(1); i <= webPage+1; i++ {
		ids = append(ids, ingest(t, s, priv, pub, i, "post.create", map[string]any{"text": fmt.Sprintf("post %d", i)}))
	}
	const live = `new EventSource("/v1/events")`
	reply := ingest(t, s, priv, pub, webPage+2, "post.create", map[string]any{"text": "a reply", "reply_to": ids[0]})
	for _, path := range []string{"/", "/p/" + ids[0]} {
		if _, body := get(t, s.Handler(), path); strings.Contains(body, live) || strings.Contains(body, "data-live") {
			t.Errorf("%s: live on a hub without an event bus", path)
		}
	}
	s.Events = events.New()
	h := s.Handler()
	if _, body := get(t, h, "/"); !strings.Contains(body, live) || !strings.Contains(body, `<div class="frame" data-live="feed">`) {
		t.Error("first page lacks the live script or its frame's mark")
	}
	// a thread, and a reply's own page (the tree under it)
	for _, id := range []string{ids[0], reply} {
		if _, body := get(t, h, "/p/"+id); !strings.Contains(body, live) || !strings.Contains(body, `<div class="frame" data-live="thread">`) {
			t.Errorf("/p/%s lacks the live script or its frame's mark", id[:8])
		}
	}
	// the live script reads the frame before another script rewrites it
	if _, body := get(t, h, "/p/"+ids[0]); strings.Index(body, live) > strings.Index(body, "localTimes(document)") {
		t.Error("the live script stands after the local-time rewrite")
	}
	if _, body := get(t, h, "/?before="+ids[1]); strings.Contains(body, live) {
		t.Error("live script on an older page")
	}
	if _, body := get(t, h, "/?after="+ids[0]); strings.Contains(body, live) {
		t.Error("live script on a newer page")
	}
	if _, body := get(t, h, "/u/"+strings.Repeat("0", 16)); strings.Contains(body, live) {
		t.Error("live script on a profile page")
	}
}

// TestWebIcons: the shortcut, touch and manifest icons are served, each
// PNG at the size the manifest declares, and the pages point at them.
func TestWebIcons(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	h := s.Handler()
	for _, c := range []struct {
		path, mime, magic string
		size              int // a PNG's width and height
	}{
		{"/favicon.ico", "image/x-icon", "\x00\x00\x01\x00", 0},
		{"/apple-touch-icon.png", "image/png", "\x89PNG", 180},
		{"/apple-touch-icon-precomposed.png", "image/png", "\x89PNG", 180},
		{"/icon-192.png", "image/png", "\x89PNG", 192},
		{"/icon-512.png", "image/png", "\x89PNG", 512},
		{"/icon-maskable-512.png", "image/png", "\x89PNG", 512},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://hub.example"+c.path, nil))
		if w.Code != 200 || w.Header().Get("Content-Type") != c.mime || !strings.HasPrefix(w.Body.String(), c.magic) {
			t.Errorf("%s: %d %s %q", c.path, w.Code, w.Header().Get("Content-Type"), w.Body.String()[:4])
		}
		if c.size == 0 {
			continue
		}
		if cfg, err := png.DecodeConfig(w.Body); err != nil || cfg.Width != c.size || cfg.Height != c.size {
			t.Errorf("%s: %dx%d %v, want %d square", c.path, cfg.Width, cfg.Height, err, c.size)
		}
	}
	_, body := get(t, h, "/")
	if !strings.Contains(body, `<link rel="icon" href="/favicon.ico"`) || !strings.Contains(body, `<link rel="apple-touch-icon" href="/apple-touch-icon.png">`) ||
		!strings.Contains(body, `<meta property="og:image" content="http://hub.example/v1/preview/home.png">`) {
		t.Error("page lacks the icon links")
	}
}

// TestWebManifest: the pages are installable — they link a manifest
// named for the host, asking for a standalone window with the 192 and
// 512 icons and a maskable one — and, installed, the home page hides
// the join block by the display-mode media feature.
func TestWebManifest(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	h := s.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://hub.example/manifest.webmanifest", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var m struct {
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		Start     string `json:"start_url"`
		Scope     string `json:"scope"`
		Display   string `json:"display"`
		Theme     string `json:"theme_color"`
		Icons     []struct{ Src, Sizes, Type, Purpose string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "hub.example" || m.ShortName != "hub.example" || m.Start != "/" || m.Scope != "/" || m.Display != "standalone" || m.Theme != "#cccccc" {
		t.Errorf("manifest: %+v", m)
	}
	want := map[string]string{"/icon-192.png": "192x192", "/icon-512.png": "512x512", "/icon-maskable-512.png": "512x512"}
	for _, ic := range m.Icons {
		if want[ic.Src] != ic.Sizes || ic.Type != "image/png" || (ic.Purpose == "maskable") != strings.Contains(ic.Src, "maskable") {
			t.Errorf("icon %+v", ic)
		}
		delete(want, ic.Src)
	}
	if len(want) > 0 {
		t.Errorf("manifest lacks icons %v", want)
	}
	_, body := get(t, h, "/")
	if !strings.Contains(body, `<link rel="manifest" href="/manifest.webmanifest">`) {
		t.Error("page lacks the manifest link")
	}
	if !strings.Contains(body, "@media (display-mode: standalone), (display-mode: fullscreen), (display-mode: minimal-ui) {\n  .desk .join { display: none; }") {
		t.Error("installed, the page does not hide the join block")
	}
}

func postJSON(t *testing.T, h http.Handler, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "http://hub.example"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestWebPush: on a hub with a push key the home page carries the head
// script, the Notify bell with the key, and the push-only service worker;
// subscribing takes a browser's subscription and nothing else, and
// unsubscribing forgets it. Without a key, none of it.
func TestWebPush(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	h := s.Handler()
	if _, body := get(t, h, "/"); strings.Contains(body, `id="notify"`) || strings.Contains(body, `classList.add("push")`) {
		t.Error("Notify bell on a hub without push")
	}
	if code, _ := postJSON(t, h, "/v1/push/subscribe", `{}`); code != 404 {
		t.Errorf("subscribe without push: %d", code)
	}

	var err error
	if s.Push, err = push.LoadKey(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if s.Hub, err = identity.Load(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s.Events = events.New() // the first page is live, and its swap must keep the strip
	key := s.Push.Public()
	_, body := get(t, h, "/")
	for _, want := range []string{`classList.add("push")`, `<button type="button" class="btn bell" id="notify" aria-pressed="false"`, `data-key="` + key + `"`, `<svg viewBox="0 0 14 14"`, `navigator.serviceWorker.register("/sw.js")`, `k === "find"`} {
		if !strings.Contains(body, want) {
			t.Errorf("home page lacks %q", want)
		}
	}
	if _, body := get(t, h, "/search?q=x"); strings.Contains(body, `id="notify"`) {
		t.Error("Notify bell on the search page")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://hub.example/sw.js", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || !strings.Contains(w.Body.String(), `addEventListener("push"`) || strings.Contains(w.Body.String(), `"fetch"`) {
		t.Errorf("sw.js: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	_, hub := get(t, h, "/v1/hub")
	if !strings.Contains(hub, `"push":{"key":"`+key+`"}`) {
		t.Errorf("/v1/hub lacks the push key: %s", hub)
	}

	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	p256dh := base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())
	auth := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	if code, body := postJSON(t, h, "/v1/push/subscribe", `{"endpoint":"http://push.example/x","keys":{"p256dh":"`+p256dh+`","auth":"`+auth+`"}}`); code != 400 {
		t.Errorf("http endpoint: %d %s", code, body)
	}
	sub := `{"endpoint":"https://push.example/v1/abc","expirationTime":null,"keys":{"p256dh":"` + p256dh + `","auth":"` + auth + `"}}`
	if code, body := postJSON(t, h, "/v1/push/subscribe", sub); code != 200 {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	subs, _ := s.St.PushSubs()
	if len(subs) != 1 || subs[0].Endpoint != "https://push.example/v1/abc" || subs[0].Base != "http://hub.example" || len(subs[0].P256dh) != 65 {
		t.Fatalf("stored: %+v", subs)
	}
	postJSON(t, h, "/v1/push/subscribe", sub) // again: the same one, not a second
	if n, _ := s.St.PushCount(); n != 1 {
		t.Errorf("resubscribe counted twice: %d", n)
	}
	if code, _ := postJSON(t, h, "/v1/push/unsubscribe", `{"endpoint":"https://push.example/v1/abc"}`); code != 200 {
		t.Errorf("unsubscribe: %d", code)
	}
	if n, _ := s.St.PushCount(); n != 0 {
		t.Errorf("still subscribed: %d", n)
	}
}

// TestWebCompose: the wallet strip is on the home page's first page and on
// every thread (replying to the post the page shows), never on an older
// page, a profile or search; the script asks only for message signatures.
func TestWebCompose(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	var ids []string
	for i := int64(1); i <= webPage+1; i++ {
		ids = append(ids, ingest(t, s, priv, pub, i, "post.create", map[string]any{"text": fmt.Sprintf("post %d", i)}))
	}
	h := s.Handler()
	_, home := get(t, h, "/")
	// its own window, above the feed's, both in the desk's main column
	if a, b, c := strings.Index(home, `<div class="main">`), strings.Index(home, `<div class="window composer">`), strings.Index(home, `<div class="window feed">`); a < 0 || !(a < b && b < c) {
		t.Errorf("home: main %d, composer window %d, feed window %d — want the composer window first in the main column", a, b, c)
	}
	for _, want := range []string{`<span class="title">Post</span>`, `<div class="frame"><div class="compose" id="compose">`, `Sign in with Solana`, `classList.add("wallet")`,
		`class="btn profile-btn">Profile…</button>`, `<span class="grow who-wrap"><a class="me-av" aria-label="Your page" hidden><img alt=""></a><span class="who"><b class="name">`, `role="dialog" aria-modal="true" aria-label="Profile"`,
		`<button type="button" class="btn left p-edit">Edit</button>`, `class="btn p-choose" hidden>Choose Picture…</button>`,
		`accept="image/png,image/jpeg,image/gif"`, `"/v1/avatar"`, `"exe-hub:v1\nupload\n"`,
		`"solana:signMessage"`, `wallet-standard:app-ready`, `"/v1/gate?author="`, `From a Solana wallet:`,
		// a phone without a wallet gets no Post window: hidden under a coarse pointer unless a wallet is there or signed in before
		"@media (pointer: coarse) {\n  .js .composer { display: none; }\n  .js.solana .composer, .js.wallet .composer { display: block; }\n}", `root.classList.add("solana")`} {
		if !strings.Contains(home, want) {
			t.Errorf("home page lacks %q", want)
		}
	}
	for _, never := range []string{"signTransaction", "signAndSendTransaction"} {
		if strings.Contains(home, never) {
			t.Errorf("the page mentions %s", never)
		}
	}
	_, thread := get(t, h, "/p/"+ids[0])
	if !strings.Contains(thread, `<div class="frame"><div class="compose" id="compose" data-reply-to="`+ids[0]+`">`) {
		t.Error("thread page lacks the reply window")
	}
	if b, c := strings.Index(thread, `<span class="title">Reply</span>`), strings.Index(thread, `title="Back to the feed"`); b < 0 || b > c {
		t.Errorf("thread: the Reply window (%d) should come before the thread window (%d)", b, c)
	}
	for _, path := range []string{"/?before=" + ids[1], "/u/" + identity.Fingerprint(pub), "/search?q=post"} {
		if _, body := get(t, h, path); strings.Contains(body, `id="compose"`) {
			t.Errorf("%s carries the compose strip", path)
		}
	}
}

// TestWebPreview: every page carries a full link preview — site name,
// canonical address, a 1200×630 picture drawn from the page with its
// size and alt text, the Twitter card kind, a thread's time and author
// — and the pictures themselves are PNGs of that size: a post's words,
// a profile, the hub. A post with a picture keeps the picture.
func TestWebPreview(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	h := s.Handler()
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "profile.set", map[string]any{"name": "Livid", "bio": "Makes things."})
	id := ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "# Hello\n\nexe 功能一览 · a post with words"})
	posts, _ := s.St.Feed("", 10, true)
	author := posts[0].Author
	_, body := get(t, h, "/p/"+id)
	for _, want := range []string{
		`<link rel="canonical" href="http://hub.example/p/` + id + `">`,
		`<meta property="og:site_name" content="hub.example">`,
		`<meta property="og:type" content="article">`,
		`<meta property="og:image" content="http://hub.example/v1/preview/post/` + id + `.png">`,
		`<meta property="og:image:width" content="1200">`,
		`<meta property="og:image:height" content="630">`,
		`<meta property="og:image:alt" content="Livid on hub.example: Hello exe 功能一览 · a post with words">`,
		`<meta name="twitter:card" content="summary_large_image">`,
		`<title>Hello — Livid</title>`,
		`<meta property="og:title" content="Hello — Livid">`,
		`<meta name="twitter:title" content="Hello — Livid">`,
		`<meta name="twitter:image" content="http://hub.example/v1/preview/post/` + id + `.png">`,
		`<meta property="article:published_time" content="` + webStamp(posts[0].TS) + `">`,
		`<meta property="article:author" content="http://hub.example/u/` + author + `">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("thread page lacks %s", want)
		}
	}
	_, body = get(t, h, "/u/"+author)
	for _, want := range []string{
		`<link rel="canonical" href="http://hub.example/u/` + author + `">`,
		`<meta property="og:type" content="profile">`,
		`<meta property="og:image" content="http://hub.example/v1/preview/profile/` + author + `.png">`,
		`<meta name="twitter:card" content="summary_large_image">`,
		`<meta property="profile:username" content="Livid">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("profile page lacks %s", want)
		}
	}
	_, body = get(t, h, "/")
	if !strings.Contains(body, `<link rel="canonical" href="http://hub.example/">`) || !strings.Contains(body, `<meta name="twitter:card" content="summary_large_image">`) {
		t.Error("home page lacks its preview")
	}
	// the search page keeps the small icon and says so
	_, body = get(t, h, "/search?q=x")
	if !strings.Contains(body, `<meta name="twitter:card" content="summary">`) || strings.Contains(body, `rel="canonical"`) {
		t.Error("search page preview")
	}
	// the pictures
	for _, path := range []string{"/v1/preview/post/" + id + ".png", "/v1/preview/post/" + id, "/v1/preview/profile/" + author + ".png", "/v1/preview/home.png"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://hub.example"+path, nil))
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("ETag") == "" {
			t.Errorf("%s: %d %s", path, w.Code, w.Header().Get("Content-Type"))
			continue
		}
		if cfg, err := png.DecodeConfig(w.Body); err != nil || cfg.Width != 1200 || cfg.Height != 630 {
			t.Errorf("%s: %dx%d %v", path, cfg.Width, cfg.Height, err)
		}
	}
	for _, path := range []string{"/v1/preview/post/" + strings.Repeat("0", 64) + ".png", "/v1/preview/profile/nobody.png"} {
		if code, _ := get(t, h, path); code != 404 {
			t.Errorf("%s: %d, want 404", path, code)
		}
	}
	// a post with a picture keeps it, with the size the embed declares
	cid := strings.Repeat("b", 59)
	if err := s.St.AddPin(cid, 1000, "image/png", false); err != nil {
		t.Fatal(err)
	}
	pic := ingest(t, s, priv, pub, 3, "post.create", map[string]any{"text": "", "embeds": []map[string]any{{"cid": cid, "mime": "image/png", "width": 800, "height": 600, "alt": "a cat"}}})
	_, body = get(t, h, "/p/"+pic)
	for _, want := range []string{
		`<meta property="og:image" content="http://hub.example/v1/embed/` + cid + `">`,
		`<meta property="og:image:width" content="800">`,
		`<meta property="og:image:alt" content="a cat">`,
		`<meta property="og:description" content="A picture. By Livid on hub.example.">`,
		`<title>Livid on hub.example</title>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("picture post lacks %s", want)
		}
	}
}

// TestWebIdenticon: someone with no picture wears the face drawn from
// their id — on a post's row, on their page, on their cards — and
// someone with a picture wears that.
func TestWebIdenticon(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	h := s.Handler()
	pub, priv, _ := ed25519.GenerateKey(nil)
	id := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "no picture yet"})
	posts, _ := s.St.Feed("", 10, true)
	author := posts[0].Author
	face := `<img class="idn" src="/v1/identicon/` + author + `.svg" alt="">`
	for _, path := range []string{"/", "/p/" + id, "/u/" + author} {
		if _, body := get(t, h, path); strings.Count(body, face) == 0 {
			t.Errorf("%s: no face for a key without a picture", path)
		}
	}
	if _, body := get(t, h, "/u/"+author); !strings.Contains(body, `<span class="av">`+face+`</span>`) {
		t.Error("the profile's head does not wear the face")
	}

	// the picture itself: the package's SVG, with or without the suffix
	for _, path := range []string{"/v1/identicon/" + author + ".svg", "/v1/identicon/" + author} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://hub.example"+path, nil))
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" || w.Body.String() != string(identicon.SVG(author)) {
			t.Errorf("%s: %d %s\n%s", path, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	for _, path := range []string{"/v1/identicon/nobody.svg", "/v1/identicon/" + strings.ToUpper(author) + ".svg", "/v1/identicon/" + author + "00.svg"} {
		if code, _ := get(t, h, path); code != 404 {
			t.Errorf("%s: %d, want 404", path, code)
		}
	}

	// the cards: the face at the picture's size, in its own colours
	for _, path := range []string{"/v1/preview/post/" + id + ".png", "/v1/preview/profile/" + author + ".png"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://hub.example"+path, nil))
		img, err := png.Decode(w.Body)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		ink, _ := identicon.Face(author)
		var ground, inked int
		for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
			for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
				switch r, g, b, _ := img.At(x, y).RGBA(); [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)} {
				case [3]uint8{identicon.Ground.R, identicon.Ground.G, identicon.Ground.B}:
					ground++
				case [3]uint8{ink.R, ink.G, ink.B}:
					inked++
				}
			}
		}
		_, on := identicon.Face(author)
		cells := 0
		for _, row := range on {
			for _, c := range row {
				if c {
					cells++
				}
			}
		}
		c := identicon.Cell(preview.Pic)
		if ground != preview.Pic*preview.Pic-cells*c*c || inked != cells*c*c {
			t.Errorf("%s: %d ground and %d ink pixels, want %d and %d", path, ground, inked, preview.Pic*preview.Pic-cells*c*c, cells*c*c)
		}
	}

	// a picture of their own takes the face's place
	cid := strings.Repeat("b", 59)
	if err := s.St.AddPin(cid, 1000, "image/png", true); err != nil {
		t.Fatal(err)
	}
	ingest(t, s, priv, pub, 2, "profile.set", map[string]any{"name": "Pictured", "avatar": cid})
	for _, path := range []string{"/", "/u/" + author} {
		if _, body := get(t, h, path); strings.Contains(body, face) || !strings.Contains(body, `<img src="/v1/embed/`+cid+`" alt="">`) {
			t.Errorf("%s: a profile with a picture still wears the face", path)
		}
	}
}

// TestOpening: the thread title's sentence — the first line with
// words, a heading's marks dropped, cut at a sentence end that ends a
// word (a dotted name is not one), trimmed near 70 characters at a
// word boundary, a closing full stop dropped; and excerpt never cuts a
// character in two.
func TestOpening(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Idea: every hub account gets a home page. Open a profile and beside it sits Home.", "Idea: every hub account gets a home page"},
		{"# Hello\n\nexe 功能一览 · more words", "Hello"},
		{"\n\n  \nSecond line first! Then this.", "Second line first!"},
		{"A profile.set field naming the page's CID? Yes, at hub.v2core.com.", "A profile.set field naming the page's CID?"},
		{"today at hub.v2core.com we shipped it", "today at hub.v2core.com we shipped it"},
		{"one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen", "one two three four five six seven eight nine ten eleven twelve…"},
		{"exe 功能一览。今天新增了价格提醒", "exe 功能一览"},
		{"Hmm... and then", "Hmm"},
		{"[The order property](https://developer.mozilla.org/order) moves the picture. Not the sequence.", "The order property moves the picture"},
		{"", ""},
		{"   ", ""},
	} {
		if got := opening(c.in); got != c.want {
			t.Errorf("opening(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	zh := strings.Repeat("字", 300)
	if got := excerpt(zh, 200); !utf8.ValidString(got) || utf8.RuneCountInString(got) != 201 {
		t.Errorf("excerpt cut a character: %d runes, valid %v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
	if got := excerpt("one two three", 8); got != "one two…" {
		t.Errorf("excerpt at a word: %q", got)
	}
}
