package accesslog

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"gopkg.in/natefinch/lumberjack.v2"
)

type Logger struct {
	writer    io.WriteCloser
	trusted   []netip.Prefix
	mu        sync.Mutex
	lastError time.Time
	Path      string
}

// New validates and opens the destination before serving traffic. Only the
// hub process writes this file; lumberjack owns rotation, not logrotate.
func New(c Config, stateDir string) (*Logger, error) {
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	path := c.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(stateDir, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("access log: %w", err)
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	l := &Logger{Path: path, writer: &lumberjack.Logger{Filename: path, MaxSize: c.MaxSizeMB,
		MaxBackups: c.MaxBackups, MaxAge: c.MaxAgeDays, Compress: true}}
	for _, raw := range c.TrustedProxies {
		l.trusted = append(l.trusted, netip.MustParsePrefix(raw))
	}
	return l, nil
}

func (l *Logger) Close() error { return l.writer.Close() }

type Record struct {
	Type                 string            `json:"type"`
	Time                 time.Time         `json:"time"`
	StartedAt            time.Time         `json:"started_at"`
	Method               string            `json:"method"`
	Scheme               string            `json:"scheme"`
	Host                 string            `json:"host"`
	Protocol             string            `json:"protocol"`
	Path                 string            `json:"path"`
	Query                string            `json:"query,omitempty"`
	RemoteAddr           string            `json:"remote_addr"`
	ClientIP             string            `json:"client_ip"`
	ClientIPSource       string            `json:"client_ip_source"`
	TrustedProxy         bool              `json:"trusted_proxy"`
	CFRay                string            `json:"cf_ray,omitempty"`
	Country              string            `json:"country,omitempty"`
	UserAgent            string            `json:"user_agent,omitempty"`
	Referer              string            `json:"referer,omitempty"`
	Headers              map[string]string `json:"request_headers,omitempty"`
	Status               int               `json:"status"`
	RequestContentLength int64             `json:"request_content_length"`
	RequestBodyBytesRead int64             `json:"request_body_bytes_read"`
	ResponseBytes        int64             `json:"response_bytes"`
	ContentType          string            `json:"content_type,omitempty"`
	ContentEncoding      string            `json:"content_encoding,omitempty"`
	CacheControl         string            `json:"cache_control,omitempty"`
	ETag                 string            `json:"etag,omitempty"`
	ContentRange         string            `json:"content_range,omitempty"`
	DurationMS           float64           `json:"duration_ms"`
	FirstByteMS          *float64          `json:"first_byte_ms,omitempty"`
	Outcome              string            `json:"outcome"`
	ResponseWriteError   bool              `json:"response_write_error,omitempty"`
}

type bodyCounter struct {
	io.ReadCloser
	n int64
}

func (b *bodyCounter) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

func (l *Logger) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		record := l.request(r, start)
		var body *bodyCounter
		if r.Body != nil {
			body = &bodyCounter{ReadCloser: r.Body}
			r.Body = body
		}
		complete := false
		var responseHeaders http.Header
		commit := func(status int) {
			if record.Status != 0 {
				return
			}
			record.Status = status
			ms := float64(time.Since(start)) / float64(time.Millisecond)
			record.FirstByteMS = &ms
			responseHeaders = w.Header().Clone()
		}
		defer func() {
			record.Outcome = "complete"
			if !complete {
				record.Outcome = "aborted"
			} else if r.Context().Err() != nil {
				record.Outcome = "canceled"
			}
			if complete && record.Status == 0 {
				commit(http.StatusOK)
			}
			record.Time = time.Now().UTC()
			record.DurationMS = float64(time.Since(start)) / float64(time.Millisecond)
			if body != nil {
				record.RequestBodyBytesRead = body.n
			}
			record.ContentType = bounded(responseHeaders.Get("Content-Type"), 256)
			record.ContentEncoding = bounded(responseHeaders.Get("Content-Encoding"), 128)
			record.CacheControl = bounded(responseHeaders.Get("Cache-Control"), 512)
			record.ETag = bounded(responseHeaders.Get("ETag"), 256)
			record.ContentRange = bounded(responseHeaders.Get("Content-Range"), 256)
			l.write(record)
		}()
		wrapped := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(status int) {
					if status >= 200 || status == 101 {
						commit(status)
					}
					next(status)
				}
			},
			Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(p []byte) (int, error) {
					commit(200)
					n, err := next(p)
					record.ResponseBytes += int64(n)
					record.ResponseWriteError = record.ResponseWriteError || err != nil
					return n, err
				}
			},
			ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
				return func(src io.Reader) (int64, error) {
					commit(200)
					n, err := next(src)
					record.ResponseBytes += n
					record.ResponseWriteError = record.ResponseWriteError || err != nil
					return n, err
				}
			},
			Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
				return func() { commit(200); next() }
			},
		})
		next.ServeHTTP(wrapped, r)
		complete = true
	})
}

