package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"
	"time"

	"exehub/internal/identicon"
	"exehub/internal/preview"
	"exehub/internal/store"
)

// The preview pictures: what a chat app or a social site shows for a
// pasted link (see PLAN.md, Public pages — Link previews). Each is
// drawn on request from what the page shows, so it is never stale, and
// cached for a while by whoever fetches it. `.png` on the id is for
// the crawlers that want an extension; the route takes it either way.

// previewMax bounds a picture read from IPFS for a card: an avatar is
// a 128px PNG, a hub icon smaller still.
const previewMax = 4 << 20

// picture is the image behind a pinned CID, nil when there is none,
// the store has no IPFS, or the bytes are not a picture.
func (s *Server) picture(ctx context.Context, cid string) image.Image {
	if cid == "" || s.IPFS == nil {
		return nil
	}
	pin, err := s.St.PinInfo(cid)
	if err != nil || !strings.HasPrefix(pin.MIME, "image/") || pin.Size > previewMax {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	f := s.IPFS.Open(ctx, cid, pin.Size)
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, previewMax))
	if err != nil {
		return nil
	}
	img, err := preview.Decode(b)
	if err != nil {
		return nil
	}
	return img
}

// portrait is who a card is about: the avatar, or for a profile with no
// picture (or one that can't be read just now) the face the pages draw
// from the id — made at the card's size in whole cells, so it goes on
// crisp.
func (s *Server) portrait(ctx context.Context, cid, id string) (image.Image, bool) {
	if img := s.picture(ctx, cid); img != nil {
		return img, false
	}
	if !identicon.Valid(id) {
		return nil, false
	}
	return identicon.Image(id, preview.Pic), true
}

// servePreview writes a card as PNG: public, cached ten minutes (a
// reply count moves), with an ETag over the bytes.
func servePreview(w http.ResponseWriter, r *http.Request, c preview.Card) {
	b, err := c.PNG()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sum := sha256.Sum256(b)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=600")
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(b)))
}

func previewID(r *http.Request) string {
	return strings.TrimSuffix(r.PathValue("id"), ".png")
}

// previewWhen is the date line a card shows for a post: UTC, the
// pages' local-time rewrite being script.
func previewWhen(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2 Jan 2006 · 15:04 UTC")
}

// previewReplies is the thread page's status line.
func previewReplies(n int) string {
	switch n {
	case 0:
		return "No replies yet"
	case 1:
		return "1 reply"
	}
	return fmt.Sprintf("%d replies", n)
}

// handlePreviewPost: GET /v1/preview/post/{id} — the author and the
// words of one post, the thread's reply count on the status line.
func (s *Server) handlePreviewPost(w http.ResponseWriter, r *http.Request) {
	p, err := s.St.Post(previewID(r))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, errors.New("no such post"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	body := webWords(named(*p))
	if strings.TrimSpace(body) == "" {
		body = previewNoWords(*p)
	}
	pic, crisp := s.portrait(r.Context(), p.Avatar, p.Author)
	servePreview(w, r, preview.Card{
		Title: r.Host, Picture: pic, Crisp: crisp,
		Name: authorLabel(*p), Sub: previewWhen(p.TS), Body: body,
		Foot: previewReplies(p.Replies),
	})
}

// previewNoWords stands in for a post that is all pictures or files.
func previewNoWords(p store.FeedPost) string {
	n := 0
	for _, e := range p.Embeds {
		if strings.HasPrefix(e.MIME, "image/") {
			n++
		}
	}
	switch {
	case n == 1:
		return "A picture."
	case n > 1:
		return fmt.Sprintf("%d pictures.", n)
	case len(p.Embeds) > 0:
		return "An attachment."
	}
	return "A post."
}

// handlePreviewProfile: GET /v1/preview/profile/{id} — the avatar, the
// name, the bio, the post count. A key with posts but no profile row
// stands as its id, as the page does.
func (s *Server) handlePreviewProfile(w http.ResponseWriter, r *http.Request) {
	id := previewID(r)
	pr, err := s.St.Profile(id)
	if errors.Is(err, store.ErrNotFound) {
		if posts, ferr := s.St.ProfileFeed(id, "", 1); ferr != nil || len(posts) == 0 {
			writeErr(w, http.StatusNotFound, errors.New("no such profile"))
			return
		}
		pr = &store.Profile{ID: id}
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	pic, crisp := s.portrait(r.Context(), pr.Avatar, pr.ID)
	c := preview.Card{Title: r.Host, Picture: pic, Crisp: crisp, Name: profileName(pr), Body: pr.Bio}
	if pr.Created > 0 {
		c.Sub = "since " + time.UnixMilli(pr.Created).UTC().Format("2 Jan 2006")
		c.Foot = fmt.Sprintf("%d posts", pr.Posts)
		if pr.Posts == 1 {
			c.Foot = "1 post"
		}
	} else {
		c.Sub = pr.ID
	}
	if c.Name != pr.ID && c.Sub != pr.ID {
		c.Sub += " · " + pr.ID
	}
	servePreview(w, r, c)
}

// handlePreviewHome: GET /v1/preview/home.png — the hub itself: its
// icon, its host, what it is, how many are here.
func (s *Server) handlePreviewHome(w http.ResponseWriter, r *http.Request) {
	members, count, _ := s.St.Counts()
	icon, _ := preview.Decode(webIcon512)
	servePreview(w, r, preview.Card{
		Title: r.Host, Picture: icon, Crisp: true, Name: r.Host, Sub: "an exe-hub",
		Body: strings.ToUpper(webDesc[:1]) + webDesc[1:] + ".",
		Foot: fmt.Sprintf("%d members · %d posts", members, count),
	})
}
