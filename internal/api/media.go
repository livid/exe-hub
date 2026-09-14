package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"exehub/internal/identity"
	"exehub/internal/media"
	"exehub/internal/store"
)

// Media is the conversion queue behind POST /v1/media (PLAN.md, Media):
// an upload streams to a job directory, one job converts at a time, and
// its outputs are pinned like uploads, staged until a post names them.
type Media struct {
	Conv     *media.Converter
	Dir      string // job directories; emptied at start
	MaxBytes int64  // an input's size limit

	mu    sync.Mutex
	jobs  map[string]*mediaJob
	queue chan *mediaJob
	seq   int64 // numbers jobs in arrival order, for their place in the queue
}

// mediaQueue is how many jobs may wait behind the one converting.
const mediaQueue = 16

// mediaKeep is how long a finished job stays readable.
const mediaKeep = time.Hour

func NewMedia(conv *media.Converter, dir string, maxBytes int64) (*Media, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Media{Conv: conv, Dir: dir, MaxBytes: maxBytes, jobs: map[string]*mediaJob{}, queue: make(chan *mediaJob, mediaQueue)}, nil
}

// Info is /v1/hub's "media": what this hub converts.
func (m *Media) Info() map[string]any {
	return map[string]any{
		"max_mb":      m.MaxBytes >> 20,
		"max_video_s": int(m.Conv.MaxVideo.Seconds()),
		"max_audio_s": int(m.Conv.MaxAudio.Seconds()),
		"encoder":     m.Conv.Encoder(),
	}
}

type mediaJob struct {
	ID       string     `json:"job"`
	Status   string     `json:"status"` // uploading, queued, converting, done, failed
	Progress float64    `json:"progress"`
	Error    string     `json:"error,omitempty"`
	Result   *mediaFile `json:"result,omitempty"`
	Ahead    int        `json:"ahead,omitempty"` // jobs before this one, while queued
	author   string     // profile id
	dir      string
	ended    time.Time
	seq      int64
}

