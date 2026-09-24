package replicate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"exehub/internal/envelope"
	"exehub/internal/ipfs"
	"exehub/internal/store"
)

// what a PNG starts with: enough for the sniffer to call it one
var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

// fakePeer is a hub that serves the files it holds, under whatever
// Content-Type it likes, and counts what it is asked.
type fakePeer struct {
	hub   string
	srv   *httptest.Server
	files map[string][]byte
	ctype string
	asked int
}

func newPeer(t *testing.T, hub string, cids ...string) *fakePeer {
	t.Helper()
	fp := &fakePeer{hub: hub, files: map[string][]byte{}, ctype: "image/png"}
	for _, c := range cids {
		fp.files[c] = png
	}
	fp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.asked++
		body, ok := fp.files[r.URL.Path[len("/v1/embed/"):]]
		if !ok {
			http.Error(w, `{"error":"no such embed"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", fp.ctype)
		w.Write(body)
	}))
	t.Cleanup(fp.srv.Close)
	return fp
}

func (fp *fakePeer) peer() store.Peer {
	u, _ := url.Parse(fp.srv.URL)
	return store.Peer{Hub: fp.hub, Addr: "/ip4/" + u.Hostname() + "/tcp/" + u.Port() + "/http"}
}

// rig is a puller over an empty store and a kubo that mints, for any
// bytes, the CID the test expects next.
type rig struct {
	t    *testing.T
	st   *store.Store
	p    *Puller
	kubo *httptest.Server
	mint string // the CID kubo answers an add with
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	seq  int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r.st = st
	r.kubo = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, `{"Name":"x","Hash":%q}`+"\n", r.mint)
	}))
	t.Cleanup(r.kubo.Close)
	r.pub, r.priv, _ = ed25519.GenerateKey(rand.Reader)
	r.p = &Puller{St: st, IPFS: ipfs.New(r.kubo.URL), Self: "0011223344556677", client: &http.Client{Timeout: 5 * time.Second}}
	return r
}

// lands ingests a post with one picture the way a pull whose mirror failed
// left it: the post is there, the pin is not.
func (r *rig) lands(cid, origin string) {
	r.t.Helper()
	r.seq++
	raw, _ := json.Marshal(map[string]any{
		"type": "post.create", "author": base64.StdEncoding.EncodeToString(r.pub), "seq": r.seq, "ts": 1756500000000,
		"body": map[string]any{"text": fmt.Sprint("pic ", r.seq), "embeds": []map[string]string{{"cid": cid, "mime": "image/png"}}},
	})
	e, err := envelope.Parse(raw)
	if err != nil {
		r.t.Fatal(err)
	}
	op, err := e.Op()
	if err != nil {
		r.t.Fatal(err)
	}
	sig := ed25519.Sign(r.priv, append([]byte(envelope.Prefix), raw...))
	if _, _, err := r.st.IngestReplicated(raw, sig, e, op, origin); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) pinned(cid string) bool {
	_, err := r.st.PinInfo(cid)
	return err == nil
}

// due makes every waiting file's turn come now.
func (r *rig) due() {
	for _, try := range r.p.healing {
		try.at = time.Now().Add(-time.Second)
	}
}

// A hub that boots before its kubo pulls its first page with no way to
// mirror: the post lands, the picture does not. While kubo is down heal
// asks nobody and no wait grows; the cycle after kubo answers, the file is
// there, its pin carrying the post's reference.
func TestHealWaitsForKubo(t *testing.T) {
	const cid = "bafyhealcid234567"
	r := newRig(t)
	a := newPeer(t, "aaaaaaaaaaaaaaaa", cid)
	r.lands(cid, a.hub)

	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // an address nothing listens on: connection refused
	up := r.p.IPFS
	r.p.IPFS = ipfs.New(down.URL)
	r.p.heal([]store.Peer{a.peer()})
	if r.pinned(cid) || a.asked != 0 || len(r.p.healing) != 0 {
		t.Fatalf("with kubo down: pinned %v, peer asked %d times, waits %+v", r.pinned(cid), a.asked, r.p.healing)
	}

	r.p.IPFS, r.mint = up, cid
	r.p.heal([]store.Peer{a.peer()})
	pin, err := r.st.PinInfo(cid)
	if err != nil || pin.MIME != "image/png" || pin.Size != int64(len(png)) {
		t.Fatalf("pin = %+v, %v", pin, err)
	}
	if missing, _ := r.st.MissingMirrors(); len(missing) != 0 || len(r.p.healing) != 0 {
		t.Fatalf("still missing %+v, still waiting %+v", missing, r.p.healing)
	}
}

// The peer a post came through has lost the file; another configured peer,
// which never sent the post here, holds it. One round asks both, the named
// one first, and the file is healed without any wait.
func TestHealAsksEveryPeerInOneRound(t *testing.T) {
	const cid = "bafyhealcid234567"
	r := newRig(t)
	a := newPeer(t, "aaaaaaaaaaaaaaaa")      // named the file, lost it
	b := newPeer(t, "bbbbbbbbbbbbbbbb", cid) // never named it, has it
	r.lands(cid, a.hub)
	r.mint = cid

	r.p.heal([]store.Peer{b.peer(), a.peer()}) // the store's order puts b first; the named peer still goes first
	if !r.pinned(cid) || a.asked != 1 || b.asked != 1 || len(r.p.healing) != 0 {
		t.Fatalf("pinned %v, a asked %d, b asked %d, waits %+v", r.pinned(cid), a.asked, b.asked, r.p.healing)
	}
}

// Only a round in which every peer failed backs the file off; the wait
// then holds for every peer alike, and a peer that was not there last
// cycle — new, or back from an outage — starts it over.
func TestHealBacksOffAfterAllFailedAndStartsOverForANewPeer(t *testing.T) {
	const cid = "bafyhealcid234567"
	r := newRig(t)
	a, b := newPeer(t, "aaaaaaaaaaaaaaaa"), newPeer(t, "bbbbbbbbbbbbbbbb")
	r.lands(cid, a.hub)
	r.lands(cid, b.hub) // named through both: still one file, one round
	r.mint = cid
	two := []store.Peer{a.peer(), b.peer()}

	r.p.heal(two)
	if try := r.p.healing[cid]; try == nil || try.wait != interval || a.asked != 1 || b.asked != 1 {
		t.Fatalf("after one failed round: %+v, a asked %d, b asked %d", try, a.asked, b.asked)
	}
	r.p.heal(two) // not its turn: nobody is asked
	if a.asked != 1 || b.asked != 1 {
		t.Fatalf("asked before the wait ran out: a %d, b %d", a.asked, b.asked)
	}
	r.due()
	r.p.heal(two)
	if try := r.p.healing[cid]; try.wait != 2*interval || a.asked != 2 || b.asked != 2 {
		t.Fatalf("after two failed rounds: %+v, a asked %d, b asked %d", try, a.asked, b.asked)
	}

	c := newPeer(t, "cccccccccccccccc", cid)
	r.p.heal(append(two, c.peer())) // the wait has an hour to run, and c is asked now
	if !r.pinned(cid) || c.asked != 1 {
		t.Fatalf("new peer: pinned %v, c asked %d", r.pinned(cid), c.asked)
	}
}

// Nothing but the bytes is taken from a peer: the type a mirrored file is
// served with is read from the bytes, so a peer cannot label a picture as
// something a browser would run. Bytes that hash to another CID are
// refused, and that counts as a failed round.
func TestMirrorTakesOnlyBytesFromAPeer(t *testing.T) {
	const cid = "bafyhealcid234567"
	r := newRig(t)
	a := newPeer(t, "aaaaaaaaaaaaaaaa", cid)
	a.ctype = "image/svg+xml"
	r.lands(cid, a.hub)

	r.mint = "bafysomethingelse"
	r.p.heal([]store.Peer{a.peer()})
	if r.pinned(cid) || r.p.healing[cid] == nil {
		t.Fatalf("kept bytes that mint another CID: pinned %v, waits %+v", r.pinned(cid), r.p.healing)
	}

	r.mint = cid
	r.due()
	r.p.heal([]store.Peer{a.peer()})
	if pin, err := r.st.PinInfo(cid); err != nil || pin.MIME != "image/png" {
		t.Fatalf("pin = %+v, %v; want the sniffed image/png", pin, err)
	}
}

// A peer that stops answering is asked once in a cycle, however many files
// are missing, and its silence says nothing about them: no wait grows.
func TestHealLeavesASilentPeerAlone(t *testing.T) {
	r := newRig(t)
	r.mint = "bafyhealcid234567"
	dead := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(dead.URL)
	dead.Close()
	peer := store.Peer{Hub: "dddddddddddddddd", Addr: "/ip4/" + u.Hostname() + "/tcp/" + u.Port() + "/http"}
	r.lands("bafyhealcid234567", peer.Hub)
	r.lands("bafyhealcid765432", peer.Hub)

	tries := 0
	r.p.client = &http.Client{Timeout: 5 * time.Second, Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		tries++
		return http.DefaultTransport.RoundTrip(req)
	})}
	r.p.heal([]store.Peer{peer})
	if tries != 1 || len(r.p.healing) != 0 {
		t.Fatalf("silent peer tried %d times for two files, waits %+v", tries, r.p.healing)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestMarkReplicates: a post.mark travels between hubs like the post it
// ticks — the puller keeps it (its type switch dropped every op it did
// not name, which lost the first marks on the public hub, 2026-09-24)
// and the box shows ticked here after the pull.
func TestMarkReplicates(t *testing.T) {
	r := newRig(t)
	peer := newServingHub(t)
	raw, sig, e, op := r.signed("- [ ] one\n- [ ] two")
	id, _, err := peer.st.Ingest(raw, sig, e, op)
	if err != nil {
		t.Fatal(err)
	}
	r.seq++
	mraw, _ := json.Marshal(map[string]any{
		"type": "post.mark", "author": base64.StdEncoding.EncodeToString(r.pub), "seq": r.seq, "ts": 1756500000001,
		"body": map[string]any{"post": id, "box": 1, "done": true},
	})
	me, err := envelope.Parse(mraw)
	if err != nil {
		t.Fatal(err)
	}
	mop, err := me.Op()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := peer.st.Ingest(mraw, ed25519.Sign(r.priv, append([]byte(envelope.Prefix), mraw...)), me, mop); err != nil {
		t.Fatal(err)
	}
	r.st.Ingest(peerAdd(t, r, peer))
	r.p.round()
	p, err := r.st.Post(id)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Boxes[1] {
		t.Fatalf("after the pull, boxes %v: want box 1 ticked", p.Boxes)
	}
}
