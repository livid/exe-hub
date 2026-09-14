package store

import (
	"testing"
	"time"
	"unicode/utf8"
)

// TestCards: a card joins its post in every read, a failed attempt stays
// invisible, deleting the post releases the picture's pin, and Rebuild
// keeps card pins honest (cards are not in the log).
func TestCards(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	id := ingest(t, s, a, "post.create", map[string]any{"text": "see https://a.example/x"})

	if has, _ := s.HasCard(id); has {
		t.Fatal("card before any attempt")
	}
	want := Card{URL: "https://a.example/x", Host: "a.example", Title: "A Page", Desc: "words", Image: "bafycard"}
	if _, err := s.SetCard(id, want, 123, "image/png", true); err != nil {
		t.Fatal(err)
	}
	posts, err := s.Feed("", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].Card == nil || *posts[0].Card != want {
		t.Fatalf("feed card = %+v", posts[0].Card)
	}
	var refs int
	if err := s.db.QueryRow(`SELECT refs FROM pins WHERE cid='bafycard'`).Scan(&refs); err != nil || refs != 1 {
		t.Fatalf("picture pin refs = %d (%v), want 1", refs, err)
	}

	// backfill skips attempted posts
	left, err := s.PostsWithoutCards(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("PostsWithoutCards = %d, want 0", len(left))
	}

	// a card stored with raw legacy bytes is listed for a redo; a UTF-8
	// one and a failed attempt are not
	notUTF8 := func(title, desc string) bool {
		return !utf8.ValidString(title + desc)
	}
	idMis := ingest(t, s, a, "post.create", map[string]any{"text": "https://mame.example/x"})
	if _, err := s.SetCard(idMis, Card{URL: "https://mame.example/x", Host: "mame.example", Title: "\x83\x7d\x83\x81"}, 0, "", true); err != nil {
		t.Fatal(err)
	}
	mis, err := s.CardsMisread(notUTF8)
	if err != nil {
		t.Fatal(err)
	}
	if len(mis) != 1 || mis[0].ID != idMis || mis[0].Text != "https://mame.example/x" {
		t.Fatalf("CardsMisread = %+v, want only the raw-bytes card", mis)
	}
	if _, err := s.SetCard(idMis, Card{URL: "https://mame.example/x", Host: "mame.example", Title: "マメ"}, 0, "", true); err != nil {
		t.Fatal(err)
	}
	if mis, _ := s.CardsMisread(notUTF8); len(mis) != 0 {
		t.Fatalf("redone card still listed: %+v", mis)
	}

	// a failed attempt is recorded but never served
	id2 := ingest(t, s, a, "post.create", map[string]any{"text": "https://dead.example"})
	if _, err := s.SetCard(id2, Card{URL: "https://dead.example"}, 0, "", false); err != nil {
		t.Fatal(err)
	}
	p2, err := s.Post(id2)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Card != nil {
		t.Fatalf("failed card served: %+v", p2.Card)
	}

	// Rebuild replays the log; the card, absent from it, must survive
	// with its pin's refcount intact.
	if err := s.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT refs FROM pins WHERE cid='bafycard'`).Scan(&refs); err != nil || refs != 1 {
		t.Fatalf("after rebuild, picture pin refs = %d (%v), want 1", refs, err)
	}
	p1, err := s.Post(id)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Card == nil {
		t.Fatal("card gone after rebuild")
	}

	// an archive round: begun once per window, counted, bounded, and the
	// copy served in the card once set (see PLAN.md, Archived copies)
	far := time.Now().Add(time.Hour).UnixMilli()
	todo, err := s.CardsToArchive(3, far, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(todo) != 2 { // the ok cards; the failed one is never sent to the Archive
		t.Fatalf("CardsToArchive = %+v, want the two ok cards", todo)
	}
	if link, err := s.BeginArchive(id2, 3, far); err != nil || link != "" {
		t.Fatalf("BeginArchive on a failed card = %q, %v", link, err)
	}
	link, err := s.BeginArchive(id, 3, time.Now().UnixMilli()+1)
	if err != nil || link != "https://a.example/x" {
		t.Fatalf("BeginArchive = %q, %v", link, err)
	}
	if again, _ := s.BeginArchive(id, 3, time.Now().Add(-time.Hour).UnixMilli()); again != "" {
		t.Fatal("a second round began inside the window")
	}
	if todo, _ := s.CardsToArchive(3, time.Now().Add(-time.Hour).UnixMilli(), 10); len(todo) != 1 || todo[0].ID != idMis || todo[0].Text != "https://mame.example/x" {
		t.Fatalf("sweep after a round = %+v, want only the untried card", todo)
	}
	for i := 0; i < 2; i++ {
		if l, _ := s.BeginArchive(id, 3, far); l == "" {
			t.Fatalf("round %d refused", i+2)
		}
	}
	if l, _ := s.BeginArchive(id, 3, far); l != "" {
		t.Fatal("a fourth round began")
	}
	const copyURL = "https://web.archive.org/web/20180523210631/https://a.example/x"
	if err := s.SetArchive(id, copyURL); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Post(id); p.Card == nil || p.Card.Archive != copyURL || p.Card.ArchiveDate() != "2018-05-23" {
		t.Fatalf("card after SetArchive = %+v", p.Card)
	}
	if (Card{}).ArchiveDate() != "" || (Card{Archive: "https://web.archive.org/web/2018x/y"}).ArchiveDate() != "" {
		t.Error("ArchiveDate of no copy or a bad stamp should be empty")
	}

	// deleting the post takes the card and queues the picture's unpin
	raw, sig, e, op := a.msg(t, "post.delete", map[string]any{"post": id})
	_, unpin, err := s.Ingest(raw, sig, e, op)
	if err != nil {
		t.Fatal(err)
	}
	if len(unpin) != 1 || unpin[0] != "bafycard" {
		t.Fatalf("unpin = %v, want [bafycard]", unpin)
	}
	if has, _ := s.HasCard(id); has {
		t.Fatal("card outlived its post")
	}
}
