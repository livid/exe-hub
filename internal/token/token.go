// Package token reads what is known about an SPL token, for the page a
// token-gated hub shows about its gate's token (PLAN.md, The token page).
//
// Three places are asked, and none of them needs a key of its own:
//
//   - the gate's RPC, for what the chain itself says of the mint: its
//     supply, its decimals and whether anyone may still mint or freeze
//     (getAccountInfo, which every RPC answers), and — where the RPC
//     speaks the DAS API, as Helius does — its description and whether
//     its metadata may still change (getAsset);
//   - Jupiter's token search, for the name, the picture, the price, the
//     holder count, where it was launched and when;
//   - GeckoTerminal, for the pools it trades in and a pool's candles.
//
// Each answer is kept on its own for a while (Source.keep). A place that
// stops answering does not empty the page: its last answer stands until
// it answers again, and the page says how old its numbers are.
package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultJupiter = "https://lite-api.jup.ag"
	DefaultGecko   = "https://api.geckoterminal.com/api/v2"
)

// How long each answer is believed. The market's numbers move by the
// minute and both places serve them from a cache of about that age; what
// the chain says of a mint changes rarely, and its metadata rarer still.
const (
	marketFresh = time.Minute
	mintFresh   = 10 * time.Minute
	assetFresh  = time.Hour
	// retryAfter is how soon a place that failed is asked again: every
	// reader in between gets the last answer, or none, at once.
	retryAfter = 20 * time.Second
	// MaxMarkets is how many pools the page lists.
	MaxMarkets = 10
)

// Info is a token as the page shows it. A number nobody answered is
// zero, a yes-or-no nobody answered is nil; the page leaves such a row
// out rather than print a nought.
type Info struct {
	Mint               string
	Name, Symbol, Desc string
	Icon               []string // the picture's addresses, the likeliest first
	Website, Twitter   string
	Decimals           int
	Program            string  // the token program that owns the mint
	Supply             float64 // whole tokens
	Price              float64 // US dollars
	Change             *float64
	Holders            int
	Volume             float64 // 24 hours, US dollars
	Liquidity          float64
	MarketCap, FDV     float64
	Issuer             string // the launchpad, when it came from one
	Created            time.Time
	Creator            string
	MintRevoked        *bool // nobody may mint more
	FreezeRevoked      *bool // nobody may freeze a holder's account
	Immutable          *bool // the metadata cannot change
	Markets            []Market
	Pool               string    // the pool the chart is drawn from: the deepest
	At                 time.Time // when the market's numbers were read; zero when there are none
}

// Market is one pool the token trades in.
type Market struct {
	Address   string
	Pair      string // "V2EX / SOL"
	Dex       string
	Created   time.Time
	Rate      float64 // the pool's base token in its quote token
	Price     float64 // this token in US dollars, in this pool
	Liquidity float64
	Volume    float64 // 24 hours
	Trades    int     // 24 hours
}

// Candle is a span of trading in one pool: when it began, the price it
// ended at in US dollars, and what was traded.
type Candle struct {
	T      int64 // unix seconds
	Close  float64
	Volume float64
}

// Range is a span the chart can show: its name in the address, how long
// it is, and the candles that fill it.
type Range struct {
	Name      string
	Span      time.Duration
	frame     string
	aggregate int
	limit     int
	fresh     time.Duration
}

// Ranges are the chart's spans, the shortest first. GeckoTerminal leaves
// out a candle in which nothing traded, so a quiet token's last sixty
// one-minute candles may reach back a day: each range asks for more
// candles than fill it, and the chart keeps those inside its span and
// the last one before it, which is the price the span opened at.
var Ranges = []Range{
	{"1h", time.Hour, "minute", 1, 120, time.Minute},
	{"24h", 24 * time.Hour, "minute", 15, 200, 2 * time.Minute},
	{"7d", 7 * 24 * time.Hour, "hour", 1, 300, 10 * time.Minute},
	{"1m", 30 * 24 * time.Hour, "hour", 4, 300, 30 * time.Minute},
	{"1y", 365 * 24 * time.Hour, "day", 1, 400, time.Hour},
}

// Step is how long one of the range's candles is.
func (r Range) Step() time.Duration {
	unit := time.Minute
	switch r.frame {
	case "hour":
		unit = time.Hour
	case "day":
		unit = 24 * time.Hour
	}
	return time.Duration(r.aggregate) * unit
}

// DefaultRange is the span a page shows when its address names none.
const DefaultRange = "24h"

