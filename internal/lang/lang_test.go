package lang

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"exehub/internal/store"
)

// TestNormal: an answer is kept as the language alone, with the script
// only where it tells (a language written in one script loses it, one
// written in two keeps it); a region is dropped, Chinese always says its
// script, and what is no tag is no answer.
func TestNormal(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"en", "en"},
		{" EN\n", "en"},
		{"en-US", "en"},
		{"en-Latn", "en"},
		{"ja", "ja"},
		{"zh-Hans", "zh-Hans"},
		{"zh-hant", "zh-Hant"},
		{"zh-CN", "zh-Hans"},
		{"zh-TW", "zh-Hant"},
		{"zh-HK", "zh-Hant"},
		{"zh-Hant-TW", "zh-Hant"},
		{"sr-Latn", "sr-Latn"},
		{"sr-Cyrl", "sr-Cyrl"},
		{"pt-BR", "pt"},
		{"zxx", "zxx"},
		{"und", "und"},
		{"`zh-Hans`", "zh-Hans"},
		{`"fr".`, "fr"},
		{`{"lang": "ko"}`, "ko"},
	} {
		if got, ok := Normal(c.in); !ok || got != c.want {
			t.Errorf("Normal(%q) = %q, %v; want %q", c.in, got, ok, c.want)
		}
	}
	for _, in := range []string{"", "zh", "banana!", "The post is in English.", "en (English)", `{"lang": 3}`, strings.Repeat("a", 40)} {
		if got, ok := Normal(in); ok {
			t.Errorf("Normal(%q) = %q, want no answer", in, got)
		}
	}
}

func TestWordless(t *testing.T) {
	for text, want := range map[string]bool{
		"":                               true,
		"🚀🚀 https://example.com/a/b?c=d": true,
		"12:30 → 14:00":                  true,
		"ok":                             false,
		// prose flush against a link is still prose; the link alone is not
		"https://example.com/，这个链接打不开":  false,
		"https://example.com/ ，这个链接打不开": false,
		"https://example.com/a/b?c=d":   true,
		"详见https://x.y的说明":              false,
		"看 https://example.com/":        false,
	} {
		if got := Wordless(text); got != want {
			t.Errorf("Wordless(%q) = %v, want %v", text, got, want)
		}
	}
}

// fakeOllama answers /api/chat from a function of the post, and keeps
// what it was asked.
type fakeOllama struct {
	mu     sync.Mutex
	asked  []chatRequest
	answer func(post string) (status int, content string)
	levels bool // false: an Ollama that takes think only as true or false
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/version" {
		io.WriteString(w, `{"version":"0.0.0"}`)
		return
	}
	var req chatRequest
	if r.URL.Path != "/api/chat" || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Messages) != 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.asked = append(f.asked, req)
	f.mu.Unlock()
	if _, level := req.Think.(string); level && !f.levels {
		http.Error(w, `{"error":"invalid think value"}`, http.StatusBadRequest)
		return
	}
	status, content := f.answer(req.Messages[1].Content)
	if status != http.StatusOK {
		http.Error(w, content, status)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"message": message{"assistant", content}})
}

func newFake(t *testing.T, f *fakeOllama) *Detector {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return NewDetector(srv.URL+"/", "", "glm-5.3:cloud", "max")
}

