package api

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"exehub/internal/config"
	"exehub/internal/store"
)

// TestBuildArchives: every month from the hub's first to this one stands
// whole, newest first; an older month stands only for the posts it has;
// a day's colour is the quarter its count falls in; days still to come
// and days before the hub's first carry no count; a post dated after
// today is today's.
func TestBuildArchives(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	a := buildArchives([]store.DayPosts{
		{Day: "1970-01-01", Posts: 1}, // a claim older than the hub
		{Day: "2026-08-30", Posts: 2},
		{Day: "2026-10-31", Posts: 8},
		{Day: "2026-11-01", Posts: 4},
		{Day: "2026-11-09", Posts: 3},
		{Day: "2026-11-10", Posts: 2}, // ahead of today
	}, "2026-08-30", now)
	var got []string
	for _, m := range a.Months {
		got = append(got, time.Date(m.Year, time.Month(m.Month), 1, 0, 0, 0, 0, time.UTC).Format("2006-01"))
	}
	if want := "2026-11 2026-10 2026-09 2026-08 1970-01"; strings.Join(got, " ") != want {
		t.Fatalf("months %v, want %s", got, want)
	}
	if a.Posts != 20 || a.Days != 5 || a.Zone != "UTC" {
		t.Errorf("%d posts on %d days in %s, want 20 on 5 in UTC", a.Posts, a.Days, a.Zone)
	}
	cell := func(date string) webArchDay {
		t.Helper()
		for _, m := range a.Months {
			for _, w := range m.Weeks {
				for i, d := range w {
					if d.Date == date {
						if wd, _ := time.Parse("2006-01-02", date); int(wd.Weekday()) != i {
							t.Errorf("%s stands in column %d, want %d", date, i, wd.Weekday())
						}
						return d
					}
				}
			}
		}
		t.Fatalf("no cell for %s", date)
		return webArchDay{}
	}
	for _, c := range []struct {
		date         string
		posts, level int
		off, today   bool
	}{
		{"2026-11-09", 5, 3, false, true},  // today, with tomorrow's two
		{"2026-11-10", 0, 0, true, false},  // still to come
		{"2026-11-01", 4, 2, false, false}, // counts 1 2 4 5 8: the quarters end at 2, 4 and 5
		{"2026-10-31", 8, 4, false, false}, //
		{"2026-08-30", 2, 1, false, false}, //
		{"2026-09-15", 0, 0, false, false}, // a quiet day of the hub's
		{"2026-08-29", 0, 0, true, false},  // before its first
		{"1970-01-01", 1, 1, false, false}, // older than the hub, but there
		{"1970-01-02", 0, 0, true, false},  // and the rest of that month is not the hub's
	} {
		d := cell(c.date)
		if d.Posts != c.posts || d.Level != c.level || d.Off != c.off || d.Today != c.today {
			t.Errorf("%s: %d posts, level %d, off %v, today %v; want %d, %d, %v, %v",
				c.date, d.Posts, d.Level, d.Off, d.Today, c.posts, c.level, c.off, c.today)
		}
	}
	if m := a.Months[0]; m.Posts != 9 || len(m.Weeks) != 5 || m.Weeks[0][0].Day != 1 || m.Weeks[4][1].Day != 30 || m.Weeks[4][2].Day != 0 {
		t.Errorf("November 2026: %d posts in %d weeks, want 9 in 5, the 1st a Sunday and the 30th a Monday", m.Posts, len(m.Weeks))
	}

	// an empty hub: this month, nothing counted
	e := buildArchives(nil, "", now)
	if len(e.Months) != 1 || e.Posts != 0 || e.Days != 0 {
		t.Errorf("an empty hub: %d months, %d posts on %d days", len(e.Months), e.Posts, e.Days)
	}
	// the first morning of a month: the month is there
	if f := buildArchives(nil, "2026-10-20", time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC)); len(f.Months) != 2 {
		t.Errorf("at 00:05 on the 1st: %d months, want October and November", len(f.Months))
	}
}

