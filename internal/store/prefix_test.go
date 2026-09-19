package store

import (
	"errors"
	"strings"
	"testing"
)

// TestResolvePrefix: the start of an id finds its post only when exactly
// one post ever began that way. Codex's cases: one match resolves; two
// never pick a winner, and deleting either one changes nothing, since
// the log keeps the post.create a delete leaves behind — an old short
// link fails rather than move to another post. A post that is gone, a
// prefix under the floor, and anything not lower-case hex are not found;
// a message that is no post does not count; the lookup is a range on
// the log's primary key.
func TestResolvePrefix(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	real := ingest(t, s, a, "post.create", map[string]any{"text": "the one with the six-column table"})
	other := ingest(t, s, a, "post.create", map[string]any{"text": "another post"})
	short := real[:PostPrefixMin]

	if id, err := s.ResolvePrefix(short); err != nil || id != real {
		t.Fatalf("ResolvePrefix(%s) = %q, %v; want %s", short, id, err, real)
	}
	if id, err := s.ResolvePrefix(real[:40]); err != nil || id != real {
		t.Fatalf("a longer prefix = %q, %v", id, err)
	}
	for _, bad := range []string{real[:PostPrefixMin-1], real[:8], "", strings.ToUpper(short), short[:11] + "g", short + "/", real, "zzzzzzzzzzzz"} {
		if id, err := s.ResolvePrefix(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolvePrefix(%q) = %q, %v; want ErrNotFound", bad, id, err)
		}
	}
	if _, err := s.ResolvePrefix(strings.Repeat("0", PostPrefixMin)); !errors.Is(err, ErrNotFound) {
		t.Errorf("a prefix no post has: %v", err)
	}

	// a message with the prefix that is no post does not make it ambiguous
	craft := func(id, typ string, seq int, post bool) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO messages (id, author, seq, type, ts, received, raw, sig) VALUES (?, 'crafted', ?, ?, 0, 0, x'', x'')`, id, seq, typ); err != nil {
			t.Fatal(err)
		}
		if post {
			if _, err := s.db.Exec(`INSERT INTO posts (id, author, text, ts, received, activity) VALUES (?, 'crafted', 'twin', 0, 1, 1)`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	craft(short+strings.Repeat("0", 64-PostPrefixMin), "profile.set", 1, false)
	if id, err := s.ResolvePrefix(short); err != nil || id != real {
		t.Fatalf("beside a profile.set with the prefix = %q, %v", id, err)
	}

	// a second post with the same first twelve: never a winner
	fill := "f" // any hex but the real post's own thirteenth, so one character more tells them apart
	if real[PostPrefixMin] == 'f' {
		fill = "e"
	}
	twin := short + strings.Repeat(fill, 64-PostPrefixMin)
	craft(twin, "post.create", 2, true)
	if id, err := s.ResolvePrefix(short); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("two posts = %q, %v; want ErrAmbiguous", id, err)
	}
	if id, err := s.ResolvePrefix(real[:PostPrefixMin+1]); err != nil || id != real {
		t.Fatalf("one character more tells them apart = %q, %v", id, err)
	}

	// delete the real one, the way a delete happens: its post.create stays in the log
	ingest(t, s, a, "post.delete", map[string]any{"post": real})
	if id, err := s.ResolvePrefix(short); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("after deleting one of two = %q, %v; the old short link must not move to the twin", id, err)
	}
	if id, err := s.ResolvePrefix(real[:PostPrefixMin+1]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the deleted post's own prefix = %q, %v; want ErrNotFound", id, err)
	}
	// and the other way round: the twin gone, the prefix still fits two posts that were
	if _, err := s.db.Exec(`DELETE FROM posts WHERE id = ?`, twin); err != nil {
		t.Fatal(err)
	}
	if id, err := s.ResolvePrefix(short); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("after deleting both = %q, %v", id, err)
	}
	if id, err := s.ResolvePrefix(other[:PostPrefixMin]); err != nil || id != other {
		t.Fatalf("an untouched post = %q, %v", id, err)
	}

	// a range on the primary key, never a walk of the log
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+prefixQuery, short, short+"g")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "SEARCH") || !strings.Contains(plan, "id>? AND id<?") || strings.Contains(plan, "SCAN") {
		t.Fatalf("the lookup's plan:\n%s", plan)
	}
}
