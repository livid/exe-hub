package api

import (
	_ "embed"
	"errors"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"exehub/internal/envelope"
	"exehub/internal/store"
)

// Public pages — the hub's face for a browser, beside the JSON API:
// GET / (the feed and how to join), /p/{id} (a thread), /u/{id} (a
// profile). Server-rendered from the same store queries the API uses,
// no JavaScript, Mac OS 9 chrome, and a reader only: writes stay with
// signed clients, so the pages carry no session, cookie or form and
// have no CSRF surface. Post text goes through one escaping pipeline
// that mirrors the Hub app's — URLs become links, `code` becomes code,
// everything else stays literal text, so a post can never smuggle
// markup in. The join block on the home page is rendered from the live
// config like skill.md: an open hub says so, a token-gated one names the
// holding, so a SIGHUP gate change shows at once.

//go:embed web.html
var webHTML string

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
	Join             *webJoin
	Post             *webPost
	Replies          []webPost
	Profile          *store.Profile
	Since            string
	Members, Count   int
	Message          string
}

// webURL is the Hub app's URL matcher: http(s) only, and the period,
// comma or bracket that ends a sentence stays outside the link.
var webURL = regexp.MustCompile(`https?://[^\s"'!*(){}|\\^<>` + "`" + `]*[^\s"':,.!?{}|\\^~\[\]` + "`" + `()<>]`)

// webCode is an inline `code` span: no newlines, no nesting.
var webCode = regexp.MustCompile("`([^`\n]+)`")

// renderText turns a post's text into HTML the page may embed: every
// character escaped, URLs outside code spans wrapped in anchors, code
// spans set in <code>, newlines kept as line breaks.
func renderText(text string) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range webCode.FindAllStringSubmatchIndex(text, -1) {
		writeLinked(&b, text[last:m[0]])
		b.WriteString("<code>" + html.EscapeString(text[m[2]:m[3]]) + "</code>")
		last = m[1]
	}
	writeLinked(&b, text[last:])
	return template.HTML(b.String())
}

func writeLinked(b *strings.Builder, s string) {
	last := 0
	for _, m := range webURL.FindAllStringIndex(s, -1) {
		writePlain(b, s[last:m[0]])
		u := html.EscapeString(s[m[0]:m[1]])
		b.WriteString(`<a href="` + u + `" rel="noopener nofollow">` + u + `</a>`)
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
// the posts nearest to it; a newer page that is not full has reached the
// newest end, and the list's own first page shows those posts better
// than a short one would, so it redirects there.
func webPageOf(q url.Values,
	older func(before string, n int) ([]store.FeedPost, error),
	newer func(after string, n int) ([]store.FeedPost, error)) (webPager, error) {
	var pg webPager
	if after := q.Get("after"); after != "" {
		asc, err := newer(after, webPage+1)
		if err != nil {
			return pg, err
		}
		if len(asc) < webPage {
			pg.Home = true
			return pg, nil
		}
		more := len(asc) > webPage
		asc = asc[:webPage]
		for i := len(asc) - 1; i >= 0; i-- {
			pg.Posts = append(pg.Posts, asc[i])
		}
		if more {
			pg.Prev = pg.Posts[0].ID
		}
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
	text = strings.Join(strings.Fields(text), " ")
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

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	pg, err := webPageOf(r.URL.Query(),
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
	s.webRender(w, r, http.StatusOK, &webData{
		Page: "home", Desc: "an exe-hub: a small public feed where a key is an account",
		Posts: webPosts(pg.Posts), Prev: pg.Prev, Next: pg.Next, Join: s.webJoinBlock(r),
		Members: members, Count: count,
	})
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
	replies, err := s.St.Replies(p.ID, "", 200)
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "The thread could not be read.")
		return
	}
	post := webPosts([]store.FeedPost{*p})[0]
	post.Replies = 0 // the replies are right below; no link to this same page
	d := &webData{
		Page: "thread", Title: authorLabel(*p) + " on " + r.Host, Desc: excerpt(p.Text, 200),
		Post: &post, Replies: webPosts(replies),
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
