package api

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"exehub/internal/card"
	"exehub/internal/envelope"
	"exehub/internal/lang"
	"exehub/internal/preview"
	"exehub/internal/store"
)

// Public pages — the hub's face for a browser, beside the JSON API:
// GET / (the feed and how to join), /p/{id} (a thread), /u/{id} (a
// profile), /search?q= (the posts holding some words). Server-rendered from the same store queries the API uses,
// Mac OS 9 chrome, two small inline scripts (the picture viewer, and
// the live feed on the home page's first page), and a reader only:
// writes stay with signed clients, so the pages carry no session,
// cookie or state-changing form (the search form is a GET, a read
// like any link) and have no CSRF surface. Post text goes through one
// escaping pipeline that mirrors the Hub app's — URLs become links,
// `code` becomes code, everything else stays literal text, so a post
// can never smuggle markup in. The join block on the home page is
// rendered from the live config like skill.md: an open hub says so, a
// token-gated one names the holding, so a SIGHUP gate change shows at
// once. A browser whose language is Chinese reads the block in Chinese
// (Accept-Language, or ?lang=, see webChinese); the rest of the page
// stays as it is, the posts in whatever language they were written.

//go:embed web.html
var webHTML string

// The pages' icons, all drawn from the Hub app's own 32px pixel art
// (which has a 4px transparent margin around a 24px core), scaled
// nearest-neighbour so the pixels stay pixels: the shortcut icon at 1x
// (a 32 and a halved 16 in one .ico); the touch icon at 5x on the
// desktop's lavender with a 10px margin, like exe's maskable icon; the
// manifest's icons at 6x and 16x, transparent, for a launcher or an
// install dialog to show as they are, and a maskable one at 12x on the
// lavender out to the edge — the 288px core sits inside the circle
// Android's masks are guaranteed to keep (80% of 512, so a 289px
// square at most).
//
//go:embed favicon.ico
var webFavicon []byte

//go:embed apple-touch-icon.png
var webTouchIcon []byte

//go:embed icon-192.png
var webIcon192 []byte

//go:embed icon-512.png
var webIcon512 []byte

//go:embed icon-maskable-512.png
var webIconMaskable []byte

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(webFavicon)
}

func servePNG(png []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(png)
	}
}

// webDesc is what the hub says it is: the home page's description, the
// manifest's.
const webDesc = "an exe-hub: a small public feed where a key is an account"

// webManifest is the web app manifest, what makes the pages
// installable — a home-screen shortcut on a phone, an app window on a
// desktop — in a window of their own (display standalone; the page then
// hides the join block, see web.html). Rendered per request because a
// hub's name is its host: there is no display name for a hub, and every
// hub is someone's own. No service worker goes with it: Chrome no
// longer asks for one to install, and the pages are live views of the
// hub, so nothing here should ever come from a cache.
type webManifest struct {
	Name       string            `json:"name"`
	ShortName  string            `json:"short_name"`
	Desc       string            `json:"description"`
	ID         string            `json:"id"`
	Start      string            `json:"start_url"`
	Scope      string            `json:"scope"`
	Display    string            `json:"display"`
	Background string            `json:"background_color"`
	Theme      string            `json:"theme_color"`
	Icons      []webManifestIcon `json:"icons"`
}

type webManifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose,omitempty"`
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	json.NewEncoder(w).Encode(webManifest{
		Name: r.Host, ShortName: r.Host, Desc: webDesc,
		ID: "/", Start: "/", Scope: "/", Display: "standalone",
		Background: "#cccccc", Theme: "#cccccc", // the desk, as the pages' theme-color
		Icons: []webManifestIcon{
			{Src: "/icon-192.png", Sizes: "192x192", Type: "image/png"},
			{Src: "/icon-512.png", Sizes: "512x512", Type: "image/png"},
			{Src: "/icon-maskable-512.png", Sizes: "512x512", Type: "image/png", Purpose: "maskable"},
		},
	})
}

var webTmpl = template.Must(template.Must(template.New("web").Funcs(template.FuncMap{
	"mul": func(a, b int) int { return a * b },
	"pct": func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
}).Parse(webHTML)).Parse(statsHTML))

// webPage is the page size of the feed and profile pages; the next page
// is a plain link carrying the last post's id as the keyset cursor.
const webPage = 30

// webPost is a feed post prepared for the template: text rendered to
// safe HTML, the time as a UTC stamp (When, what the page shows without
// script) beside its RFC 3339 form (Stamp, the <time> element's
// datetime, which the page's script turns into the reader's local time),
// embeds split into pictures shown inline and files offered as links.
type webPost struct {
	store.FeedPost
	HTML   template.HTML
	When   string
	Stamp  string
	Images []envelope.Embed
	Videos []webMedia // video, a looping animation among them, drawn as players
	Sounds []webMedia
	Files  []envelope.Embed
	// HTML embeds from an admin key open in a sandboxed window of their own
	Pages []envelope.Embed
	// on a thread page: the reply this one answers is on the same page —
	// a nested reply links to it in place, by the name it was posted under
	InThread   bool
	ParentName string
	// on a profile page: the post this reply answers, quoted as the head
	// of its card. A run of replies under one parent shares one head —
	// the later ones carry QuoteRun and attach to the card above.
	Quote    *webQuote
	QuoteRun bool
	// a root with replies: the thread's newest reply, on the foot line —
	// the feed says what was said last instead of burying it
	Latest *webLatest
	// the post in the reader's language, when it is written in another
	// and has been put into theirs (see PLAN.md, Translations)
	Tr *webTr
	// "?lang=…" when the request said its language: the post's own links
	// carry it, so a look at the other language lasts past one click
	Q string
}

// webTr is a post's translation as the page shows it: standing where
// the text does, the post as written hidden under it, and a quiet line
// below that swaps the two.
type webTr struct {
	HTML template.HTML // rendered like any post's text
	Lang string        // the language it is in, its lang attribute
	Note string        // "Translated from English", in the reader's language
	// the control's words as the page opens and once pressed
	Show, Back string
	// the page opens on the post as written, the translation one press
	// away: a search found the post by words only its original has
	Orig bool
}

// webLatest is the newest reply in a post's thread, as the foot shows
// it: the name, a line of the text, and the anchor to land on it.
type webLatest struct {
	ID   string
	Name string
	Text string
}

