package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exehub/internal/card"
	"exehub/internal/config"
	"exehub/internal/gate"
	"exehub/internal/store"
	"exehub/internal/token"
)

const tokenMint = "9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump"

// No test of this package reaches Jupiter or GeckoTerminal: a hub that
// was given no token source of its own gets one that asks a dead port.
func init() {
	newTokenSource = func(rpc func() string) *token.Source {
		s := token.New(rpc)
		s.Jupiter, s.Gecko, s.Log = "http://127.0.0.1:1", "http://127.0.0.1:1", nil
		return s
	}
}

// tokenWorld stands in for everything the token page asks: Jupiter,
// GeckoTerminal, a Solana RPC and the address of the token's picture.
// down turns all of it off.
type tokenWorld struct {
	*httptest.Server
	down bool
	tx   string // what the RPC says of a signature
	txN  int    // how often it was asked
	held string // the raw balance every owner holds
	balN int    // how often a balance was read
}

func newTokenWorld(t *testing.T) *tokenWorld {
	t.Helper()
	tw := &tokenWorld{}
	now := time.Now().Unix()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tokens/v2/search", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id":%q,"name":"V2EX <b>","symbol":"V2EX","icon":%q,"decimals":6,
			"twitter":"https://x.com/V2EX","website":"https://www.v2ex.com/","dev":"4JBz4tAKgAmxjDPHHi9HRLj14RsCQJyuCkCFKnpz7B9s",
			"totalSupply":999998556.42674,"launchpad":"pump.fun","holderCount":5766,"fdv":1946171.67,"mcap":1946171.67,
			"usdPrice":0.00194617,"liquidity":99858.92,"stats24h":{"priceChange":-1.0744,"buyVolume":340.82,"sellVolume":320.33},
			"createdAt":"2024-09-27T14:18:41Z"}]`, r.URL.Query().Get("query"), tw.URL+"/v2ex.png")
	})
	mux.HandleFunc("GET /networks/solana/tokens/{mint}/pools", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[
		 {"attributes":{"address":"BusyPool","name":"V2EX / SOL","pool_created_at":"2025-07-07T17:00:34Z",
		   "base_token_price_quote_token":"0.00002279","token_price_usd":"0.001948","reserve_in_usd":"199519.41",
		   "volume_usd":{"h24":"631.71"},"transactions":{"h24":{"buys":38,"sells":24}}},
		  "relationships":{"dex":{"data":{"id":"pumpswap"}}}},
		 {"attributes":{"address":"IdlePool","name":"PUMP / V2EX","pool_created_at":null,
		   "base_token_price_quote_token":null,"token_price_usd":null,"reserve_in_usd":"0.0",
		   "volume_usd":{"h24":"0.0"},"transactions":{"h24":{"buys":0,"sells":0}}},
		  "relationships":{"dex":{"data":{"id":"meteora"}}}}],
		 "included":[{"id":"pumpswap","type":"dex","attributes":{"name":"PumpSwap"}}]}`)
	})
	mux.HandleFunc("GET /networks/solana/pools/{pool}/ohlcv/{frame}", func(w http.ResponseWriter, r *http.Request) {
		// a candle before any range's start, and two inside the last hour
		fmt.Fprintf(w, `{"data":{"attributes":{"ohlcv_list":[[%d,0,0,0,0.0021,5],[%d,0,0,0,0.0019,5],[%d,0,0,0,0.0020,5]]}}}`,
			now-600, now-1800, now-400*86400)
	})
	mux.HandleFunc("POST /rpc", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "getAccountInfo":
			io.WriteString(w, `{"result":{"value":{"owner":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
			 "data":{"parsed":{"type":"mint","info":{"decimals":6,"supply":"999998556426740","mintAuthority":null,"freezeAuthority":"Fr33zer"}}}}}}`)
		case "getAsset":
			io.WriteString(w, `{"result":{"mutable":false,"content":{"metadata":{"name":"V2EX <b>","symbol":"V2EX","description":"way to explore"},"links":{}}}}`)
		case "getTokenAccountsByOwner":
			tw.balN++
			fmt.Fprintf(w, `{"result":{"value":[{"account":{"data":{"parsed":{"info":{"tokenAmount":{"amount":%q}}}}}}]}}`, tw.held)
		case "getTokenSupply":
			io.WriteString(w, `{"result":{"value":{"decimals":6}}}`)
		case "getSignatureStatuses":
			tw.txN++
			io.WriteString(w, `{"result":{"value":[`+tw.tx+`]}}`)
		default:
			io.WriteString(w, `{"error":{"code":-32601,"message":"Method not found"}}`)
		}
	})
	mux.HandleFunc("GET /v2ex.png", func(w http.ResponseWriter, r *http.Request) {
		img := image.NewRGBA(image.Rect(0, 0, 400, 300))
		for i := range img.Pix {
			img.Pix[i] = 0xff
		}
		img.Set(1, 1, color.Black)
		png.Encode(w, img)
	})
	tw.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tw.down {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(tw.Close)
	return tw
}

