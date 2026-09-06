package api

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/events"
	"exehub/internal/identity"
	"exehub/internal/push"
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
}

// TestWebThreadProfile: a thread page shows the post and its replies, a
// profile page the author's posts; unknown ids render the 404 page.
func TestWebThreadProfile(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	root := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "root post"})
	ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "the reply", "reply_to": root})
	h := s.Handler()

	code, body := get(t, h, "/p/"+root)
	if code != 200 || !strings.Contains(body, "root post") || !strings.Contains(body, "the reply") || !strings.Contains(body, "1 reply<") {
		t.Errorf("thread page: %d\n%s", code, body)
	}
	if !strings.Contains(body, `<meta property="og:description" content="root post">`) {
		t.Error("thread page lacks the OpenGraph description")
	}
	if code, body := get(t, h, "/p/"+strings.Repeat("0", 64)); code != 404 || !strings.Contains(body, "No such post.") {
		t.Errorf("unknown post: %d\n%s", code, body)
	}
	_, home := get(t, h, "/")
	if strings.Contains(home, "the reply") {
		t.Error("a reply leaked into the home feed")
	}
	if !strings.Contains(home, "1 reply ▸") {
		t.Error("home lacks the reply count link")
	}

	// the profile id is the author's fingerprint, as the feed reports it
	posts, _ := s.St.Feed("", 10, true)
	author := posts[0].Author
	code, body = get(t, h, "/u/"+author)
	if code != 200 || !strings.Contains(body, "root post") || !strings.Contains(body, "the reply") || !strings.Contains(body, "<h1>"+author+"</h1>") {
		t.Errorf("profile page: %d\n%s", code, body)
	}
	// with a profile.set the page gains the name, count and date
	ingest(t, s, priv, pub, 3, "profile.set", map[string]any{"name": "Ann", "bio": "hi <there>"})
	code, body = get(t, h, "/u/"+author)
	if code != 200 || !strings.Contains(body, "<h1>Ann</h1>") || !strings.Contains(body, "· since 20") || !strings.Contains(body, `<span class="stats">2 posts</span>`) || !strings.Contains(body, "hi &lt;there&gt;") {
		t.Errorf("named profile page: %d %q", code, statusLine(body))
	}
	if code, _ := get(t, h, "/u/nobody"); code != 404 {
		t.Errorf("unknown profile = %d", code)
	}
}

