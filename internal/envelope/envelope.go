// Package envelope defines exe-hub's signed message format. Clients
// serialize an envelope once, sign those exact bytes, and send
// {envelope, sig}; the hub verifies over the received bytes and stores
// them verbatim — no canonical-JSON re-serialization anywhere. The message
// id is the sha256 of the envelope bytes.
package envelope

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"exehub/internal/identity"
)

// Prefix domain-separates hub signatures from every other use of the same
// key: exe's peer-sync signatures always start with an HTTP method, so the
// two protocols can never accept each other's signatures.
const Prefix = "exe-hub:v1\n"

// UploadPrefix domain-separates upload authorizations (which sign a body
// digest, not an envelope) from envelope signatures.
const UploadPrefix = "exe-hub:v1\nupload\n"

// ReplicatePrefix domain-separates the hub identity's signature over a
// /v1/replicate response payload from every author-key use of ed25519.
const ReplicatePrefix = "exe-hub:v1\nreplicate\n"

// MaxRaw caps the serialized envelope. Post text is capped separately;
// this bounds the whole message including embeds metadata.
const MaxRaw = 64 << 10

type Envelope struct {
	Type   string          `json:"type"`
	Author string          `json:"author"` // base64 raw ed25519 public key
	Seq    int64           `json:"seq"`    // strictly increasing per author
	TS     int64           `json:"ts"`     // client claim, ms; display only
	Body   json.RawMessage `json:"body"`
}

type Embed struct {
	CID      string `json:"cid"`
	MIME     string `json:"mime"`
	Filename string `json:"filename,omitempty"`
	Alt      string `json:"alt,omitempty"`
}

type ProfileSet struct {
	Name   string `json:"name"`
	Bio    string `json:"bio,omitempty"`
	Avatar string `json:"avatar,omitempty"` // embed CID
}

type PostCreate struct {
	Text    string  `json:"text"`
	ReplyTo string  `json:"reply_to,omitempty"` // parent post id (msg id)
	Embeds  []Embed `json:"embeds,omitempty"`
}

type PostDelete struct {
	Post string `json:"post"`
}

// BanSet / BanLift target a profile id (pubkey fingerprint), admin-only.
type BanSet struct {
	Target string `json:"target"`
	Reason string `json:"reason,omitempty"`
}

type BanLift struct {
	Target string `json:"target"`
}

// PeerAdd / PeerRemove curate the hubs this hub replicates from,
// admin-only. Hub is the remote hub identity's fingerprint; Addr an HTTP
// multiaddr profile (see ParseMultiaddr).
type PeerAdd struct {
	Hub  string `json:"hub"`
	Addr string `json:"addr"`
}

type PeerRemove struct {
	Hub string `json:"hub"`
}

