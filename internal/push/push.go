// Package push sends Web Push notifications — one for every post that
// lands on the hub — to the browsers that subscribed on the public page.
// The protocol is three RFCs and nothing but the standard library:
// RFC 8030 (the request to the push service), RFC 8291 (the payload
// encryption, aes128gcm) and RFC 8292 (VAPID: the hub identifying itself
// to the service with a P-256 key). See PLAN.md "Notifications".
package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"exehub/internal/events"
	"exehub/internal/store"
)

// Key is the hub's VAPID key: a P-256 pair generated on first start at
// <stateDir>/vapid_p256, beside the hub identity (which is ed25519, and
// VAPID signs ES256, so it cannot serve). Browsers bind a subscription
// to the public key, so a changed key orphans every subscription.
type Key struct{ priv *ecdsa.PrivateKey }

func LoadKey(stateDir string) (*Key, error) {
	p := filepath.Join(stateDir, "vapid_p256")
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return nil, err
		}
		b = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return nil, err
		}
		return &Key{priv}, nil
	} else if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("vapid_p256: not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := key.(*ecdsa.PrivateKey)
	if !ok || priv.Curve != elliptic.P256() {
		return nil, errors.New("vapid_p256: not a P-256 key")
	}
	return &Key{priv}, nil
}

// Public is the key as the page hands it to pushManager.subscribe and as
// the Authorization header carries it: the uncompressed point, base64url
// without padding.
func (k *Key) Public() string {
	pub, _ := k.priv.PublicKey.ECDH()
	return base64.RawURLEncoding.EncodeToString(pub.Bytes())
}

// vapid is the Authorization header for one push service (RFC 8292): a
// JWT signed ES256 naming the service's origin as audience, the hub as
// subject and an expiry inside the 24 hours allowed, plus the public key.
func (k *Key) vapid(endpoint, subject string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(map[string]string{"typ": "JWT", "alg": "ES256"}) + "." +
		enc(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now.Add(12 * time.Hour).Unix(), "sub": subject})
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS wants r || s, each 32 bytes, not ASN.1
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + base64.RawURLEncoding.EncodeToString(sig) + ", k=" + k.Public(), nil
}

// encrypt is RFC 8291: the payload for one subscriber, readable by its
// browser alone. A fresh P-256 pair and salt per message; ECDH with the
// subscription's key, two HKDF rounds keyed by the subscription's auth
// secret, then one AES-128-GCM record (record size 4096 — a notification
// always fits one) behind the aes128gcm header carrying the salt, the
// record size and the message's public key. The test holds it to the
// RFC's own vector.
func encrypt(plain, uaPub, auth []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	pub, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		return nil, err
	}
	secret, err := as.ECDH(pub)
	if err != nil {
		return nil, err
	}
	asPub := as.PublicKey().Bytes()
	prkKey, err := hkdf.Extract(sha256.New, secret, auth)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(uaPub)+string(asPub), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(salt)
	binary.Write(&out, binary.BigEndian, uint32(4096))
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	record := append(append([]byte{}, plain...), 0x02) // the last record's delimiter, no padding
	out.Write(gcm.Seal(nil, nonce, record, nil))
	return out.Bytes(), nil
}

// ParseSubscription reads what a browser's PushSubscription.toJSON() sent:
// an https endpoint the push service minted and the two keys the payload
// is encrypted to. Anything else is a 400.
func ParseSubscription(body []byte) (store.PushSub, error) {
	var in struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return store.PushSub{}, errors.New("subscription is not JSON")
	}
	u, err := url.Parse(in.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(in.Endpoint) > 2048 {
		return store.PushSub{}, errors.New("endpoint must be an https URL")
	}
	dec := func(s string) []byte {
		b, _ := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
		return b
	}
	sub := store.PushSub{Endpoint: in.Endpoint, P256dh: dec(in.Keys.P256dh), Auth: dec(in.Keys.Auth)}
	if _, err := ecdh.P256().NewPublicKey(sub.P256dh); err != nil {
		return store.PushSub{}, errors.New("p256dh must be a P-256 public key")
	}
	if len(sub.Auth) != 16 {
		return store.PushSub{}, errors.New("auth must be 16 bytes")
	}
	return sub, nil
}

// ErrGone: the push service says the subscription no longer exists.
var ErrGone = errors.New("push: subscription gone")

