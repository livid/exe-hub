package replicate

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"exehub/internal/api"
	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/events"
	"exehub/internal/gate"
	"exehub/internal/identity"
	"exehub/internal/store"
)

// servingHub is a real hub, its own store behind its own signing
// /v1/translations, for a puller to take from.
type servingHub struct {
	st   *store.Store
	id   *identity.Identity
	srv  *httptest.Server
	gone bool // answer 404, as a hub from before /v1/translations does
}

func newServingHub(t *testing.T) *servingHub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "peer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := config.NewHolder(&config.Config{Gate: config.Gate{Mode: "open"}})
	sh := &servingHub{st: st, id: id}
	handler := (&api.Server{Cfg: h, St: st, Gate: gate.New(h), Hub: id}).Handler()
	sh.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sh.gone && r.URL.Path == "/v1/translations" {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(sh.srv.Close)
	return sh
}

func (sh *servingHub) peer(trCursor int64) store.Peer {
	u, _ := url.Parse(sh.srv.URL)
	return store.Peer{Hub: sh.id.ID, Addr: "/ip4/" + u.Hostname() + "/tcp/" + u.Port() + "/http",
		PubKey: sh.id.PubKey(), TrCursor: trCursor}
}

// post puts the same signed post into every store given, and names it.
func (r *rig) post(text, tag string, into ...*store.Store) string {
	r.t.Helper()
	r.seq++
	raw, _ := json.Marshal(map[string]any{
		"type": "post.create", "author": base64.StdEncoding.EncodeToString(r.pub), "seq": r.seq, "ts": 1756500000000,
		"body": map[string]any{"text": text},
	})
	e, err := envelope.Parse(raw)
	if err != nil {
		r.t.Fatal(err)
	}
	op, _ := e.Op()
	sig := ed25519.Sign(r.priv, append([]byte(envelope.Prefix), raw...))
	id := ""
	for _, st := range into {
		if id, _, err = st.IngestReplicated(raw, sig, e, op, "elsewhere"); err != nil {
			r.t.Fatal(err)
		}
		st.SetLang(id, tag, "m", true)
	}
	return id
}

// TestPullTranslations: a hub takes the translations its peer made, off
// the peer's signed pages: one that passes this hub's own check of its
// own copy of the post is kept as the peer's, its punctuation set by
// this hub's rule, and announced; one out of shape, one of a post this
// hub does not hold and one into a language it does not keep are passed
// over; the cursor moves on, and a redo on the peer comes through and
// wins for being newer, over this hub's own as well.
func TestPullTranslations(t *testing.T) {
	r := newRig(t)
	peer := newServingHub(t)
	bus := events.New()
	r.p.Bus = bus
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	good := r.post("Try `?lang=zh` on https://hub.example/ now.", "en", r.st, peer.st)
	bent := r.post("See https://hub.example/docs for the rest.", "en", r.st, peer.st)
	theirs := r.post("a post only the peer holds", "en", peer.st)
	mine := r.post("both hubs translated this one", "en", r.st, peer.st)

	peer.st.SetTranslation(good, "zh-Hans", "现在在 https://hub.example/ 上试试 `?lang=zh`,就这样。", "glm", true)
	peer.st.SetTranslation(bent, "zh-Hans", "其余的见文档。", "glm", true) // the link is gone: no translation to keep
	peer.st.SetTranslation(theirs, "zh-Hans", "只有对方持有的帖子", "glm", true)
	r.st.SetTranslation(mine, "zh-Hans", "两个 hub 都翻译了这一条（本 hub 的，较早）", "mine", true)
	time.Sleep(5 * time.Millisecond) // the peer's is made later
	peer.st.SetTranslation(mine, "zh-Hans", "两个 hub 都翻译了这一条", "glm", true)

	if err := r.p.pullTranslations(peer.peer(0)); err != nil {
		t.Fatal(err)
	}
	trs, _ := r.st.Translations([]string{good, bent, theirs, mine}, "zh-Hans")
	if len(trs) != 2 {
		t.Fatalf("kept %+v, want the good one and the newer one only", trs)
	}
	if got := trs[good].Text; got != "现在在 https://hub.example/ 上试试 `?lang=zh`，就这样。" {
		t.Errorf("the taken translation = %q, want this hub's own punctuation rule run over it", got)
	}
	if trs[mine].Text != "两个 hub 都翻译了这一条" {
		t.Errorf("the newer one did not win: %q", trs[mine].Text)
	}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-sub:
			if ev.Type != "post.translation" || (ev.ID != good && ev.ID != mine) {
				t.Errorf("event %+v", ev)
			}
		case <-time.After(time.Second):
			t.Fatal("a taken translation was not announced")
		}
	}
	// what was taken is the peer's, and not served on
	if served, _, _ := r.st.TranslationsPage(0, 10); len(served) != 0 {
		t.Errorf("serving on what was taken: %+v", served)
	}
	// the cursor is kept, at the peer's last rev: nothing is read twice
	_, cur, _ := peer.st.TranslationsPage(0, 10)
	r.st.Ingest(peerAdd(t, r, peer)) // the peers table is where Peers() reads the cursor back from
	if ps, _ := r.st.Peers(); len(ps) != 1 || ps[0].TrCursor != cur || cur == 0 {
		t.Fatalf("peers = %+v, want the translations cursor at %d", ps, cur)
	}

	// a redo on the peer comes through, newer, and replaces what was taken before
	if err := r.p.pullTranslations(peer.peer(cur)); err != nil {
		t.Fatal(err)
	}
	peer.st.DropTranslations(good, "")
	time.Sleep(5 * time.Millisecond)
	peer.st.SetTranslation(good, "zh-Hans", "现在就在 https://hub.example/ 上试试 `?lang=zh`。", "glm", true)
	if err := r.p.pullTranslations(peer.peer(cur)); err != nil {
		t.Fatal(err)
	}
	if trs, _ = r.st.Translations([]string{good}, "zh-Hans"); trs[good].Text != "现在就在 https://hub.example/ 上试试 `?lang=zh`。" {
		t.Errorf("the peer's redo did not come through: %q", trs[good].Text)
	}
}

