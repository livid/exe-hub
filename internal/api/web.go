package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"exehub/internal/envelope"
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
// once.

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

var webTmpl = template.Must(template.New("web").Parse(webHTML))

// webPage is the page size of the feed and profile pages; the next page
// is a plain link carrying the last post's id as the keyset cursor.
const webPage = 30

// webPost is a feed post prepared for the template: text rendered to
// safe HTML, the time as a fixed UTC stamp (the page is static, so a
// relative time would go stale), embeds split into pictures shown
// inline and files offered as links.
type webPost struct {
	store.FeedPost
	HTML   template.HTML
	When   string
	Images []envelope.Embed
	Files  []envelope.Embed
	// on a thread page: the reply this one answers is on the same page —
	// a nested reply links to it in place, by the name it was posted under
	InThread   bool
	ParentName string
}

// webJoin is the join block: where this hub is, what its gate asks for.
type webJoin struct {
	Base     string
	HubID    string
	Token    bool
	Mints    []webMint
	Cooldown int
}

type webMint struct {
	Amount string // "10,000 tokens", or "10000000000 raw base units" before the RPC has answered
	Mint   string
}

// webData is the template's world for one page.
type webData struct {
	Base, Host, Path string
	Title, Desc      string
	Image            string // OpenGraph picture, when the page has one
	Page             string // "home" | "thread" | "profile" | "error"
	Posts            []webPost
	Prev, Next       string // keyset cursors for the neighbouring pages, "" at either end
	Live             bool   // the home page's first page: ships the live-feed script
	PushKey          string // the home page on a hub that pushes: the VAPID public key for the Notify box
	Query            string // the search page's words, and what the find strip's field holds
	Join             *webJoin
	Post             *webPost
	Replies          []webPost
	Profile          *store.Profile
	Since            string
	Members, Count   int
	Message          string
}

// webURL is the Hub app's URL matcher: http(s) only; the period, comma
// or bracket that ends a sentence stays outside the link; and a URL is
// ASCII plus accented Latin letters, so CJK prose and its fullwidth
// punctuation end it — Chinese sits flush against a link with no space
// ("来自小虾平台（https://xiaoxia.app）的助手", "详见https://x.y的说明").
// A raw CJK path loses its tail; browsers paste those percent-encoded.
const webURLLatin = `\x{C0}-\x{D6}\x{D8}-\x{F6}\x{F8}-\x{24F}`

var webURL = regexp.MustCompile(`https?://[\w#$%&+,\-./:;=?@\[\]~` + webURLLatin + `]*[\w#$%&+\-/;=@` + webURLLatin + `]`)

// webCode is an inline `code` span: no newlines, no nesting.
var webCode = regexp.MustCompile("`([^`\n]+)`")

// webHeading is a heading line: one to three # and a space, then words
// (Markdown's ATX form, three levels — the one piece of Markdown a post
// takes, nothing else of it). webHeadingMark is the marker alone, for
// an excerpt to drop.
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
// marked .first, for no room above it — and elsewhere URLs outside code
// spans wrapped in anchors that open in a new tab, code spans set in
// <code>, newlines kept as line breaks.
func renderText(text string) template.HTML {
	var b strings.Builder
	lines := strings.Split(text, "\n")
	plain := ""
	flush := func() {
		if plain != "" {
			writeInline(&b, plain)
			plain = ""
		}
	}
	for i := 0; i < len(lines); i++ {
		m := webHeading.FindStringSubmatch(lines[i])
		if m == nil {
			plain += lines[i]
			if i < len(lines)-1 {
				plain += "\n"
			}
			continue
		}
		plain = strings.TrimSuffix(plain, "\n") // the break before the heading
		plain = strings.TrimSuffix(plain, "\n") // one blank line above it
		flush()
		tag := "h" + strconv.Itoa(len(m[1]))
		if b.Len() == 0 {
			b.WriteString("<" + tag + ` class="first">`)
		} else {
			b.WriteString("<" + tag + ">")
		}
		writeInline(&b, m[2])
		b.WriteString("</" + tag + ">\n")
		if i+1 < len(lines) && lines[i+1] == "" {
			i++ // one blank line below it
		}
	}
	flush()
	return template.HTML(b.String())
}

