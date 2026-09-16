// Package stats is the hub's own analytics for its public pages (PLAN.md,
// Stats): which pages a browser opened, where the visitor came from, from
// what country and on what device — counted on the server as a page is
// served, with no script and no cookie. A visitor is the day's hash of a
// secret salt, the address and the browser, which rotates every day and
// cannot be turned back into a person; the address and the user agent
// themselves are never stored. Sessions are assigned here as hits arrive
// (a new one after 30 quiet minutes), so every query is a plain GROUP BY.
package stats

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"exehub/internal/store"
)

// SessionGap is the quiet time that ends a session: the next page after
// it starts a new one, as every analytics tool has it.
const SessionGap = 30 * time.Minute

// OnlineWindow is how recent a hit must be for its visitor to count as
// online now.
const OnlineWindow = 5 * time.Minute

// queueSize bounds the hits waiting for the writer; past it a hit is
// dropped rather than a request delayed.
const queueSize = 1024

// Collector turns requests into hits and writes them in batches off the
// request path.
type Collector struct {
	st  *store.Store
	loc *time.Location
	ch  chan store.Hit

	mu   sync.Mutex // the day's salt
	day  string
	salt []byte

	sessions map[string]*session // by visitor; the writer goroutine's own

	Log func(format string, args ...any)
}

type session struct {
	id      string
	last    int64
	ref     string
	channel string
	utmSrc  string
	utmMed  string
	utmCamp string
}

// New makes a collector whose days begin at midnight in loc (the salt
// rotates then, and the day buckets of the page follow it). Sessions
// still open in the store are picked up, so a restart splits none.
func New(st *store.Store, loc *time.Location) (*Collector, error) {
	c := &Collector{st: st, loc: loc, ch: make(chan store.Hit, queueSize), sessions: map[string]*session{}, Log: log.Printf}
	open, err := st.StatsOpenSessions(time.Now().Add(-SessionGap).UnixMilli())
	if err != nil {
		return nil, err
	}
	for _, o := range open {
		c.sessions[o.VID] = &session{id: o.SID, last: o.Last, ref: o.Ref, channel: o.Channel,
			utmSrc: o.UTMSource, utmMed: o.UTMMedium, utmCamp: o.UTMCampaign}
	}
	return c, nil
}

// Location is where the collector's days begin.
func (c *Collector) Location() *time.Location { return c.loc }

// Run is the writer: hits from the queue get their session and land in
// the store in batches, once a second or every 64 hits.
func (c *Collector) Run() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var batch []store.Hit
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := c.st.StatsAdd(batch); err != nil {
			c.Log("stats: %v", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case h := <-c.ch:
			batch = append(batch, c.assign(h))
			if len(batch) >= 64 {
				flush()
			}
		case <-tick.C:
			flush()
			c.prune()
		}
	}
}

// assign gives a hit its session: the visitor's open one, or a new one
// when there is none or it has been quiet past the gap. The session's
// source is its first page's, copied onto every later hit, so a filter
// by source takes the whole visit.
func (c *Collector) assign(h store.Hit) store.Hit {
	s := c.sessions[h.VID]
	if s == nil || h.TS-s.last > SessionGap.Milliseconds() {
		s = &session{id: newID(), ref: h.Ref, channel: h.Channel, utmSrc: h.UTMSource, utmMed: h.UTMMedium, utmCamp: h.UTMCampaign}
		c.sessions[h.VID] = s
		h.Entry = true
	} else {
		h.Ref, h.Channel = s.ref, s.channel
		h.UTMSource, h.UTMMedium, h.UTMCampaign = s.utmSrc, s.utmMed, s.utmCamp
	}
	if h.TS > s.last {
		s.last = h.TS
	}
	h.SID = s.id
	return h
}

// TakeForTest takes one queued hit through the session step, as Run
// would; false when the queue is empty. For tests.
func (c *Collector) TakeForTest() (store.Hit, bool) {
	select {
	case h := <-c.ch:
		return c.assign(h), true
	default:
		return store.Hit{}, false
	}
}

func (c *Collector) prune() {
	cut := time.Now().Add(-SessionGap).UnixMilli()
	for vid, s := range c.sessions {
		if s.last < cut {
			delete(c.sessions, vid)
		}
	}
}

