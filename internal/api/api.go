// Package api is exe-hub's HTTP surface. Reads are public (CORS *); writes
// authenticate by ed25519 signature alone — POST /v1/msg carries a signed
// envelope, POST /v1/upload a signed digest. Policy (gate, bans, admin)
// is enforced here at ingest; the store below assumes verified input.
package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

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

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hub", s.handleHub)
	mux.HandleFunc("POST /v1/msg", s.handleMsg)
	mux.HandleFunc("POST /v1/upload", s.handleUpload)
	mux.HandleFunc("GET /v1/feed", s.handleFeed)
	mux.HandleFunc("GET /v1/profile/{id}", s.handleProfile)
	mux.HandleFunc("GET /v1/profile/{id}/feed", s.handleProfileFeed)
	mux.HandleFunc("GET /v1/post/{id}", s.handlePost)
	mux.HandleFunc("GET /v1/embed/{cid}", s.handleEmbed)
	mux.HandleFunc("GET /v1/seq", s.handleSeq)
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
	if e.Type == "peer.add" || e.Type == "peer.remove" {
		writeErr(w, http.StatusNotImplemented, errors.New("peer ops are reserved for aggregation"))
		return
	}
	op, err := e.Op()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.policy(e); err != nil {
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
	case errors.Is(err, store.ErrNotOwner), errors.Is(err, store.ErrNoPin), errors.Is(err, store.ErrNotFound):
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

// policy enforces who may write: bans and the token gate for content ops,
// admin identity for moderation ops. post.delete passes untouched — one
// may always remove one's own posts (ownership is checked in the store).
func (s *Server) policy(e *envelope.Envelope) error {
	c := s.Cfg.Get()
	pid := e.ProfileID()
	admin := c.IsAdmin(pid)
	switch e.Type {
	case "ban.set", "ban.lift":
		if !admin {
			return errors.New("moderation requires an admin key")
		}
	case "post.create", "profile.set":
		banned, err := s.St.Banned(pid)
		if err != nil {
			return err
		}
		if banned {
			return errors.New("this key is banned from posting here")
		}
		if !admin {
			pub, _ := e.Pub()
			return s.Gate.Check(pub)
		}
	}
	return nil
}

// handleUpload accepts embed bytes under a signed authorization: the
// author signs UploadPrefix + ts + "\n" + hex sha256(body). Upload rides
// the same gate/ban policy as posting so the storage can't be used by
// anyone who couldn't post the result.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	author := r.Header.Get("X-Hub-Author")
	pubB, err := base64.StdEncoding.DecodeString(author)
	if err != nil || len(pubB) != ed25519.PublicKeySize {
		writeErr(w, http.StatusBadRequest, errors.New("X-Hub-Author must be a base64 ed25519 public key"))
		return
	}
	pub := ed25519.PublicKey(pubB)
	ms, err := strconv.ParseInt(r.Header.Get("X-Hub-Ts"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad X-Hub-Ts"))
		return
	}
	if d := time.Since(time.UnixMilli(ms)); d > uploadSkew || d < -uploadSkew {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("upload timestamp skew %s", d.Round(time.Second)))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxUpload))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("embeds are capped at %dMB", MaxUpload>>20))
		return
	}
	sum := envelope.MsgID(body) // hex sha256
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Hub-Sig"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad X-Hub-Sig encoding"))
		return
	}
	msg := []byte(envelope.UploadPrefix + r.Header.Get("X-Hub-Ts") + "\n" + sum)
	if !ed25519.Verify(pub, msg, sig) {
		writeErr(w, http.StatusUnauthorized, errors.New("bad upload signature"))
		return
	}

	pid := identity.Fingerprint(pub)
	c := s.Cfg.Get()
	if banned, err := s.St.Banned(pid); err != nil || banned {
		writeErr(w, http.StatusForbidden, errors.New("this key is banned from posting here"))
		return
	}
	if !c.IsAdmin(pid) {
		if err := s.Gate.Check(pub); err != nil {
			writeErr(w, http.StatusForbidden, err)
			return
		}
	}

	if err := s.IPFS.Available(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("IPFS backend unavailable: %v", err))
		return
	}
	// Sniff the real MIME; the declared one is advisory. The sniffed type
	// is what /v1/embed will serve with.
	mime := http.DetectContentType(body)
	if i := strings.Index(mime, ";"); i > 0 {
		mime = mime[:i]
	}
	cid, err := s.IPFS.Add(strings.NewReader(string(body)), sum)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := s.St.AddPin(cid, int64(len(body)), mime); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cid": cid, "mime": mime, "size": len(body)})
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
	posts, err := s.St.Feed(r.URL.Query().Get("before"), limitParam(r))
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
