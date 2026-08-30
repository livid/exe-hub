package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"exehub/internal/envelope"
	"exehub/internal/identity"
)

type author struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	seq  int64
}

func newAuthor(t *testing.T) *author {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &author{priv: priv, pub: pub}
}

func (a *author) msg(t *testing.T, typ string, body any) ([]byte, []byte, *envelope.Envelope, any) {
	t.Helper()
	a.seq++
	raw, err := json.Marshal(map[string]any{
		"type": typ, "author": base64.StdEncoding.EncodeToString(a.pub),
		"seq": a.seq, "ts": 1756500000000, "body": body,
	})
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(a.priv, append([]byte(envelope.Prefix), raw...))
	e, err := envelope.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := envelope.Verify(raw, sig, e); err != nil {
		t.Fatal(err)
	}
	op, err := e.Op()
	if err != nil {
		t.Fatal(err)
	}
	return raw, sig, e, op
}

func (a *author) id() string { return identity.Fingerprint(a.pub) }

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ingest(t *testing.T, s *Store, a *author, typ string, body any) string {
	t.Helper()
	raw, sig, e, op := a.msg(t, typ, body)
	id, _, err := s.Ingest(raw, sig, e, op)
	if err != nil {
		t.Fatalf("ingest %s: %v", typ, err)
	}
	return id
}

func TestIngestFeedAndReplies(t *testing.T) {
	s := openTest(t)
	alice, bob := newAuthor(t), newAuthor(t)

	ingest(t, s, alice, "profile.set", map[string]string{"name": "Alice", "bio": "hi"})
	p1 := ingest(t, s, alice, "post.create", map[string]string{"text": "first!"})
	p2 := ingest(t, s, bob, "post.create", map[string]string{"text": "second"})
	r1 := ingest(t, s, bob, "post.create", map[string]string{"text": "re: first", "reply_to": p1})

	// the home feed hides replies; replies=1 shows everything
	feed, err := s.Feed("", 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 2 {
		t.Fatalf("feed len %d, want 2 (reply hidden)", len(feed))
	}
	if feed[0].ID != p2 || feed[1].ID != p1 {
		t.Fatalf("feed order wrong: %v", []string{feed[0].ID, feed[1].ID})
	}
	if feed[1].AuthorName != "Alice" {
		t.Fatalf("author name not joined: %q", feed[1].AuthorName)
	}
	if feed[1].Replies != 1 {
		t.Fatalf("reply count %d, want 1", feed[1].Replies)
	}
	all, err := s.Feed("", 50, true)
	if err != nil || len(all) != 3 || all[0].ID != r1 {
		t.Fatalf("replies=1 feed: %v len %d", err, len(all))
	}

	// keyset pagination: page of 1, then the rest
	page, err := s.Feed("", 1, false)
	if err != nil || len(page) != 1 {
		t.Fatalf("page1: %v len %d", err, len(page))
	}
	page2, err := s.Feed(page[0].ID, 2, false)
	if err != nil || len(page2) != 1 || page2[0].ID != p1 {
		t.Fatalf("page2 wrong: %v", page2)
	}

	post, err := s.Post(p1)
	if err != nil || post.Text != "first!" {
		t.Fatalf("post: %v %v", post, err)
	}
	replies, err := s.Replies(p1, "", 50)
	if err != nil || len(replies) != 1 || replies[0].ID != r1 {
		t.Fatalf("replies: %v %v", replies, err)
	}

	pf, err := s.ProfileFeed(bob.id(), "", 50)
	if err != nil || len(pf) != 2 {
		t.Fatalf("profile feed: %v len %d", err, len(pf))
	}
	_ = p2
}

func TestSeqAndDuplicates(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	raw, sig, e, op := a.msg(t, "post.create", map[string]string{"text": "x"})
	if _, _, err := s.Ingest(raw, sig, e, op); err != nil {
		t.Fatal(err)
	}
	// same message again: duplicate
	if _, _, err := s.Ingest(raw, sig, e, op); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	// a fresh message re-using the old seq: stale
	a.seq = 0
	raw2, sig2, e2, op2 := a.msg(t, "post.create", map[string]string{"text": "y"})
	if _, _, err := s.Ingest(raw2, sig2, e2, op2); !errors.Is(err, ErrStaleSeq) {
		t.Fatalf("want ErrStaleSeq, got %v", err)
	}
	n, err := s.Seq(e.Author)
	if err != nil || n != 1 {
		t.Fatalf("seq %d %v, want 1", n, err)
	}
}

func TestDeleteOwnershipAndPins(t *testing.T) {
	s := openTest(t)
	alice, mallory := newAuthor(t), newAuthor(t)

	if err := s.AddPin("bafytestcid234567", 100, "image/png", false); err != nil {
		t.Fatal(err)
	}
	p := ingest(t, s, alice, "post.create", map[string]any{
		"text": "pic", "embeds": []map[string]string{{"cid": "bafytestcid234567", "mime": "image/png"}}})

	// a post referencing an unknown CID is rejected
	raw, sig, e, op := alice.msg(t, "post.create", map[string]any{
		"text": "bad", "embeds": []map[string]string{{"cid": "bafyunknowncid234", "mime": "image/png"}}})
	if _, _, err := s.Ingest(raw, sig, e, op); !errors.Is(err, ErrNoPin) {
		t.Fatalf("want ErrNoPin, got %v", err)
	}
	alice.seq-- // the rejected message consumed no seq

	// mallory cannot delete alice's post
	raw, sig, e, op = mallory.msg(t, "post.delete", map[string]string{"post": p})
	if _, _, err := s.Ingest(raw, sig, e, op); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("want ErrNotOwner, got %v", err)
	}

	// alice deletes; the pin refcount drops to zero and is handed back
	raw, sig, e, op = alice.msg(t, "post.delete", map[string]string{"post": p})
	_, unpin, err := s.Ingest(raw, sig, e, op)
	if err != nil {
		t.Fatal(err)
	}
	if len(unpin) != 1 || unpin[0] != "bafytestcid234567" {
		t.Fatalf("unpin %v", unpin)
	}
	if feed, _ := s.Feed("", 50, false); len(feed) != 0 {
		t.Fatalf("post still in feed after delete")
	}
	// the log still holds the embed post whose pin row is now gone —
	// rebuild must replay through it (pin checks are ingest-time policy)
	if err := s.Rebuild(); err != nil {
		t.Fatalf("rebuild after embed delete: %v", err)
	}
}

