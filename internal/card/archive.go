package card

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"exehub/internal/events"
	"exehub/internal/store"
)

// Archived copies (see PLAN.md): every card's page is kept in the
// Internet Archive. The newest capture that answered 200 is a usable
// copy; with none, Save Page Now makes one. The copy's Wayback URL is
// stored beside the card and served under it.

// WaybackWeb prefixes every copy the hub serves: WaybackWeb + timestamp +
// "/" + link.
const WaybackWeb = "https://web.archive.org/web/"

// Wayback is the Internet Archive client: the CDX index for a capture,
// and Save Page Now's public form for a new one. The host is ours, not
// the post's, so it needs no dial guard.
type Wayback struct {
	Base   string // "https://web.archive.org"; tests point it at a local server
	client *http.Client
}

func NewWayback() *Wayback {
	return &Wayback{Base: "https://web.archive.org", client: &http.Client{Timeout: 60 * time.Second}}
}

var (
	// errBusy is archive.org's 429: the archiver goes quiet for a while.
	errBusy = errors.New("archive.org: too many requests")
	// errSaveFailed is a save job that ended without a copy; any other
	// error asking about a job is the line, and the job is asked again.
	errSaveFailed = errors.New("save failed")
)

// archiveLink is the link as the Archive keys it: the fragment is the
// reader's, never the server's.
func archiveLink(link string) string {
	if i := strings.IndexByte(link, '#'); i >= 0 {
		return link[:i]
	}
	return link
}

// waybackTS says s is a Wayback timestamp: 14 digits, YYYYMMDDhhmmss.
func waybackTS(s string) bool {
	if len(s) != 14 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (w *Wayback) do(req *http.Request) ([]byte, error) {
	req.Header.Set("User-Agent", "exe-hub/1 (link cards; archived copies)")
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, errBusy
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTML))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: %s", req.Method, req.URL.Path, resp.Status)
	}
	return body, nil
}

// Latest returns the newest capture of link that answered 200 as a
// Wayback URL, or "" when the Archive has none.
func (w *Wayback) Latest(ctx context.Context, link string) (string, error) {
	link = archiveLink(link)
	q := url.Values{"url": {link}, "output": {"json"}, "fl": {"timestamp"},
		"filter": {"statuscode:200"}, "limit": {"-1"}}
	req, err := http.NewRequestWithContext(ctx, "GET", w.Base+"/cdx/search/cdx?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	body, err := w.do(req)
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(body)) == 0 { // the index's answer for a link it never saw
		return "", nil
	}
	var rows [][]string // a header row, then one row per capture
	if err := json.Unmarshal(body, &rows); err != nil {
		return "", fmt.Errorf("cdx: %v", err)
	}
	if len(rows) < 2 || len(rows[len(rows)-1]) == 0 {
		return "", nil
	}
	ts := rows[len(rows)-1][0]
	if !waybackTS(ts) {
		return "", fmt.Errorf("cdx: timestamp %q", ts)
	}
	return WaybackWeb + ts + "/" + link, nil
}

var saveJob = regexp.MustCompile(`spn\.watchJob\("(spn2-[0-9a-f]+)"`)