// RangeOf is the range called name, the default one for any other word.
func RangeOf(name string) Range {
	for _, r := range Ranges {
		if r.Name == strings.ToLower(name) {
			return r
		}
	}
	return RangeOf(DefaultRange)
}

// Source asks the three places and keeps their answers. The zero value
// is not ready: use New.
type Source struct {
	Jupiter, Gecko string        // base addresses; a test's own servers
	RPC            func() string // the gate's RPC as the config has it now; "" for none
	Log            func(format string, v ...any)

	client *http.Client
	mu     sync.Mutex
	slots  map[string]*slot
	now    func() time.Time
}

// New is a Source that asks Jupiter, GeckoTerminal and the RPC rpc names.
func New(rpc func() string) *Source {
	return &Source{Jupiter: DefaultJupiter, Gecko: DefaultGecko, RPC: rpc, Log: log.Printf,
		client: &http.Client{Timeout: 10 * time.Second}, slots: map[string]*slot{}}
}

func (s *Source) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// slot is one kept answer.
type slot struct {
	val   any
	ok    bool          // val is an answer
	at    time.Time     // when it was read
	tried time.Time     // the last time the place was asked
	busy  chan struct{} // closed when the asking in flight ends; nil when none is
}

// keep is the answer under key and when it was read. An answer younger
// than fresh is returned as it is. An older one is returned too, at
// once, while the place is asked again behind the reader: nobody waits
// for a number they already have. Only when there is no answer at all
// does the reader wait for the one being asked for — every reader for
// the same one. A place that failed is left alone for retryAfter.
func (s *Source) keep(key string, fresh time.Duration, ask func(context.Context) (any, error)) (any, time.Time) {
	s.mu.Lock()
	sl := s.slots[key]
	if sl == nil {
		sl = &slot{}
		s.slots[key] = sl
	}
	now := s.clock()
	if sl.ok && now.Sub(sl.at) < fresh {
		defer s.mu.Unlock()
		return sl.val, sl.at
	}
	if sl.busy == nil && (sl.tried.IsZero() || now.Sub(sl.tried) >= retryAfter) {
		done := make(chan struct{})
		sl.busy, sl.tried = done, now
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			v, err := ask(ctx)
			s.mu.Lock()
			if err == nil {
				sl.val, sl.ok, sl.at = v, true, s.clock()
			} else if s.Log != nil {
				s.Log("token: %s: %v", key, err)
			}
			sl.busy = nil
			s.mu.Unlock()
			close(done)
		}()
	}
	val, ok, at, busy := sl.val, sl.ok, sl.at, sl.busy
	s.mu.Unlock()
	if ok || busy == nil {
		return val, at
	}
	<-busy
	s.mu.Lock()
	defer s.mu.Unlock()
	return sl.val, sl.at
}

