package card

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func pageHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, body)
	})
}
