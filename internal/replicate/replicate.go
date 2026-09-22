// Package replicate is the pulling half of hub aggregation (see PLAN.md):
// a background loop that drains each curated peer's /v1/replicate log page
// by page, verifies the hub signature on every page and the author
// signature on every envelope, mirrors embeds through the peer (and goes
// back, to any peer, for the ones that failed), and ingests what survives. Trust is the admin's peer.add — nothing here
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
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"exehub/internal/api"
	"exehub/internal/envelope"
	"exehub/internal/events"
	"exehub/internal/identity"
	"exehub/internal/ipfs"
	"exehub/internal/lang"
	"exehub/internal/media"
	"exehub/internal/store"
)

const (
	interval  = 30 * time.Second
	pageLimit = 200
	embedMax  = 8 << 20   // mirrors the upload cap
	healMax   = time.Hour // the longest wait between two rounds for one file
)

type Puller struct {
	St   *store.Store
	IPFS *ipfs.Client
	Self string // own hub id; self-peering is refused at ingest but guard anyway
	// Bus, when set, is told of each translation taken from a peer, so a
	// live page brings it in like one made here
	Bus *events.Broadcaster
	// the peers that have no /v1/translations, said once each
	noTranslations map[string]bool

	client    *http.Client
	healing   map[string]*healTry // CID → when its sources are asked next
	healPeers map[string]bool     // the peers heal saw last cycle
}

type healTry struct {
	at   time.Time
	wait time.Duration
}

// peerDown is a mirror that failed because the peer did not answer at all,
// kuboDown one that failed on local kubo: neither says anything about the
// file, and heal treats them differently from a peer's "no such embed".
type (
	peerDown struct{ error }
	kuboDown struct{ error }
)

// Run pulls forever. Call in a goroutine; it shares the process lifetime
// like the sweep loop.
func (p *Puller) Run() {
	p.client = &http.Client{Timeout: 30 * time.Second}
	for {
		p.round()
		time.Sleep(interval)
	}
}

