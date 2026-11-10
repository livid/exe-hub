package api

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"exehub/internal/config"
	"exehub/internal/identity"
)

// TestWebPictures: /pictures is every picture a post carries as its own,
// the newest post first, each post's pictures together, a month a block
// with its name in the page's language; a drawing's tile is marked; a
// file that is not a picture is not on the wall; a profile's wall holds
// that profile's pictures alone; the Navigation window carries the row.
func TestWebPictures(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}, Stats: &config.Stats{Location: time.UTC}})
	const cat, pad, doc, dog = "bafybeicat", "bafybeipad", "bafybeidoc", "bafybeidog"
	for cid, mime := range map[string]string{cat: "image/jpeg", pad: "image/png", doc: "text/plain", dog: "image/png"} {
		if err := s.St.AddPin(cid, 1234, mime, false); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	one := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "a cat and a drawing", "embeds": []map[string]any{
		{"cid": cat, "mime": "image/jpeg", "filename": "cat.jpg", "alt": "a cat"},
		{"cid": pad, "mime": "image/png", "filename": "drawing.png", "width": 256, "height": 128},
	}})
	ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "a file", "embeds": []map[string]any{
		{"cid": doc, "mime": "text/plain", "filename": "notes.txt"}}})
	pub2, priv2, _ := ed25519.GenerateKey(nil)
	two := ingest(t, s, priv2, pub2, 3, "post.create", map[string]any{"text": "a dog", "embeds": []map[string]any{
		{"cid": dog, "mime": "image/png", "filename": "dog.png", "width": 640, "height": 480, "alt": "a dog"}}})
	h := s.Handler()

	code, body := get(t, h, "/pictures")
	if code != http.StatusOK {
		t.Fatalf("/pictures answered %d", code)
	}
	author, author2 := identity.Fingerprint(pub), identity.Fingerprint(pub2)
	for _, want := range []string{
		"<title>Pictures · hub.example</title>",
		`<link rel="canonical" href="http://hub.example/pictures">`,
		`<span class="title">Pictures</span>`,
		`<nav class="places" aria-label="Navigation" data-page="pictures" data-for="" data-q="">`,
		`<a class="place" data-to="pictures" href="/pictures">`,
		`<div class="wallcap">August 2025</div>`, // the posts' own time (ingest's seq is their clock)
		`<a class="tile" href="/p/` + two + `" title="` + author2 + ` · 2025-08-29"><img src="/v1/embed/` + dog + `" alt="a dog" loading="lazy"></a>`,
		`<a class="tile" href="/p/` + one + `" title="` + author + ` · 2025-08-29"><img src="/v1/embed/` + cat + `" alt="a cat" loading="lazy"></a>` +
			`<a class="tile" href="/p/` + one + `" title="` + author + ` · 2025-08-29"><img src="/v1/embed/` + pad + `" alt="" loading="lazy" class="draw"></a>`,
		`<div class="statusbar"><span>3 pictures</span></div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/pictures lacks %q", want)
		}
	}
	if strings.Contains(body, doc) {
		t.Error("a file that is not a picture is on the wall")
	}
	if i, j := strings.Index(body, dog), strings.Index(body, cat); i > j {
		t.Error("the newer post's picture does not come first")
	}
	if strings.Contains(body, "pictures?before=") {
		t.Error("three pictures have an Older button")
	}

	for _, c := range []struct{ q, title, month, status string }{
		{"?lang=zh", "图片 · hub.example", "2025 年 8 月", "3 张图片"},
		{"?lang=ja", "画像 · hub.example", "2025年8月", "3 件の画像"},
	} {
		_, body := get(t, h, "/pictures"+c.q)
		for _, want := range []string{"<title>" + c.title + "</title>", `<div class="wallcap">` + c.month + "</div>",
			"<span>" + c.status + "</span>", `href="/pictures` + c.q + `"`, `href="/p/` + two + c.q + `"`} {
			if !strings.Contains(body, want) {
				t.Errorf("/pictures%s lacks %q", c.q, want)
			}
		}
	}

	// one profile's wall
	code, body = get(t, h, "/u/"+author2+"/pictures")
	if code != http.StatusOK {
		t.Fatalf("/u/{id}/pictures answered %d", code)
	}
	for _, want := range []string{
		"<title>Pictures by " + author2 + " · hub.example</title>",
		`<span class="title"><a href="/">hub.example</a></span>`, // the profile's own window
		`<h1>` + author2 + `</h1>`,
		`<div class="tabbar ptabs" role="tablist"><a class="tab" href="/u/` + author2 + `">Posts</a><a class="tab active" href="/u/` + author2 + `/pictures" aria-current="page">Pictures</a></div>`,
		`<img src="/v1/embed/` + dog + `"`,
		`<div class="statusbar"><span>1 picture</span></div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/u/{id}/pictures lacks %q", want)
		}
	}
	if strings.Contains(body, cat) {
		t.Error("another profile's picture is on this profile's wall")
	}
	// the profile page has the tabs too, Posts on, and the zh query rides on both
	if _, body := get(t, h, "/u/"+author2+"?lang=zh"); !strings.Contains(body,
		`<div class="tabbar ptabs" role="tablist"><a class="tab active" href="/u/`+author2+`?lang=zh" aria-current="page">帖子</a><a class="tab" href="/u/`+author2+`/pictures?lang=zh">图片</a></div>`) {
		t.Error("the profile page has no Posts | Pictures tabs")
	}
	if code, body := get(t, h, "/u/0123456789abcdef/pictures"); code != http.StatusOK || !strings.Contains(body, `<div class="wallnone">No pictures yet.</div>`) {
		t.Errorf("an id that never posted answered %d without the empty wall", code)
	}
	if code, _ := get(t, h, "/u/nobody/pictures"); code != http.StatusNotFound {
		t.Errorf("a wall for what is not an id answered %d", code)
	}
	if code, _ := get(t, h, "/pictures?before=nosuchpost"); code != http.StatusNotFound {
		t.Errorf("an unknown cursor answered %d", code)
	}
	if _, home := get(t, h, "/?lang=zh"); !strings.Contains(home, `<a class="place" data-to="pictures" href="/pictures?lang=zh">`) {
		t.Error("the home page's Navigation window has no Pictures row")
	}
}