// Parse validates the envelope's frame; per-type body validation happens
// in Op. It does not verify the signature — call Verify with the same raw
// bytes.
func Parse(raw []byte) (*Envelope, error) {
	if len(raw) == 0 || len(raw) > MaxRaw {
		return nil, fmt.Errorf("envelope size %d (max %d)", len(raw), MaxRaw)
	}
	e := &Envelope{}
	if err := json.Unmarshal(raw, e); err != nil {
		return nil, err
	}
	if e.Seq <= 0 {
		return nil, errors.New("seq must be positive")
	}
	if _, err := e.Pub(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Envelope) Pub() (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(e.Author)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("author is not a base64 ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// ProfileID is the author's canonical id (pubkey fingerprint).
func (e *Envelope) ProfileID() string {
	pub, _ := e.Pub()
	return identity.Fingerprint(pub)
}

// Verify checks sig over the domain-prefixed raw envelope bytes.
func Verify(raw, sig []byte, e *Envelope) error {
	pub, err := e.Pub()
	if err != nil {
		return err
	}
	msg := append([]byte(Prefix), raw...)
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("bad signature")
	}
	return nil
}

// MsgID is hex sha256 of the raw envelope bytes; it doubles as the post id
// for post.create messages.
func MsgID(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Content limits. Bytes, not runes — they bound storage, not typography.
const (
	MaxText     = 8 << 10
	MaxName     = 64
	MaxBio      = 1024
	MaxAlt      = 512
	MaxFilename = 128
	MaxMIME     = 128
	MaxEmbeds   = 4
	MaxReason   = 512
)

// Op decodes and validates the envelope's body for its type. Unknown types
// are rejected here; reserved-but-unimplemented types are the API layer's
// concern.
func (e *Envelope) Op() (any, error) {
	switch e.Type {
	case "profile.set":
		v := &ProfileSet{}
		if err := strictBody(e.Body, v); err != nil {
			return nil, err
		}
		if v.Name == "" || len(v.Name) > MaxName || !utf8.ValidString(v.Name) {
			return nil, errors.New("profile name: required, at most 64 bytes")
		}
		if len(v.Bio) > MaxBio || !utf8.ValidString(v.Bio) {
			return nil, errors.New("profile bio too long")
		}
		if v.Avatar != "" && !validCID(v.Avatar) {
			return nil, errors.New("avatar: not a CID")
		}
		return v, nil
	case "post.create":
		v := &PostCreate{}
		if err := strictBody(e.Body, v); err != nil {
			return nil, err
		}
		if v.Text == "" && len(v.Embeds) == 0 {
			return nil, errors.New("empty post")
		}
		if len(v.Text) > MaxText || !utf8.ValidString(v.Text) {
			return nil, errors.New("post text too long")
		}
		if v.ReplyTo != "" && !validMsgID(v.ReplyTo) {
			return nil, errors.New("reply_to: not a post id")
		}
		if len(v.Embeds) > MaxEmbeds {
			return nil, fmt.Errorf("at most %d embeds", MaxEmbeds)
		}
		for _, em := range v.Embeds {
			if !validCID(em.CID) {
				return nil, errors.New("embed cid invalid")
			}
			if em.MIME == "" || len(em.MIME) > MaxMIME {
				return nil, errors.New("embed mime: required")
			}
			if len(em.Filename) > MaxFilename || len(em.Alt) > MaxAlt {
				return nil, errors.New("embed filename/alt too long")
			}
		}
		return v, nil
	case "post.delete":
		v := &PostDelete{}
		if err := strictBody(e.Body, v); err != nil {
			return nil, err
		}
		if !validMsgID(v.Post) {
			return nil, errors.New("post: not a post id")
		}
		return v, nil
	case "ban.set":
		v := &BanSet{}
		if err := strictBody(e.Body, v); err != nil {
			return nil, err
		}
		if !validProfileID(v.Target) {
			return nil, errors.New("target: not a profile id")
		}
		if len(v.Reason) > MaxReason {
			return nil, errors.New("reason too long")
		}
		return v, nil
	case "ban.lift":
		v := &BanLift{}
		if err := strictBody(e.Body, v); err != nil {
			return nil, err
		}
		if !validProfileID(v.Target) {
			return nil, errors.New("target: not a profile id")
		}
		return v, nil
	case "peer.add":
		v := &PeerAdd{}
		if err := strictBody(e.Body, v); err != nil {
			return nil, err
		}
		if !validProfileID(v.Hub) {
			return nil, errors.New("hub: not a hub id")
		}
		if _, err := ParseMultiaddr(v.Addr); err != nil {
			return nil, fmt.Errorf("addr: %w", err)
		}
		return v, nil
	case "peer.remove":
		v := &PeerRemove{}
		if err := strictBody(e.Body, v); err != nil {
			return nil, err
		}
		if !validProfileID(v.Hub) {
			return nil, errors.New("hub: not a hub id")
		}
		return v, nil
	}
	return nil, fmt.Errorf("unknown type %q", e.Type)
}

func strictBody(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return errors.New("missing body")
	}
	return json.Unmarshal(raw, v)
}

func validMsgID(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func validProfileID(s string) bool {
	if len(s) != 16 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// validCID accepts base32 CIDv1 strings — the only kind our kubo mints
// (uploads use cid-version=1). v1 is hub-mediated upload only, so a CID is
// further required to exist in the pins table at post time; this check just
// keeps garbage out of the log.
func validCID(s string) bool {
	if len(s) < 10 || len(s) > 128 || s[0] != 'b' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// ParseMultiaddr resolves the HTTP multiaddr profiles peer.add accepts —
// /ip4|ip6|dns4|dns6/<host>/tcp/<port>/http|https, optionally followed by
// /http-path/<percent-encoded prefix> — into a base URL. Anything else is
// rejected until a libp2p-style transport is actually wanted.
func ParseMultiaddr(addr string) (string, error) {
	p := strings.Split(addr, "/")
	// leading slash yields an empty first element
	if len(p) < 6 || p[0] != "" {
		return "", errors.New("not an HTTP multiaddr")
	}
	switch p[1] {
	case "ip4", "ip6", "dns4", "dns6":
	default:
		return "", fmt.Errorf("unsupported transport %q", p[1])
	}
	host := p[2]
	if host == "" {
		return "", errors.New("empty host")
	}
	if p[1] == "ip6" {
		host = "[" + host + "]"
	}
	if p[3] != "tcp" {
		return "", fmt.Errorf("unsupported protocol %q", p[3])
	}
	port, err := strconv.Atoi(p[4])
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("bad port")
	}
	var scheme string
	switch p[5] {
	case "http", "https":
		scheme = p[5]
	default:
		return "", fmt.Errorf("unsupported scheme %q", p[5])
	}
	base := scheme + "://" + host + ":" + p[4]
	rest := p[6:]
	if len(rest) > 0 {
		if rest[0] != "http-path" || len(rest) != 2 || rest[1] == "" {
			return "", errors.New("only /http-path/<prefix> may follow the scheme")
		}
		path, err := url.PathUnescape(rest[1])
		if err != nil {
			return "", fmt.Errorf("http-path: %w", err)
		}
		base += "/" + strings.Trim(path, "/")
	}
	return base, nil
}
