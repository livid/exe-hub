package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/identity"
	"exehub/internal/store"
)

// TestWebReading: ?lang= when the request says, else the browser's first
// language; a Chinese reader reads Simplified, a Japanese one Japanese,
// every other English, a request that names no language every post as
// written.
func TestWebReading(t *testing.T) {
	for _, c := range []struct{ path, header, reader, target, q string }{
		{"/", "", "", "", ""},
		{"/", "*", "", "", ""},
		{"/", "en-US,en;q=0.9,zh-CN;q=0.8", "en", "en", ""},
		{"/", "zh-CN,zh;q=0.9,en;q=0.8", "zh-Hans", "zh-Hans", ""},
		{"/", "zh", "zh-Hans", "zh-Hans", ""},
		{"/", "zh-TW", "zh-Hant", "zh-Hans", ""},
		{"/", "ja,en;q=0.8", "ja", "ja", ""},
		{"/", "ja-JP", "ja", "ja", ""},
		{"/", "fr-FR,fr;q=0.9", "fr", "en", ""},
		{"/?lang=zh", "en-US", "zh-Hans", "zh-Hans", "?lang=zh"},
		{"/?lang=ZH-TW", "en-US", "zh-Hant", "zh-Hans", "?lang=zh-tw"},
		{"/?lang=en", "zh-CN", "en", "en", "?lang=en"},
		{"/?lang=ja", "zh-CN", "ja", "ja", "?lang=ja"},
		{"/?lang=orig", "zh-CN", "orig", "", "?lang=orig"},
		{"/?lang=klingon!", "zh-CN", "zh-Hans", "zh-Hans", ""}, // no language: the browser's
		{"/?lang=zxx", "en", "en", "en", ""},
	} {
		req := httptest.NewRequest("GET", "http://hub.example"+c.path, nil)
		req.Header.Set("Accept-Language", c.header)
		rd := webReadingOf(req)
		if rd.Reader != c.reader || rd.Target != c.target || rd.Q != c.q {
			t.Errorf("%s (%q) = %+v, want reader %q target %q q %q", c.path, c.header, rd, c.reader, c.target, c.q)
		}
	}
	zhTW := webReading{Reader: "zh-Hant", Target: "zh-Hans"}
	ja, fr := webReading{Reader: "ja", Target: "ja"}, webReading{Reader: "fr", Target: "en"}
	for _, c := range []struct {
		rd   webReading
		from string
		want bool
	}{
		{zhTW, "zh-Hant", false}, {zhTW, "zh-Hans", false}, {zhTW, "en", true}, {zhTW, "ja", true},
		{ja, "ja", false}, {ja, "en", true}, {ja, "zh-Hans", true},
		{fr, "fr", false}, {fr, "en", false}, {fr, "ja", true},
		{webReading{Reader: "orig"}, "en", false}, {webReading{}, "en", false},
	} {
		if got := c.rd.shows(c.from); got != c.want {
			t.Errorf("%+v shows %s = %v", c.rd, c.from, got)
		}
	}
}

