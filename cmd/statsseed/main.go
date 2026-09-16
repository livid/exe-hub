// statsseed fills a hub database with a week of made-up page views, for
// screenshots of /stats. Scratch only; never run against a real hub.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"time"

	"exehub/internal/store"
)

func pick[T any](r *rand.Rand, w []struct {
	v T
	p int
}) T {
	t := 0
	for _, x := range w {
		t += x.p
	}
	n := r.Intn(t)
	for _, x := range w {
		if n < x.p {
			return x.v
		}
		n -= x.p
	}
	return w[0].v
}

type wv = struct {
	v string
	p int
}

func main() {
	db := flag.String("db", "", "hub.db to seed")
	days := flag.Int("days", 8, "days of traffic")
	flag.Parse()
	st, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	posts, _ := st.Feed("", 20, false)
	var paths []string
	paths = append(paths, "/", "/", "/", "/", "/")
	for _, p := range posts {
		paths = append(paths, "/p/"+p.ID)
		if p.Author != "" {
			paths = append(paths, "/u/"+p.Author)
		}
	}
	paths = append(paths, "/search", "/skill.md")
	countries := []wv{{"CN", 30}, {"US", 25}, {"JP", 10}, {"DE", 8}, {"GB", 6}, {"SG", 5}, {"TW", 5}, {"HK", 4}, {"KR", 3}, {"FR", 3}, {"BR", 2}, {"IN", 2}, {"CA", 2}, {"AU", 1}, {"NL", 1}}
	devices := []wv{{"desktop", 62}, {"mobile", 30}, {"tablet", 4}, {"agent", 4}}
	sources := []wv{{"", 55}, {"Google", 14}, {"X", 9}, {"V2EX", 8}, {"Hacker News", 4}, {"Telegram", 3}, {"GitHub", 3}, {"ChatGPT", 2}, {"Bing", 1}, {"blog.example.org", 1}}
	channel := map[string]string{"": "direct", "Google": "search", "Bing": "search", "X": "social", "V2EX": "social", "Hacker News": "social", "Telegram": "social", "GitHub": "referral", "ChatGPT": "ai", "blog.example.org": "referral"}
	langOf := map[string]string{"CN": "zh-CN", "US": "en-US", "JP": "ja", "DE": "de", "GB": "en-GB", "SG": "en", "TW": "zh-TW", "HK": "zh-HK", "KR": "ko", "FR": "fr", "BR": "pt-BR", "IN": "en-IN", "CA": "en-CA", "AU": "en-AU", "NL": "nl"}
	r := rand.New(rand.NewSource(7))
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Now().In(loc)
	var hits []store.Hit
	id := func() string {
		b := make([]byte, 12)
		r.Read(b)
		return hex.EncodeToString(b)
	}
	for d := *days - 1; d >= 0; d-- {
		day := time.Date(now.Year(), now.Month(), now.Day()-d, 0, 0, 0, 0, loc)
		n := 28 + r.Intn(30) + d%3*6
		if d == 0 {
			n = n * (now.Hour() + 1) / 24
		}
		for v := 0; v < n; v++ {
			// a daily curve: quiet at night, busy in the afternoon and evening
			h := 0.0
			for {
				h = r.Float64() * 24
				if r.Float64() < 0.35+0.65*math.Max(0, math.Sin((h-6)/18*math.Pi)) {
					break
				}
			}
			t := day.Add(time.Duration(h * float64(time.Hour)))
			if t.After(now) {
				continue
			}
			vid := id()
			c := pick(r, countries)
			dev := pick(r, devices)
			src := pick(r, sources)
			browser, os := "Chrome", "Windows"
			switch dev {
			case "mobile":
				browser, os = pick(r, []wv{{"Safari", 55}, {"Chrome", 35}, {"WeChat", 10}}), "iOS"
				if browser == "Chrome" || browser == "WeChat" && r.Intn(2) == 0 {
					os = "Android"
				}
			case "desktop":
				browser = pick(r, []wv{{"Chrome", 55}, {"Safari", 20}, {"Firefox", 12}, {"Edge", 13}})
				os = pick(r, []wv{{"Windows", 45}, {"macOS", 40}, {"Linux", 15}})
				if browser == "Safari" {
					os = "macOS"
				}
			case "tablet":
				browser, os = "Safari", "iOS"
			case "agent":
				browser, os = pick(r, []wv{{"curl", 50}, {"Claude", 30}, {"Python", 20}}), ""
			}
			pages := 1 + r.Intn(4)
			if r.Float64() < 0.45 {
				pages = 1
			}
			sid := id()
			for p := 0; p < pages; p++ {
				path := paths[r.Intn(len(paths))]
				if dev == "agent" {
					path = "/skill.md"
					pages = 1
				}
				kind := "home"
				switch {
				case len(path) > 3 && path[:3] == "/p/":
					kind = "thread"
				case len(path) > 3 && path[:3] == "/u/":
					kind = "profile"
				case path == "/search":
					kind = "search"
				case path == "/skill.md":
					kind = "skill"
				}
				hits = append(hits, store.Hit{TS: t.UnixMilli(), VID: vid, SID: sid, Entry: p == 0, Path: path, Kind: kind,
					Ref: src, Channel: channel[src], Country: c, Device: dev, Browser: browser, OS: os, Lang: langOf[c]})
				t = t.Add(time.Duration(20+r.Intn(200)) * time.Second)
				if t.After(now) {
					break
				}
			}
		}
	}
	// a few people here right now
	for i := 0; i < 5; i++ {
		t := now.Add(-time.Duration(r.Intn(280)) * time.Second)
		c := pick(r, countries)
		hits = append(hits, store.Hit{TS: t.UnixMilli(), VID: id(), SID: id(), Entry: true, Path: paths[r.Intn(len(paths))], Kind: "thread",
			Channel: "direct", Country: c, Device: "desktop", Browser: "Chrome", OS: "macOS", Lang: langOf[c]})
	}
	if err := st.StatsAdd(hits); err != nil {
		log.Fatal(err)
	}
	fmt.Println("seeded", len(hits), "page views")
}
