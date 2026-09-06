package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"exehub/internal/envelope"
	"exehub/internal/events"
	"exehub/internal/store"
)

func parseIP(s string) net.IP { return net.ParseIP(s) }

func b64(s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// TestEncryptRFC8291 holds encrypt to the RFC's Appendix A vector: the
// same keys and salt give the same bytes, header and ciphertext.
func TestEncryptRFC8291(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(b64("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encrypt(
		b64("V2hlbiBJIGdyb3cgdXAsIEkgd2FudCB0byBiZSBhIHdhdGVybWVsb24"),
		b64("BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		b64("BTBZMqHH6r4Tts7J_aSIgg"), as, b64("DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	want := append(b64("DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8"),
		b64("8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ")...)
	if !bytes.Equal(got, want) {
		t.Errorf("encrypt:\n got %x\nwant %x", got, want)
	}
	if string(as.PublicKey().Bytes()) != string(b64("BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")) {
		t.Error("as_public differs from the vector")
	}
}

// decrypt is the browser's side of RFC 8291, for the tests: the header's
// salt and key, the same derivation with the subscription's private key.
func decrypt(msg, auth []byte, ua *ecdh.PrivateKey) ([]byte, error) {
	salt, asPub, ct := msg[:16], msg[21:21+int(msg[20])], msg[21+int(msg[20]):]
	if binary.BigEndian.Uint32(msg[16:20]) != 4096 {
		panic("record size")
	}
	pub, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		return nil, err
	}
	secret, err := ua.ECDH(pub)
	if err != nil {
		return nil, err
	}
	prkKey, _ := hkdf.Extract(sha256.New, secret, auth)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(ua.PublicKey().Bytes())+string(asPub), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, err
	}
	return plain[:bytes.LastIndexByte(plain, 0x02)], nil
}

// TestVAPID: the Authorization header is a JWT the key's public half
// verifies, for the endpoint's origin, with the subject and a same-day
// expiry, and carries the key.
func TestVAPID(t *testing.T) {
	k, err := LoadKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	h, err := k.vapid("https://push.example/v1/abc", "https://hub.example", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "vapid t=") || !strings.HasSuffix(h, ", k="+k.Public()) {
		t.Fatalf("header %q", h)
	}
	jwt := strings.TrimPrefix(strings.Split(h, ",")[0], "vapid t=")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q", jwt)
	}
	var claims struct {
		Aud string `json:"aud"`
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(b64(parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://push.example" || claims.Sub != "https://hub.example" || claims.Exp <= now.Unix() || claims.Exp > now.Add(24*time.Hour).Unix() {
		t.Errorf("claims %+v", claims)
	}
	sig := b64(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&k.priv.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Error("signature does not verify")
	}
	// the key is the same one when loaded again
	k2, _ := LoadKey(t.TempDir())
	if k2.Public() == k.Public() {
		t.Error("two state dirs, one key")
	}
}

// TestDialGuard: the sender never dials a loopback, private, link-local
// or CGNAT address, whatever the endpoint's name resolves to.
func TestDialGuard(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"100.116.32.57": false, "169.254.1.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false,
	} {
		if got := public(parseIP(ip)); got != want {
			t.Errorf("public(%s) = %v", ip, got)
		}
	}
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("dial loopback: %v", err)
	}
}

// TestParseSubscription: what a browser sends is accepted, and only that.
func TestParseSubscription(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	p256dh := base64.RawURLEncoding.EncodeToString(ua.PublicKey().Bytes())
	auth := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	good := `{"endpoint":"https://push.example/v1/abc","expirationTime":null,"keys":{"p256dh":"` + p256dh + `","auth":"` + auth + `=="}}`
	sub, err := ParseSubscription([]byte(good))
	if err != nil || sub.Endpoint != "https://push.example/v1/abc" || len(sub.P256dh) != 65 || len(sub.Auth) != 16 {
		t.Errorf("good: %v %+v", err, sub)
	}
	for _, bad := range []string{
		`nope`,
		`{"endpoint":"http://push.example/x","keys":{"p256dh":"` + p256dh + `","auth":"` + auth + `"}}`,
		`{"endpoint":"https://push.example/x","keys":{"p256dh":"AAAA","auth":"` + auth + `"}}`,
		`{"endpoint":"https://push.example/x","keys":{"p256dh":"` + p256dh + `","auth":"AAAA"}}`,
	} {
		if _, err := ParseSubscription([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestMessage(t *testing.T) {
	p := &store.FeedPost{ID: "p1", Author: "abc", AuthorName: "Ann", Text: "hello   there\nworld"}
	if m := message(p); m != (Message{Title: "Ann", Body: "hello there world", URL: "/p/p1", Tag: "p1"}) {
		t.Errorf("post: %+v", m)
	}
	p.ReplyTo, p.AuthorName = "p0", ""
	if m := message(p); m.Title != "abc replied" || m.URL != "/p/p0" {
		t.Errorf("reply: %+v", m)
	}
	p.Text, p.ReplyTo = "", ""
	p.Embeds = []envelope.Embed{{CID: "c", MIME: "image/png"}}
	if m := message(p); m.Body != "a picture" {
		t.Errorf("picture: %+v", m)
	}
	long := strings.Repeat("字", 300)
	if e := excerpt(long, 200); len([]rune(e)) != 201 || !strings.HasSuffix(e, "…") {
		t.Errorf("excerpt cut inside a rune: %d runes", len([]rune(e)))
	}
	if e := excerpt(strings.Repeat("word ", 100), 200); !strings.HasSuffix(e, "word…") || strings.Contains(e, " …") {
		t.Errorf("excerpt %q", e)
	}
}

// TestNotify: a post lands, every subscriber's push service gets a
// payload only that browser can read, and a subscription its service
// reports gone is forgotten.
func TestNotify(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/hub.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k, _ := LoadKey(t.TempDir())
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)

	var mu sync.Mutex
	var got []*http.Request
	var bodies [][]byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		b.ReadFrom(r.Body)
		mu.Lock()
		got = append(got, r)
		bodies = append(bodies, b.Bytes())
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()
	for _, ep := range []string{ts.URL + "/live", ts.URL + "/gone"} {
		if err := st.PushAdd(store.PushSub{Endpoint: ep, P256dh: ua.PublicKey().Bytes(), Auth: auth, Base: "https://hub.example"}); err != nil {
			t.Fatal(err)
		}
	}

	pub, priv, _ := ed25519.GenerateKey(nil)
	raw, _ := json.Marshal(map[string]any{
		"type": "post.create", "author": base64.StdEncoding.EncodeToString(pub),
		"seq": 1, "ts": 1756500000000, "body": map[string]any{"text": "When I grow up, I want to be a watermelon"},
	})
	e, err := envelope.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := e.Op()
	id, _, err := st.Ingest(raw, ed25519.Sign(priv, raw), e, op)
	if err != nil {
		t.Fatal(err)
	}

	n := &Notifier{St: st, Bus: events.New(), Sender: &Sender{Key: k, Client: ts.Client()}}
	n.notify(events.Event{Type: "post.create", ID: id, Author: e.ProfileID()})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("%d pushes", len(got))
	}
	for i, r := range got {
		if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") != "86400" ||
			!strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") || !strings.HasSuffix(r.Header.Get("Authorization"), ", k="+k.Public()) {
			t.Errorf("headers %v", r.Header)
		}
		plain, err := decrypt(bodies[i], auth, ua)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		var m Message
		json.Unmarshal(plain, &m)
		if m.Title != e.ProfileID() || m.Body != "When I grow up, I want to be a watermelon" || m.URL != "/p/"+id || m.Tag != id {
			t.Errorf("message %+v", m)
		}
	}
	subs, _ := st.PushSubs()
	if len(subs) != 1 || subs[0].Endpoint != ts.URL+"/live" {
		t.Errorf("after a 410, subscriptions: %+v", subs)
	}
}