// TestDetect: the post goes as the user's message under the system
// prompt with the think level asked for, cut to what the model needs;
// the answer comes back in normal form, and one that is no tag is
// ErrAnswer while a server error is not.
func TestDetect(t *testing.T) {
	f := &fakeOllama{levels: true, answer: func(post string) (int, string) {
		switch {
		case strings.HasPrefix(post, "今天"):
			return 200, "zh-CN\n"
		case post == "chatty":
			return 200, "This post is written in English."
		case post == "broken":
			return 500, "upstream"
		}
		return 200, "en"
	}}
	d := newFake(t, f)
	if err := d.Available(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if tag, err := d.Detect(ctx, "今天"+strings.Repeat("好", 3000)); err != nil || tag != "zh-Hans" {
		t.Fatalf("Detect = %q, %v; want zh-Hans", tag, err)
	}
	req := f.asked[0]
	if req.Model != "glm-5.3:cloud" || req.Think != "max" || req.Stream || req.Messages[0].Role != "system" || req.Messages[0].Content != prompt {
		t.Fatalf("asked %+v", req)
	}
	if n := len([]rune(req.Messages[1].Content)); n != maxRunes {
		t.Fatalf("post sent at %d runes, want %d", n, maxRunes)
	}
	if _, err := d.Detect(ctx, "chatty"); !errors.Is(err, ErrAnswer) {
		t.Fatalf("a sentence: err = %v, want ErrAnswer", err)
	}
	if _, err := d.Detect(ctx, "broken"); err == nil || errors.Is(err, ErrAnswer) {
		t.Fatalf("HTTP 500: err = %v, want the line's error", err)
	}
}

// TestThinkStepsDown: a think level the server refuses is asked again as
// plain true — never as false, never dropped while true is taken.
func TestThinkStepsDown(t *testing.T) {
	f := &fakeOllama{answer: func(string) (int, string) { return 200, "ja" }}
	d := newFake(t, f)
	if tag, err := d.Detect(context.Background(), "こんにちは"); err != nil || tag != "ja" {
		t.Fatalf("Detect = %q, %v", tag, err)
	}
	if len(f.asked) != 2 || f.asked[0].Think != "max" || f.asked[1].Think != true {
		t.Fatalf("think went %v, want max then true", []any{f.asked[0].Think, f.asked[len(f.asked)-1].Think})
	}
}

// fakePosts is the worker's slice of the store.
type fakePosts struct {
	posts []store.CardPost
	rows  map[string]fakeRow
}

type fakeRow struct {
	lang, model string
	ok          bool
	tries       int
}

func (p *fakePosts) PostsWithoutLang(maxTries int, before int64, limit int) ([]store.CardPost, error) {
	var out []store.CardPost
	for _, c := range p.posts {
		if _, done := p.rows[c.ID]; !done && len(out) < limit { // a failed row waits an hour, so never in one pass
			out = append(out, c)
		}
	}
	return out, nil
}

func (p *fakePosts) SetLang(post, lang, model string, ok bool) error {
	p.rows[post] = fakeRow{lang, model, ok, p.rows[post].tries + 1}
	return nil
}

// TestPass: one pass names every post, pages and all; a post with no
// words is zxx with nobody asked, an answer that is no tag is a failed
// try, and a post the server refuses is stepped over with nothing
// recorded, the ones behind it named all the same.
func TestPass(t *testing.T) {
	f := &fakeOllama{levels: true, answer: func(post string) (int, string) {
		switch {
		case post == "refused":
			return 500, "upstream"
		case post == "chatty":
			return 200, "It is English."
		case strings.HasPrefix(post, "今天"):
			return 200, "zh-Hans"
		}
		return 200, "en"
	}}
	st := &fakePosts{rows: map[string]fakeRow{}}
	add := func(id, text string) { st.posts = append(st.posts, store.CardPost{ID: id, Text: text}) }
	add("refused", "refused")
	add("zh", "今天天气很好")
	add("emoji", "🚀 https://example.com/")
	add("chatty", "chatty")
	for i := 0; i < page+10; i++ {
		add("en"+string(rune('A'+i%26))+string(rune('a'+i/26)), "hello there")
	}
	w := NewWorker(st, newFake(t, f))
	if !w.pass() {
		t.Fatal("pass = false with one refused post, want true: Ollama is up")
	}
	if _, has := st.rows["refused"]; has {
		t.Fatalf("a refused post got a row: %+v", st.rows["refused"])
	}
	for id, want := range map[string]fakeRow{
		"zh":     {"zh-Hans", "glm-5.3:cloud", true, 1},
		"emoji":  {"zxx", "", true, 1},
		"chatty": {"", "glm-5.3:cloud", false, 1},
	} {
		if st.rows[id] != want {
			t.Errorf("%s = %+v, want %+v", id, st.rows[id], want)
		}
	}
	if len(st.rows) != 3+page+10 {
		t.Fatalf("%d posts have rows, want %d: a pass goes through every page", len(st.rows), 3+page+10)
	}
	for _, req := range f.asked {
		if strings.Contains(req.Messages[1].Content, "🚀") {
			t.Fatal("a post with no words was sent to the model")
		}
	}
}

// TestPassOllamaAway: an Ollama that does not answer ends the pass after
// a few posts, false, with nothing recorded against any of them.
func TestPassOllamaAway(t *testing.T) {
	f := &fakeOllama{levels: true, answer: func(string) (int, string) { return 503, "away" }}
	st := &fakePosts{rows: map[string]fakeRow{}}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		st.posts = append(st.posts, store.CardPost{ID: id, Text: "hello"})
	}
	w := NewWorker(st, newFake(t, f))
	if w.pass() {
		t.Fatal("pass = true with Ollama away")
	}
	if len(st.rows) != 0 || len(f.asked) != downAfter {
		t.Fatalf("%d rows after %d asks, want none after %d", len(st.rows), len(f.asked), downAfter)
	}
}

func TestTally(t *testing.T) {
	if got := tally(map[string]int{"en": 7, "zh-Hans": 4, "ja": 1, "de": 1}); got != "13 posts named: en 7, zh-Hans 4, de 1, ja 1" {
		t.Fatal(got)
	}
	if got := tally(map[string]int{"ja": 1}); got != "1 post named: ja 1" {
		t.Fatal(got)
	}
}
