// Package replicate is the pulling half of hub aggregation (see PLAN.md):
// a background loop that drains each curated peer's /v1/replicate log page
// by page, verifies the hub signature on every page and the author
// signature on every envelope, mirrors embeds through the peer, and
// ingests what survives. Trust is the admin's peer.add — nothing here
// discovers peers or follows a peer's peers.
package replicate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"exehub/internal/api"
	"exehub/internal/envelope"
	"exehub/internal/identity"
	"exehub/internal/ipfs"
	"exehub/internal/store"
)

const (
	interval  = 30 * time.Second
	pageLimit = 200
	embedMax  = 8 << 20 // mirrors the upload cap
)

type Puller struct {
	St   *store.Store
	IPFS *ipfs.Client
	Self string // own hub id; self-peering is refused at ingest but guard anyway

	client *http.Client
}

// Run pulls forever. Call in a goroutine; it shares the process lifetime
// like the sweep loop.
func (p *Puller) Run() {
	p.client = &http.Client{Timeout: 30 * time.Second}
	for {
		peers, err := p.St.Peers()
		if err != nil {
			log.Printf("replicate: peers: %v", err)
		}
		for _, peer := range peers {
			if peer.Hub == p.Self {
				continue
			}
			if err := p.pull(peer); err != nil {
				log.Printf("replicate %s: %v", peer.Hub, err)
			}
		}
		time.Sleep(interval)
	}
}

// pull drains one peer: resolve+verify its pubkey once, then fetch pages
// until next stops advancing.
func (p *Puller) pull(peer store.Peer) error {
	base, err := envelope.ParseMultiaddr(peer.Addr)
	if err != nil {
		return fmt.Errorf("addr %q: %w", peer.Addr, err)
	}
	pub, err := p.peerKey(peer, base)
	if err != nil {
		return err
	}
	cursor := peer.Cursor
	for {
		page, err := p.fetchPage(base, pub, peer.Hub, cursor)
		if err != nil {
			return err
		}
		for _, m := range page.Messages {
			p.handle(m, peer.Hub, base)
		}
		if page.Next <= cursor {
			return nil // drained
		}
		cursor = page.Next
		if err := p.St.SetPeerCursor(peer.Hub, cursor); err != nil {
			return err
		}
	}
}

// peerKey returns the peer's verified public key, resolving it via
// GET /v1/hub on first contact. The fingerprint check binds the key to the
// admin-configured id, so a hijacked address can't impersonate the peer.
func (p *Puller) peerKey(peer store.Peer, base string) (ed25519.PublicKey, error) {
	if peer.PubKey != "" {
		b, err := base64.StdEncoding.DecodeString(peer.PubKey)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, errors.New("cached pubkey corrupt")
		}
		return ed25519.PublicKey(b), nil
	}
	resp, err := p.client.Get(base + "/v1/hub")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var info struct {
		ID     string `json:"id"`
		PubKey string `json:"pubkey"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&info); err != nil {
		return nil, fmt.Errorf("hub info: %w", err)
	}
	b, err := base64.StdEncoding.DecodeString(info.PubKey)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("hub info: bad pubkey")
	}
	pub := ed25519.PublicKey(b)
	if identity.Fingerprint(pub) != peer.Hub || info.ID != peer.Hub {
		return nil, fmt.Errorf("hub at %s identifies as %s, expected %s", base, info.ID, peer.Hub)
	}
	if err := p.St.SetPeerPubkey(peer.Hub, info.PubKey); err != nil {
		return nil, err
	}
	return pub, nil
}

// fetchPage gets one /v1/replicate page and verifies the hub signature
// over the exact payload bytes, the nonce echo, and the hub id.
func (p *Puller) fetchPage(base string, pub ed25519.PublicKey, hub string, after int64) (*api.ReplicatePayload, error) {
	nb := make([]byte, 16)
	rand.Read(nb)
	nonce := hex.EncodeToString(nb)
	resp, err := p.client.Get(base + "/v1/replicate?after=" + strconv.FormatInt(after, 10) +
		"&limit=" + strconv.Itoa(pageLimit) + "&nonce=" + nonce)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("replicate: %s: %s", resp.Status, b)
	}
	var out struct {
		Payload json.RawMessage `json:"payload"`
		Sig     []byte          `json:"sig"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if !ed25519.Verify(pub, append([]byte(envelope.ReplicatePrefix), out.Payload...), out.Sig) {
		return nil, errors.New("bad page signature")
	}
	pl := &api.ReplicatePayload{}
	if err := json.Unmarshal(out.Payload, pl); err != nil {
		return nil, err
	}
	if pl.Nonce != nonce || pl.Hub != hub {
		return nil, errors.New("page nonce/hub mismatch")
	}
	return pl, nil
}