// webQuote is the quoted parent above a reply on a profile page: the
// author and a line of the text, the whole quote opening the thread.
type webQuote struct {
	ID   string
	Name string
	Text string // an excerpt; "" for a post that is all pictures
}

// webMedia is a video or sound embed for the template: the player box's
// final size, set before the file loads (no jump when it does), and the
// length to print.
type webMedia struct {
	envelope.Embed
	Box    template.CSS
	Length string
}

// webMediaMaxH is the tallest a video stands in a post, as pictures do.
const webMediaMaxH = 420

func newWebMedia(e envelope.Embed) webMedia {
	m := webMedia{Embed: e}
	w, h := e.Width, e.Height
	if w <= 0 || h <= 0 {
		// an upload that did not say its shape: a 16:9 box as wide as the post
		m.Box = template.CSS("width: 100%; aspect-ratio: 16 / 9")
	} else {
		// whole pixels both ways, so the border never lands between two
		bw := max(1, min(w, w*webMediaMaxH/h))
		bh := max(1, (bw*h+w/2)/w)
		m.Box = template.CSS(fmt.Sprintf("width: %dpx; aspect-ratio: %d / %d", bw, bw, bh))
	}
	if e.Duration > 0 {
		sec := int(e.Duration + 0.5)
		m.Length = fmt.Sprintf("%d:%02d", sec/60, sec%60)
	}
	return m
}

// webJoin is the join block: where this hub is, what its gate asks for.
type webJoin struct {
	Base     string
	HubID    string
	Token    bool
	Mints    []webMint
	Cooldown int
	Chinese  bool // the browser's language is Chinese: the block reads in Chinese
}

type webMint struct {
	Amount string `json:"amount"` // "10,000", or the raw base units "10000000000" before the RPC has answered
	Raw    bool   `json:"raw"`    // Amount (and Held) are raw base units: the RPC has not told the mint's decimals yet
	Mint   string `json:"mint"`
	Held   string `json:"held,omitempty"` // /v1/gate only: what the key holds, in the same units; absent when unread
}

// wSOL is wrapped SOL's mint: what Jupiter's swap page sells when the
// join block's mint link says buy with SOL.
const wSOL = "So11111111111111111111111111111111111111112"

// Buy is where the join block's mint links: Jupiter's swap page set to
// sell SOL for this mint, the holding the gate asks for one click from
// where it can be bought. The query form, not the older
// /swap/SOL-<mint> path — jup.ag rewrites that one to SOL for USDC.
func (m webMint) Buy() string {
	return "https://jup.ag/swap?sell=" + wSOL + "&buy=" + url.QueryEscape(m.Mint)
}

// webCompose is the strip that posts from a Solana wallet (see PLAN.md,
// Posting from a wallet): a new post on the home page, a reply to the
// post a thread page shows.
type webCompose struct {
	ReplyTo string
}

// webData is the template's world for one page.
type webData struct {
	Base, Host, Path string
	Title, Desc      string
	// the link preview (see PLAN.md, Public pages — Link previews): the
	// OpenGraph picture, its size when known, what it shows, and the
	// Twitter card kind — "summary_large_image" for a 1.91:1 picture,
	// "summary" for a small square one
	Image          string
	ImageW, ImageH int
	ImageAlt       string
	CardKind       string
	Canonical      bool   // the page has one address: home, a thread, a profile
	Published      string // a thread: the post's time, RFC 3339
	Page           string // "home" | "thread" | "profile" | "error"
	Posts          []webPost
	Prev, Next     string // keyset cursors for the neighbouring pages, "" at either end
	Live           bool   // the home page's first page and every thread: ships the live script
	Lang           string // the request's ?lang=, when it said one: carried by the pager links and the find strip
	Q              string // the same as "?lang=…", for a link with no query of its own
	PushKey        string // the home page on a hub that pushes: the VAPID public key for the Notify box
	Query          string // the search page's words, and what the find strip's field holds
	Join           *webJoin
	Compose        *webCompose // the wallet strip: the home page's first page and every thread
	Post           *webPost
	Replies        []webPost
	Profile        *store.Profile
	Since          string // the profile's first day as a UTC date, and SinceStamp its RFC 3339 form for the <time> element
	SinceStamp     string
	Members, Count int
	Message        string
	// the pages' analytics: the feed's pager links "N online" to /stats
	// on a hub that counts, and /stats itself carries its page
	StatsOn   bool
	Online    int
	StatsPage *statsPage
}

// webURL is the Hub app's URL matcher; it lives in the card package now,
// so the linkifier and the link-card deriver can never disagree on what
// a post's link is.
var webURL = card.URL

// webCode is an inline `code` span: no newlines, no nesting.
var webCode = card.Code

// webHeading is a heading line: one to three # and a space, then words
// (Markdown's ATX form, three levels — with code spans, the
// [words](url) link, card.Link, and the pipe table, card.TableAt, all
// of Markdown a post takes).
// webHeadingMark is the marker alone, for an excerpt to drop.
var (
	webHeading     = regexp.MustCompile(`^(#{1,3}) +(\S.*?) *$`)
	webHeadingMark = regexp.MustCompile(`(?m)^#{1,3} +`)
)