// Info is the token as the three places describe it now. It never
// fails: what nobody answered is left empty.
func (s *Source) Info(mint string) *Info {
	var (
		wg              sync.WaitGroup
		jup             *jupToken
		pools           []Market
		onchain         *mintAccount
		asset           *dasAsset
		jupAt, poolsAt  time.Time
		rpc             = ""
		jupKey, poolKey = "jupiter:" + mint, "pools:" + mint
	)
	if s.RPC != nil {
		rpc = s.RPC()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		v, at := s.keep(jupKey, marketFresh, func(ctx context.Context) (any, error) { return s.jupiter(ctx, mint) })
		jup, _ = v.(*jupToken)
		jupAt = at
	}()
	go func() {
		defer wg.Done()
		v, at := s.keep(poolKey, marketFresh, func(ctx context.Context) (any, error) { return s.pools(ctx, mint) })
		pools, _ = v.([]Market)
		poolsAt = at
	}()
	if rpc != "" {
		wg.Add(2)
		go func() {
			defer wg.Done()
			v, _ := s.keep("mint:"+mint, mintFresh, func(ctx context.Context) (any, error) { return s.mintAccount(ctx, rpc, mint) })
			onchain, _ = v.(*mintAccount)
		}()
		go func() {
			defer wg.Done()
			v, _ := s.keep("asset:"+mint, assetFresh, func(ctx context.Context) (any, error) { return s.asset(ctx, rpc, mint) })
			asset, _ = v.(*dasAsset)
		}()
	}
	wg.Wait()

	in := &Info{Mint: mint}
	// the chain first: what it says of the mint is not an opinion
	if onchain != nil {
		in.Decimals, in.Program = onchain.Decimals, onchain.Program
		in.Supply = onchain.Supply / math.Pow10(onchain.Decimals)
		in.MintRevoked, in.FreezeRevoked = ptr(onchain.MintAuthority == ""), ptr(onchain.FreezeAuthority == "")
	}
	if asset != nil {
		in.Name, in.Symbol, in.Desc = asset.Name, asset.Symbol, asset.Desc
		if asset.Image != "" {
			in.Icon = append(in.Icon, asset.Image)
		}
		if asset.Known {
			in.Immutable = ptr(!asset.Mutable)
		}
	}
	if jup != nil {
		in.Name, in.Symbol = or(in.Name, jup.Name), or(in.Symbol, jup.Symbol)
		if jup.Icon != "" && !slices.Contains(in.Icon, jup.Icon) {
			in.Icon = append(in.Icon, jup.Icon)
		}
		in.Website, in.Twitter = webLink(jup.Website), webLink(jup.Twitter)
		in.Price, in.Holders = jup.USDPrice, jup.HolderCount
		in.MarketCap, in.FDV, in.Liquidity = jup.MCap, jup.FDV, jup.Liquidity
		if st := jup.Stats24h; st != nil {
			in.Change = st.PriceChange
			in.Volume = st.BuyVolume + st.SellVolume
		}
		in.Issuer, in.Creator = jup.Launchpad, jup.Dev
		if t, err := time.Parse(time.RFC3339, jup.CreatedAt); err == nil {
			in.Created = t
		}
		if onchain == nil {
			in.Decimals, in.Supply, in.Program = jup.Decimals, jup.TotalSupply, jup.TokenProgram
			if a := jup.Audit; a != nil {
				in.MintRevoked, in.FreezeRevoked = a.MintAuthorityDisabled, a.FreezeAuthorityDisabled
			}
		}
		in.At = jupAt
	}
	if len(pools) > 0 {
		// a pool list is the whole market: its sums are the token's
		// volume and liquidity, where Jupiter counts its own routes.
		// Liquidity is summed over the pools that traded today: an idle
		// pool's reserves are valued at the price of its last trade,
		// which may be months old and far from the market's (V2EX's idle
		// pools added half again to its liquidity, at twice its price).
		// With no pool trading, the deepest stands alone.
		in.Volume, in.Liquidity = 0, 0
		deep := pools[0]
		for _, p := range pools {
			in.Volume += p.Volume
			if p.Volume > 0 {
				in.Liquidity += p.Liquidity
			}
			if p.Liquidity > deep.Liquidity {
				deep = p
			}
		}
		if in.Liquidity == 0 {
			in.Liquidity = deep.Liquidity
		}
		in.Pool = deep.Address
		if in.Price == 0 {
			in.Price = deep.Price
		}
		in.Markets = pools
		if len(in.Markets) > MaxMarkets {
			in.Markets = in.Markets[:MaxMarkets]
		}
		if poolsAt.After(in.At) {
			in.At = poolsAt
		}
	}
	if in.FDV == 0 && in.Price > 0 {
		in.FDV = in.Price * in.Supply
	}
	if in.MarketCap == 0 {
		in.MarketCap = in.FDV
	}
	return in
}

// Candles is the pool's price over r, the oldest first, and when it was
// read; nil when GeckoTerminal has not answered.
func (s *Source) Candles(mint, pool string, r Range) ([]Candle, time.Time) {
	if pool == "" {
		return nil, time.Time{}
	}
	v, at := s.keep("candles:"+pool+":"+r.Name, r.fresh, func(ctx context.Context) (any, error) {
		return s.candles(ctx, mint, pool, r)
	})
	c, _ := v.([]Candle)
	return c, at
}

func ptr[T any](v T) *T { return &v }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// webLink is u when it is an http(s) address, "" for anything else: a
// link a stranger wrote into a token's listing goes on the page only as
// what a browser opens as a page.
func webLink(u string) string {
	pu, err := url.Parse(strings.TrimSpace(u))
	if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Hostname() == "" {
		return ""
	}
	return pu.String()
}

// ---- Jupiter ----

