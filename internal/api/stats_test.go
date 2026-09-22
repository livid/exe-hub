package api

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exehub/internal/config"
	"exehub/internal/identity"
	stats "github.com/livid/exe-stats"
)

func testStats(t *testing.T) *Server {
	t.Helper()
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	an, err := stats.New(s.St.DB(), stats.Options{Location: time.UTC, PathLabel: s.StatsPathLabel,
		AnyClientKinds: []string{"skill"}, HomeURL: "/", HomeLabel: "Back to the feed",
		Log: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	s.Stats = an
	return s
}

// statsHits writes hits straight into the hub's database, as the
// collector's writer would, and lets the next report see them.
func statsHits(t *testing.T, s *Server, hits []stats.Hit) {
	t.Helper()
	if err := statsDB(t, s).Add(hits); err != nil {
		t.Fatal(err)
	}
	s.Stats.ResetCache()
}

// drain writes what the collector has queued, as its writer does once
// a second, and lets the next report see it.
func drain(t *testing.T, s *Server) {
	t.Helper()
	if err := s.Stats.Flush(); err != nil {
		t.Fatal(err)
	}
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
	sum, err := statsDB(t, s).Summary(stats.Filter{From: 0, To: time.Now().UnixMilli() + 1000})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Pageviews != 5 {
		t.Errorf("page views = %d, want 5 (four pages and the skill; the crawler apart)", sum.Pageviews)
	}
	bots, _ := statsDB(t, s).Top(stats.Filter{To: time.Now().UnixMilli() + 1000}, "crawler")
	if len(bots) != 1 || bots[0].Key != "Googlebot" || bots[0].N != 1 {
		t.Errorf("crawlers %v, want Googlebot 1", bots)
	}
	if sum.Visitors != 2 || sum.Sessions != 2 {
		t.Errorf("visitors %d sessions %d, want 2 and 2 (the browser, and curl)", sum.Visitors, sum.Sessions)
	}
	rows, _ := statsDB(t, s).Top(stats.Filter{To: time.Now().UnixMilli() + 1000}, "path")
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
	statsHits(t, s, []stats.Hit{
		{TS: now - 60_000, VID: "aaaa", SID: "s1", Entry: true, Path: "/", Kind: "home", Ref: "Google", Channel: "search", Country: "JP", Device: "desktop", Browser: "Chrome", OS: "Windows", Lang: "ja"},
		{TS: now - 30_000, VID: "aaaa", SID: "s1", Path: "/p/" + id, Kind: "thread", Ref: "Google", Channel: "search", Country: "JP", Device: "desktop", Browser: "Chrome", OS: "Windows", Lang: "ja"},
		{TS: now - 20_000, VID: "bbbb", SID: "s2", Entry: true, Path: "/u/" + pub2id(pub), Kind: "profile", Channel: "direct", Country: "CN", Device: "mobile", Browser: "Safari", OS: "iOS", Lang: "zh-CN"},
		{TS: now - 26*3600_000, VID: "cccc", SID: "s3", Entry: true, Path: "/", Kind: "home", Channel: "direct", Country: "US", Device: "desktop"},
		{TS: now - 10_000, VID: "gggg", SID: "g1", Entry: true, Path: "/p/" + id, Kind: "thread", Channel: "direct", Country: "US", Device: "bot", Browser: "Googlebot", Bot: true},
	})
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
	if !strings.Contains(body, `<a href="/stats" title="Who reads this hub"><span data-n="online">2</span> online</a>`) {
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

func pub2id(pub ed25519.PublicKey) string { return identity.Fingerprint(pub) }

var _ = http.StatusOK

// statsDB is the hits tables of this test's hub.
func statsDB(t *testing.T, s *Server) *stats.DB {
	t.Helper()
	d, err := stats.Open(s.St.DB())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The hits are the hub's own record of its readers, not derived from
// the log: a Rebuild replays every post and leaves them where they are.
func TestStatsSurviveRebuild(t *testing.T) {
	s := testStats(t)
	now := time.Now().UnixMilli()
	statsHits(t, s, []stats.Hit{
		{TS: now - 1000, VID: "aaaa", SID: "s1", Entry: true, Path: "/", Kind: "home", Country: "JP", Device: "desktop"},
	})
	if err := s.St.Rebuild(); err != nil {
		t.Fatal(err)
	}
	sum, err := statsDB(t, s).Summary(stats.Filter{To: now + 1000})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Pageviews != 1 || sum.Visitors != 1 {
		t.Errorf("the hits did not survive a rebuild: %+v", sum)
	}
}
