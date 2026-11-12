package api

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"net/http"

	stats "github.com/livid/exe-stats"
)

// Offline (PLAN.md, Offline): the service worker keeps the pages a
// reader has seen and their pictures on the device, and shows the kept
// copy when the hub cannot be reached; a page it never kept gets the
// offline page below. The worker also shows the hub's pushes (PLAN.md,
// Notifications).

//go:embed sw.js
var swSrc []byte

// webBuild names the bytes the pages are made of: the template, their
// words in every language, the chrome and the stats desk's blocks the
// template takes from exe-stats, the colours' night values (theme.go),
// and the worker. A deploy that changes
// any of them is another build. Every page says which build served it
// (its exe-hub-build meta), so a page left open can tell that the hub
// has moved on under it (web.html, the live script); the worker carries
// it as its VERSION.
var webBuild = func() string {
	h := sha256.New()
	h.Write(swSrc)
	h.Write([]byte(webHTML))
	words, _ := json.Marshal(webStrings) // map keys sorted: the same words, the same bytes
	h.Write(words)
	for _, part := range []string{stats.ChromeCSS(), stats.PopupCSS(), stats.TemplateHTML(), webTheme.scheme} {
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}()

// swJS is the worker as served, VERSION stamped with the build, so a
// deploy that changes the pages hands the browser a byte-different
// worker, which installs and keeps the offline page anew.
var swJS = bytes.ReplaceAll(swSrc, []byte("__HUB_BUILD__"), []byte(webBuild))

// handleSW serves the worker, kept by nothing on the way: a browser asks
// for it again on every check, and compares the bytes. no-store rather
// than no-cache, which says the same to a browser but not to an edge:
// Cloudflare keeps a .js that says no-cache, asks the hub again each
// time, and hands the browser its own "max-age=14400" in the header's
// place (measured on hub.v2core.com, 2026-10-11).
func (s *Server) handleSW(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(swJS)
}

// handleOffline is the page the worker shows at the address of a page it
// never kept, while the hub cannot be reached: it says so, leads back to
// the feed (kept), and loads the page it stands for once the hub answers.
// The worker fetches it as it installs, in the browser's language, and
// in a language an address asks for (?lang=) once a page there is kept.
// No page view, and nothing for a search engine.
func (s *Server) handleOffline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	rd := webReadingOf(r)
	s.webRender(w, r, http.StatusOK, &webData{Page: "offline", Title: rd.L.T("offline.title") + " · " + r.Host,
		Message: rd.L.T("err.offline"), Lang: rd.Lang, Q: rd.Q})
}
