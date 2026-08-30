package store

import (
	"encoding/base64"
	"errors"
	"testing"

	"exehub/internal/envelope"
)

func TestParseMultiaddr(t *testing.T) {
	ok := map[string]string{
		"/ip4/172.30.0.2/tcp/7788/http":                    "http://172.30.0.2:7788",
		"/dns4/hub.example.com/tcp/443/https":              "https://hub.example.com:443",
		"/ip6/::1/tcp/7788/http":                           "http://[::1]:7788",
		"/dns4/x.io/tcp/443/https/http-path/hub%2Fapi":     "https://x.io:443/hub/api",
	}
	for in, want := range ok {
		got, err := envelope.ParseMultiaddr(in)
		if err != nil || got != want {
			t.Errorf("ParseMultiaddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "http://x.io", "/ip4/1.2.3.4/udp/53/http", "/ip4/1.2.3.4/tcp/0/http",
		"/ip4/1.2.3.4/tcp/80/ws", "/ip4/1.2.3.4/tcp/80/http/extra/junk",
		"/unix/tmp/sock/tcp/1/http",
	} {
		if _, err := envelope.ParseMultiaddr(in); err == nil {
			t.Errorf("ParseMultiaddr(%q) accepted", in)
		}
	}
}

func TestPeerOpsAndReplicationPage(t *testing.T) {
	s := openTest(t)
	admin := newAuthor(t)

	raw, sig, e, op := admin.msg(t, "peer.add", map[string]string{
		"hub": "00112233445566aa", "addr": "/ip4/10.0.0.9/tcp/7788/http"})
	if _, _, err := s.Ingest(raw, sig, e, op); err != nil {
		t.Fatal(err)
	}
	peers, err := s.Peers()
	if err != nil || len(peers) != 1 || peers[0].Hub != "00112233445566aa" {
		t.Fatalf("peers = %+v, %v", peers, err)
	}

	// peer state survives cursor updates and dies with peer.remove
	if err := s.SetPeerCursor("00112233445566aa", 42); err != nil {
		t.Fatal(err)
	}
	raw, sig, e, op = admin.msg(t, "peer.remove", map[string]string{"hub": "00112233445566aa"})
	if _, _, err := s.Ingest(raw, sig, e, op); err != nil {
		t.Fatal(err)
	}
	if peers, _ = s.Peers(); len(peers) != 0 {
		t.Fatalf("peer survived remove: %+v", peers)
	}

	// content and non-content local messages, plus one replicated message
	alice := newAuthor(t)
	raw, sig, e, op = alice.msg(t, "post.create", map[string]string{"text": "local"})
	if _, _, err := s.Ingest(raw, sig, e, op); err != nil {
		t.Fatal(err)
	}
	bob := newAuthor(t)
	rraw, rsig, re, rop := bob.msg(t, "post.create", map[string]string{"text": "from peer"})
	if _, _, err := s.IngestReplicated(rraw, rsig, re, rop, "ffeeddccbbaa9988"); err != nil {
		t.Fatal(err)
	}

	// the page serves local-origin content only: the two peer ops advance
	// the cursor without being served; the replicated post is excluded
	msgs, next, err := s.ReplicationPage(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || string(msgs[0].Envelope) != string(raw) {
		t.Fatalf("page = %d msgs, want just the local post", len(msgs))
	}
	if next <= 2 {
		t.Fatalf("next = %d, cursor did not advance past non-content rows", next)
	}
	if m2, n2, _ := s.ReplicationPage(next, 100); len(m2) != 0 || n2 != next {
		t.Fatalf("drained page = %d msgs, next %d->%d", len(m2), next, n2)
	}

	// replicated ingest recorded its origin; a duplicate is refused
	if _, _, err := s.IngestReplicated(rraw, rsig, re, rop, "ffeeddccbbaa9988"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("dup replicated ingest: %v", err)
	}
	var origin string
	if err := s.db.QueryRow(`SELECT origin FROM messages WHERE id=?`, envelope.MsgID(rraw)).Scan(&origin); err != nil || origin != "ffeeddccbbaa9988" {
		t.Fatalf("origin = %q, %v", origin, err)
	}
}

// Seqs are per-author per hub: the same author's same-numbered messages
// from different hubs must all land, must advance the local high-water,
// and must not unlock seq reuse for direct ingest.
func TestReplicatedSeqConflicts(t *testing.T) {
	s := openTest(t)
	carol := newAuthor(t)

	r1, s1, e1, o1 := carol.msg(t, "post.create", map[string]string{"text": "on hub X"})
	carol.seq = 0 // same seq, different content, as written on another hub
	r2, s2, e2, o2 := carol.msg(t, "post.create", map[string]string{"text": "on hub Y"})
	if _, _, err := s.IngestReplicated(r1, s1, e1, o1, "aaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IngestReplicated(r2, s2, e2, o2, "bbbbbbbbbbbbbbbb"); err != nil {
		t.Fatalf("same-seq from second hub refused: %v", err)
	}
	if n, err := s.Seq(base64.StdEncoding.EncodeToString(carol.pub)); err != nil || n != 1 {
		t.Fatalf("seq high-water = %d, %v", n, err)
	}
	// direct ingest still enforces monotonicity past the replicated water
	carol.seq = 0
	r3, s3, e3, o3 := carol.msg(t, "post.create", map[string]string{"text": "local dup seq"})
	if _, _, err := s.Ingest(r3, s3, e3, o3); !errors.Is(err, ErrStaleSeq) {
		t.Fatalf("local seq reuse: %v", err)
	}
	carol.seq = 1
	r4, s4, e4, o4 := carol.msg(t, "post.create", map[string]string{"text": "local next"})
	if _, _, err := s.Ingest(r4, s4, e4, o4); err != nil {
		t.Fatal(err)
	}
	// a replicated message must never regress the high-water
	if n, _ := s.Seq(base64.StdEncoding.EncodeToString(carol.pub)); n != 2 {
		t.Fatalf("seq high-water = %d, want 2", n)
	}
}
