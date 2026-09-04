package main

import (
	"testing"
	"time"
)

// An address that is on no interface yet (a Tailscale IP before tailscaled
// has logged in) is retried until the wait runs out, not refused at once.
func TestListenWaitRetriesUnassignedAddress(t *testing.T) {
	const addr = "192.0.2.1:0" // TEST-NET-1: never a local address
	start := time.Now()
	if _, err := listenWait(addr, 400*time.Millisecond); err == nil {
		t.Fatalf("listenWait(%s) succeeded", addr)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Fatalf("gave up on %s after %s, before the wait ran out", addr, time.Since(start))
	}
}

// Anything else fails at once.
func TestListenWaitRefusesBadAddress(t *testing.T) {
	start := time.Now()
	if _, err := listenWait("127.0.0.1:notaport", 5*time.Second); err == nil {
		t.Fatal("listenWait(bad address) succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("retried an unbindable address for %s", time.Since(start))
	}
}
