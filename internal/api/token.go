package api

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"exehub/internal/avatar"
	"exehub/internal/card"
	"exehub/internal/config"
	"exehub/internal/token"

	// a token's picture is as often WebP as PNG; the decoder registers
	// itself for every image.Decode in the hub, the avatar's too
	_ "golang.org/x/image/webp"
)

// The token page (PLAN.md, The token page): on a hub whose gate asks for
// a token, GET /token goes to /token/{mint} — the gate's first mint — and
// that page says what the token is: its name and picture, its price and
// the day's change, its holders, supply, liquidity and market value, a
// chart of its price, the pools it trades in, and what this hub's gate
// asks for in it. A hub with an open gate has no such page, and the page
// exists only for the mints its gate names: the hub is not an explorer.
//
// The numbers come from internal/token, which asks Jupiter, GeckoTerminal
// and the gate's RPC and keeps their answers. Nothing on the page is
// fetched by the reader's browser from anyone but the hub — the picture
// too, which the hub fetches once and serves itself.

// webToken is the page as the template draws it. Every figure is set
// here, the same in every language; the words are the template's.
type webToken struct {
	Mint, Head, Tail string // the mint, and its halves either side of a <wbr>
	Name, Symbol     string
	Desc             string
	Icon             bool // the hub has a picture's address to try
	Website, Twitter string
	Explorer, Buy    string
	Flags            []webTokenFlag
	Price            string
	Change           string // "+2.42%", "" when unknown
	Dir              string // "up", "down" or "" for no change known
	Rows             []webTokenRow
	Ranges           []webTokenRange
	Range            string
	Chart            *webChart
	ChartOff         bool // GeckoTerminal has not answered: no chart to draw
	Markets          []webMarket
	Need             string // what the gate asks for in this token: "10,000"
	NeedRaw          bool   // Need is raw base units: nobody has said the decimals
	Worth            string // what that is in dollars now, "" when there is no price
	Others           []webMint
	Stamp            string // when the market's numbers were read, RFC 3339; "" for none
}

// webTokenFlag is one of the chips under the name: Key names its words,
// Good says the token's holders are the safer for it.
type webTokenFlag struct {
	Key  string
	Good bool
}

// webTokenRow is a line of the figures: Key names its label; Value is
// the figure, Href where it leads, Stamp a day for the reader's locale.
type webTokenRow struct {
	Key, Value, Href, Stamp string
}

type webTokenRange struct {
	Label, Href string
	On          bool
}

type webMarket struct {
	Pair, Dex, Href string
	Days            int // the pool's age; 0 when unknown
	Rate            string
	Liquidity       string
	Volume          string
	Trades          int
}

// webChart is the price over the range, drawn in a box 1000 by 1000 that
// the page stretches to its plot: Line and Area are the points, the
// ticks say where the grid's lines and their labels stand as a share of
// the box, and Data is the same points as "seconds,price;…" for the
// script that reads a price off the line under the pointer.
type webChart struct {
	Line, Area string
	Up         bool
	Y, X       []webTick
	Data       string
	Kind       string // how the script sets a time: "time", "day" or "month"
	From, To   int64  // the range, unix seconds
	Lo, Hi     string // the scale's ends: the prices at the plot's bottom and top
	Change     string
	High, Low  string
}

type webTick struct {
	Style template.CSS // "top:25%" or "left:12.5%"
	Label string
	Unix  int64
}

// tokens is the hub's token source, made on first use: the RPC it asks
// is the gate's, read from the config each time, so a reloaded config's
// new key is the one used.
func (s *Server) tokens() *token.Source {
	s.tokenOnce.Do(func() {
		if s.Token == nil {
			s.Token = token.New(func() string {
				if c := s.Cfg.Get(); c.Gate.Mode == "token" {
					return c.Gate.Token.RPCURL
				}
				return ""
			})
		}
	})
	return s.Token
}

// gateMint is the gate's entry for mint: nil on a hub with an open gate
// and for a mint its gate does not name.
func (s *Server) gateMint(mint string) *config.MintReq {
	c := s.Cfg.Get()
	if c.Gate.Mode != "token" {
		return nil
	}
	for i := range c.Gate.Token.Mints {
		if c.Gate.Token.Mints[i].Mint == mint {
			return &c.Gate.Token.Mints[i]
		}
	}
	return nil
}

