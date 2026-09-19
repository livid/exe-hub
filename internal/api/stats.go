package api

import (
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"exehub/internal/stats"
	"exehub/internal/store"
)

// Stats — the pages' own analytics (PLAN.md, Stats). GET /stats is a
// Platinum window of the numbers: a range of days, the headline tiles
// with their change against the span before, a chart of page views and
// visitors, who is here now, and ranked lists of sources, pages,
// locations and devices; every state is a URL (range, list views,
// stacked filters), so the page works without script and a view can be
// shared as a link. GET /v1/stats is the same report as JSON. The
// counting itself is in internal/stats and the wrap below.

//go:embed stats.html
var statsHTML string

// counted wraps a page handler so a served page is recorded as a hit
// (once the handler answered 200, and only for a request that was a
// person opening a page: stats.Wanted). Without a collector the handler
// is returned as it is.
func (s *Server) counted(kind string, h http.HandlerFunc) http.HandlerFunc {
	if s.Stats == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		h(sw, r)
		if sw.code == http.StatusOK && stats.Wanted(r, kind) {
			s.Stats.Record(r, kind)
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statsRanges are the spans the page offers, in the order of its buttons.
var statsRanges = []struct{ key, label, compare string }{
	{"today", "Today", "the same hours yesterday"}, {"yesterday", "Yesterday", "the day before"},
	{"24h", "24 hours", "the 24 hours before"}, {"7d", "7 days", "the 7 days before"},
	{"30d", "30 days", "the 30 days before"}, {"3m", "3 months", "the 3 months before"},
	{"6m", "6 months", "the 6 months before"}, {"12m", "12 months", "the 12 months before"},
}

const statsDefaultRange = "7d"

// statsSpan is a resolved range: its bounds, the span before it of the
// same length (what the tiles compare against), and the bucket its
// chart draws in.
type statsSpan struct {
	Key, Label     string
	Compare        string    // what the tiles' change is against, in words
	From, To       time.Time // [From, To)
	PrevFrom       time.Time
	PrevTo         time.Time
	Step           string // hour | day | month
	Buckets        []time.Time
	IncompleteLast bool // the last bucket holds now: still filling
}

// statsSpanOf resolves a range key at a moment in the collector's zone.
// today and yesterday are calendar days there (today up to now, drawn
// on the whole day's axis), 24h the clock's last day, 7d and 30d that
// many days ending today, 3m/6m/12m calendar months from the first of
// the month that many months back. The span before: the same length,
// ending where this one starts — today's is the same hours of yesterday.
func statsSpanOf(key string, now time.Time, loc *time.Location) statsSpan {
	now = now.In(loc)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	sp := statsSpan{Key: key}
	for _, r := range statsRanges {
		if r.key == key {
			sp.Label, sp.Compare = r.label, r.compare
		}
	}
	end := now.Add(time.Millisecond) // the current millisecond included
	switch key {
	case "today":
		sp.From, sp.To, sp.Step = day, end, "hour"
		sp.PrevFrom, sp.PrevTo = day.AddDate(0, 0, -1), end.AddDate(0, 0, -1)
	case "yesterday":
		sp.From, sp.To, sp.Step = day.AddDate(0, 0, -1), day, "hour"
		sp.PrevFrom, sp.PrevTo = day.AddDate(0, 0, -2), day.AddDate(0, 0, -1)
	case "24h":
		sp.From, sp.To, sp.Step = now.Add(-24*time.Hour), end, "hour"
		sp.PrevFrom, sp.PrevTo = sp.From.Add(-24*time.Hour), sp.From
	case "30d":
		sp.From, sp.To, sp.Step = day.AddDate(0, 0, -29), end, "day"
		sp.PrevFrom, sp.PrevTo = sp.From.AddDate(0, 0, -30), sp.From
	case "3m":
		sp.From, sp.To, sp.Step = month.AddDate(0, -2, 0), end, "day"
		sp.PrevFrom, sp.PrevTo = sp.From.AddDate(0, -3, 0), sp.From
	case "6m":
		sp.From, sp.To, sp.Step = month.AddDate(0, -5, 0), end, "month"
		sp.PrevFrom, sp.PrevTo = sp.From.AddDate(0, -6, 0), sp.From
	case "12m":
		sp.From, sp.To, sp.Step = month.AddDate(0, -11, 0), end, "month"
		sp.PrevFrom, sp.PrevTo = sp.From.AddDate(0, -12, 0), sp.From
	default: // 7d
		sp.Key, sp.Label, sp.Compare = "7d", "7 days", "the 7 days before"
		sp.From, sp.To, sp.Step = day.AddDate(0, 0, -6), end, "day"
		sp.PrevFrom, sp.PrevTo = sp.From.AddDate(0, 0, -7), sp.From
	}
	// the buckets: whole hours, days or months from the span's start up
	// to its end — today and 24h draw the whole day's axis, so the
	// chart's shape is the same all day
	axisEnd := sp.To
	switch key {
	case "today":
		axisEnd = day.AddDate(0, 0, 1)
	case "24h":
		axisEnd = sp.From.Add(24 * time.Hour)
	}
	var b time.Time
	switch sp.Step {
	case "hour":
		b = time.Date(sp.From.Year(), sp.From.Month(), sp.From.Day(), sp.From.Hour(), 0, 0, 0, loc)
	case "day":
		b = time.Date(sp.From.Year(), sp.From.Month(), sp.From.Day(), 0, 0, 0, 0, loc)
	default:
		b = time.Date(sp.From.Year(), sp.From.Month(), 1, 0, 0, 0, 0, loc)
	}
	for b.Before(axisEnd) {
		sp.Buckets = append(sp.Buckets, b)
		switch sp.Step {
		case "hour":
			b = b.Add(time.Hour)
		case "day":
			b = b.AddDate(0, 0, 1)
		default:
			b = b.AddDate(0, 1, 0)
		}
	}
	sp.IncompleteLast = key != "yesterday" // every other span reaches now: its last bucket is still filling
	return sp
}

// statsFilters are the dimensions a view can be held to, as the query
// names them; the order is the filter row's.
var statsFilters = []struct{ param, label string }{
	{"country", "Country"}, {"region", "Region"}, {"city", "City"}, {"lang", "Language"},
	{"page", "Page"}, {"source", "Source"}, {"channel", "Channel"}, {"campaign", "Campaign"},
	{"device", "Device"}, {"browser", "Browser"}, {"os", "OS"}, {"bot", "Bot"},
}

func statsFilterOf(q url.Values) store.StatsFilter {
	get := func(k string) string {
		v := q.Get(k)
		if len(v) > 256 {
			v = v[:256]
		}
		return v
	}
	return store.StatsFilter{
		Country: get("country"), Region: get("region"), City: get("city"), Lang: get("lang"),
		Path: get("page"), Source: get("source"), Channel: get("channel"), Campaign: get("campaign"),
		Device: get("device"), Browser: get("browser"), OS: get("os"), Bot: get("bot"),
	}
}

func statsValue(f store.StatsFilter, param string) string {
	switch param {
	case "country":
		return f.Country
	case "region":
		return f.Region
	case "city":
		return f.City
	case "lang":
		return f.Lang
	case "page":
		return f.Path
	case "source":
		return f.Source
	case "channel":
		return f.Channel
	case "campaign":
		return f.Campaign
	case "device":
		return f.Device
	case "browser":
		return f.Browser
	case "os":
		return f.OS
	case "bot":
		return f.Bot
	}
	return ""
}

// statsPoint is one bucket of the chart.
type statsPoint struct {
	T         string `json:"t"` // the bucket's start, RFC 3339 in the zone
	Pageviews int    `json:"pageviews"`
	Visitors  int    `json:"visitors"`
	Label     string `json:"-"` // the axis label, "" for a bucket without one
	Future    bool   `json:"-"` // after now: on the axis, without a point
	start     time.Time
}

// statsLive is who is here now: the online count and the latest pages.
type statsLive struct {
	Online int            `json:"online"`
	Recent []statsLiveRow `json:"recent"`
}

type statsLiveRow struct {
	Alias   string `json:"alias"`
	Colour  string `json:"colour"` // the alias's colour, the disc before the name
	Country string `json:"country,omitempty"`
	Path    string `json:"path"`
	Label   string `json:"label,omitempty"`
	Device  string `json:"device,omitempty"`
	TS      string `json:"ts"`
	Ago     string `json:"-"`
}

// statsReport is one view of the numbers, for the page and the JSON.
type statsReport struct {
	Range    string                      `json:"range"`
	From     string                      `json:"from"`
	To       string                      `json:"to"`
	Zone     string                      `json:"zone"`
	Step     string                      `json:"step"`
	Filters  map[string]string           `json:"filters,omitempty"`
	Summary  store.StatsSummary          `json:"summary"`
	Previous store.StatsSummary          `json:"previous"`
	Series   []statsPoint                `json:"series"`
	Lists    map[string][]store.StatsRow `json:"lists"`
	Live     statsLive                   `json:"live"`
	span     statsSpan
	filter   store.StatsFilter
}

// statsLists are the ranked lists by the view name the page uses, with
// the store dimension and what the count is.
var statsLists = map[string]struct{ dim, count string }{
	"sources": {"source", "Sessions"}, "channels": {"channel", "Sessions"}, "campaigns": {"campaign", "Sessions"},
	"pages": {"path", "Visitors"}, "entry": {"entry", "Sessions"}, "exit": {"exit", "Sessions"},
	"countries": {"country", "Visitors"}, "regions": {"region", "Visitors"}, "cities": {"city", "Visitors"}, "languages": {"lang", "Visitors"},
	"devices": {"device", "Visitors"}, "browsers": {"browser", "Visitors"}, "oses": {"os", "Visitors"},
	"crawlers": {"crawler", "Page views"}, "botpages": {"botpath", "Page views"},
}

// statsRecentN is how many latest page views the Live list shows.
const statsRecentN = 10

// statsBuild computes a report: the span's numbers and the span before,
// the series, the lists asked for (nil = all of them) and the live part.
func (s *Server) statsBuild(sp statsSpan, f store.StatsFilter, lists []string, now time.Time) (*statsReport, error) {
	loc := sp.From.Location()
	r := &statsReport{Range: sp.Key, From: sp.From.Format(time.RFC3339), To: sp.To.Format(time.RFC3339),
		Zone: loc.String(), Step: sp.Step, Lists: map[string][]store.StatsRow{}, span: sp, filter: f}
	f.From, f.To = sp.From.UnixMilli(), sp.To.UnixMilli()
	var err error
	if r.Summary, err = s.St.StatsSummary(f); err != nil {
		return nil, err
	}
	pf := f
	pf.From, pf.To = sp.PrevFrom.UnixMilli(), sp.PrevTo.UnixMilli()
	if r.Previous, err = s.St.StatsSummary(pf); err != nil {
		return nil, err
	}
	r.Filters = map[string]string{}
	for _, d := range statsFilters {
		if v := statsValue(f, d.param); v != "" {
			r.Filters[d.param] = v
		}
	}
	if r.Series, err = s.statsSeries(sp, f, now); err != nil {
		return nil, err
	}
	if lists == nil {
		for name := range statsLists {
			lists = append(lists, name)
		}
		sort.Strings(lists)
	}
	for _, name := range lists {
		l, ok := statsLists[name]
		if !ok {
			continue
		}
		rows, err := s.St.StatsTop(f, l.dim)
		if err != nil {
			return nil, err
		}
		if rows == nil {
			rows = []store.StatsRow{}
		}
		r.Lists[name] = rows
	}
	online, err := s.St.StatsOnline(now.Add(-stats.OnlineWindow).UnixMilli())
	if err != nil {
		return nil, err
	}
	recent, err := s.St.StatsRecent(statsRecentN)
	if err != nil {
		return nil, err
	}
	r.Live = statsLive{Online: online, Recent: []statsLiveRow{}}
	for _, h := range recent {
		r.Live.Recent = append(r.Live.Recent, statsLiveRow{
			Alias: stats.Alias(h.VID), Colour: stats.Colour(h.VID), Country: h.Country, Path: h.Path, Device: h.Device,
			TS: time.UnixMilli(h.TS).In(loc).Format(time.RFC3339), Ago: statsAgo(now.Sub(time.UnixMilli(h.TS))),
		})
	}
	return r, nil
}

// statsSeries buckets the span's hours (page views, and visitors within
// the hour) and its visitors' first hits (one per visitor-day) into the
// span's buckets: an hour bucket takes its hour's visitors, a day or
// month bucket counts the visitors whose day falls in it.
func (s *Server) statsSeries(sp statsSpan, f store.StatsFilter, now time.Time) ([]statsPoint, error) {
	hours, err := s.St.StatsHours(f)
	if err != nil {
		return nil, err
	}
	pts := make([]statsPoint, len(sp.Buckets))
	for i, b := range sp.Buckets {
		pts[i].start = b
		pts[i].T = b.Format(time.RFC3339)
		pts[i].Future = !b.Before(now)
	}
	at := func(t time.Time) int {
		// the last bucket starting at or before t
		i := sort.Search(len(sp.Buckets), func(i int) bool { return sp.Buckets[i].After(t) }) - 1
		return i
	}
	for _, h := range hours {
		if i := at(time.UnixMilli(h.Hour * 3600000)); i >= 0 {
			pts[i].Pageviews += h.Pageviews
			if sp.Step == "hour" {
				pts[i].Visitors += h.Visitors
			}
		}
	}
	if sp.Step != "hour" {
		firsts, err := s.St.StatsVisitorFirst(f)
		if err != nil {
			return nil, err
		}
		for _, t := range firsts {
			if i := at(time.UnixMilli(t)); i >= 0 {
				pts[i].Visitors++
			}
		}
	}
	// axis labels: hours every 4th (or 6th past 24), days every one up
	// to 10 buckets, then every 5th, months each
	n := len(pts)
	for i := range pts {
		b := pts[i].start
		switch sp.Step {
		case "hour":
			if i%4 == 0 {
				pts[i].Label = b.Format("15:04")
			}
		case "day":
			switch {
			case n <= 10:
				pts[i].Label = b.Format("Jan 2")
			case n <= 40 && i%5 == 0:
				pts[i].Label = b.Format("Jan 2")
			case n > 40 && b.Day() == 1:
				pts[i].Label = b.Format("Jan 2")
			}
		default:
			pts[i].Label = b.Format("Jan")
			if b.Month() == time.January || i == 0 {
				pts[i].Label = b.Format("Jan 2006")
			}
		}
	}
	return pts, nil
}

func statsAgo(d time.Duration) string {
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
}

// ---- the page ----

// statsTile is one headline number and its change against the span before.
type statsTile struct {
	Label, Value, Delta string
}

// statsList is one ranked-list window as the page draws it: its views
// (the small buttons along its strip), the rows with bar widths, and
// what the count is.
type statsList struct {
	Title    string
	Anchor   string // the window's id: what a link lands on without script
	Views    []statsView
	Rows     []statsRowView
	Count    string // "Visitors" | "Sessions" | "Page views"
	More     int    // rows past those shown
	Empty    string
	Bots     bool   // the Bots window
	OnlyBots string // the Bots window, unfiltered: the link that holds the whole view to crawlers
}

type statsView struct {
	Key, Label, URL string
	On              bool
}

type statsRowView struct {
	Label, Title, URL string
	N                 string
	Pct               int    // the bar, per cent of the top row
	Tag               string // a country code beside its name
}

// statsPage is the template's world for /stats.
type statsPage struct {
	Report   *statsReport
	Ranges   []statsView
	Default  string // the range of a URL that names none: the remembered one need not replace it
	Filters  []statsChip
	Tiles    []statsTile
	Chart    statsChart
	Lanes    [][]statsList // the list windows, dealt two abreast (statsLanes)
	Span     string        // the span in words, the chart's status line
	Zone     string
	ZoneAbbr string // the zone as the clock says it now: PDT
	JSON     string // the same view as JSON
	Online   int
	Recent   []statsLiveRow
}

type statsChip struct {
	Label, Value, URL string
}

// statsChart is the chart's geometry: two polylines in a 1000×1000 box
// stretched to the plot (strokes non-scaling, so they stay 2px), the
// y-axis labels and the x-axis labels with their positions.
type statsChart struct {
	Views, Visitors  string // "x,y x,y …" up to the last complete bucket
	ViewsEnd, VisEnd string // the segment into the incomplete bucket
	Y                []string
	X                []statsXLabel
	Max              int
	Empty            bool
}

type statsXLabel struct {
	Label string
	Pct   float64
	Edge  string // "first" | "last" | ""
}

// statsShown caps a list on the page; the JSON has them all.
const statsShown = 12

// A list window's height is known before it is drawn: its chrome, 20px
// a row, one line when the list is empty, and the margin under it
// (stats.html; measured in the browser, 92 + 20n).
const (
	statsWinChrome = 92 // titlebar 17, the window's and the frame's edges 8, strip 37, the rows' padding 14, status line 16
	statsWinRow    = 20
	statsWinNone   = 22 // the line an empty list says
	statsWinGap    = 22 // the margin under a window
)

// statsLanes deals the list windows into the two lanes they stand in
// once there is room. A lane is a run of the reading order: the first
// few windows left, the rest right, cut where the two lanes' heights
// come closest (the left one the taller on a tie) — so Devices climbs
// up under a short lane instead of leaving a hole beside Locations, and
// the order the page is written in is the order the eye reads at every
// width: down the left lane and then the right, or down a phone's one
// column. Tab and a screen reader step through the markup, so the two
// must not disagree — a shorter-lane-first packing dealt the windows
// out of order and put them back with the `order` property, which moves
// the picture and not the sequence (Codex caught it, 2026-09-17). The
// server cuts because it can: it knows every window's height, so no
// script measures and nothing moves after the first paint. (CSS Grid
// Level 3's `display: grid-lanes` packs without the server, but only
// Safari 26.4 has it; Chrome and Firefox keep it behind a flag, so the
// page does not lean on it.)
func statsLanes(lists []statsList) [][]statsList {
	tall := make([]int, len(lists))
	all := 0
	for i, l := range lists {
		tall[i] = statsWinChrome + statsWinRow*len(l.Rows) + statsWinGap
		if len(l.Rows) == 0 {
			tall[i] += statsWinNone
		}
		all += tall[i]
	}
	cut, best, left := len(lists), all+1, 0
	for k := 1; k < len(lists); k++ { // both lanes hold a window when there are two
		left += tall[k-1]
		apart := left - (all - left)
		if apart < 0 {
			apart = -apart
		}
		if apart <= best {
			cut, best = k, apart
		}
	}
	return [][]statsList{lists[:cut], lists[cut:]}
}

// statsListViews are each window's views, in strip order.
var statsListViews = []struct {
	param, title string
	views        []struct{ key, label string }
}{
	{"src", "Sources", []struct{ key, label string }{{"sources", "Sources"}, {"channels", "Channels"}, {"campaigns", "Campaigns"}}},
	{"pg", "Pages", []struct{ key, label string }{{"pages", "Top"}, {"entry", "Entry"}, {"exit", "Exit"}}},
	{"loc", "Locations", []struct{ key, label string }{{"countries", "Countries"}, {"regions", "Regions"}, {"cities", "Cities"}, {"languages", "Languages"}}},
	{"dev", "Devices", []struct{ key, label string }{{"devices", "Device"}, {"browsers", "Browser"}, {"oses", "OS"}}},
	{"bt", "Bots", []struct{ key, label string }{{"crawlers", "Crawlers"}, {"botpages", "Pages"}}},
}

// statsQuery is the page's URL state: the range, each window's view and
// the filters; withParam and withoutParam derive a neighbouring view's
// URL from it.
type statsQuery struct{ v url.Values }

func statsQueryOf(q url.Values) statsQuery {
	keep := url.Values{}
	for _, k := range []string{"range", "src", "pg", "loc", "dev", "bt"} {
		if v := q.Get(k); v != "" {
			keep.Set(k, v)
		}
	}
	for _, d := range statsFilters {
		if v := q.Get(d.param); v != "" {
			keep.Set(d.param, v)
		}
	}
	return statsQuery{keep}
}

func (q statsQuery) with(k, v string) string {
	c := url.Values{}
	for key, vals := range q.v {
		c[key] = vals
	}
	if v == "" {
		c.Del(k)
	} else {
		c.Set(k, v)
	}
	if len(c) == 0 {
		return "/stats"
	}
	return "/stats?" + c.Encode()
}

func (q statsQuery) String() string { return q.with("", "") }

// withBot is with, over the crawlers' hits: a click in the Bots window
// holds the view to bots (all of them, unless one is held already).
func (q statsQuery) withBot(k, v string) string {
	if q.v.Get("bot") != "" {
		return q.with(k, v)
	}
	c := statsQuery{url.Values{}}
	for key, vals := range q.v {
		c.v[key] = vals
	}
	c.v.Set("bot", "all")
	return c.with(k, v)
}

// handleStatsPage renders the window.
func (s *Server) handleStatsPage(w http.ResponseWriter, r *http.Request) {
	if s.Stats == nil {
		s.webError(w, r, http.StatusNotFound, "No stats on this hub.")
		return
	}
	q := r.URL.Query()
	now := time.Now()
	sp := statsSpanOf(q.Get("range"), now, s.Stats.Location())
	f := statsFilterOf(q)
	sq := statsQueryOf(q)
	// each window's view, the first when the query names none or a stranger
	var want []string
	views := map[string]string{}
	for _, lv := range statsListViews {
		v := q.Get(lv.param)
		ok := false
		for _, o := range lv.views {
			ok = ok || o.key == v
		}
		if !ok {
			v = lv.views[0].key
		}
		views[lv.param] = v
		want = append(want, v)
	}
	rep, err := s.statsCached(sp, f, want, now)
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "The stats could not be read.")
		return
	}
	p := &statsPage{Report: rep, Default: statsDefaultRange, Online: rep.Live.Online, Recent: rep.Live.Recent, Zone: rep.Zone,
		ZoneAbbr: now.In(s.Stats.Location()).Format("MST")}
	p.JSON = strings.Replace(sq.String(), "/stats", "/v1/stats", 1)
	for _, rg := range statsRanges {
		p.Ranges = append(p.Ranges, statsView{Key: rg.key, Label: rg.label, URL: sq.with("range", rg.key), On: rg.key == sp.Key})
	}
	for _, d := range statsFilters {
		if v := statsValue(f, d.param); v != "" {
			label := v
			if d.param == "country" {
				label = stats.CountryName(v)
			}
			if d.param == "bot" && v == "all" {
				label = "all crawlers"
			}
			p.Filters = append(p.Filters, statsChip{Label: d.label, Value: label, URL: sq.with(d.param, "")})
		}
	}
	p.Tiles = statsTiles(rep.Summary, rep.Previous)
	p.Chart = statsChartOf(rep.Series, sp)
	var lists []statsList
	for _, lv := range statsListViews {
		l := statsList{Title: lv.title, Anchor: "w-" + lv.param}
		for _, o := range lv.views {
			l.Views = append(l.Views, statsView{Key: o.key, Label: o.label, URL: sq.with(lv.param, o.key), On: o.key == views[lv.param]})
		}
		view := views[lv.param]
		l.Count = statsLists[view].count
		rows := rep.Lists[view]
		if len(rows) > statsShown {
			l.More = len(rows) - statsShown
			rows = rows[:statsShown]
		}
		top := 1
		if len(rows) > 0 {
			top = max(1, rows[0].N)
		}
		for _, row := range rows {
			l.Rows = append(l.Rows, s.statsRowView(view, row, top, sq))
		}
		if len(rows) == 0 {
			l.Empty = "Nothing yet."
			if view == "campaigns" {
				l.Empty = "No tagged links yet: a link with utm_campaign shows here."
			}
			if view == "regions" || view == "cities" {
				l.Empty = "Not known: the proxy in front sends no region or city."
			}
			if view == "crawlers" || view == "botpages" {
				l.Empty = "No crawler yet."
			}
		}
		if lv.param == "bt" {
			l.Bots = true
			if f.Bot == "" {
				l.OnlyBots = sq.with("bot", "all") + "#w-bt"
			}
		}
		lists = append(lists, l)
	}
	p.Lanes = statsLanes(lists)
	for i := range p.Recent {
		p.Recent[i].Label = s.statsPathLabel(p.Recent[i].Path)
	}
	p.Span = statsSpanWords(sp)
	d := &webData{Page: "stats", Title: "Stats · " + r.Host, Desc: "who reads " + r.Host + ": pages, sources, locations, devices", StatsPage: p,
		Image: webBase(r) + "/apple-touch-icon.png"}
	w.Header().Set("Cache-Control", "no-cache")
	s.webRender(w, r, http.StatusOK, d)
}

