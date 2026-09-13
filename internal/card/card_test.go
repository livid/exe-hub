package card

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestFirst(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{"see https://a.example/x.", "https://a.example/x"},
		{"详见https://x.y的说明", "https://x.y"},
		{"two https://a.example and https://b.example", "https://a.example"},
		{"no link here", ""},
		{"(https://a.example/p?q=1)", "https://a.example/p?q=1"},
	} {
		if got := First(c.text); got != c.want {
			t.Errorf("First(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestParseMeta(t *testing.T) {
	m := parseMeta(`<html><head>
<title>Fallback &amp; Title</title>
<meta content="OG  Title" property="og:title">
<meta property='og:description' content='One line&#39;s worth'/>
<meta name="twitter:image" content="/wrong.png">
<meta property="og:image" content="/art.png">
</head></html>`)
	if m.Title != "OG Title" {
		t.Errorf("title %q", m.Title) // reversed attribute order, whitespace collapsed
	}
	if m.Desc != "One line's worth" {
		t.Errorf("desc %q", m.Desc)
	}
	if m.ImageURL != "/art.png" {
		t.Errorf("image %q — og:image outranks twitter:image", m.ImageURL)
	}
	if m := parseMeta(`<title>Just A Title</title>`); m.Title != "Just A Title" {
		t.Errorf("title fallback: %q", m.Title)
	}
}

// TestFetch drives the whole page path against a local server (the guard
// lifted for the loopback address), relative image resolution included.
func TestFetch(t *testing.T) {
	pageSrv := httptest.NewServer(pageHandler(`<head><meta property="og:title" content="A Page">
<meta property="og:image" content="/pic.png"></head>`))
	defer pageSrv.Close()

	f := NewFetcher()
	f.AllowPrivate = true
	m, err := f.Fetch(context.Background(), pageSrv.URL+"/x")
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "A Page" || m.ImageURL != pageSrv.URL+"/pic.png" {
		t.Errorf("got %+v", m)
	}
	if m.Host == "" || strings.Contains(m.Host, "www.") {
		t.Errorf("host %q", m.Host)
	}
}

// TestGuard: without AllowPrivate the same loopback fetch is refused at
// the dial, so a post can never point the hub at its own network.
func TestGuard(t *testing.T) {
	srv := httptest.NewServer(pageHandler(`<title>secret</title>`))
	defer srv.Close()
	f := NewFetcher()
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("a loopback fetch got through the guard")
	}
}

func TestFetchImageSniff(t *testing.T) {
	srv := httptest.NewServer(pageHandler("not a picture at all"))
	defer srv.Close()
	f := NewFetcher()
	f.AllowPrivate = true
	if _, _, err := f.FetchImage(context.Background(), srv.URL); err == nil {
		t.Fatal("non-image bytes accepted as a card picture")
	}
}

// TestDecode: the charset comes from the header, else the page's own
// declaration; undeclared pages read as UTF-8, and stray bytes never
// survive into a card.
func TestDecode(t *testing.T) {
	sjis, _ := japanese.ShiftJIS.NewEncoder().String(`<title>マメフルードフィルター【コラボ】</title>
<meta http-equiv="Content-Type" content="text/html; charset=Shift_JIS">`)
	gbk, _ := simplifiedchinese.GBK.NewEncoder().String(`<title>流动过滤器</title>`)
	for _, c := range []struct{ name, body, ct, want string }{
		{"http-equiv Shift_JIS, bare header", sjis, "text/html", "マメフルードフィルター【コラボ】"},
		{"header charset", gbk, "text/html; charset=GBK", "流动过滤器"},
		{"quoted header charset", gbk, `text/html; charset="gb2312"`, "流动过滤器"},
		{"meta charset", `<meta charset="utf-8"><title>日本語</title>`, "", "日本語"},
		{"undeclared UTF-8", `<title>Café</title>`, "text/html", "Café"},
		{"undeclared stray bytes dropped", "<title>A\xff\xfeB</title>", "text/html", "AB"},
		{"unknown label", "<meta charset=x-klingon><title>Q\x80</title>", "", "Q"},
	} {
		m := parseMeta(decode([]byte(c.body), c.ct))
		if m.Title != c.want {
			t.Errorf("%s: title %q, want %q", c.name, m.Title, c.want)
		}
		if Misread(m.Title, m.Desc) {
			t.Errorf("%s: decoded card still misread", c.name)
		}
	}
	if !Misread("\x83\x7d\x83\x81", "") {
		t.Error("raw Shift_JIS bytes not flagged as misread")
	}
}

// TestFetchShiftJIS: a page shaped like mame-design.jp's — Shift_JIS
// declared only in the page, served as bare text/html — unfurls legibly.
func TestFetchShiftJIS(t *testing.T) {
	page, _ := japanese.ShiftJIS.NewEncoder().String(`<html><head>
<title>マメフルードフィルター</title>
<meta name="description" content="最強の流動フィルター">
<meta http-equiv="Content-Type" content="text/html; charset=Shift_JIS">
</head></html>`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, page)
	}))
	defer srv.Close()
	f := NewFetcher()
	f.AllowPrivate = true
	m, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "マメフルードフィルター" || m.Desc != "最強の流動フィルター" {
		t.Errorf("got title %q desc %q", m.Title, m.Desc)
	}
}

func pageHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, body)
	})
}
