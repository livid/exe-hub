// Package memcache is the hub's cache in memory: a value under a key,
// kept for a while and then drawn again. It is for a page whose numbers
// cost a walk over the whole store and are the same for every reader —
// the Archives page (PLAN.md, Archives) — so a hundred readers cost one
// walk, not a hundred.
//
// It lives in the process. Nothing is written anywhere and no server
// stands beside the hub: a restart starts it empty, and two hubs each
// keep their own.
package memcache

import (
	"sync"
	"time"
)

// Cache holds values by key until each one's time is up. The zero value
// is an empty cache, ready to use; it is safe for many goroutines.
type Cache struct {
	mu  sync.Mutex
	m   map[string]*entry
	now func() time.Time // the clock, a test's own when set
}

type entry struct {
	val any
	exp time.Time
	// a value being drawn: closed when it is there, err what drawing it
	// said. nil for a value that was set.
	ready chan struct{}
	err   error
}

func (c *Cache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Get is the value under key, false when there is none or its time is up.
func (c *Cache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[key]
	if e == nil || e.ready != nil || !c.clock().Before(e.exp) {
		return nil, false
	}
	return e.val, true
}

// Set keeps val under key for ttl.
func (c *Cache) Set(key string, val any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set(key, &entry{val: val, exp: c.clock().Add(ttl)})
}

// set stores e and drops every value whose time is up, so a key nobody
// asks for again (yesterday's page) does not stay for good.
func (c *Cache) set(key string, e *entry) {
	if c.m == nil {
		c.m = map[string]*entry{}
	}
	now := c.clock()
	for k, old := range c.m {
		if old.ready == nil && !now.Before(old.exp) {
			delete(c.m, k)
		}
	}
	c.m[key] = e
}

// Delete drops the value under key.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.m[key]; e != nil && e.ready == nil {
		delete(c.m, key)
	}
}

// Fetch is the value under key, drawn by fill and kept for ttl when
// there is none. Readers who ask while it is being drawn wait for that
// one drawing and share it. An error is handed to them and not kept:
// the next reader draws again.
func (c *Cache) Fetch(key string, ttl time.Duration, fill func() (any, error)) (any, error) {
	c.mu.Lock()
	if e := c.m[key]; e != nil {
		if e.ready != nil {
			c.mu.Unlock()
			<-e.ready
			return e.val, e.err
		}
		if c.clock().Before(e.exp) {
			c.mu.Unlock()
			return e.val, nil
		}
	}
	e := &entry{ready: make(chan struct{})}
	c.set(key, e)
	c.mu.Unlock()

	val, err := fill()

	c.mu.Lock()
	e.val, e.err = val, err
	if c.m[key] == e {
		if err != nil {
			delete(c.m, key)
		} else {
			c.m[key] = &entry{val: val, exp: c.clock().Add(ttl)}
		}
	}
	c.mu.Unlock()
	close(e.ready)
	return val, err
}
