// exe-hub: a signed-message social feed daemon. See PLAN.md — the single
// source of truth for every design decision here.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // stats.timezone on a host without zoneinfo

	stats "github.com/livid/exe-stats"

	"exehub/internal/api"
	"exehub/internal/card"
	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/events"
	"exehub/internal/gate"
	"exehub/internal/identity"
	"exehub/internal/ipfs"
	"exehub/internal/lang"
	"exehub/internal/media"
	"exehub/internal/push"
	"exehub/internal/replicate"
	"exehub/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	stateDir := flag.String("state", "", "state directory (default ~/.exe-hub)")
	sig := flag.String("s", "", `send a signal to the running daemon: "reload" re-reads config (nginx-style; editing the file alone changes nothing)`)
	redo := flag.String("retranslate", "", "forget one post's kept translations so the running daemon makes them again: the post's id, or the first 12 characters or more of it")
	redoTo := flag.String("to", "", "with -retranslate: only the translation into this language (zh-Hans, en)")
	redoNote := flag.String("note", "", "with -retranslate: an editor's note on the post for its translator, kept with the post — what a terse or ambiguous line means")
	flag.Parse()

	if *stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatal(err)
		}
		*stateDir = filepath.Join(home, ".exe-hub")
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		log.Fatal(err)
	}
	pidPath := filepath.Join(*stateDir, "exe-hub.pid")

	if *redo != "" {
		if err := retranslate(filepath.Join(*stateDir, "hub.db"), *redo, *redoTo, *redoNote); err != nil {
			log.Fatal(err)
		}
		// the reload signal also wakes the language workers; with no
		// daemon up the translations are simply owed at its next start
		if pid, err := signalDaemon(pidPath); err != nil {
			fmt.Printf("no running daemon (%v): owed at its next start\n", err)
		} else {
			fmt.Printf("woke the daemon (SIGHUP to %d)\n", pid)
		}
		return
	}

	if *sig != "" {
		if *sig != "reload" {
			log.Fatalf("-s %q: only \"reload\" is supported", *sig)
		}
		pid, err := signalDaemon(pidPath)
		if err != nil {
			log.Fatalf("no running daemon? %v", err)
		}
		fmt.Printf("sent SIGHUP to %d\n", pid)
		return
	}

	if err := serve(*cfgPath, *stateDir, pidPath); err != nil {
		log.Fatal(err)
	}
}

// signalDaemon sends the running daemon SIGHUP: re-read the config, and
// look again at what the language workers owe.
func signalDaemon(pidPath string) (int, error) {
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("bad pidfile: %w", err)
	}
	return pid, syscall.Kill(pid, syscall.SIGHUP)
}