// tokenPageServer is a hub whose gate asks for 10,000 of tokenMint, and
// for as much of each mint in more, reading the token from tw.
func tokenPageServer(t *testing.T, tw *tokenWorld, more ...string) *Server {
	t.Helper()
	mints := []map[string]any{{"mint": tokenMint, "min_amount": "10000000000"}}
	for _, m := range more {
		mints = append(mints, map[string]any{"mint": m, "min_amount": "5"})
	}
	b, _ := json.Marshal(map[string]any{"gate": map[string]any{"mode": "token", "token": map[string]any{
		"rpc_url": tw.URL + "/rpc", "mints": mints, "recheck": "10m"}}})
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
	src := token.New(func() string { return h.Get().Gate.Token.RPCURL })
	src.Jupiter, src.Gecko, src.Log = tw.URL, tw.URL, nil
	f := card.NewFetcher()
	f.AllowPrivate = true
	return &Server{Cfg: h, St: st, Gate: gate.New(h), Token: src, TokenFetch: f}
}

// GET /token goes to the gate's first mint, keeping the query; a hub
// with an open gate has no token page at all.
func TestTokenRedirect(t *testing.T) {
	s := tokenPageServer(t, newTokenWorld(t), "OtherMint")
	h := s.Handler()
	for path, want := range map[string]string{
		"/token":            "/token/" + tokenMint,
		"/token/":           "/token/" + tokenMint,
		"/token?lang=zh":    "/token/" + tokenMint + "?lang=zh",
		"/token?range=7d&x": "/token/" + tokenMint + "?range=7d&x",
	} {
		req := httptest.NewRequest("GET", "http://hub.example"+path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d to %q, want 302 to %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}

	open := testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}}).Handler()
	for _, path := range []string{"/token", "/token/" + tokenMint} {
		code, body := get(t, open, path)
		if code != http.StatusNotFound || !strings.Contains(body, "This hub has no token gate.") {
			t.Errorf("open gate, %s: %d, want a 404 that says there is no token gate", path, code)
		}
	}
	if code, _ := get(t, open, "/token/"+tokenMint+"/icon"); code != http.StatusNotFound {
		t.Errorf("open gate, the picture: %d, want 404", code)
	}
}