func TestAvatarPins(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	if err := s.AddPin("bafyavatarone2345", 50, "image/png", true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPin("bafyavatartwo2345", 50, "image/png", true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPin("bafyplainpng23456", 50, "image/png", false); err != nil {
		t.Fatal(err)
	}

	// a plain (non-avatar) pin is rejected as a profile image
	raw, sig, e, op := a.msg(t, "profile.set", map[string]string{"name": "A", "avatar": "bafyplainpng23456"})
	if _, _, err := s.Ingest(raw, sig, e, op); !errors.Is(err, ErrNotAvatar) {
		t.Fatalf("want ErrNotAvatar, got %v", err)
	}
	a.seq--

	ingest(t, s, a, "profile.set", map[string]string{"name": "A", "avatar": "bafyavatarone2345"})
	// replacing the avatar releases the old pin (refs hit 0 → unpin)
	raw, sig, e, op = a.msg(t, "profile.set", map[string]string{"name": "A", "avatar": "bafyavatartwo2345"})
	_, unpin, err := s.Ingest(raw, sig, e, op)
	if err != nil {
		t.Fatal(err)
	}
	if len(unpin) != 1 || unpin[0] != "bafyavatarone2345" {
		t.Fatalf("unpin %v, want the replaced avatar", unpin)
	}
	// re-setting the same avatar must not double-count or release it
	raw, sig, e, op = a.msg(t, "profile.set", map[string]string{"name": "A renamed", "avatar": "bafyavatartwo2345"})
	if _, unpin, err = s.Ingest(raw, sig, e, op); err != nil || len(unpin) != 0 {
		t.Fatalf("same-avatar update: unpin=%v err=%v", unpin, err)
	}
	p, err := s.Profile(a.id())
	if err != nil || p.Avatar != "bafyavatartwo2345" || p.Name != "A renamed" {
		t.Fatalf("profile %+v %v", p, err)
	}
	if err := s.Rebuild(); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Profile(a.id())
	if p.Avatar != "bafyavatartwo2345" {
		t.Fatalf("avatar lost in rebuild: %+v", p)
	}
}

func TestBans(t *testing.T) {
	s := openTest(t)
	admin, troll := newAuthor(t), newAuthor(t)

	ingest(t, s, admin, "ban.set", map[string]string{"target": troll.id(), "reason": "spam"})
	if banned, _ := s.Banned(troll.id()); !banned {
		t.Fatal("not banned")
	}
	ingest(t, s, admin, "ban.lift", map[string]string{"target": troll.id()})
	if banned, _ := s.Banned(troll.id()); banned {
		t.Fatal("still banned")
	}
}

func TestRebuild(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	ingest(t, s, a, "profile.set", map[string]string{"name": "A"})
	p1 := ingest(t, s, a, "post.create", map[string]string{"text": "keep"})
	p2 := ingest(t, s, a, "post.create", map[string]string{"text": "gone"})
	ingest(t, s, a, "post.delete", map[string]string{"post": p2})

	if err := s.Rebuild(); err != nil {
		t.Fatal(err)
	}
	feed, err := s.Feed("", 50, false)
	if err != nil || len(feed) != 1 || feed[0].ID != p1 {
		t.Fatalf("rebuilt feed wrong: %v %v", feed, err)
	}
	if feed[0].AuthorName != "A" {
		t.Fatal("profile lost in rebuild")
	}
	n, _ := s.Seq(base64.StdEncoding.EncodeToString(a.pub))
	if n != 4 {
		t.Fatalf("seq after rebuild %d, want 4", n)
	}
}