// handleTokenHome: GET /token, the way to the gate's token — its first,
// when it takes several. The address keeps its query, so ?lang= rides.
func (s *Server) handleTokenHome(w http.ResponseWriter, r *http.Request) {
	c := s.Cfg.Get()
	if c.Gate.Mode != "token" || len(c.Gate.Token.Mints) == 0 {
		s.webError(w, r, http.StatusNotFound, "err.notoken")
		return
	}
	to := "/token/" + url.PathEscape(c.Gate.Token.Mints[0].Mint)
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	// the gate's token is the config's to change: a redirect a browser
	// may not keep
	w.Header().Set("Cache-Control", "no-cache")
	http.Redirect(w, r, to, http.StatusFound)
}

// handleTokenPage: GET /token/{mint}.
func (s *Server) handleTokenPage(w http.ResponseWriter, r *http.Request) {
	mint := r.PathValue("mint")
	req := s.gateMint(mint)
	if req == nil {
		key := "err.nomint"
		if s.Cfg.Get().Gate.Mode != "token" {
			key = "err.notoken"
		}
		s.webError(w, r, http.StatusNotFound, key)
		return
	}
	src := s.tokens()
	in := src.Info(mint)
	rng := token.RangeOf(r.URL.Query().Get("range"))
	candles, at := src.Candles(mint, in.Pool, rng)
	rd := webReadingOf(r)
	t := s.webTokenOf(in, req, rd, rng)
	if at.IsZero() {
		t.ChartOff = true
	} else {
		t.Chart = chartOf(candles, rng, at)
	}
	name := tokenName(in)
	desc := rd.L.T("token.desc", "name", name, "host", r.Host)
	if t.Price != "" {
		desc = rd.L.T("token.desc.price", "name", name, "price", t.Price, "host", r.Host)
	}
	// the page is the same for every reader for as long as the numbers
	// are believed; a shared cache may keep it that long
	w.Header().Set("Cache-Control", "public, max-age=30")
	s.webRender(w, r, http.StatusOK, &webData{
		Page: "token", Title: name + " · " + r.Host, Desc: desc,
		Canonical: true, Lang: rd.Lang, Q: rd.Q, Token: t,
	})
}

// tokenName is what a token is called: its name, else its symbol, else
// its mint cut short.
func tokenName(in *token.Info) string {
	switch {
	case in.Name != "":
		return in.Name
	case in.Symbol != "":
		return in.Symbol
	}
	return shortMint(in.Mint)
}

// shortMint is an address cut to its ends: 9raU…pump.
func shortMint(m string) string {
	if len(m) <= 10 {
		return m
	}
	return m[:4] + "…" + m[len(m)-4:]
}