// TestPullTranslationsOldPeerAndBadPage: a peer from before
// /v1/translations answers 404 and is left alone, no error; a page
// signed by another key, or for another purpose, is refused whole.
func TestPullTranslationsOldPeerAndBadPage(t *testing.T) {
	r := newRig(t)
	peer := newServingHub(t)
	id := r.post("hello there, everyone", "en", r.st, peer.st)
	peer.st.SetTranslation(id, "zh-Hans", "大家好。", "glm", true)

	peer.gone = true
	if err := r.p.pullTranslations(peer.peer(0)); err != nil {
		t.Fatalf("an old peer: %v, want it left alone", err)
	}
	peer.gone = false

	other := newServingHub(t) // another hub's key in the admin's record: the page's signature is not its
	wrong := peer.peer(0)
	wrong.PubKey = other.id.PubKey()
	if err := r.p.pullTranslations(wrong); err == nil {
		t.Fatal("a page signed by another key was taken")
	}
	if trs, _ := r.st.Translations([]string{id}, "zh-Hans"); len(trs) != 0 {
		t.Fatalf("kept %+v from a page that did not verify", trs)
	}
	if err := r.p.pullTranslations(peer.peer(0)); err != nil {
		t.Fatal(err)
	}
	if trs, _ := r.st.Translations([]string{id}, "zh-Hans"); trs[id].Text != "大家好。" {
		t.Fatalf("kept %+v, want the peer's", trs)
	}
}

// peerAdd is an admin's peer.add for the serving hub, as Ingest takes it.
func peerAdd(t *testing.T, r *rig, peer *servingHub) ([]byte, []byte, *envelope.Envelope, any) {
	t.Helper()
	r.seq++
	raw, _ := json.Marshal(map[string]any{
		"type": "peer.add", "author": base64.StdEncoding.EncodeToString(r.pub), "seq": r.seq, "ts": 1756500000000,
		"body": map[string]any{"hub": peer.id.ID, "addr": peer.peer(0).Addr},
	})
	e, err := envelope.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	op, err := e.Op()
	if err != nil {
		t.Fatal(err)
	}
	return raw, ed25519.Sign(r.priv, append([]byte(envelope.Prefix), raw...)), e, op
}
