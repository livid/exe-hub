// exe-hub: a signed-message social feed daemon. See PLAN.md — the single
// source of truth for every design decision here.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"exehub/internal/api"
	"exehub/internal/config"
	"exehub/internal/envelope"
	"exehub/internal/events"
	"exehub/internal/gate"
	"exehub/internal/identity"
	"exehub/internal/ipfs"
	"exehub/internal/replicate"
	"exehub/internal/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	stateDir := flag.String("state", "", "state directory (default ~/.exe-hub)")
	sig := flag.String("s", "", `send a signal to the running daemon: "reload" re-reads config (nginx-style; editing the file alone changes nothing)`)
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

	if *sig != "" {
		if *sig != "reload" {
			log.Fatalf("-s %q: only \"reload\" is supported", *sig)
		}
		b, err := os.ReadFile(pidPath)
		if err != nil {
			log.Fatalf("no running daemon? %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			log.Fatalf("bad pidfile: %v", err)
		}
		if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
			log.Fatalf("signal pid %d: %v", pid, err)
		}
		fmt.Printf("sent SIGHUP to %d\n", pid)
		return
	}

	if err := serve(*cfgPath, *stateDir, pidPath); err != nil {
		log.Fatal(err)
	}
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
	defer st.Close()

	ipfsc := ipfs.New(cfg.IPFSAPI)
	if err := ipfsc.Available(); err != nil {
		log.Printf("ipfs api %s unreachable (%v) — uploads and embeds return 503 until it appears", cfg.IPFSAPI, err)
	}

	// Live events: every committed post.create/post.delete/profile.set —
	// direct or replicated — fans out to /v1/events subscribers.
	bus := events.New()
	st.OnMessage = func(e *envelope.Envelope, op any, id string) {
		switch o := op.(type) {
		case *envelope.PostCreate:
			bus.Emit(events.Event{Type: "post.create", ID: id, ReplyTo: o.ReplyTo, Author: e.ProfileID()})
		case *envelope.PostDelete:
			bus.Emit(events.Event{Type: "post.delete", ID: o.Post, Author: e.ProfileID()})
		case *envelope.ProfileSet:
			bus.Emit(events.Event{Type: "profile.set", ID: id, Author: e.ProfileID()})
		}
	}

	srv := &api.Server{Cfg: holder, St: st, Gate: gate.New(holder), IPFS: ipfsc, Hub: hub, Events: bus}
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler()}

	// Pull from curated peers (peer.add ops); the loop re-reads the peers
	// table each pass, so curation applies without a restart.
	go (&replicate.Puller{St: st, IPFS: ipfsc, Self: hub.ID}).Run()

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

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(ctx)
	}()

	log.Printf("exe-hub %s listening on %s (gate=%s, state=%s)", hub.ID, cfg.Listen, cfg.Gate.Mode, stateDir)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