// Save asks Save Page Now for a capture through the public form (the
// JSON API wants an account) and returns the job to ask about. Error
// pages are not saved: a copy is only worth linking when it is the page.
func (w *Wayback) Save(ctx context.Context, link string) (string, error) {
	form := url.Values{"url": {archiveLink(link)}}
	req, err := http.NewRequestWithContext(ctx, "POST", w.Base+"/save/", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body, err := w.do(req)
	if err != nil {
		return "", err
	}
	m := saveJob.FindSubmatch(body)
	if m == nil {
		return "", errors.New("save: no job in the answer")
	}
	return string(m[1]), nil
}

// Saved asks about a save job: the copy's Wayback URL once it succeeded,
// "" while it is pending, errSaveFailed when it ended without one.
func (w *Wayback) Saved(ctx context.Context, job, link string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", w.Base+"/save/status/"+url.PathEscape(job), nil)
	if err != nil {
		return "", err
	}
	body, err := w.do(req)
	if err != nil {
		return "", err
	}
	var st struct {
		Status     string `json:"status"`
		Timestamp  string `json:"timestamp"`
		StatusExt  string `json:"status_ext"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return "", fmt.Errorf("save status: %v", err)
	}
	switch st.Status {
	case "pending":
		return "", nil
	case "success":
		if !waybackTS(st.Timestamp) {
			return "", fmt.Errorf("%w: timestamp %q", errSaveFailed, st.Timestamp)
		}
		if st.HTTPStatus != 0 && st.HTTPStatus != http.StatusOK { // a capture, but not of the page
			return "", fmt.Errorf("%w: the page answered %d", errSaveFailed, st.HTTPStatus)
		}
		return WaybackWeb + st.Timestamp + "/" + archiveLink(link), nil
	}
	return "", fmt.Errorf("%w: %s %s %s", errSaveFailed, st.Status, st.StatusExt, cut(st.Message, 200))
}

// ArchiveStore is what the archiver keeps in the hub's store.
type ArchiveStore interface {
	BeginArchive(post string, maxTries int, before int64) (string, error)
	SetArchive(post, archive string) error
	CardsToArchive(maxTries int, before int64, limit int) ([]store.CardPost, error)
}

// A post gets at most archiveTries rounds — a lookup, then a save and
// its polls — an hour apart, the sweep's period.
const (
	archiveTries = 3
	archiveEvery = time.Hour
)

// Archiver gives cards their archived copies on one goroutine, pausing
// between calls to archive.org and going quiet after a 429. A round that
// has to wait — a lookup the index failed, a save still queued — goes
// back to the queue until its next step is due, so it never holds up
// the next card.
type Archiver struct {
	St      ArchiveStore
	Bus     *events.Broadcaster
	WB      *Wayback
	Gap     time.Duration   // the least pause between two calls to archive.org
	Quiet   time.Duration   // how long a 429 silences the archiver
	Retries []time.Duration // when a failed lookup is tried again; past the last, save anyway
	Polls   []time.Duration // when a save job is asked about, each after the last

	queue      chan archiveJob
	busy       map[string]bool // posts with a round in flight; Run's alone
	last, calm time.Time       // the last call, and the end of a quiet
}

// archiveJob is one round's next step: begin (no link yet), look up
// (no job yet) or poll the save job.
type archiveJob struct {
	post, author string
	link, job    string
	step         int // lookups failed, or polls made
}

func NewArchiver(st ArchiveStore, bus *events.Broadcaster) *Archiver {
	return &Archiver{St: st, Bus: bus, WB: NewWayback(),
		Gap: 5 * time.Second, Quiet: 10 * time.Minute,
		// the index answers 503 often, and a retry a little later often works
		Retries: []time.Duration{20 * time.Second, time.Minute, 3 * time.Minute},
		Polls: []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute,
			8 * time.Minute, 15 * time.Minute, 30 * time.Minute},
		queue: make(chan archiveJob, 256), busy: map[string]bool{}}
}

// Enqueue asks for a round on a post whose card just landed. A full
// queue drops it; the sweep picks it up.
func (a *Archiver) Enqueue(post, author string) {
	select {
	case a.queue <- archiveJob{post: post, author: author}:
	default:
		log.Printf("archive: queue full, dropping %s", post)
	}
}

func (a *Archiver) Run() {
	for j := range a.queue {
		switch {
		case j.link == "":
			a.begin(j)
		case j.job == "":
			a.lookup(j)
		default:
			a.poll(j)
		}
	}
}

// Sweep queues a round, hourly, for every card still without a copy
// whose last round is an hour old — the retries, and the cards from
// before this feature.
func (a *Archiver) Sweep() {
	time.Sleep(20 * time.Second) // after the card backfill has had its turn
	for {
		posts, err := a.St.CardsToArchive(archiveTries, time.Now().Add(-archiveEvery).UnixMilli(), 64)
		if err != nil {
			log.Printf("archive sweep: %v", err)
		}
		for _, p := range posts {
			a.Enqueue(p.ID, p.Author)
		}
		time.Sleep(archiveEvery)
	}
}

func (a *Archiver) begin(j archiveJob) {
	if a.busy[j.post] {
		return
	}
	link, err := a.St.BeginArchive(j.post, archiveTries, time.Now().Add(-archiveEvery).UnixMilli())
	if err != nil {
		log.Printf("archive %s: %v", j.post, err)
	}
	if link == "" {
		return
	}
	a.busy[j.post] = true
	j.link = link
	a.lookup(j)
}

func (a *Archiver) lookup(j archiveJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a.wait()
	found, err := a.WB.Latest(ctx, j.link)
	if a.failed(j.post, "lookup", err) {
		if j.step < len(a.Retries) {
			j.step++
			a.later(j, a.Retries[j.step-1])
			return
		}
		// the index stays down: a save's job names its copy without it
	} else if found != "" {
		a.land(j, found)
		return
	}
	a.wait()
	job, err := a.WB.Save(ctx, j.link)
	if a.failed(j.post, "save", err) {
		delete(a.busy, j.post)
		return
	}
	log.Printf("archive %s: saving %s (%s)", j.post, j.link, job)
	j.job, j.step = job, 0
	a.later(j, a.Polls[0])
}

func (a *Archiver) poll(j archiveJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a.wait()
	got, err := a.WB.Saved(ctx, j.job, j.link)
	if errors.Is(err, errSaveFailed) {
		a.failed(j.post, "save", err)
		delete(a.busy, j.post)
		return
	}
	a.failed(j.post, "save status", err) // the line or a 429: ask again
	if got != "" {
		a.land(j, got)
		return
	}
	if j.step++; j.step >= len(a.Polls) {
		log.Printf("archive %s: save %s still pending, next round in an hour", j.post, j.job)
		delete(a.busy, j.post)
		return
	}
	a.later(j, a.Polls[j.step])
}

// later brings a round back to the queue when its next step is due.
func (a *Archiver) later(j archiveJob, after time.Duration) {
	time.AfterFunc(after, func() { a.queue <- j })
}

// wait keeps the pause between calls, and a quiet after a 429.
func (a *Archiver) wait() {
	next := a.last.Add(a.Gap)
	if a.calm.After(next) {
		next = a.calm
	}
	if d := time.Until(next); d > 0 {
		time.Sleep(d)
	}
	a.last = time.Now()
}

func (a *Archiver) failed(post, what string, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errBusy) {
		a.calm = time.Now().Add(a.Quiet)
	}
	log.Printf("archive %s: %s: %v", post, what, err)
	return true
}

// land stores the copy, ends the round and tells live views to redraw
// the card.
func (a *Archiver) land(j archiveJob, archive string) {
	delete(a.busy, j.post)
	if err := a.St.SetArchive(j.post, archive); err != nil {
		log.Printf("archive %s: store: %v", j.post, err)
		return
	}
	log.Printf("archive %s: %s", j.post, archive)
	if a.Bus != nil {
		a.Bus.Emit(events.Event{Type: "post.card", ID: j.post, Author: j.author})
	}
}
