package ipfs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// kubo's add writes the file's JSON object first and pins the root after;
// a client that hangs up on that first object cancels the pin.
func TestAddWaitsForKuboToFinish(t *testing.T) {
	var pinned atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/add" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Name":"t","Hash":"bafytest","Size":"3"}` + "\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done(): // the client hung up: kubo drops the root pin here
		case <-time.After(150 * time.Millisecond):
			pinned.Store(true)
		}
	}))
	defer srv.Close()
	cid, err := New(srv.URL).Add(strings.NewReader("abc"), "t")
	if err != nil || cid != "bafytest" {
		t.Fatalf("Add = %q, %v", cid, err)
	}
	if !pinned.Load() {
		t.Fatal("Add returned before kubo finished; the root pin would be lost")
	}
}

func TestPinnedAndPin(t *testing.T) {
	var added atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/pin/ls":
			if r.URL.Query().Get("type") != "recursive" {
				t.Errorf("pin/ls type = %q", r.URL.Query().Get("type"))
			}
			w.Write([]byte(`{"Keys":{"bafya":{"Type":"recursive"},"bafyb":{"Type":"recursive"}}}`))
		case "/api/v0/pin/add":
			added.Store(r.URL.Query().Get("arg"))
			w.Write([]byte(`{"Pins":["bafyc"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	have, err := c.Pinned()
	if err != nil || !have["bafya"] || !have["bafyb"] || have["bafyc"] {
		t.Fatalf("Pinned = %v, %v", have, err)
	}
	if err := c.Pin("bafyc"); err != nil || added.Load() != "bafyc" {
		t.Fatalf("Pin: %v, added %v", err, added.Load())
	}
}
