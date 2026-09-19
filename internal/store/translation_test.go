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

// TestTranslationRewriteAndDrop: a rule run over kept translations
// reads them with their posts and rewrites the words alone; dropping a
// post's translations, one language or all, makes them owed again with
// their tries forgotten.
func TestTranslationRewriteAndDrop(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	ja := ingest(t, s, a, "post.create", map[string]any{"text": "こんにちは"})
	s.SetLang(ja, "ja", "m", true)
	s.SetTranslation(ja, "zh-Hans", "你好,世界", "m", true)
	s.SetTranslation(ja, "en", "", "m", false)
	s.SetTranslation(ja, "en", "", "m", false)
	s.SetTranslation(ja, "en", "", "m", false) // out of tries

	kept, err := s.KeptTranslations("zh-Hans")
	if err != nil || len(kept) != 1 || kept[0] != (KeptTranslation{ja, "こんにちは", "你好,世界"}) {
		t.Fatalf("KeptTranslations = %+v, %v", kept, err)
	}
	if kept, _ = s.KeptTranslations("en"); len(kept) != 0 {
		t.Fatalf("a failed try listed as kept: %+v", kept)
	}
	if err := s.RewriteTranslation(ja, "zh-Hans", "你好，世界"); err != nil {
		t.Fatal(err)
	}
	s.RewriteTranslation(ja, "en", "never kept") // no ok row: nothing to rewrite
	var text, model string
	var tries int
	if err := s.db.QueryRow(`SELECT text, model, tries FROM translations WHERE post=? AND lang='zh-Hans'`, ja).Scan(&text, &model, &tries); err != nil ||
		text != "你好，世界" || model != "m" || tries != 1 {
		t.Fatalf("after rewrite: %q %q %d, %v", text, model, tries, err)
	}
	if trs, _ := s.Translations([]string{ja}, "en"); len(trs) != 0 {
		t.Fatal("a rewrite made a translation of a failed try")
	}

	targets, soon := []string{"zh-Hans", "en"}, time.Now().Add(time.Hour).UnixMilli()
	if owed, _ := s.PostsToTranslate(targets, 3, soon, 10); len(owed) != 0 {
		t.Fatalf("owed before any drop: %+v", owed)
	}
	if n, err := s.DropTranslations(ja, "en"); err != nil || n != 1 {
		t.Fatalf("drop en = %d, %v", n, err)
	}
	if owed, _ := s.PostsToTranslate(targets, 3, 0, 10); len(owed) != 1 || owed[0].To != "en" {
		t.Fatalf("owed after dropping en = %+v, want it owed at once, tries forgotten", owed)
	}
	if n, err := s.DropTranslations(ja, ""); err != nil || n != 1 {
		t.Fatalf("drop all = %d, %v", n, err)
	}
	if owed, _ := s.PostsToTranslate(targets, 3, 0, 10); len(owed) != 2 {
		t.Fatalf("owed after dropping all = %+v", owed)
	}
	if n, _ := s.DropTranslations("nothing", ""); n != 0 {
		t.Fatal("dropped rows of a post that has none")
	}

	// an editor's note rides with what is owed, the latest one standing,
	// survives Rebuild, and goes with its post
	s.SetTranslationNote(ja, "first")
	if err := s.SetTranslationNote(ja, "it is a greeting"); err != nil {
		t.Fatal(err)
	}
	s.SetTranslationNote("gone", "no such post")
	owed, _ := s.PostsToTranslate(targets, 3, 0, 10)
	if len(owed) != 2 || owed[0].Note != "it is a greeting" || owed[1].Note != "it is a greeting" {
		t.Fatalf("owed with a note = %+v", owed)
	}
	notes := func() (n int) {
		s.db.QueryRow(`SELECT COUNT(*) FROM translation_notes`).Scan(&n)
		return n
	}
	if err := s.Rebuild(); err != nil || notes() != 1 {
		t.Fatalf("after Rebuild: %d notes, %v", notes(), err)
	}
	ingest(t, s, a, "post.delete", map[string]any{"post": ja})
	if notes() != 0 {
		t.Fatal("a deleted post's note stayed")
	}
}