// handle verifies and ingests one replicated message. Failures are logged
// and skipped, never fatal to the drain: the log is append-only, so a
// message rejected today would be rejected on every future pass too.
func (p *Puller) handle(m store.ReplMsg, hub, base string) {
	e, err := envelope.Parse(m.Envelope)
	if err != nil {
		log.Printf("replicate %s: bad envelope: %v", hub, err)
		return
	}
	if err := envelope.Verify(m.Envelope, m.Sig, e); err != nil {
		log.Printf("replicate %s: %s: %v", hub, envelope.MsgID(m.Envelope), err)
		return
	}
	op, err := e.Op()
	if err != nil {
		log.Printf("replicate %s: %s: %v", hub, envelope.MsgID(m.Envelope), err)
		return
	}
	// Local bans apply to replicated content; the gate and cooldown do not
	// (the admin chose to trust this peer's own policy).
	if banned, err := p.St.Banned(e.ProfileID()); err != nil || banned {
		return
	}
	switch v := op.(type) {
	case *envelope.PostCreate:
		for _, em := range v.Embeds {
			p.mirror(base, hub, em.CID, false)
		}
	case *envelope.ProfileSet:
		if v.Avatar != "" {
			p.mirror(base, hub, v.Avatar, true)
		}
	case *envelope.PostDelete:
		// no preparation needed
	default:
		// moderation and peer ops never replicate; the server filters
		// them, but dropping them here makes that this hub's policy
		// rather than the peer's courtesy
		return
	}
	_, unpin, err := p.St.IngestReplicated(m.Envelope, m.Sig, e, op, hub)
	switch {
	case errors.Is(err, store.ErrDuplicate), errors.Is(err, store.ErrStaleSeq):
		// duplicate: both hubs saw it (mutual peering, or a re-pull);
		// stale seq: the author wrote elsewhere first — first arrival wins
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrNotOwner):
		// a delete for a post we never had (banned author, or its parent
		// hub is not our peer) — nothing to remove
	case err != nil:
		log.Printf("replicate %s: ingest %s: %v", hub, envelope.MsgID(m.Envelope), err)
	}
	for _, cid := range unpin {
		if err := p.IPFS.Unpin(cid); err != nil {
			log.Printf("replicate %s: unpin %s: %v", hub, cid, err)
		}
	}
}

// mirror fetches one embed CID through the peer that referenced it —
// never an arbitrary gateway — size-capped, and keeps it only if local
// kubo mints the identical CID (both hubs add with the same params, so a
// mismatch means the bytes are not what the author signed for).
func (p *Puller) mirror(base, hub, cid string, avatar bool) {
	if _, err := p.St.PinInfo(cid); err == nil {
		return // already pinned here
	}
	resp, err := p.client.Get(base + "/v1/embed/" + cid)
	if err != nil {
		log.Printf("replicate %s: mirror %s: %v", hub, cid, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("replicate %s: mirror %s: %s", hub, cid, resp.Status)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, embedMax+1))
	if err != nil || len(body) > embedMax {
		log.Printf("replicate %s: mirror %s: oversized or truncated", hub, cid)
		return
	}
	got, err := p.IPFS.Add(strings.NewReader(string(body)), envelope.MsgID(body))
	if err != nil {
		log.Printf("replicate %s: mirror %s: %v", hub, cid, err)
		return
	}
	if got != cid {
		log.Printf("replicate %s: mirror %s: peer served different content (%s)", hub, cid, got)
		if err := p.IPFS.Unpin(got); err != nil {
			log.Printf("replicate %s: unpin %s: %v", hub, got, err)
		}
		return
	}
	mime := resp.Header.Get("Content-Type")
	if i := strings.Index(mime, ";"); i > 0 {
		mime = mime[:i]
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	if err := p.St.AddPin(cid, int64(len(body)), mime, avatar); err != nil {
		log.Printf("replicate %s: mirror %s: %v", hub, cid, err)
	}
}
