package memcache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCache: a value is there until its time is up, a new one replaces
// it, Delete drops it, and a value whose time is up leaves the map when
// the next one is set.
func TestCache(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &Cache{now: func() time.Time { return now }}
	if _, ok := c.Get("a"); ok {
		t.Fatal("an empty cache has a value")
	}
	c.Set("a", 1, time.Minute)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get = %v, %v; want 1", v, ok)
	}
	c.Set("a", 2, time.Minute)
	if v, _ := c.Get("a"); v != 2 {
		t.Errorf("Get after a second Set = %v, want 2", v)
	}
	now = now.Add(time.Minute)
	if _, ok := c.Get("a"); ok {
		t.Error("a value is still there when its time is up")
	}
	c.Set("b", 3, time.Minute)
	if len(c.m) != 1 {
		t.Errorf("%d values kept, want b alone", len(c.m))
	}
	c.Delete("b")
	if _, ok := c.Get("b"); ok {
		t.Error("a deleted value is still there")
	}
}

// TestFetch: the value is drawn once and kept, drawn again when its time
// is up, and an error is handed on and not kept.
func TestFetch(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &Cache{now: func() time.Time { return now }}
	n := 0
	fill := func() (any, error) { n++; return n, nil }
	for i := 0; i < 3; i++ {
		if v, err := c.Fetch("k", time.Minute, fill); err != nil || v != 1 {
			t.Fatalf("Fetch = %v, %v; want 1", v, err)
		}
	}
	now = now.Add(time.Minute)
	if v, _ := c.Fetch("k", time.Minute, fill); v != 2 {
		t.Errorf("Fetch after the time is up = %v, want 2", v)
	}
	bad := errors.New("no")
	if _, err := c.Fetch("e", time.Minute, func() (any, error) { return nil, bad }); err != bad {
		t.Errorf("Fetch error = %v", err)
	}
	if v, err := c.Fetch("e", time.Minute, func() (any, error) { return "ok", nil }); err != nil || v != "ok" {
		t.Errorf("Fetch after an error = %v, %v; an error must not be kept", v, err)
	}
}

// TestFetchOnce: readers who ask together share one drawing.
func TestFetchOnce(t *testing.T) {
	var c Cache
	var calls atomic.Int32
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.Fetch("k", time.Minute, func() (any, error) {
				calls.Add(1)
				<-gate
				return "v", nil
			})
			if err != nil || v != "v" {
				t.Errorf("Fetch = %v, %v", v, err)
			}
		}()
	}
	// the first reader is drawing; give the rest time to arrive and wait
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("drawn %d times, want once", n)
	}
}