// mediaFile is a job's outcome as a post embeds it.
type mediaFile struct {
	Kind     string  `json:"kind"` // video, audio, loop or picture
	CID      string  `json:"cid"`
	MIME     string  `json:"mime"`
	Size     int64   `json:"size"`
	Poster   string  `json:"poster,omitempty"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Duration float64 `json:"duration,omitempty"`
	Loop     bool    `json:"loop,omitempty"`
}

// handleMedia takes one video, sound or GIF for conversion. The upload is
// authorized like /v1/upload (the author signs the body's SHA-256); the
// key's form, time, ban and gate are checked before a byte is read, and
// the signature once the body, streamed to disk and hashed on the way, is
// in. One job per author at a time.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	m := s.Media
	if m == nil {
		writeErr(w, http.StatusNotFound, errors.New("this hub does not convert media"))
		return
	}
	pub, sig, ok := uploadHeaders(w, r)
	if !ok || !s.uploadPolicy(w, pub) {
		return
	}
	job, err := m.reserve(identity.Fingerprint(pub))
	if err != nil {
		writeErr(w, http.StatusTooManyRequests, err)
		return
	}
	drop := func() {
		os.RemoveAll(job.dir)
		m.mu.Lock()
		delete(m.jobs, job.ID)
		m.mu.Unlock()
	}
	f, err := os.Create(filepath.Join(job.dir, "in"))
	if err != nil {
		drop()
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), http.MaxBytesReader(w, r.Body, m.MaxBytes))
	f.Close()
	if err != nil {
		drop()
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("videos and sounds are capped at %d MB", m.MaxBytes>>20))
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !uploadSigned(w, r, pub, sig, hex.EncodeToString(h.Sum(nil))) {
		drop()
		return
	}
	m.mu.Lock()
	job.Status = "queued"
	m.mu.Unlock()
	select {
	case m.queue <- job:
	default:
		drop()
		writeErr(w, http.StatusServiceUnavailable, errors.New("the conversion queue is full: try again in a minute"))
		return
	}
	writeJSON(w, http.StatusAccepted, m.view(job))
}

// reserve opens a job for an author with none open, forgetting jobs that
// ended long enough ago.
func (m *Media) reserve(author string) (*mediaJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, j := range m.jobs {
		if !j.ended.IsZero() && time.Since(j.ended) > mediaKeep {
			delete(m.jobs, id)
			continue
		}
		if j.author == author && j.ended.IsZero() {
			return nil, errors.New("one conversion at a time: wait for the one in progress")
		}
	}
	b := make([]byte, 12)
	rand.Read(b)
	id := hex.EncodeToString(b)
	dir := filepath.Join(m.Dir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	m.seq++
	j := &mediaJob{ID: id, Status: "uploading", author: author, dir: dir, seq: m.seq}
	m.jobs[id] = j
	return j, nil
}

// view copies a job for JSON, with its place in the queue.
func (m *Media) view(j *mediaJob) mediaJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := *j
	if v.Status == "queued" {
		for _, o := range m.jobs {
			if o.seq < j.seq && o.ended.IsZero() && o.Status != "uploading" {
				v.Ahead++
			}
		}
	}
	return v
}

// handleMediaJob reports a job: its status, progress while converting,
// and the file to embed once done. Public like every read; a job id is
// 96 random bits.
func (s *Server) handleMediaJob(w http.ResponseWriter, r *http.Request) {
	if s.Media == nil {
		writeErr(w, http.StatusNotFound, errors.New("this hub does not convert media"))
		return
	}
	s.Media.mu.Lock()
	j := s.Media.jobs[r.PathValue("job")]
	s.Media.mu.Unlock()
	if j == nil {
		writeErr(w, http.StatusNotFound, errors.New("no such job (they are kept an hour)"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.Media.view(j))
}

// RunMedia converts the queue, one job at a time.
func (s *Server) RunMedia() {
	for j := range s.Media.queue {
		s.convert(j)
	}
}

func (s *Server) convert(j *mediaJob) {
	m := s.Media
	set := func(f func()) {
		m.mu.Lock()
		f()
		m.mu.Unlock()
	}
	set(func() { j.Status = "converting" })
	start := time.Now()
	res, err := m.Conv.Convert(context.Background(), filepath.Join(j.dir, "in"), j.dir, func(p float64) {
		set(func() { j.Progress = p * 0.95 })
	})
	var out *mediaFile
	if err == nil {
		out, err = s.pinMedia(res)
	}
	os.RemoveAll(j.dir)
	set(func() {
		j.ended = time.Now()
		if err != nil {
			j.Status, j.Error = "failed", err.Error()
			return
		}
		j.Status, j.Progress, j.Result = "done", 1, out
	})
	if err != nil {
		log.Printf("media %s: failed after %s: %v", j.ID, time.Since(start).Round(time.Millisecond), err)
		return
	}
	log.Printf("media %s: %s %dx%d %.1fs, %d bytes, in %s", j.ID, out.Kind, out.Width, out.Height, out.Duration, out.Size, time.Since(start).Round(time.Millisecond))
}

// pinMedia adds a conversion's outputs to IPFS: the poster as a plain
// upload, the file with the facts a post must repeat. Both stay staged
// (refs 0) until a post names them, and are swept a day later if none
// does.
func (s *Server) pinMedia(res *media.Result) (*mediaFile, error) {
	add := func(path string) (string, int64, error) {
		f, err := os.Open(path)
		if err != nil {
			return "", 0, err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return "", 0, err
		}
		cid, err := s.IPFS.Add(f, filepath.Base(path))
		return cid, st.Size(), err
	}
	out := &mediaFile{Kind: res.Kind, MIME: res.MIME, Width: res.Width, Height: res.Height, Duration: res.Duration, Loop: res.Kind == media.Loop}
	var err error
	if res.Poster != "" {
		cid, size, err := add(res.Poster)
		if err != nil {
			return nil, err
		}
		if err := s.St.AddPin(cid, size, res.PosterMIME, false); err != nil {
			return nil, err
		}
		out.Poster = cid
	}
	if out.CID, out.Size, err = add(res.File); err != nil {
		return nil, err
	}
	if res.Kind == media.Picture {
		// a still GIF is a picture like any upload; it has no facts to keep
		out.Width, out.Height = 0, 0
		return out, s.St.AddPin(out.CID, out.Size, res.MIME, false)
	}
	return out, s.St.AddMediaPin(out.CID, out.Size, res.MIME, store.Facts{
		Poster: out.Poster, Width: out.Width, Height: out.Height, Duration: out.Duration, Loop: out.Loop})
}