// handleStats is the report as JSON: every list, the series and who is
// here now, under the same query as the page.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.Stats == nil {
		writeErr(w, http.StatusNotFound, errors.New("no stats on this hub"))
		return
	}
	q := r.URL.Query()
	now := time.Now()
	sp := statsSpanOf(q.Get("range"), now, s.Stats.Location())
	rep, err := s.statsCached(sp, statsFilterOf(q), nil, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, rep)
}

// statsCache keeps a computed report for a few seconds under its query,
// so the page's refetches and a busy moment cost the store one pass.
const statsCacheTTL = 10 * time.Second

type statsCacheEntry struct {
	at  time.Time
	rep *statsReport
}

func (s *Server) statsCached(sp statsSpan, f store.StatsFilter, lists []string, now time.Time) (*statsReport, error) {
	key := fmt.Sprintf("%s|%v|%v", sp.Key, f, lists)
	s.statsMu.Lock()
	if e, ok := s.statsCache[key]; ok && now.Sub(e.at) < statsCacheTTL {
		s.statsMu.Unlock()
		return e.rep, nil
	}
	s.statsMu.Unlock()
	rep, err := s.statsBuild(sp, f, lists, now)
	if err != nil {
		return nil, err
	}
	s.statsMu.Lock()
	if s.statsCache == nil || len(s.statsCache) > 64 {
		s.statsCache = map[string]statsCacheEntry{}
	}
	s.statsCache[key] = statsCacheEntry{at: now, rep: rep}
	s.statsMu.Unlock()
	return rep, nil
}

