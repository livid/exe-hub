package token

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
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
	das   bool   // the RPC speaks getAsset
	tx    string // what getSignatureStatuses says of the one signature asked
	// the pool's candles, the oldest first (an unusable one has Close 0),
	// and how many each asking asked for
	candles []Candle
	limits  []int
	window  int // the most candles one answer holds, however many are asked for; 0 for no such cap
	// candles before this time are refused (401), as those past
	// GeckoTerminal's free history are; 0 for no such floor
	floor int64
	// failOlder answers 429 to every asking for older candles
	failOlder bool
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
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		before, _ := strconv.ParseInt(r.URL.Query().Get("before_timestamp"), 10, 64)
		f.limits = append(f.limits, limit)
		// the last limit of them — of those before before_timestamp, and no
		// more than a window's worth — newest first, as GeckoTerminal sends them
		rows := f.candles
		if before > 0 && f.failOlder {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		if before > 0 && f.floor > 0 && before <= f.floor {
			http.Error(w, "older than the free history", http.StatusUnauthorized)
			return
		}
		if before > 0 {
			k := 0
			for k < len(rows) && rows[k].T < before {
				k++
			}
			rows = rows[:k]
		}
		if f.window > 0 && limit > f.window {
			limit = f.window
		}
		if len(rows) > limit {
			rows = rows[len(rows)-limit:]
		}
		var b strings.Builder
		for i := len(rows) - 1; i >= 0; i-- {
			if b.Len() > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "[%d,0,0,0,%g,1]", rows[i].T, rows[i].Close)
		}
		fmt.Fprintf(w, `{"data":{"attributes":{"ohlcv_list":[%s]}}}`, b.String())
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
		case "getSignatureStatuses":
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1},"value":[`+f.tx+`]}}`)
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

// memStore is a CandleStore in memory: what the hub's database is to
// the hub (internal/store has the real one and its own test).
type memStore struct {
	cs           map[string][]Candle
	since, until map[string]int64
	pools        map[string]string
	saves        int
}

func newMemStore() *memStore {
	return &memStore{cs: map[string][]Candle{}, since: map[string]int64{}, until: map[string]int64{}, pools: map[string]string{}}
}

func (m *memStore) ChartPool(mint string) (string, error) { return m.pools[mint], nil }
func (m *memStore) SetChartPool(mint, pool string) error  { m.pools[mint] = pool; m.saves++; return nil }

func (m *memStore) LoadCandles(pool, frame string) ([]Candle, int64, int64, error) {
	k := pool + ":" + frame
	return append([]Candle(nil), m.cs[k]...), m.since[k], m.until[k], nil
}

func (m *memStore) SaveCandles(pool, frame string, add []Candle, since, until, dropBefore int64, reset bool) error {
	k := pool + ":" + frame
	m.saves++
	byT := map[int64]Candle{}
	if !reset {
		for _, c := range m.cs[k] {
			if c.T >= dropBefore {
				byT[c.T] = c
			}
		}
	}
	for _, c := range add {
		byT[c.T] = c
	}
	all := make([]Candle, 0, len(byT))
	for _, c := range byT {
		all = append(all, c)
	}
	slices.SortFunc(all, func(a, b Candle) int { return int(a.T - b.T) })
	m.cs[k], m.since[k], m.until[k] = all, since, until
	return nil
}

// A closed candle is asked for once. GeckoTerminal is asked again only
// when a new candle can have closed, and then for the few since; the
// span still open is never kept; what is kept outlives the Source and a
// GeckoTerminal that stops answering.
func TestCandlesKept(t *testing.T) {
	f := newFake(t)
	const hour = 3600
	// 12:30 on some day; the 7d range's candles are an hour each
	now := time.Unix(1_800_000_000/hour*hour+1800, 0)
	h0 := now.Unix() / hour * hour // the hour now open
	// forty closed hours, one of them unusable, and the open one
	for i := 40; i >= 1; i-- {
		c := Candle{T: h0 - int64(i)*hour, Close: 1 + float64(i)/100}
		if i == 7 {
			c.Close = 0
		}
		f.candles = append(f.candles, c)
	}
	f.candles = append(f.candles, Candle{T: h0, Close: 9})
	st := newMemStore()
	s := f.source()
	s.Store = st
	s.now = func() time.Time { return now }
	week := RangeOf("7d")

	cs, at := s.Candles(testMint, "BusyPool", week)
	if len(cs) != 39 || cs[0].T != h0-40*hour || cs[len(cs)-1].T != h0-hour || at.IsZero() {
		t.Fatalf("first reading: %d candles from %d to %d, want the 39 usable closed ones", len(cs), cs[0].T, cs[len(cs)-1].T)
	}
	// forty hours do not reach a week back: the page before them is asked for, comes back
	// empty, and so the forty are the pool's whole history
	if len(f.limits) != 2 || f.limits[0] != 300 {
		t.Errorf("first reading: limits %v, want the range's 300 and one page more", f.limits)
	}
	if n := len(st.cs["BusyPool:hour/1"]); n != 39 || st.since["BusyPool:hour/1"] != 0 || st.until["BusyPool:hour/1"] != h0 {
		t.Errorf("stored: %d candles, since %d, until %d; want 39, the whole history, up to the open hour", n, st.since["BusyPool:hour/1"], st.until["BusyPool:hour/1"])
	}

	// the same hour: nobody is asked
	now = now.Add(20 * time.Minute)
	s.Candles(testMint, "BusyPool", week)
	if n := f.asked["candles"].Load(); n != 2 {
		t.Errorf("asked %d times within the hour, want the first reading's 2", n)
	}

	// the hour turns: the open candle has closed (at another price than it showed), a new one is open
	f.candles[len(f.candles)-1].Close = 2
	f.candles = append(f.candles, Candle{T: h0 + hour, Close: 9})
	now = time.Unix(h0+hour+600, 0)
	for i := 0; i < 100; i++ { // the asking runs behind the reader
		cs, _ = s.Candles(testMint, "BusyPool", week)
		if len(cs) == 40 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(cs) != 40 || cs[39].T != h0 || cs[39].Close != 2 {
		t.Fatalf("after the hour: %d candles, the last %+v; want 40 ending with the hour that closed at 2", len(cs), cs[len(cs)-1])
	}
	if len(f.limits) != 3 || f.limits[2] != 3 {
		t.Errorf("after the hour the asking was for %v candles, want a third, for 3: the one that closed and two for the join", f.limits)
	}
	if n := len(st.cs["BusyPool:hour/1"]); n != 40 || st.until["BusyPool:hour/1"] != h0+hour {
		t.Errorf("stored after the hour: %d candles until %d", n, st.until["BusyPool:hour/1"])
	}

	// the hub is started again, and GeckoTerminal answers nothing: the chart is the stored one
	f.down["candles"] = true
	again := f.source()
	again.Store = st
	again.now = func() time.Time { return now }
	before := f.asked["candles"].Load()
	cs, at = again.Candles(testMint, "BusyPool", week)
	if len(cs) != 40 || at.IsZero() || f.asked["candles"].Load() != before {
		t.Errorf("a new Source in the same hour: %d candles, asked %d more times; want the 40 stored and no asking", len(cs), f.asked["candles"].Load()-before)
	}
	// and when an hour has turned meanwhile it is asked, fails, and what is kept still stands
	now = now.Add(time.Hour)
	third := f.source()
	third.Store = st
	third.now = func() time.Time { return now }
	if cs, at = third.Candles(testMint, "BusyPool", week); len(cs) != 40 || at.IsZero() {
		t.Errorf("with GeckoTerminal down after a restart: %d candles", len(cs))
	}
	if f.asked["candles"].Load() == before {
		t.Error("a closed hour was not asked for")
	}

	// no pool, an unknown range
	if cs, _ := s.Candles(testMint, "", week); cs != nil {
		t.Error("candles for no pool")
	}
	if r := RangeOf("nonsense"); r.Name != DefaultRange {
		t.Errorf("RangeOf(nonsense) = %q", r.Name)
	}
	if r := RangeOf("1Y"); r.Name != "1y" || r.Frame() != "day/1" {
		t.Errorf("RangeOf(1Y) = %q, frame %q", r.Name, r.Frame())
	}
}

// GeckoTerminal answers with about six months of candles at the most. A
// year's daily candles are read a page at a time until they reach the
// year's start; a pool younger than the range is read to its beginning,
// and its history is whole.
func TestCandlesPagedBack(t *testing.T) {
	const day = 86400
	now := time.Unix(1_800_000_000/day*day+12*3600, 0)
	d0 := now.Unix() / day * day
	year := RangeOf("1y")
	fill := func(f *fake, days int) {
		f.candles = nil
		for i := days; i >= 1; i-- {
			f.candles = append(f.candles, Candle{T: d0 - int64(i)*day, Close: 1 + float64(i)/1000})
		}
		f.candles = append(f.candles, Candle{T: d0, Close: 9}) // today, still open
		f.window = 180
	}
	// a pool 500 days old
	f := newFake(t)
	fill(f, 500)
	st := newMemStore()
	s := f.source()
	s.Store, s.now = st, func() time.Time { return now }
	cs, _ := s.Candles(testMint, "BusyPool", year)
	if len(f.limits) != 3 {
		t.Fatalf("asked %d times (%v), want three pages", len(f.limits), f.limits)
	}
	// 180 + 180 + 180 answered, today's not kept: 539 closed days, back past the year's start
	if len(cs) != 500 && (len(cs) < 366 || cs[0].T > d0-366*day || cs[len(cs)-1].T != d0-day) {
		t.Fatalf("%d candles from %d days ago to %d days ago; want the year covered up to yesterday", len(cs), (d0-cs[0].T)/day, (d0-cs[len(cs)-1].T)/day)
	}
	if since := st.since["BusyPool:day/1"]; since == 0 || since > d0-366*day {
		t.Errorf("since = %d days ago, want a day at or before the year's start and not the whole history", (d0-since)/day)
	}
	// nothing more is asked that day; the next day one page is: the day that closed, and two for the join
	s.Candles(testMint, "BusyPool", year)
	now = now.Add(24 * time.Hour)
	f.candles = append(f.candles, Candle{T: d0 + day, Close: 9})
	want := len(cs) + 1
	for i := 0; i < 100; i++ {
		if cs, _ = s.Candles(testMint, "BusyPool", year); len(cs) == want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(cs) != want || len(f.limits) != 4 || f.limits[3] != 3 {
		t.Errorf("the next day: %d candles (want %d), askings %v (want a fourth, for 3)", len(cs), want, f.limits)
	}

	// a pool 200 days old: two pages and an empty third, and its history is whole
	f2 := newFake(t)
	fill(f2, 200)
	st2 := newMemStore()
	s2 := f2.source()
	s2.Store, s2.now = st2, func() time.Time { return now.Add(-24 * time.Hour) }
	cs, _ = s2.Candles(testMint, "BusyPool", year)
	if len(cs) != 200 || len(f2.limits) != 3 || st2.since["BusyPool:day/1"] != 0 {
		t.Errorf("a young pool: %d candles, %d askings, since %d; want all 200, three askings, the whole history", len(cs), len(f2.limits), st2.since["BusyPool:day/1"])
	}
	// and with its history whole nothing is asked again that day
	s2.Candles(testMint, "BusyPool", year)
	if len(f2.limits) != 3 {
		t.Errorf("a whole history was asked for again: %v", f2.limits)
	}

	// GeckoTerminal's free history ends 300 days back: the page before that is refused, and
	// what was read is all there is to have — kept as such, and not asked for at every reading
	f3 := newFake(t)
	fill(f3, 500)
	f3.floor = d0 - 300*day
	st3 := newMemStore()
	s3 := f3.source()
	s3.Store, s3.now = st3, func() time.Time { return now.Add(-24 * time.Hour) }
	cs, _ = s3.Candles(testMint, "BusyPool", year)
	if len(cs) != 359 || len(f3.limits) != 3 || st3.since["BusyPool:day/1"] != 0 {
		t.Errorf("a floor on the history: %d candles, %d askings, since %d; want the 359 closed days of two pages, a refused third, and no more to have", len(cs), len(f3.limits), st3.since["BusyPool:day/1"])
	}
	s3.Candles(testMint, "BusyPool", year)
	if len(f3.limits) != 3 {
		t.Errorf("what cannot be had was asked for again: %v", f3.limits)
	}

	// a page that fails for a while (429) leaves what was read; the older candles are asked
	// for again after ten minutes, alone, and join what is kept
	f4 := newFake(t)
	fill(f4, 500)
	st4 := newMemStore()
	s4 := f4.source()
	at := now.Add(-24 * time.Hour)
	s4.Store, s4.now = st4, func() time.Time { return at }
	f4.failOlder = true
	cs, _ = s4.Candles(testMint, "BusyPool", year)
	if len(cs) != 179 || st4.since["BusyPool:day/1"] == 0 {
		t.Fatalf("with the older pages failing: %d candles, since %d; want the first page's 179 closed days", len(cs), st4.since["BusyPool:day/1"])
	}
	n := len(f4.limits)
	s4.Candles(testMint, "BusyPool", year)
	time.Sleep(20 * time.Millisecond)
	if len(f4.limits) != n {
		t.Errorf("the older pages were asked for again at once: %d askings more", len(f4.limits)-n)
	}
	f4.failOlder = false
	at = at.Add(11 * time.Minute)
	for i := 0; i < 100; i++ {
		if cs, _ = s4.Candles(testMint, "BusyPool", year); len(cs) > 366 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(cs) < 366 || cs[len(cs)-1].T != d0-day || len(st4.cs["BusyPool:day/1"]) != len(cs) {
		t.Errorf("after ten minutes: %d candles, %d stored; want the year covered and kept", len(cs), len(st4.cs["BusyPool:day/1"]))
	}
	for k := 1; k < len(cs); k++ {
		if cs[k].T <= cs[k-1].T {
			t.Fatalf("candles out of order or twice at %d: %d then %d", k, cs[k-1].T, cs[k].T)
		}
	}
}

// The pool a chart is drawn from is kept too: a Source started while
// GeckoTerminal says nothing of the pools still names it, and ends the
// chart at Jupiter's price.
func TestChartPoolKept(t *testing.T) {
	f := newFake(t)
	st := newMemStore()
	s := f.source()
	s.Store = st
	if in := s.Info(testMint); in.Pool != "BusyPool" || in.PoolPrice != 0.001948201042 {
		t.Fatalf("pool %q at %v", in.Pool, in.PoolPrice)
	}
	if st.pools[testMint] != "BusyPool" {
		t.Fatalf("the store keeps %q", st.pools[testMint])
	}
	n := st.saves
	s.Info(testMint)
	if st.saves != n {
		t.Error("the same pool was written to the store again")
	}
	f.down["pools"] = true
	again := f.source()
	again.Store = st
	in := again.Info(testMint)
	if in.Pool != "BusyPool" || in.PoolPrice != in.Price || in.Price == 0 || len(in.Markets) != 0 {
		t.Errorf("with the pools unanswered: pool %q at %v (Jupiter says %v), %d markets", in.Pool, in.PoolPrice, in.Price, len(in.Markets))
	}
	// with no store there is nothing to go by
	bare := f.source()
	if in := bare.Info(testMint); in.Pool != "" {
		t.Errorf("no store, no pools: pool %q", in.Pool)
	}
}

// What a range will never show again is dropped, all but the candle the
// range opens at; a hub that was away longer than a range takes lets
// what it kept go, since a hole would be drawn as a flat line.
func TestCandlesPrunedAndReset(t *testing.T) {
	f := newFake(t)
	now := time.Unix(1_800_000_000/60*60+30, 0)
	m0 := now.Unix() / 60 * 60
	hourR := RangeOf("1h") // one-minute candles, kept three hours
	// a trade five hours ago, one four hours ago, then one a minute for the last ten
	f.candles = []Candle{{T: m0 - 300*60, Close: 1}, {T: m0 - 240*60, Close: 2}}
	for i := 12; i >= 3; i-- {
		f.candles = append(f.candles, Candle{T: m0 - int64(i)*60, Close: 3})
	}
	st := newMemStore()
	s := f.source()
	s.Store = st
	s.now = func() time.Time { return now }
	cs, _ := s.Candles(testMint, "BusyPool", hourR)
	if len(cs) != 11 || cs[0].T != m0-240*60 || cs[0].Close != 2 {
		t.Fatalf("kept %d candles from %+v; want the ten of the hour and the one before the cut, the price it opens at", len(cs), cs[0])
	}
	if n := len(st.cs["BusyPool:minute/1"]); n != 11 {
		t.Errorf("stored %d", n)
	}

	// away for a day; 151 candles closed meanwhile, more than an asking takes: the page before
	// them is asked for too, reaches what is kept, and nothing is lost
	now = now.Add(24 * time.Hour)
	m1 := now.Unix() / 60 * 60
	for i := 153; i >= 3; i-- {
		f.candles = append(f.candles, Candle{T: m1 - int64(i)*60, Close: 4})
	}
	again := f.source()
	again.Store = st
	again.now = func() time.Time { return now }
	cs, _ = again.Candles(testMint, "BusyPool", hourR)
	if len(cs) != 152 || cs[0].Close != 3 || cs[1].T != m1-153*60 {
		t.Errorf("after a day away: %d candles from %+v; want the 151 of the day and the one before the cut", len(cs), cs[0])
	}
	if got := st.cs["BusyPool:minute/1"]; len(got) != 152 {
		t.Errorf("stored after a day away: %d", len(got))
	}

	// away again, and GeckoTerminal hands out ten candles a page: four pages do not reach what
	// is kept, so it is let go — a hole would be drawn as a flat line
	now = now.Add(24 * time.Hour)
	m2 := now.Unix() / 60 * 60
	for i := 100; i >= 3; i-- {
		f.candles = append(f.candles, Candle{T: m2 - int64(i)*60, Close: 5})
	}
	f.window = 10
	third := f.source()
	third.Store = st
	third.now = func() time.Time { return now }
	cs, _ = third.Candles(testMint, "BusyPool", hourR)
	if len(cs) != 40 || cs[0].T != m2-42*60 || cs[0].Close != 5 {
		t.Errorf("after a hole: %d candles from %+v; want the 40 read and nothing older", len(cs), cs[0])
	}
	if got := st.cs["BusyPool:minute/1"]; len(got) != 40 || st.since["BusyPool:minute/1"] != m2-42*60 {
		t.Errorf("stored after a hole: %d since %d", len(got), st.since["BusyPool:minute/1"])
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

func TestTxStatus(t *testing.T) {
	f := newFake(t)
	s := f.source()
	for answer, want := range map[string]string{
		`null`: "pending",
		`{"slot":1,"confirmations":null,"err":null,"confirmationStatus":"processed"}`:                                  "pending",
		`{"slot":1,"confirmations":3,"err":null,"confirmationStatus":"confirmed"}`:                                     "confirmed",
		`{"slot":1,"confirmations":null,"err":null,"confirmationStatus":"finalized"}`:                                  "confirmed",
		`{"slot":1,"confirmations":3,"err":{"InstructionError":[2,{"Custom":6001}]},"confirmationStatus":"confirmed"}`: "failed",
	} {
		f.tx = answer
		if got, err := s.TxStatus(context.Background(), "sig"); err != nil || got != want {
			t.Errorf("%s: %q, %v, want %q", answer, got, err, want)
		}
	}
	f.down["rpc"] = true
	if _, err := s.TxStatus(context.Background(), "sig"); err == nil {
		t.Error("a dead RPC gave a status")
	}
	if _, err := New(nil).TxStatus(context.Background(), "sig"); err == nil {
		t.Error("no RPC gave a status")
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
	s.Store = newMemStore()
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