func writeInline(b *strings.Builder, text string) {
	last := 0
	for _, m := range webCode.FindAllStringSubmatchIndex(text, -1) {
		writeLinked(b, text[last:m[0]])
		b.WriteString("<code>" + html.EscapeString(text[m[2]:m[3]]) + "</code>")
		last = m[1]
	}
	writeLinked(b, text[last:])
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

// webWhen formats a post's own timestamp (kept for display, as PLAN.md
// says; ordering uses receive time) as a fixed UTC stamp.
func webWhen(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC")
}

func webPosts(posts []store.FeedPost) []webPost {
	out := make([]webPost, len(posts))
	for i, p := range posts {
		out[i] = webPost{FeedPost: p, HTML: renderText(p.Text), When: webWhen(p.TS)}
		for _, e := range p.Embeds {
			if strings.HasPrefix(e.MIME, "image/") {
				out[i].Images = append(out[i].Images, e)
			} else {
				out[i].Files = append(out[i].Files, e)
			}
		}
	}
	return out
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
	j := &webJoin{Base: webBase(r), Cooldown: c.CooldownSec()}
	if s.Hub != nil {
		j.HubID = s.Hub.ID
	}
	if c.Gate.Mode == "token" {
		j.Token = true
		for _, m := range c.Gate.Token.Mints {
			amount := m.MinAmount + " raw base units"
			if s.Gate != nil {
				if dec, known := s.Gate.Decimals(m.Mint); known {
					amount = humanUnits(m.MinRaw, dec) + " tokens"
				}
			}
			j.Mints = append(j.Mints, webMint{Amount: amount, Mint: m.Mint})
		}
	}
	return j
}

func (s *Server) webRender(w http.ResponseWriter, r *http.Request, code int, d *webData) {
	d.Base, d.Host, d.Path = webBase(r), r.Host, r.URL.Path
	if d.Title == "" {
		d.Title = r.Host
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	if err := webTmpl.ExecuteTemplate(w, "page", d); err != nil {
		// headers are out; the log is all that is left
		fmt.Fprintf(w, "<!-- render: %s -->", html.EscapeString(err.Error()))
	}
}

func (s *Server) webError(w http.ResponseWriter, r *http.Request, code int, msg string) {
	s.webRender(w, r, code, &webData{Page: "error", Title: msg + " · " + r.Host, Message: msg})
}

// excerpt is a post's first line or so, for the page title and the
// OpenGraph description a chat app shows when a link is pasted.
func excerpt(text string, n int) string {
	text = strings.Join(strings.Fields(webHeadingMark.ReplaceAllString(text, "")), " ")
	if len(text) <= n {
		return text
	}
	cut := strings.LastIndexByte(text[:n], ' ')
	if cut < n/2 {
		cut = n
	}
	return text[:cut] + "…"
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
	if pg.Home {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	members, count, _ := s.St.Counts()
	d := &webData{
		Page: "home", Desc: webDesc,
		Image: webBase(r) + "/apple-touch-icon.png",
		Posts: webPosts(pg.Posts), Prev: pg.Prev, Next: pg.Next, Join: s.webJoinBlock(r),
		Live:    s.Events != nil && q.Get("before") == "" && q.Get("after") == "",
		Members: members, Count: count,
	}
	if s.Push != nil {
		d.PushKey = s.Push.Public()
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
	d := &webData{Page: "search", Title: "Search · " + r.Host, Query: q, Image: webBase(r) + "/apple-touch-icon.png"}
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
		http.Redirect(w, r, "/search?q="+url.QueryEscape(q), http.StatusFound)
		return
	}
	count, err := s.St.SearchCount(q)
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "The posts could not be searched.")
		return
	}
	d.Posts, d.Prev, d.Next, d.Count = webPosts(pg.Posts), pg.Prev, pg.Next, count
	s.webRender(w, r, http.StatusOK, d)
}

func (s *Server) handleThreadPage(w http.ResponseWriter, r *http.Request) {
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
	post := webPosts([]store.FeedPost{*p})[0]
	post.Replies = 0 // the replies are right below; no link to this same page
	replies := webPosts(thread)
	names := map[string]string{p.ID: authorLabel(*p)}
	for _, t := range thread {
		names[t.ID] = authorLabel(t)
	}
	for i := range replies {
		replies[i].InThread = true
		replies[i].ParentName = names[replies[i].ReplyTo]
	}
	d := &webData{
		Page: "thread", Title: authorLabel(*p) + " on " + r.Host, Desc: excerpt(p.Text, 200),
		Post: &post, Replies: replies,
	}
	if len(post.Images) > 0 {
		d.Image = webBase(r) + "/v1/embed/" + post.Images[0].CID
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
	if pg.Home {
		http.Redirect(w, r, "/u/"+url.PathEscape(id), http.StatusFound)
		return
	}
	posts := pg.Posts
	pr, err := s.St.Profile(id)
	d := &webData{Page: "profile", Posts: webPosts(posts), Prev: pg.Prev, Next: pg.Next}
	switch {
	case err == nil:
		d.Profile, d.Count = pr, pr.Posts
		d.Since = time.UnixMilli(pr.Created).UTC().Format("2006-01-02")
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
	d.Image = webBase(r) + "/apple-touch-icon.png"
	if d.Profile.Avatar != "" {
		d.Image = webBase(r) + "/v1/embed/" + d.Profile.Avatar
	}
	s.webRender(w, r, http.StatusOK, d)
}

func profileName(p *store.Profile) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}