func statsTiles(cur, prev store.StatsSummary) []statsTile {
	pct := func(a, b int) string {
		if b == 0 {
			return ""
		}
		d := float64(a-b) / float64(b) * 100
		return statsSigned(d, "%")
	}
	bounce := func(s store.StatsSummary) float64 {
		if s.Sessions == 0 {
			return 0
		}
		return float64(s.Bounces) / float64(s.Sessions) * 100
	}
	tiles := []statsTile{
		{"Visitors", statsNum(cur.Visitors), pct(cur.Visitors, prev.Visitors)},
		{"Page views", statsNum(cur.Pageviews), pct(cur.Pageviews, prev.Pageviews)},
		{"Sessions", statsNum(cur.Sessions), pct(cur.Sessions, prev.Sessions)},
		{"Bounce rate", fmt.Sprintf("%.0f%%", bounce(cur)), ""},
		{"Session time", statsDur(cur.Duration), ""},
	}
	if prev.Sessions > 0 && cur.Sessions > 0 {
		tiles[3].Delta = statsSigned(bounce(cur)-bounce(prev), " pp")
	}
	if prev.Duration > 0 && cur.Sessions > 0 {
		tiles[4].Delta = statsSigned((cur.Duration-prev.Duration)/prev.Duration*100, "%")
	}
	return tiles
}

