package api

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"exehub/internal/config"
	"exehub/internal/identity"
	"exehub/internal/stats"
	"exehub/internal/store"
)

func testStats(t *testing.T) *Server {
	t.Helper()
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	c, err := stats.New(s.St, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	c.Log = func(string, ...any) {}
	s.Stats = c
	return s
}

// drain takes the queued hits through the collector's session step and
// into the store, what Run does once a second.
func drain(t *testing.T, s *Server) {
	t.Helper()
	var batch []store.Hit
	for {
		h, ok := s.Stats.TakeForTest()
		if !ok {
			break
		}
		batch = append(batch, h)
	}
	if len(batch) > 0 {
		if err := s.St.StatsAdd(batch); err != nil {
			t.Fatal(err)
		}
	}
	s.statsMu.Lock()
	s.statsCache = nil
	s.statsMu.Unlock()
}

// TestStatsCounting: a browser's navigation to a page is a page view,
// a fetch(), a refetch, a crawler, a 404 and the JSON API are not; the
// skill guide counts a curl.
func TestStatsCounting(t *testing.T) {
	s := testStats(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "profile.set", map[string]any{"name": "Ann"})
	id := ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "hello stats world"})
	h := s.Handler()
	browser := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	nav := func(path string, hdr ...string) int {
		code, _ := get(t, h, path, append([]string{"User-Agent", browser, "Sec-Fetch-Dest", "document", "Accept", "text/html", "CF-IPCountry", "JP", "CF-Connecting-IP", "203.0.113.5"}, hdr...)...)
		return code
	}
	nav("/")
	nav("/p/" + id)
	nav("/u/" + pub2id(pub))
	nav("/search?q=hello")
	nav("/nothing")                                                  // 404
	nav("/", "X-Hub-Live", "1")                                      // the live feed's refetch
	get(t, h, "/", "User-Agent", browser, "Sec-Fetch-Dest", "empty") // a fetch()
	get(t, h, "/", "User-Agent", "Googlebot/2.1", "Sec-Fetch-Dest", "document", "Accept", "text/html")
	get(t, h, "/v1/feed", "User-Agent", browser, "Sec-Fetch-Dest", "document")
	get(t, h, "/skill.md", "User-Agent", "curl/8.5.0", "Accept", "*/*")
	get(t, h, "/stats", "User-Agent", browser, "Sec-Fetch-Dest", "document", "Accept", "text/html")
	drain(t, s)
	sum, err := s.St.StatsSummary(store.StatsFilter{From: 0, To: time.Now().UnixMilli() + 1000})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Pageviews != 5 {
		t.Errorf("page views = %d, want 5 (four pages and the skill; the crawler apart)", sum.Pageviews)
	}
	bots, _ := s.St.StatsTop(store.StatsFilter{To: time.Now().UnixMilli() + 1000}, "crawler")
	if len(bots) != 1 || bots[0].Key != "Googlebot" || bots[0].N != 1 {
		t.Errorf("crawlers %v, want Googlebot 1", bots)
	}
	if sum.Visitors != 2 || sum.Sessions != 2 {
		t.Errorf("visitors %d sessions %d, want 2 and 2 (the browser, and curl)", sum.Visitors, sum.Sessions)
	}
	rows, _ := s.St.StatsTop(store.StatsFilter{To: time.Now().UnixMilli() + 1000}, "path")
	paths := map[string]int{}
	for _, r := range rows {
		paths[r.Key] = r.N
	}
	if paths["/"] != 1 || paths["/p/"+id] != 1 || paths["/search"] != 1 || paths["/skill.md"] != 1 || paths["/stats"] != 0 || paths["/nothing"] != 0 {
		t.Errorf("paths %v", paths)
	}
}

