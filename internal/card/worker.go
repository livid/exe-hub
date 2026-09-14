package card

import (
	"bytes"
	"context"
	"errors"
	"log"
	"time"

	"exehub/internal/envelope"
	"exehub/internal/events"
	"exehub/internal/ipfs"
	"exehub/internal/store"
)

// Worker derives what a post's links yield, off the ingest path: its
// link card, and the pictures its IPFS links name (see PLAN.md, Link
// cards and Linked pictures). Enqueue never blocks, one goroutine
// fetches, and whatever lands is announced on the bus as post.card so
// live pages bring it in. A card is attempted once — success or failure
// is recorded — so a dead link never turns into a retry loop; a linked
// picture, whose gateway may not answer the first time, gets a few
// rounds an hour apart. A repost gets fresh tries.
type Worker struct {
	St   *store.Store
	IPFS *ipfs.Client
	Bus  *events.Broadcaster
	F    *Fetcher
	// Archive, when set, is asked for the page's archived copy once a
	// card lands (see PLAN.md, Archived copies).
	Archive *Archiver
	queue   chan job
}

type job struct {
	post, author, text string
	card               bool // derive the card too (a bare-link post; one with embeds gets none)
	redo               bool // replace a card already stored (a misread one)
}

// A linked picture gets at most pictureTries fetches, an hour apart —
// the sweep's period — before its link is left a link.
const (
	pictureTries = 3
	pictureEvery = time.Hour
)

func NewWorker(st *store.Store, ipfsc *ipfs.Client, bus *events.Broadcaster) *Worker {
	return &Worker{St: st, IPFS: ipfsc, Bus: bus, F: NewFetcher(), queue: make(chan job, 256)}
}

// Enqueue asks for what the post's links yield: its card, when card is
// set and the text carries a link, and its linked pictures. A full
// queue drops the job — the backfill pass at next start absorbs the
// loss.
func (w *Worker) Enqueue(post, author, text string, card bool) {
	w.enqueue(job{post: post, author: author, text: text, card: card})
}

func (w *Worker) enqueue(j job) {
	if (!j.card || First(j.text) == "") && len(IPFSLinks(j.text)) == 0 {
		return
	}
	select {
	case w.queue <- j:
	default:
		log.Printf("card: queue full, dropping %s", j.post)
	}
}

func (w *Worker) Run() {
	for j := range w.queue {
		w.derive(j)
	}
}

// Backfill queues a card for every existing bare-link post that has
// none, and the linked pictures of every post never tried for them —
// the pass that gives history its cards and pictures after each feature
// (or a dropped queue) and costs nothing when there is nothing to do.
func (w *Worker) Backfill() {
	time.Sleep(15 * time.Second) // let the daemon and kubo settle first
	posts, err := w.St.PostsWithoutCards(256)
	if err != nil {
		log.Printf("card backfill: %v", err)
		return
	}
	n := 0
	for _, p := range posts {
		if First(p.Text) != "" {
			n++
		}
		w.Enqueue(p.ID, p.Author, p.Text, true)
	}
	if n > 0 {
		log.Printf("card backfill: %d posts queued", n)
	}
	misread, err := w.St.CardsMisread(Misread)
	if err != nil {
		log.Printf("card backfill: %v", err)
		return
	}
	for _, p := range misread {
		select {
		case w.queue <- job{post: p.ID, author: p.Author, text: p.Text, card: true, redo: true}:
		default:
		}
	}
	if len(misread) > 0 {
		log.Printf("card backfill: %d misread cards queued again", len(misread))
	}
	untried, err := w.St.PostsWithoutPictures(256)
	if err != nil {
		log.Printf("picture backfill: %v", err)
		return
	}
	n = 0
	for _, p := range untried {
		if len(IPFSLinks(p.Text)) == 0 {
			continue
		}
		n++
		w.Enqueue(p.ID, p.Author, p.Text, false)
	}
	if n > 0 {
		log.Printf("picture backfill: %d posts queued", n)
	}
}

// Sweep queues, hourly, every post with a linked picture that failed,
// has rounds left and was last tried an hour ago — the gateway that did
// not answer the first time.
func (w *Worker) Sweep() {
	time.Sleep(25 * time.Second) // after the backfill has had its turn
	for {
		posts, err := w.St.PicturesToRetry(pictureTries, time.Now().Add(-pictureEvery).UnixMilli(), 64)
		if err != nil {
			log.Printf("picture sweep: %v", err)
		}
		for _, p := range posts {
			w.Enqueue(p.ID, p.Author, p.Text, false)
		}
		time.Sleep(pictureEvery)
	}
}

