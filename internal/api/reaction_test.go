package api

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exehub/internal/config"
	"exehub/internal/emoji"
	"exehub/internal/envelope"
	"exehub/internal/identity"
)

// TestWebReactions (PLAN.md, Reactions): the row under a post — a button
// per emoji with who gave it, in the order given — on every page that
// shows the post; Add Reaction only where the wallet can sign; the who
// endpoint; the JSON post's reactions; and every emoji named in every
// language the pages speak.
func TestWebReactions(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	mePub, mePriv, _ := ed25519.GenerateKey(nil)
	bobPub, bobPriv, _ := ed25519.GenerateKey(nil)
	me, bob := identity.Fingerprint(mePub), identity.Fingerprint(bobPub)
	ingest(t, s, bobPriv, bobPub, 1, "profile.set", map[string]any{"name": "Bob"})
	p := ingest(t, s, mePriv, mePub, 1, "post.create", map[string]any{"text": "exe is out"})
	bare := ingest(t, s, mePriv, mePub, 2, "post.create", map[string]any{"text": "nothing on this one"})
	ingest(t, s, bobPriv, bobPub, 2, "post.react", map[string]any{"post": p, "emoji": "🎉", "on": true})
	ingest(t, s, mePriv, mePub, 3, "post.react", map[string]any{"post": p, "emoji": "🎉", "on": true})
	ingest(t, s, mePriv, mePub, 4, "post.react", map[string]any{"post": p, "emoji": "❤️", "on": true})
	h := s.Handler()

	row := `<div class="rxs"><button type="button" class="btn rx" data-e="🎉" data-name="Party Popper" data-ids="` + bob + ` ` + me + `" aria-label="Party Popper, 2" title="2 people. Right-click to see who."><span class="em">🎉</span><span class="n">2</span></button>` +
		`<button type="button" class="btn rx" data-e="❤️" data-name="Red Heart" data-ids="` + me + `" aria-label="Red Heart, 1" title="1 person. Right-click to see who."><span class="em">❤️</span><span class="n">1</span></button></div>`
	for _, path := range []string{"/", "/p/" + p, "/u/" + me, "/search?q=exe"} {
		_, page := get(t, h, path)
		if !strings.Contains(page, row) {
			t.Errorf("%s: no row of reactions, or not as given", path)
		}
		if !strings.Contains(page, `window.hubMine = id =>`) || !strings.Contains(page, `"rx":[["👍","Thumbs Up"],["❤️","Red Heart"]`) {
			t.Errorf("%s: no rule for the reader's own, or no emoji table for the script", path)
		}
	}
	_, home := get(t, h, "/")
	if strings.Count(home, `class="rxs"`) != 1 {
		t.Error("home: a row of reactions on a post that has none")
	}
	// Add Reaction beside Bookmark, at its left: it floats after it
	if strings.Count(home, `class="btn rx-add"`) != 2 || !strings.Contains(home, `</svg></button><button type="button" class="btn rx-add" aria-label="Add Reaction"`) {
		t.Error("home: Add Reaction is not beside Bookmark on each post")
	}
	if !strings.Contains(home, `window.hubReact = async (id, emoji, on, flip) =>`) || !strings.Contains(home, `hubMine(st.v.profile);`) {
		t.Error("home: the compose script cannot sign a reaction, or does not press the reader's own")
	}
	if _, prof := get(t, h, "/u/"+me); strings.Contains(prof, `class="btn rx-add"`) || strings.Contains(prof, `window.hubReact =`) {
		t.Error("profile: a page with no Post window offers to react")
	}
	if _, zh := get(t, h, "/?lang=zh"); !strings.Contains(zh, `aria-label="拉炮彩带，2"`) || !strings.Contains(zh, `aria-label="添加回应"`) {
		t.Error("home in Chinese: the reaction or Add Reaction is not in the page's language")
	}

	// the JSON post carries who gave each, earliest first
	var one struct {
		Post struct {
			Reactions []struct {
				Emoji string
				By    []string
			}
		}
	}
	_, body := get(t, h, "/v1/post/"+p)
	if err := json.Unmarshal([]byte(body), &one); err != nil || len(one.Post.Reactions) != 2 ||
		one.Post.Reactions[0].Emoji != "🎉" || strings.Join(one.Post.Reactions[0].By, " ") != bob+" "+me || one.Post.Reactions[1].Emoji != "❤️" {
		t.Errorf("post JSON: %s", body)
	}
	if _, body := get(t, h, "/v1/post/"+bare); strings.Contains(body, `"reactions"`) {
		t.Errorf("a post with none carries reactions: %s", body)
	}

	// who reacted, with names
	var who struct {
		Post      string
		Reactions []struct {
			Emoji  string
			People []struct {
				ID, Name string
				TS       int64
			}
		}
	}
	code, body := get(t, h, "/v1/post/"+p+"/reactions")
	if err := json.Unmarshal([]byte(body), &who); code != http.StatusOK || err != nil || who.Post != p || len(who.Reactions) != 2 ||
		len(who.Reactions[0].People) != 2 || who.Reactions[0].People[0].Name != "Bob" || who.Reactions[0].People[0].TS != 1756500000002 ||
		who.Reactions[0].People[1].ID != me || who.Reactions[0].People[1].Name != "" {
		t.Errorf("who: %d %s", code, body)
	}
	if code, body := get(t, h, "/v1/post/"+bare+"/reactions"); code != http.StatusOK || !strings.Contains(body, `"reactions":[]`) {
		t.Errorf("who on a post with none: %d %s", code, body)
	}
	if code, _ := get(t, h, "/v1/post/"+strings.Repeat("0", 64)+"/reactions"); code != http.StatusNotFound {
		t.Errorf("who on no post: %d", code)
	}

	// the picker's own 24 are emoji a hub takes, each named in every
	// language the pages speak
	for _, e := range rxQuick {
		if !emoji.Is(e) {
			t.Errorf("%s is no emoji a hub takes", e)
		}
		for code, l := range webLocales {
			if l.rxName(e) == e {
				t.Errorf("%s: no name for %s", code, e)
			}
		}
	}

	// one beyond the 24: its row named by Unicode's name for it
	ingest(t, s, bobPriv, bobPub, 3, "post.react", map[string]any{"post": bare, "emoji": "🦄", "on": true})
	if _, home := get(t, h, "/"); !strings.Contains(home, `data-e="🦄" data-name="Unicorn" data-ids="`+bob+`" aria-label="Unicorn, 1"`) {
		t.Error("home: a reaction beyond the picker's own is not named")
	}
	if _, ja := get(t, h, "/?lang=ja"); !strings.Contains(ja, `data-name="ユニコーンの顔"`) || !strings.Contains(ja, `"rxnames":["/v1/emoji/ja?v=`) || !strings.Contains(ja, `","/v1/emoji/en?v=`) {
		t.Error("home in Japanese: no Japanese name, or the search's tables are not offered")
	}
}

