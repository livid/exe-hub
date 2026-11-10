package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// TestPostDays: a post counts on the day of its own time in the zone
// asked for, a reply like any post and a deleted post not at all; a time
// that runs ahead of the hub's clock is not believed, and the post
// counts on the day it was received.
func TestPostDays(t *testing.T) {
	s := openTest(t)
	if days, first, err := s.PostDays(time.UTC); err != nil || len(days) != 0 || first != "" {
		t.Fatalf("an empty hub: %v, %q, %v", days, first, err)
	}
	a := newAuthor(t)
	at := func(day string, hour int) int64 {
		d, _ := time.Parse("2006-01-02", day)
		return d.Add(time.Duration(hour) * time.Hour).UnixMilli()
	}
	a.ts = at("2026-03-01", 2)
	root := ingest(t, s, a, "post.create", map[string]any{"text": "one"})
	a.ts = at("2026-03-01", 23)
	ingest(t, s, a, "post.create", map[string]any{"text": "two", "reply_to": root})
	a.ts = at("2026-03-03", 12)
	gone := ingest(t, s, a, "post.create", map[string]any{"text": "three"})
	ingest(t, s, a, "post.delete", map[string]any{"post": gone})
	a.ts = at("2999-01-01", 0)
	ingest(t, s, a, "post.create", map[string]any{"text": "from the future"})

	today := time.Now().UTC().Format("2006-01-02")
	days, first, err := s.PostDays(time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	want := []DayPosts{{"2026-03-01", 2}, {today, 1}}
	if !slices.Equal(days, want) || first != today {
		t.Errorf("PostDays = %v, first %q; want %v, first %q", days, first, want, today)
	}
	// five hours behind UTC, the post sent at 02:00 was sent the evening before
	west := time.FixedZone("west", -5*3600)
	days, _, err = s.PostDays(west)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) < 2 || days[0] != (DayPosts{"2026-02-28", 1}) || days[1] != (DayPosts{"2026-03-01", 1}) {
		t.Errorf("PostDays five hours west = %v; want 2026-02-28 and 2026-03-01 with one post each", days)
	}
}

// TestDayFeed: a day's posts are the ones PostDays counts on it — a
// reply among them, a time ahead of the hub's clock read as the time
// received — the newest first, paged by a post's id in either direction.
func TestDayFeed(t *testing.T) {
	s := openTest(t)
	a := newAuthor(t)
	at := func(day string, hour int) int64 {
		d, _ := time.Parse("2006-01-02", day)
		return d.Add(time.Duration(hour) * time.Hour).UnixMilli()
	}
	a.ts = at("2026-03-01", 2)
	one := ingest(t, s, a, "post.create", map[string]any{"text": "one"})
	a.ts = at("2026-03-01", 12)
	two := ingest(t, s, a, "post.create", map[string]any{"text": "two"})
	a.ts = at("2026-03-01", 23)
	three := ingest(t, s, a, "post.create", map[string]any{"text": "three", "reply_to": one})
	a.ts = at("2026-03-02", 0)
	ingest(t, s, a, "post.create", map[string]any{"text": "the next day"})
	a.ts = at("2999-01-01", 0)
	ahead := ingest(t, s, a, "post.create", map[string]any{"text": "from the future"})

	ids := func(posts []FeedPost, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, p := range posts {
			out = append(out, p.ID)
		}
		return out
	}
	from, to := at("2026-03-01", 0), at("2026-03-02", 0)
	if got := ids(s.DayFeed(from, to, "", 10)); !slices.Equal(got, []string{three, two, one}) {
		t.Errorf("the day = %v, want three, two, one", got)
	}
	if n, err := s.DayCount(from, to); err != nil || n != 3 {
		t.Errorf("DayCount = %d, %v; want 3", n, err)
	}
	if got := ids(s.DayFeed(from, to, three, 1)); !slices.Equal(got, []string{two}) {
		t.Errorf("one post before three = %v, want two", got)
	}
	if got := ids(s.DayFeed(from, to, one, 10)); len(got) != 0 {
		t.Errorf("before the day's first post: %v", got)
	}
	if got := ids(s.DayFeedNewer(from, to, one, 10)); !slices.Equal(got, []string{two, three}) {
		t.Errorf("newer than one = %v, want two, three", got)
	}
	if _, err := s.DayFeed(from, to, "nope", 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown cursor: %v, want ErrNotFound", err)
	}
	// five hours west, the day before holds the post sent at 02:00
	if got := ids(s.DayFeed(from-29*3600_000, from-5*3600_000, "", 10)); len(got) != 0 {
		t.Errorf("2026-02-28 five hours east of where it was sent: %v", got)
	}
	if got := ids(s.DayFeed(from-19*3600_000, from+5*3600_000, "", 10)); !slices.Equal(got, []string{one}) {
		t.Errorf("2026-02-28 five hours west = %v, want one", got)
	}
	// the post that claims 2999 stands where the hub received it: now
	now := time.Now().UnixMilli()
	if got := ids(s.DayFeed(now-3600_000, 1<<62, "", 10)); !slices.Equal(got, []string{ahead}) {
		t.Errorf("the last hour = %v, want the post from the future", got)
	}
	if n, _ := s.DayCount(at("2999-01-01", 0), at("2999-01-02", 0)); n != 0 {
		t.Errorf("%d posts on the day the claim names", n)
	}
}
