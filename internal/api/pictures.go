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
	Path   string // the wall's own address, which Older pages: /pictures, /u/{id}/pictures, /u/{id}/pictures/{day}
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

// wall is a page of the wall, its pictures laid in months by dayZone;
// from and to narrow it to the posts of a span (a day of the grid).
func (s *Server) wall(author, before string, from, to int64) (*webWall, error) {
	pics, more, err := s.St.WallBetween(author, from, to, before, wallPage)
	if err != nil {
		return nil, err
	}
	count, err := s.St.WallCountBetween(author, from, to)
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
	s.wallPage(w, r, nil, "")
}

// handleProfilePicturesDayPage: GET /u/{id}/pictures/{day}, the
// profile's pictures of one day — what a square of its Pictures grid
// counts (PLAN.md, Activity — a square is a link) — in the profile's
// window, the square marked. A day with no picture has no page.
func (s *Server) handleProfilePicturesDayPage(w http.ResponseWriter, r *http.Request) {
	id, day := r.PathValue("id"), r.PathValue("day")
	if !profileIDRe.MatchString(id) {
		s.webError(w, r, http.StatusNotFound, "err.noprofile")
		return
	}
	if _, _, _, ok := s.dayWindow(day); !ok {
		s.webError(w, r, http.StatusNotFound, "err.noday")
		return
	}
	p, err := s.St.Profile(id)
	if errors.Is(err, store.ErrNotFound) {
		p, err = &store.Profile{ID: id}, nil
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.pictures")
		return
	}
	s.wallPage(w, r, p, day)
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
	s.wallPage(w, r, p, "")
}

// wallPage renders a page of the wall, the hub's (of nil) or a profile's.
// ?before= names the last post of the page before, as the page's Older
// button carries it; an unknown one has no page.
func (s *Server) wallPage(w http.ResponseWriter, r *http.Request, of *store.Profile, day string) {
	author := ""
	if of != nil {
		author = of.ID
	}
	var from, to int64 = 0, 1 << 62
	var t time.Time
	if day != "" {
		from, to, t, _ = s.dayWindow(day) // the caller checked the day
	}
	before := r.URL.Query().Get("before")
	wl, err := s.wall(author, before, from, to)
	if errors.Is(err, store.ErrNotFound) {
		s.webError(w, r, http.StatusNotFound, "err.nopage")
		return
	}
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "err.pictures")
		return
	}
	if day != "" && wl.Count == 0 {
		s.webError(w, r, http.StatusNotFound, "err.noday")
		return
	}
	rd := webReadingOf(r)
	wl.Path = "/pictures"
	if of != nil {
		wl.Path = "/u/" + of.ID + "/pictures"
		if day != "" {
			wl.Path += "/" + day
		}
	}
	title, desc := rd.L.T("pictures"), rd.L.T("pictures.desc", "host", r.Host)
	d := &webData{
		Page: "pictures", Canonical: before == "", Lang: rd.Lang, Q: rd.Q, Wall: wl,
		Message: rd.L.T("pictures.n", "n", wl.Count),
	}
	if of != nil { // the profile's window, its Pictures tab on
		wl.Of, wl.Name = of, profileName(of)
		title, desc = rd.L.T("pictures.of", "name", wl.Name), rd.L.T("pictures.of.desc", "name", wl.Name, "host", r.Host)
		if err := s.profileHead(d, of); err != nil {
			s.webError(w, r, http.StatusInternalServerError, "err.pictures")
			return
		}
		if d.Activity, err = s.activity(of.ID, time.Now(), rd.L, "pictures", "/u/"+of.ID+"/pictures", rd.Q, day); err != nil {
			s.webError(w, r, http.StatusInternalServerError, "err.pictures")
			return
		}
		d.NavFor = of.ID
		if day != "" {
			d.Day, d.DayName, d.DayCount = day, dayName(rd.L, t), wl.Count
			title = d.DayName + " · " + title
			desc = rd.L.T("activity.pictures.day.desc", "name", wl.Name, "date", d.DayName, "n", wl.Count)
		}
	}
	d.Title, d.Desc = title+" · "+r.Host, desc
	s.webRender(w, r, http.StatusOK, d)
}
