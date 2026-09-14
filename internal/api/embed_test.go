package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"exehub/internal/config"
	"exehub/internal/ipfs"
)

// fakeKubo serves one CID's bytes through cat, honouring offset and
// length the way kubo does, and counts the cats.
func fakeKubo(t *testing.T, cid string, data []byte) (*httptest.Server, *atomic.Int32) {
	var cats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/cat" || r.URL.Query().Get("arg") != cid {
			http.NotFound(w, r)
			return
		}
		cats.Add(1)
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		end := int64(len(data))
		if n, _ := strconv.ParseInt(r.URL.Query().Get("length"), 10, 64); n > 0 && off+n < end {
			end = off + n
		}
		w.Write(data[off:end])
	}))
	t.Cleanup(srv.Close)
	return srv, &cats
}

// TestEmbedRange: a player's byte spans come back as 206s, each one cat
// of just that span; HEAD and a revalidation cost no cat at all.
func TestEmbedRange(t *testing.T) {
	const cid = "bafkreiembedrangetest"
	data := []byte("0123456789abcdefghij")
	kubo, cats := fakeKubo(t, cid, data)
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	s.IPFS = ipfs.New(kubo.URL)
	if err := s.St.AddPin(cid, int64(len(data)), "video/mp4", false); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	do := func(method string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/embed/"+cid, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	w := do("GET")
	if w.Code != 200 || w.Body.String() != string(data) || w.Header().Get("Accept-Ranges") != "bytes" ||
		w.Header().Get("Content-Length") != "20" || w.Header().Get("Content-Disposition") != "inline" ||
		w.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("full GET: %d %q %v", w.Code, w.Body, w.Header())
	}
	for _, c := range []struct{ rng, body, cr string }{
		{"bytes=0-1", "01", "bytes 0-1/20"}, // Safari's first probe
		{"bytes=5-9", "56789", "bytes 5-9/20"},
		{"bytes=15-", "fghij", "bytes 15-19/20"},
		{"bytes=-3", "hij", "bytes 17-19/20"},
		{"bytes=18-99", "ij", "bytes 18-19/20"},
	} {
		before := cats.Load()
		w := do("GET", "Range", c.rng)
		if w.Code != 206 || w.Body.String() != c.body || w.Header().Get("Content-Range") != c.cr {
			t.Errorf("%s: %d %q %q", c.rng, w.Code, w.Body, w.Header().Get("Content-Range"))
		}
		if n := cats.Load() - before; n != 1 {
			t.Errorf("%s: %d cats, want 1", c.rng, n)
		}
	}
	if w := do("GET", "Range", "bytes=30-40"); w.Code != 416 || w.Header().Get("Content-Range") != "bytes */20" {
		t.Errorf("unsatisfiable range: %d %v", w.Code, w.Header())
	}
	before := cats.Load()
	if w := do("HEAD"); w.Code != 200 || w.Header().Get("Content-Length") != "20" || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d %v %q", w.Code, w.Header(), w.Body)
	}
	if w := do("GET", "If-None-Match", `"`+cid+`"`); w.Code != 304 {
		t.Errorf("revalidation: %d", w.Code)
	}
	if n := cats.Load() - before; n != 0 {
		t.Errorf("HEAD and a 304 cost %d cats", n)
	}
	if w := do("GET", "Range", "bytes=0-1,4-5"); w.Code != 206 || !strings.Contains(w.Header().Get("Content-Type"), "multipart/byteranges") {
		t.Errorf("two ranges: %d %v", w.Code, w.Header())
	}
}
