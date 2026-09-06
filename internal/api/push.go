package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"exehub/internal/push"
)

// Web Push for every new post (PLAN.md "Notifications"): the browser
// subscribes on the public page and the hub pushes to it. Subscribing is
// anonymous like reading; an endpoint is a secret the browser minted, so
// knowing it is owning it, and unsubscribing needs nothing more.

//go:embed sw.js
var swJS []byte

// pushMax bounds the notifier's work per post.
const pushMax = 10000

func (s *Server) handleSW(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(swJS)
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		writeErr(w, http.StatusNotFound, errors.New("push is not enabled on this hub"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sub, err := push.ParseSubscription(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if n, err := s.St.PushCount(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	} else if n >= pushMax {
		writeErr(w, http.StatusTooManyRequests, errors.New("this hub has all the subscribers it can notify"))
		return
	}
	sub.Base = webBase(r) // the VAPID subject: the hub as the subscriber reached it
	if err := s.St.PushAdd(sub); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		writeErr(w, http.StatusNotFound, errors.New("push is not enabled on this hub"))
		return
	}
	var in struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil || in.Endpoint == "" {
		writeErr(w, http.StatusBadRequest, errors.New("endpoint is required"))
		return
	}
	if err := s.St.PushRemove(in.Endpoint); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
