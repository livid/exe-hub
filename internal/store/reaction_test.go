package store

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"exehub/internal/envelope"
)

// TestReactions: a post.react gives an emoji to a post or takes it back,
// the newest by its own ts standing whatever the order it arrives in; a
// post that is not here is refused; a post's reactions stand in the
// order each emoji was first given, each with who gave it, earliest
// first; who reacted comes with names; Rebuild lands on the same; a
// deleted post takes them with it; and the op replicates.
func TestReactions(t *testing.T) {
	s := openTest(t)
	alice, bob, carol := newAuthor(t), newAuthor(t), newAuthor(t)
	ingest(t, s, alice, "profile.set", map[string]string{"name": "Alice"})
	ingest(t, s, bob, "profile.set", map[string]string{"name": "Bob"})
	p := ingest(t, s, alice, "post.create", map[string]string{"text": "exe is out"})
	other := ingest(t, s, alice, "post.create", map[string]string{"text": "another"})

	raw, sig, e, op := bob.msg(t, "post.react", map[string]any{"post": fmt.Sprintf("%064x", 7), "emoji": "🎉", "on": true})
	if _, _, err := s.Ingest(raw, sig, e, op); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a reaction to no post: want ErrNotFound, got %v", err)
	}
	bob.seq--

	react := func(a *author, ts int64, emoji string, on bool) {
		t.Helper()
		a.ts = ts
		ingest(t, s, a, "post.react", map[string]any{"post": p, "emoji": emoji, "on": on})
	}
	type row struct {
		emoji string
		by    []string
	}
	check := func(want ...row) {
		t.Helper()
		got, err := s.Post(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Reactions) != len(want) {
			t.Fatalf("reactions %v, want %v", got.Reactions, want)
		}
		for i, w := range want {
			if got.Reactions[i].Emoji != w.emoji || !slices.Equal(got.Reactions[i].By, w.by) {
				t.Fatalf("reactions %v, want %v", got.Reactions, want)
			}
		}
	}
	check()
	react(bob, 1000, "🎉", true)
	react(alice, 2000, "🚀", true) // one's own post too
	react(carol, 3000, "🎉", true)
	react(bob, 4000, "🚀", true) // a second emoji from the same author
	check(row{"🎉", []string{bob.id(), carol.id()}}, row{"🚀", []string{alice.id(), bob.id()}})

	// taken back: 🎉 keeps its place though its first giver is gone
	react(bob, 5000, "🎉", false)
	check(row{"🎉", []string{carol.id()}}, row{"🚀", []string{alice.id(), bob.id()}})
	// an op made before the take-back, arriving after it, loses
	react(bob, 4500, "🎉", true)
	check(row{"🎉", []string{carol.id()}}, row{"🚀", []string{alice.id(), bob.id()}})
	// a newer one gives it again, and the row orders by when it was given
	react(bob, 6000, "🎉", true)
	check(row{"🎉", []string{carol.id(), bob.id()}}, row{"🚀", []string{alice.id(), bob.id()}})
	// the last of an emoji taken back leaves the row
	react(alice, 7000, "🚀", false)
	react(bob, 7000, "🚀", false)
	check(row{"🎉", []string{carol.id(), bob.id()}})

	who, err := s.PostReactions(p)
	if err != nil || len(who) != 1 || who[0].Emoji != "🎉" || len(who[0].People) != 2 {
		t.Fatalf("who: %+v, %v", who, err)
	}
	if c, b := who[0].People[0], who[0].People[1]; c.ID != carol.id() || c.Name != "" || c.TS != 3000 || b.ID != bob.id() || b.Name != "Bob" || b.TS != 6000 {
		t.Fatalf("who: %+v", who[0].People)
	}
	if who, err := s.PostReactions(other); err != nil || len(who) != 0 {
		t.Fatalf("who on a post with none: %+v, %v", who, err)
	}
	if _, err := s.PostReactions(fmt.Sprintf("%064x", 7)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("who on no post: want ErrNotFound, got %v", err)
	}

	if err := s.Rebuild(); err != nil {
		t.Fatal(err)
	}
	check(row{"🎉", []string{carol.id(), bob.id()}})

	// a reaction travels between hubs like a post
	page, _, err := s.ReplicationPage(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	reacts := 0
	for _, m := range page {
		if e, err := envelope.Parse(m.Envelope); err == nil && e.Type == "post.react" {
			reacts++
		}
	}
	if reacts != 9 {
		t.Fatalf("replication page carries %d reactions, want 9", reacts)
	}

	ingest(t, s, alice, "post.delete", map[string]string{"post": p})
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reactions WHERE post=?`, p).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reactions after delete: %d, %v", n, err)
	}
}

// TestReactionsAgree: two hubs taking one author's reactions in opposite
// orders — the take-back first on one, last on the other — show the
// same, since the newest by ts stands.
func TestReactionsAgree(t *testing.T) {
	a, b := openTest(t), openTest(t)
	alice, bob := newAuthor(t), newAuthor(t)
	raw, sig, e, op := alice.msg(t, "post.create", map[string]string{"text": "both hubs"})
	var p string
	for _, s := range []*Store{a, b} {
		id, _, err := s.Ingest(raw, sig, e, op)
		if err != nil {
			t.Fatal(err)
		}
		p = id
	}
	type msg struct {
		raw, sig []byte
		e        *envelope.Envelope
		op       any
	}
	var ms []msg
	for i, on := range []bool{true, false} {
		bob.ts = int64(1000 * (i + 1))
		raw, sig, e, op := bob.msg(t, "post.react", map[string]any{"post": p, "emoji": "👀", "on": on})
		ms = append(ms, msg{raw, sig, e, op})
	}
	for _, m := range ms {
		if _, _, err := a.Ingest(m.raw, m.sig, m.e, m.op); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range slices.Backward(ms) {
		if _, _, err := b.IngestReplicated(m.raw, m.sig, m.e, m.op, "peer"); err != nil {
			t.Fatal(err)
		}
	}
	for i, s := range []*Store{a, b} {
		got, err := s.Post(p)
		if err != nil || len(got.Reactions) != 0 {
			t.Fatalf("hub %d: %v, %v", i, got.Reactions, err)
		}
	}
}