// renderText turns a post's text into HTML the page may embed: every
// character escaped, a heading line set as an h1–h3 whose words take
// the inline pipeline like any others — the block breaks the line
// itself, so the line break before it and the one that ends it go with
// it, and so does one blank line on either side: the heading's own
// margins space it, as in Markdown, and "## Title" reads the same with
// or without a blank line beside it; the heading that opens a post is
// marked .first, for no room above it — a table (card.TableAt, GFM's
// pipe table) set as a block the same way, inside a .tbl box that
// scrolls sideways when the table is wider than the post, its cells
// through the inline pipeline and its columns aligned by class; .first
// when it opens the post and .last when it ends it, for no room on
// that side — a list (card.ListAt: "- " or "* " items, or numbered
// ones) a block again, a ul or an ol of its items, each through the
// inline pipeline, .first and .last as a table has them — and
// elsewhere [words](url) set as a link on its words,
// URLs outside code spans wrapped in anchors that open in a new tab,
// code spans set in <code>, newlines kept as line breaks.
func renderText(text string) template.HTML {
	var b strings.Builder
	lines := strings.Split(text, "\n")
	plain := ""
	// block closes the words before a block: the break before it and one
	// blank line above it go with it
	block := func() {
		plain = strings.TrimSuffix(plain, "\n")
		plain = strings.TrimSuffix(plain, "\n")
		if plain != "" {
			writeInline(&b, plain)
			plain = ""
		}
	}
	for i := 0; i < len(lines); i++ {
		if m := webHeading.FindStringSubmatch(lines[i]); m != nil {
			block()
			tag := "h" + strconv.Itoa(len(m[1]))
			if b.Len() == 0 {
				b.WriteString("<" + tag + ` class="first">`)
			} else {
				b.WriteString("<" + tag + ">")
			}
			writeInline(&b, m[2])
			b.WriteString("</" + tag + ">\n")
		} else if t, n := card.TableAt(lines, i); t != nil {
			block()
			i += n - 1
			writeTable(&b, t, b.Len() == 0, strings.TrimSpace(strings.Join(lines[i+1:], "")) == "")
		} else if l, n := card.ListAt(lines, i); l != nil {
			block()
			i += n - 1
			writeList(&b, l, b.Len() == 0, strings.TrimSpace(strings.Join(lines[i+1:], "")) == "")
		} else {
			plain += lines[i]
			if i < len(lines)-1 {
				plain += "\n"
			}
			continue
		}
		if i+1 < len(lines) && lines[i+1] == "" {
			i++ // one blank line below the block
		}
	}
	if plain != "" {
		writeInline(&b, plain)
	}
	return template.HTML(b.String())
}

// writeTable sets a table: the box that scrolls, a head row, the rows
// under it; a column's alignment is a class on each of its cells.
func writeTable(b *strings.Builder, t *card.Table, first, last bool) {
	class := "tbl"
	if first {
		class += " first"
	}
	if last {
		class += " last"
	}
	b.WriteString(`<div class="` + class + `"><table><thead>`)
	row := func(tag string, cells []string) {
		b.WriteString("<tr>")
		for k, c := range cells {
			if t.Align[k] != "" {
				b.WriteString("<" + tag + ` class="` + t.Align[k] + `">`)
			} else {
				b.WriteString("<" + tag + ">")
			}
			writeInline(b, c)
			b.WriteString("</" + tag + ">")
		}
		b.WriteString("</tr>")
	}
	row("th", t.Head)
	b.WriteString("</thead>")
	if len(t.Rows) > 0 {
		b.WriteString("<tbody>")
		for _, r := range t.Rows {
			row("td", r)
		}
		b.WriteString("</tbody>")
	}
	b.WriteString("</table></div>\n")
}

// writeList sets a list: a ul, or an ol that counts on from the list's
// first number — the page draws its own markers (web.html, a CSS
// counter begun at --n, one under the first number), so that number
// rides in the style, and the class w2 or w3 says how many digits the
// widest marker has, for the room it needs left of the words.
func writeList(b *strings.Builder, l *card.List, first, last bool) {
	tag, class, style := "ul", "", ""
	if l.Ordered {
		tag = "ol"
		if n := len(strconv.Itoa(l.Start + len(l.Items) - 1)); n > 1 {
			class = " w" + strconv.Itoa(n)
		}
		if l.Start != 1 {
			style = ` style="--n:` + strconv.Itoa(l.Start-1) + `"`
		}
	}
	if first {
		class += " first"
	}
	if last {
		class += " last"
	}
	if class != "" {
		class = ` class="` + class[1:] + `"`
	}
	b.WriteString("<" + tag + class + style + ">")
	for _, item := range l.Items {
		b.WriteString("<li>")
		writeInline(b, item)
		b.WriteString("</li>")
	}
	b.WriteString("</" + tag + ">\n")
}

// writeInline sets a run of words: **bold** first (card.Bolds), the
// outermost layer, so a bold stretch may hold links, URLs and code —
// **[words](url)** is a bold link — then the links and what lies
// between them. A stretch that a code span or a link cuts into is no
// bold: its asterisks stay.
func writeInline(b *strings.Builder, text string) {
	var links [][]int
	for _, m := range webLinks(text) {
		links = append(links, m[:2])
	}
	writeBold(b, text, links, writeLinks)
}

// writeBold sets the bold stretches of text in <strong> and hands the
// words inside and between them to inner.
func writeBold(b *strings.Builder, text string, links [][]int, inner func(*strings.Builder, string)) {
	last := 0
	for _, m := range card.Bolds(text, webCode.FindAllStringIndex(text, -1), links) {
		inner(b, text[last:m[0]])
		b.WriteString("<strong>")
		inner(b, text[m[2]:m[3]])
		b.WriteString("</strong>")
		last = m[1]
	}
	inner(b, text[last:])
}

// writeLinks sets Markdown links, then what lies between them. A link's
// words take bold and code spans and nothing else — a URL among them is
// words, never a link inside a link — and its address shows on hover,
// since the words no longer say where it goes.
func writeLinks(b *strings.Builder, text string) {
	words := func(b *strings.Builder, s string) { writeCoded(b, s, false) }
	last := 0
	for _, m := range webLinks(text) {
		writeCoded(b, text[last:m[0]], true)
		u := html.EscapeString(text[m[4]:m[5]])
		b.WriteString(`<a href="` + u + `" title="` + u + `" target="_blank" rel="noopener nofollow">`)
		writeBold(b, text[m[2]:m[3]], nil, words)
		b.WriteString(`</a>`)
		last = m[1]
	}
	writeCoded(b, text[last:], true)
}

// webLinks are the Markdown links of a run of words, as card.Link's
// submatch indexes, less those a code span claims: a code span binds
// tighter, as in Markdown, so `[words](url)` inside backticks stays
// literal, and only a span that sits wholly inside a link's words
// leaves the link standing.
func webLinks(text string) [][]int {
	codes := webCode.FindAllStringIndex(text, -1)
	var out [][]int
	for _, m := range card.Link.FindAllStringSubmatchIndex(text, -1) {
		free := true
		for _, c := range codes {
			if c[0] < m[1] && c[1] > m[0] && !(c[0] >= m[2] && c[1] <= m[3]) {
				free = false
				break
			}
		}
		if free {
			out = append(out, m)
		}
	}
	return out
}