// round is one cycle: every peer's messages and then its translations,
// the translations that were waiting for a post that has come by now,
// and the files still to mirror.
func (p *Puller) round() {
	peers, err := p.St.Peers()
	if err != nil {
		log.Printf("replicate: peers: %v", err)
	}
	var up []store.Peer // the peers that answered this cycle: the ones heal may ask
	for _, peer := range peers {
		if peer.Hub == p.Self {
			continue
		}
		if err := p.pull(peer); err != nil {
			log.Printf("replicate %s: %v", peer.Hub, err)
			continue
		}
		up = append(up, peer)
		// after its messages, so the posts a page's translations are
		// of are mostly here; the rest wait (see take)
		if err := p.pullTranslations(peer); err != nil {
			log.Printf("replicate %s: translations: %v", peer.Hub, err)
		}
	}
	p.settle("")
	p.heal(up)
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

// pullTranslations takes the translations a peer made itself (PLAN.md,
// Translations — one hub pays, its peers take): its hub-signed pages
// from this hub's cursor on, each translation kept only if this hub
// holds the post, keeps posts in that language, and the words pass this
// hub's own Check against its own copy of the post after its own
// FullWidth — a peer can offer a bad translation, as a model can, never
// one the checks would have refused — and only when it is newer than
// what is kept. The cursor moves past a page whatever became of what it
// held, so what is passed over is passed for good, and that is right for
// one the checks refuse: they would refuse it on every later pass too.
// It is wrong for one whose post is not here yet, which the first cut
// dropped the same way (Codex's catch): that one is set aside, and
// taken when its post comes (see take, settle). A peer from before
// /v1/translations answers 404 and is left alone.
func (p *Puller) pullTranslations(peer store.Peer) error {
	base, err := envelope.ParseMultiaddr(peer.Addr)
	if err != nil {
		return err
	}
	pub, err := p.peerKey(peer, base)
	if err != nil {
		return err
	}
	cursor, taken, aside := peer.TrCursor, 0, 0
	defer func() {
		if taken > 0 {
			log.Printf("replicate %s: took %d translations", peer.Hub, taken)
		}
		if aside > 0 {
			log.Printf("replicate %s: set %d translations aside for posts not here yet", peer.Hub, aside)
		}
	}()
	for {
		page, err := p.fetchTranslations(base, pub, peer.Hub, cursor)
		if errors.Is(err, errNoTranslations) {
			if !p.noTranslations[peer.Hub] {
				if p.noTranslations == nil {
					p.noTranslations = map[string]bool{}
				}
				p.noTranslations[peer.Hub] = true
				log.Printf("replicate %s: serves no translations", peer.Hub)
			}
			return nil
		}
		if err != nil {
			return err
		}
		for _, t := range page.Translations {
			switch p.take(peer.Hub, t) {
			case kept:
				taken++
			case waits:
				aside++
			}
		}
		if page.Next <= cursor {
			return nil // drained
		}
		cursor = page.Next
		if err := p.St.SetPeerTranslationCursor(peer.Hub, cursor); err != nil {
			return err
		}
	}
}

// translationMax bounds a translation's size: several times a post's,
// since English runs to a few bytes for each of Chinese's.
const translationMax = 8 * envelope.MaxText

// What became of one translation a peer offered.
type taking int

const (
	kept   taking = iota // it is this hub's translation of the post now
	passed               // not kept, for good: malformed, refused by Check, or no newer than what is kept
	waits                // its post is not here yet: set aside until it is
	failed               // the store failed: nothing is known, try again
)

// A peer can name posts that will never come — written on a hub this
// one does not pull from, or deleted since — so what waits is bounded:
// so many to a peer, the longest-waiting dropped first, and so long.
const (
	pendingMax = 2000
	pendingAge = 30 * 24 * time.Hour
)

// take keeps one translation from a peer if it may be kept. One of a
// post this hub does not hold is no refusal: the peer translates every
// post it holds and serves only the posts written on it, so the words
// can come from one peer and the post from another, or a round later. It
// is set aside, well-formed, and settle takes it when the post is here.
func (p *Puller) take(hub string, t store.SharedTranslation) taking {
	if !slices.Contains(lang.Targets, t.Lang) || t.Text == "" || len(t.Text) > translationMax || !utf8.ValidString(t.Text) ||
		len(t.Post) != 64 || strings.Trim(t.Post, "0123456789abcdef") != "" {
		return passed
	}
	if len(t.Model) > 200 {
		t.Model = t.Model[:200]
	}
	src, from, held, err := p.St.PostText(t.Post)
	if err != nil {
		return failed
	}
	if !held {
		dropped, err := p.St.SetPendingTranslation(hub, t, pendingMax)
		if err != nil {
			log.Printf("replicate %s: translation of %s set aside: %v", hub, t.Post, err)
			return failed
		}
		if dropped > 0 {
			log.Printf("replicate %s: %d translations waiting longest for their posts dropped: more than %d wait", hub, dropped, pendingMax)
		}
		return waits
	}
	if from == t.Lang {
		return passed // the post is in that language already
	}
	t.Text = lang.Tidy(t.Lang, t.Text) // the punctuation rule for its language, as the maker would have run it
	if err := lang.Check(src, t.Text, t.Lang); err != nil {
		log.Printf("replicate %s: translation of %s to %s refused: %v", hub, t.Post, t.Lang, err)
		return passed
	}
	ok, err := p.St.AcceptTranslation(hub, t)
	if err != nil {
		log.Printf("replicate %s: translation of %s: %v", hub, t.Post, err)
		return failed
	}
	if !ok {
		return passed
	}
	if p.Bus != nil {
		p.Bus.Emit(events.Event{Type: "post.translation", ID: t.Post})
	}
	return kept
}

// settle tries the translations that were set aside for a post, now
// that it is here: for one post, the moment a pulled post.create is
// kept, or with "" for every post that has come by any way, at the end
// of each round. Tried means taken like any other, and what take decides
// then is final — kept, or passed for good — so the row goes either
// way; only a store that failed leaves it for the next round. What has
// waited longer than pendingAge goes unasked.
func (p *Puller) settle(post string) {
	if post == "" {
		if n, err := p.St.AgePendingTranslations(time.Now().Add(-pendingAge).UnixMilli()); err != nil {
			log.Printf("replicate: translations set aside: %v", err)
		} else if n > 0 {
			log.Printf("replicate: %d translations waited %s for posts that never came, dropped", n, pendingAge)
		}
	}
	waiting, err := p.St.PendingTranslations(post, 500)
	if err != nil {
		log.Printf("replicate: translations set aside: %v", err)
		return
	}
	n := 0
	for _, w := range waiting { // the newest first, so an older one of the same post finds it kept and passes
		got := p.take(w.Peer, w.SharedTranslation)
		if got == failed || got == waits {
			continue
		}
		if got == kept {
			n++
		}
		if err := p.St.DropPendingTranslation(w.Peer, w.Post, w.Lang); err != nil {
			log.Printf("replicate: translations set aside: %v", err)
		}
	}
	if n > 0 {
		log.Printf("replicate: took %d translations that had waited for their posts", n)
	}
}

var errNoTranslations = errors.New("peer has no /v1/translations")

// fetchTranslations gets one /v1/translations page, verified like a
// /v1/replicate one under its own prefix.
func (p *Puller) fetchTranslations(base string, pub ed25519.PublicKey, hub string, after int64) (*api.TranslationsPayload, error) {
	nb := make([]byte, 16)
	rand.Read(nb)
	nonce := hex.EncodeToString(nb)
	resp, err := p.client.Get(base + "/v1/translations?after=" + strconv.FormatInt(after, 10) +
		"&limit=" + strconv.Itoa(pageLimit) + "&nonce=" + nonce)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoTranslations
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s", resp.Status, b)
	}
	var out struct {
		Payload json.RawMessage `json:"payload"`
		Sig     []byte          `json:"sig"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return nil, err
	}
	if !ed25519.Verify(pub, append([]byte(envelope.TranslationsPrefix), out.Payload...), out.Sig) {
		return nil, errors.New("bad page signature")
	}
	pl := &api.TranslationsPayload{}
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
		// a mirror that fails here is heal's to try again
		for _, em := range v.Embeds {
			p.mirrorNow(base, hub, em.CID, false)
			if em.Poster != "" {
				p.mirrorNow(base, hub, em.Poster, false)
			}
		}
	case *envelope.ProfileSet:
		if v.Avatar != "" {
			p.mirrorNow(base, hub, v.Avatar, true)
		}
	case *envelope.PostDelete:
		// no preparation needed
	default:
		// moderation and peer ops never replicate; the server filters
		// them, but dropping them here makes that this hub's policy
		// rather than the peer's courtesy
		return
	}
	id, unpin, err := p.St.IngestReplicated(m.Envelope, m.Sig, e, op, hub)
	if _, post := op.(*envelope.PostCreate); post && err == nil {
		p.settle(id) // the translations that came before it, from any peer
	}
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

// mirrorNow mirrors a CID as its message is ingested, from the peer the
// message came through.
func (p *Puller) mirrorNow(base, hub, cid string, avatar bool) {
	if err := p.mirror(base, cid, avatar, false); err != nil {
		log.Printf("replicate %s: mirror %s: %v", hub, cid, err)
	}
}

// mirror fetches one CID from a configured peer — never an arbitrary
// gateway — size-capped, and keeps it only if local kubo mints the
// identical CID (both hubs add with the same params, so a mismatch means
// the bytes are not what the author signed for). Nothing but the bytes is
// taken from the peer: the type they are served with is read from them
// here, the way an upload's is (media.Sniff), not from the peer's
// Content-Type — which is what makes a peer that never named the file as
// safe to ask as the one that did. nil means the CID is pinned here now.
// late marks heal's retry, after the message that names the CID has been
// ingested: the pin then starts with the references it already has
// (AdoptPin).
func (p *Puller) mirror(base, cid string, avatar, late bool) error {
	if _, err := p.St.PinInfo(cid); err == nil {
		return nil // already pinned here
	}
	resp, err := p.client.Get(base + "/v1/embed/" + cid)
	if err != nil {
		return peerDown{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New(resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, embedMax+1))
	if err != nil || len(body) > embedMax {
		return errors.New("oversized or truncated")
	}
	got, err := p.IPFS.Add(strings.NewReader(string(body)), envelope.MsgID(body))
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return kuboDown{err}
		}
		return err
	}
	if got != cid {
		if err := p.IPFS.Unpin(got); err != nil {
			log.Printf("replicate: unpin %s: %v", got, err)
		}
		return fmt.Errorf("peer served different content (%s)", got)
	}
	add := p.St.AddPin
	if late {
		add = p.St.AdoptPin
	}
	return add(cid, int64(len(body)), media.Sniff(body), avatar)
}

// heal tries the failed mirrors again. A replicated post lands even when
// its picture could not be fetched — local kubo not up yet (a hub that
// boots before its kubo pulls its first page without one), the peer slow —
// and no later message brings the picture back, so the embed stayed a 404
// for good. Each cycle asks the store which CIDs replicated messages name
// without a pin, and a file whose turn has come has its sources asked in
// one round: first the peers its messages came through, then every other
// configured peer — one that mirrored the post may hold the file without
// ever having sent it here, and mirror takes nothing from a peer but bytes
// that must hash to the signed CID. The first copy that checks out ends
// the round. Only a round in which every source failed backs the file
// off, from the next cycle doubling to healMax, so a file that is lost
// everywhere costs one request per peer an hour. What says nothing about
// the file does not count: with local kubo down heal waits for it, and
// peers are the ones whose pull just succeeded — a peer that stops
// answering halfway is left alone for the rest of the cycle, and a round
// in which no peer answered at all is simply held again next cycle. A
// peer that was not there last cycle, new or back from an outage, starts
// every wait over, so it is asked now rather than within the hour.
func (p *Puller) heal(peers []store.Peer) {
	missing, err := p.St.MissingMirrors()
	if err != nil {
		log.Printf("replicate: heal: %v", err)
		return
	}
	if p.healing == nil {
		p.healing = map[string]*healTry{}
	}

	// the peers there are to ask, in the store's order
	var hubs []string
	bases := map[string]string{}
	for _, peer := range peers {
		if base, err := envelope.ParseMultiaddr(peer.Addr); err == nil && peer.Hub != p.Self && bases[peer.Hub] == "" {
			hubs = append(hubs, peer.Hub)
			bases[peer.Hub] = base
		}
	}
	seen := map[string]bool{}
	for _, hub := range hubs {
		seen[hub] = true
		if !p.healPeers[hub] {
			clear(p.healing)
		}
	}
	p.healPeers = seen

	// one file per CID, with the peers that named it
	type file struct {
		cid    string
		avatar bool
		named  []string
	}
	var files []*file
	byCID := map[string]*file{}
	for _, m := range missing {
		f := byCID[m.CID]
		if f == nil {
			f = &file{cid: m.CID}
			byCID[m.CID] = f
			files = append(files, f)
		}
		f.avatar = f.avatar || m.Avatar
		f.named = append(f.named, m.Origin)
	}
	for cid := range p.healing {
		if byCID[cid] == nil {
			delete(p.healing, cid) // its post was deleted meanwhile
		}
	}
	if len(hubs) == 0 {
		return // nowhere trusted to ask
	}

	now := time.Now()
	down := map[string]bool{} // peers that did not answer this cycle
	kubo := false             // local kubo answered this cycle
	for _, f := range files {
		try := p.healing[f.cid]
		if try != nil && now.Before(try.at) {
			continue
		}
		if !kubo {
			if err := p.IPFS.Available(); err != nil {
				return // nothing can be kept; the files' waits stand still
			}
			kubo = true
		}
		var sources []string
		asked := map[string]bool{}
		for _, hub := range append(f.named, hubs...) {
			if bases[hub] != "" && !asked[hub] {
				asked[hub] = true
				sources = append(sources, hub)
			}
		}
		var fails []string
		healed, answered := false, false
		for _, hub := range sources {
			if down[hub] {
				continue
			}
			err := p.mirror(bases[hub], f.cid, f.avatar, true)
			if err == nil {
				log.Printf("replicate %s: mirror %s: healed", hub, f.cid)
				healed = true
				break
			}
			if errors.As(err, &kuboDown{}) {
				log.Printf("replicate: heal %s: %v", f.cid, err)
				return
			}
			if errors.As(err, &peerDown{}) {
				down[hub] = true
			} else {
				answered = true
			}
			fails = append(fails, hub+": "+err.Error())
		}
		if healed {
			delete(p.healing, f.cid)
			continue
		}
		if !answered {
			continue
		}
		if try == nil {
			try = &healTry{wait: interval / 2}
			p.healing[f.cid] = try
		}
		try.wait = min(try.wait*2, healMax)
		try.at = now.Add(try.wait - time.Second) // a cycle is never exactly interval apart
		log.Printf("replicate: heal %s: %s; next round in %s", f.cid, strings.Join(fails, "; "), try.wait)
	}
}
