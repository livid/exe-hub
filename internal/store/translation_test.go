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
	// the rewrite travels: this hub's own row is served again under a
	// new rev with a new time, so a peer that took it takes it again
	page, next, err := s.TranslationsPage(0, 10)
	if err != nil || len(page) != 1 || page[0].Text != "你好，世界" || next < 2 {
		t.Fatalf("after rewrite TranslationsPage = %+v, %d, %v", page, next, err)
	}
	before := page[0].TS
	time.Sleep(2 * time.Millisecond)
	if err := s.RewriteTranslation(ja, "zh-Hans", "你好，世界。"); err != nil {
		t.Fatal(err)
	}
	if again, next2, _ := s.TranslationsPage(next, 10); len(again) != 1 || again[0].Text != "你好，世界。" || again[0].TS <= before || next2 <= next {
		t.Fatalf("a rewrite not served again: %+v, %d after %d", again, next2, next)
	}
	// a peer's row rewritten here keeps its rev of 0 and its time: it is
	// its maker's to serve again
	// (the store takes any target; the post's own language is one the
	// puller would refuse, and one this post is never owed, so the rest
	// of the test stands)
	if ok, err := s.AcceptTranslation("peer", SharedTranslation{Post: ja, Lang: "ja", Text: "こんにちは:皆さん", Model: "m", TS: 5}); !ok || err != nil {
		t.Fatalf("AcceptTranslation = %v, %v", ok, err)
	}
	if err := s.RewriteTranslation(ja, "ja", "こんにちは：皆さん"); err != nil {
		t.Fatal(err)
	}
	var rev, ts int64
	var origin string
	if err := s.db.QueryRow(`SELECT text, origin, rev, ts FROM translations WHERE post=? AND lang='ja'`, ja).Scan(&text, &origin, &rev, &ts); err != nil ||
		text != "こんにちは：皆さん" || origin != "peer" || rev != 0 || ts != 5 {
		t.Fatalf("a peer's row after rewrite: %q %q rev %d ts %d, %v", text, origin, rev, ts, err)
	}
	if page, _, _ := s.TranslationsPage(0, 10); len(page) != 1 {
		t.Fatalf("a peer's row served on: %+v", page)
	}
	s.db.Exec(`DELETE FROM translations WHERE post=? AND lang='ja'`, ja)
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

// TestPendingTranslations: a peer's translation of a post not held is
// set aside, the newest per peer, post and language standing; it is
// listed for its post from every peer, newest first, and with no post
// named only once the post is held; a peer is kept to its cap, the
// longest-waiting going first, and what waited too long goes unasked.
func TestPendingTranslations(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	held := ingest(t, s, a, "post.create", map[string]any{"text": "held"})
	set := func(peer, post, text string, ts int64, max int) int64 {
		n, err := s.SetPendingTranslation(peer, SharedTranslation{Post: post, Lang: "zh-Hans", Text: text, Model: "m", TS: ts}, max)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	set("one", "absent", "旧", 100, 10)
	set("one", "absent", "新", 200, 10)
	set("one", "absent", "更旧", 50, 10) // older than what waits: it stands
	set("two", "absent", "别家的", 150, 10)
	set("one", held, "已有的帖子", 100, 10)

	got, err := s.PendingTranslations("absent", 10)
	if err != nil || len(got) != 2 || got[0].Text != "新" || got[0].Peer != "one" || got[1].Peer != "two" || got[1].Model != "m" {
		t.Fatalf("for the post = %+v, %v; want one's newest, then two's", got, err)
	}
	if got, _ = s.PendingTranslations("", 10); len(got) != 1 || got[0].Post != held {
		t.Fatalf("for posts held by now = %+v, want only the held post's", got)
	}
	if err := s.DropPendingTranslation("one", held, "zh-Hans"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.PendingTranslations("", 10); len(got) != 0 {
		t.Fatalf("after the drop = %+v", got)
	}

	// the cap is each peer's own: three more from "one" at a cap of 3 push out its longest-waiting
	time.Sleep(2 * time.Millisecond)
	for i, post := range []string{"p1", "p2", "p3"} {
		if dropped := set("one", post, "字", 300, 3); (i == 2) != (dropped == 1) {
			t.Fatalf("after %s: dropped %d", post, dropped)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got, _ = s.PendingTranslations("absent", 10); len(got) != 1 || got[0].Peer != "two" {
		t.Fatalf("after the cap = %+v, want one's longest-waiting gone and two's untouched", got)
	}

	if n, err := s.AgePendingTranslations(time.Now().Add(time.Hour).UnixMilli()); err != nil || n != 4 {
		t.Fatalf("aged %d, %v; want all four", n, err)
	}
	if err := s.Rebuild(); err != nil { // not derived: Rebuild leaves the table alone
		t.Fatal(err)
	}
}
