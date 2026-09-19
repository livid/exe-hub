package store

import (
	"testing"
	"time"
)

// TestTranslations: a post is owed every target it is not written in,
// once it has a language with words in it; what is kept is read back by
// post and language with the language it came from; a failed try waits
// its hour and stops at its tries; a read carries the post's own lang;
// delete takes the rows and Rebuild keeps those of surviving posts.
func TestTranslations(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	en := ingest(t, s, a, "post.create", map[string]any{"text": "hello there"})
	zh := ingest(t, s, a, "post.create", map[string]any{"text": "今天天气很好"})
	ja := ingest(t, s, a, "post.create", map[string]any{"text": "こんにちは"})
	link := ingest(t, s, a, "post.create", map[string]any{"text": "https://a.example/"})
	ingest(t, s, a, "post.create", map[string]any{"text": "not named yet"})
	targets := []string{"zh-Hans", "en"}
	soon := time.Now().Add(time.Hour).UnixMilli()

	if owed, err := s.PostsToTranslate(targets, 3, 0, 10); err != nil || len(owed) != 0 {
		t.Fatalf("owed before any language = %+v, %v", owed, err)
	}
	for id, tag := range map[string]string{en: "en", zh: "zh-Hans", ja: "ja", link: "zxx"} {
		if err := s.SetLang(id, tag, "m", true); err != nil {
			t.Fatal(err)
		}
	}
	owed, err := s.PostsToTranslate(targets, 3, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range owed {
		got = append(got, o.From+">"+o.To)
		if o.Author != a.id() || o.Text == "" {
			t.Fatalf("owed %+v", o)
		}
	}
	// newest post first, a post's targets in order; nothing for the link
	if want := "ja>en ja>zh-Hans zh-Hans>en en>zh-Hans"; joined(got) != want {
		t.Fatalf("owed = %q, want %q", joined(got), want)
	}

	if err := s.SetTranslation(en, "zh-Hans", "你好", "m", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTranslation(ja, "zh-Hans", "你好", "m", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTranslation(ja, "en", "whatever came back", "m", false); err != nil {
		t.Fatal(err)
	}
	trs, err := s.Translations([]string{en, zh, ja, "gone"}, "zh-Hans")
	if err != nil || len(trs) != 2 || trs[en] != (Translation{"你好", "en"}) || trs[ja] != (Translation{"你好", "ja"}) {
		t.Fatalf("Translations = %+v, %v", trs, err)
	}
	if trs, _ = s.Translations([]string{ja}, "en"); len(trs) != 0 {
		t.Fatalf("a failed try read back as a translation: %+v", trs)
	}
	if trs, _ = s.Translations(nil, "en"); len(trs) != 0 {
		t.Fatal("no ids")
	}

	if owed, _ = s.PostsToTranslate(targets, 3, 0, 10); len(owed) != 1 || owed[0].ID != zh {
		t.Fatalf("owed after three answers = %+v, want the Chinese post's English", owed)
	}
	if owed, _ = s.PostsToTranslate(targets, 3, soon, 10); len(owed) != 2 {
		t.Fatalf("owed an hour on = %+v, want the failed try back", owed)
	}
	s.SetTranslation(ja, "en", "", "m", false)
	s.SetTranslation(ja, "en", "", "m", false)
	if owed, _ = s.PostsToTranslate(targets, 3, soon, 10); len(owed) != 1 {
		t.Fatalf("owed after three tries = %+v", owed)
	}

	p, err := s.Post(ja)
	if err != nil || p.Lang != "ja" {
		t.Fatalf("Post.Lang = %q, %v", p.Lang, err)
	}
	thread, _ := s.Thread(ja, 10)
	feed, err := s.Feed("", 10, false)
	if err != nil || len(feed) != 5 || feed[0].Lang != "" || feed[1].Lang != "zxx" || len(thread) != 0 {
		t.Fatalf("Feed langs = %+v, %v", feed, err)
	}

	if err := s.SetTranslation("gone", "en", "x", "m", true); err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM translations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 3 {
		t.Fatalf("%d rows, want 3: a missing post got one", n)
	}
	ingest(t, s, a, "post.delete", map[string]any{"post": ja})
	if n := count(); n != 1 {
		t.Fatalf("%d rows after deleting the post with two, want 1", n)
	}
	if _, err := s.db.Exec(`INSERT INTO translations (post, lang, text, status, ts) VALUES ('orphan', 'en', 'x', 'ok', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if trs, _ = s.Translations([]string{en, "orphan"}, "zh-Hans"); count() != 1 || len(trs) != 1 {
		t.Fatalf("after Rebuild: %d rows, %+v", count(), trs)
	}
}

func joined(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += " "
		}
		out += v
	}
	return out
}