// TestWebPicturesPaged: a page ends at a post's last picture, never in
// the middle of a post, and the Older button carries the last post shown
// as the cursor of the next page.
func TestWebPicturesPaged(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}, Stats: &config.Stats{Location: time.UTC}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	var ids []string
	for i := 0; i < wallPage/2+2; i++ { // each post two pictures: the page would end mid-post
		a, b := fmt.Sprintf("bafybeiaaa%c%c", 'a'+i/26, 'a'+i%26), fmt.Sprintf("bafybeibbb%c%c", 'a'+i/26, 'a'+i%26)
		for _, cid := range []string{a, b} {
			if err := s.St.AddPin(cid, 1234, "image/png", false); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, ingest(t, s, priv, pub, int64(i+1), "post.create", map[string]any{"text": fmt.Sprint("post ", i), "embeds": []map[string]any{
			{"cid": a, "mime": "image/png", "filename": "a.png"}, {"cid": b, "mime": "image/png", "filename": "b.png"}}}))
	}
	h := s.Handler()
	_, body := get(t, h, "/pictures")
	n := strings.Count(body, `<a class="tile"`)
	if n != wallPage {
		t.Errorf("the first page holds %d tiles, want %d", n, wallPage)
	}
	// the oldest post on the page is the cursor; its two pictures are both here
	last := ids[len(ids)-wallPage/2]
	if strings.Count(body, `href="/p/`+last+`"`) != 2 {
		t.Errorf("the cursor post's pictures are not both on the page")
	}
	if !strings.Contains(body, `href="/pictures?before=`+last+`"><span>Older</span>`) {
		t.Error("no Older button to the page after")
	}
	_, body = get(t, h, "/pictures?before="+last)
	if n := strings.Count(body, `<a class="tile"`); n != 4 {
		t.Errorf("the second page holds %d tiles, want 4", n)
	}
	if strings.Contains(body, "pictures?before=") || !strings.Contains(body, `<span>`+fmt.Sprint(wallPage+4)+` pictures</span>`) {
		t.Error("the last page has an Older button, or the count is not the whole wall's")
	}
	if strings.Contains(body, `rel="canonical"`) {
		t.Error("a paged wall claims to be canonical")
	}
}