func (s *Server) webTokenOf(in *token.Info, req *config.MintReq, rd webReading, rng token.Range) *webToken {
	m := webMint{Mint: in.Mint}
	t := &webToken{Mint: in.Mint, Head: m.Head(), Tail: m.Tail(), Name: tokenName(in), Symbol: in.Symbol, Desc: in.Desc,
		Icon: len(in.Icon) > 0, Website: in.Website, Twitter: in.Twitter,
		Explorer: "https://orbmarkets.io/token/" + url.PathEscape(in.Mint), Buy: m.Buy(), Range: rng.Name}
	for _, f := range []struct {
		v        *bool
		yes, no  string
		goodWhen bool
	}{
		{in.MintRevoked, "token.mint.revoked", "token.mint.live", true},
		{in.FreezeRevoked, "token.freeze.revoked", "token.freeze.live", true},
		{in.Immutable, "token.meta.fixed", "token.meta.open", true},
	} {
		if f.v == nil {
			continue
		}
		key := f.no
		if *f.v {
			key = f.yes
		}
		t.Flags = append(t.Flags, webTokenFlag{Key: key, Good: *f.v == f.goodWhen})
	}
	if in.Price > 0 {
		t.Price = usdPrice(in.Price)
	}
	if in.Change != nil {
		t.Change, t.Dir = percent(*in.Change), direction(*in.Change)
	}
	row := func(key, value string) {
		if value != "" {
			t.Rows = append(t.Rows, webTokenRow{Key: key, Value: value})
		}
	}
	if in.Holders > 0 {
		row("token.holders", grouped(float64(in.Holders), 0))
	}
	row("token.volume", usdAmount(in.Volume))
	if in.Supply > 0 {
		row("token.supply", grouped(in.Supply, 2))
	}
	row("token.liquidity", usdAmount(in.Liquidity))
	row("token.mcap", usdAmount(in.MarketCap))
	if in.FDV > 0 && math.Abs(in.FDV-in.MarketCap) > in.MarketCap/200 {
		row("token.fdv", usdAmount(in.FDV))
	}
	row("token.issuer", in.Issuer)
	if !in.Created.IsZero() {
		t.Rows = append(t.Rows, webTokenRow{Key: "token.created", Value: in.Created.UTC().Format("2006-01-02"),
			Stamp: in.Created.UTC().Format(time.RFC3339)})
	}
	if in.Creator != "" {
		t.Rows = append(t.Rows, webTokenRow{Key: "token.creator", Value: shortMint(in.Creator),
			Href: "https://orbmarkets.io/address/" + url.PathEscape(in.Creator)})
	}

	for _, r := range token.Ranges {
		q := url.Values{}
		if r.Name != token.DefaultRange {
			q.Set("range", r.Name)
		}
		if rd.Lang != "" {
			q.Set("lang", rd.Lang)
		}
		href := "/token/" + url.PathEscape(in.Mint)
		if len(q) > 0 {
			href += "?" + q.Encode()
		}
		t.Ranges = append(t.Ranges, webTokenRange{Label: strings.ToUpper(r.Name), Href: href, On: r.Name == rng.Name})
	}

	now := time.Now()
	for _, mk := range in.Markets {
		wm := webMarket{Pair: mk.Pair, Dex: mk.Dex, Href: "https://www.geckoterminal.com/solana/pools/" + url.PathEscape(mk.Address),
			Liquidity: usdAmount(mk.Liquidity), Volume: usdAmount(mk.Volume), Trades: mk.Trades}
		if mk.Rate > 0 {
			wm.Rate = figures(mk.Rate, 4)
		}
		if !mk.Created.IsZero() && now.After(mk.Created) {
			wm.Days = int(now.Sub(mk.Created).Hours() / 24)
		}
		if wm.Liquidity == "" {
			wm.Liquidity = "$0"
		}
		if wm.Volume == "" {
			wm.Volume = "$0"
		}
		t.Markets = append(t.Markets, wm)
	}

	// what the gate asks for, in the token's own units once anyone has
	// said its decimals — the chain or Jupiter here, else the gate's RPC
	dec, known := in.Decimals, in.Supply > 0
	if !known && s.Gate != nil {
		dec, known = s.Gate.Decimals(in.Mint)
	}
	t.Need, t.NeedRaw = req.MinAmount, true
	if known {
		t.Need, t.NeedRaw = humanUnits(req.MinRaw, dec), false
		if in.Price > 0 {
			t.Worth = usdAmount(float64(req.MinRaw) / math.Pow10(dec) * in.Price)
		}
	}
	for _, o := range s.Cfg.Get().Gate.Token.Mints {
		if o.Mint != in.Mint {
			t.Others = append(t.Others, webMint{Mint: o.Mint})
		}
	}
	if !in.At.IsZero() {
		t.Stamp = in.At.UTC().Format(time.RFC3339)
	}
	return t
}

// Short is the mint cut to its ends, for a line that lists several.
func (m webMint) Short() string { return shortMint(m.Mint) }

