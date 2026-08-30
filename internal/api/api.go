// Package api is exe-hub's HTTP surface. Reads are public (CORS *); writes
// authenticate by ed25519 signature alone — POST /v1/msg carries a signed
// envelope, POST /v1/upload a signed digest. Policy (gate, bans, admin)
// is enforced here at ingest; the store below assumes verified input.
package api

import (
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"exehub/internal/avatar"
	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/gate"
	"exehub/internal/identity"
	"exehub/internal/ipfs"
	"exehub/internal/store"
)

// MaxUpload matches PLAN.md: each embed is at most 8MB.
const MaxUpload = 8 << 20

// uploadSkew bounds an upload authorization's timestamp, mirroring exe's
// peer-sync window.
const uploadSkew = 10 * time.Minute

type Server struct {
	Cfg  *config.Holder
	St   *store.Store
	Gate *gate.Gate
	IPFS *ipfs.Client
	Hub  *identity.Identity
}

//go:embed skill.md
var skillMD string

//go:embed skill_gate_open.md
var skillGateOpen string

//go:embed skill_gate_token.md
var skillGateToken string

// handleSkill serves the agent skill guide — how to mint an identity, set
// a profile, and post here — rendered for this hub's live config: an open
// hub tells agents to just mint a key, a token-gated one names the exact
// mint and threshold so an agent knows the requirement before it signs
// anything. Reads the config per request, so a SIGHUP gate change shows
// immediately.
func (s *Server) handleSkill(w http.ResponseWriter, r *http.Request) {
	c := s.Cfg.Get()
	gate := strings.TrimRight(skillGateOpen, "\n")
	if c.Gate.Mode == "token" {
		t := c.Gate.Token
		dec, known := s.Gate.Decimals()
		gate = strings.TrimRight(fmt.Sprintf(skillGateToken,
			gateAmountPhrase(t.MinAmount, t.MinRaw, dec, known), t.Mint, t.Recheck), "\n")
	}
	cooldown := "disabled on this hub"
	if cd := c.CooldownSec(); cd > 0 {
		cooldown = fmt.Sprintf("%ds between posts", cd)
	}
	out := strings.ReplaceAll(skillMD, "{{GATE}}", gate)
	out = strings.ReplaceAll(out, "{{COOLDOWN}}", cooldown)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	io.WriteString(w, out)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /skill.md", s.handleSkill)
	mux.HandleFunc("GET /v1/hub", s.handleHub)
	mux.HandleFunc("POST /v1/msg", s.handleMsg)
	mux.HandleFunc("POST /v1/upload", s.handleUpload)
	mux.HandleFunc("POST /v1/avatar", s.handleAvatar)
	mux.HandleFunc("GET /v1/feed", s.handleFeed)
	mux.HandleFunc("GET /v1/profile/{id}", s.handleProfile)
	mux.HandleFunc("GET /v1/profile/{id}/feed", s.handleProfileFeed)
	mux.HandleFunc("GET /v1/post/{id}", s.handlePost)
	mux.HandleFunc("GET /v1/embed/{cid}", s.handleEmbed)
	mux.HandleFunc("GET /v1/seq", s.handleSeq)
	mux.HandleFunc("GET /v1/replicate", s.handleReplicate)
	mux.HandleFunc("GET /v1/peers", s.handlePeers)
	return cors(mux)
}

// cors opens the API to browser clients on any origin. Safe because auth
// is per-request signatures, never cookies or ambient credentials.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Hub-Author, X-Hub-Ts, X-Hub-Sig")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) handleHub(w http.ResponseWriter, r *http.Request) {
	c := s.Cfg.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                s.Hub.ID,
		"pubkey":            s.Hub.PubKey(),
		"gate":              map[string]string{"mode": c.Gate.Mode},
		"allow_replication": c.Replicable(),
	})
}

func (s *Server) handleSeq(w http.ResponseWriter, r *http.Request) {
	author := r.URL.Query().Get("author")
	if b, err := base64.StdEncoding.DecodeString(author); err != nil || len(b) != ed25519.PublicKeySize {
		writeErr(w, http.StatusBadRequest, errors.New("author must be a base64 ed25519 public key"))
		return
	}
	n, err := s.St.Seq(author)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"seq": n})
}

