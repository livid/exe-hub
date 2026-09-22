package lang

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"exehub/internal/events"
	"exehub/internal/store"
)

// A post gets at most maxTries answers that are no answer, retryEvery
// apart. A pass that could not reach Ollama comes again after downEvery;
// one that did, after retryEvery or at the next wake, whichever is first.
const (
	maxTries   = 3
	retryEvery = time.Hour
	downEvery  = 5 * time.Minute
	downAfter  = 3
	// how much of its worklist a pass reads at a time. Naming is a second
	// a post; a translation is a minute or more, and the list is newest
	// first, so the translator reads it again every few: a post that
	// arrives while history is being worked through is next but a few.
	page   = 64
	trPage = 4
)

// Posts is the language worker's slice of the store.
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
	St Posts
	M  *Model
	// Named, when set, is told each time a post got its language: the
	// translator's wake.
	Named func()
	wake  chan struct{}
}

func NewWorker(st Posts, m *Model) *Worker {
	return &Worker{St: st, M: m, wake: make(chan struct{}, 1)}
}

// Wake says a post has arrived. It never blocks; a wake during a pass
// is kept for one more.
func (w *Worker) Wake() { wake(w.wake) }

func (w *Worker) Run() { run(w.wake, 20*time.Second, w.pass) }

func wake(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// run is a worker's life: a pause for the daemon to settle, then a pass
// at every wake, and without one after retryEvery — or downEvery, when
// the last pass found Ollama away.
func run(wake chan struct{}, settle time.Duration, pass func() bool) {
	time.Sleep(settle)
	for {
		wait := retryEvery
		if !pass() {
			wait = downEvery
		}
		select {
		case <-wake:
		case <-time.After(wait):
		}
	}
}

// What became of one piece of a pass's work.
type outcome int

const (
	wrote    outcome = iota // a row was written: an answer kept, or a try spent
	noAnswer                // Ollama gave none: nothing recorded, stepped over
	stop                    // the store failed: the pass ends
)

// drain is one pass over a worklist kept in the store: list is asked for
// a page of what is still owed, newest first, until it has nothing new.
// Work Ollama gave no answer for has nothing recorded against it and is
// stepped over until the next pass, so a post the server always refuses
// cannot hold up the ones behind it; downAfter of them in a row is an
// Ollama that is away, and the pass ends, false, to come again soon.
func drain[T any](size int, list func(limit int) ([]T, error), key func(T) string, do func(T) outcome) (up bool) {
	return drainN(1, size, list, key, do)
}

// drainN is drain with up to n pieces of work in flight at once (the
// translator's, see PLAN.md, Translations — Japanese): a page is worked
// with n in flight at all times, the next piece begun as one comes
// back, and the outcomes are judged in the order listed, so the
// step-over and the three-misses rule read as they do one at a time;
// n of 1 is exactly that. A pass that ends early, on a store failure or
// an Ollama that is away, begins nothing more and waits for what is in
// flight, whose rows are written like any other — so an Ollama that is
// away may be asked up to n times more than the three that said so.
func drainN[T any](n, size int, list func(limit int) ([]T, error), key func(T) string, do func(T) outcome) (up bool) {
	n = max(n, 1)
	skip := map[string]bool{}
	missed := 0
	for {
		work, err := list(max(size, n) + len(skip))
		if err != nil {
			log.Printf("lang: %v", err)
			return true
		}
		todo := work[:0:0]
		for _, t := range work {
			if !skip[key(t)] {
				todo = append(todo, t)
			}
		}
		if len(todo) == 0 { // nothing left but what was stepped over
			return true
		}
		// judge is what one outcome does to the pass: ended says it is
		// over, and up whether Ollama was there
		judge := func(i int, r outcome) (ended, up bool) {
			switch r {
			case stop:
				return true, true
			case noAnswer:
				skip[key(todo[i])] = true
				if missed++; missed >= downAfter {
					return true, false
				}
			default:
				missed = 0
			}
			return false, false
		}
		if n == 1 { // one at a time: each judged before the next is begun
			for i := range todo {
				if ended, up := judge(i, do(todo[i])); ended {
					return up
				}
			}
			continue
		}
		// the pool: n in flight, the next begun as one comes back, so an
		// early end may find up to n begun past the piece that ended it
		results := make([]chan outcome, len(todo))
		for i := range results {
			results[i] = make(chan outcome, 1)
		}
		var wg sync.WaitGroup
		quit, pooled := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(pooled)
			slots := make(chan struct{}, n)
			for i := range todo {
				select {
				case slots <- struct{}{}:
				case <-quit:
					return
				}
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					defer func() { <-slots }()
					results[i] <- do(todo[i])
				}(i)
			}
		}()
		// the pool starts nothing more, then what is in flight lands
		end := func(up bool) bool { close(quit); <-pooled; wg.Wait(); return up }
		for i := range todo {
			if ended, up := judge(i, <-results[i]); ended {
				return end(up)
			}
		}
		<-pooled
		wg.Wait()
	}
}

// pass names every post still to name.
func (w *Worker) pass() (up bool) {
	named := map[string]int{}
	defer func() {
		if len(named) > 0 {
			log.Printf("lang: %s", tally(named, "post named", "posts named"))
		}
	}()
	return drain(page,
		func(limit int) ([]store.CardPost, error) {
			return w.St.PostsWithoutLang(maxTries, time.Now().Add(-retryEvery).UnixMilli(), limit)
		},
		func(p store.CardPost) string { return p.ID },
		func(p store.CardPost) outcome {
			tag, model, err := w.name(p.Text)
			if err != nil {
				log.Printf("lang %s: %v", p.ID, err)
				if !errors.Is(err, ErrAnswer) {
					return noAnswer
				}
			}
			if err := w.St.SetLang(p.ID, tag, model, err == nil); err != nil {
				log.Printf("lang %s: store: %v", p.ID, err)
				return stop
			}
			if err == nil {
				named[tag]++
				if w.Named != nil {
					w.Named()
				}
			}
			return wrote
		})
}