// chartOf lays the candles out over the range that ends at now. The
// line opens at the price the range opened at — the last candle before
// it, when there is one — and runs to now at the last price, so a token
// that last traded an hour ago draws a flat hour and not a gap. nil
// when the range holds no price at all.
func chartOf(candles []token.Candle, rng token.Range, now time.Time) *webChart {
	to := now.Unix()
	from := to - int64(rng.Span/time.Second)
	step := int64(rng.Step() / time.Second)
	type pt struct {
		t int64
		p float64
	}
	var pts []pt
	var open float64
	for _, c := range candles {
		// a candle's price is its close: the price when its span ended
		end := c.T + step
		switch {
		case end <= from:
			open = c.Close
		case end > to:
			pts = append(pts, pt{to, c.Close})
		default:
			pts = append(pts, pt{end, c.Close})
		}
	}
	if open > 0 {
		pts = append([]pt{{from, open}}, pts...)
	}
	if len(pts) == 0 {
		return nil
	}
	if last := pts[len(pts)-1]; last.t < to {
		pts = append(pts, pt{to, last.p})
	}
	lo, hi := pts[0].p, pts[0].p
	for _, p := range pts {
		lo, hi = math.Min(lo, p.p), math.Max(hi, p.p)
	}
	c := &webChart{High: "$" + figures(hi, 4), Low: "$" + figures(lo, 4), Up: pts[len(pts)-1].p >= pts[0].p,
		Change: percent((pts[len(pts)-1].p/pts[0].p - 1) * 100)}
	// the scale: the prices with a tenth of their spread above and below,
	// or two percent of the price when it never moved
	pad := (hi - lo) / 10
	if pad == 0 {
		pad = hi / 50
	}
	lo, hi = lo-pad, hi+pad
	c.From, c.To = from, to
	c.Lo, c.Hi = strconv.FormatFloat(lo, 'g', 10, 64), strconv.FormatFloat(hi, 'g', 10, 64)
	x := func(t int64) float64 { return float64(t-from) / float64(to-from) * 1000 }
	y := func(p float64) float64 { return (hi - p) / (hi - lo) * 1000 }
	var line, data strings.Builder
	for i, p := range pts {
		if i > 0 {
			line.WriteByte(' ')
			data.WriteByte(';')
		}
		fmt.Fprintf(&line, "%.1f,%.1f", x(p.t), y(p.p))
		fmt.Fprintf(&data, "%d,%s", p.t, strconv.FormatFloat(p.p, 'g', 8, 64))
	}
	c.Line, c.Data = line.String(), data.String()
	c.Area = fmt.Sprintf("%.1f,1000 %s 1000,1000", x(pts[0].t), c.Line)

	ticks, places := niceTicks(lo, hi, 4)
	for _, v := range ticks {
		c.Y = append(c.Y, webTick{Style: template.CSS(fmt.Sprintf("top:%.3f%%", y(v)/10)), Label: "$" + strconv.FormatFloat(v, 'f', places, 64)})
	}
	c.Kind = "time"
	layout := "15:04"
	switch {
	case rng.Span > 60*24*time.Hour:
		c.Kind, layout = "month", "Jan 2006"
	case rng.Span > 24*time.Hour:
		c.Kind, layout = "day", "Jan 2"
	}
	for i := 0; i < 4; i++ {
		at := from + (to-from)*int64(2*i+1)/8
		c.X = append(c.X, webTick{Style: template.CSS(fmt.Sprintf("left:%.1f%%", float64(2*i+1)*12.5)),
			Label: time.Unix(at, 0).UTC().Format(layout), Unix: at})
	}
	return c
}

// niceTicks is up to about n round values inside lo..hi, steps of 1, 2
// or 5 times a power of ten, and the decimal places that tell them
// apart.
func niceTicks(lo, hi float64, n int) (ticks []float64, places int) {
	if !(hi > lo) || n < 1 {
		return nil, 0
	}
	raw := (hi - lo) / float64(n)
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	step := mag * 10
	for _, m := range []float64{1, 2, 5} {
		if m*mag >= raw {
			step = m * mag
			break
		}
	}
	if e := int(math.Floor(math.Log10(step))); e < 0 {
		places = -e
	}
	for v := math.Ceil(lo/step) * step; v <= hi+step*1e-9; v += step {
		ticks = append(ticks, v)
	}
	return ticks, places
}

// ---- figures ----

// grouped is v with places decimals and a comma every three digits.
func grouped(v float64, places int) string {
	s := strconv.FormatFloat(math.Abs(v), 'f', places, 64)
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	if v < 0 {
		b.WriteByte('-')
	}
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if frac != "" {
		b.WriteString("." + frac)
	}
	return b.String()
}

// figures is v to n significant figures, grouped: a rate or a price of
// any size keeps the digits that matter (0.00002279, 0.0145, 1,234.5).
func figures(v float64, n int) string {
	if v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return "0"
	}
	places := n - 1 - int(math.Floor(math.Log10(v)))
	if places < 0 {
		places = 0
	}
	if places > 12 {
		places = 12
	}
	return grouped(v, places)
}