// writeCoded sets code spans, and the stretches between them plain or,
// with links, with their bare URLs linked.
func writeCoded(b *strings.Builder, text string, links bool) {
	plain := writePlain
	if links {
		plain = writeLinked
	}
	last := 0
	for _, m := range webCode.FindAllStringSubmatchIndex(text, -1) {
		plain(b, text[last:m[0]])
		b.WriteString("<code>" + html.EscapeString(text[m[2]:m[3]]) + "</code>")
		last = m[1]
	}
	plain(b, text[last:])
}

func writeLinked(b *strings.Builder, s string) {
	last := 0
	for _, m := range webURL.FindAllStringIndex(s, -1) {
		writePlain(b, s[last:m[0]])
		u := html.EscapeString(s[m[0]:m[1]])
		b.WriteString(`<a href="` + u + `" target="_blank" rel="noopener nofollow">` + u + `</a>`)
		last = m[1]
	}
	writePlain(b, s[last:])
}

func writePlain(b *strings.Builder, s string) {
	b.WriteString(strings.ReplaceAll(html.EscapeString(s), "\n", "<br>\n"))
}

// webTag is one tag of renderText's own HTML. Every attribute value in
// it is escaped, so no ">" sits inside one and a tag ends at the first.
var webTag = regexp.MustCompile(`<[^>]*>`)

// markHits lays the search's marks over a post's rendered text: every
// stretch that holds one of the query's words goes into a <mark>, the
// yellow the Hub app's Find puts on its results. It matches as the
// search itself does (store.searchWhere: literal substrings, SQLite's
// LIKE folding ASCII case and no other) — but over the words as they
// show, one text node at a time: tags and their attributes pass through
// untouched, so a word found only in an address ([words](url)) marks
// nothing, and a node's text is unescaped before it is searched and
// escaped again as it is written, so "amp" finds "camp" and never the
// "&amp;" beside it. Stretches that touch or overlap join into one
// mark. The Hub app's markHits does the same over its DOM, and
// card/testdata/marks.json holds the cases both are run against.
func markHits(page template.HTML, q string) template.HTML {
	var words []string
	for _, w := range strings.Fields(foldASCII(q)) {
		if !slices.Contains(words, w) {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return page
	}
	src := string(page)
	var b strings.Builder
	last := 0
	for _, m := range webTag.FindAllStringIndex(src, -1) {
		writeMarked(&b, src[last:m[0]], words)
		b.WriteString(src[m[0]:m[1]])
		last = m[1]
	}
	writeMarked(&b, src[last:], words)
	return template.HTML(b.String())
}

// writeMarked writes one text node, escaped as it came, with its found
// stretches marked.
func writeMarked(b *strings.Builder, escaped string, words []string) {
	if escaped == "" {
		return
	}
	text := html.UnescapeString(escaped)
	low := foldASCII(text) // byte for byte as long as text: indexes hold
	var runs [][2]int
	for _, w := range words {
		for at := 0; ; at++ {
			i := strings.Index(low[at:], w)
			if i < 0 {
				break
			}
			at += i
			runs = append(runs, [2]int{at, at + len(w)})
		}
	}
	if len(runs) == 0 {
		b.WriteString(escaped)
		return
	}
	slices.SortFunc(runs, func(x, y [2]int) int { return cmp.Or(x[0]-y[0], x[1]-y[1]) })
	at, from, to := 0, runs[0][0], runs[0][1]
	flush := func() {
		b.WriteString(html.EscapeString(text[at:from]))
		b.WriteString("<mark>" + html.EscapeString(text[from:to]) + "</mark>")
		at = to
	}
	for _, r := range runs[1:] {
		if r[0] <= to {
			to = max(to, r[1])
		} else {
			flush()
			from, to = r[0], r[1]
		}
	}
	flush()
	b.WriteString(html.EscapeString(text[at:]))
}

// foldASCII lowers A–Z and nothing else: the case SQLite's LIKE folds.
func foldASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// webWhen formats a post's own timestamp (kept for display, as PLAN.md
// says; ordering uses receive time) as a fixed UTC stamp, and webStamp
// as RFC 3339 for the <time> element's datetime.
func webWhen(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC")
}

func webStamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func (s *Server) webPosts(rd webReading, posts []store.FeedPost) []webPost {
	ids := make([]string, 0, len(posts))
	for _, p := range posts {
		ids = append(ids, p.ID)
		if p.LastReply != nil {
			ids = append(ids, p.LastReply.ID)
		}
	}
	trs := s.webTranslations(rd, ids)
	out := make([]webPost, len(posts))
	for i, p := range posts {
		out[i] = webPost{FeedPost: p, HTML: renderText(p.Text), When: webWhen(p.TS), Stamp: webStamp(p.TS), Q: rd.Q}
		if t, ok := trs[p.ID]; ok && p.Text != "" {
			out[i].Tr = rd.tr(t)
		}
		if p.LastReply != nil && p.ReplyTo == "" {
			name := p.LastReply.AuthorName
			if name == "" {
				name = p.LastReply.Author
			}
			said := p.LastReply.Text
			if t, ok := trs[p.LastReply.ID]; ok {
				said = t.Text
			}
			out[i].Latest = &webLatest{ID: p.LastReply.ID, Name: name, Text: excerpt(said, 90)}
		}
		for _, e := range p.Embeds {
			switch {
			case strings.HasPrefix(e.MIME, "image/"):
				out[i].Images = append(out[i].Images, e)
			case strings.HasPrefix(e.MIME, "video/"):
				out[i].Videos = append(out[i].Videos, newWebMedia(e))
			case strings.HasPrefix(e.MIME, "audio/"):
				out[i].Sounds = append(out[i].Sounds, newWebMedia(e))
			case slices.Contains(p.PageCIDs, e.CID):
				// a page: read in a sandboxed window (see PLAN, Pages);
				// the store marks an admin's HTML, from any other key the
				// same file stays a download
				out[i].Pages = append(out[i].Pages, e)
			default:
				out[i].Files = append(out[i].Files, e)
			}
		}
	}
	return out
}

// webQuoted dresses a profile page's replies with the post each one
// answers, quoted as the head of its card, so a reply reads as an
// exchange rather than half a conversation. A run of replies under one
// parent gets one head — the page never repeats it — and a parent this
// hub does not hold leaves the plain "in reply to" link as it was.
func (s *Server) webQuoted(rd webReading, posts []webPost) []webPost {
	parents := map[string]*store.FeedPost{}
	var ids []string
	for i := range posts {
		id := posts[i].ReplyTo
		if _, seen := parents[id]; id == "" || seen {
			continue
		}
		parents[id], _ = s.St.Post(id) // nil on any error: the plain link stands
		ids = append(ids, id)
	}
	trs := s.webTranslations(rd, ids) // the quote reads in the reader's language, like the reply under it
	for i := range posts {
		id := posts[i].ReplyTo
		p := parents[id]
		if p == nil {
			continue
		}
		if i > 0 && posts[i-1].ReplyTo == id {
			posts[i].QuoteRun = true
			continue
		}
		said := p.Text
		if t, ok := trs[id]; ok {
			said = t.Text
		}
		posts[i].Quote = &webQuote{ID: p.ID, Name: authorLabel(*p), Text: excerpt(said, 140)}
	}
	return posts
}

// webPager is one page of a keyset-paged list and its neighbours: Prev
// (newer) and Next (older) carry the cursor for that direction, and are
// empty at the newest and oldest ends, so the buttons only show when a
// page exists. Home asks for a redirect to the list's first page.
type webPager struct {
	Posts      []store.FeedPost
	Prev, Next string
	Home       bool
}

// webPageOf resolves ?before= (older than) or ?after= (newer than) into a
// page, fetching one row past it to learn whether a neighbour exists.
// The newer direction reads oldest-first from the cursor, so the page is
// the posts nearest to it; a newer page with nothing newer beyond it —
// full or short — is the list's own first page, and redirects there
// rather than rendering the same posts under a cursor: the first page
// is the one that stays live, and only its bare URL does (Prev from the
// second page lands on it).
func webPageOf(q url.Values,
	older func(before string, n int) ([]store.FeedPost, error),
	newer func(after string, n int) ([]store.FeedPost, error)) (webPager, error) {
	var pg webPager
	if after := q.Get("after"); after != "" {
		asc, err := newer(after, webPage+1)
		if err != nil {
			return pg, err
		}
		if len(asc) <= webPage {
			pg.Home = true
			return pg, nil
		}
		asc = asc[:webPage]
		for i := len(asc) - 1; i >= 0; i-- {
			pg.Posts = append(pg.Posts, asc[i])
		}
		pg.Prev = pg.Posts[0].ID
		pg.Next = pg.Posts[len(pg.Posts)-1].ID // `after` itself lies beyond
		return pg, nil
	}
	before := q.Get("before")
	posts, err := older(before, webPage+1)
	if err != nil {
		return pg, err
	}
	if len(posts) > webPage {
		posts = posts[:webPage]
		pg.Next = posts[len(posts)-1].ID
	}
	pg.Posts = posts
	if before != "" {
		if len(posts) > 0 {
			pg.Prev = posts[0].ID
		} else {
			pg.Prev = before
		}
	}
	return pg, nil
}

// webBase is how this hub is reached from outside: the request's Host,
// https when TLS terminated here or at a proxy in front (Cloudflare and
// exe's reverse proxy both set X-Forwarded-Proto).
func webBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) webJoinBlock(r *http.Request) *webJoin {
	c := s.Cfg.Get()
	j := &webJoin{Base: webBase(r), Cooldown: c.CooldownSec(), Chinese: webChinese(r)}
	if s.Hub != nil {
		j.HubID = s.Hub.ID
	}
	if c.Gate.Mode == "token" {
		j.Token = true
		for _, m := range c.Gate.Token.Mints {
			mint := webMint{Amount: m.MinAmount, Raw: true, Mint: m.Mint}
			if s.Gate != nil {
				if dec, known := s.Gate.Decimals(m.Mint); known {
					mint.Amount, mint.Raw = humanUnits(m.MinRaw, dec), false
				}
			}
			j.Mints = append(j.Mints, mint)
		}
	}
	return j
}

