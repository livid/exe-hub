package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	st.PageAuthor = func(id string) bool { return h.Get().IsAdmin(id) }
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

// a post.mark is no post: it passes without the cooldown, and /v1/msg
// refuses one naming a box the post does not have while taking one it
// has, which the post then carries
func TestMarkPolicyAndHandler(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	author := base64.StdEncoding.EncodeToString(pub)
	send := func(seq int64, body any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"type": "post.mark", "author": author, "seq": seq, "ts": seq, "body": body})
		in, _ := json.Marshal(map[string][]byte{"envelope": raw, "sig": ed25519.Sign(priv, append([]byte(envelope.Prefix), raw...))})
		rec := httptest.NewRecorder()
		s.handleMsg(rec, httptest.NewRequest("POST", "/v1/msg", bytes.NewReader(in)))
		return rec.Code, rec.Body.String()
	}
	// the post, straight into the store: two boxes
	raw, _ := json.Marshal(map[string]any{"type": "post.create", "author": author, "seq": 1, "ts": 1, "body": map[string]string{"text": "- [ ] a\n- [ ] b"}})
	e, _ := envelope.Parse(raw)
	op, _ := e.Op()
	id, _, err := s.St.Ingest(raw, ed25519.Sign(priv, append([]byte(envelope.Prefix), raw...)), e, op)
	if err != nil {
		t.Fatal(err)
	}
	// the cooldown that a second post would hit does not touch a mark
	me, _ := json.Marshal(map[string]any{"type": "post.mark", "author": author, "seq": 2, "ts": 2, "body": map[string]any{"post": id, "box": 0, "done": true}})
	pe, _ := envelope.Parse(me)
	if err := s.policy(pe); err != nil {
		t.Fatalf("post.mark hit the policy: %v", err)
	}
	if code, body := send(2, map[string]any{"post": id, "box": 2, "done": true}); code != 400 || !strings.Contains(body, "no box 2") {
		t.Fatalf("box 2 of 2: %d %s", code, body)
	}
	if code, body := send(2, map[string]any{"post": id, "box": 1, "done": true}); code != 200 {
		t.Fatalf("box 1: %d %s", code, body)
	}
	p, err := s.St.Post(id)
	if err != nil || len(p.Boxes) != 1 || !p.Boxes[1] {
		t.Fatalf("post boxes %v, %v", p.Boxes, err)
	}
	// a mark for a post that is not here is a 400, not a crash
	if code, _ := send(3, map[string]any{"post": strings.Repeat("0", 64), "box": 0, "done": true}); code != 400 {
		t.Fatalf("mark on no post: %d", code)
	}
}
