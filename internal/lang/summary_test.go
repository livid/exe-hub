package lang

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"exehub/internal/store"
)

func replies(n int) []store.FeedPost {
	out := make([]store.FeedPost, n)
	for i := range out {
		out[i] = store.FeedPost{ID: fmt.Sprintf("r%02d", i+1), Author: "a", AuthorName: "Ann", Text: fmt.Sprintf("reply %d", i+1)}
		if i > 0 && i%3 == 0 {
			out[i].ReplyTo = out[i-1].ID
		}
	}
	return out
}

// TestCheckSummary: the shape asked for and nothing the thread did not
// give — the bold point first, at most five bullets and nothing else,
// no heading or code, a length, the script, every cite one of the
// replies read (returned by id), no foreign link.
func TestCheckSummary(t *testing.T) {
	root := store.FeedPost{ID: "root", Text: "The post, with https://a.example/one in it."}
	rs := replies(12)
	rs[4].Text = "see https://b.example/two"
	good := "**The thread settled on paging first.**\n\n- Paging was asked for and agreed [#2]\n- The summary reads the first N replies [#5]\n- Open: the window's width"
	cites, err := CheckSummary(good, "en", root, rs)
	if err != nil || len(cites) != 2 || cites[2] != "r02" || cites[5] != "r05" {
		t.Fatalf("good: %v, %v", cites, err)
	}
	if c, err := CheckSummary("**Alone.**", "en", root, rs); err != nil || c != nil {
		t.Errorf("a bold line alone: %v, %v", c, err)
	}
	if _, err := CheckSummary("**Links kept.**\n- see https://b.example/two and https://a.example/one", "en", root, rs); err != nil {
		t.Errorf("the thread's own links: %v", err)
	}
	bad := map[string]string{
		"empty":          "",
		"no bold first":  "The thread settled.\n- one",
		"a heading":      "**Point.**\n## Points\n- one",
		"prose after":    "**Point.**\n- one\nAnd a closing line.",
		"six bullets":    "**Point.**\n- 1\n- 2\n- 3\n- 4\n- 5\n- 6",
		"a code block":   "**Point.**\n```\nx\n```",
		"too long":       "**Point.**\n- " + strings.Repeat("word ", 400),
		"a cite too far": "**Point.**\n- one [#13]",
		"cite zero":      "**Point.**\n- one [#0]",
		"a foreign link": "**Point.**\n- one https://c.example/",
		"wrong script":   "**要点。**\n- " + strings.Repeat("中文", 30),
	}
	for name, s := range bad {
		if _, err := CheckSummary(s, "en", root, rs); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	if _, err := CheckSummary("**要点。**\n- 分页先做 [#1]", "zh-Hans", root, rs); err != nil {
		t.Errorf("Chinese: %v", err)
	}
	if _, err := CheckSummary("**要点。**\n- "+strings.Repeat("字", 700), "zh-Hans", root, rs); err == nil {
		t.Error("a Chinese summary over its cap passed")
	}
}

type fakeSummaries struct {
	owed  []store.OwedSummary
	posts map[string]*store.FeedPost
	trees map[string][]store.FeedPost
	rows  map[string]string // "post step" -> text, "" for a failed try
	cites map[string]map[int]string
}

func (f *fakeSummaries) PostsToSummarize(steps []int, maxTries int, before int64, limit int) ([]store.OwedSummary, error) {
	var out []store.OwedSummary
	for _, o := range f.owed {
		if _, done := f.rows[o.ID+" "+fmt.Sprint(o.Step)]; !done {
			out = append(out, o)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
func (f *fakeSummaries) Post(id string) (*store.FeedPost, error) {
	if p := f.posts[id]; p != nil {
		return p, nil
	}
	return nil, store.ErrNotFound
}
func (f *fakeSummaries) Thread(id string, limit int) ([]store.FeedPost, error) {
	t := f.trees[id]
	if len(t) > limit {
		t = t[:limit]
	}
	return t, nil
}
func (f *fakeSummaries) SetSummary(post string, step int, lang, text, model string, replies int, cites map[int]string, ok bool) error {
	f.rows[post+" "+fmt.Sprint(step)] = text
	f.cites[post+" "+fmt.Sprint(step)] = cites
	return nil
}

// TestSummarizerPass: the thread goes to the model numbered in order,
// under a prompt in the post's language; a kept answer is written with
// its cites and the replies read; an answer that fails the check is a
// spent try; a thread that lost replies below its step is stepped over;
// a refused call records nothing.
func TestSummarizerPass(t *testing.T) {
	var seen []string
	f := &fakeOllama{levels: true, answer: func(p string) (int, string) {
		seen = append(seen, p)
		switch {
		case strings.Contains(p, "The post, by Ann:\nrefuse me"):
			return 500, "upstream"
		case strings.Contains(p, "The post, by Ann:\nshort answer"):
			return 200, "not the shape"
		}
		return 200, "**Where it stands.**\n\n- One point [#2]\n- Open: the rest [#10]\n"
	}}
	st := &fakeSummaries{rows: map[string]string{}, cites: map[string]map[int]string{},
		posts: map[string]*store.FeedPost{
			"ok":    {ID: "ok", AuthorName: "Ann", Text: "the post"},
			"bad":   {ID: "bad", AuthorName: "Ann", Text: "short answer"},
			"r":     {ID: "r", AuthorName: "Ann", Text: "refuse me"},
			"thin":  {ID: "thin", AuthorName: "Ann", Text: "lost replies"},
			"twice": {ID: "twice", AuthorName: "Ann", Text: "the other post"},
		},
		trees: map[string][]store.FeedPost{"ok": replies(12), "bad": replies(10), "r": replies(10), "thin": replies(7), "twice": replies(25)},
		owed: []store.OwedSummary{
			{ID: "r", Lang: "en", Step: 10, Replies: 10}, {ID: "ok", Lang: "en", Step: 10, Replies: 12},
			{ID: "bad", Lang: "en", Step: 10, Replies: 10}, {ID: "thin", Lang: "en", Step: 10, Replies: 10},
			{ID: "twice", Lang: "en", Step: 10, Replies: 25}, {ID: "twice", Lang: "en", Step: 20, Replies: 25},
		}}
	z := NewSummarizer(st, newFake(t, f), nil)
	if !z.pass() {
		t.Fatal("pass = false with one refused thread, want true")
	}
	if len(st.rows) != 4 || st.rows["ok 10"] == "" || st.rows["bad 10"] != "" || st.rows["twice 10"] == "" || st.rows["twice 20"] == "" {
		t.Fatalf("rows = %+v", st.rows)
	}
	if _, there := st.rows["r 10"]; there {
		t.Error("a refused call wrote a row")
	}
	if _, there := st.rows["thin 10"]; there {
		t.Error("a thread below its step wrote a row")
	}
	if c := st.cites["ok 10"]; c[2] != "r02" || c[10] != "r10" || len(c) != 2 {
		t.Errorf("cites = %v", c)
	}
	// what the model read: the post, then the replies numbered in order,
	// a nested one under the reply it answers, cut at the step
	var ok string
	for _, p := range seen {
		if strings.HasPrefix(p, "The post, by Ann:\nthe post\n\nReply #1, by Ann, to the post:\nreply 1\n") {
			ok = p
		}
	}
	if ok == "" || !strings.Contains(ok, "\nReply #4, by Ann, to reply #3:\nreply 4\n") || !strings.Contains(ok, "Reply #10, by Ann, to reply #9:") || strings.Contains(ok, "Reply #11") {
		t.Errorf("the thread as read:\n%s", ok)
	}
	// the 20 step of the same thread read twenty
	for _, p := range seen {
		if strings.Contains(p, "Reply #20,") && strings.Contains(p, "Reply #21,") {
			t.Error("the 20 step read past twenty")
		}
	}
	// Summarize alone: the shape check is ErrAnswer, a refusal is not
	d := newFake(t, f)
	if _, _, err := d.Summarize(context.Background(), *st.posts["bad"], replies(10), "en"); !errors.Is(err, ErrAnswer) {
		t.Errorf("a wrong shape: %v", err)
	}
	if _, _, err := d.Summarize(context.Background(), *st.posts["r"], replies(10), "en"); err == nil || errors.Is(err, ErrAnswer) {
		t.Errorf("a refusal: %v", err)
	}
}
