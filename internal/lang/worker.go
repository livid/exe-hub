package lang

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"exehub/internal/store"
)

// A post gets at most maxTries answers that are no tag, retryEvery apart.
// A pass that could not reach Ollama comes again after downEvery; one
// that did, after retryEvery or at the next post, whichever is first.
const (
	maxTries   = 3
	retryEvery = time.Hour
	downEvery  = 5 * time.Minute
	downAfter  = 3
	page       = 64
)

// Posts is the worker's slice of the store.
type Posts interface {
	PostsWithoutLang(maxTries int, before int64, limit int) ([]store.CardPost, error)
	SetLang(post, lang, model string, ok bool) error
}

// Worker names the language of every post that has none, off the ingest
// path (see PLAN.md, Post language). The langs table is its queue: Wake
// only says there is work, and a pass reads what is left to do from the
// store — so the posts from before this feature, the ones that arrived
// while Ollama was away and the one just posted are all the same work,
// and a restart loses none of it.
type Worker struct {
	St   Posts
	D    *Detector
	wake chan struct{}
}

func NewWorker(st Posts, d *Detector) *Worker {
	return &Worker{St: st, D: d, wake: make(chan struct{}, 1)}
}

// Wake says a post has arrived. It never blocks; a wake during a pass
// is kept for one more.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) Run() {
	time.Sleep(20 * time.Second) // let the daemon settle first
	for {
		wait := retryEvery
		if !w.pass() {
			wait = downEvery
		}
		select {
		case <-w.wake:
		case <-time.After(wait):
		}
	}
}

// pass names every post still to name, a page at a time, newest first.
// A post Ollama gave no answer for has nothing recorded against it and
// is stepped over until the next pass, so one the server always refuses
// cannot hold up the posts behind it; downAfter of them in a row is an
// Ollama that is away, and the pass ends, false, to come again soon.
func (w *Worker) pass() (up bool) {
	named := map[string]int{}
	defer func() {
		if len(named) > 0 {
			log.Printf("lang: %s", tally(named))
		}
	}()
	skip := map[string]bool{}
	missed := 0
	for {
		posts, err := w.St.PostsWithoutLang(maxTries, time.Now().Add(-retryEvery).UnixMilli(), page+len(skip))
		if err != nil {
			log.Printf("lang: %v", err)
			return true
		}
		wrote := false
		for _, p := range posts {
			if skip[p.ID] {
				continue
			}
			tag, model, err := w.name(p.Text)
			if err != nil && !errors.Is(err, ErrAnswer) {
				log.Printf("lang %s: %v", p.ID, err)
				skip[p.ID] = true
				if missed++; missed >= downAfter {
					return false
				}
				continue
			}
			missed = 0
			if err != nil {
				log.Printf("lang %s: %v", p.ID, err)
			} else {
				named[tag]++
			}
			if err := w.St.SetLang(p.ID, tag, model, err == nil); err != nil {
				log.Printf("lang %s: store: %v", p.ID, err)
				return true
			}
			wrote = true
		}
		if !wrote { // nothing left but what was stepped over
			return true
		}
	}
}

// name is the post's tag and who said it: the model, or nobody for a
// post with no words to read.
func (w *Worker) name(text string) (tag, model string, err error) {
	if Wordless(text) {
		return NoWords, "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tag, err = w.D.Detect(ctx, text)
	return tag, w.D.Model, err
}

// tally is a pass's one log line: "12 posts named: en 7, zh-Hans 4, ja 1".
func tally(named map[string]int) string {
	tags := make([]string, 0, len(named))
	n := 0
	for t, c := range named {
		tags = append(tags, t)
		n += c
	}
	sort.Slice(tags, func(i, j int) bool {
		if named[tags[i]] != named[tags[j]] {
			return named[tags[i]] > named[tags[j]]
		}
		return tags[i] < tags[j]
	})
	parts := make([]string, len(tags))
	for i, t := range tags {
		parts[i] = fmt.Sprintf("%s %d", t, named[t])
	}
	s := "posts"
	if n == 1 {
		s = "post"
	}
	return fmt.Sprintf("%d %s named: %s", n, s, strings.Join(parts, ", "))
}