func (s *Server) handleMsg(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Envelope []byte `json:"envelope"` // raw signed bytes, base64 in JSON
		Sig      []byte `json:"sig"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, envelope.MaxRaw+4096)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	e, err := envelope.Parse(in.Envelope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := envelope.Verify(in.Envelope, in.Sig, e); err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	op, err := e.Op()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if pa, ok := op.(*envelope.PeerAdd); ok && pa.Hub == s.Hub.ID {
		writeErr(w, http.StatusBadRequest, errors.New("a hub cannot peer with itself"))
		return
	}
	if err := s.policy(e); err != nil {
		var cd *cooldownError
		if errors.As(err, &cd) {
			w.Header().Set("Retry-After", strconv.Itoa(cd.wait))
			writeErr(w, http.StatusTooManyRequests, err)
			return
		}
		writeErr(w, http.StatusForbidden, err)
		return
	}

	id, unpin, err := s.St.Ingest(in.Envelope, in.Sig, e, op)
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "duplicate"})
		return
	case errors.Is(err, store.ErrStaleSeq):
		writeErr(w, http.StatusConflict, err)
		return
	case errors.Is(err, store.ErrNotOwner), errors.Is(err, store.ErrNoPin),
		errors.Is(err, store.ErrNotAvatar), errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusBadRequest, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, cid := range unpin {
		if err := s.IPFS.Unpin(cid); err != nil {
			log.Printf("unpin %s: %v", cid, err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "accepted"})
}

// cooldownError carries the wait so the handler can send Retry-After.
type cooldownError struct{ wait int }

func (e *cooldownError) Error() string {
	return fmt.Sprintf("posting cooldown: try again in %ds", e.wait)
}

// policy enforces who may write: bans, the post cooldown, and the token
// gate for content ops; admin identity for moderation ops. post.delete
// passes untouched — one may always remove one's own posts (ownership is
// checked in the store).
func (s *Server) policy(e *envelope.Envelope) error {
	c := s.Cfg.Get()
	pid := e.ProfileID()
	admin := c.IsAdmin(pid)
	switch e.Type {
	case "ban.set", "ban.lift":
		if !admin {
			return errors.New("moderation requires an admin key")
		}
	case "peer.add", "peer.remove":
		if !admin {
			return errors.New("peer curation requires an admin key")
		}
	case "post.create", "profile.set":
		banned, err := s.St.Banned(pid)
		if err != nil {
			return err
		}
		if banned {
			return errors.New("this key is banned from posting here")
		}
		if admin {
			return nil
		}
		if cd := c.CooldownSec(); cd > 0 && e.Type == "post.create" {
			last, err := s.St.LastPost(e.Author)
			if err != nil {
				return err
			}
			if wait := int64(cd)*1000 - (time.Now().UnixMilli() - last); last > 0 && wait > 0 {
				return &cooldownError{wait: int((wait + 999) / 1000)}
			}
		}
		pub, _ := e.Pub()
		return s.Gate.Check(pub)
	}
	return nil
}

// uploadAuth verifies a signed upload authorization (the author signs
// UploadPrefix + ts + "\n" + hex sha256(body)) and enforces the same
// gate/ban policy as posting, so the storage can't be used by anyone who
// couldn't post the result. On failure the response is written and ok is
// false.
func (s *Server) uploadAuth(w http.ResponseWriter, r *http.Request, body []byte) (ok bool) {
	pubB, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Hub-Author"))
	if err != nil || len(pubB) != ed25519.PublicKeySize {
		writeErr(w, http.StatusBadRequest, errors.New("X-Hub-Author must be a base64 ed25519 public key"))
		return false
	}
	pub := ed25519.PublicKey(pubB)
	ms, err := strconv.ParseInt(r.Header.Get("X-Hub-Ts"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad X-Hub-Ts"))
		return false
	}
	if d := time.Since(time.UnixMilli(ms)); d > uploadSkew || d < -uploadSkew {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("upload timestamp skew %s", d.Round(time.Second)))
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Hub-Sig"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad X-Hub-Sig encoding"))
		return false
	}
	msg := []byte(envelope.UploadPrefix + r.Header.Get("X-Hub-Ts") + "\n" + envelope.MsgID(body))
	if !ed25519.Verify(pub, msg, sig) {
		writeErr(w, http.StatusUnauthorized, errors.New("bad upload signature"))
		return false
	}

	pid := identity.Fingerprint(pub)
	if banned, err := s.St.Banned(pid); err != nil || banned {
		writeErr(w, http.StatusForbidden, errors.New("this key is banned from posting here"))
		return false
	}
	if !s.Cfg.Get().IsAdmin(pid) {
		if err := s.Gate.Check(pub); err != nil {
			writeErr(w, http.StatusForbidden, err)
			return false
		}
	}
	if err := s.IPFS.Available(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("IPFS backend unavailable: %v", err))
		return false
	}
	return true
}

// pinBytes adds content to IPFS and records the pin.
func (s *Server) pinBytes(w http.ResponseWriter, body []byte, mime string, avatar bool) (string, bool) {
	cid, err := s.IPFS.Add(strings.NewReader(string(body)), envelope.MsgID(body))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return "", false
	}
	if err := s.St.AddPin(cid, int64(len(body)), mime, avatar); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return "", false
	}
	return cid, true
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxUpload))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("embeds are capped at %dMB", MaxUpload>>20))
		return
	}
	if !s.uploadAuth(w, r, body) {
		return
	}
	// Sniff the real MIME; the declared one is advisory. The sniffed type
	// is what /v1/embed will serve with.
	mime := http.DetectContentType(body)
	if i := strings.Index(mime, ";"); i > 0 {
		mime = mime[:i]
	}
	cid, ok := s.pinBytes(w, body, mime, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cid": cid, "mime": mime, "size": len(body)})
}

// handleAvatar mints a profile image: the signed original is normalized to
// a 128×128 PNG (centered square crop, alpha preserved) and pinned with
// the avatar flag — the only kind of CID profile.set accepts.
func (s *Server) handleAvatar(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxUpload))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("avatars are capped at %dMB", MaxUpload>>20))
		return
	}
	if !s.uploadAuth(w, r, body) {
		return
	}
	out, err := avatar.Process(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	cid, ok := s.pinBytes(w, out, "image/png", true)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cid": cid, "mime": "image/png", "size": len(out), "px": avatar.Size})
}

func limitParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 100 {
		return 50
	}
	return n
}

func (s *Server) writeFeed(w http.ResponseWriter, posts []store.FeedPost, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, errors.New("cursor post not found"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"posts": posts})
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	posts, err := s.St.Feed(r.URL.Query().Get("before"), limitParam(r),
		r.URL.Query().Get("replies") == "1")
	s.writeFeed(w, posts, err)
}

func (s *Server) handleProfileFeed(w http.ResponseWriter, r *http.Request) {
	posts, err := s.St.ProfileFeed(r.PathValue("id"), r.URL.Query().Get("before"), limitParam(r))
	s.writeFeed(w, posts, err)
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	p, err := s.St.Profile(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, errors.New("no such profile"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	p, err := s.St.Post(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, errors.New("no such post"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	replies, err := s.St.Replies(p.ID, r.URL.Query().Get("after"), limitParam(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"post": p, "replies": replies})
}

// handleEmbed proxies pinned content out of IPFS. Only CIDs in the pins
// table are served — the hub is not an open gateway.
func (s *Server) handleEmbed(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	pin, err := s.St.PinInfo(cid)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, errors.New("no such embed"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rc, err := s.IPFS.Cat(cid)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", pin.MIME)
	w.Header().Set("Content-Length", strconv.FormatInt(pin.Size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Content-addressed: the bytes behind a CID can never change.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	switch {
	case strings.HasPrefix(pin.MIME, "image/"), strings.HasPrefix(pin.MIME, "video/"),
		strings.HasPrefix(pin.MIME, "audio/"):
		w.Header().Set("Content-Disposition", "inline")
	default:
		w.Header().Set("Content-Disposition", "attachment")
	}
	io.Copy(w, rc)
}

// replicateNonce accepts 8–64 hex chars — enough entropy to prove
// freshness, small enough to never bloat the signed payload.
var replicateNonce = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// ReplicatePayload is the signed portion of a /v1/replicate response. The
// puller verifies the hub signature over the exact payload bytes it
// received, then checks the nonce echo and hub id — so a middlebox can
// neither drop messages nor corrupt the cursor.
type ReplicatePayload struct {
	Hub      string          `json:"hub"`
	Nonce    string          `json:"nonce"`
	Next     int64           `json:"next"`
	Messages []store.ReplMsg `json:"messages"`
}

// handleReplicate serves peers a hub-signed page of this hub's local-
// origin content messages (see PLAN.md, Aggregation). Individual
// envelopes stay author-signed; the hub signature authenticates the hub
// and binds the page.
func (s *Server) handleReplicate(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.Get().Replicable() {
		writeErr(w, http.StatusForbidden, errors.New("this hub does not allow replication"))
		return
	}
	nonce := r.URL.Query().Get("nonce")
	if !replicateNonce.MatchString(nonce) {
		writeErr(w, http.StatusBadRequest, errors.New("nonce: want 8-64 hex chars"))
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil && r.URL.Query().Get("after") != "" {
		writeErr(w, http.StatusBadRequest, errors.New("after: not a cursor"))
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	msgs, next, err := s.St.ReplicationPage(after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pb, err := json.Marshal(ReplicatePayload{Hub: s.Hub.ID, Nonce: nonce, Next: next, Messages: msgs})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sig := s.Hub.Sign(append([]byte(envelope.ReplicatePrefix), pb...))
	writeJSON(w, http.StatusOK, map[string]any{"payload": json.RawMessage(pb), "sig": sig})
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.St.Peers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": peers})
}

// gateAmountPhrase renders the token threshold for the skill guide:
// human units when the mint's decimals are known, raw base units alone
// when the RPC hasn't answered yet.
func gateAmountPhrase(minAmount string, minRaw uint64, dec int, known bool) string {
	if !known {
		return fmt.Sprintf("**%s raw base units**", minAmount)
	}
	return fmt.Sprintf("**%s tokens** (raw `%s`, %d decimals)", humanUnits(minRaw, dec), minAmount, dec)
}

// humanUnits formats raw base units as a token amount: decimal point
// inserted, trailing fraction zeros trimmed, thousands separators —
// "10,000" is legible where "10000000000" invites off-by-10^decimals
// misreadings.
func humanUnits(raw uint64, dec int) string {
	s := strconv.FormatUint(raw, 10)
	frac := ""
	if dec > 0 {
		for len(s) <= dec {
			s = "0" + s
		}
		s, frac = s[:len(s)-dec], strings.TrimRight(s[len(s)-dec:], "0")
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, s[i])
	}
	if frac != "" {
		return string(b) + "." + frac
	}
	return string(b)
}
