package api

import (
	"errors"
	"net/http"
	"time"

	"exehub/internal/envelope"
	"exehub/internal/store"
)

// The Pictures page (PLAN.md, Pictures): GET /pictures, every picture
// the hub holds as a wall of tiles, the newest first by its post's time,
// a month a block, each tile the way to its post; GET /u/{id}/pictures,
// the same wall for one profile. The wall is read per request: a page is
// one indexed query over the posts, and a new picture should be on the
// wall as soon as its post is in the feed.

// wallPage is about how many pictures a page holds: the page ends at a
// post's last picture (store.Wall).
const wallPage = 48

// webWall is the page's tiles in their months, newest first, with the
// count of the whole wall, the cursor of the page after this one ("" at
// the end) and, for one profile's wall, who.
type webWall struct {
	Months []webWallMonth
	Count  int
	Next   string
	Of     *store.Profile
	Name   string // Of's name as the page says it
}

type webWallMonth struct {
	Year, Month int
	Tiles       []webWallTile
}

// webWallTile is one picture: its post, who sent it and when (the
// tile's title), the hub's copy of it, and whether it is a drawing,
// which stands whole in its tile, pixel for pixel.
type webWallTile struct {
	Post, Author, Name, CID, Alt, Date string
	Drawing                            bool
}

// wall is a page of the wall, its pictures laid in months by dayZone.
func (s *Server) wall(author, before string) (*webWall, error) {
	pics, more, err := s.St.Wall(author, before, wallPage)
	if err != nil {
		return nil, err
	}
	count, err := s.St.WallCount(author)
	if err != nil {
		return nil, err
	}
	loc := s.dayZone()
	w := &webWall{Count: count}
	for _, p := range pics {
		t := time.UnixMilli(p.At).In(loc)
		y, m := t.Year(), int(t.Month())
		if n := len(w.Months); n == 0 || w.Months[n-1].Year != y || w.Months[n-1].Month != m {
			w.Months = append(w.Months, webWallMonth{Year: y, Month: m})
		}
		mo := &w.Months[len(w.Months)-1]
		name := p.AuthorName
		if name == "" {
			name = p.Author
		}
		mo.Tiles = append(mo.Tiles, webWallTile{Post: p.Post, Author: p.Author, Name: name, CID: p.CID, Alt: p.Alt,
			Date: t.Format("2006-01-02"), Drawing: isDrawing(envelope.Embed{MIME: p.MIME, Width: p.Width, Height: p.Height})})
	}
	if more && len(pics) > 0 {
		w.Next = pics[len(pics)-1].Post
	}
	return w, nil
}

// handlePicturesPage: GET /pictures, the hub's wall.
func (s *Server) handlePicturesPage(w http.ResponseWriter, r *http.Request) {
	s.wallPage(w, r, nil)
}

// handleProfilePicturesPage: GET /u/{id}/pictures, one profile's wall.
// Any 16-hex id has one, a profile or not, as a desk list does: an id
// may post before it sets a profile.
func (s *Server) handleProfilePicturesPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !profileIDRe.MatchString(id) {
		s.webError(w, r, http.StatusNotFound, "err.noprofile")
		return
	}
	p, err := s.St.Profile(id)
	if errors.Is(err, store.ErrNotFound) { // an id that never set a profile may still have posted
		p, err = &store.Profile{ID: id}, nil
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.pictures")
		return
	}
	s.wallPage(w, r, p)
}

// wallPage renders a page of the wall, the hub's (of nil) or a profile's.
// ?before= names the last post of the page before, as the page's Older
// button carries it; an unknown one has no page.
func (s *Server) wallPage(w http.ResponseWriter, r *http.Request, of *store.Profile) {
	author := ""
	if of != nil {
		author = of.ID
	}
	before := r.URL.Query().Get("before")
	wl, err := s.wall(author, before)
	if errors.Is(err, store.ErrNotFound) {
		s.webError(w, r, http.StatusNotFound, "err.nopage")
		return
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.pictures")
		return
	}
	rd := webReadingOf(r)
	title, desc := rd.L.T("pictures"), rd.L.T("pictures.desc", "host", r.Host)
	if of != nil {
		wl.Of, wl.Name = of, profileName(of)
		title, desc = rd.L.T("pictures.of", "name", wl.Name), rd.L.T("pictures.of.desc", "name", wl.Name, "host", r.Host)
	}
	s.webRender(w, r, http.StatusOK, &webData{
		Page: "pictures", Title: title + " · " + r.Host, Desc: desc,
		Canonical: before == "", Lang: rd.Lang, Q: rd.Q, Wall: wl, Profile: of,
		Message: rd.L.T("pictures.n", "n", wl.Count),
	})
}
