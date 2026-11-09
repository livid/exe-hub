package accesslog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLog(t *testing.T) *Logger {
	t.Helper()
	l, err := New(Config{TrustedProxies: []string{"172.30.0.1/32", "127.0.0.0/8"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func records(t *testing.T, l *Logger) []Record {
	t.Helper()
	data, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	var result []Record
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("bad JSON line: %v", err)
		}
		result = append(result, record)
	}
	return result
}

func TestAttributionAndSecrets(t *testing.T) {
	l := testLog(t)
	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		fmt.Fprint(w, "created")
	}))
	for _, remote := range []string{"172.30.0.1:1234", "203.0.113.50:1234"} {
		r := httptest.NewRequest("POST", "http://hub.example/stats?page=2&token=secret-query&API_KEY=secret-key&auth=secret-auth&private_key=secret-private&x_hub_sig=secret-sig", strings.NewReader("secret-body"))
		r.RemoteAddr = remote
		r.Header.Set("CF-Connecting-IP", "198.51.100.8")
		r.Header.Set("CF-Ray", "ray-LAX")
		r.Header.Set("CF-IPCountry", "US")
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("Authorization", "Bearer secret-authorization")
		r.Header.Set("Cookie", "session=secret-cookie")
		r.Header.Set("X-Hub-Sig", "secret-signature")
		r.Header.Set("Referer", "https://user:secret-userinfo@hub.example/stats?password=secret-ref#secret-fragment")
		r.Header.Set("Range", "bytes=0-63")
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	got := records(t, l)
	if len(got) != 2 {
		t.Fatal(len(got))
	}
	trusted, direct := got[0], got[1]
	if trusted.ClientIP != "198.51.100.8" || trusted.ClientIPSource != "cf-connecting-ip" || !trusted.TrustedProxy || trusted.CFRay != "ray-LAX" || trusted.Scheme != "https" {
		t.Fatalf("trusted attribution: %+v", trusted)
	}
	if direct.ClientIP != "203.0.113.50" || direct.TrustedProxy || direct.CFRay != "" || direct.Country != "" || direct.Scheme != "http" {
		t.Fatalf("spoofed attribution: %+v", direct)
	}
	if trusted.Status != 201 || trusted.ResponseBytes != 7 || trusted.RequestBodyBytesRead != 11 || trusted.Headers["range"] != "bytes=0-63" || !strings.Contains(trusted.Query, "page=2") {
		t.Fatalf("metrics: %+v", trusted)
	}
	data, _ := os.ReadFile(l.Path)
	if bytes.Contains(data, []byte("secret-")) || bytes.Contains(data, []byte("secret-body")) {
		t.Fatalf("secret in access log: %s", data)
	}
	info, _ := os.Stat(l.Path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}

func TestForwardedChainFromTrustedEnd(t *testing.T) {
	l := testLog(t)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.30.0.1:9"
	r.Header.Set("X-Forwarded-For", "203.0.113.99, 198.51.100.7, 127.0.0.2")
	h.ServeHTTP(httptest.NewRecorder(), r)
	got := records(t, l)[0]
	if got.ClientIP != "198.51.100.7" || got.ClientIPSource != "x-forwarded-for" {
		t.Fatalf("trusted chain: %+v", got)
	}
}

func TestRangesHEADAndConditional(t *testing.T) {
	l := testLog(t)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"immutable"`)
		http.ServeContent(w, r, "sample.png", time.Time{}, strings.NewReader("0123456789"))
	}))
	for _, tc := range []struct {
		method, header, value string
		status                int
		bytes                 int64
	}{
		{"GET", "Range", "bytes=2-5", 206, 4},
		{"HEAD", "", "", 200, 0},
		{"GET", "If-None-Match", `"immutable"`, 304, 0},
	} {
		r := httptest.NewRequest(tc.method, "/v1/embed/cid", nil)
		if tc.header != "" {
			r.Header.Set(tc.header, tc.value)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		got := records(t, l)
		last := got[len(got)-1]
		if w.Code != tc.status || last.Status != tc.status || last.ResponseBytes != tc.bytes {
			t.Fatalf("%s %s: response %d, record %+v", tc.method, tc.header, w.Code, last)
		}
	}
}

func TestStreamingFlushAndCancellation(t *testing.T) {
	l := testLog(t)
	finished := make(chan struct{})
	srv := httptest.NewServer(l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": connected\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
		<-r.Context().Done()
		close(finished)
	})))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/events", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != ": connected\n" {
		t.Fatalf("stream blocked: %q %v", line, err)
	}
	cancel()
	response.Body.Close()
	<-finished
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if info, _ := os.Stat(l.Path); info.Size() > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	got := records(t, l)[0]
	if got.Status != 200 || got.ResponseBytes != 13 || got.Outcome != "canceled" || got.ContentType != "text/event-stream" || got.FirstByteMS == nil {
		t.Fatalf("stream record: %+v", got)
	}
}

func TestPanicStillLogsAndPropagates(t *testing.T) {
	l := testLog(t)
	func() {
		defer func() {
			if recover() != "boom" {
				t.Error("panic swallowed")
			}
		}()
		l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/panic", nil))
	}()
	got := records(t, l)[0]
	if got.Outcome != "aborted" || got.Status != 0 {
		t.Fatalf("uncommitted panic: %+v", got)
	}
}

func TestConcurrentLinesAndCopy(t *testing.T) {
	l := testLog(t)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(w, strings.NewReader(strings.Repeat("x", 8192))) }))
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/copy", nil)) }()
	}
	wg.Wait()
	got := records(t, l)
	if len(got) != 30 {
		t.Fatal(len(got))
	}
	for _, record := range got {
		if record.Status != 200 || record.ResponseBytes != 8192 {
			t.Fatalf("copy: %+v", record)
		}
	}
}

func TestConfigDefaultsAndInvalidTrust(t *testing.T) {
	c := Config{}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.MaxSizeMB != 50 || c.MaxBackups != 7 || c.MaxAgeDays != 7 || c.Path != "access.jsonl" {
		t.Fatalf("defaults: %+v", c)
	}
	for _, c := range []Config{{MaxSizeMB: -1}, {MaxBackups: -1}, {MaxAgeDays: -1}, {TrustedProxies: []string{"all"}}} {
		if c.Normalize() == nil {
			t.Fatalf("accepted invalid config: %+v", c)
		}
	}
}

type readerFromRecorder struct{ *httptest.ResponseRecorder }

func (w readerFromRecorder) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(w.ResponseRecorder, r)
}

func TestReaderFromBytes(t *testing.T) {
	l := testLog(t)
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hide the source's WriterTo to exercise the destination's ReadFrom.
		_, err := io.Copy(w, struct{ io.Reader }{strings.NewReader("copied")})
		if err != nil {
			t.Error(err)
		}
	}))
	h.ServeHTTP(readerFromRecorder{httptest.NewRecorder()}, httptest.NewRequest("GET", "/copy", nil))
	got := records(t, l)[0]
	if got.ResponseBytes != 6 {
		t.Fatalf("ReaderFrom metrics: %+v", got)
	}
}

type brokenLog struct{}

func (brokenLog) Write(p []byte) (int, error) { return 0, errors.New("disk unavailable") }
func (brokenLog) Close() error                { return nil }

func TestLogFailureDoesNotFailRequest(t *testing.T) {
	l := &Logger{writer: brokenLog{}}
	w := httptest.NewRecorder()
	l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || w.Body.String() != "ok" {
		t.Fatal("logging failure affected response")
	}
	if l.lastError.IsZero() {
		t.Fatal("logging failure was not reported")
	}
}