type jupToken struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Symbol       string  `json:"symbol"`
	Icon         string  `json:"icon"`
	Decimals     int     `json:"decimals"`
	Twitter      string  `json:"twitter"`
	Website      string  `json:"website"`
	Dev          string  `json:"dev"`
	TotalSupply  float64 `json:"totalSupply"`
	TokenProgram string  `json:"tokenProgram"`
	Launchpad    string  `json:"launchpad"`
	HolderCount  int     `json:"holderCount"`
	FDV          float64 `json:"fdv"`
	MCap         float64 `json:"mcap"`
	USDPrice     float64 `json:"usdPrice"`
	Liquidity    float64 `json:"liquidity"`
	CreatedAt    string  `json:"createdAt"`
	Stats24h     *struct {
		PriceChange *float64 `json:"priceChange"`
		BuyVolume   float64  `json:"buyVolume"`
		SellVolume  float64  `json:"sellVolume"`
	} `json:"stats24h"`
	Audit *struct {
		MintAuthorityDisabled   *bool `json:"mintAuthorityDisabled"`
		FreezeAuthorityDisabled *bool `json:"freezeAuthorityDisabled"`
	} `json:"audit"`
}

func (s *Source) jupiter(ctx context.Context, mint string) (*jupToken, error) {
	var out []jupToken
	if err := s.getJSON(ctx, s.Jupiter+"/tokens/v2/search?query="+url.QueryEscape(mint), &out); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].ID == mint {
			return &out[i], nil
		}
	}
	return nil, errors.New("Jupiter does not list this mint")
}

// ---- GeckoTerminal ----

// num is a number GeckoTerminal writes as a string ("0.0019", "0.0") or
// leaves null.
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		*n = 0
		return nil
	}
	*n = num(f)
	return nil
}

