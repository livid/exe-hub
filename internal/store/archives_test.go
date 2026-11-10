package store

import (
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
