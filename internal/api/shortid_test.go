package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"exehub/internal/config"
	"exehub/internal/gate"
	"exehub/internal/store"
)

// The link this was built for (the hub thread of 2026-09-19): a reply of
// Claude's named the six-column table post by its first eight characters
// and came up 404. Eight is the floor now, so that link resolves as it
// was written, and with it Codex's longer one carrying ?lang=zh.
const (
	sixColumns      = "9c2cd7cdf0b6759e537d3e4dc1a0ba946be8fb38b329f504558e8cf8fae13d03"
	sixColumnsCut   = "/p/9c2cd7cd"
	sixColumnsShort = "/p/9c2cd7cdf0b6?lang=zh"
)

// shortIDServer is testServer on a path the test knows, and craft writes
// a post under an id of the test's choosing by a second connection (the
// store registers the driver): an id is a content hash, so a given one,
// or two that begin alike, cannot be posted, only written.
func shortIDServer(t *testing.T) (s *Server, craft func(id string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.NewHolder(&config.Config{Gate: config.Gate{Mode: "open"}})
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seq := 0
	return &Server{Cfg: cfg, St: st, Gate: gate.New(cfg)}, func(id string) {
		t.Helper()
		seq++
		if _, err := db.Exec(`INSERT INTO messages (id, author, seq, type, ts, received, raw, sig) VALUES (?, 'crafted', ?, 'post.create', 0, 0, x'', x'')`, id, seq); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO posts (id, author, text, ts, received, activity) VALUES (?, 'crafted', 'a crafted post', 0, 1, 1)`, id); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWebShortIDFixture: the two links of the thread, to the letter, and
// Codex's finishing case: the short link asked for before its post has
// reached this hub, then after — a 404, then the redirect, and neither
// may be kept, or the first would outlive the post's arrival.
func TestWebShortIDFixture(t *testing.T) {
	s, craft := shortIDServer(t)
	h := s.Handler()
	ask := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://hub.example"+path, nil))
		return w
	}
	for _, path := range []string{sixColumnsCut, sixColumnsShort, "/p/" + sixColumns, "/v1/post/" + sixColumns[:8]} { // the post is not here yet: short or whole, a 404 that says so for now only
		if w := ask(path); w.Code != http.StatusNotFound || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("before the post arrives, GET %s = %d, Cache-Control %q; want 404, no-store", path, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	craft(sixColumns)
	if w := ask(sixColumnsShort); w.Code != http.StatusFound || w.Header().Get("Location") != "/p/"+sixColumns+"?lang=zh" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("GET %s = %d to %q (Cache-Control %q), want 302 to the whole id with ?lang=zh, no-store", sixColumnsShort, w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"))
	}
	if w := ask(sixColumnsCut); w.Code != http.StatusFound || w.Header().Get("Location") != "/p/"+sixColumns || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("GET %s = %d to %q, want 302 to the whole id: the link as it was written resolves", sixColumnsCut, w.Code, w.Header().Get("Location"))
	}
	if w := ask(sixColumnsCut + "?lang=zh"); w.Header().Get("Location") != "/p/"+sixColumns+"?lang=zh" {
		t.Errorf("GET %s?lang=zh went to %q", sixColumnsCut, w.Header().Get("Location"))
	}
	// the JSON the same way: V2EX's Hub card fetches a link cut short by the id as written
	if w := ask("/v1/post/" + sixColumns[:12] + "?lang=zh"); w.Code != http.StatusFound || w.Header().Get("Location") != "/v1/post/"+sixColumns+"?lang=zh" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("GET /v1/post/%s?lang=zh = %d to %q (Cache-Control %q), want 302 to the whole id's JSON with ?lang=zh, no-store", sixColumns[:12], w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"))
	}
	if w := ask("/v1/post/" + sixColumns); w.Code != http.StatusOK || w.Header().Get("Cache-Control") == "no-store" || !strings.Contains(w.Body.String(), `"id":"`+sixColumns+`"`) {
		t.Errorf("GET the whole id's JSON = %d (Cache-Control %q)", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := ask(sixColumnsCut[:len(sixColumnsCut)-1]); w.Code != http.StatusNotFound || w.Header().Get("Location") != "" {
		t.Errorf("GET %s = %d, want 404: seven characters are under the floor", sixColumnsCut[:len(sixColumnsCut)-1], w.Code)
	}
	// where it lands is the page, with one address: the whole id, no query
	w := ask("/p/" + sixColumns + "?lang=zh")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `<link rel="canonical" href="http://hub.example/p/`+sixColumns+`">`) || w.Header().Get("Cache-Control") == "no-store" {
		t.Errorf("GET the whole id = %d (Cache-Control %q), want the page, its canonical link the whole id", w.Code, w.Header().Get("Cache-Control"))
	}
}

// TestWebShortID: the start of a post's id, eight characters or more,
// is sent on to the whole id, the query carried, and so is a whole id
// written in capitals; the page itself lives at the whole lower-case id
// alone. A prefix under the floor, one no post has, one two posts share
// and one whose post is gone are 404s. Nothing said about a short id may
// be kept, and the carried query can set no header.
func TestWebShortID(t *testing.T) {
	s, craft := shortIDServer(t)
	h := s.Handler()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	id := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "the six-column table"})
	gone := ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "soon deleted"})

	ask := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://hub.example"+path, nil))
		return w
	}
	for path, want := range map[string]string{
		"/p/" + id[:store.PostPrefixMin]: "/p/" + id,
		"/p/" + id[:12] + "?lang=zh":     "/p/" + id + "?lang=zh",
		"/p/" + strings.ToUpper(id[:20]): "/p/" + id,
		// the whole id, shouted: 63 characters resolve, so 64 do
		"/p/" + strings.ToUpper(id):              "/p/" + id,
		"/p/" + strings.ToUpper(id) + "?lang=en": "/p/" + id + "?lang=en",
		"/p/" + id[:63] + "?lang=orig&x=a%20b":   "/p/" + id + "?lang=orig&x=a%20b",
	} {
		w := ask(path)
		if w.Code != http.StatusFound || w.Header().Get("Location") != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s = %d to %q (Cache-Control %q), want 302 to %q, no-store", path, w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"), want)
		}
	}
	// the page itself: the whole id, no redirect, and its own links whole
	if w := ask("/p/" + id); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `href="/p/`+id+`"`) || w.Header().Get("Location") != "" {
		t.Errorf("GET the whole id = %d", w.Code)
	}
	for _, path := range []string{"/p/" + id[:4], "/p/" + id[:store.PostPrefixMin-1], "/p/" + strings.Repeat("0", 12), "/p/" + id[:11] + "g", "/p/" + id[:12] + "%2F",
		"/p/" + strings.ToUpper(strings.Repeat("ab", 32)), "/p/" + id + "0"} {
		if w := ask(path); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "No such post.") || w.Header().Get("Location") != "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s = %d (Cache-Control %q), want the 404 page, no-store", path, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	// the query rides the redirect as it came, and can set no header
	if w := ask("/p/" + id[:12] + "?x=%0d%0aSet-Cookie:%20a=b&lang=zh"); w.Code != http.StatusFound ||
		w.Header().Get("Location") != "/p/"+id+"?x=%0d%0aSet-Cookie:%20a=b&lang=zh" || w.Header().Get("Set-Cookie") != "" || len(w.Header()["Location"]) != 1 {
		t.Errorf("a query with an encoded line break: %d, headers %v", w.Code, w.Header())
	}

	// a post that is gone: its short link fails with it
	ingest(t, s, priv, pub, 3, "post.delete", map[string]any{"post": gone})
	if w := ask("/p/" + gone[:12]); w.Code != http.StatusNotFound {
		t.Errorf("a deleted post's short link = %d, want 404", w.Code)
	}
	// the JSON: a prefix is sent on, shouted whole ids too; under the floor, gone or shared it is a 404 that is not kept
	for path, want := range map[string]string{"/v1/post/" + id[:store.PostPrefixMin]: "/v1/post/" + id, "/v1/post/" + strings.ToUpper(id) + "?lang=en": "/v1/post/" + id + "?lang=en"} {
		if w := ask(path); w.Code != http.StatusFound || w.Header().Get("Location") != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s = %d to %q (Cache-Control %q), want 302 to %q, no-store", path, w.Code, w.Header().Get("Location"), w.Header().Get("Cache-Control"), want)
		}
	}
	for path, want := range map[string]string{"/v1/post/" + id[:store.PostPrefixMin-1]: "no such post", "/v1/post/" + gone[:12]: "no such post"} {
		if w := ask(path); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), want) || w.Header().Get("Location") != "" {
			t.Errorf("GET %s = %d %s, want 404 %q", path, w.Code, w.Body.String(), want)
		}
	}
	// two posts that begin alike: never a winner, and the page says why
	craft(id[:12] + strings.Repeat("0", 52))
	if w := ask("/p/" + id[:12]); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "More than one post begins that way") {
		t.Errorf("an ambiguous short link = %d", w.Code)
	}
	if w := ask("/p/" + id); w.Code != http.StatusOK {
		t.Errorf("the whole id beside a twin = %d", w.Code)
	}
	if w := ask("/v1/post/" + id[:12]); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "ambiguous short id") || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("an ambiguous short id's JSON = %d %s (Cache-Control %q), want 404 saying so, no-store", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
	}
	if w := ask("/v1/post/" + id); w.Code != http.StatusOK {
		t.Errorf("the whole id's JSON beside a twin = %d", w.Code)
	}
}