// Sender delivers payloads to push services on the hub's behalf.
type Sender struct {
	Key    *Key
	Client *http.Client // nil: a client that dials public addresses only
}

func (s *Sender) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DialContext: dial, TLSHandshakeTimeout: 10 * time.Second}}
}

// Send pushes one payload to one subscription, to be held a day for a
// device that is offline (RFC 8030's TTL).
func (s *Sender) Send(ctx context.Context, sub store.PushSub, payload []byte) error {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	body, err := encrypt(payload, sub.P256dh, sub.Auth, as, salt)
	if err != nil {
		return err
	}
	auth, err := s.Key.vapid(sub.Endpoint, subject(sub.Base), time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "normal")
	req.Header.Set("Authorization", auth)
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode/100 == 2:
		return nil
	default:
		return fmt.Errorf("push: %s: %s", req.URL.Host, resp.Status)
	}
}

// subject is the VAPID subject, a contact for the push service's
// operator: the hub as the subscriber reached it, when that is an https
// URL (a service may refuse http); else the project's page.
func subject(base string) string {
	if strings.HasPrefix(base, "https://") {
		return base
	}
	return "https://github.com/livid/exe-hub"
}

// dial connects to public addresses only, checked on the resolved IP so
// a name that rebinds cannot help: a subscription's endpoint is whatever
// a client sent, and the hub POSTs to it — without this, an endpoint of
// http://127.0.0.1:5001/… would have the hub talk to its own kubo RPC.
func dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || !public(ip) {
			return fmt.Errorf("push: %s is not a public address", host)
		}
		return nil
	}}
	return d.DialContext(ctx, network, addr)
}

var cgnat = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)} // Tailscale lives here

func public(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !cgnat.Contains(ip)
}

// Message is what the service worker shows: a title, a line of body, the
// page to open, and the post id as the tag so a duplicate delivery is
// one notification.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

func message(p *store.FeedPost) Message {
	who := p.AuthorName
	if who == "" {
		who = p.Author
	}
	m := Message{Title: who, Body: excerpt(p.Text, 200), URL: "/p/" + p.ID, Tag: p.ID}
	if p.ReplyTo != "" {
		m.Title, m.URL = who+" replied", "/p/"+p.ReplyTo
	}
	if m.Body == "" {
		pics, files := 0, 0
		for _, e := range p.Embeds {
			if strings.HasPrefix(e.MIME, "image/") {
				pics++
			} else {
				files++
			}
		}
		switch {
		case pics == 1:
			m.Body = "a picture"
		case pics > 1:
			m.Body = fmt.Sprintf("%d pictures", pics)
		case files > 0:
			m.Body = "a file"
		}
	}
	return m
}

// excerpt is the post's first n characters or so, cut at a space, never
// inside a character.
func excerpt(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	s := string(r[:n])
	if cut := strings.LastIndexByte(s, ' '); cut >= len(s)/2 {
		s = s[:cut]
	}
	return s + "…"
}

// Notifier turns every post landing on the hub into a push to every
// subscriber. Driven by the event bus like the page's live feed: events
// one at a time, each fanned out eight sends wide, and a subscription
// its service reports gone is dropped.
type Notifier struct {
	St     *store.Store
	Bus    *events.Broadcaster
	Sender *Sender
}

func (n *Notifier) Run() {
	for ev := range n.Bus.Subscribe() {
		if ev.Type == "post.create" {
			n.notify(ev)
		}
	}
}

func (n *Notifier) notify(ev events.Event) {
	subs, err := n.St.PushSubs()
	if err != nil {
		log.Printf("push: %v", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	p, err := n.St.Post(ev.ID)
	if err != nil {
		log.Printf("push: post %s: %v", ev.ID, err)
		return
	}
	payload, _ := json.Marshal(message(p))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		gone []string
		sem  = make(chan struct{}, 8)
	)
	for _, sub := range subs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			switch err := n.Sender.Send(ctx, sub, payload); {
			case errors.Is(err, ErrGone):
				mu.Lock()
				gone = append(gone, sub.Endpoint)
				mu.Unlock()
			case err != nil:
				log.Printf("push: %v", err)
			}
		}()
	}
	wg.Wait()
	for _, e := range gone {
		if err := n.St.PushRemove(e); err != nil {
			log.Printf("push: forget %s: %v", e, err)
		}
	}
	log.Printf("push: post %.8s to %d subscribers, %d gone", ev.ID, len(subs), len(gone))
}
