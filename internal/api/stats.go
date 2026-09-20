package api

// The pages' own analytics: the counting, the report and the desk at
// /stats all live in github.com/livid/exe-stats, which any Go server can
// use. The hub keeps its two tables in hub.db beside its posts and hands
// the package the three things only the hub knows: where a day begins,
// what its paths are called, and which handlers serve pages. The rest of
// this file is that wiring; PLAN.md's Stats section is the behaviour.

import (
	"errors"
	"net/http"
	"strings"
)

// counted wraps a page handler so a served page is recorded as a hit.
// Without stats the handler is returned as it is.
func (s *Server) counted(kind string, h http.HandlerFunc) http.HandlerFunc {
	if s.Stats == nil {
		return h
	}
	return s.Stats.Counted(kind, h)
}

// StatsPathLabel names a page in the lists and the Live window: a thread
// by its author and first words, a profile by its name, the feed and the
// rest by their path. It is what the hub lends the stats package.
func (s *Server) StatsPathLabel(path string) string {
	switch {
	case path == "/":
		return "/ (the feed)"
	case strings.HasPrefix(path, "/p/"):
		if p, err := s.St.Post(strings.TrimPrefix(path, "/p/")); err == nil {
			return authorLabel(*p) + ": " + excerpt(named(*p), 48)
		}
	case strings.HasPrefix(path, "/u/"):
		if pr, err := s.St.Profile(strings.TrimPrefix(path, "/u/")); err == nil && pr.Name != "" {
			return "Profile: " + pr.Name
		}
	}
	return path
}

// handleStatsPage renders the desk inside the hub's own chrome: the
// package builds the view model, the hub's template set draws it.
func (s *Server) handleStatsPage(w http.ResponseWriter, r *http.Request) {
	if s.Stats == nil {
		s.webError(w, r, http.StatusNotFound, "No stats on this hub.")
		return
	}
	p, err := s.Stats.PageData(r)
	if err != nil {
		s.webError(w, r, http.StatusInternalServerError, "The stats could not be read.")
		return
	}
	d := &webData{Page: "stats", Title: "Stats · " + r.Host,
		Desc:      "who reads " + r.Host + ": pages, sources, locations, devices",
		StatsPage: p, Image: webBase(r) + "/apple-touch-icon.png"}
	w.Header().Set("Cache-Control", "no-cache")
	s.webRender(w, r, http.StatusOK, d)
}

// handleStats is the same report as JSON, under the same query.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.Stats == nil {
		writeErr(w, http.StatusNotFound, errors.New("no stats on this hub"))
		return
	}
	s.Stats.ServeJSON(w, r)
}