// retranslate forgets what one post was put into — every language, or
// the one named — so the translator owes it again (PLAN.md,
// Translations: one translation again). The shape check cannot judge
// words; this is for the translation a reader found wrong. A model that
// misread a terse line is likely to misread it again, so the reader can
// say what it means: the note is kept with the post and given to the
// translator with it from then on. It opens the database beside the
// running daemon, which SQLite allows, and until the new translation
// lands the pages show the post as written.
func retranslate(dbPath, post, to, note string) error {
	if _, err := os.Stat(dbPath); err != nil {
		return err
	}
	if to != "" && !slices.Contains(lang.Targets, to) {
		return fmt.Errorf("-to %q: the hub translates into %s", to, strings.Join(lang.Targets, " and "))
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	id := strings.ToLower(strings.TrimSpace(post))
	if len(id) != 64 {
		if id, err = st.ResolvePrefix(id); err != nil {
			return fmt.Errorf("%s: %w", post, err)
		}
	} else if _, err := st.Post(id); err != nil {
		return fmt.Errorf("%s: %w", post, err)
	}
	if note = strings.TrimSpace(note); note != "" {
		if len(note) > 1000 {
			return errors.New("-note: a line or two, 1000 bytes at most")
		}
		if err := st.SetTranslationNote(id, note); err != nil {
			return err
		}
	}
	n, err := st.DropTranslations(id, to)
	if err != nil {
		return err
	}
	what, s := "every language", "s"
	if to != "" {
		what = to
	}
	if n == 1 {
		s = ""
	}
	fmt.Printf("%s: forgot %d translation%s (%s)\n", id, n, s, what)
	if note != "" {
		fmt.Println("the translator will be given the note with this post")
	}
	return nil
}

func serve(cfgPath, stateDir, pidPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	holder := config.NewHolder(cfg)

	hub, err := identity.Load(stateDir)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(stateDir, "hub.db"))
	if err != nil {
		return err
	}
	st.PageAuthor = func(id string) bool { return holder.Get().IsAdmin(id) } // an admin's HTML is a page (PLAN.md, Pages)
	defer st.Close()

	ipfsc := ipfs.New(cfg.IPFSAPI)
	if err := ipfsc.Available(); err != nil {
		log.Printf("ipfs api %s unreachable (%v) — uploads and embeds return 503 until it appears", cfg.IPFSAPI, err)
	}

	// Live events: every committed post.create/post.delete/profile.set —
	// direct or replicated — fans out to /v1/events subscribers.
	bus := events.New()
	// Link cards and linked pictures: a bare-link post gets its first
	// link unfurled, and any post its IPFS links fetched as pictures, off
	// the ingest path (the OnMessage hook sees direct and replicated posts
	// both); the backfill gives the posts from before each feature their
	// cards and pictures once, and the sweep retries the pictures a
	// gateway did not serve. Each card's page also gets a copy in the
	// Internet Archive, found or saved; its sweep retries and covers
	// older cards.
	cards := card.NewWorker(st, ipfsc, bus)
	cards.Archive = card.NewArchiver(st, bus)
	go cards.Archive.Run()
	go cards.Archive.Sweep()
	go cards.Run()
	go cards.Backfill()
	go cards.Sweep()
	// Post language and translations: with an ollama block in the config,
	// a model names the language of every post and puts it into the two
	// the hub's readers read (PLAN.md, Post language and Translations).
	// Each table is its worker's queue, so a first pass is the backfill,
	// and an Ollama that is away only makes the posts wait.
	var langs *lang.Worker
	wakeLangs := func() {} // what the reload signal also does: see below
	if o := cfg.Ollama; o != nil {
		model := lang.NewModel(o.BaseURL, o.APIKey, o.Model, o.Effort)
		does := "names each post's language"
		if o.Translates() {
			does += " and translates it into " + strings.Join(lang.Targets, " and ")
		}
		if err := model.Available(); err != nil {
			log.Printf("lang: ollama %s unreachable (%v) — posts wait for it", o.BaseURL, err)
		} else {
			log.Printf("lang: %s at %s %s (think=%s)", o.Model, o.BaseURL, does, o.Effort)
		}
		langs = lang.NewWorker(st, model)
		wakeLangs = langs.Wake
		if o.Translates() {
			tr := lang.NewTranslator(st, model, bus)
			langs.Named = tr.Wake
			wakeLangs = func() { langs.Wake(); tr.Wake() }
			go tr.Run()
		}
		go langs.Run()
	}
	st.OnMessage = func(e *envelope.Envelope, op any, id string) {
		switch o := op.(type) {
		case *envelope.PostCreate:
			bus.Emit(events.Event{Type: "post.create", ID: id, ReplyTo: o.ReplyTo, Author: e.ProfileID()})
			// a card is for a bare link; a post already showing something needs none
			cards.Enqueue(id, e.ProfileID(), o.Text, len(o.Embeds) == 0)
			if langs != nil {
				langs.Wake()
			}
		case *envelope.PostDelete:
			bus.Emit(events.Event{Type: "post.delete", ID: o.Post, Author: e.ProfileID()})
		case *envelope.ProfileSet:
			bus.Emit(events.Event{Type: "profile.set", ID: id, Author: e.ProfileID()})
		}
	}

	// Web Push: every post.create on the bus goes to every subscriber.
	pk, err := push.LoadKey(stateDir)
	if err != nil {
		return err
	}
	go (&push.Notifier{St: st, Bus: bus, Sender: &push.Sender{Key: pk}}).Run()

	srv := &api.Server{Cfg: holder, St: st, Gate: gate.New(holder), IPFS: ipfsc, Hub: hub, Events: bus, Push: pk}
	if m := cfg.Media; m != nil {
		// a test encode or two settles NVENC and the Vulkan filters, so
		// a GPU-less hub converts on the CPU from its first job
		if conv, err := media.New(m.FFmpeg, m.FFprobe, m.Encoder); err != nil {
			log.Printf("media: %v — /v1/media stays off", err)
		} else {
			conv.MaxVideo = time.Duration(m.MaxVideo) * time.Second
			conv.MaxAudio = time.Duration(m.MaxAudio) * time.Second
			conv.Log = log.Printf
			if srv.Media, err = api.NewMedia(conv, filepath.Join(stateDir, "media"), int64(m.MaxMB)<<20); err != nil {
				return err
			}
			go srv.RunMedia()
			log.Printf("media: converting with %s (%s), inputs to %d MB", conv.Encoder(), conv.FFmpeg, m.MaxMB)
		}
	}
	// Stats: the pages' own analytics (PLAN.md, Stats) — page views
	// counted as they are served, kept for the retention, /stats to read.
	if cfg.StatsOn() {
		an, err := stats.New(st.DB(), stats.Options{
			Location:  cfg.Stats.Location,
			PathLabel: srv.StatsPathLabel,
			Title:     "Stats",
			// the agent guide is read with curl as much as with a
			// browser, and the desk's close box goes back to the feed
			AnyClientKinds: []string{"skill"},
			HomeURL:        "/",
			HomeLabel:      "Back to the feed",
		})
		if err != nil {
			return err
		}
		go an.Run()
		srv.Stats = an
		kept := "forever"
		if cfg.Stats.Retention > 0 {
			kept = fmt.Sprintf("%d days", cfg.Stats.Retention)
			go func() {
				for {
					if n, err := srv.Stats.Sweep(time.Now().AddDate(0, 0, -cfg.Stats.Retention)); err != nil {
						log.Printf("stats sweep: %v", err)
					} else if n > 0 {
						log.Printf("stats: dropped %d page views older than %d days", n, cfg.Stats.Retention)
					}
					time.Sleep(24 * time.Hour)
				}
			}()
		}
		log.Printf("stats: counting page views, days in %s, kept %s", cfg.Stats.Location, kept)
	}
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler()}

	// Pull from curated peers (peer.add ops); the loop re-reads the peers
	// table each pass, so curation applies without a restart.
	go (&replicate.Puller{St: st, IPFS: ipfsc, Self: hub.ID, Bus: bus}).Run()

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return err
	}
	defer os.Remove(pidPath)

	// Config reload is manual, nginx-style: only SIGHUP re-reads the file.
	// listen/ipfs_api/state changes still need a restart.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			next, err := config.Load(cfgPath)
			if err != nil {
				log.Printf("reload: %v — keeping the running config", err)
				continue
			}
			if next.Listen != holder.Get().Listen {
				log.Printf("reload: listen changed (%s -> %s) — needs a restart, ignoring that field", holder.Get().Listen, next.Listen)
				next.Listen = holder.Get().Listen
			}
			holder.Set(next)
			log.Printf("reload: config applied (gate=%s, admins=%d, allow_replication=%v, cooldown=%ds)",
				next.Gate.Mode, len(next.Admins), next.Replicable(), next.CooldownSec())
			// what the language workers owe may have changed beside us
			// (exe-hub -retranslate), so they look again
			wakeLangs()
		}
	}()

	// Sweep staged uploads that no post ever referenced.
	go func() {
		for range time.Tick(time.Hour) {
			cids, err := st.SweepStaged(time.Now().Add(-24 * time.Hour))
			if err != nil {
				log.Printf("sweep: %v", err)
				continue
			}
			for _, cid := range cids {
				if err := ipfsc.Unpin(cid); err != nil {
					log.Printf("sweep unpin %s: %v", cid, err)
				}
			}
			if len(cids) > 0 {
				log.Printf("sweep: unpinned %d never-referenced uploads", len(cids))
			}
		}
	}()

	// Re-pin what kubo holds without a pin. Until 2026-09-09 Add hung up on
	// the add response early and the root pin raced the close, so a share of
	// uploads were stored but never pinned; this repairs a hub's history at
	// start and keeps watch daily in case anything else ever drifts.
	go func() {
		for {
			if reconcilePins(st, ipfsc) {
				time.Sleep(24 * time.Hour)
			} else {
				time.Sleep(10 * time.Minute)
			}
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(ctx)
		// the page views counted in the last second are written before
		// the store closes under them
		if srv.Stats != nil {
			srv.Stats.Stop()
		}
	}()

	ln, err := listenWait(cfg.Listen, 5*time.Minute)
	if err != nil {
		return err
	}
	log.Printf("exe-hub %s listening on %s (gate=%s, state=%s)", hub.ID, cfg.Listen, cfg.Gate.Mode, stateDir)
	if err := httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listenWait binds addr, retrying for up to wait while the address is on
// no interface yet: a Tailscale IP in `listen` appears only once tailscaled
// has logged in, seconds to minutes after systemd started us, and a daemon
// that gave up on it stayed down until someone noticed. Should the wait run
// out we exit non-zero and systemd's Restart= starts us again.
func listenWait(addr string, wait time.Duration) (net.Listener, error) {
	start := time.Now()
	logged := false
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			if logged {
				log.Printf("bind %s: ok after %s", addr, time.Since(start).Round(time.Second))
			}
			return ln, nil
		}
		if !errors.Is(err, syscall.EADDRNOTAVAIL) || time.Since(start) > wait {
			return nil, err
		}
		if !logged {
			logged = true
			log.Printf("bind %s: %v — retrying for up to %s", addr, err, wait)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// reconcilePins pins every upload in the pins table that kubo does not
// list as pinned; false means kubo could not be asked and the caller
// should try again soon.
func reconcilePins(st *store.Store, ipfsc *ipfs.Client) bool {
	want, err := st.PinCIDs()
	if err != nil {
		log.Printf("pins: %v", err)
		return true
	}
	have, err := ipfsc.Pinned()
	if err != nil {
		log.Printf("pins: %v", err)
		return false
	}
	missing, fixed := 0, 0
	for _, cid := range want {
		if have[cid] {
			continue
		}
		missing++
		if err := ipfsc.Pin(cid); err != nil {
			log.Printf("pins: re-pin %s: %v", cid, err)
			continue
		}
		fixed++
	}
	if missing > 0 {
		log.Printf("pins: %d of %d uploads had no kubo pin, re-pinned %d", missing, len(want), fixed)
	}
	return true
}