func (s *Source) pools(ctx context.Context, mint string) ([]Market, error) {
	var out struct {
		Data []struct {
			Attributes struct {
				Address   string `json:"address"`
				Name      string `json:"name"`
				CreatedAt string `json:"pool_created_at"`
				Rate      num    `json:"base_token_price_quote_token"`
				Price     num    `json:"token_price_usd"`
				Reserve   num    `json:"reserve_in_usd"`
				Volume    struct {
					H24 num `json:"h24"`
				} `json:"volume_usd"`
				Transactions struct {
					H24 struct {
						Buys  int `json:"buys"`
						Sells int `json:"sells"`
					} `json:"h24"`
				} `json:"transactions"`
			} `json:"attributes"`
			Relationships struct {
				Dex struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"dex"`
			} `json:"relationships"`
		} `json:"data"`
		Included []struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"included"`
	}
	if err := s.getJSON(ctx, s.Gecko+"/networks/solana/tokens/"+url.PathEscape(mint)+"/pools?include=dex&page=1", &out); err != nil {
		return nil, err
	}
	dex := map[string]string{}
	for _, in := range out.Included {
		if in.Type == "dex" {
			dex[in.ID] = in.Attributes.Name
		}
	}
	ms := make([]Market, 0, len(out.Data))
	for _, d := range out.Data {
		a := d.Attributes
		if a.Address == "" {
			continue
		}
		m := Market{Address: a.Address, Pair: a.Name, Dex: or(dex[d.Relationships.Dex.Data.ID], d.Relationships.Dex.Data.ID),
			Rate: float64(a.Rate), Price: float64(a.Price), Liquidity: float64(a.Reserve),
			Volume: float64(a.Volume.H24), Trades: a.Transactions.H24.Buys + a.Transactions.H24.Sells}
		if t, err := time.Parse(time.RFC3339, a.CreatedAt); err == nil {
			m.Created = t
		}
		ms = append(ms, m)
	}
	// the busiest first, and among the quiet ones the deepest
	slices.SortStableFunc(ms, func(a, b Market) int {
		if a.Volume != b.Volume {
			if a.Volume > b.Volume {
				return -1
			}
			return 1
		}
		if a.Liquidity > b.Liquidity {
			return -1
		}
		if a.Liquidity < b.Liquidity {
			return 1
		}
		return 0
	})
	return ms, nil
}

func (s *Source) candles(ctx context.Context, mint, pool string, r Range) ([]Candle, error) {
	var out struct {
		Data struct {
			Attributes struct {
				List [][]float64 `json:"ohlcv_list"`
			} `json:"attributes"`
		} `json:"data"`
	}
	u := fmt.Sprintf("%s/networks/solana/pools/%s/ohlcv/%s?aggregate=%d&limit=%d&currency=usd&token=%s",
		s.Gecko, url.PathEscape(pool), r.frame, r.aggregate, r.limit, url.QueryEscape(mint))
	if err := s.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	cs := make([]Candle, 0, len(out.Data.Attributes.List))
	for _, row := range out.Data.Attributes.List {
		// [time, open, high, low, close, volume]
		if len(row) < 6 || row[4] <= 0 || math.IsNaN(row[4]) || math.IsInf(row[4], 0) {
			continue
		}
		cs = append(cs, Candle{T: int64(row[0]), Close: row[4], Volume: row[5]})
	}
	slices.SortFunc(cs, func(a, b Candle) int { return int(a.T - b.T) })
	return cs, nil
}

func (s *Source) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "exe-hub/1 (token page)")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", req.URL.Host, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// ---- the RPC ----

// mintAccount is what the chain holds for a mint.
type mintAccount struct {
	Decimals        int
	Supply          float64 // raw base units
	MintAuthority   string  // "" when nobody may mint
	FreezeAuthority string
	Program         string
}

func (s *Source) mintAccount(ctx context.Context, rpc, mint string) (*mintAccount, error) {
	var out struct {
		Value *struct {
			Owner string `json:"owner"`
			Data  struct {
				Parsed struct {
					Type string `json:"type"`
					Info struct {
						Decimals        int     `json:"decimals"`
						Supply          string  `json:"supply"`
						MintAuthority   *string `json:"mintAuthority"`
						FreezeAuthority *string `json:"freezeAuthority"`
					} `json:"info"`
				} `json:"parsed"`
			} `json:"data"`
		} `json:"value"`
	}
	if err := s.rpc(ctx, rpc, "getAccountInfo", []any{mint, map[string]string{"encoding": "jsonParsed"}}, &out); err != nil {
		return nil, err
	}
	if out.Value == nil || out.Value.Data.Parsed.Type != "mint" {
		return nil, errors.New("not a mint")
	}
	in := out.Value.Data.Parsed.Info
	supply, err := strconv.ParseFloat(in.Supply, 64)
	if err != nil {
		return nil, fmt.Errorf("supply %q", in.Supply)
	}
	m := &mintAccount{Decimals: in.Decimals, Supply: supply, Program: out.Value.Owner}
	if in.MintAuthority != nil {
		m.MintAuthority = *in.MintAuthority
	}
	if in.FreezeAuthority != nil {
		m.FreezeAuthority = *in.FreezeAuthority
	}
	return m, nil
}

// dasAsset is a token's metadata as the DAS API reads it. Known is
// false on an RPC that does not speak DAS: the answer is kept all the
// same, so such an RPC is asked once an hour and not once a reader.
type dasAsset struct {
	Known              bool
	Name, Symbol, Desc string
	Image              string
	Mutable            bool
}

// errNoMethod is the RPC saying it has no such method.
var errNoMethod = errors.New("method not found")

func (s *Source) asset(ctx context.Context, rpc, mint string) (*dasAsset, error) {
	var out struct {
		Mutable *bool `json:"mutable"`
		Content struct {
			Metadata struct {
				Name   string `json:"name"`
				Symbol string `json:"symbol"`
				Desc   string `json:"description"`
			} `json:"metadata"`
			Links struct {
				Image string `json:"image"`
			} `json:"links"`
		} `json:"content"`
	}
	err := s.rpc(ctx, rpc, "getAsset", map[string]string{"id": mint}, &out)
	if errors.Is(err, errNoMethod) {
		return &dasAsset{}, nil
	}
	if err != nil {
		return nil, err
	}
	a := &dasAsset{Known: out.Mutable != nil, Name: strings.TrimSpace(out.Content.Metadata.Name),
		Symbol: strings.TrimSpace(out.Content.Metadata.Symbol), Desc: strings.TrimSpace(out.Content.Metadata.Desc),
		Image: webLink(out.Content.Links.Image)}
	if out.Mutable != nil {
		a.Mutable = *out.Mutable
	}
	return a, nil
}

// rpc calls one JSON-RPC method. The RPC's address holds its key, and
// Go's client errors quote the address they failed on, so what comes
// back from here never does: a failure on the line is named by its
// cause alone.
func (s *Source) rpc(ctx context.Context, rpc, method string, params, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, "POST", rpc, bytes.NewReader(body))
	if err != nil {
		return errors.New("rpc: bad address")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("rpc %s: %v", method, err)
	}
	defer resp.Body.Close()
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env); err != nil {
		return fmt.Errorf("rpc %s: %s", method, resp.Status)
	}
	if env.Error != nil {
		if env.Error.Code == -32601 {
			return errNoMethod
		}
		return fmt.Errorf("rpc %s: %s", method, env.Error.Message)
	}
	// jsonParsed answers wrap the account in {context, value}; DAS ones
	// are the object itself. Both decode into out as it is shaped.
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("rpc %s: %v", method, err)
	}
	return nil
}