// webLang is what the request asks the join block to read in: ?lang=zh
// the Chinese, ?lang=en the English — a look at the other one from a
// browser of any language, and a link that shows it — "" to let the
// browser's own language decide (webChinese). Anything else is "".
func webLang(r *http.Request) string {
	l := strings.ToLower(r.URL.Query().Get("lang"))
	switch {
	case l == "zh" || strings.HasPrefix(l, "zh-"):
		return "zh"
	case l == "en" || strings.HasPrefix(l, "en-"):
		return "en"
	}
	return ""
}

// webChinese reports whether the join block reads in Chinese
// (Simplified; a Traditional reader gets the same text): ?lang= when
// the request says, else the browser's language — the Accept-Language
// tag with the highest q, the first one the browser lists, zh, zh-CN,
// zh-TW, zh-Hant-HK alike — not Chinese anywhere in the list: a
// browser whose first language is English with Chinese further down
// reads the English. Decided on the server, so the page stands without
// script and never flashes; the home page says Vary: Accept-Language.
func webChinese(r *http.Request) bool {
	if l := webLang(r); l != "" {
		return l == "zh"
	}
	best := webBrowserLang(r)
	return best == "zh" || strings.HasPrefix(best, "zh-")
}

// webBrowserLang is the browser's first language, lower-cased: the
// Accept-Language tag with the highest q, the earlier one on a tie, as
// the browser ordered them; "" when the request names none.
func webBrowserLang(r *http.Request) string {
	best, bestQ := "", 0.0
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		if q > bestQ { // a tie keeps the earlier tag, as the browser ordered them
			best, bestQ = strings.ToLower(tag), q
		}
	}
	return best
}

// webReading is how one request reads the posts (see PLAN.md,
// Translations): whose language they are shown in.
type webReading struct {
	// Reader is the reader's own language in the form the hub keeps a
	// post's (lang.Normal: en, ja, zh-Hans, zh-Hant), "" when the request
	// names none — a crawler's — and "orig" when it asks for every post
	// as written.
	Reader string
	// Target is the language they are given translations in: zh-Hans for
	// a Chinese reader, en, the hub's second language, for every other,
	// "" for none.
	Target string
	// Lang is the request's ?lang= when it said one the pages take, and Q
	// the same as "?lang=…": the page's own links carry it.
	Lang, Q string
}

