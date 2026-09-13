package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"exehub/internal/config"
	"exehub/internal/gate"
	"exehub/internal/identity"
	"exehub/internal/store"
)

// gateRPC answers getTokenAccountsByOwner with a balance for the owners it
// knows (base58 addresses) and nothing for the rest; it counts calls.
func gateRPC(t *testing.T, held map[string]uint64, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "getTokenAccountsByOwner" {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "no"}})
			return
		}
		*calls++
		var owner string
		json.Unmarshal(req.Params[0], &owner)
		value := []any{}
		if raw, ok := held[owner]; ok {
			value = append(value, map[string]any{"account": map[string]any{"data": map[string]any{
				"parsed": map[string]any{"info": map[string]any{
					"tokenAmount": map[string]any{"amount": fmt.Sprint(raw)}}}}}})
		}
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"value": value}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tokenServer(t *testing.T, rpcURL string, admins []string) *Server {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"admins": admins, "cooldown": 60, "gate": map[string]any{"mode": "token", "token": map[string]any{
		"rpc_url": rpcURL, "mints": []map[string]any{{"mint": "MintAaa", "min_amount": "1000"}}, "recheck": "10m"}}})
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := config.NewHolder(c)
	return &Server{Cfg: h, St: st, Gate: gate.New(h)}
}

type gateOut struct {
	Profile  string `json:"profile"`
	Mode     string `json:"mode"`
	Gate     string `json:"gate"`
	Banned   bool   `json:"banned"`
	Cooldown int    `json:"cooldown"`
	Wait     int    `json:"wait"`
	Mints    []struct {
		Amount, Mint string
		Raw          bool
	} `json:"mints"`
}

func gateGet(t *testing.T, s *Server, pub ed25519.PublicKey) (int, gateOut) {
	t.Helper()
	code, body := get(t, s.Handler(), "/v1/gate?author="+url.QueryEscape(base64.StdEncoding.EncodeToString(pub)))
	var out gateOut
	json.Unmarshal([]byte(body), &out)
	return code, out
}

// TestGateEndpoint: the verdict a post would meet, before anything is
// signed — pass, below, admin, banned, the cooldown's wait, an RPC that
// is down — and uncached checks share a rate limit.
func TestGateEndpoint(t *testing.T) {
	holder, holderPriv, _ := ed25519.GenerateKey(nil)
	poor, _, _ := ed25519.GenerateKey(nil)
	admin, adminPriv, _ := ed25519.GenerateKey(nil)
	calls := 0
	rpc := gateRPC(t, map[string]uint64{gate.Base58(holder): 5000, gate.Base58(poor): 10}, &calls)
	s := tokenServer(t, rpc.URL, []string{identity.Fingerprint(admin)})

	if code, _ := get(t, s.Handler(), "/v1/gate?author=nope"); code != http.StatusBadRequest {
		t.Errorf("bad author: %d", code)
	}
	code, out := gateGet(t, s, holder)
	if code != 200 || out.Gate != "pass" || out.Mode != "token" || out.Cooldown != 60 || out.Wait != 0 || out.Banned ||
		out.Profile != identity.Fingerprint(holder) || len(out.Mints) != 1 || out.Mints[0].Mint != "MintAaa" {
		t.Fatalf("holder: %d %+v", code, out)
	}
	if _, out := gateGet(t, s, poor); out.Gate != "below" {
		t.Errorf("poor: %+v", out)
	}
	if _, out := gateGet(t, s, admin); out.Gate != "admin" || out.Cooldown != 0 {
		t.Errorf("admin: %+v", out)
	}

	// a post starts the holder's cooldown
	ingest(t, s, holderPriv, holder, 1, "post.create", map[string]any{"text": "hi"})
	if _, out := gateGet(t, s, holder); out.Wait < 55 || out.Wait > 60 {
		t.Errorf("wait after a post: %+v", out)
	}
	// a ban shows
	ingest(t, s, adminPriv, admin, 1, "ban.set", map[string]any{"target": identity.Fingerprint(poor)})
	if _, out := gateGet(t, s, poor); !out.Banned {
		t.Errorf("banned: %+v", out)
	}

	// cached verdicts cost no RPC call and pass the limiter; new keys
	// spend it until 429
	before := calls
	for i := 0; i < 5; i++ {
		if code, _ := gateGet(t, s, holder); code != 200 {
			t.Fatalf("cached check %d: %d", i, code)
		}
	}
	if calls != before {
		t.Errorf("cached checks called the RPC %d times", calls-before)
	}
	limited := false
	for i := 0; i < 20 && !limited; i++ {
		k, _, _ := ed25519.GenerateKey(nil)
		code, _ := gateGet(t, s, k)
		limited = code == http.StatusTooManyRequests
	}
	if !limited {
		t.Error("uncached checks were never rate limited")
	}
	if code, _ := gateGet(t, s, holder); code != 200 {
		t.Errorf("a cached check under the limit: %d", code)
	}

	// an RPC that is down, with no cached verdict
	rpc.Close()
	s2 := tokenServer(t, rpc.URL, nil)
	if _, out := gateGet(t, s2, holder); out.Gate != "unavailable" {
		t.Errorf("rpc down: %+v", out)
	}
	// open mode
	s3 := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}})
	if _, out := gateGet(t, s3, poor); out.Gate != "open" || out.Mode != "open" {
		t.Errorf("open: %+v", out)
	}
}