func TestTokenPage(t *testing.T) {
	s := tokenPageServer(t, newTokenWorld(t), "OtherMintOfTheGate")
	h := s.Handler()
	code, body := get(t, h, "/token/"+tokenMint)
	if code != http.StatusOK {
		t.Fatalf("the page answered %d", code)
	}
	if strings.Contains(body, "<!-- render:") {
		t.Fatalf("the template failed: %s", body[strings.Index(body, "<!-- render:"):])
	}
	for _, want := range []string{
		"<title>V2EX &lt;b&gt; · hub.example</title>", // a listing's name is a stranger's word: escaped
		`<link rel="canonical" href="http://hub.example/token/` + tokenMint + `">`,
		`<span class="title">V2EX &lt;b&gt;</span>`,
		`<img id="tk-icon" src="/token/` + tokenMint + `/icon" alt="">`,
		`<b>V2EX</b> · <span class="mono">9raUVuzeWUk53co63M4WXL<wbr>WPWE4Xc6Lpn7RS9dnkpump</span>`,
		`<p>way to explore</p>`,
		`<a href="https://www.v2ex.com/" target="_blank" rel="noopener nofollow">Website</a>`,
		`Mint revoked</span>`,
		`Freeze authority live</span>`, // the chain says someone may still freeze
		`Immutable metadata</span>`,
		`<div class="tk-price">$0.00194617</div>`,
		`<div class="tk-chg down">`, `<span>−1.07%</span><i>24h</i>`,
		`<dt>Holders</dt><dd>5,766</dd>`,
		`<dt>24h volume</dt><dd>$631.71</dd>`,
		`<dt>Supply</dt><dd>999,998,556.43</dd>`,
		`<dt>Liquidity</dt><dd>$199,519</dd>`,
		`<dt>Market cap</dt><dd>$1,946,172</dd>`,
		`<dt>Issuer</dt><dd>pump.fun</dd>`,
		`<dt>Created</dt><dd><time datetime="2024-09-27T14:18:41Z" data-date>2024-09-27</time></dd>`,
		`<dt>Creator</dt><dd><span class="mono" title="4JBz4tAKgAmxjDPHHi9HRLj14RsCQJyuCkCFKnpz7B9s">4JBz…7B9s</span></dd>`,
		`<a class="btn" href="/token/` + tokenMint + `" aria-current="true">24H</a>`,
		`<a class="btn" href="/token/` + tokenMint + `?range=7d">7D</a>`,
		`<div class="tk-chart up" id="tk-chart"`, `data-kind="time"`, `<polyline class="l" points="0.0,`,
		`<b>&#43;5.00%</b> · High $0.002100 · Low $0.001900`, // html/template writes + as &#43;
		`<a href="https://www.geckoterminal.com/solana/pools/BusyPool" target="_blank" rel="noopener nofollow">V2EX / SOL</a><span>PumpSwap · `,
		`<td class="opt">0.00002279</td><td>$199,519</td><td>$631.71</td><td class="opt">62</td>`,
		`<td class="opt">—</td><td>$0</td><td>$0</td><td class="opt">0</td>`,
		`<b>Gate:</b> to post on this hub, the Solana address of your key must hold at least 10,000 V2EX. That is about $19.46 now. The gate also takes: <a class="mono" href="/token/OtherMintOfTheGate">Othe…Gate</a>`,
		`Market data: Jupiter, GeckoTerminal · <time datetime="`,
		// Buy: a push button where the link to jup.ag was, still that link without script, and its window
		`<a class="btn" id="tk-buy" href="https://jup.ag/swap?sell=So11111111111111111111111111111111111111112&amp;buy=` + tokenMint + `" target="_blank" rel="noopener nofollow" title="Buy on jup.ag" data-mint="` + tokenMint + `" data-symbol="V2EX" data-dec="6">Buy</a>`,
		`<div class="tk-veil" id="tk-veil" hidden>`,
		`<h2 id="tk-buy-h"></h2>`, // the script names the token being bought
		`<button type="button" class="btn" data-pay="SOL" aria-pressed="true">SOL</button><button type="button" class="btn" data-pay="USDC" aria-pressed="false">USDC</button><button type="button" class="btn" data-pay="USDT" aria-pressed="false">USDT</button>`,
		`<button type="button" class="btn" data-qty="10000" aria-pressed="true">10,000</button><button type="button" class="btn" data-qty="20000" aria-pressed="false">20,000</button><button type="button" class="btn" data-qty="50000" aria-pressed="false">50,000</button>`,
		`<button type="button" class="btn default" id="tk-go" disabled>Buy</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(body, "orbmarkets") {
		t.Error("the page links to Orb")
	}
	if strings.Contains(body, "<dt>FDV</dt>") {
		t.Error("FDV is listed though it is the market cap")
	}

	// the range in the address: pressed, and the chart set by the day
	_, body = get(t, h, "/token/"+tokenMint+"?range=7d&lang=zh")
	for _, want := range []string{
		`<a class="btn" href="/token/` + tokenMint + `?lang=zh&amp;range=7d" aria-current="true">7D</a>`,
		`<a class="btn" href="/token/` + tokenMint + `?lang=zh">24H</a>`,
		`data-kind="day"`,
		`<dt>持有人</dt><dd>5,766</dd>`,
		`增发权限已放弃</span>`,
		`<b>发帖条件：</b>在这个 hub 发帖，你的密钥对应的 Solana 地址需要持有至少 10,000 个 V2EX。现在约合 $19.46。也接受：<a class="mono" href="/token/OtherMintOfTheGate?lang=zh">`,
		`<a class="btn prev back" href="/?lang=zh">`,
		`行情数据：Jupiter、GeckoTerminal`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("?range=7d&lang=zh lacks %q", want)
		}
	}
	if _, body = get(t, h, "/token/"+tokenMint+"?lang=ja"); !strings.Contains(body, `<dt>保有者</dt>`) || !strings.Contains(body, "<b>投稿の条件：</b>この hub に投稿するには") {
		t.Error("?lang=ja is not in Japanese")
	}

	// the gate's other mint has its page; any other mint has none
	if code, _ := get(t, h, "/token/OtherMintOfTheGate"); code != http.StatusOK {
		t.Errorf("the gate's second mint: %d", code)
	}
	code, body = get(t, h, "/token/So11111111111111111111111111111111111111112")
	if code != http.StatusNotFound || !strings.Contains(body, "This hub&#39;s gate does not ask for that token.") {
		t.Errorf("a mint the gate does not name: %d", code)
	}

	// the home page's join window leads here
	if _, body = get(t, h, "/"); !strings.Contains(body, `(<a href="/token/`+tokenMint+`">about this token</a>)`) {
		t.Error("the join window has no link to the token page")
	}
	// and its Post window carries the guide for a wallet that holds too
	// little — a row, hidden until the gate says so — and the Buy window
	// the guide opens, once
	for _, want := range []string{
		`<div class="row guide" hidden><span class="grow guide-text"></span><button type="button" class="btn guide-buy"></button></div>`,
		`<div class="tk-veil" id="tk-veil" hidden>`,
		`window.hubBuy = opts =>`,
	} {
		if n := strings.Count(body, want); n != 1 {
			t.Errorf("the home page has %q %d times, want once", want, n)
		}
	}
	// a hub with an open gate has neither
	_, body = get(t, testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}}).Handler(), "/")
	for _, not := range []string{`class="row guide"`, `id="tk-veil"`, `window.hubBuy = opts`} {
		if strings.Contains(body, not) {
			t.Errorf("an open hub's home page has %q", not)
		}
	}
}

// With nobody answering, the page still stands: the mint, what the gate
// asks for in raw units, and a line that says the numbers are missing.
func TestTokenPageNothingAnswers(t *testing.T) {
	tw := newTokenWorld(t)
	tw.down = true
	code, body := get(t, tokenPageServer(t, tw).Handler(), "/token/"+tokenMint)
	if code != http.StatusOK {
		t.Fatalf("the page answered %d", code)
	}
	for _, want := range []string{
		"<title>9raU…pump · hub.example</title>",
		`<span class="av"></span>`,
		`<div class="tk-price">—</div>`,
		`<div class="tk-chart none"><div class="tk-plot">The chart is not available right now.</div></div>`,
		`must hold at least 10000000000 raw base units of this token.</div>`,
		`<span>The market&#39;s numbers are not available right now.</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// nobody said the decimals: Buy is the link to jup.ag and has no window
	if !strings.Contains(body, `title="Buy on jup.ag" data-mint="`+tokenMint+`" data-symbol="9raU…pump">Buy</a>`) {
		t.Error("the Buy link is not the plain one")
	}
	for _, not := range []string{`class="tk-flags"`, `class="tk-mk"`, "<!-- render:", `data-dec=`} {
		if strings.Contains(body, not) {
			t.Errorf("the page has %q with nothing known", not)
		}
	}
}

// The picture is fetched by the hub and served as the avatar's square.
func TestTokenIcon(t *testing.T) {
	h := tokenPageServer(t, newTokenWorld(t)).Handler()
	req := httptest.NewRequest("GET", "http://hub.example/token/"+tokenMint+"/icon", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("the picture: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if err != nil || cfg.Width != 128 || cfg.Height != 128 {
		t.Errorf("the picture is %dx%d (%v), want 128 square", cfg.Width, cfg.Height, err)
	}
	if code, _ := get(t, h, "/token/NotTheGatesMint/icon"); code != http.StatusNotFound {
		t.Errorf("a picture for a mint the gate does not name: %d", code)
	}
}

// GET /v1/token/tx/{sig}: what the chain did with a transaction the Buy
// window sent, kept so that a window asking every few seconds costs the
// RPC one call while the answer stands.
func TestTokenTx(t *testing.T) {
	tw := newTokenWorld(t)
	s := tokenPageServer(t, tw)
	h := s.Handler()
	sig := strings.Repeat("5", 87)
	tw.tx = `null`
	if code, body := get(t, h, "/v1/token/tx/"+sig); code != http.StatusOK || !strings.Contains(body, `"status":"pending"`) {
		t.Fatalf("a transaction not seen yet: %d %s", code, body)
	}
	get(t, h, "/v1/token/tx/"+sig)
	if tw.txN != 1 {
		t.Errorf("the RPC was asked %d times for two asks a moment apart, want 1", tw.txN)
	}
	sig2 := strings.Repeat("6", 88)
	tw.tx = `{"err":null,"confirmationStatus":"confirmed"}`
	if code, body := get(t, h, "/v1/token/tx/"+sig2); code != http.StatusOK || !strings.Contains(body, `"status":"confirmed"`) {
		t.Errorf("a confirmed transaction: %d %s", code, body)
	}
	tw.tx = `{"err":{"InstructionError":[0,"Custom"]},"confirmationStatus":"confirmed"}`
	if code, body := get(t, h, "/v1/token/tx/"+strings.Repeat("7", 86)); code != http.StatusOK || !strings.Contains(body, `"status":"failed"`) {
		t.Errorf("a failed transaction: %d %s", code, body)
	}
	for _, bad := range []string{"abc", strings.Repeat("0", 87), strings.Repeat("5", 89)} { // 0 is no base58 digit
		if code, _ := get(t, h, "/v1/token/tx/"+bad); code != http.StatusNotFound {
			t.Errorf("%q is no signature, yet the answer was %d", bad, code)
		}
	}
	// the RPC down: a 503 that says nothing of the RPC's address
	tw.down = true
	code, body := get(t, h, "/v1/token/tx/"+strings.Repeat("8", 87))
	if code != http.StatusServiceUnavailable || strings.Contains(body, "rpc") || strings.Contains(body, "127.0.0.1") {
		t.Errorf("with the RPC down: %d %s", code, body)
	}
	// a burst past the bucket is turned away before the RPC
	tw.down = false
	limited := false
	for i := 0; i < 20 && !limited; i++ {
		code, _ := get(t, h, "/v1/token/tx/"+strings.Repeat("9", 80)+fmt.Sprintf("%07d", 1111111+i))
		limited = code == http.StatusTooManyRequests
	}
	if !limited {
		t.Error("twenty transactions asked about at once and none turned away")
	}
	// a hub with an open gate has no RPC to ask
	if code, _ := get(t, testServer(t, &config.Config{Gate: config.Gate{Mode: "open"}}).Handler(), "/v1/token/tx/"+sig); code != http.StatusNotFound {
		t.Errorf("open gate: %d", code)
	}
}

// /v1/gate names the token for the Post window's guide — its symbol once
// anyone has said it, its decimals — and ?fresh=1 reads the balance
// again though a verdict is kept, which is how a key that has just
// bought what it lacked gets to post without waiting out recheck.
func TestGateGuide(t *testing.T) {
	tw := newTokenWorld(t)
	s := tokenPageServer(t, tw)
	h := s.Handler()
	pub, _, _ := ed25519.GenerateKey(nil)
	author := url.QueryEscape(base64.StdEncoding.EncodeToString(pub))
	ask := func(q string) (out struct {
		Gate  string
		Mints []struct {
			Amount, Held, Symbol, Mint string
			Raw                        bool
			Decimals                   *int
			Price                      float64
			Change                     *float64
		}
	}) {
		t.Helper()
		code, body := get(t, h, "/v1/gate?author="+author+q)
		if code != http.StatusOK {
			t.Fatalf("/v1/gate%s: %d %s", q, code, body)
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	tw.held = "0"
	v := ask("")
	if v.Gate != "below" || len(v.Mints) != 1 || v.Mints[0].Held != "0" || v.Mints[0].Amount != "10,000" || v.Mints[0].Decimals == nil || *v.Mints[0].Decimals != 6 {
		t.Fatalf("a key holding nothing: %+v", v)
	}
	// the symbol is asked for behind the first answer and stands in a later one
	for i := 0; i < 100 && v.Mints[0].Symbol == ""; i++ {
		time.Sleep(10 * time.Millisecond)
		v = ask("")
	}
	if v.Mints[0].Symbol != "V2EX" {
		t.Errorf("symbol = %q", v.Mints[0].Symbol)
	}
	// with it, for the Profile dialog's holding, the price and the day's change
	if m := v.Mints[0]; m.Price != 0.00194617 || m.Change == nil || *m.Change != -1.0744 {
		t.Errorf("price %v, change %v, want Jupiter's 0.00194617 and -1.0744", m.Price, m.Change)
	}
	// the key buys: the kept verdict still says below, a fresh check does not
	tw.held = "10010000000"
	n := tw.balN
	if v = ask(""); v.Gate != "below" || tw.balN != n {
		t.Errorf("a kept verdict: gate %q after %d more balance reads, want below and none", v.Gate, tw.balN-n)
	}
	if v = ask("&fresh=1"); v.Gate != "pass" || v.Mints[0].Held != "10,010" || tw.balN != n+1 {
		t.Errorf("a fresh check: gate %q, held %q, %d balance reads", v.Gate, v.Mints[0].Held, tw.balN-n)
	}
	if v = ask(""); v.Gate != "pass" {
		t.Errorf("after the fresh check the kept verdict is %q", v.Gate)
	}
	// fresh checks are rationed like unkept ones
	limited := false
	for i := 0; i < 20 && !limited; i++ {
		code, _ := get(t, h, "/v1/gate?author="+author+"&fresh=1")
		limited = code == http.StatusTooManyRequests
	}
	if !limited {
		t.Error("twenty fresh checks at once and none turned away")
	}
}

func TestChartOf(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	hour := token.RangeOf("1h") // one-minute candles
	// a candle that ended before the hour began opens the line; one in
	// it moves it; the line runs flat to now
	c := chartOf([]token.Candle{{T: 1_000_000 - 7200, Close: 2}, {T: 1_000_000 - 1860, Close: 3}}, hour, now)
	if c == nil {
		t.Fatal("no chart")
	}
	if c.Data != "996400,2;998200,3;1000000,3" {
		t.Errorf("points = %s", c.Data)
	}
	if !strings.HasPrefix(c.Line, "0.0,") || !strings.HasSuffix(strings.Fields(c.Line)[2], strings.Fields(c.Line)[1][strings.Index(strings.Fields(c.Line)[1], ","):]) {
		t.Errorf("line = %s, want it to begin at the left edge and end flat", c.Line)
	}
	if !strings.HasPrefix(strings.Fields(c.Line)[1], "500.0,") || !strings.HasPrefix(strings.Fields(c.Line)[2], "1000.0,") {
		t.Errorf("line = %s, want points at the middle and the right edge", c.Line)
	}
	if !c.Up || c.Change != "+50.00%" || c.High != "$3.000" || c.Low != "$2.000" || c.From != 996400 || c.To != 1_000_000 {
		t.Errorf("chart = %+v", c)
	}
	if len(c.X) != 4 || c.X[0].Style != "left:12.5%" || c.X[0].Label != time.Unix(996400+450, 0).UTC().Format("15:04") {
		t.Errorf("x ticks = %+v", c.X)
	}
	if len(c.Y) < 2 || !strings.HasPrefix(string(c.Y[0].Style), "top:") || !strings.HasPrefix(c.Y[0].Label, "$") {
		t.Errorf("y ticks = %+v", c.Y)
	}

	// no candle before the range: the line begins where trading did
	c = chartOf([]token.Candle{{T: 1_000_000 - 1860, Close: 3}}, hour, now)
	if c == nil || c.Data != "998200,3;1000000,3" || c.Change != "0.00%" || !c.Up {
		t.Errorf("chart without an opening price = %+v", c)
	}
	// a price that fell
	if c = chartOf([]token.Candle{{T: 1_000_000 - 7200, Close: 4}, {T: 1_000_000 - 1860, Close: 3}}, hour, now); c == nil || c.Up || c.Change != "−25.00%" {
		t.Errorf("a falling chart = %+v", c)
	}
	// nothing at all
	if c = chartOf(nil, hour, now); c != nil {
		t.Errorf("a chart of no candles = %+v", c)
	}
	if k := chartOf([]token.Candle{{T: 1, Close: 1}}, token.RangeOf("1y"), now).Kind; k != "month" {
		t.Errorf("a year's chart sets its times by the %s", k)
	}
}

func TestTokenFigures(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{usdPrice(0.0019597412), "$0.00195974"},
		{usdPrice(0.5), "$0.500000"},
		{usdPrice(86.0154), "$86.02"},
		{usdPrice(12345.678), "$12,345.68"},
		{usdAmount(200715.4), "$200,715"},
		{usdAmount(657.94), "$657.94"},
		{usdAmount(0.11), "$0.11"},
		{usdAmount(0), ""},
		{grouped(999998556.42674, 2), "999,998,556.43"},
		{grouped(5766, 0), "5,766"},
		{grouped(-1234.5, 1), "-1,234.5"},
		{figures(0.0000227863, 4), "0.00002279"},
		{figures(0.0145, 4), "0.01450"},
		{figures(44152.769, 4), "44,153"},
		{figures(0, 4), "0"},
		{percent(2.4211), "+2.42%"},
		{percent(-0.301), "−0.30%"},
		{percent(-0.001), "0.00%"},
		{direction(2.4), "up"},
		{direction(-2.4), "down"},
		{direction(0.001), ""},
		{shortMint(tokenMint), "9raU…pump"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	ticks, places := niceTicks(0.001921, 0.001989, 4)
	if len(ticks) != 3 || places != 5 || fmt.Sprintf("%.5f %.5f %.5f", ticks[0], ticks[1], ticks[2]) != "0.00194 0.00196 0.00198" {
		t.Errorf("ticks = %v at %d places", ticks, places)
	}
	if ticks, _ := niceTicks(5, 5, 4); ticks != nil {
		t.Errorf("ticks of an empty span = %v", ticks)
	}
}
