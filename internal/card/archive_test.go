package card

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"exehub/internal/events"
	"exehub/internal/store"
)

// fakeArchive stands in for archive.org: a CDX index, the Save Page Now
// form and its job status, each answering what the real one does.
type fakeArchive struct {
	mu       sync.Mutex
	captures map[string]string // link → newest 200 timestamp
	pending  int               // status answers "pending" this many times, then success
	saves    []string          // links asked to be saved
	polls    int
	busy     bool // answer 429 to everything
}

func (f *fakeArchive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	switch {
	case r.URL.Path == "/cdx/search/cdx":
		q := r.URL.Query()
		if q.Get("filter") != "statuscode:200" || q.Get("limit") != "-1" || q.Get("output") != "json" {
			http.Error(w, "bad query", 400)
			return
		}
		if ts, ok := f.captures[q.Get("url")]; ok {
			fmt.Fprintf(w, "[[\"timestamp\"],\n[%q]]\n", ts)
		} // else: the index's empty answer
	case r.URL.Path == "/save/" && r.Method == "POST":
		r.ParseForm()
		f.saves = append(f.saves, r.PostForm.Get("url"))
		fmt.Fprint(w, `<h2 id="spn-title">Saving page</h2><script>
spn.watchJob("spn2-9878e19ee26531d6415d4cfc93c52b9bf4341a39", "https://web-static.archive.org/_static/",
	     6000,
         false);
</script>`)
	case r.URL.Path == "/save/status/spn2-9878e19ee26531d6415d4cfc93c52b9bf4341a39":
		f.polls++
		if f.pending > 0 {
			f.pending--
			fmt.Fprint(w, `{"status":"pending","job_id":"spn2-9878e19ee26531d6415d4cfc93c52b9bf4341a39","resources":[]}`)
			return
		}
		fmt.Fprint(w, `{"status":"success","job_id":"spn2-9878e19ee26531d6415d4cfc93c52b9bf4341a39","original_url":"https://b.example/new","timestamp":"20260913230102","duration_sec":41.2,"http_status":200}`)
	case r.URL.Path == "/save/status/spn2-gone":
		fmt.Fprint(w, `{"status":"success","timestamp":"20260913230102","original_url":"https://b.example/new","http_status":404}`)
	case r.URL.Path == "/save/status/spn2-dead":
		fmt.Fprint(w, `{"status":"error","status_ext":"error:not-found","message":"The server cannot find the requested resource."}`)
	default:
		http.NotFound(w, r)
	}
}

func testWayback(t *testing.T, f *fakeArchive) *Wayback {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	w := NewWayback()
	w.Base = srv.URL
	return w
}

// TestWayback: the client reads a capture from the index (the fragment
// dropped, none when the index answers nothing), finds the job in the
// save form's page, and tells a pending, a finished and a failed job apart.
func TestWayback(t *testing.T) {
	f := &fakeArchive{captures: map[string]string{"https://a.example/old": "20180523210631"}, pending: 1}
	w := testWayback(t, f)
	ctx := context.Background()

	got, err := w.Latest(ctx, "https://a.example/old#section")
	if err != nil || got != "https://web.archive.org/web/20180523210631/https://a.example/old" {
		t.Fatalf("Latest = %q, %v", got, err)
	}
	if got, err := w.Latest(ctx, "https://b.example/new"); err != nil || got != "" {
		t.Fatalf("Latest of an uncaptured link = %q, %v", got, err)
	}

	job, err := w.Save(ctx, "https://b.example/new#top")
	if err != nil || job != "spn2-9878e19ee26531d6415d4cfc93c52b9bf4341a39" {
		t.Fatalf("Save = %q, %v", job, err)
	}
	if len(f.saves) != 1 || f.saves[0] != "https://b.example/new" {
		t.Fatalf("saved %v", f.saves)
	}
	if got, err := w.Saved(ctx, job, "https://b.example/new"); err != nil || got != "" {
		t.Fatalf("pending job = %q, %v", got, err)
	}
	if got, err := w.Saved(ctx, job, "https://b.example/new"); err != nil || got != "https://web.archive.org/web/20260913230102/https://b.example/new" {
		t.Fatalf("finished job = %q, %v", got, err)
	}
	for _, dead := range []string{"spn2-dead", "spn2-gone"} { // a failed job, and a capture of an error page
		if _, err := w.Saved(ctx, dead, "https://b.example/new"); !errors.Is(err, errSaveFailed) {
			t.Fatalf("%s err = %v, want errSaveFailed", dead, err)
		}
	}
	f.busy = true
	if _, err := w.Latest(ctx, "https://a.example/old"); !errors.Is(err, errBusy) {
		t.Fatalf("429 err = %v, want errBusy", err)
	}
}

// fakeCards is the archiver's slice of the store.
type fakeCards struct {
	mu     sync.Mutex
	links  map[string]string // post → link, for ok cards without a copy
	tries  map[string]int
	copies map[string]string
	landed chan string
}

func (c *fakeCards) BeginArchive(post string, maxTries int, before int64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, done := c.copies[post]; done || c.tries[post] >= maxTries {
		return "", nil
	}
	c.tries[post]++
	return c.links[post], nil
}

func (c *fakeCards) SetArchive(post, archive string) error {
	c.mu.Lock()
	c.copies[post] = archive
	c.mu.Unlock()
	c.landed <- post
	return nil
}

func (c *fakeCards) CardsToArchive(int, int64, int) ([]store.CardPost, error) { return nil, nil }

// TestArchiver: a captured link lands its copy from the index alone; an
// uncaptured one is saved, polled until the job finishes, and lands —
// each announced as post.card so live views redraw the card.
func TestArchiver(t *testing.T) {
	f := &fakeArchive{captures: map[string]string{"https://a.example/old": "20180523210631"}, pending: 2}
	cards := &fakeCards{links: map[string]string{"p-old": "https://a.example/old", "p-new": "https://b.example/new"},
		tries: map[string]int{}, copies: map[string]string{}, landed: make(chan string, 4)}
	bus := events.New()
	sub := bus.Subscribe()
	a := NewArchiver(cards, bus)
	a.WB = testWayback(t, f)
	a.Gap = 0
	a.Polls = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	go a.Run()

	a.Enqueue("p-old", "ann")
	a.Enqueue("p-new", "bob")
	for i := 0; i < 2; i++ {
		select {
		case <-cards.landed:
		case <-time.After(5 * time.Second):
			t.Fatalf("copies landed: %v", cards.copies)
		}
	}
	cards.mu.Lock()
	defer cards.mu.Unlock()
	if cards.copies["p-old"] != "https://web.archive.org/web/20180523210631/https://a.example/old" {
		t.Errorf("found copy = %q", cards.copies["p-old"])
	}
	if cards.copies["p-new"] != "https://web.archive.org/web/20260913230102/https://b.example/new" {
		t.Errorf("saved copy = %q", cards.copies["p-new"])
	}
	f.mu.Lock()
	if len(f.saves) != 1 || f.polls != 3 {
		t.Errorf("saves %v, polls %d: want one save and three polls", f.saves, f.polls)
	}
	f.mu.Unlock()
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case ev := <-sub:
			if ev.Type == "post.card" {
				seen[ev.ID] = true
			}
		case <-time.After(time.Second):
			t.Fatalf("post.card events: %v", seen)
		}
	}
}