// usdPrice is a price in dollars: cents for a dollar and up, six
// figures under one ($0.00195974).
func usdPrice(v float64) string {
	if v >= 1 {
		return "$" + grouped(v, 2)
	}
	return "$" + figures(v, 6)
}

// usdAmount is a sum in dollars: whole dollars from a thousand up
// ($200,715), cents under ($657.94); "" for none.
func usdAmount(v float64) string {
	switch {
	case !(v > 0):
		return ""
	case v >= 1000:
		return "$" + grouped(v, 0)
	}
	return "$" + grouped(v, 2)
}

// percent is a change with its sign: +2.42%, −0.30%, 0.00%.
func percent(v float64) string {
	s := strconv.FormatFloat(math.Abs(v), 'f', 2, 64)
	switch {
	case s == "0.00" || math.IsNaN(v) || math.IsInf(v, 0):
		return "0.00%"
	case v < 0:
		return "−" + s + "%"
	}
	return "+" + s + "%"
}

func direction(v float64) string {
	switch percent(v)[0] {
	case '+':
		return "up"
	case '0':
		return ""
	}
	return "down"
}

// ---- the picture ----

// tokenIcon is a token's picture as the hub serves it.
type tokenIcon struct {
	data []byte
	mime string
}

// handleTokenIcon: GET /token/{mint}/icon. The picture's address is
// whatever the token's listing says, a stranger's word, so it is fetched
// by the link cards' guarded client (no private address, capped, a
// picture by its bytes and not its name), cut to the avatar's square,
// and kept in memory for an hour — the browser's own copy stands a day.
// A listing with no picture that can be read is a 404 for that hour.
func (s *Server) handleTokenIcon(w http.ResponseWriter, r *http.Request) {
	mint := r.PathValue("mint")
	if s.gateMint(mint) == nil {
		http.NotFound(w, r)
		return
	}
	v, _ := s.mem.Fetch("token-icon:"+mint, time.Hour, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		for _, u := range s.tokens().Info(mint).Icon {
			raw, err := s.tokenPicture(ctx, u)
			if err != nil {
				continue
			}
			if img, err := avatar.Process(raw); err == nil {
				return &tokenIcon{data: img, mime: http.DetectContentType(img)}, nil
			}
		}
		return (*tokenIcon)(nil), nil
	})
	ic, _ := v.(*tokenIcon)
	if ic == nil {
		w.Header().Set("Cache-Control", "no-cache")
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ic.mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(ic.data)
}

// tokenPicture reads the picture at u. Most tokens keep theirs on IPFS
// and name it by a public gateway, and a public gateway turns a server
// away sooner than a browser (ipfs.io answered this hub 429): a link
// that names a CID is read from the hub's own IPFS node first, which
// finds the bytes wherever they are, and from its gateway only when
// the node cannot. The cap is the embed's either way.
func (s *Server) tokenPicture(ctx context.Context, u string) ([]byte, error) {
	if cid := card.IPFSCID(u); cid != "" && s.IPFS != nil {
		// what follows the CID in a path gateway's address is a path
		// inside it, which the node resolves as the gateway would
		arg := cid
		if pu, err := url.Parse(u); err == nil {
			if rest, ok := strings.CutPrefix(pu.Path, "/ipfs/"+cid); ok {
				arg += strings.TrimRight(rest, "/")
			} else {
				arg += strings.TrimRight(pu.Path, "/")
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		rc, err := s.IPFS.Cat(cctx, arg, 0, card.MaxImage+1)
		if err == nil {
			raw, err := io.ReadAll(io.LimitReader(rc, card.MaxImage+1))
			rc.Close()
			if err == nil && len(raw) > 0 && len(raw) <= card.MaxImage {
				cancel()
				return raw, nil
			}
		}
		cancel()
	}
	raw, _, err := s.tokenFetcher().FetchImage(ctx, u)
	return raw, err
}

func (s *Server) tokenFetcher() *card.Fetcher {
	s.fetchOnce.Do(func() {
		if s.TokenFetch == nil {
			s.TokenFetch = card.NewFetcher()
		}
	})
	return s.TokenFetch
}
