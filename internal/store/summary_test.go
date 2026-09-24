package store

import (
	"testing"
	"time"

	"exehub/internal/envelope"
)

// TestSummaries: a thread owes a summary at every step of the ladder its
// tree has reached and has none for, lowest step first, the threads
// touched last first; a post without a language, or with no words, owes
// none; a kept one is read back newest step first with its cites; a
// failed try waits its hour and stops at its tries; a reply's delete
// takes the summary that cites it and no other, a root's takes them all,
// Rebuild keeps the rest; DropSummaries forgets the newest step; and
// the hook names the thread a reply or a delete touches.
func TestSummaries(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	var roots []string
	s.OnMessage = func(e *envelope.Envelope, op any, id, root string) { roots = append(roots, root) }
	big := ingest(t, s, a, "post.create", map[string]any{"text": "a big thread"})
	small := ingest(t, s, a, "post.create", map[string]any{"text": "a small one"})
	pic := ingest(t, s, a, "post.create", map[string]any{"text": "https://a.example/x.png"})
	if roots[0] != "" {
		t.Errorf("a root's own create names a root: %q", roots[0])
	}
	var replies []string
	for i := 0; i < 24; i++ {
		to := big
		if i%3 == 2 {
			to = replies[i-1] // every third a reply to the one before: the tree, not the first level
		}
		replies = append(replies, ingest(t, s, a, "post.create", map[string]any{"text": "reply", "reply_to": to}))
	}
	ingest(t, s, a, "post.create", map[string]any{"text": "small reply", "reply_to": small})
	for i := 0; i < 12; i++ {
		ingest(t, s, a, "post.create", map[string]any{"text": "on the picture", "reply_to": pic})
	}
	if roots[len(roots)-1] != pic || roots[3] != big || roots[5] != big {
		t.Errorf("the hook's roots: %q", roots[3:6])
	}
	steps := []int{10, 20, 50}
	if owed, err := s.PostsToSummarize(steps, 3, 0, 10); err != nil || len(owed) != 0 {
		t.Fatalf("owed before any language = %+v, %v", owed, err)
	}
	for id, tag := range map[string]string{big: "en", small: "en", pic: "zxx"} {
		if err := s.SetLang(id, tag, "m", true); err != nil {
			t.Fatal(err)
		}
	}
	owed, err := s.PostsToSummarize(steps, 3, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(owed) != 2 || owed[0].ID != big || owed[0].Step != 10 || owed[0].Replies != 24 || owed[0].Lang != "en" || owed[0].Author != a.id() || owed[1].Step != 20 {
		t.Fatalf("owed = %+v", owed)
	}
	// the 10-reply one lands with a cite; the 20 one fails its first try
	if _, err := s.SetSummary(big, 10, "en", "**Ten.**\n- one [#3]", "m", 10, map[int]string{3: replies[2]}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetSummary(big, 20, "en", "garbage", "m", 20, nil, false); err != nil {
		t.Fatal(err)
	}
	if owed, _ = s.PostsToSummarize(steps, 3, 0, 10); len(owed) != 0 {
		t.Fatalf("owed with a fresh failure = %+v", owed)
	}
	soon := time.Now().Add(time.Hour).UnixMilli()
	if owed, _ = s.PostsToSummarize(steps, 3, soon, 10); len(owed) != 1 || owed[0].Step != 20 {
		t.Fatalf("owed an hour on = %+v", owed)
	}
	s.SetSummary(big, 20, "en", "", "m", 20, nil, false)
	s.SetSummary(big, 20, "en", "", "m", 20, nil, false)
	if owed, _ = s.PostsToSummarize(steps, 3, soon, 10); len(owed) != 0 {
		t.Fatalf("owed after three misses = %+v", owed)
	}
	if _, err := s.SetSummary(big, 20, "en", "**Twenty.**", "m", 20, nil, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.Summaries(big)
	if err != nil || len(got) != 2 || got[0].Step != 20 || got[0].Text != "**Twenty.**" || got[0].Replies != 20 || got[0].Cites != nil ||
		got[1].Step != 10 || got[1].Cites[3] != replies[2] || got[1].Lang != "en" || got[1].Src != "" || got[1].Model != "m" {
		t.Fatalf("Summaries = %+v, %v", got, err)
	}
	// a reply the 10-reply summary does not cite goes: nothing changes
	ingest(t, s, a, "post.delete", map[string]any{"post": replies[10]})
	if roots[len(roots)-1] != big {
		t.Errorf("a delete's root: %q", roots[len(roots)-1])
	}
	if got, _ = s.Summaries(big); len(got) != 2 {
		t.Fatalf("a delete elsewhere took a summary: %+v", got)
	}
	// the cited one goes: the 10 summary is owed again, the 20 stays
	ingest(t, s, a, "post.delete", map[string]any{"post": replies[2]})
	if got, _ = s.Summaries(big); len(got) != 1 || got[0].Step != 20 {
		t.Fatalf("after the cited reply went: %+v", got)
	}
	if owed, _ = s.PostsToSummarize(steps, 3, 0, 10); len(owed) != 1 || owed[0].Step != 10 || owed[0].Replies != 21 {
		t.Fatalf("owed after the cited reply went = %+v", owed)
	}
	// the in-flight hole (Codex's catch): a summary the model wrote while a
	// cited reply left the thread — deleted, or under a deleted parent —
	// is refused in the transaction that would keep it, no row, no try
	// spent; so is one whose root went; a translation of one that is gone
	// is refused too
	if wrote, err := s.SetSummary(big, 10, "en", "**Stale.**\n- cites the deleted [#3]", "m", 10, map[int]string{3: replies[2]}, true); err != nil || wrote {
		t.Errorf("a summary citing a deleted reply: wrote %v, %v", wrote, err)
	}
	if wrote, _ := s.SetSummary(big, 10, "en", "**Stale.**\n- cites the orphan [#4]", "m", 10, map[int]string{4: replies[11]}, true); wrote {
		t.Error("a summary citing a reply under a deleted parent was kept")
	}
	if got, _ = s.Summaries(big); len(got) != 1 || got[0].Step != 20 {
		t.Fatalf("after the refused rows: %+v", got)
	}
	var tries int
	s.db.QueryRow(`SELECT IFNULL(MAX(tries), 0) FROM summaries WHERE post = ? AND step = 10`, big).Scan(&tries)
	if tries != 0 {
		t.Errorf("a refused row spent a try: %d", tries)
	}
	if wrote, _ := s.SetSummaryTranslation(big, 10, "zh-Hans", "en", "**十。**", "m", 10, nil, true); wrote {
		t.Error("a translation of a summary that is gone was kept")
	}
	if wrote, err := s.SetSummaryTranslation(big, 20, "zh-Hans", "en", "**二十。**", "m", 20, nil, true); err != nil || !wrote {
		t.Errorf("a translation of a kept summary: wrote %v, %v", wrote, err)
	}
	if got, _ = s.Summaries(big); len(got) != 2 || got[1].Lang != "zh-Hans" || got[1].Src != "en" {
		t.Fatalf("with the translation: %+v", got)
	}
	if n, err := s.DropSummaries(big, 0); err != nil || n != 2 {
		t.Fatalf("DropSummaries = %d, %v", n, err)
	}
	if owed, _ = s.PostsToSummarize(steps, 3, 0, 10); len(owed) != 2 {
		t.Fatalf("owed after the drop = %+v", owed)
	}
	s.SetSummary(big, 10, "en", "**Ten again.**", "m", 10, nil, true)
	s.SetSummary(small, 10, "en", "orphan-to-be", "m", 10, nil, true)
	if err := s.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Summaries(big); len(got) != 1 || got[0].Text != "**Ten again.**" {
		t.Fatalf("after Rebuild: %+v", got)
	}
	ingest(t, s, a, "post.delete", map[string]any{"post": big})
	if got, _ = s.Summaries(big); len(got) != 0 {
		t.Fatalf("the root went and its summaries stayed: %+v", got)
	}
	if wrote, _ := s.SetSummary(big, 10, "en", "**Late.**", "m", 10, nil, true); wrote {
		t.Error("a summary of a deleted root was kept")
	}
	if r, err := s.Root(replies[0]); err != nil || r != "" {
		t.Errorf("Root of an orphaned reply = %q, %v", r, err)
	}
	if r, _ := s.Root(small); r != small {
		t.Errorf("Root of a root = %q", r)
	}
}
