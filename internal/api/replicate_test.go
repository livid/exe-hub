package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/identity"
)

// TestReplicateHandler round-trips a signed page the way a puller would:
// verify the hub signature over the exact payload bytes, the nonce echo,
// and the hub id.
func TestReplicateHandler(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.Hub = id

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	post(t, s, priv, pub, 1) // seeds one accepted local post

	req := httptest.NewRequest("GET", "/v1/replicate?after=0&limit=10&nonce=deadbeef01", nil)
	w := httptest.NewRecorder()
	s.handleReplicate(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var out struct {
		Payload json.RawMessage `json:"payload"`
		Sig     []byte          `json:"sig"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	pubB, err := base64.StdEncoding.DecodeString(id.PubKey())
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pubB), append([]byte(envelope.ReplicatePrefix), out.Payload...), out.Sig) {
		t.Fatal("page signature does not verify")
	}
	pl := &ReplicatePayload{}
	if err := json.Unmarshal(out.Payload, pl); err != nil {
		t.Fatal(err)
	}
	if pl.Nonce != "deadbeef01" || pl.Hub != id.ID || len(pl.Messages) != 1 || pl.Next == 0 {
		t.Fatalf("payload = %+v", pl)
	}

	// a missing nonce is refused; allow_replication=false turns pulls off
	w = httptest.NewRecorder()
	s.handleReplicate(w, httptest.NewRequest("GET", "/v1/replicate?after=0", nil))
	if w.Code != 400 {
		t.Fatalf("no nonce: status %d", w.Code)
	}
	no := false
	s.Cfg.Set(&config.Config{Gate: config.Gate{Mode: "open"}, AllowReplication: &no})
	w = httptest.NewRecorder()
	s.handleReplicate(w, httptest.NewRequest("GET", "/v1/replicate?after=0&nonce=deadbeef01", nil))
	if w.Code != 403 {
		t.Fatalf("replication off: status %d", w.Code)
	}
}