func (w *Worker) derive(j job) {
	if j.card {
		if has, err := w.St.HasCard(j.post); err == nil && (!has || j.redo) {
			w.deriveCard(j)
		}
	}
	w.derivePictures(j)
}

func (w *Worker) deriveCard(j job) {
	link := First(j.text)
	if link == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	m, err := w.F.Fetch(ctx, link)
	if err != nil {
		log.Printf("card %s: %v", j.post, err)
		w.store(j, store.Card{URL: link}, 0, "", false)
		return
	}
	c := store.Card{URL: link, Host: m.Host, Title: m.Title, Desc: m.Desc}
	var size int64
	mime := ""
	if m.ImageURL != "" {
		data, mt, err := w.F.FetchImage(ctx, m.ImageURL)
		if err == nil {
			cid, err := w.IPFS.Add(bytes.NewReader(data), envelope.MsgID(data))
			if err == nil {
				c.Image, size, mime = cid, int64(len(data)), mt
			} else {
				log.Printf("card %s: pin picture: %v", j.post, err)
			}
		} else {
			log.Printf("card %s: picture: %v", j.post, err) // the card stands without it
		}
	}
	w.store(j, c, size, mime, true)
}

func (w *Worker) store(j job, c store.Card, imageSize int64, imageMIME string, ok bool) {
	unpin, err := w.St.SetCard(j.post, c, imageSize, imageMIME, ok)
	if err != nil {
		log.Printf("card %s: store: %v", j.post, err)
		return
	}
	for _, cid := range unpin {
		if err := w.IPFS.Unpin(cid); err != nil {
			log.Printf("unpin %s: %v", cid, err)
		}
	}
	if ok && w.Bus != nil {
		w.Bus.Emit(events.Event{Type: "post.card", ID: j.post, Author: j.author})
	}
	if ok && w.Archive != nil {
		w.Archive.Enqueue(j.post, j.author)
	}
}

// derivePictures fetches the post's IPFS links still to try — never
// tried, or unanswered with rounds left — and keeps the ones that are
// pictures: read through the guarded fetcher like a card's picture
// (never through kubo, which would hang on a CID nobody serves), sniffed,
// added to kubo under the hub's own CID and refcounted like an embed. A
// link that answered with something other than a picture is final at
// once (what a CID names never changes); one a gateway did not answer
// gets its further rounds. Either way it stays a link, and one
// post.card tells live views when any picture landed.
func (w *Worker) derivePictures(j job) {
	links := IPFSLinks(j.text)
	if len(links) == 0 {
		return
	}
	tried, err := w.St.PictureTries(j.post)
	if err != nil {
		log.Printf("pictures %s: %v", j.post, err)
		return
	}
	landed := false
	for i, link := range links {
		t := tried[link]
		if t.OK || t.Tries >= pictureTries {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		data, mime, err := w.F.FetchImage(ctx, link)
		cancel()
		cid := ""
		if err == nil {
			cid, err = w.IPFS.Add(bytes.NewReader(data), envelope.MsgID(data))
		}
		if err != nil {
			log.Printf("picture %s: %s: %v", j.post, link, err)
			tries := t.Tries + 1
			if errors.Is(err, ErrNotPicture) {
				tries = pictureTries
			}
			w.storePicture(j.post, i, link, "", 0, "", tries, false)
			continue
		}
		if w.storePicture(j.post, i, link, cid, int64(len(data)), mime, t.Tries+1, true) {
			landed = true
		}
	}
	if landed && w.Bus != nil {
		w.Bus.Emit(events.Event{Type: "post.card", ID: j.post, Author: j.author})
	}
}

func (w *Worker) storePicture(post string, idx int, link, cid string, size int64, mime string, tries int, ok bool) bool {
	unpin, err := w.St.SetPicture(post, idx, link, cid, size, mime, tries, ok)
	if err != nil {
		log.Printf("picture %s: store: %v", post, err)
		return false
	}
	for _, c := range unpin {
		if err := w.IPFS.Unpin(c); err != nil {
			log.Printf("unpin %s: %v", c, err)
		}
	}
	return ok
}