// TestEmojiNames: every emoji's name in one language, kept for good under
// its version, gzipped for a browser that takes it, checked again under
// any other address.
func TestEmojiNames(t *testing.T) {
	h := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}}).Handler()
	raw, ver := emoji.Names("zh")
	req := httptest.NewRequest("GET", "http://hub.example/v1/emoji/zh?v="+ver, nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") || w.Body.Len() > len(raw)/3 {
		t.Fatalf("zh: %d %v, %d bytes of %d", w.Code, w.Header(), w.Body.Len(), len(raw))
	}
	z, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(z); !bytes.Equal(got, raw) {
		t.Error("zh: the gzipped table is not the table")
	}
	code, body := get(t, h, "/v1/emoji/en")
	if code != 200 || !strings.HasPrefix(body, `[["😀","Grinning Face"`) {
		t.Errorf("en: %d %.60s", code, body)
	}
	req = httptest.NewRequest("GET", "http://hub.example/v1/emoji/en", nil)
	_, enVer := emoji.Names("en")
	req.Header.Set("If-None-Match", `"`+enVer+`"`)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotModified || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("en, asked again without its version: %d %v", w.Code, w.Header())
	}
	if code, _ := get(t, h, "/v1/emoji/fr"); code != http.StatusNotFound {
		t.Errorf("fr: %d", code)
	}
}

// TestReactPolicy: a reaction is gated as a bookmark is — a key the token
// gate turns away is turned away here too — and has no cooldown.
func TestReactPolicy(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	op := func(seq int64) *envelope.Envelope {
		raw, _ := json.Marshal(map[string]any{"type": "post.react", "author": base64.StdEncoding.EncodeToString(pub),
			"seq": seq, "ts": 1, "body": map[string]any{"post": strings.Repeat("a", 64), "emoji": "👀", "on": true}})
		e, err := envelope.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	open := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	post(t, open, priv, pub, 1) // a post a moment ago: the cooldown runs
	if err := open.policy(op(3)); err != nil {
		t.Errorf("an open hub refuses a reaction: %v", err)
	}
	gated := testServer(t, &config.Config{Gate: config.Gate{Mode: "token", Token: config.TokenGate{
		Mints:  []config.MintReq{{Mint: "9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump", MinAmount: "1", MinRaw: 1}},
		RPCURL: "http://127.0.0.1:1", RPCUnavailable: "deny", Recheck: "10m"}}})
	if err := gated.policy(op(1)); err == nil {
		t.Error("a token gate that turns the key away lets its reaction in")
	}
}
