package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exehub/internal/config"
	"exehub/internal/envelope"
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
		`<a href="https://example.com/a" rel="noopener nofollow">https://example.com/a</a>.`, // the period stays outside
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
	if strings.Contains(body, `class="btn`) {
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
		{"go to https://x.y/z?q=1, now", `go to <a href="https://x.y/z?q=1" rel="noopener nofollow">https://x.y/z?q=1</a>, now`},
		{"(see https://x.y/)", `(see <a href="https://x.y/" rel="noopener nofollow">https://x.y/</a>)`},
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

// TestWebPaging: 31 posts make two pages. The first page has Next only,
// the last has Prev only, Prev from the last page lands on a full page
// with no Prev of its own, and a short newer page redirects to the top.
func TestWebPaging(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "profile.set", map[string]any{"name": "Ann"})
	var ids []string // oldest first
	for i := int64(1); i <= webPage+1; i++ {
		ids = append(ids, ingest(t, s, priv, pub, i+1, "post.create", map[string]any{"text": fmt.Sprintf("post %d", i)}))
	}
	h := s.Handler()
	oldestOnFirst := ids[1] // 31 posts newest-first: the first page ends at post 2

	_, body := get(t, h, "/")
	if strings.Contains(body, "Prev") || !strings.Contains(body, `<a class="btn next" href="/?before=`+oldestOnFirst+`">Next &gt;</a>`) {
		t.Errorf("first page: %q", statusLine(body))
	}
	if !strings.Contains(body, `<span class="stats">1 members · 31 posts</span>`) {
		t.Errorf("stats: %q", statusLine(body))
	}

	_, body = get(t, h, "/?before="+oldestOnFirst)
	if !strings.Contains(body, "post 1<") || strings.Contains(body, "post 2<") {
		t.Error("second page should hold post 1 only")
	}
	if strings.Contains(body, "Next") || !strings.Contains(body, `<a class="btn prev" href="/?after=`+ids[0]+`">&lt; Prev</a>`) {
		t.Errorf("last page: %q", statusLine(body))
	}

	// Prev from the last page: the 30 posts newer than post 1 — a full page, nothing newer
	_, body = get(t, h, "/?after="+ids[0])
	if strings.Contains(body, "Prev") || !strings.Contains(body, "post 31<") || !strings.Contains(body, "post 2<") ||
		!strings.Contains(body, `<a class="btn next" href="/?before=`+oldestOnFirst+`">Next &gt;</a>`) {
		t.Errorf("newer page: %q", statusLine(body))
	}

	// only a few posts newer than post 20: back to the top page instead of a short one
	req := httptest.NewRequest("GET", "http://hub.example/?after="+ids[19], nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 302 || w.Header().Get("Location") != "/" {
		t.Errorf("short newer page: %d %s", w.Code, w.Header().Get("Location"))
	}
	if code, _ := get(t, h, "/?before="+strings.Repeat("0", 64)); code != 404 {
		t.Errorf("unknown cursor = %d", code)
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