// name is the post's tag and who said it: the model, or nobody for a
// post with no words to read.
func (w *Worker) name(text string) (tag, model string, err error) {
	if Wordless(text) {
		return NoWords, "", nil
	}
	tag, err = w.M.Detect(context.Background(), text)
	return tag, w.M.Name, err
}

// Owed is the translator's slice of the store.
type Owed interface {
	PostsToTranslate(targets []string, maxTries int, before int64, limit int) ([]store.OwedTranslation, error)
	SetTranslation(post, lang, text, model string, ok bool) error
	KeptTranslations(lang string) ([]store.KeptTranslation, error)
	RewriteTranslation(post, lang, text string) error
}

// Translator puts every post into the languages the hub keeps it in
// (see PLAN.md, Translations), the way Worker names them: the
// translations table is its queue, a pass takes what is still owed,
// newest first, so the posts on the first page are read in the reader's
// language first and history follows. Worker wakes it as posts get
// their languages. A translation at full thought is a minute or more,
// so a hub's history is hours of passes; whatever lands is announced on
// the bus as post.translation, so a live page brings it in.
type Translator struct {
	St  Owed
	M   *Model
	Bus *events.Broadcaster
	// Parallel is how many translations are in flight at once; 0 or 1
	// is one at a time. A backfill of a new language over a hub's
	// history (Japanese, 2026-09-22: a thousand posts at two to four
	// minutes each) is days alone and hours a few abreast.
	Parallel int
	wake     chan struct{}
}

func NewTranslator(st Owed, m *Model, bus *events.Broadcaster) *Translator {
	return &Translator{St: st, M: m, Bus: bus, wake: make(chan struct{}, 1)}
}

func (t *Translator) Wake() { wake(t.wake) }

func (t *Translator) Run() {
	t.tidy()
	run(t.wake, 40*time.Second, t.pass) // after the languages' first pass has begun
}

// tidy runs the punctuation rule (Tidy: FullWidth for Chinese,
// FullWidthJa for Japanese) over the translations kept before it, once
// at start: a translation is rewritten only when the rule changes it
// and it still passes Check, so after the first start this finds
// nothing to do.
func (t *Translator) tidy() {
	n := 0
	for _, to := range Targets {
		if Tidy(to, "a:b") == "a:b" && Tidy(to, "中:文") == "中:文" && Tidy(to, "か:な") == "か:な" {
			continue // a language the rule leaves alone
		}
		kept, err := t.St.KeptTranslations(to)
		if err != nil {
			log.Printf("translate: tidy: %v", err)
			return
		}
		for _, k := range kept {
			set := Tidy(to, k.Text)
			if set == k.Text || Check(k.Source, set, to) != nil {
				continue
			}
			if err := t.St.RewriteTranslation(k.Post, to, set); err != nil {
				log.Printf("translate: tidy %s: %v", k.Post, err)
				return
			}
			n++
		}
	}
	if n > 0 {
		log.Printf("translate: punctuation set full-width in %d kept translations", n)
	}
}

// logEvery is how many translations a long pass keeps between the lines
// it logs: history's pass is hours, and says how far it is.
const logEvery = 50

func (t *Translator) pass() (up bool) {
	kept, n := map[string]int{}, 0
	var mu sync.Mutex // kept and n, written by as many as are in flight
	say := func() {
		if len(kept) > 0 {
			log.Printf("translate: %s", tally(kept, "translation kept", "translations kept"))
		}
	}
	defer say()
	return drainN(t.Parallel, trPage,
		func(limit int) ([]store.OwedTranslation, error) {
			return t.St.PostsToTranslate(Targets, maxTries, time.Now().Add(-retryEvery).UnixMilli(), limit)
		},
		func(o store.OwedTranslation) string { return o.ID + " " + o.To },
		func(o store.OwedTranslation) outcome {
			out, err := t.M.Translate(context.Background(), o.Text, o.From, o.To, o.Note)
			if err != nil {
				log.Printf("translate %s to %s: %v", o.ID, o.To, err)
				if !errors.Is(err, ErrAnswer) {
					return noAnswer
				}
			}
			if err := t.St.SetTranslation(o.ID, o.To, out, t.M.Name, err == nil); err != nil {
				log.Printf("translate %s to %s: store: %v", o.ID, o.To, err)
				return stop
			}
			if err == nil {
				mu.Lock()
				kept[o.To]++
				if n++; n%logEvery == 0 {
					say()
				}
				mu.Unlock()
				if t.Bus != nil {
					t.Bus.Emit(events.Event{Type: "post.translation", ID: o.ID, Author: o.Author})
				}
			}
			return wrote
		})
}

// tally is a pass's log line: "12 posts named: en 7, zh-Hans 4, ja 1".
func tally(count map[string]int, one, many string) string {
	tags := make([]string, 0, len(count))
	n := 0
	for t, c := range count {
		tags = append(tags, t)
		n += c
	}
	sort.Slice(tags, func(i, j int) bool {
		if count[tags[i]] != count[tags[j]] {
			return count[tags[i]] > count[tags[j]]
		}
		return tags[i] < tags[j]
	})
	parts := make([]string, len(tags))
	for i, t := range tags {
		parts[i] = fmt.Sprintf("%s %d", t, count[t])
	}
	what := many
	if n == 1 {
		what = one
	}
	return fmt.Sprintf("%d %s: %s", n, what, strings.Join(parts, ", "))
}
