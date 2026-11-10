package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
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

// tokenWorld stands in for everything the token page asks: Jupiter,
// GeckoTerminal, a Solana RPC and the address of the token's picture.
// down turns all of it off.
type tokenWorld struct {
	*httptest.Server
	down bool
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
		`<a href="https://orbmarkets.io/token/` + tokenMint + `" target="_blank" rel="noopener nofollow">Explorer</a>`,
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
		`<dt>Creator</dt><dd><a class="mono" href="https://orbmarkets.io/address/4JBz4tAKgAmxjDPHHi9HRLj14RsCQJyuCkCFKnpz7B9s"`,
		`<a class="btn" href="/token/` + tokenMint + `" aria-current="true">24H</a>`,
		`<a class="btn" href="/token/` + tokenMint + `?range=7d">7D</a>`,
		`<div class="tk-chart up" id="tk-chart"`, `data-kind="time"`, `<polyline class="l" points="0.0,`,
		`<b>&#43;5.00%</b> · High $0.002100 · Low $0.001900`, // html/template writes + as &#43;
		`<a href="https://www.geckoterminal.com/solana/pools/BusyPool" target="_blank" rel="noopener nofollow">V2EX / SOL</a><span>PumpSwap · `,
		`<td class="opt">0.00002279</td><td>$199,519</td><td>$631.71</td><td class="opt">62</td>`,
		`<td class="opt">—</td><td>$0</td><td>$0</td><td class="opt">0</td>`,
		`<b>Gate:</b> to post on this hub, the Solana address of your key must hold at least 10,000 V2EX. That is about $19.46 now. The gate also takes: <a class="mono" href="/token/OtherMintOfTheGate">Othe…Gate</a>`,
		`Market data: Jupiter, GeckoTerminal · <time datetime="`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
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
	for _, not := range []string{`class="tk-flags"`, `class="tk-mk"`, "<!-- render:"} {
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
