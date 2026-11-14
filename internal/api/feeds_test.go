package api

import (
	"crypto/ed25519"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exehub/internal/config"
	"exehub/internal/identity"
)

// TestWebFeeds: a profile's three feeds carry its roots alone, newest
// first, each the post as the pages draw it with absolute addresses and
// its pictures under it; the profile page names the three in its head;
// an id that is not one, or has nothing, is a 404. A profile that
// replies far more than it posts (Codex's case, 2026-11-14: one root,
// then 60 replies) still gives the root, with or without ?replies=1.
func TestWebFeeds(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	const cid = "bafybeieeqqg3m4keu6lt3bwmo6s2gofxyrrn3sbyayw63kvxzrq4p5rdsy"
	if err := s.St.AddPin(cid, 1234, "image/jpeg", false); err != nil {
		t.Fatal(err)
	}
	apub, apriv, _ := ed25519.GenerateKey(nil)
	bpub, bpriv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, apriv, apub, 1, "profile.set", map[string]any{"name": "Ann", "bio": "Notes from Ann"})
	first := ingest(t, s, apriv, apub, 2, "post.create", map[string]any{"text": "# The first post\n\nwith **bold** words and a [link](https://example.com/)"})
	second := ingest(t, s, apriv, apub, 3, "post.create", map[string]any{"text": "look",
		"embeds": []map[string]any{{"cid": cid, "mime": "image/jpeg", "filename": "cat.jpg", "alt": "a cat"}}})
	ingest(t, s, bpriv, bpub, 1, "post.create", map[string]any{"text": "an answer", "reply_to": first})
	var seq int64 = 4
	for i := 0; i < 60; i++ { // Ann replies far more than she posts
		ingest(t, s, apriv, apub, seq, "post.create", map[string]any{"text": "my own reply", "reply_to": first})
		seq++
	}
	id := identity.Fingerprint(apub)
	h := s.Handler()
	img := `https://hub.example/v1/embed/` + cid

	// Atom
	code, body, ct := getFeed(t, h, "/u/"+id+"/feed.atom")
	if code != 200 || !strings.HasPrefix(ct, "application/atom+xml") {
		t.Fatalf("atom: %d %s", code, ct)
	}
	var atom struct {
		Title    string `xml:"title"`
		Subtitle string `xml:"subtitle"`
		ID       string `xml:"id"`
		Icon     string `xml:"icon"`
		Links    []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
		} `xml:"link"`
		Entries []struct {
			ID      string `xml:"id"`
			Title   string `xml:"title"`
			Content string `xml:"content"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal([]byte(body), &atom); err != nil {
		t.Fatalf("atom does not parse: %v\n%s", err, body)
	}
	if atom.Title != "Ann" || atom.Subtitle != "Notes from Ann" || atom.ID != "https://hub.example/u/"+id || atom.Icon != "https://hub.example/v1/identicon/"+id+".svg" {
		t.Errorf("atom head: %+v", atom)
	}
	if len(atom.Links) != 2 || atom.Links[1].Rel != "self" || atom.Links[1].Href != "https://hub.example/u/"+id+"/feed.atom" {
		t.Errorf("atom links: %+v", atom.Links)
	}
	if len(atom.Entries) != 2 {
		t.Fatalf("atom entries: %d, want the two roots\n%s", len(atom.Entries), body)
	}
	if e := atom.Entries[0]; e.ID != "https://hub.example/p/"+second || e.Title != "look" || !strings.Contains(e.Content, `<p><a href="`+img+`"><img src="`+img+`" alt="a cat"></a></p>`) {
		t.Errorf("newest entry: %+v", e)
	}
	if e := atom.Entries[1]; e.ID != "https://hub.example/p/"+first || e.Title != "The first post" || !strings.Contains(e.Content, "bold") || !strings.Contains(e.Content, `href="https://example.com/"`) {
		t.Errorf("older entry: %+v", e)
	}
	if strings.Contains(body, "an answer") || strings.Contains(body, "my own reply") {
		t.Error("a reply is in the feed")
	}

	// RSS
	code, body, ct = getFeed(t, h, "/u/"+id+"/feed.rss")
	if code != 200 || !strings.HasPrefix(ct, "application/rss+xml") {
		t.Fatalf("rss: %d %s", code, ct)
	}
	var rss struct {
		Channel struct {
			Title       string `xml:"title"`
			Description string `xml:"description"`
			Items       []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				GUID        string `xml:"guid"`
				PubDate     string `xml:"pubDate"`
				Description string `xml:"description"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal([]byte(body), &rss); err != nil {
		t.Fatalf("rss does not parse: %v\n%s", err, body)
	}
	c := rss.Channel
	// (<link> and <atom:link> share a local name, so the channel's link is read as text)
	if c.Title != "Ann" || !strings.Contains(body, "<link>https://hub.example/u/"+id+"</link>") || c.Description != "Notes from Ann" || len(c.Items) != 2 {
		t.Fatalf("rss channel: %+v", c)
	}
	if it := c.Items[0]; it.Title != "look" || it.Link != "https://hub.example/p/"+second || it.GUID != it.Link || !strings.Contains(it.Description, `<img src="`+img+`"`) || !strings.HasSuffix(it.PubDate, " +0000") {
		t.Errorf("rss item: %+v", it)
	}

	// JSON Feed
	code, body, ct = getFeed(t, h, "/u/"+id+"/feed.json")
	if code != 200 || !strings.HasPrefix(ct, "application/feed+json") {
		t.Fatalf("json: %d %s", code, ct)
	}
	var jf struct {
		Version string `json:"version"`
		Title   string `json:"title"`
		FeedURL string `json:"feed_url"`
		Authors []struct {
			Name string `json:"name"`
		} `json:"authors"`
		Items []struct {
			ID, URL, Title, Image string
			ContentHTML           string `json:"content_html"`
			DatePublished         string `json:"date_published"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &jf); err != nil {
		t.Fatalf("json feed does not parse: %v\n%s", err, body)
	}
	if jf.Version != "https://jsonfeed.org/version/1.1" || jf.Title != "Ann" || jf.FeedURL != "https://hub.example/u/"+id+"/feed.json" || len(jf.Authors) != 1 || jf.Authors[0].Name != "Ann" || len(jf.Items) != 2 {
		t.Fatalf("json feed head: %s", body)
	}
	if it := jf.Items[0]; it.ID != "https://hub.example/p/"+second || it.Image != img || it.Title != "look" || !strings.HasPrefix(it.ContentHTML, `<div>look</div><p><a href="`+img+`">`) || !strings.HasPrefix(it.DatePublished, "2025-08-29T") {
		t.Errorf("json item: %+v", it)
	}
	if !strings.Contains(body, `"content_html":"<div>look</div><p>`) { // the tags as they are, not \u003c
		t.Errorf("json escapes the HTML: %s", body)
	}

	// ?replies=1 changes nothing: roots only, whatever is asked
	if code, body, _ := getFeed(t, h, "/u/"+id+"/feed.json?replies=1"); code != 200 || strings.Contains(body, "my own reply") {
		t.Errorf("replies=1: %d, replies in = %v", code, strings.Contains(body, "my own reply"))
	}
	// ?lang=zh with no translation stored: as written, 200
	if code, body, _ := getFeed(t, h, "/u/"+id+"/feed.atom?lang=zh"); code != 200 || !strings.Contains(body, "The first post") {
		t.Errorf("lang=zh: %d", code)
	}

	// the profile page names the three
	_, page := get(t, h, "/u/"+id)
	for _, want := range []string{
		`<link rel="alternate" type="application/rss+xml" title="RSS" href="http://hub.example/u/` + id + `/feed.rss">`,
		`<link rel="alternate" type="application/atom+xml" title="Atom" href="http://hub.example/u/` + id + `/feed.atom">`,
		`<link rel="alternate" type="application/feed+json" title="JSON Feed" href="http://hub.example/u/` + id + `/feed.json">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("profile head lacks %s", want)
		}
	}
	if _, home := get(t, h, "/"); strings.Contains(home, `rel="alternate"`) {
		t.Error("the home page names a feed")
	}

	// nobody, and an id that is not one
	if code, _, _ := getFeed(t, h, "/u/0123456789abcdef/feed.atom"); code != 404 {
		t.Errorf("an empty id's feed = %d", code)
	}
	if code, _, _ := getFeed(t, h, "/u/zzz/feed.rss"); code != 404 {
		t.Errorf("a bad id's feed = %d", code)
	}
	// a profile with a name and no post has a feed with no entry
	cpub, cpriv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, cpriv, cpub, 1, "profile.set", map[string]any{"name": "Cid"})
	if code, body, _ := getFeed(t, h, "/u/"+identity.Fingerprint(cpub)+"/feed.json"); code != 200 || !strings.Contains(body, `"items":[]`) {
		t.Errorf("a quiet profile's feed: %d %s", code, body)
	}
}

func getFeed(t *testing.T, h http.Handler, path string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "http://hub.example"+path, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String(), w.Header().Get("Content-Type")
}

// TestFeedTitle: the first line, its Markdown marks off, cut at 100.
func TestFeedTitle(t *testing.T) {
	for in, want := range map[string]string{
		"# The first post\n\nmore":              "The first post",
		"\n\n- [ ] buy milk\n- [x] done":        "buy milk",
		"**Bold** start and `code`":             "Bold start and code",
		"> quoted words":                        "quoted words",
		"":                                      "",
		"   \n  ":                               "",
		strings.Repeat("word ", 30):             excerpt(strings.TrimSpace(strings.Repeat("word ", 30)), 100),
		"https://example.com/ a link first":     "https://example.com/ a link first",
		"@abcdef0123456789 named by the caller": "@abcdef0123456789 named by the caller",
	} {
		if got := feedTitle(in); got != want {
			t.Errorf("feedTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if s := feedAbsolute(`<a href="/p/x">p</a> <img src="/v1/embed/c"> <a href="//cdn.example/x">cdn</a> <a href="https://e.com/">e</a>`, "https://hub.example"); s != `<a href="https://hub.example/p/x">p</a> <img src="https://hub.example/v1/embed/c"> <a href="//cdn.example/x">cdn</a> <a href="https://e.com/">e</a>` {
		t.Errorf("feedAbsolute: %s", s)
	}
}