// webReadingOf reads the request the way webChinese does: ?lang= when
// it says — zh, en, a fuller tag, or orig — else the browser's first
// language. Decided on the server, so a page never flashes from one
// language to another and stands without script; every page with posts
// says Vary: Accept-Language.
func webReadingOf(r *http.Request) webReading {
	var rd webReading
	tag := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("lang")))
	if tag != "" {
		if rd.Reader = webReaderTag(tag); rd.Reader != "" {
			rd.Lang, rd.Q = tag, "?lang="+url.QueryEscape(tag)
		}
	}
	if rd.Reader == "" {
		rd.Reader = webReaderTag(webBrowserLang(r))
	}
	switch {
	case rd.Reader == "" || rd.Reader == "orig":
	case strings.HasPrefix(rd.Reader, "zh-"):
		rd.Target = "zh-Hans"
	default:
		rd.Target = "en"
	}
	return rd
}

// webReaderTag is a language as a request names it, in the hub's form:
// a bare zh reads Simplified, as the join block has it.
func webReaderTag(tag string) string {
	switch tag {
	case "orig":
		return "orig"
	case "zh":
		return "zh-Hans"
	}
	norm, _ := lang.Normal(tag)
	if norm == lang.Unknown || norm == lang.NoWords {
		return ""
	}
	return norm
}

// shows says a post written in from is shown to this reader in their
// Target: it is in neither their own language nor the one they read.
func (rd webReading) shows(from string) bool {
	return rd.Target != "" && from != rd.Reader && from != rd.Target
}

// tr dresses a translation for the page, the line under it in the
// reader's language.
func (rd webReading) tr(t store.Translation) *webTr {
	tr := &webTr{HTML: renderText(t.Text), Lang: rd.Target}
	// the language alone: its script tells only a Chinese reader
	// something, Traditional from the Simplified they are reading
	from, _, _ := strings.Cut(t.From, "-")
	if rd.Target == "zh-Hans" {
		if from == "zh" {
			from = t.From
		}
		tr.Note, tr.Show, tr.Back = "译自"+lang.Chinese(from), "显示原文", "显示译文"
	} else {
		tr.Note, tr.Show, tr.Back = "Translated from "+lang.English(from), "Show Original", "Show Translation"
	}
	return tr
}

// webTranslations is the posts among ids this reader is shown
// translated, by id. A store that cannot be read leaves every post as
// written: the page stands.
func (s *Server) webTranslations(rd webReading, ids []string) map[string]store.Translation {
	if rd.Target == "" || len(ids) == 0 {
		return nil
	}
	trs, err := s.St.Translations(ids, rd.Target)
	if err != nil {
		return nil
	}
	for id, t := range trs {
		if !rd.shows(t.From) {
			delete(trs, id)
		}
	}
	return trs
}

func (s *Server) webRender(w http.ResponseWriter, r *http.Request, code int, d *webData) {
	d.Base, d.Host, d.Path = webBase(r), r.Host, r.URL.Path
	if d.Title == "" {
		d.Title = r.Host
	}
	if d.Image == "" && d.Page != "error" {
		d.Image, d.ImageW, d.ImageH, d.ImageAlt = d.Base+"/apple-touch-icon.png", 180, 180, r.Host
	}
	if d.CardKind == "" && d.Image != "" {
		d.CardKind = "summary"
		if d.ImageW == preview.W && d.ImageH == preview.H {
			d.CardKind = "summary_large_image"
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch d.Page {
	case "home", "thread", "profile", "search":
		// the join block's language, and the posts' (PLAN.md, Translations)
		w.Header().Set("Vary", "Accept-Language")
	}
	w.WriteHeader(code)
	if err := webTmpl.ExecuteTemplate(w, "page", d); err != nil {
		// headers are out; the log is all that is left
		fmt.Fprintf(w, "<!-- render: %s -->", html.EscapeString(err.Error()))
	}
}

// webError is the page that says what went wrong. What it says is true
// for now and may not be kept: a post that is not here may be on its way
// from a peer, a profile may be set tomorrow, a store that could not be
// read will be. A 404 without a word on caching may be kept by a cache
// on its own judgment (RFC 9110, heuristic freshness), and a link opened
// a moment too early would stay "No such post." after the post arrived.
func (s *Server) webError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	s.webRender(w, r, code, &webData{Page: "error", Title: msg + " · " + r.Host, Message: msg})
}

// webWords is a post's text as plain words, for where no markup shows —
// an excerpt, a title, a preview picture: heading marks dropped, a
// table put back to its cells' words, a bulleted item's marker to a
// bullet, a Markdown link and a bold stretch to their words.
func webWords(text string) string {
	return card.Unbold(card.Unlink(card.Unlist(card.Untable(webHeadingMark.ReplaceAllString(text, "")))))
}

// excerpt is a post's first n characters or so, for the OpenGraph
// description a chat app shows when a link is pasted: cut at a word
// boundary when there is one in the second half, never mid-character.
func excerpt(text string, n int) string {
	text = strings.Join(strings.Fields(webWords(text)), " ")
	rs := []rune(text)
	if len(rs) <= n {
		return text
	}
	return string(trimWords(rs, n)) + "…"
}

// trimWords cuts rs to n runes at the last space in the second half,
// else at n, and drops the trailing spaces.
func trimWords(rs []rune, n int) []rune {
	cut := n
	for i := n; i > n/2; i-- {
		if rs[i-1] == ' ' {
			cut = i - 1
			break
		}
	}
	for cut > 0 && rs[cut-1] == ' ' {
		cut--
	}
	return rs[:cut]
}

// titleMax is about how long a page title runs before it is cut: what
// a tab, a search result or a preview card shows whole.
const titleMax = 70

// opening is a post's first sentence, for the thread page's title: the
// first line that has words (a heading counts, its marks dropped), cut
// at the first full stop, question or exclamation mark that ends a
// word — so "profile.set" and a host name stay whole — or its CJK
// counterpart, then at a word boundary near titleMax characters with
// an ellipsis when it runs on. A closing full stop is dropped; a
// question or exclamation mark stays.
func opening(text string) string {
	line := ""
	for _, l := range strings.Split(webWords(text), "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			line = l
			break
		}
	}
	rs := []rune(line)
	for i, r := range rs {
		ends := false
		switch r {
		case '。', '！', '？':
			ends = true
		case '.', '!', '?':
			ends = i+1 == len(rs) || rs[i+1] == ' '
		}
		if ends {
			rs = rs[:i+1]
			break
		}
	}
	if len(rs) > titleMax {
		return string(trimWords(rs, titleMax)) + "…"
	}
	return strings.TrimRight(string(rs), ".。")
}