// TestWebTranslations: a post in another language stands translated for
// the reader it was translated for, the post as written hidden under it
// and the control below in the reader's language; a post in the
// reader's own language, a reader who asks for the originals and a
// request that names no language get it as written, with its lang set;
// an explicit ?lang= rides the page's own links; the feed's newest-reply
// line reads in the reader's language too.
func TestWebTranslations(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	h := s.Handler()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	en := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "The **hub** speaks `two` languages"})
	zh := ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "今天天气很好"})
	reply := ingest(t, s, priv, pub, 3, "post.create", map[string]any{"text": "a reply in English", "reply_to": zh})
	for id, tag := range map[string]string{en: "en", zh: "zh-Hans", reply: "en"} {
		if err := s.St.SetLang(id, tag, "m", true); err != nil {
			t.Fatal(err)
		}
	}
	for _, tr := range [][3]string{
		{en, "zh-Hans", "**Hub** 现在说 `two` 种语言"},
		{zh, "en", "Lovely weather today"},
		{reply, "zh-Hans", "一条英文回复"},
	} {
		if err := s.St.SetTranslation(tr[0], tr[1], tr[2], "m", true); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://hub.example/", nil)
	req.Header.Set("Accept-Language", "zh-CN")
	h.ServeHTTP(w, req)
	if v := w.Header().Get("Vary"); v != "Accept-Language" {
		t.Errorf("Vary = %q", v)
	}
	body := w.Body.String()
	for _, want := range []string{
		// the translation stands, rendered like any text; the post as written is hidden under it
		`<div class="text" lang="zh-Hans"><strong>Hub</strong> 现在说 <code>two</code> 种语言</div><div class="text" lang="en" hidden>The <strong>hub</strong>`,
		`<div class="trl"><span>译自英语 · </span><a class="tro" role="button" href="/p/` + en + `?lang=orig" data-other="显示译文">显示原文</a></div>`,
		// the Chinese post is the reader's own: as written, no control
		`<div class="text" lang="zh-Hans">今天天气很好</div>`,
		// its newest reply, on the foot line, in Chinese
		`一条英文回复</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a Chinese reader's feed lacks %q", want)
		}
	}
	if strings.Count(body, `class="trl"`) != 1 {
		t.Errorf("%d controls on a Chinese reader's feed, want 1", strings.Count(body, `class="trl"`))
	}

	_, body = get(t, h, "/", "Accept-Language", "en-US,en;q=0.9")
	for _, want := range []string{
		`<div class="text" lang="en">Lovely weather today</div><div class="text" lang="zh-Hans" hidden>今天天气很好</div>`,
		`<span>Translated from Chinese · </span><a class="tro" role="button" href="/p/` + zh + `?lang=orig" data-other="Show Translation">Show Original</a>`,
		`<div class="text" lang="en">The <strong>hub</strong>`,
		`a reply in English</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("an English reader's feed lacks %q", want)
		}
	}

	// as written: no language named, ?lang=orig, and a thread from the control's link
	for _, path := range []string{"/", "/?lang=orig", "/p/" + en + "?lang=orig"} {
		hdr := []string{}
		if path != "/" {
			hdr = []string{"Accept-Language", "zh-CN"}
		}
		if _, body = get(t, h, path, hdr...); strings.Contains(body, `class="trl"`) || strings.Contains(body, `<div class="text" lang="en" hidden>`) || !strings.Contains(body, `<div class="text" lang="en">The <strong>hub</strong>`) {
			t.Errorf("%s shows a translation", path)
		}
	}

	// ?lang= from a browser of another language, carried by the page's links
	_, body = get(t, h, "/?lang=zh", "Accept-Language", "en-US")
	for _, want := range []string{
		`显示原文`, `href="/u/` + identity.Fingerprint(pub) + `?lang=zh"`, `href="/p/` + en + `?lang=zh"`,
		`href="/p/` + zh + `?lang=zh#` + reply + `"`, `<input type="hidden" name="lang" value="zh">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/?lang=zh lacks %q", want)
		}
	}
	if code, thread := get(t, h, "/p/"+zh+"?lang=zh", "Accept-Language", "en-US"); code != http.StatusOK ||
		!strings.Contains(thread, `href="/?lang=zh"`) || !strings.Contains(thread, "一条英文回复") || strings.Contains(thread, "Lovely weather") {
		t.Errorf("the thread under ?lang=zh: %d", code)
	}
}

// TestWebSearchTranslated: the search looks in the posts as written. A
// post found by words only its original has opens as written, the
// found words on yellow, the translation one press away; one whose
// translation holds the words too stands translated.
func TestWebSearchTranslated(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	h := s.Handler()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	a := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "the exe window manager"})
	b := ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "exe notes"})
	for id, zh := range map[string]string{a: "窗口管理器", b: "exe 笔记"} {
		s.St.SetLang(id, "en", "m", true)
		if err := s.St.SetTranslation(id, "zh-Hans", zh, "m", true); err != nil {
			t.Fatal(err)
		}
	}
	_, body := get(t, h, "/search?q=exe&lang=zh")
	for _, want := range []string{
		`<div class="text" lang="zh-Hans" hidden>窗口管理器</div><div class="text" lang="en">the <mark>exe</mark> window manager</div>`,
		`<span hidden>译自英语 · </span><a class="tro" role="button" href="/p/` + a + `?lang=zh" data-other="显示原文">显示译文</a>`,
		`<div class="text" lang="zh-Hans"><mark>exe</mark> 笔记</div><div class="text" lang="en" hidden><mark>exe</mark> notes</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the search page lacks %q", want)
		}
	}
}