func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Record queues one page view for the request, once the page was served:
// the caller has decided (Wanted) that the request was a person opening
// a page. A crawler's request is dropped here, by its user agent.
func (c *Collector) Record(r *http.Request, kind string) {
	ua := r.UserAgent()
	device, browser, os, bot := Classify(ua)
	now := time.Now()
	h := store.Hit{
		TS: now.UnixMilli(), Path: clip(r.URL.Path, 256), Kind: kind,
		Device: device, Browser: browser, OS: os, Bot: bot,
		Country: country(r.Header.Get("CF-IPCountry")),
		Region:  clip(r.Header.Get("CF-Region"), 64),
		City:    clip(r.Header.Get("CF-IPCity"), 64),
		Lang:    Language(r.Header.Get("Accept-Language")),
	}
	h.VID = c.visitor(now, ClientIP(r), ua, r.Host)
	q := r.URL.Query()
	h.UTMSource, h.UTMMedium, h.UTMCampaign = clip(q.Get("utm_source"), 64), clip(q.Get("utm_medium"), 64), clip(q.Get("utm_campaign"), 64)
	h.Ref, h.Channel = Source(r.Referer(), r.Host)
	if h.Ref == "" && h.UTMSource != "" {
		// a tagged link: the campaign names the source, wherever the click came from
		h.Ref, h.Channel = h.UTMSource, "campaign"
	}
	select {
	case c.ch <- h:
	default: // a burst past the queue: the request is not delayed, the hit is lost
	}
}

// visitor is the day's hash of the salt, the hub, the address and the
// browser: the same person on the same day is one visitor; tomorrow the
// salt is new and yesterday's ids match nothing. The salt is random,
// kept in the store for the day (so a restart keeps the day whole) and
// then deleted.
func (c *Collector) visitor(now time.Time, ip, ua, host string) string {
	day := now.In(c.loc).Format("2006-01-02")
	c.mu.Lock()
	if day != c.day {
		salt, err := c.st.StatsSalt(day)
		if err != nil {
			// no store: a salt for this process alone, still rotating daily
			salt = make([]byte, 16)
			rand.Read(salt)
			c.Log("stats: salt: %v", err)
		}
		c.day, c.salt = day, salt
	}
	salt := c.salt
	c.mu.Unlock()
	sum := sha256.New()
	sum.Write(salt)
	sum.Write([]byte("\n" + host + "\n" + ip + "\n" + ua))
	return hex.EncodeToString(sum.Sum(nil))[:24]
}

// Online is how many visitors had a page in the last five minutes.
func (c *Collector) Online() int {
	n, _ := c.st.StatsOnline(time.Now().Add(-OnlineWindow).UnixMilli())
	return n
}

// Wanted says whether a served page counts: a GET that is not one of
// the pages' own refetches (the live feed and the stats page fetch
// themselves with X-Hub-Live) nor a prefetch or a preview; then the
// skill guide counts every read, whatever the client accepts, since an
// agent reads it with curl or fetch; a crawler's GET counts as a crawl,
// whatever it accepts; and a page counts for a person when a browser
// made a navigation of it (Sec-Fetch-Dest: document) — a client that
// says nothing of Sec-Fetch, an older browser or a tool, is taken at
// its Accept: HTML asked for is a page view, curl's */* is not.
func Wanted(r *http.Request, kind string) bool {
	if r.Method != http.MethodGet || r.Header.Get("X-Hub-Live") != "" {
		return false
	}
	purpose := strings.ToLower(r.Header.Get("Sec-Purpose") + " " + r.Header.Get("Purpose"))
	if strings.Contains(purpose, "prefetch") || strings.Contains(purpose, "prerender") || strings.Contains(purpose, "preview") {
		return false
	}
	if kind == "skill" {
		return true
	}
	if _, _, _, bot := Classify(r.UserAgent()); bot {
		return true
	}
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" {
		return d == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// ClientIP is the visitor's address as far as the hub can tell: what
// Cloudflare says in front of the public hub, else the nearest proxy's
// X-Forwarded-For when it names a public address, else the connection.
// It is hashed with the day's salt and never stored.
func ClientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil && !ip.IsLoopback() && !ip.IsPrivate() {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// country normalises Cloudflare's CF-IPCountry: an ISO 3166-1 alpha-2
// code upper-cased, T1 (Tor) kept as a code of its own, XX (unknown)
// and anything that is not two letters dropped.
func country(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 2 || code == "XX" {
		return ""
	}
	for _, r := range code {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return code
}

// Language is the browser's first language as a tag ("zh-CN", "en"):
// the Accept-Language entry with the highest q, the first when tied,
// lower-case language and upper-case region, other subtags dropped.
func Language(accept string) string {
	best, bestQ := "", 0.0
	for _, part := range strings.Split(accept, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		tag = strings.TrimSpace(tag)
		if tag == "" || tag == "*" {
			continue
		}
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				var f float64
				if _, err := fmtSscan(strings.TrimSpace(v), &f); err == nil {
					q = f
				}
			}
		}
		if q > bestQ {
			best, bestQ = tag, q
		}
	}
	if best == "" {
		return ""
	}
	parts := strings.Split(best, "-")
	lang := strings.ToLower(parts[0])
	if len(lang) < 2 || len(lang) > 3 {
		return ""
	}
	for _, p := range parts[1:] {
		if len(p) == 2 {
			return lang + "-" + strings.ToUpper(p)
		}
		if len(p) == 4 { // a script: zh-Hant
			return lang + "-" + strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
		}
	}
	return lang
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}
