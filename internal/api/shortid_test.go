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

// TestWebShortID: the start of a post's id, twelve characters or more,
// is sent on to the whole id — a 302 nothing may keep, the query carried
// — and the page itself lives at the whole id alone. A prefix under the
// floor (the eight characters a link of Claude's was once cut to), one
// no post has, one two posts share and one whose post is gone are 404s.
func TestWebShortID(t *testing.T) {
	// testServer's, on a path the test knows: two ids that begin alike
	// cannot be posted, only written, so the twin goes in by a second
	// connection (the store registers the driver)
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.NewHolder(&config.Config{Gate: config.Gate{Mode: "open"}})
	s := &Server{Cfg: cfg, St: st, Gate: gate.New(cfg)}
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
		"/p/" + id[:12]:                        "/p/" + id,
		"/p/" + id[:12] + "?lang=zh":           "/p/" + id + "?lang=zh",
		"/p/" + strings.ToUpper(id[:20]):       "/p/" + id,
		"/p/" + id[:63] + "?lang=orig&x=a%20b": "/p/" + id + "?lang=orig&x=a%20b",
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
	for _, path := range []string{"/p/" + id[:8], "/p/" + id[:11], "/p/" + strings.Repeat("0", 12), "/p/" + id[:11] + "g", "/p/" + id[:12] + "%2F"} {
		if w := ask(path); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "No such post.") || w.Header().Get("Location") != "" {
			t.Errorf("GET %s = %d, want the 404 page", path, w.Code)
		}
	}

	// a post that is gone: its short link fails with it
	ingest(t, s, priv, pub, 3, "post.delete", map[string]any{"post": gone})
	if w := ask("/p/" + gone[:12]); w.Code != http.StatusNotFound {
		t.Errorf("a deleted post's short link = %d, want 404", w.Code)
	}
	// two posts that begin alike: never a winner, and the page says why
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	twin := id[:12] + strings.Repeat("0", 52)
	if _, err := db.Exec(`INSERT INTO messages (id, author, seq, type, ts, received, raw, sig) VALUES (?, 'crafted', 1, 'post.create', 0, 0, x'', x'')`, twin); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO posts (id, author, text, ts, received, activity) VALUES (?, 'crafted', 'twin', 0, 1, 1)`, twin); err != nil {
		t.Fatal(err)
	}
	if w := ask("/p/" + id[:12]); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "More than one post begins that way") {
		t.Errorf("an ambiguous short link = %d", w.Code)
	}
	if w := ask("/p/" + id); w.Code != http.StatusOK {
		t.Errorf("the whole id beside a twin = %d", w.Code)
	}
}
