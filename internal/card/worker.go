package card

import (
	"bytes"
	"context"
	"log"
	"time"

	"exehub/internal/envelope"
	"exehub/internal/events"
	"exehub/internal/ipfs"
	"exehub/internal/store"
)

// Worker derives link cards off the ingest path: Enqueue never blocks,
// one goroutine fetches, and a card that lands is announced on the bus
// as post.card so live pages bring it in. A post is attempted once —
// success or failure is recorded — so a dead link never turns into a
// retry loop; a repost gets a fresh try.
type Worker struct {
	St    *store.Store
	IPFS  *ipfs.Client
	Bus   *events.Broadcaster
	F     *Fetcher
	queue chan job
}

type job struct{ post, author, text string }

func NewWorker(st *store.Store, ipfsc *ipfs.Client, bus *events.Broadcaster) *Worker {
	return &Worker{St: st, IPFS: ipfsc, Bus: bus, F: NewFetcher(), queue: make(chan job, 256)}
}

// Enqueue asks for a card when the post carries a link. A full queue
// drops the job — the backfill pass at next start absorbs the loss.
func (w *Worker) Enqueue(post, author, text string) {
	if First(text) == "" {
		return
	}
	select {
	case w.queue <- job{post, author, text}:
	default:
		log.Printf("card: queue full, dropping %s", post)
	}
}

func (w *Worker) Run() {
	for j := range w.queue {
		w.derive(j)
	}
}

// Backfill queues a card for every existing bare-link post that has
// none — the pass that gives history its cards after this feature (or a
// dropped queue) and costs nothing when there is nothing to do.
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
		w.Enqueue(p.ID, p.Author, p.Text)
	}
	if n > 0 {
		log.Printf("card backfill: %d posts queued", n)
	}
}

func (w *Worker) derive(j job) {
	if has, err := w.St.HasCard(j.post); err != nil || has {
		return
	}
	link := First(j.text)
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
}
