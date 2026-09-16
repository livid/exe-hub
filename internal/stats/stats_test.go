package stats

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"exehub/internal/store"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		ua                  string
		device, browser, os string
		bot                 bool
	}{
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 Edg/128.0.0.0", "desktop", "Edge", "Windows", false},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15", "desktop", "Safari", "macOS", false},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1", "mobile", "Safari", "iOS", false},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 MicroMessenger/8.0.49", "mobile", "WeChat", "iOS", false},
		{"Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/126.0 Mobile/15E148 Safari/604.1", "tablet", "Chrome", "iOS", false},
		{"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36", "mobile", "Chrome", "Android", false},
		{"Mozilla/5.0 (Linux; Android 13; SM-X710) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/24.0 Chrome/117.0.0.0 Safari/537.36", "tablet", "Samsung Internet", "Android", false},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0", "desktop", "Firefox", "Linux", false},
		{"Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36", "desktop", "Chrome", "ChromeOS", false},
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "", "", "", true},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/126.0.0.0 Safari/537.36", "", "", "", true},
		{"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)", "", "", "", true},
		{"curl/8.5.0", "agent", "curl", "", false},
		{"Go-http-client/2.0", "agent", "Go", "", false},
		{"python-requests/2.32.3", "agent", "Python", "", false},
		{"Claude-User/1.0", "agent", "Claude", "", false},
		{"", "agent", "", "", false},
	} {
		d, b, o, bot := Classify(c.ua)
		if d != c.device || b != c.browser || o != c.os || bot != c.bot {
			t.Errorf("%q = %s/%s/%s/%v, want %s/%s/%s/%v", c.ua, d, b, o, bot, c.device, c.browser, c.os, c.bot)
		}
	}
}

func TestSource(t *testing.T) {
	for _, c := range []struct{ ref, name, channel string }{
		{"", "", "direct"},
		{"https://hub.example/p/abc", "", "direct"},
		{"https://www.google.com/", "Google", "search"},
		{"https://www.google.co.uk/search?q=exe", "Google", "search"},
		{"https://yandex.ru/", "Yandex", "search"},
		{"https://t.co/abc", "X", "social"},
		{"https://news.ycombinator.com/item?id=1", "Hacker News", "social"},
		{"https://www.v2ex.com/t/1", "V2EX", "social"},
		{"https://chatgpt.com/", "ChatGPT", "ai"},
		{"https://m.facebook.com/", "Facebook", "social"},
		{"https://blog.example.org/post", "blog.example.org", "referral"},
		{"android-app://com.google.android.googlequicksearchbox/", "Google", "search"},
		{"android-app://com.example.app/", "com.example.app", "referral"},
		{"not a url", "", "direct"},
	} {
		n, ch := Source(c.ref, "hub.example:7788")
		if n != c.name || ch != c.channel {
			t.Errorf("Source(%q) = %q/%q, want %q/%q", c.ref, n, ch, c.name, c.channel)
		}
	}
}

