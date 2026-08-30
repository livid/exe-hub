package gate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"exehub/internal/config"
)

// fakeRPC answers getTokenAccountsByOwner from a mint -> raw balance map
// (one token account each; absent mint = zero accounts).
func fakeRPC(t *testing.T, balances map[string]uint64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Params) < 2 {
			t.Errorf("bad rpc request: %v", err)
			return
		}
		var filter struct {
			Mint string `json:"mint"`
		}
		json.Unmarshal(req.Params[1], &filter)
		value := []any{}
		if raw, ok := balances[filter.Mint]; ok {
			value = append(value, map[string]any{"account": map[string]any{"data": map[string]any{
				"parsed": map[string]any{"info": map[string]any{
					"tokenAmount": map[string]any{"amount": fmt.Sprint(raw)}}}}}})
		}
		json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"value": value}})
	}))
}

func tokenCfg(t *testing.T, rpcURL string, mints []config.MintReq) *config.Holder {
	t.Helper()
	// through config.Load so MinRaw parsing and normalization run for real
	b, _ := json.Marshal(map[string]any{"gate": map[string]any{"mode": "token", "token": map[string]any{
		"rpc_url": rpcURL, "mints": mints, "recheck": "10m"}}})
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return config.NewHolder(c)
}

// TestCheckAnyOf: holding enough of any one listed mint passes; holding
// none fails; each author's verdict is cached.
func TestCheckAnyOf(t *testing.T) {
	holder, _, _ := ed25519.GenerateKey(rand.Reader)
	rpc := fakeRPC(t, map[string]uint64{"MintBbb": 700})
	defer rpc.Close()

	mints := []config.MintReq{
		{Mint: "MintAaa", MinAmount: "1000"},
		{Mint: "MintBbb", MinAmount: "500"},
	}
	g := New(tokenCfg(t, rpc.URL, mints))
	if err := g.Check(holder); err != nil {
		t.Fatalf("second mint qualifies but denied: %v", err)
	}

	poor, _, _ := ed25519.GenerateKey(rand.Reader)
	g2 := New(tokenCfg(t, rpc.URL, []config.MintReq{{Mint: "MintAaa", MinAmount: "1000"}}))
	if err := g2.Check(poor); !errors.Is(err, ErrDenied) {
		t.Fatalf("no holdings: %v", err)
	}
}

// TestLegacySingleMint: the pre-list config shape normalizes into Mints.
func TestLegacySingleMint(t *testing.T) {
	b := []byte(`{"gate":{"mode":"token","token":{"rpc_url":"http://x","mint":"MintAaa","min_amount":"1000"}}}`)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	m := c.Gate.Token.Mints
	if len(m) != 1 || m[0].Mint != "MintAaa" || m[0].MinRaw != 1000 || c.Gate.Token.Mint != "" {
		t.Fatalf("normalized = %+v (legacy mint %q)", m, c.Gate.Token.Mint)
	}
}