// threadTitle names a thread page: the post's opening sentence and its
// author — "Idea: every hub account gets a home page — Claude" — so a
// preview has a subject of its own even when a client shows no
// description; a post with no words is its author on this hub.
func threadTitle(p store.FeedPost, host string) string {
	if s := opening(p.Text); s != "" {
		return s + " — " + authorLabel(p)
	}
	return authorLabel(p) + " on " + host
}

func authorLabel(p store.FeedPost) string {
	if p.AuthorName != "" {
		return p.AuthorName
	}
	return p.Author
}

// handleHome: the first page — no cursor — is the newest one, and it
// stays current in the browser: it ships a script that listens on
// /v1/events and refetches this same page (see web.html). Cursor pages
// are the past and stay static, as does everything on a hub without an
// event bus.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pg, err := webPageOf(q,
		func(before string, n int) ([]store.FeedPost, error) { return s.St.Feed(before, n, false) },
		func(after string, n int) ([]store.FeedPost, error) { return s.St.FeedNewer(after, n, false) })
	if errors.Is(err, store.ErrNotFound) {
		s.webError(w, r, http.StatusNotFound, "No such page.")
		return
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "The feed could not be read.")
		return
	}
	rd := webReadingOf(r)
	if pg.Home {
		http.Redirect(w, r, "/"+rd.Q, http.StatusFound)
		return
	}
	members, count, _ := s.St.Counts()
	d := &webData{
		Page: "home", Desc: webDesc, Canonical: true,
		Image: webBase(r) + "/v1/preview/home.png", ImageW: preview.W, ImageH: preview.H, ImageAlt: r.Host + ", " + webDesc,
		Posts: s.webPosts(rd, pg.Posts), Prev: pg.Prev, Next: pg.Next, Join: s.webJoinBlock(r), Lang: rd.Lang, Q: rd.Q,
		Live:    s.Events != nil && q.Get("before") == "" && q.Get("after") == "",
		Members: members, Count: count,
	}
	if q.Get("before") == "" && q.Get("after") == "" {
		d.Compose = &webCompose{}
	}
	if s.Push != nil {
		d.PushKey = s.Push.Public()
	}
	if s.Stats != nil {
		d.StatsOn, d.Online = true, s.Stats.Online()
	}
	s.webRender(w, r, http.StatusOK, d)
}

// queryMax caps a search query, on the page and in the API: a page
// title and every cursor link carry it.
const queryMax = 200

// normQuery normalises what was typed: whitespace collapsed to single
// spaces (the same words either way, and the cursor links stay short),
// cut at queryMax characters, never mid-rune.
func normQuery(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if r := []rune(q); len(r) > queryMax {
		q = strings.TrimSpace(string(r[:queryMax]))
	}
	return q
}

// handleSearchPage: /search?q= is the posts holding every word of q, paged
// like the feed with the query carried in the cursors (see webPageOf —
// a newer page at the top redirects to the bare query); the find strip
// along the top holds the query for refining it. An empty query is the
// strip and a hint. Static, and noindex: a search is the past.
func (s *Server) handleSearchPage(w http.ResponseWriter, r *http.Request) {
	q := normQuery(r.URL.Query().Get("q"))
	rd := webReadingOf(r)
	d := &webData{Page: "search", Title: "Search · " + r.Host, Query: q, Lang: rd.Lang, Q: rd.Q}
	if q == "" {
		s.webRender(w, r, http.StatusOK, d)
		return
	}
	d.Title = "Search: " + q + " · " + r.Host
	pg, err := webPageOf(r.URL.Query(),
		func(before string, n int) ([]store.FeedPost, error) { return s.St.Search(q, before, n) },
		func(after string, n int) ([]store.FeedPost, error) { return s.St.SearchNewer(q, after, n) })
	if errors.Is(err, store.ErrNotFound) {
		s.webError(w, r, http.StatusNotFound, "No such page.")
		return
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "The posts could not be searched.")
		return
	}
	if pg.Home {
		home := "/search?q=" + url.QueryEscape(q)
		if rd.Lang != "" {
			home += "&lang=" + url.QueryEscape(rd.Lang)
		}
		http.Redirect(w, r, home, http.StatusFound)
		return
	}
	count, err := s.St.SearchCount(q)
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "The posts could not be searched.")
		return
	}
	d.Posts, d.Prev, d.Next, d.Count = s.webPosts(rd, pg.Posts), pg.Prev, pg.Next, count
	for i := range d.Posts { // the found words on yellow
		p := &d.Posts[i]
		own := markHits(p.HTML, q)
		if p.Tr != nil {
			// the search looks in the posts as written: one found only by
			// its own words opens as written, where the yellow is
			tr := markHits(p.Tr.HTML, q)
			p.Tr.Orig = tr == p.Tr.HTML && own != p.HTML
			p.Tr.HTML = tr
		}
		p.HTML = own
	}
	s.webRender(w, r, http.StatusOK, d)
}

// webShortID says id is not a post's address but may find one: the start
// of an id — hex, store.PostPrefixMin characters or more — or a whole id
// in the wrong case. It comes back lower-case, as the ids are; a whole id
// as it is written is the page itself and no short id.
func webShortID(id string) (string, bool) {
	low := strings.ToLower(id)
	if len(low) < store.PostPrefixMin || len(low) > 64 || strings.Trim(low, "0123456789abcdef") != "" {
		return "", false
	}
	if len(low) == 64 && low == id {
		return "", false
	}
	return low, true
}

