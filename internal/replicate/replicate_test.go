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

// A hub that boots before its kubo pulls its first page with no way to
// mirror: the post lands, the picture does not. heal goes back for it once
// kubo answers — on its backoff, not every cycle — and the late pin carries
// the post's reference.
func TestHealRetriesAFailedMirror(t *testing.T) {
	const cid, peerHub = "bafyhealcid234567", "ffeeddccbbaa9988"
	picture := []byte("png bytes")

	asked := 0
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embed/"+cid {
			http.NotFound(w, r)
			return
		}
		asked++
		w.Header().Set("Content-Type", "image/png")
		w.Write(picture)
	}))
	defer peer.Close()
	kubo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"Name":"x","Hash":%q}`+"\n", cid)
	}))
	defer kubo.Close()
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close() // an address nothing listens on: connection refused

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, _ := url.Parse(peer.URL)
	peers := []store.Peer{{Hub: peerHub, Addr: "/ip4/" + u.Hostname() + "/tcp/" + u.Port() + "/http"}}

	// the post as the first pull left it: ingested, its mirror failed
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := st.IngestReplicated(mustMsg(t, pub, priv, 1, cid)); err != nil {
		t.Fatal(err)
	}

	p := &Puller{St: st, IPFS: ipfs.New(down.URL), Self: "0011223344556677", client: &http.Client{Timeout: 5 * time.Second}}
	p.heal(peers)
	if _, err := st.PinInfo(cid); err == nil {
		t.Fatal("pinned with kubo down")
	}
	try := p.healing[cid]
	if try == nil || try.wait != interval {
		t.Fatalf("after one failure: %+v, want a wait of one cycle", try)
	}

	// kubo is up, but the CID's turn has not come: nothing is asked
	p.IPFS = ipfs.New(kubo.URL)
	before := asked
	p.heal(peers)
	if asked != before {
		t.Fatal("retried before its backoff ran out")
	}
	try.at = time.Now().Add(-time.Second)
	p.heal(peers)
	pin, err := st.PinInfo(cid)
	if err != nil || pin.MIME != "image/png" || pin.Size != int64(len(picture)) {
		t.Fatalf("pin = %+v, %v", pin, err)
	}
	if missing, _ := st.MissingMirrors(); len(missing) != 0 || len(p.healing) != 0 {
		t.Fatalf("still missing %+v, still healing %+v", missing, p.healing)
	}

	// a peer that is no longer one is not asked
	if _, _, err := st.IngestReplicated(mustMsg(t, pub, priv, 2, "bafyhealcid765432")); err != nil {
		t.Fatal(err)
	}
	before = asked
	p.heal(nil)
	if asked != before {
		t.Fatal("asked a hub that is not a peer")
	}
}

// mustMsg is a signed post with one picture, as pulled from the peer.
func mustMsg(t *testing.T, pub ed25519.PublicKey, priv ed25519.PrivateKey, seq int, cid string) ([]byte, []byte, *envelope.Envelope, any, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"type": "post.create", "author": base64.StdEncoding.EncodeToString(pub), "seq": seq, "ts": 1756500000000,
		"body": map[string]any{"text": "pic", "embeds": []map[string]string{{"cid": cid, "mime": "image/png"}}},
	})
	e, err := envelope.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	op, err := e.Op()
	if err != nil {
		t.Fatal(err)
	}
	return raw, ed25519.Sign(priv, append([]byte(envelope.Prefix), raw...)), e, op, "ffeeddccbbaa9988"
}
