package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestStats: hits land, the summary counts visitors, views, sessions,
// bounces and length; the ranked lists count what they should; the
// filter narrows every query; the sweep drops old rows.
func TestStats(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := int64(1_760_000_000_000)
	hits := []Hit{
		// visitor a: a two-page session from Google, then one page 40 min later (another session)
		{TS: base, VID: "a", SID: "s1", Entry: true, Path: "/", Kind: "home", Ref: "Google", Channel: "search", Country: "DE", Device: "desktop", Browser: "Chrome", OS: "Windows", Lang: "de"},
		{TS: base + 60_000, VID: "a", SID: "s1", Path: "/p/1", Kind: "thread", Ref: "Google", Channel: "search", Country: "DE", Device: "desktop", Browser: "Chrome", OS: "Windows", Lang: "de"},
		{TS: base + 40*60_000, VID: "a", SID: "s2", Entry: true, Path: "/p/2", Kind: "thread", Channel: "direct", Country: "DE", Device: "desktop", Browser: "Chrome", OS: "Windows", Lang: "de"},
		// visitor b: one page, direct, on a phone
		{TS: base + 10_000, VID: "b", SID: "s3", Entry: true, Path: "/p/1", Kind: "thread", Channel: "direct", Country: "CN", Device: "mobile", Browser: "Safari", OS: "iOS", Lang: "zh-CN", UTMCampaign: "sept"},
		// an old one, outside the span
		{TS: base - 3_600_000, VID: "z", SID: "s0", Entry: true, Path: "/", Kind: "home", Channel: "direct", Country: "US", Device: "desktop"},
	}
	if err := s.StatsAdd(hits); err != nil {
		t.Fatal(err)
	}
	f := StatsFilter{From: base, To: base + 3_600_000}
	sum, err := s.StatsSummary(f)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Visitors != 2 || sum.Pageviews != 4 || sum.Sessions != 3 || sum.Bounces != 2 {
		t.Errorf("summary %+v", sum)
	}
	if sum.Duration != 20 { // (60 + 0 + 0) / 3
		t.Errorf("duration %v", sum.Duration)
	}
	top := func(dim string, f StatsFilter) map[string]int {
		rows, err := s.StatsTop(f, dim)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]int{}
		for _, r := range rows {
			m[r.Key] = r.N
		}
		return m
	}
	if m := top("source", f); m["Google"] != 1 || m[""] != 2 {
		t.Errorf("sources %v", m)
	}
	if m := top("channel", f); m["search"] != 1 || m["direct"] != 2 {
		t.Errorf("channels %v", m)
	}
	if m := top("campaign", f); m["sept"] != 1 || len(m) != 1 {
		t.Errorf("campaigns %v", m)
	}
	if m := top("path", f); m["/p/1"] != 2 || m["/"] != 1 || m["/p/2"] != 1 {
		t.Errorf("pages %v", m)
	}
	if m := top("entry", f); m["/"] != 1 || m["/p/1"] != 1 || m["/p/2"] != 1 {
		t.Errorf("entry %v", m)
	}
	if m := top("exit", f); m["/p/1"] != 2 || m["/p/2"] != 1 || m["/"] != 0 {
		t.Errorf("exit %v", m)
	}
	if m := top("country", f); m["DE"] != 1 || m["CN"] != 1 || m["US"] != 0 {
		t.Errorf("countries %v", m)
	}
	if m := top("device", f); m["desktop"] != 1 || m["mobile"] != 1 {
		t.Errorf("devices %v", m)
	}
	if m := top("lang", f); m["de"] != 1 || m["zh-CN"] != 1 {
		t.Errorf("languages %v", m)
	}
	if _, err := s.StatsTop(f, "vid"); err == nil {
		t.Error("an unknown dimension must be refused")
	}
	// a filter narrows every query
	g := f
	g.Country = "DE"
	sum, _ = s.StatsSummary(g)
	if sum.Visitors != 1 || sum.Pageviews != 3 || sum.Sessions != 2 {
		t.Errorf("filtered summary %+v", sum)
	}
	if m := top("path", g); m["/p/1"] != 1 || len(m) != 3 {
		t.Errorf("filtered pages %v", m)
	}
	hours, err := s.StatsHours(f)
	if err != nil || len(hours) == 0 {
		t.Fatalf("hours %v %v", hours, err)
	}
	views := 0
	for _, h := range hours {
		views += h.Pageviews
	}
	if views != 4 {
		t.Errorf("hourly views %d", views)
	}
	firsts, err := s.StatsVisitorFirst(f)
	if err != nil || len(firsts) != 2 {
		t.Errorf("visitor firsts %v %v", firsts, err)
	}
	recent, err := s.StatsRecent(3)
	if err != nil || len(recent) != 3 || recent[0].Path != "/p/2" || recent[2].Path != "/p/1" { // newest first, by time
		t.Errorf("recent %v %v", recent, err)
	}
	if n, _ := s.StatsOnline(base + 30*60_000); n != 1 {
		t.Errorf("online %d", n)
	}
	open, err := s.StatsOpenSessions(base + 30*60_000)
	if err != nil || len(open) != 1 || open[0].SID != "s2" {
		t.Errorf("open %v %v", open, err)
	}
	// the salt: one per day, kept for the day, the others dropped
	s1, err := s.StatsSalt("2026-09-16")
	if err != nil || len(s1) != 16 {
		t.Fatal(err)
	}
	if s2, _ := s.StatsSalt("2026-09-16"); string(s2) != string(s1) {
		t.Error("the day's salt changed")
	}
	if s3, _ := s.StatsSalt("2026-09-17"); string(s3) == string(s1) {
		t.Error("a new day, the same salt")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM hits_salt`).Scan(&n); err != nil || n != 1 {
		t.Errorf("salts kept: %d", n)
	}
	gone, err := s.StatsSweep(time.UnixMilli(base))
	if err != nil || gone != 1 {
		t.Errorf("sweep %d %v", gone, err)
	}
	// Rebuild leaves the hits alone
	if err := s.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if sum, _ := s.StatsSummary(f); sum.Pageviews != 4 {
		t.Errorf("hits lost in Rebuild: %+v", sum)
	}
}