// TestWebTrNote: the line names the language alone, and the script only
// to a Chinese reader, whom it tells Traditional from Simplified.
func TestWebTrNote(t *testing.T) {
	zh, en := webReading{Reader: "zh-Hans", Target: "zh-Hans", L: webLocales["zh"]}, webReading{Reader: "en", Target: "en", L: webLocales["en"]}
	ja := webReading{Reader: "ja", Target: "ja", L: webLocales["ja"]}
	for _, c := range []struct {
		rd         webReading
		from, want string
	}{
		{en, "zh-Hans", "Translated from Chinese"}, {en, "zh-Hant", "Translated from Chinese"}, {en, "sr-Latn", "Translated from Serbian"},
		{zh, "en", "译自英语"}, {zh, "zh-Hant", "译自繁体中文"}, {zh, "sr-Latn", "译自塞尔维亚语"}, {zh, "ja", "译自日语"},
		{ja, "zh-Hans", "中国語から翻訳"}, {ja, "zh-Hant", "中国語から翻訳"}, {ja, "sr-Latn", "セルビア語から翻訳"},
	} {
		tr := c.rd.tr(store.Translation{Text: "x", From: c.from}, nil)
		if tr.Note != c.want {
			t.Errorf("%s for %s = %q, want %q", c.from, c.rd.Reader, tr.Note, c.want)
		}
		if want := map[string][2]string{"en": {"Show Original", "Show Translation"}, "zh": {"显示原文", "显示译文"}, "ja": {"原文を表示", "翻訳を表示"}}[c.rd.L.Code]; tr.Show != want[0] || tr.Back != want[1] {
			t.Errorf("%s control = %q/%q", c.rd.L.Code, tr.Show, tr.Back)
		}
	}
}

// TestTranslationsPage: /v1/translations is a hub-signed page of the
// translations this hub made, under a prefix of its own, behind the
// replication switch and a nonce like /v1/replicate.
func TestTranslationsPage(t *testing.T) {
	no := false
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	var err error
	if s.Hub, err = identity.Load(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	id := ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "hello there, everyone"})
	s.St.SetLang(id, "en", "m", true)
	s.St.SetTranslation(id, "zh-Hans", "大家好。", "glm", true)

	code, body := get(t, h, "/v1/translations?nonce=00112233aabbccdd")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, body)
	}
	var out struct {
		Payload json.RawMessage `json:"payload"`
		Sig     []byte          `json:"sig"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	key, _ := base64.StdEncoding.DecodeString(s.Hub.PubKey())
	if !ed25519.Verify(key, append([]byte(envelope.TranslationsPrefix), out.Payload...), out.Sig) {
		t.Fatal("the page does not verify under the translations prefix")
	}
	if ed25519.Verify(key, append([]byte(envelope.ReplicatePrefix), out.Payload...), out.Sig) {
		t.Fatal("the page verifies as a /v1/replicate page too")
	}
	var pl TranslationsPayload
	if err := json.Unmarshal(out.Payload, &pl); err != nil || pl.Hub != s.Hub.ID || pl.Nonce != "00112233aabbccdd" ||
		len(pl.Translations) != 1 || pl.Translations[0].Text != "大家好。" || pl.Translations[0].Post != id || pl.Next == 0 {
		t.Fatalf("payload = %+v, %v", pl, err)
	}
	if code, _ := get(t, h, "/v1/translations?nonce=xyz"); code != http.StatusBadRequest {
		t.Errorf("a bad nonce = %d", code)
	}
	if code, _ := get(t, h, "/v1/translations?nonce=00112233aabbccdd&after=soon"); code != http.StatusBadRequest {
		t.Errorf("a bad cursor = %d", code)
	}
	s.Cfg.Set(&config.Config{Gate: config.Gate{Mode: "open"}, AllowReplication: &no})
	if code, _ := get(t, h, "/v1/translations?nonce=00112233aabbccdd"); code != http.StatusForbidden {
		t.Errorf("with replication off = %d", code)
	}
}
