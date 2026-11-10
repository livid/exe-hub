package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/identity"
	"exehub/internal/store"
)

// TestBuildActivity: 52 columns ending with the week that holds today,
// Sunday first; a day after today or before the hub's first is off; a
// post dated after today is today's; the levels are the quarters among
// the profile's days; a month's name stands over the column that holds
// its first day, not in the last two columns; the total is the year's.
func TestBuildActivity(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC) // a Tuesday
	a := buildActivity([]store.DayPosts{
		{Day: "2025-11-01", Posts: 5}, // before the grid's first Sunday, 2025-11-16
		{Day: "2026-08-30", Posts: 11},
		{Day: "2026-10-31", Posts: 2},
		{Day: "2026-11-09", Posts: 1},
		{Day: "2026-11-12", Posts: 3}, // ahead of today
	}, "2026-08-30", now, webLocaleTag("en"))
	if len(a.Weeks) != 52 || a.Weeks[0].Days[0].Date != "2025-11-16" || a.Weeks[51].Days[6].Date != "2026-11-14" || a.Weeks[51].Days[2].Date != "2026-11-10" {
		t.Fatalf("the grid runs %s to %s", a.Weeks[0].Days[0].Date, a.Weeks[51].Days[6].Date)
	}
	if a.Total != 17 || a.Zone != "UTC" || a.Rows != [7]string{"", "M", "", "W", "", "F", ""} {
		t.Errorf("total %d in %s, rows %q", a.Total, a.Zone, a.Rows)
	}
	day := func(date string) webActDay {
		for _, w := range a.Weeks {
			for _, d := range w.Days {
				if d.Date == date {
					return d
				}
			}
		}
		t.Fatalf("no %s", date)
		return webActDay{}
	}
	if d := day("2026-11-10"); !d.Today || d.Posts != 3 || d.Level != 2 || d.Off { // the 3 ahead of today; the quarters of 1, 2, 3, 5, 11 are 2, 3, 5
		t.Errorf("today %+v", d)
	}
	if d := day("2026-11-11"); !d.Off || d.Posts != 0 {
		t.Errorf("tomorrow %+v", d)
	}
	if d := day("2026-08-29"); !d.Off || d.Level != 0 {
		t.Errorf("the day before the hub's first %+v", d)
	}
	if d := day("2026-08-30"); d.Off || d.Level != 4 || d.Posts != 11 {
		t.Errorf("the hub's first day %+v", d)
	}
	if d := day("2026-10-31"); d.Level != 1 {
		t.Errorf("the lightest day %+v", d)
	}
	if d := day("2026-09-15"); d.Off || d.Level != 0 || d.Posts != 0 {
		t.Errorf("a quiet day %+v", d)
	}
	var labels []string
	for _, w := range a.Weeks {
		if w.Label != "" {
			labels = append(labels, w.Label)
		}
	}
	if want := "Dec Jan Feb Mar Apr May Jun Jul Aug Sep Oct Nov"; strings.Join(labels, " ") != want { // Nov 1 is in the 51st column, with room
		t.Errorf("months %v, want %s", labels, want)
	}
	if a.Weeks[51].Label != "" || a.Weeks[50].Label != "Nov" {
		t.Errorf("the last columns are named %q %q", a.Weeks[50].Label, a.Weeks[51].Label)
	}
	if a.Weeks[2].Label != "Dec" {
		t.Errorf("December's name is over column %q", a.Weeks[2].Label)
	}
}

// TestWebActivity: the profile page carries the grid with the day's
// square and its title, the year's count, and the letters; the counts
// are kept in memory; another profile's page counts its own posts.
func TestWebActivity(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}, Stats: &config.Stats{Location: time.UTC}})
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "profile.set", map[string]any{"name": "Ann"})
	// the posts' own time is 2025-08-29, older than the grid: received today, they count today
	ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "one"})
	ingest(t, s, priv, pub, 3, "post.create", map[string]any{"text": "two"})
	id := identity.Fingerprint(pub)
	h := s.Handler()
	code, body := get(t, h, "/u/"+id)
	if code != http.StatusOK {
		t.Fatalf("/u/{id} answered %d", code)
	}
	today := time.Now().UTC().Format("2006-01-02")
	// the posts' own time, 2025-08-29, is more than 10 minutes behind the
	// clock, so the Archives believe it: a day before the grid. Nothing
	// today, and the year's count is 0 — but the hub's first day is today,
	// so today's square has a ground and tomorrow's has none.
	if !strings.Contains(body, `<div class="acts" role="img" aria-label="A year of posts, a square a day">`) ||
		!strings.Contains(body, `<div class="act-wd"><i></i><i>M</i><i></i><i>W</i><i></i><i>F</i><i></i></div>`) ||
		!strings.Contains(body, `<i class="l0 now" title="`+today+` · no posts"></i>`) ||
		!strings.Contains(body, `<span>0 posts in the last year</span>`) ||
		strings.Count(body, `<div class="wk">`) != 52 {
		t.Errorf("the grid is wrong: %d columns, today %v", strings.Count(body, `<div class="wk">`), strings.Contains(body, `title="`+today))
	}
	if strings.Count(body, `class="off"`) < 300 || strings.Contains(body, `class="off" title=`) {
		t.Error("the days before the hub's first should be off and carry no title")
	}
	// in Chinese: the names and the count
	if _, body := get(t, h, "/u/"+id+"?lang=zh"); !strings.Contains(body, `aria-label="一年的帖子，一格一天"`) || !strings.Contains(body, `<span>过去一年 0 条帖子</span>`) || !strings.Contains(body, `<div class="act-wd"><i></i><i>一</i><i></i><i>三</i><i></i><i>五</i><i></i></div>`) {
		t.Error("the Chinese grid lacks its words")
	}
	// kept in memory under the id and the day
	if _, ok := s.mem.Get("activity:" + id + ":UTC:" + today); !ok {
		t.Error("the profile's days were not kept")
	}
	// a post sent now shows once the counts are drawn again
	raw := map[string]any{"text": "three"}
	ingestNow(t, s, priv, pub, 4, raw)
	if _, body := get(t, h, "/u/"+id); !strings.Contains(body, `<span>0 posts in the last year</span>`) {
		t.Error("the counts were drawn again inside their time")
	}
	s.mem.Delete("activity:" + id + ":UTC:" + today)
	if _, body := get(t, h, "/u/"+id); !strings.Contains(body, `<i class="l1 now" title="`+today+` · 1 post"></i>`) || !strings.Contains(body, `<span>1 post in the last year</span>`) {
		t.Error("drawn again, today's square is not the new post's")
	}
	// the Pictures tab has no grid
	if _, body := get(t, h, "/u/"+id+"/pictures"); strings.Contains(body, `class="acts"`) {
		t.Error("the Pictures tab carries the grid")
	}
}

// ingestNow is ingest with the post's own time set to now, so the
// Archives and the grid count it today.
func ingestNow(t *testing.T, s *Server, priv ed25519.PrivateKey, pub ed25519.PublicKey, seq int64, body map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"type": "post.create", "author": base64.StdEncoding.EncodeToString(pub),
		"seq": seq, "ts": time.Now().UnixMilli(), "body": body,
	})
	e, err := envelope.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	op, err := e.Op()
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.St.Ingest(raw, ed25519.Sign(priv, raw), e, op)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
