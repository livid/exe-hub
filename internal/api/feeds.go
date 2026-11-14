package api

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"html"
	"net/http"
	"regexp"
	"strings"
	"time"

	"exehub/internal/card"
	"exehub/internal/envelope"
	"exehub/internal/store"
)

// A profile as a feed reader takes it (PLAN.md, Feeds): the same list
// in RSS 2.0, Atom and JSON Feed 1.1 — the profile's newest posts that
// began a thread, each the post as the pages draw it, every address
// absolute. Livid, 2026-11-14: "implement all three feed formats for
// profiles on exe-hub: RSS, Atom, JSON Feed. Roots only."

// feedRoots is how many posts a feed carries.
const feedRoots = 50

// feedTitleLen is where an entry's title is cut.
const feedTitleLen = 100

// webFeed is a profile's feed, the parts the three formats share.
type webFeed struct {
	Base, Page, Self string // the hub's address, the profile's page, this feed's own address
	Name, Desc, Icon string
	Updated          time.Time
	Lang             string // the ?lang= target the entries were put into, "" for as written
	Items            []webFeedItem
}

// webFeedItem is one post in a feed.
type webFeedItem struct {
	URL, Title string
	HTML       string // the post as the pages draw it, its embeds under it
	Lang       string // the language the entry is in, when the post has been named one
	TS         time.Time
	Image      string           // the first picture's address, "" for none
	Media      []envelope.Embed // the videos, sounds, pages and files, for a format that lists attachments
}

func (s *Server) handleProfileRSS(w http.ResponseWriter, r *http.Request) {
	f, ok := s.webFeedOf(w, r, "feed.rss")
	if !ok {
		return
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n" +
		`<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom">` + "\n<channel>\n")
	xmlTag(&b, "title", f.Name)
	xmlTag(&b, "link", f.Page)
	xmlTag(&b, "description", f.Desc)
	if f.Lang != "" {
		xmlTag(&b, "language", f.Lang)
	}
	xmlTag(&b, "lastBuildDate", f.Updated.Format(time.RFC1123Z))
	b.WriteString(`<atom:link rel="self" type="application/rss+xml" href="` + xmlEsc(f.Self) + `"/>` + "\n")
	b.WriteString("<image>")
	xmlTag(&b, "url", f.Icon)
	xmlTag(&b, "title", f.Name)
	xmlTag(&b, "link", f.Page)
	b.WriteString("</image>\n")
	xmlTag(&b, "generator", "exe-hub")
	for _, it := range f.Items {
		b.WriteString("<item>\n")
		xmlTag(&b, "title", it.Title)
		xmlTag(&b, "link", it.URL)
		b.WriteString(`<guid isPermaLink="true">` + xmlEsc(it.URL) + "</guid>\n")
		xmlTag(&b, "pubDate", it.TS.Format(time.RFC1123Z))
		xmlTag(&b, "description", it.HTML)
		b.WriteString("</item>\n")
	}
	b.WriteString("</channel>\n</rss>\n")
	writeFeedBytes(w, "application/rss+xml", b.Bytes())
}