// TestStatsPage: the window renders the tiles, the chart, the lists
// with their views and filters, and "N online" on the feed; the JSON
// carries every list; a hub without stats 404s both.
func TestStatsPage(t *testing.T) {
	s := testStats(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "profile.set", map[string]any{"name": "Ann"})
	id := ingest(t, s, priv, pub, 2, "post.create", map[string]any{"text": "# A heading\nthe words of the post"})
	now := time.Now().UnixMilli()
	if err := s.St.StatsAdd([]store.Hit{
		{TS: now - 60_000, VID: "aaaa", SID: "s1", Entry: true, Path: "/", Kind: "home", Ref: "Google", Channel: "search", Country: "JP", Device: "desktop", Browser: "Chrome", OS: "Windows", Lang: "ja"},
		{TS: now - 30_000, VID: "aaaa", SID: "s1", Path: "/p/" + id, Kind: "thread", Ref: "Google", Channel: "search", Country: "JP", Device: "desktop", Browser: "Chrome", OS: "Windows", Lang: "ja"},
		{TS: now - 20_000, VID: "bbbb", SID: "s2", Entry: true, Path: "/u/" + pub2id(pub), Kind: "profile", Channel: "direct", Country: "CN", Device: "mobile", Browser: "Safari", OS: "iOS", Lang: "zh-CN"},
		{TS: now - 26*3600_000, VID: "cccc", SID: "s3", Entry: true, Path: "/", Kind: "home", Channel: "direct", Country: "US", Device: "desktop"},
		{TS: now - 10_000, VID: "gggg", SID: "g1", Entry: true, Path: "/p/" + id, Kind: "thread", Channel: "direct", Country: "US", Device: "bot", Browser: "Googlebot", Bot: true},
	}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	code, body := get(t, h, "/stats?range=7d")
	if code != 200 {
		t.Fatalf("GET /stats = %d", code)
	}
	for _, want := range []string{
		`<a class="btn tab on" href="/stats?range=7d" aria-current="page">7 days</a>`, // the current range, held down, still a link
		`href="/stats?range=30d"`,
		`<div class="tl">Visitors</div><div class="tv">3</div>`,
		`<div class="tl">Page views</div><div class="tv">4</div>`,
		`<div class="tl">Sessions</div><div class="tv">3</div>`,
		`<div class="tl">Bounce rate</div><div class="tv">67%</div>`,
		`<polyline class="pv"`,
		`Google`, `Direct`,
		`Ann: A heading the words of the post`,        // a thread by its words
		`Profile: Ann`,                                // a profile by its name
		`Japan<span class="tag">JP</span>`,            // a country by name, its code beside it
		`href="/stats?country=CN&amp;range=7d#w-loc"`, // a row is a filter
		`href="/v1/stats?range=7d"`,                   // the same view as JSON
		`2 online`,                                    // aaaa and bbbb in the last 5 minutes
		`<path class="f" style="fill: #4b0082"`,       // bbbb's disc: 0xbb % 20 = 7, Indigo
		`<path class="f" style="fill: #c8a2c8"`,       // aaaa's: 0xaa % 20 = 10, Lilac
		`headers: { "X-Hub-Live": "1" }`,
		`<div class="window sw" id="w-bt">`,             // the Bots window
		`href="/stats?bot=Googlebot&amp;range=7d#w-bt"`, // a crawler's row holds the view to it
		`Page views · left out of every other number`,
		`<a href="/stats?bot=all&amp;range=7d#w-bt">Only crawlers</a>`,
		`<div class="window sw" id="w-dev">`,            // a window is an anchor
		`href="/stats?dev=browsers&amp;range=7d#w-dev"`, // a view link lands on its window without script
		`history.pushState(null, "", url)`,              // with script the view is fetched in place
		`<div class="sgrid"><div class="lane">`,         // the lists cut into lanes by the server: no masonry CSS to wait for
		// the range last pressed is remembered in the browser: the head goes on to it, past the default, among the ranges there are
		`r !== "7d" && ["today", "yesterday", "24h", "7d", "30d", "3m", "6m", "12m", ].includes(r)`,
		`localStorage.setItem("exe-hub-stats-range", u.searchParams.get("range"))`,
		// one view at a time: an overtaken answer is turned away, its fetch called off
		`if (mine !== turn) return false;`, `signal: ctl.signal`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stats page lacks %q", want)
		}
	}
	// a filter narrows the view and shows as a chip; a view button holds
	code, body = get(t, h, "/stats?range=7d&country=JP&dev=browsers")
	if code != 200 {
		t.Fatalf("filtered = %d", code)
	}
	for _, want := range []string{
		`<div class="tl">Visitors</div><div class="tv">1</div>`,
		`<span class="chip">Country <b>Japan</b><a href="/stats?dev=browsers&amp;range=7d"`,
		`<a class="btn tab on" href="/stats?country=JP&amp;dev=browsers&amp;range=7d#w-dev" aria-current="page">Browser</a>`,
		`>Chrome</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("filtered page lacks %q", want)
		}
	}
	if strings.Contains(body, ">Safari</a>") {
		t.Error("a filtered view shows the other country's browser")
	}
	// held to one crawler: its numbers, its chip, and the Bots window still lists it
	code, body = get(t, h, "/stats?range=7d&bot=Googlebot")
	if code != 200 {
		t.Fatalf("bot view = %d", code)
	}
	for _, want := range []string{
		`<div class="tl">Page views</div><div class="tv">1</div>`,
		`<span class="chip">Bot <b>Googlebot</b>`,
		`>bot</a>`, // the Devices list: device "bot"
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bot view lacks %q", want)
		}
	}
	if strings.Contains(body, `Only crawlers`) {
		t.Error("the Only crawlers link on a view already held to a crawler")
	}
	// yesterday's span holds the old hit alone
	_, body = get(t, h, "/stats?range=yesterday")
	if !strings.Contains(body, `<div class="tl">Page views</div><div class="tv">1</div>`) {
		t.Error("yesterday's page view missing")
	}
	// the feed says who is here
	_, body = get(t, h, "/")
	if !strings.Contains(body, `<a href="/stats" title="Who reads this hub">2 online</a>`) {
		t.Error("the feed's pager lacks the online link")
	}
	// the JSON
	req := httptest.NewRequest("GET", "http://hub.example/v1/stats?range=7d", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/v1/stats = %d", rec.Code)
	}
	var rep struct {
		Range   string                      `json:"range"`
		Zone    string                      `json:"zone"`
		Summary map[string]float64          `json:"summary"`
		Series  []map[string]any            `json:"series"`
		Lists   map[string][]map[string]any `json:"lists"`
		Live    struct {
			Online int
			Recent []map[string]any
		} `json:"live"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Range != "7d" || rep.Zone != "UTC" || rep.Summary["pageviews"] != 4 || len(rep.Series) != 7 || len(rep.Lists) != 15 || rep.Live.Online != 2 {
		t.Errorf("json %+v", rep)
	}
	if rep.Lists["countries"][0]["key"] != "JP" && rep.Lists["countries"][0]["key"] != "CN" {
		t.Errorf("countries %v", rep.Lists["countries"])
	}
	if c := rep.Lists["crawlers"]; len(c) != 1 || c[0]["key"] != "Googlebot" {
		t.Errorf("crawlers %v", c)
	}
	// no collector: no page, no JSON, no link
	s.Stats = nil
	h = s.Handler()
	if code, _ := get(t, h, "/stats"); code != 404 {
		t.Errorf("/stats without stats = %d", code)
	}
	if code, _ := get(t, h, "/v1/stats"); code != 404 {
		t.Errorf("/v1/stats without stats = %d", code)
	}
	if _, body := get(t, h, "/"); strings.Contains(body, "online</a>") {
		t.Error("the feed links stats a hub does not have")
	}
}

// TestStatsLanes: the list windows stand in two lanes that are a cut of
// the reading order — the first few left, the rest right, cut where the
// heights come closest, the left lane the taller on a tie — so the
// markup's order is the eye's at every width.
func TestStatsLanes(t *testing.T) {
	for _, c := range []struct {
		rows []int
		want string
	}{
		{[]int{10, 12, 12, 4, 9}, "0 1|2 3 4"},   // 668 beside 842: a third window left would be 1022 beside 488
		{[]int{7, 12, 12, 4, 12}, "0 1|2 3 4"},   // the public hub's page on the day this was written
		{[]int{12, 12, 12, 12, 12}, "0 1 2|3 4"}, // a tie: the left lane is the taller
		{[]int{12, 0, 0, 0, 12}, "0 1 2|3 4"},    // 626 beside 490 either way round: the tie goes left
		{[]int{0, 0, 0, 0, 0}, "0 1 2|3 4"},
		{[]int{3}, "0|"},
		{[]int{0, 12}, "0|1"}, // two windows never share a lane
	} {
		var lists []statsList
		for i, n := range c.rows {
			lists = append(lists, statsList{Anchor: strconv.Itoa(i), Rows: make([]statsRowView, n)})
		}
		var got []string
		for _, lane := range statsLanes(lists) {
			var ids []string
			for _, l := range lane {
				ids = append(ids, l.Anchor)
			}
			got = append(got, strings.Join(ids, " "))
		}
		if g := strings.Join(got, "|"); g != c.want {
			t.Errorf("rows %v stand as %q, want %q", c.rows, g, c.want)
		}
	}
}

// TestStatsSpan: the ranges' bounds and buckets in a zone.
func TestStatsSpan(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 9, 16, 14, 30, 0, 0, loc)
	for _, c := range []struct {
		key        string
		from, prev string
		buckets    int
		step       string
	}{
		{"today", "2026-09-16T00:00", "2026-09-15T00:00", 24, "hour"},
		{"yesterday", "2026-09-15T00:00", "2026-09-14T00:00", 24, "hour"},
		{"24h", "2026-09-15T14:30", "2026-09-14T14:30", 25, "hour"},
		{"7d", "2026-09-10T00:00", "2026-09-03T00:00", 7, "day"},
		{"30d", "2026-08-18T00:00", "2026-07-19T00:00", 30, "day"},
		{"3m", "2026-07-01T00:00", "2026-04-01T00:00", 78, "day"},
		{"6m", "2026-04-01T00:00", "2025-10-01T00:00", 6, "month"},
		{"12m", "2025-10-01T00:00", "2024-10-01T00:00", 12, "month"},
		{"bogus", "2026-09-10T00:00", "2026-09-03T00:00", 7, "day"},
	} {
		sp := statsSpanOf(c.key, now, loc)
		if got := sp.From.Format("2006-01-02T15:04"); got != c.from {
			t.Errorf("%s from %s, want %s", c.key, got, c.from)
		}
		if got := sp.PrevFrom.Format("2006-01-02T15:04"); got != c.prev {
			t.Errorf("%s prev from %s, want %s", c.key, got, c.prev)
		}
		if len(sp.Buckets) != c.buckets || sp.Step != c.step {
			t.Errorf("%s: %d %s buckets, want %d %s", c.key, len(sp.Buckets), sp.Step, c.buckets, c.step)
		}
		if !sp.PrevTo.Equal(sp.From) && c.key != "today" {
			t.Errorf("%s: the span before ends where this begins", c.key)
		}
		if sp.IncompleteLast == (c.key == "yesterday") {
			t.Errorf("%s: incomplete %v", c.key, sp.IncompleteLast)
		}
	}
	if statsCeil(3) != 4 || statsCeil(37) != 40 || statsCeil(130) != 160 || statsCeil(1000) != 1200 {
		t.Error("ceilings")
	}
	if statsNum(1234567) != "1,234,567" || statsNum(999) != "999" {
		t.Error("numbers")
	}
	if statsDur(75.4) != "1m 15s" || statsDur(9) != "9s" {
		t.Error("durations")
	}
	if statsSigned(12.34, "%") != "+12.3%" || statsSigned(-5, "%") != "−5.0%" || statsSigned(0, " pp") != "±0 pp" {
		t.Error("signed")
	}
}

func pub2id(pub ed25519.PublicKey) string { return identity.Fingerprint(pub) }

var _ = http.StatusOK
