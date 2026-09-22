package api

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
// The ping carries the feed's counts; "online" only with analytics on.
func TestEvents(t *testing.T) {
	s := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	s.Events = events.New()
	pub, priv, _ := ed25519.GenerateKey(nil)
	ingest(t, s, priv, pub, 1, "post.create", map[string]any{"text": "one post"})
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
	var ping map[string]any
	if l := next(); !strings.HasPrefix(l, "data: ") || json.Unmarshal([]byte(l[6:]), &ping) != nil {
		t.Fatalf("the heartbeat's data is %q", l)
	} else if ping["type"] != "ping" || ping["posts"] != 1.0 || ping["members"] != 0.0 {
		t.Errorf("the heartbeat carries %v, want type ping, 1 post, 0 members", ping)
	} else if _, on := ping["online"]; on {
		t.Error("the heartbeat says online with no analytics")
	}
	if b := s.pingData(); &b[0] != &s.ping.data[0] {
		t.Error("a second ask within pingFresh drew the counts again")
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
