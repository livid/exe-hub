package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"exehub/internal/config"
	"exehub/internal/events"
)

// TestEvents: the stream opens with a comment, carries each event as an
// unnamed message, and every heartbeat is a named ping with JSON data —
// something a page can hear (an onmessage handler never sees it; a line
// reader parses the data and drops it by type). A comment was invisible
// to script, so a stream that died without a word looked open forever.
func TestEvents(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	s.Events = events.New()
	was := eventsHeartbeat
	eventsHeartbeat = 30 * time.Millisecond
	t.Cleanup(func() { eventsHeartbeat = was })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	next := func() string {
		for sc.Scan() {
			if l := sc.Text(); l != "" {
				return l
			}
		}
		t.Fatal("the stream ended")
		return ""
	}
	if l := next(); l != ": connected" {
		t.Fatalf("first line %q", l)
	}
	if l := next(); l != "event: ping" {
		t.Fatalf("the heartbeat begins %q, want a named ping", l)
	}
	if l := next(); l != `data: {"type":"ping"}` {
		t.Fatalf("the heartbeat's data is %q", l)
	}
	s.Events.Emit(events.Event{Type: "post.create", ID: "abc", Author: "def"})
	for { // the event, whatever pings come first
		l := next()
		if l == "event: ping" {
			next()
			continue
		}
		if l != `data: {"type":"post.create","id":"abc","author":"def"}` {
			t.Fatalf("the event came as %q", l)
		}
		return
	}
}