func TestWanted(t *testing.T) {
	req := func(method string, hdr ...string) *httptest.ResponseRecorder {
		return nil
	}
	_ = req
	mk := func(method string, hdr ...string) bool {
		r := httptest.NewRequest(method, "http://hub.example/", nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return Wanted(r)
	}
	if !mk("GET", "Sec-Fetch-Dest", "document", "Accept", "text/html") {
		t.Error("a navigation is wanted")
	}
	if mk("GET", "Sec-Fetch-Dest", "empty", "Accept", "text/html") {
		t.Error("a fetch() is not a page view")
	}
	if mk("GET", "Sec-Fetch-Dest", "document", "X-Hub-Live", "1") {
		t.Error("the page's own refetch is not a page view")
	}
	if mk("GET", "Sec-Fetch-Dest", "document", "Sec-Purpose", "prefetch") {
		t.Error("a prefetch is not a page view")
	}
	if !mk("GET", "Accept", "text/html,application/xhtml+xml") {
		t.Error("an older browser asking for HTML is wanted")
	}
	if mk("GET", "Accept", "*/*") {
		t.Error("curl's */* is not a page view")
	}
	if mk("HEAD", "Sec-Fetch-Dest", "document") || mk("POST", "Sec-Fetch-Dest", "document") {
		t.Error("only GET")
	}
	r := httptest.NewRequest("GET", "http://hub.example/skill.md", nil)
	r.Header.Set("Accept", "*/*")
	if !WantedRead(r) {
		t.Error("a curl of skill.md is a read")
	}
	r.Header.Set("X-Hub-Live", "1")
	if WantedRead(r) {
		t.Error("a refetch is not a read")
	}
}

func TestLanguage(t *testing.T) {
	for _, c := range []struct{ accept, want string }{
		{"zh-CN,zh;q=0.9,en;q=0.8", "zh-CN"},
		{"en-US,en;q=0.5", "en-US"},
		{"en", "en"},
		{"zh-Hant-TW", "zh-Hant"},
		{"fr;q=0.8, ja;q=0.9", "ja"},
		{"*", ""},
		{"", ""},
	} {
		if got := Language(c.accept); got != c.want {
			t.Errorf("Language(%q) = %q, want %q", c.accept, got, c.want)
		}
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "http://hub.example/", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	if ip := ClientIP(r); ip != "10.0.0.2" {
		t.Errorf("connection: %q", ip)
	}
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	if ip := ClientIP(r); ip != "10.0.0.2" {
		t.Errorf("a loopback forward is not the client: %q", ip)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if ip := ClientIP(r); ip != "203.0.113.9" {
		t.Errorf("first forwarded: %q", ip)
	}
	r.Header.Set("CF-Connecting-IP", "198.51.100.7")
	if ip := ClientIP(r); ip != "198.51.100.7" {
		t.Errorf("cloudflare first: %q", ip)
	}
}

// TestCollector: a visitor's id is the same within the day and another
// the next; a second page inside the gap shares the session and takes
// its source, one past the gap starts a new session; a crawler is
// dropped; the batch lands in the store.
func TestCollector(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, err := New(st, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	c.Log = func(string, ...any) {}
	day1 := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	a := c.visitor(day1, "203.0.113.9", "Mozilla/5.0 Chrome/1", "hub.example")
	if b := c.visitor(day1.Add(time.Hour), "203.0.113.9", "Mozilla/5.0 Chrome/1", "hub.example"); b != a {
		t.Error("the same person within the day is one visitor")
	}
	if b := c.visitor(day1, "203.0.113.10", "Mozilla/5.0 Chrome/1", "hub.example"); b == a {
		t.Error("another address is another visitor")
	}
	if b := c.visitor(day1.AddDate(0, 0, 1), "203.0.113.9", "Mozilla/5.0 Chrome/1", "hub.example"); b == a {
		t.Error("the next day is another visitor")
	}
	if len(a) != 24 {
		t.Errorf("id length %d", len(a))
	}

	h1 := c.assign(store.Hit{TS: 1000, VID: "v1", Path: "/", Ref: "Google", Channel: "search"})
	h2 := c.assign(store.Hit{TS: 1000 + 5*60*1000, VID: "v1", Path: "/p/x", Ref: "", Channel: "direct"})
	h3 := c.assign(store.Hit{TS: 1000 + 40*60*1000, VID: "v1", Path: "/", Ref: "", Channel: "direct"})
	if !h1.Entry || h2.Entry || !h3.Entry {
		t.Errorf("entries: %v %v %v", h1.Entry, h2.Entry, h3.Entry)
	}
	if h1.SID == "" || h1.SID != h2.SID || h3.SID == h1.SID {
		t.Errorf("sessions: %q %q %q", h1.SID, h2.SID, h3.SID)
	}
	if h2.Ref != "Google" || h2.Channel != "search" {
		t.Errorf("the session's source is its first page's: %q/%q", h2.Ref, h2.Channel)
	}
	if h3.Ref != "" || h3.Channel != "direct" {
		t.Errorf("a new session has its own source: %q/%q", h3.Ref, h3.Channel)
	}

	// through Record: a browser lands, a crawler does not
	r := httptest.NewRequest("GET", "http://hub.example/p/abc?utm_source=newsletter&utm_campaign=sept", nil)
	r.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0) Chrome/128.0 Safari/537.36")
	r.Header.Set("CF-IPCountry", "de")
	r.Header.Set("Accept-Language", "de-DE,de;q=0.9")
	c.Record(r, "thread")
	g := httptest.NewRequest("GET", "http://hub.example/", nil)
	g.Header.Set("User-Agent", "Googlebot/2.1")
	c.Record(g, "home")
	if len(c.ch) != 1 {
		t.Fatalf("queued %d, want 1", len(c.ch))
	}
	h := c.assign(<-c.ch)
	if h.Country != "DE" || h.Lang != "de-DE" || h.Device != "desktop" || h.Browser != "Chrome" || h.OS != "Windows" {
		t.Errorf("hit: %+v", h)
	}
	if h.Ref != "newsletter" || h.Channel != "campaign" || h.UTMCampaign != "sept" {
		t.Errorf("a tagged link names its source: %+v", h)
	}
	if h.Path != "/p/abc" {
		t.Errorf("the path without its query: %q", h.Path)
	}
	if err := st.StatsAdd([]store.Hit{h1, h2, h3, h}); err != nil {
		t.Fatal(err)
	}
	n, err := st.StatsOnline(0)
	if err != nil || n != 2 {
		t.Errorf("online = %d, %v", n, err)
	}
	// a restart resumes the open sessions
	c2, err := New(st, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if s := c2.sessions[h.VID]; s == nil || s.id != h.SID || s.ref != "newsletter" {
		t.Errorf("open session not resumed: %+v", s)
	}
}

func TestAlias(t *testing.T) {
	if a := Alias("00ff"); a != "Amber Lynx" { // 0 → Amber, 255 % 40 = 15 → Lynx
		t.Errorf("Alias = %q", a)
	}
	if a := Alias("zz"); a != "Visitor" {
		t.Errorf("bad id: %q", a)
	}
}
