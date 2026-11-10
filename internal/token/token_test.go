package token

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testMint = "9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump"

// fake stands in for the three places: Jupiter's search, GeckoTerminal's
// pools and candles, and a Solana RPC. down turns each one off; asked
// counts what each was asked.
type fake struct {
	*httptest.Server
	down  map[string]bool
	asked map[string]*atomic.Int32
	das   bool // the RPC speaks getAsset
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{down: map[string]bool{}, das: true,
		asked: map[string]*atomic.Int32{"jupiter": {}, "pools": {}, "candles": {}, "getAccountInfo": {}, "getAsset": {}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tokens/v2/search", func(w http.ResponseWriter, r *http.Request) {
		f.asked["jupiter"].Add(1)
		if f.down["jupiter"] {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, `[{"id":"other","name":"Other"},{"id":%q,"name":"V2EX","symbol":"V2EX",
			"icon":"https://ipfs.io/ipfs/QmT8rZaCR85GUJB3x7CTf4oQ3BZvK3BUEgRe6jWXwWzXaC","decimals":6,
			"twitter":"https://x.com/V2EX","website":"javascript:alert(1)","dev":"4JBz4tAKgAmxjDPHHi9HRLj14RsCQJyuCkCFKnpz7B9s",
			"totalSupply":999998556.42674,"tokenProgram":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","launchpad":"pump.fun",
			"holderCount":5766,"fdv":1946171.67,"mcap":1946171.67,"usdPrice":0.00194617,"liquidity":99858.92,
			"stats24h":{"priceChange":1.0744,"buyVolume":340.82,"sellVolume":320.33},
			"audit":{"mintAuthorityDisabled":true,"freezeAuthorityDisabled":false},
			"createdAt":"2024-09-27T14:18:41Z"}]`, r.URL.Query().Get("query"))
	})
	mux.HandleFunc("GET /networks/solana/tokens/{mint}/pools", func(w http.ResponseWriter, r *http.Request) {
		f.asked["pools"].Add(1)
		if f.down["pools"] {
			http.Error(w, "down", http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"data":[
		 {"attributes":{"address":"QuietPool","name":"V2EX / USDT","pool_created_at":"2025-08-30T00:00:00Z",
		   "base_token_price_quote_token":"0.00195259","token_price_usd":"0.00195259","reserve_in_usd":"922.34",
		   "volume_usd":{"h24":"21.85"},"transactions":{"h24":{"buys":13,"sells":10}}},
		  "relationships":{"dex":{"data":{"id":"meteora-damm-v2"}}}},
		 {"attributes":{"address":"BusyPool","name":"V2EX / SOL","pool_created_at":"2025-07-07T17:00:34Z",
		   "base_token_price_quote_token":"0.00002279","token_price_usd":"0.001948201042","reserve_in_usd":"199519.41",
		   "volume_usd":{"h24":"631.71"},"transactions":{"h24":{"buys":38,"sells":24}}},
		  "relationships":{"dex":{"data":{"id":"pumpswap"}}}},
		 {"attributes":{"address":"DeadPool","name":"PUMP / V2EX","pool_created_at":null,
		   "base_token_price_quote_token":null,"token_price_usd":null,"reserve_in_usd":"25679.0",
		   "volume_usd":{"h24":"0.0"},"transactions":{"h24":{"buys":0,"sells":0}}},
		  "relationships":{"dex":{"data":{"id":"meteora"}}}}],
		 "included":[{"id":"pumpswap","type":"dex","attributes":{"name":"PumpSwap"}},
		  {"id":"meteora-damm-v2","type":"dex","attributes":{"name":"Meteora DAMM v2"}}]}`)
	})
	mux.HandleFunc("GET /networks/solana/pools/{pool}/ohlcv/{frame}", func(w http.ResponseWriter, r *http.Request) {
		f.asked["candles"].Add(1)
		if f.down["candles"] {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.PathValue("pool") != "BusyPool" || r.URL.Query().Get("token") != testMint {
			http.Error(w, "wrong pool or token", http.StatusBadRequest)
			return
		}
		// newest first, as GeckoTerminal sends them, with one unusable row
		fmt.Fprintf(w, `{"data":{"attributes":{"ohlcv_list":[[300,2,2,2,2.5,7],[200,1,1,1,0,0],[100,1,1,1,1.5,3]]}},"meta":{"frame":%q}}`,
			r.PathValue("frame")+"/"+r.URL.Query().Get("aggregate")+"/"+r.URL.Query().Get("limit"))
	})
	mux.HandleFunc("POST /rpc", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if n := f.asked[req.Method]; n != nil {
			n.Add(1)
		}
		if f.down["rpc"] {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		switch req.Method {
		case "getAccountInfo":
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1},"value":{"owner":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
			 "data":{"program":"spl-token","parsed":{"type":"mint","info":{"decimals":6,"supply":"999998556426740","mintAuthority":null,"freezeAuthority":null}}}}}}`)
		case "getAsset":
			if !f.das {
				io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`)
				return
			}
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"mutable":false,"content":{"metadata":{"name":"V2EX","symbol":"V2EX","description":" way to explore "},
			 "links":{"image":"https://example.com/v2ex.png"}}}}`)
		default:
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`)
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fake) source() *Source {
	s := New(func() string { return f.URL + "/rpc" })
	s.Jupiter, s.Gecko, s.Log = f.URL, f.URL, nil
	return s
}

func TestInfo(t *testing.T) {
	f := newFake(t)
	in := f.source().Info(testMint)
	if in.Name != "V2EX" || in.Symbol != "V2EX" || in.Desc != "way to explore" {
		t.Errorf("name, symbol, description = %q, %q, %q", in.Name, in.Symbol, in.Desc)
	}
	if len(in.Icon) != 2 || in.Icon[0] != "https://example.com/v2ex.png" {
		t.Errorf("icons = %q, want the chain's own first, then Jupiter's", in.Icon)
	}
	if in.Website != "" {
		t.Errorf("website = %q: a javascript: address must not reach the page", in.Website)
	}
	if in.Twitter != "https://x.com/V2EX" {
		t.Errorf("twitter = %q", in.Twitter)
	}
	// the chain's word stands over Jupiter's: the fake Jupiter says the
	// freeze authority is live, the fake chain says nobody holds it
	if in.MintRevoked == nil || !*in.MintRevoked || in.FreezeRevoked == nil || !*in.FreezeRevoked {
		t.Errorf("authorities revoked = %v, %v, want both true from the chain", in.MintRevoked, in.FreezeRevoked)
	}
	if in.Immutable == nil || !*in.Immutable {
		t.Errorf("immutable = %v", in.Immutable)
	}
	if in.Decimals != 6 || in.Supply < 999998556.42 || in.Supply > 999998556.43 {
		t.Errorf("decimals %d, supply %f", in.Decimals, in.Supply)
	}
	if in.Price != 0.00194617 || in.Holders != 5766 || in.Change == nil || *in.Change != 1.0744 {
		t.Errorf("price %v, holders %d, change %v", in.Price, in.Holders, in.Change)
	}
	// volume and liquidity are the pools' sums, not Jupiter's own; the
	// pool nobody traded in today keeps its 25,679 out of the liquidity
	if got := fmt.Sprintf("%.2f %.2f", in.Volume, in.Liquidity); got != "653.56 200441.75" {
		t.Errorf("volume, liquidity = %s", got)
	}
	if in.Issuer != "pump.fun" || in.Created.Format("2006-01-02") != "2024-09-27" || !strings.HasPrefix(in.Creator, "4JBz") {
		t.Errorf("issuer %q, created %v, creator %q", in.Issuer, in.Created, in.Creator)
	}
	if len(in.Markets) != 3 || in.Markets[0].Address != "BusyPool" || in.Markets[1].Address != "QuietPool" {
		t.Fatalf("markets = %+v, want the busiest first", in.Markets)
	}
	if m := in.Markets[0]; m.Dex != "PumpSwap" || m.Trades != 62 || m.Pair != "V2EX / SOL" || m.Created.IsZero() {
		t.Errorf("first market = %+v", m)
	}
	if m := in.Markets[2]; m.Dex != "meteora" || m.Rate != 0 || !m.Created.IsZero() {
		t.Errorf("a pool with nulls = %+v, want its dex id and no rate", m)
	}
	if in.Pool != "BusyPool" {
		t.Errorf("chart pool = %q, want the deepest", in.Pool)
	}
	if in.At.IsZero() {
		t.Error("At is zero with market numbers read")
	}
}

// A place that stops answering does not empty the page: its last answer
// stands, and it is not asked again by every reader.
func TestKeepsLastAnswer(t *testing.T) {
	f := newFake(t)
	s := f.source()
	now := time.Now()
	s.now = func() time.Time { return now }
	first := s.Info(testMint)
	if first.Holders != 5766 {
		t.Fatalf("holders = %d", first.Holders)
	}
	s.Info(testMint)
	if n := f.asked["jupiter"].Load(); n != 1 {
		t.Errorf("Jupiter asked %d times for two readers a moment apart, want 1", n)
	}

	f.down["jupiter"], f.down["pools"], f.down["rpc"] = true, true, true
	now = now.Add(2 * time.Hour) // every answer is old
	var in *Info
	for i := 0; i < 50; i++ { // the asking runs behind the reader: wait for it to end
		in = s.Info(testMint)
		if f.asked["jupiter"].Load() >= 2 && f.asked["getAsset"].Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if in.Holders != 5766 || len(in.Markets) != 3 || in.Desc != "way to explore" || in.Supply == 0 {
		t.Errorf("with every place down: holders %d, markets %d, desc %q, supply %v — the last answers should stand",
			in.Holders, len(in.Markets), in.Desc, in.Supply)
	}
	if !in.At.Equal(first.At) {
		t.Errorf("At = %v, want the time of the last answer %v", in.At, first.At)
	}
	time.Sleep(30 * time.Millisecond)
	before := f.asked["jupiter"].Load()
	for i := 0; i < 5; i++ {
		s.Info(testMint)
	}
	time.Sleep(30 * time.Millisecond)
	if n := f.asked["jupiter"].Load(); n != before {
		t.Errorf("a failed Jupiter was asked %d more times inside retryAfter", n-before)
	}
}

// With nothing answering there is still a page's worth: the mint.
func TestNothingAnswers(t *testing.T) {
	f := newFake(t)
	f.down["jupiter"], f.down["pools"], f.down["rpc"] = true, true, true
	in := f.source().Info(testMint)
	if in.Mint != testMint || in.Name != "" || in.Price != 0 || len(in.Markets) != 0 || !in.At.IsZero() || in.MintRevoked != nil {
		t.Errorf("info = %+v, want the mint alone", in)
	}
}

// An RPC without the DAS API, and no RPC at all: Jupiter fills in.
func TestWithoutDAS(t *testing.T) {
	f := newFake(t)
	f.das = false
	s := f.source()
	in := s.Info(testMint)
	if in.Name != "V2EX" || in.Desc != "" || in.Immutable != nil || len(in.Icon) != 1 {
		t.Errorf("no DAS: name %q, desc %q, immutable %v, icons %q", in.Name, in.Desc, in.Immutable, in.Icon)
	}
	s.Info(testMint)
	if n := f.asked["getAsset"].Load(); n != 1 {
		t.Errorf("getAsset asked %d times of an RPC that has none, want once", n)
	}

	none := New(nil)
	none.Jupiter, none.Gecko, none.Log = f.URL, f.URL, nil
	in = none.Info(testMint)
	if in.Decimals != 6 || in.Supply == 0 || in.MintRevoked == nil || !*in.MintRevoked || in.FreezeRevoked == nil || *in.FreezeRevoked {
		t.Errorf("no RPC: decimals %d, supply %v, revoked %v %v — want Jupiter's", in.Decimals, in.Supply, in.MintRevoked, in.FreezeRevoked)
	}
}

func TestCandles(t *testing.T) {
	f := newFake(t)
	s := f.source()
	cs, at := s.Candles(testMint, "BusyPool", RangeOf("7d"))
	if len(cs) != 2 || cs[0].T != 100 || cs[0].Close != 1.5 || cs[1].T != 300 || cs[1].Close != 2.5 {
		t.Fatalf("candles = %+v, want the two usable ones, the oldest first", cs)
	}
	if at.IsZero() {
		t.Error("no time for candles read")
	}
	if cs, _ := s.Candles(testMint, "", RangeOf("7d")); cs != nil {
		t.Error("candles for no pool")
	}
	if r := RangeOf("nonsense"); r.Name != DefaultRange {
		t.Errorf("RangeOf(nonsense) = %q", r.Name)
	}
	if r := RangeOf("1Y"); r.Name != "1y" {
		t.Errorf("RangeOf(1Y) = %q", r.Name)
	}
}

// The RPC's address holds its key: no error from it may quote it.
func TestRPCErrorHidesAddress(t *testing.T) {
	var logged []string
	s := New(func() string { return "http://127.0.0.1:1/?api-key=SECRETKEY" })
	s.Jupiter, s.Gecko = "http://127.0.0.1:1", "http://127.0.0.1:1"
	s.Log = func(format string, v ...any) { logged = append(logged, fmt.Sprintf(format, v...)) }
	s.Info(testMint)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(logged) == 0 {
		t.Fatal("nothing logged for a dead RPC")
	}
	for _, l := range logged {
		if strings.Contains(l, "SECRETKEY") {
			t.Errorf("the log quotes the RPC's key: %s", l)
		}
	}
}

// TestLive asks the real places: TOKEN_LIVE=1 go test -run Live -v
// (TOKEN_RPC names an RPC to ask too).
func TestLive(t *testing.T) {
	if os.Getenv("TOKEN_LIVE") == "" {
		t.Skip("TOKEN_LIVE not set")
	}
	s := New(func() string { return os.Getenv("TOKEN_RPC") })
	s.Log = t.Logf
	in := s.Info(testMint)
	b, _ := json.MarshalIndent(in, "", " ")
	t.Logf("%s", b)
	for _, r := range Ranges {
		cs, _ := s.Candles(testMint, in.Pool, r)
		if len(cs) == 0 {
			t.Errorf("%s: no candles", r.Name)
			continue
		}
		t.Logf("%s: %d candles, %s to %s", r.Name, len(cs),
			time.Unix(cs[0].T, 0).UTC().Format(time.RFC3339), time.Unix(cs[len(cs)-1].T, 0).UTC().Format(time.RFC3339))
	}
}