func (s *Server) handleProfileAtom(w http.ResponseWriter, r *http.Request) {
	f, ok := s.webFeedOf(w, r, "feed.atom")
	if !ok {
		return
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<feed xmlns="http://www.w3.org/2005/Atom"`)
	if f.Lang != "" {
		b.WriteString(` xml:lang="` + xmlEsc(f.Lang) + `"`)
	}
	b.WriteString(">\n")
	xmlTag(&b, "title", f.Name)
	if f.Desc != "" {
		xmlTag(&b, "subtitle", f.Desc)
	}
	xmlTag(&b, "id", f.Page)
	b.WriteString(`<link rel="alternate" type="text/html" href="` + xmlEsc(f.Page) + `"/>` + "\n")
	b.WriteString(`<link rel="self" type="application/atom+xml" href="` + xmlEsc(f.Self) + `"/>` + "\n")
	xmlTag(&b, "updated", f.Updated.Format(time.RFC3339))
	xmlTag(&b, "icon", f.Icon)
	b.WriteString("<author>")
	xmlTag(&b, "name", f.Name)
	xmlTag(&b, "uri", f.Page)
	b.WriteString("</author>\n")
	xmlTag(&b, "generator", "exe-hub")
	for _, it := range f.Items {
		b.WriteString("<entry>\n")
		xmlTag(&b, "id", it.URL)
		xmlTag(&b, "title", it.Title)
		b.WriteString(`<link rel="alternate" type="text/html" href="` + xmlEsc(it.URL) + `"/>` + "\n")
		xmlTag(&b, "published", it.TS.Format(time.RFC3339))
		xmlTag(&b, "updated", it.TS.Format(time.RFC3339))
		b.WriteString("<author>")
		xmlTag(&b, "name", f.Name)
		b.WriteString("</author>\n")
		b.WriteString(`<content type="html"`)
		if it.Lang != "" {
			b.WriteString(` xml:lang="` + xmlEsc(it.Lang) + `"`)
		}
		b.WriteString(">" + xmlEsc(it.HTML) + "</content>\n")
		b.WriteString("</entry>\n")
	}
	b.WriteString("</feed>\n")
	writeFeedBytes(w, "application/atom+xml", b.Bytes())
}

// jsonFeed and jsonFeedItem are JSON Feed 1.1's shapes.
type jsonFeed struct {
	Version     string           `json:"version"`
	Title       string           `json:"title"`
	HomePageURL string           `json:"home_page_url"`
	FeedURL     string           `json:"feed_url"`
	Description string           `json:"description,omitempty"`
	Icon        string           `json:"icon,omitempty"`
	Language    string           `json:"language,omitempty"`
	Authors     []jsonFeedAuthor `json:"authors"`
	Items       []jsonFeedItem   `json:"items"`
}

type jsonFeedAuthor struct {
	Name   string `json:"name"`
	URL    string `json:"url,omitempty"`
	Avatar string `json:"avatar,omitempty"`
}

type jsonFeedItem struct {
	ID            string               `json:"id"`
	URL           string               `json:"url"`
	Title         string               `json:"title"`
	ContentHTML   string               `json:"content_html"`
	DatePublished string               `json:"date_published"`
	Language      string               `json:"language,omitempty"`
	Image         string               `json:"image,omitempty"`
	Attachments   []jsonFeedAttachment `json:"attachments,omitempty"`
}

type jsonFeedAttachment struct {
	URL      string `json:"url"`
	MIMEType string `json:"mime_type"`
	Title    string `json:"title,omitempty"`
}

func (s *Server) handleProfileJSONFeed(w http.ResponseWriter, r *http.Request) {
	f, ok := s.webFeedOf(w, r, "feed.json")
	if !ok {
		return
	}
	out := jsonFeed{Version: "https://jsonfeed.org/version/1.1", Title: f.Name, HomePageURL: f.Page, FeedURL: f.Self,
		Description: f.Desc, Icon: f.Icon, Language: f.Lang,
		Authors: []jsonFeedAuthor{{Name: f.Name, URL: f.Page, Avatar: f.Icon}}, Items: []jsonFeedItem{}}
	for _, it := range f.Items {
		ji := jsonFeedItem{ID: it.URL, URL: it.URL, Title: it.Title, ContentHTML: it.HTML,
			DatePublished: it.TS.Format(time.RFC3339), Language: it.Lang, Image: it.Image}
		for _, e := range it.Media {
			ji.Attachments = append(ji.Attachments, jsonFeedAttachment{URL: f.Base + "/v1/embed/" + e.CID, MIMEType: e.MIME, Title: e.Filename})
		}
		out.Items = append(out.Items, ji)
	}
	var b bytes.Buffer // the HTML as it is, not \u003c for every tag
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeFeedBytes(w, "application/feed+json", b.Bytes())
}

func writeFeedBytes(w http.ResponseWriter, mime string, b []byte) {
	w.Header().Set("Content-Type", mime+"; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

// webFeedOf is the profile's feed for the request, or false after an
// error has been answered: 404 for an id that is not one, or one with no
// profile and no post. The entries are the profile's newest roots, each
// as the pages draw it (webPosts) with every address made absolute, in
// the language ?lang= names when the post has that translation — the
// query alone, as /v1/post reads it, so a reader that polls gets one
// feed whatever its browser says.
func (s *Server) webFeedOf(w http.ResponseWriter, r *http.Request, self string) (*webFeed, bool) {
	id := r.PathValue("id")
	if !profileIDRe.MatchString(id) {
		writeErr(w, http.StatusNotFound, errors.New("no such profile"))
		return nil, false
	}
	pr, err := s.St.Profile(id)
	if errors.Is(err, store.ErrNotFound) {
		pr = &store.Profile{ID: id}
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, false
	}
	posts, err := s.St.ProfileRoots(id, feedRoots)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, false
	}
	if pr.Created == 0 && len(posts) == 0 {
		writeErr(w, http.StatusNotFound, errors.New("no such profile"))
		return nil, false
	}
	rd := readingOf(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("lang"))), "")
	base := webBase(r)
	f := &webFeed{Base: base, Page: base + "/u/" + id, Self: base + "/u/" + id + "/" + self,
		Name: profileName(pr), Desc: pr.Bio, Lang: rd.Target, Icon: base + "/v1/identicon/" + id + ".svg"}
	if pr.Avatar != "" {
		f.Icon = base + "/v1/embed/" + pr.Avatar
	}
	if f.Desc == "" {
		f.Desc = f.Name + "'s posts on " + r.Host + "."
	}
	if pr.Updated > 0 {
		f.Updated = time.UnixMilli(pr.Updated).UTC()
	}
	for _, wp := range s.webPosts(rd, posts) {
		it := webFeedItem{URL: base + "/p/" + wp.ID, TS: time.UnixMilli(wp.TS).UTC(), Lang: wp.Lang}
		text, body := card.NameMentions(wp.Text, wp.Mentions), string(wp.HTML)
		if wp.Tr != nil {
			text, body, it.Lang = wp.Tr.Text, string(wp.Tr.HTML), wp.Tr.Lang
		}
		it.Title = feedTitle(text)
		if it.Title == "" {
			it.Title = f.Name + " · " + it.TS.Format("2006-01-02")
		}
		var b strings.Builder
		if body != "" {
			b.WriteString("<div>" + feedAbsolute(body, base) + "</div>")
		}
		pic := func(cid, alt string) {
			u := base + "/v1/embed/" + cid
			if it.Image == "" {
				it.Image = u
			}
			b.WriteString(`<p><a href="` + u + `"><img src="` + u + `" alt="` + html.EscapeString(alt) + `"></a></p>`)
		}
		for _, im := range wp.Images {
			pic(im.CID, im.Alt)
		}
		for _, p := range wp.Pictures {
			pic(p.CID, "")
		}
		for _, v := range wp.Videos {
			u := base + "/v1/embed/" + v.CID
			b.WriteString(`<p><video controls src="` + u + `"`)
			if v.Poster != "" {
				b.WriteString(` poster="` + base + "/v1/embed/" + v.Poster + `"`)
			}
			b.WriteString(`></video></p>`)
			it.Media = append(it.Media, v.Embed)
		}
		for _, a := range wp.Sounds {
			b.WriteString(`<p><audio controls src="` + base + "/v1/embed/" + a.CID + `"></audio></p>`)
			it.Media = append(it.Media, a.Embed)
		}
		for _, e := range append(append([]envelope.Embed{}, wp.Pages...), wp.Files...) {
			name := e.Filename
			if name == "" {
				name = e.CID
			}
			b.WriteString(`<p><a href="` + base + "/v1/embed/" + e.CID + `">` + html.EscapeString(name) + `</a></p>`)
			it.Media = append(it.Media, e)
		}
		it.HTML = b.String()
		if it.TS.After(f.Updated) {
			f.Updated = it.TS
		}
		f.Items = append(f.Items, it)
	}
	if f.Updated.IsZero() {
		f.Updated = time.Now().UTC()
	}
	return f, true
}

// feedRelRe finds a page-relative address in the pages' HTML: a link or
// a picture that starts at the hub's root, and not a protocol-relative
// one.
var feedRelRe = regexp.MustCompile(`(href|src)="/([^/"])`)

// feedAbsolute puts the hub's address in front of every page-relative
// address, so the HTML reads the same in a feed reader.
func feedAbsolute(body, base string) string {
	return feedRelRe.ReplaceAllStringFunc(body, func(m string) string {
		i := strings.Index(m, `="/`)
		return m[:i+2] + base + m[i+2:]
	})
}

// feedTitle is an entry's title: the text's first line with the
// Markdown marks taken off — a heading's #, a list's dash or star, a
// to-do box, a quote's >, bold and code marks — cut to feedTitleLen;
// "" for a post with no words.
func feedTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimLeft(line, "#> ")
		for _, m := range []string{"- [ ] ", "- [x] ", "- [X] ", "* [ ] ", "* [x] ", "- ", "* ", "+ "} {
			line = strings.TrimPrefix(line, m)
		}
		line = strings.NewReplacer("**", "", "__", "", "`", "").Replace(line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return excerpt(line, feedTitleLen)
	}
	return ""
}

// xmlTag writes <name>text</name> on a line of its own, the text
// escaped; nothing for an empty text.
func xmlTag(b *bytes.Buffer, name, text string) {
	if text == "" {
		return
	}
	b.WriteString("<" + name + ">" + xmlEsc(text) + "</" + name + ">\n")
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