// TestTranslationsShared: a hub serves the translations it made, in the
// order it kept them, by a rev that is never given twice — a redone one
// comes up again past a cursor that had read it — and never a failed try
// or one it took from a peer. Of a peer's it keeps the newest: over
// nothing, over a failed try, over an older one whoever made that, never
// over a newer; what it has from anyone it does not owe.
func TestTranslationsShared(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	one := ingest(t, s, a, "post.create", map[string]any{"text": "one"})
	two := ingest(t, s, a, "post.create", map[string]any{"text": "two"})
	three := ingest(t, s, a, "post.create", map[string]any{"text": "three"})
	for _, id := range []string{one, two, three} {
		s.SetLang(id, "en", "m", true)
	}
	s.SetTranslation(one, "zh-Hans", "一", "m", true)
	s.SetTranslation(two, "zh-Hans", "", "m", false)
	s.SetTranslation(two, "zh-Hans", "二", "m", true)

	page, next, err := s.TranslationsPage(0, 10)
	if err != nil || len(page) != 2 || page[0].Post != one || page[1].Text != "二" || page[1].Model != "m" || page[1].TS == 0 {
		t.Fatalf("page = %+v, next %d, %v", page, next, err)
	}
	if again, n, _ := s.TranslationsPage(next, 10); len(again) != 0 || n != next {
		t.Fatalf("past the end: %+v, next %d, want nothing and the same cursor %d", again, n, next)
	}
	if first, n, _ := s.TranslationsPage(0, 1); len(first) != 1 || first[0].Post != one || n >= next {
		t.Fatalf("a page of one: %+v, next %d", first, n)
	}

	// the newest, redone: it comes up again, past the cursor that had read it
	if n, _ := s.DropTranslations(two, ""); n != 1 {
		t.Fatal("drop")
	}
	s.SetTranslation(two, "zh-Hans", "二，重译", "m", true)
	redone, next2, _ := s.TranslationsPage(next, 10)
	if len(redone) != 1 || redone[0].Text != "二，重译" || next2 <= next {
		t.Fatalf("after a redo: %+v, next %d after %d — the rev was given twice", redone, next2, next)
	}

	// a peer's translations
	old, now := time.Now().Add(-time.Hour).UnixMilli(), time.Now().Add(time.Hour).UnixMilli()
	take := func(post, text string, ts int64) bool {
		ok, err := s.AcceptTranslation("peerhub", SharedTranslation{Post: post, Lang: "zh-Hans", Text: text, Model: "theirs", TS: ts})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !take(three, "三", old) {
		t.Fatal("none kept yet: not taken")
	}
	if take(three, "三（同一条）", old) || take(one, "一（旧）", old) {
		t.Fatal("took one no newer than what is kept")
	}
	if !take(one, "一（新）", now) {
		t.Fatal("a newer one not taken over this hub's own")
	}
	if take("gone", "x", now) {
		t.Fatal("took a translation of a post this hub does not hold")
	}
	s.SetTranslation(three, "en", "", "m", false) // a failed try is no translation: a peer's replaces it
	if ok, _ := s.AcceptTranslation("peerhub", SharedTranslation{Post: three, Lang: "en", Text: "three", Model: "theirs", TS: old}); !ok {
		t.Fatal("a peer's not taken over a failed try")
	}
	trs, _ := s.Translations([]string{one, two, three}, "zh-Hans")
	if trs[one].Text != "一（新）" || trs[two].Text != "二，重译" || trs[three].Text != "三" {
		t.Fatalf("kept = %+v", trs)
	}
	// taken ones are not served on, and nothing a hub has is owed
	served, _, _ := s.TranslationsPage(0, 10)
	if len(served) != 1 || served[0].Post != two {
		t.Fatalf("served after taking = %+v, want only this hub's own", served)
	}
	if owed, _ := s.PostsToTranslate([]string{"zh-Hans"}, 3, 0, 10); len(owed) != 0 {
		t.Fatalf("owed = %+v, want nothing: every post has one from someone", owed)
	}

	if text, lang, ok, err := s.PostText(one); err != nil || !ok || text != "one" || lang != "en" {
		t.Fatalf("PostText = %q %q %v %v", text, lang, ok, err)
	}
	if _, _, ok, _ := s.PostText("gone"); ok {
		t.Fatal("PostText of a post not held")
	}
}

// TestTranslationsNumbered: translations kept before they had revs get
// them at the next start, in the order they were made.
func TestTranslationsNumbered(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	x := ingest(t, s, a, "post.create", map[string]any{"text": "x"})
	y := ingest(t, s, a, "post.create", map[string]any{"text": "y"})
	for i, id := range []string{y, x} { // y was made first
		if _, err := s.db.Exec(`INSERT INTO translations (post, lang, text, status, ts) VALUES (?, 'zh-Hans', '字', 'ok', ?)`, id, 100+i); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.numberTranslations(); err != nil {
		t.Fatal(err)
	}
	page, _, _ := s.TranslationsPage(0, 10)
	if len(page) != 2 || page[0].Post != y || page[1].Post != x {
		t.Fatalf("numbered = %+v, want y then x", page)
	}
	s.numberTranslations() // again: nothing to do, the numbers stand
	if again, _, _ := s.TranslationsPage(0, 10); len(again) != 2 || again[0].Post != y {
		t.Fatalf("numbered twice = %+v", again)
	}
}
