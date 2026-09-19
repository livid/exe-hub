package store

import (
	"testing"
	"time"
)

// TestLangs: a post's language is kept beside it and read back; the
// worklist is every post never asked about plus the failed ones with
// tries left whose last try is old enough, newest first; a post that is
// gone gets no row, deleting a post takes its row, and Rebuild keeps the
// rows of the posts that survive (langs are not in the log).
func TestLangs(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	en := ingest(t, s, a, "post.create", map[string]any{"text": "hello there"})
	zh := ingest(t, s, a, "post.create", map[string]any{"text": "今天天气很好"})
	odd := ingest(t, s, a, "post.create", map[string]any{"text": "mumble"})
	soon := time.Now().Add(time.Hour).UnixMilli()

	left, err := s.PostsWithoutLang(3, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 3 || left[2].ID != en || left[2].Text != "hello there" || left[2].Author != a.id() {
		t.Fatalf("PostsWithoutLang = %+v, want all three, newest first", left)
	}
	if lang, err := s.PostLang(en); err != nil || lang != "" {
		t.Fatalf("PostLang before = %q, %v", lang, err)
	}

	if err := s.SetLang(en, "en", "glm-5.3:cloud", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLang(zh, "zh-Hans", "glm-5.3:cloud", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLang(odd, "whatever", "glm-5.3:cloud", false); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{en: "en", zh: "zh-Hans", odd: ""} {
		if lang, err := s.PostLang(id); err != nil || lang != want {
			t.Fatalf("PostLang = %q, %v; want %q", lang, err, want)
		}
	}

	// a failed post waits out its hour, comes back, and stops at its tries
	if left, _ = s.PostsWithoutLang(3, 0, 10); len(left) != 0 {
		t.Fatalf("a failed try is listed again at once: %+v", left)
	}
	if left, _ = s.PostsWithoutLang(3, soon, 10); len(left) != 1 || left[0].ID != odd {
		t.Fatalf("PostsWithoutLang an hour on = %+v, want the failed post", left)
	}
	s.SetLang(odd, "", "glm-5.3:cloud", false)
	s.SetLang(odd, "", "glm-5.3:cloud", false)
	if left, _ = s.PostsWithoutLang(3, soon, 10); len(left) != 0 {
		t.Fatalf("a post out of tries is still listed: %+v", left)
	}
	var tries int
	if err := s.db.QueryRow(`SELECT tries FROM langs WHERE post=?`, odd).Scan(&tries); err != nil || tries != 3 {
		t.Fatalf("tries = %d (%v), want 3", tries, err)
	}

	// no row for a post that is not there
	if err := s.SetLang("gone", "en", "glm-5.3:cloud", true); err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM langs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 3 {
		t.Fatalf("%d rows, want 3: a missing post got one", n)
	}

	ingest(t, s, a, "post.delete", map[string]any{"post": zh})
	if n := count(); n != 2 {
		t.Fatalf("%d rows after a delete, want 2", n)
	}
	if _, err := s.db.Exec(`INSERT INTO langs (post, lang, status, ts) VALUES ('orphan', 'en', 'ok', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("%d rows after Rebuild, want 2: the orphan dropped, the rest kept", n)
	}
	if lang, _ := s.PostLang(en); lang != "en" {
		t.Fatalf("PostLang after Rebuild = %q, want en", lang)
	}
}