// TestWebJoinToken: a token-gated hub's join block names the holding
// — raw base units until the RPC has told it the mint's decimals.
func TestWebJoinToken(t *testing.T) {
	zero := 0
	s := testServer(t, &config.Config{
		Gate: config.Gate{Mode: "token", Token: config.TokenGate{
			Mints:   []config.MintReq{{Mint: "9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump", MinAmount: "10000000000", MinRaw: 10000000000}},
			Recheck: "10m"}},
		Cooldown: &zero,
	})
	_, body := get(t, s.Handler(), "/")
	if !strings.Contains(body, "<b>10000000000 raw base units</b> of mint <code>9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump</code>") {
		t.Errorf("token join block wrong:\n%s", body)
	}
	if strings.Contains(body, "One post per") {
		t.Error("cooldown line shown for a hub with cooldown 0")
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
	}
	for _, c := range cases {
		if got := string(renderText(c.in)); got != c.want {
			t.Errorf("renderText(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

func TestExcerpt(t *testing.T) {
	if got := excerpt("one  two\nthree", 100); got != "one two three" {
		t.Errorf("excerpt = %q", got)
	}
	if got := excerpt("one two three four", 10); got != "one two…" {
		t.Errorf("excerpt cut = %q", got)
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
	if strings.Contains(body, "Prev") || !strings.Contains(body, `<a class="btn next" href="/?before=`+oldestOnFirst+`">Next &gt;</a>`) {
		t.Errorf("first page: %q", statusLine(body))
	}
	if !strings.Contains(body, `<span class="stats">1 members · 32 posts</span>`) {
		t.Errorf("stats: %q", statusLine(body))
	}

	_, body = get(t, h, "/?before="+oldestOnFirst)
	if !strings.Contains(body, "post 2<") || !strings.Contains(body, "post 1<") || strings.Contains(body, "post 3<") {
		t.Error("second page should hold posts 2 and 1 only")
	}
	if strings.Contains(body, "Next") || !strings.Contains(body, `<a class="btn prev" href="/?after=`+ids[1]+`">&lt; Prev</a>`) {
		t.Errorf("last page: %q", statusLine(body))
	}

	// Prev from the last page: the 30 posts newer than post 2 are exactly
	// the first page — back to its bare URL, where the feed is live
	if code, loc := redirect("/?after=" + ids[1]); code != 302 || loc != "/" {
		t.Errorf("full newer page at the top: %d %s", code, loc)
	}

	// 31 posts newer than post 1: the 30 nearest make a middle page, posts 31..2
	_, body = get(t, h, "/?after="+ids[0])
	if !strings.Contains(body, "post 31<") || !strings.Contains(body, "post 2<") || strings.Contains(body, "post 32<") || strings.Contains(body, "post 1<") ||
		!strings.Contains(body, `<a class="btn prev" href="/?after=`+ids[30]+`">&lt; Prev</a>`) ||
		!strings.Contains(body, `<a class="btn next" href="/?before=`+ids[1]+`">Next &gt;</a>`) {
		t.Errorf("middle newer page: %q", statusLine(body))
	}

	// only a few posts newer than post 20: back to the top page instead of a short one
	if code, loc := redirect("/?after=" + ids[19]); code != 302 || loc != "/" {
		t.Errorf("short newer page: %d %s", code, loc)
	}
	if code, _ := get(t, h, "/?before="+strings.Repeat("0", 64)); code != 404 {
		t.Errorf("unknown cursor = %d", code)
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
	if code != 200 || !strings.Contains(body, "apple pie<") || !strings.Contains(body, "Apple tart") || strings.Contains(body, "cherry") {
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
	if !strings.Contains(body, strip+`pie APPLE"`) || !strings.Contains(body, "apple pie<") || strings.Contains(body, "cherry") || strings.Contains(body, "Apple tart") ||
		!strings.Contains(body, `<span class="stats">1 post matches</span>`) {
		t.Error("two words: not the one post holding both")
	}

	// replies are found too, and shown as replies
	_, body = get(t, h, "/search?q=pie")
	if !strings.Contains(body, "pie again<") || !strings.Contains(body, `class="post reply"`) || !strings.Contains(body, `<span class="stats">3 posts match</span>`) {
		t.Error("the reply is missing from the pie results")
	}

	// the query is escaped wherever it lands, and matched literally
	_, body = get(t, h, "/search?q=%3Cb%3Ex")
	if !strings.Contains(body, strip+`&lt;b&gt;x"`) || !strings.Contains(body, `<title>Search: &lt;b&gt;x · hub.example</title>`) ||
		strings.Contains(body, "<b>x") || !strings.Contains(body, "Apple tart &lt;b&gt;x&lt;/b&gt;<") || !strings.Contains(body, "1 post matches") {
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
	if strings.Contains(body, "Prev") || !strings.Contains(body, `<a class="btn next" href="/search?q=hit%20me&amp;before=`+oldestOnFirst+`">Next &gt;</a>`) {
		t.Errorf("first page: %q", statusLine(body))
	}
	if !strings.Contains(body, `<span class="stats">32 posts match</span>`) || strings.Contains(body, "miss 99") {
		t.Errorf("stats: %q", statusLine(body))
	}
	_, body = get(t, h, "/search?q=hit+me&before="+oldestOnFirst)
	if !strings.Contains(body, "hit me 2<") || !strings.Contains(body, "hit me 1<") || strings.Contains(body, "hit me 3<") ||
		strings.Contains(body, "Next") || !strings.Contains(body, `<a class="btn prev" href="/search?q=hit%20me&amp;after=`+ids[1]+`">&lt; Prev</a>`) {
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

// statusLine is the page's pager strip, for short failure messages.
func statusLine(body string) string {
	i := strings.Index(body, `<div class="pager">`)
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
	if !strings.Contains(body, `closest("a.pic")`) {
		t.Error("viewer script missing")
	}
}

// TestWebLive: the home page's first page ships the live-feed script
// when the hub has an event bus; cursor pages and a hub without one
// stay static.
func TestWebLive(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	var ids []string
	for i := int64(1); i <= webPage+1; i++ {
		ids = append(ids, ingest(t, s, priv, pub, i, "post.create", map[string]any{"text": fmt.Sprintf("post %d", i)}))
	}
	const live = `new EventSource("/v1/events")`
	if _, body := get(t, s.Handler(), "/"); strings.Contains(body, live) {
		t.Error("live script on a hub without an event bus")
	}
	s.Events = events.New()
	h := s.Handler()
	if _, body := get(t, h, "/"); !strings.Contains(body, live) {
		t.Error("first page lacks the live script")
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
		!strings.Contains(body, `<meta property="og:image" content="http://hub.example/apple-touch-icon.png">`) {
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
	for _, want := range []string{`classList.add("push")`, `<button type="button" class="btn bell" id="notify" aria-pressed="false"`, `data-key="` + key + `"`, `<svg viewBox="0 0 14 14"`, `navigator.serviceWorker.register("/sw.js")`, `n.id === "find"`} {
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