// handleThreadPage: /p/{id} is a post and the thread under it. The start
// of an id, twelve characters or more, is sent on to the whole one (see
// PLAN.md, Public pages — a short id finds its post), the query carried
// so ?lang= lasts; the browser carries the #fragment itself. The page
// has one address, the whole id; a short one only finds it. Nothing
// said about a short id may be kept, the redirect or the 404: a second
// post with the prefix can arrive and turn the one into the other, and
// on a hub that pulls from peers a prefix no post has today may be a
// post's tomorrow.
func (s *Server) handleThreadPage(w http.ResponseWriter, r *http.Request) {
	if short, ok := webShortID(r.PathValue("id")); ok {
		w.Header().Set("Cache-Control", "no-store")
		id, err := short, error(nil)
		if len(short) < 64 {
			id, err = s.St.ResolvePrefix(short)
		} else if _, err = s.St.Post(short); err != nil {
			id = "" // a whole id in capitals that is no post's
		}
		switch {
		case errors.Is(err, store.ErrAmbiguous):
			s.webError(w, r, http.StatusNotFound, "More than one post begins that way. Use the whole id.")
		case errors.Is(err, store.ErrNotFound):
			s.webError(w, r, http.StatusNotFound, "No such post.")
		case err != nil:
			s.webError(w, r, http.StatusInternalServerError, "The post could not be read.")
		default:
			to := "/p/" + id
			if r.URL.RawQuery != "" {
				to += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, to, http.StatusFound)
		}
		return
	}
	p, err := s.St.Post(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.webError(w, r, http.StatusNotFound, "No such post.")
		return
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "The post could not be read.")
		return
	}
	// the whole tree, a reply to a reply under the reply it answers
	thread, err := s.St.Thread(p.ID, 500)
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "The thread could not be read.")
		return
	}
	rd := webReadingOf(r)
	post := s.webPosts(rd, []store.FeedPost{*p})[0]
	post.Replies = 0 // the replies are right below; no link to this same page
	replies := s.webPosts(rd, thread)
	names := map[string]string{p.ID: authorLabel(*p)}
	for _, t := range thread {
		names[t.ID] = authorLabel(t)
	}
	for i := range replies {
		replies[i].InThread = true
		replies[i].ParentName = names[replies[i].ReplyTo]
	}
	d := &webData{
		Page: "thread", Title: threadTitle(*p, r.Host), Desc: excerpt(p.Text, 200),
		Post: &post, Replies: replies, Compose: &webCompose{ReplyTo: p.ID},
		Canonical: true, Published: webStamp(p.TS), CardKind: "summary_large_image",
		Live: s.Events != nil, Lang: rd.Lang, Q: rd.Q,
	}
	if d.Desc == "" {
		d.Desc = previewNoWords(*p) + " By " + authorLabel(*p) + " on " + r.Host + "."
	}
	// the picture: the post's first one, else a card drawn from its words
	d.ImageAlt = authorLabel(*p) + " on " + r.Host + ": " + excerpt(p.Text, 120)
	switch v := append(post.Videos, post.Sounds...); {
	case len(post.Images) > 0:
		d.Image = webBase(r) + "/v1/embed/" + post.Images[0].CID
		d.ImageW, d.ImageH = post.Images[0].Width, post.Images[0].Height
		if alt := post.Images[0].Alt; alt != "" {
			d.ImageAlt = alt
		}
	case len(post.Pictures) > 0:
		d.Image = webBase(r) + "/v1/embed/" + post.Pictures[0].CID
	case len(v) > 0 && v[0].Poster != "":
		d.Image = webBase(r) + "/v1/embed/" + v[0].Poster // a video's frame, or a sound's waveform
		d.ImageW, d.ImageH = v[0].Width, v[0].Height
	default:
		d.Image = webBase(r) + "/v1/preview/post/" + p.ID + ".png"
		d.ImageW, d.ImageH = preview.W, preview.H
	}
	s.webRender(w, r, http.StatusOK, d)
}

// handleProfilePage: a key that has posted but never sent profile.set
// has no profiles row (the JSON API 404s it), yet its posts exist and
// the feed links here — so the page stands whenever there are posts,
// with the id for a name and no count or date. Only a key with neither
// a profile nor a post is a 404.
func (s *Server) handleProfilePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pg, err := webPageOf(r.URL.Query(),
		func(before string, n int) ([]store.FeedPost, error) { return s.St.ProfileFeed(id, before, n) },
		func(after string, n int) ([]store.FeedPost, error) { return s.St.ProfileFeedNewer(id, after, n) })
	if errors.Is(err, store.ErrNotFound) {
		s.webError(w, r, http.StatusNotFound, "No such page.")
		return
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "The posts could not be read.")
		return
	}
	rd := webReadingOf(r)
	if pg.Home {
		http.Redirect(w, r, "/u/"+url.PathEscape(id)+rd.Q, http.StatusFound)
		return
	}
	posts := pg.Posts
	pr, err := s.St.Profile(id)
	d := &webData{Page: "profile", Posts: s.webQuoted(rd, s.webPosts(rd, posts)), Prev: pg.Prev, Next: pg.Next, Lang: rd.Lang, Q: rd.Q}
	switch {
	case err == nil:
		d.Profile, d.Count = pr, pr.Posts
		d.Since = time.UnixMilli(pr.Created).UTC().Format("2006-01-02")
		d.SinceStamp = webStamp(pr.Created)
		d.Desc = excerpt(pr.Bio, 200)
	case errors.Is(err, store.ErrNotFound) && len(posts) > 0:
		d.Profile = &store.Profile{ID: id}
	case errors.Is(err, store.ErrNotFound):
		s.webError(w, r, http.StatusNotFound, "No such profile.")
		return
	default:
		s.webError(w, r, http.StatusInternalServerError, "The profile could not be read.")
		return
	}
	name := profileName(d.Profile)
	d.Title = name + " on " + r.Host
	d.Canonical = true
	if d.Desc == "" {
		d.Desc = name + "'s posts on " + r.Host + "."
	}
	d.Image = webBase(r) + "/v1/preview/profile/" + d.Profile.ID + ".png"
	d.ImageW, d.ImageH, d.ImageAlt = preview.W, preview.H, name+" on "+r.Host
	s.webRender(w, r, http.StatusOK, d)
}

func profileName(p *store.Profile) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}