func (l *Logger) request(r *http.Request, start time.Time) Record {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, _ := netip.ParseAddr(host)
	ip = ip.Unmap()
	trusted := l.isTrusted(ip)
	client, source := host, "remote_addr"
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	record := Record{Type: "http_access", StartedAt: start.UTC(), Method: r.Method,
		Scheme: scheme, Host: bounded(r.Host, 512), Protocol: r.Proto, Path: bounded(r.URL.EscapedPath(), 4096),
		Query: cleanQuery(r.URL.RawQuery), RemoteAddr: bounded(r.RemoteAddr, 256),
		TrustedProxy: trusted, UserAgent: bounded(r.UserAgent(), 2048), Referer: cleanReferer(r.Referer()),
		RequestContentLength: r.ContentLength, Headers: make(map[string]string)}
	if trusted {
		if cf, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); err == nil {
			client, source = cf.Unmap().String(), "cf-connecting-ip"
		} else {
			chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
			for i := len(chain) - 1; i >= 0; i-- {
				candidate, err := netip.ParseAddr(strings.TrimSpace(chain[i]))
				if err != nil {
					break
				}
				candidate = candidate.Unmap()
				client, source = candidate.String(), "x-forwarded-for"
				if !l.isTrusted(candidate) {
					break
				}
			}
		}
		if proto := r.Header.Get("X-Forwarded-Proto"); proto == "http" || proto == "https" {
			record.Scheme = proto
		}
		record.CFRay = bounded(r.Header.Get("CF-Ray"), 128)
		country := strings.ToUpper(r.Header.Get("CF-IPCountry"))
		if len(country) == 2 {
			record.Country = country
		}
	}
	record.ClientIP, record.ClientIPSource = bounded(client, 128), source
	for _, key := range []string{"Accept", "Accept-Encoding", "Accept-Language", "Range", "If-Range", "If-None-Match", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-CH-UA", "X-Hub-Live"} {
		if value := r.Header.Get(key); value != "" {
			record.Headers[strings.ToLower(key)] = bounded(value, 1024)
		}
	}
	return record
}

func (l *Logger) isTrusted(ip netip.Addr) bool {
	for _, prefix := range l.trusted {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func (l *Logger) write(record Record) {
	data, err := json.Marshal(record)
	if err == nil {
		_, err = l.writer.Write(append(data, '\n'))
	}
	if err != nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		if time.Since(l.lastError) >= time.Minute {
			log.Printf("access log: write failed: %v", err)
			l.lastError = time.Now()
		}
	}
}

func bounded(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

func cleanQuery(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > 8192 {
		return "[query too long]"
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "[invalid query]"
	}
	for key := range values {
		name := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
		if strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "password") || strings.Contains(name, "passwd") || strings.Contains(name, "credential") || strings.Contains(name, "authorization") || strings.Contains(name, "apikey") || strings.Contains(name, "signature") || strings.HasSuffix(name, "key") || strings.HasSuffix(name, "sig") || name == "auth" || name == "pass" || name == "pwd" || name == "session" || name == "sessionid" || name == "jwt" || name == "cookie" || name == "code" || name == "envelope" {
			values[key] = []string{"[redacted]"}
		}
	}
	return bounded(values.Encode(), 4096)
}

func cleanReferer(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid referer]"
	}
	u.User, u.Fragment, u.RawFragment = nil, "", ""
	u.RawQuery = cleanQuery(u.RawQuery)
	return bounded(u.String(), 2048)
}