// TestWebArchives: /archives draws the calendars in the reader's
// language with the Navigation window's own row, counts a post by its
// own day in the hub's zone, and keeps the counts in memory — a post
// that lands shows once they are drawn again.
func TestWebArchives(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}, Stats: &config.Stats{Location: time.UTC}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "one"}) // its own time is 2025-08-29 UTC
	ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "two"})
	h := s.Handler()

	code, body := get(t, h, "/archives")
	if code != http.StatusOK {
		t.Fatalf("/archives answered %d", code)
	}
	today := time.Now().UTC().Format("2006-01-02")
	for _, want := range []string{
		"<title>Archives · hub.example</title>",
		`<link rel="canonical" href="http://hub.example/archives">`,
		`<span class="title">Archives</span>`,
		`<nav class="places" aria-label="Navigation" data-page="archives" data-for="" data-q="">`,
		`<a class="place" data-to="archives" href="/archives">`,
		`<span class="cap"><b>August 2025</b><span>2</span></span>`,
		`<th scope="col">S</th><th scope="col">M</th>`,
		`<td class="l1"><a href="/archives/2025-08-29" title="2025-08-29 · 2 posts">29</a></td>`,
		`<td class="off">28</td>`,
		`<td class="off">30</td>`, // the hub's first day is the day it received them: today
		`<td class="l0 now" title="` + today + ` · no posts">`,
		`<div class="statusbar"><span>2 posts on 1 day · UTC</span></div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/archives lacks %q", want)
		}
	}
	if n := len(regexp.MustCompile(`<table class="cal">`).FindAllString(body, -1)); n != 2 {
		t.Errorf("%d months, want this one and August 2025", n)
	}

	for _, c := range []struct{ q, title, month, day, status string }{
		{"?lang=zh", "归档 · hub.example", "2025 年 8 月", "2025-08-29 · 2 条帖子", "1 天里共 2 条帖子 · UTC"},
		{"?lang=ja", "アーカイブ · hub.example", "2025年8月", "2025-08-29 · 2 件の投稿", "1 日間に 2 件の投稿 · UTC"},
	} {
		_, body := get(t, h, "/archives"+c.q)
		for _, want := range []string{"<title>" + c.title + "</title>", "<b>" + c.month + "</b>", `title="` + c.day + `"`,
			"<span>" + c.status + "</span>", `href="/archives` + c.q + `"`, `href="/` + c.q + `"`, `href="/archives/2025-08-29` + c.q + `"`} {
			if !strings.Contains(body, want) {
				t.Errorf("/archives%s lacks %q", c.q, want)
			}
		}
	}

	// kept in memory: a new post waits for the next drawing
	ingest(t, s, priv, pub, 3, "post.create", map[string]any{"text": "three"})
	if _, body := get(t, h, "/archives"); !strings.Contains(body, "2025-08-29 · 2 posts") {
		t.Error("the counts were drawn again inside their time")
	}
	key := "archives:UTC:" + today
	if _, ok := s.mem.Get(key); !ok {
		t.Fatalf("nothing kept under %q", key)
	}
	s.mem.Delete(key)
	if _, body := get(t, h, "/archives"); !strings.Contains(body, "2025-08-29 · 3 posts") {
		t.Error("drawn again, the page still lacks the third post")
	}

	// the other pages' Navigation window carries the row, and /archives/ leads here
	if _, home := get(t, h, "/?lang=zh"); !strings.Contains(home, `<a class="place" data-to="archives" href="/archives?lang=zh">`) {
		t.Error("the home page's Navigation window has no Archives row")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://hub.example/archives/?lang=ja", nil))
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/archives?lang=ja" {
		t.Errorf("/archives/ answered %d to %q", w.Code, w.Header().Get("Location"))
	}
}

// TestWebArchiveDay: a day's cell leads to /archives/{day}, the day's
// posts newest first under the Archives' row, paged like the feed with
// the day in the cursor links; a day with no posts, a day still to come
// and anything that is not a date have no page.
func TestWebArchiveDay(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}, Stats: &config.Stats{Location: time.UTC}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	var ids []string // all sent on 2025-08-29 UTC, a millisecond apart
	for i := 1; i <= webPage+2; i++ {
		ids = append(ids, ingest(t, s, priv, pub, int64(i), "post.create", map[string]any{"text": fmt.Sprintf("post number %d.", i)}))
	}
	h := s.Handler()
	posts := regexp.MustCompile(`<div class="post[ "]`)

	_, cal := get(t, h, "/archives")
	if want := `<a href="/archives/2025-08-29" title="2025-08-29 · 32 posts">29</a>`; !strings.Contains(cal, want) {
		t.Fatalf("/archives lacks %q", want)
	}
	code, body := get(t, h, "/archives/2025-08-29")
	if code != http.StatusOK {
		t.Fatalf("/archives/2025-08-29 answered %d", code)
	}
	last := ids[2] // the first page ends at the 30th from the newest
	for _, want := range []string{
		"<title>August 29, 2025 · Archives · hub.example</title>",
		`<link rel="canonical" href="http://hub.example/archives/2025-08-29">`,
		`<span class="title">August 29, 2025</span>`,
		`data-page="archives"`,
		`<a class="tbox" href="/archives"`,
		`<a class="btn prev back" href="/archives">`,
		"post number 32.", "post number 3.",
		`<span class="stats">32 posts</span>`,
		`href="/archives/2025-08-29?before=` + last + `"`,
		`<div class="statusbar"><span>32 posts · UTC</span></div>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/archives/2025-08-29 lacks %q", want)
		}
	}
	if n := len(posts.FindAllString(body, -1)); n != webPage {
		t.Errorf("%d posts on the first page, want %d", n, webPage)
	}
	if strings.Contains(body, "post number 2.") || strings.Contains(body, "after=") {
		t.Error("the first page holds an older post, or a way to a newer page")
	}
	if strings.Index(body, "post number 32.") > strings.Index(body, "post number 31.") {
		t.Error("the day does not read newest first")
	}

	// the second page, in another language: the last two, and the way back
	code, body = get(t, h, "/archives/2025-08-29?lang=zh&before="+last)
	if code != http.StatusOK || len(posts.FindAllString(body, -1)) != 2 {
		t.Fatalf("the second page answered %d with %d posts", code, len(posts.FindAllString(body, -1)))
	}
	for _, want := range []string{
		"<title>2025 年 8 月 29 日 · 归档 · hub.example</title>",
		`href="/archives/2025-08-29?lang=zh&amp;after=` + ids[1] + `"`,
		`href="/archives?lang=zh"`,
		"32 条帖子 · UTC",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the second page lacks %q", want)
		}
	}
	// Prev from the second page is the day's own address
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://hub.example/archives/2025-08-29?lang=zh&after="+ids[1], nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/archives/2025-08-29?lang=zh" {
		t.Errorf("Prev from the second page answered %d to %q", w.Code, w.Header().Get("Location"))
	}

	for _, path := range []string{
		"/archives/2025-08-30",                  // a day with no posts
		"/archives/2999-01-01",                  // a day still to come
		"/archives/2025-8-29", "/archives/nope", // not a date
		"/archives/2025-02-30",
		"/archives/2025-08-29?before=nope", // not a post
	} {
		if code, _ := get(t, h, path); code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, code)
		}
	}
	if _, body := get(t, h, "/archives/2025-08-30"); !strings.Contains(body, "No posts were sent on that day.") {
		t.Error("a day with no posts does not say so")
	}
	if got := s.StatsPathLabel("/archives/2025-08-29"); got != "Archives: 2025-08-29" {
		t.Errorf("the stats call the day %q", got)
	}
}