func statsSigned(d float64, unit string) string {
	switch {
	case d > 0.05:
		return fmt.Sprintf("+%.1f%s", d, unit)
	case d < -0.05:
		return fmt.Sprintf("−%.1f%s", -d, unit)
	}
	return "±0" + unit
}

// statsNum prints a count with thousands separators.
func statsNum(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func statsDur(sec float64) string {
	s := int(sec + 0.5)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}

// statsCeil is the chart's top: the smallest of a set of round numbers,
// each divisible by four so the quarter lines label whole numbers.
func statsCeil(m int) int {
	if m <= 4 {
		return 4
	}
	for k := 1; k < 1e9; k *= 10 {
		for _, b := range []int{4, 8, 12, 16, 20, 32, 40, 60, 80} {
			if b*k >= m {
				return b * k
			}
		}
	}
	return m
}

func statsChartOf(pts []statsPoint, sp statsSpan) statsChart {
	var c statsChart
	m := 0
	for _, p := range pts {
		m = max(m, p.Pageviews, p.Visitors)
	}
	c.Empty = m == 0
	c.Max = statsCeil(m)
	for i := 4; i >= 0; i-- {
		c.Y = append(c.Y, statsNum(c.Max*i/4))
	}
	n := len(pts)
	x := func(i int) float64 {
		if n <= 1 {
			return 0
		}
		return float64(i) * 1000 / float64(n-1)
	}
	y := func(v int) float64 { return 1000 - float64(v)*1000/float64(c.Max) }
	// the points up to the last with data (the buckets after now stay empty)
	last := -1
	for i, p := range pts {
		if !p.Future {
			last = i
		}
	}
	end := last
	if sp.IncompleteLast && last > 0 {
		end = last - 1 // the filling bucket hangs off the line, lighter
	}
	var vw, vi strings.Builder
	for i := 0; i <= end; i++ {
		fmt.Fprintf(&vw, "%.1f,%.1f ", x(i), y(pts[i].Pageviews))
		fmt.Fprintf(&vi, "%.1f,%.1f ", x(i), y(pts[i].Visitors))
	}
	c.Views, c.Visitors = strings.TrimSpace(vw.String()), strings.TrimSpace(vi.String())
	if end >= 0 && last > end {
		c.ViewsEnd = fmt.Sprintf("%.1f,%.1f %.1f,%.1f", x(end), y(pts[end].Pageviews), x(last), y(pts[last].Pageviews))
		c.VisEnd = fmt.Sprintf("%.1f,%.1f %.1f,%.1f", x(end), y(pts[end].Visitors), x(last), y(pts[last].Visitors))
	}
	for i, p := range pts {
		if p.Label == "" {
			continue
		}
		l := statsXLabel{Label: p.Label, Pct: x(i) / 10}
		if i == 0 {
			l.Edge = "first"
		} else if i == n-1 {
			l.Edge = "last"
		}
		c.X = append(c.X, l)
	}
	return c
}

// statsRowView draws one row: the label (a country by name with its
// code, a thread by its words, a profile by its name), the click that
// filters the view by it, and the bar as a share of the top row.
func (s *Server) statsRowView(view string, row store.StatsRow, top int, sq statsQuery) statsRowView {
	v := statsRowView{Label: row.Key, N: statsNum(row.N), Pct: row.N * 100 / top}
	param := ""
	switch view {
	case "sources", "channels":
		if row.Key == "" {
			v.Label = "Direct"
			if view == "channels" {
				v.Label = "direct"
			}
		}
		param = "source"
		if view == "channels" {
			param = "channel"
			if row.Key != "" {
				v.Label = row.Key
			}
		}
		if row.Key == "" && view == "channels" {
			param = "channel"
			v.URL = sq.with(param, "direct")
		}
	case "campaigns":
		param = "campaign"
	case "pages", "entry", "exit":
		param = "page"
		v.Title = row.Key
		v.Label = s.statsPathLabel(row.Key)
	case "countries":
		param = "country"
		v.Label, v.Tag = stats.CountryName(row.Key), row.Key
	case "regions":
		param = "region"
	case "cities":
		param = "city"
	case "languages":
		param = "lang"
	case "devices":
		param = "device"
	case "browsers":
		param = "browser"
	case "oses":
		param = "os"
	case "crawlers":
		v.URL = sq.with("bot", row.Key) // that crawler alone, wherever the view was
	case "botpages":
		v.Title = row.Key
		v.Label = s.statsPathLabel(row.Key)
		v.URL = sq.withBot("page", row.Key)
	}
	if v.URL == "" && param != "" {
		v.URL = sq.with(param, row.Key)
	}
	return v
}

// statsPathLabel names a page: a thread by its author and first words,
// a profile by its name, the feed and the rest by their path.
func (s *Server) statsPathLabel(path string) string {
	switch {
	case path == "/":
		return "/ (the feed)"
	case strings.HasPrefix(path, "/p/"):
		if p, err := s.St.Post(strings.TrimPrefix(path, "/p/")); err == nil {
			return authorLabel(*p) + ": " + excerpt(named(*p), 48)
		}
	case strings.HasPrefix(path, "/u/"):
		if pr, err := s.St.Profile(strings.TrimPrefix(path, "/u/")); err == nil && pr.Name != "" {
			return "Profile: " + pr.Name
		}
	}
	return path
}

// statsSpanWords is the chart's status line: the span's dates and what
// the tiles compare against.
func statsSpanWords(sp statsSpan) string {
	loc := sp.From.Location()
	last := sp.To.Add(-time.Millisecond).In(loc)
	from := sp.From.In(loc)
	var span string
	switch sp.Key {
	case "today", "yesterday":
		span = from.Format("Jan 2, 2006")
	case "24h":
		span = from.Format("Jan 2 15:04") + " – " + last.Format("Jan 2 15:04")
	default:
		span = from.Format("Jan 2") + " – " + last.Format("Jan 2, 2006")
	}
	return span + " \u00b7 change vs " + sp.Compare
}
