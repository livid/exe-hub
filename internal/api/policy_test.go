package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/gate"
	"exehub/internal/identity"
	"exehub/internal/store"
)

func testServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := config.NewHolder(cfg)
	return &Server{Cfg: h, St: st, Gate: gate.New(h)}
}

func post(t *testing.T, s *Server, priv ed25519.PrivateKey, pub ed25519.PublicKey, seq int64) *envelope.Envelope {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"type": "post.create", "author": base64.StdEncoding.EncodeToString(pub),
		"seq": seq, "ts": 1, "body": map[string]string{"text": "x"},
	})
	e, err := envelope.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	op, err := e.Op()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.St.Ingest(raw, ed25519.Sign(priv, raw), e, op); err != nil {
		t.Fatal(err)
	}
	next, _ := json.Marshal(map[string]any{
		"type": "post.create", "author": base64.StdEncoding.EncodeToString(pub),
		"seq": seq + 1, "ts": 1, "body": map[string]string{"text": "y"},
	})
	e2, _ := envelope.Parse(next)
	return e2
}

func TestPostCooldown(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})

	next := post(t, s, priv, pub, 1)
	err := s.policy(next)
	var cd *cooldownError
	if !errors.As(err, &cd) || cd.wait < 1 || cd.wait > 60 {
		t.Fatalf("want cooldownError with 1..60s wait, got %v", err)
	}

	// profile.set is never cooled down
	prof, _ := json.Marshal(map[string]any{
		"type": "profile.set", "author": base64.StdEncoding.EncodeToString(pub),
		"seq": 2, "ts": 1, "body": map[string]string{"name": "n"},
	})
	pe, _ := envelope.Parse(prof)
	if err := s.policy(pe); err != nil {
		t.Fatalf("profile.set hit cooldown: %v", err)
	}
}

func TestPostCooldownAdminAndDisabled(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	admin := identity.Fingerprint(pub)

	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}, Admins: []string{admin}})
	if err := s.policy(post(t, s, priv, pub, 1)); err != nil {
		t.Fatalf("admin hit cooldown: %v", err)
	}

	zero := 0
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	s2 := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}, Cooldown: &zero})
	if err := s2.policy(post(t, s2, priv2, pub2, 1)); err != nil {
		t.Fatalf("cooldown 0 still enforced: %v", err)
	}
}
