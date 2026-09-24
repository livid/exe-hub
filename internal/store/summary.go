package store

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// ---- thread summaries (see PLAN.md, Thread summaries) ----

// OwedSummary is one piece of the summariser's work: a root post, the
// language it is written in, and a step of the ladder its thread has
// reached that has no summary yet.
type OwedSummary struct {
	ID, Author, Lang string
	Step, Replies    int // the step owed, and how many replies the tree holds now
}

// PostsToSummarize lists what the summariser still owes, the threads
// touched most recently first and a thread's steps lowest first: every
// root with words and a language whose tree has as many replies as a
// step of the ladder, for each such step with no summary written from
// the thread — never tried, or an answer that was none, tries left and
// the last one before (unix ms). One walk over every thread counts
// them all.
func (s *Store) PostsToSummarize(steps []int, maxTries int, before int64, limit int) ([]OwedSummary, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(steps)+3)
	for _, n := range steps {
		args = append(args, n)
	}
	args = append(args, maxTries, before, limit)
	rows, err := s.db.Query(`WITH RECURSIVE ladder(step) AS (VALUES `+strings.TrimSuffix(strings.Repeat("(?),", len(steps)), ",")+`),
		tree(root, id) AS (
			SELECT r.id, c.id FROM posts r JOIN posts c ON c.reply_to = r.id WHERE r.reply_to = ''
			UNION ALL
			SELECT t.root, c.id FROM posts c JOIN tree t ON c.reply_to = t.id),
		counted(root, n) AS (SELECT root, COUNT(*) FROM tree GROUP BY root)
		SELECT id, author, lang, step, n FROM (
			SELECT p.id, p.author, p.activity, s.step, c.n,
			-- a post with no words (a picture) is summarised in the language most of its replies are in
			CASE WHEN l.lang = 'zxx' THEN IFNULL((SELECT rl.lang FROM tree t2 JOIN langs rl ON rl.post = t2.id AND rl.status = 'ok'
				AND rl.lang NOT IN ('', 'zxx', 'und') WHERE t2.root = p.id GROUP BY rl.lang ORDER BY COUNT(*) DESC, rl.lang LIMIT 1), '')
			ELSE l.lang END AS lang
			FROM counted c
			JOIN posts p ON p.id = c.root
			JOIN langs l ON l.post = p.id AND l.status = 'ok' AND l.lang NOT IN ('', 'und')
			JOIN ladder s ON s.step <= c.n
			LEFT JOIN summaries m ON m.post = p.id AND m.step = s.step AND m.src = ''
			WHERE m.post IS NULL OR (m.status = 'failed' AND m.tries < ? AND m.ts < ?))
		WHERE lang <> ''
		ORDER BY activity DESC, id DESC, step LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwedSummary
	for rows.Next() {
		var o OwedSummary
		if err := rows.Scan(&o.ID, &o.Author, &o.Lang, &o.Step, &o.Replies); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetSummary records a thread's summary at a step, written by model
// from the first `replies` replies in lang, the post's own language,
// citing the replies in cites ([#n] to the reply's id) — or, with ok
// false, one more answer that was none. It says whether it wrote: a
// root deleted while the model read gets no row, and neither does a
// summary citing a reply that has left the thread meanwhile (deleted,
// or under a deleted parent, which stays in posts but falls out of the
// walk) — checked in the transaction that would keep it (Codex's
// catch, 2026-09-24), the tries left alone since nothing failed.
func (s *Store) SetSummary(post string, step int, lang, text, model string, replies int, cites map[int]string, ok bool) (bool, error) {
	status, c := "ok", ""
	if !ok {
		status, text = "failed", ""
	} else if len(cites) > 0 {
		b, err := json.Marshal(cites)
		if err != nil {
			return false, err
		}
		c = string(b)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if root, err := rootOf(tx, post); err != nil || root != post {
		return false, err // gone, or no root
	}
	for _, id := range cites {
		if root, err := rootOf(tx, id); err != nil || root != post {
			return false, err // the cited reply has left the thread
		}
	}
	var rev int64
	if ok {
		if rev, err = nextRev(tx); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO summaries (post, step, lang, src, text, model, replies, cites, status, tries, ts, origin, rev)
		VALUES (?,?,?,'',?,?,?,?,?,1,?,'',?)
		ON CONFLICT(post, step, lang) DO UPDATE SET text=excluded.text, model=excluded.model, replies=excluded.replies,
		cites=excluded.cites, status=excluded.status, tries=summaries.tries+1, ts=excluded.ts, origin='', rev=excluded.rev`,
		post, step, lang, text, model, replies, c, status, time.Now().UnixMilli(), rev); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Summary is one kept summary of a thread.
type Summary struct {
	Post    string
	Step    int
	Lang    string // the language it is in
	Src     string // "" for the one written from the thread, else the language it was translated from
	Text    string
	Model   string
	Replies int            // how many replies it read
	Cites   map[int]string // [#n] in the text to the reply it points at
	TS      int64
}

// Summaries is what a thread has, newest step first and within a step
// the one written from the thread before its translations.
func (s *Store) Summaries(post string) ([]Summary, error) {
	rows, err := s.db.Query(`SELECT post, step, lang, src, text, model, replies, cites, ts FROM summaries
		WHERE post = ? AND status = 'ok' ORDER BY step DESC, src, lang`, post)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var m Summary
		var cites string
		if err := rows.Scan(&m.Post, &m.Step, &m.Lang, &m.Src, &m.Text, &m.Model, &m.Replies, &cites, &m.TS); err != nil {
			return nil, err
		}
		if cites != "" {
			if err := json.Unmarshal([]byte(cites), &m.Cites); err != nil {
				return nil, err
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DropSummaries forgets a thread's summaries at a step, the newest step
// for 0 — every language, tries and all — so the summariser owes it
// again (exe-hub -resummarize). It returns how many rows went.
func (s *Store) DropSummaries(post string, step int) (int64, error) {
	if step == 0 {
		err := s.db.QueryRow(`SELECT IFNULL(MAX(step), 0) FROM summaries WHERE post = ?`, post).Scan(&step)
		if err != nil && err != sql.ErrNoRows {
			return 0, err
		}
	}
	res, err := s.db.Exec(`DELETE FROM summaries WHERE post = ? AND step = ?`, post, step)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// OwedSummaryTranslation is one piece of the translator's summary work:
// a thread's newest summary, the language it is in and one it is still
// to be put into, with the cites it must keep.
type OwedSummaryTranslation struct {
	Post     string
	Step     int
	Text     string
	From, To string
	Replies  int
	Cites    map[int]string
	Author   string
}

// SummariesToTranslate lists the summary translations still owed, the
// threads touched last first: each thread's newest summary written from
// the thread, for each of targets it is not in, with no translation at
// that step — never tried, or an answer that was none, tries left and
// the last one before (unix ms). An earlier step's translations stay
// with it and are neither owed nor touched.
func (s *Store) SummariesToTranslate(targets []string, maxTries int, before int64, limit int) ([]OwedSummaryTranslation, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(targets)+3)
	for _, t := range targets {
		args = append(args, t)
	}
	args = append(args, maxTries, before, limit)
	rows, err := s.db.Query(`WITH want(lang) AS (VALUES `+strings.TrimSuffix(strings.Repeat("(?),", len(targets)), ",")+`),
		newest AS (SELECT post, MAX(step) step FROM summaries WHERE src = '' AND status = 'ok' GROUP BY post)
		SELECT m.post, m.step, m.text, m.lang, w.lang, m.replies, m.cites, p.author FROM newest n
		JOIN summaries m ON m.post = n.post AND m.step = n.step AND m.src = ''
		JOIN posts p ON p.id = m.post
		JOIN want w ON w.lang <> m.lang
		LEFT JOIN summaries t ON t.post = m.post AND t.step = m.step AND t.lang = w.lang
		WHERE t.post IS NULL OR (t.status = 'failed' AND t.tries < ? AND t.ts < ?)
		ORDER BY p.activity DESC, p.id DESC, w.lang LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwedSummaryTranslation
	for rows.Next() {
		var o OwedSummaryTranslation
		var cites string
		if err := rows.Scan(&o.Post, &o.Step, &o.Text, &o.From, &o.To, &o.Replies, &cites, &o.Author); err != nil {
			return nil, err
		}
		if cites != "" {
			if err := json.Unmarshal([]byte(cites), &o.Cites); err != nil {
				return nil, err
			}
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetSummaryTranslation records a thread's summary at a step put into
// lang from src, the language it was written in, keeping the cites of
// the one it was made from — or, with ok false, one more answer that
// was none. It says whether it wrote: a root gone meanwhile, or the
// original it translates gone (a cited reply's delete took it), gets
// no row.
func (s *Store) SetSummaryTranslation(post string, step int, lang, src, text, model string, replies int, cites map[int]string, ok bool) (bool, error) {
	status, c := "ok", ""
	if !ok {
		status, text = "failed", ""
	} else if len(cites) > 0 {
		b, err := json.Marshal(cites)
		if err != nil {
			return false, err
		}
		c = string(b)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM summaries m JOIN posts p ON p.id = m.post
		WHERE m.post = ? AND m.step = ? AND m.src = '' AND m.status = 'ok'`, post, step).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	var rev int64
	if ok {
		if rev, err = nextRev(tx); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO summaries (post, step, lang, src, text, model, replies, cites, status, tries, ts, origin, rev)
		VALUES (?,?,?,?,?,?,?,?,?,1,?,'',?)
		ON CONFLICT(post, step, lang) DO UPDATE SET src=excluded.src, text=excluded.text, model=excluded.model, replies=excluded.replies,
		cites=excluded.cites, status=excluded.status, tries=summaries.tries+1, ts=excluded.ts, origin='', rev=excluded.rev`,
		post, step, lang, src, text, model, replies, c, status, time.Now().UnixMilli(), rev); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ---- one hub pays, its peers take (PLAN.md, Thread summaries) ----

// SharedSummary is a summary as one hub serves it to its peers: the one
// written from the thread (src "") or a translation of it (src the
// language it was made from), with the cites as kept.
type SharedSummary struct {
	Post    string `json:"post"`
	Step    int    `json:"step"`
	Lang    string `json:"lang"`
	Src     string `json:"src,omitempty"`
	Text    string `json:"text"`
	Model   string `json:"model"`
	Replies int    `json:"replies"`
	Cites   string `json:"cites,omitempty"` // JSON, [#n] to the reply's id
	TS      int64  `json:"ts"`              // when it was made, by the hub that made it: the newest wins
}

// SummariesPage is the summaries this hub made itself, in the order it
// kept them, after the rev a peer holds as its cursor; next is the
// cursor for the page after. One hop, like TranslationsPage.
func (s *Store) SummariesPage(after int64, limit int) (out []SharedSummary, next int64, err error) {
	rows, err := s.db.Query(`SELECT post, step, lang, src, text, model, replies, cites, ts, rev FROM summaries
		WHERE rev > ? AND origin = '' AND status = 'ok' ORDER BY rev LIMIT ?`, after, limit)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	out, next = []SharedSummary{}, after
	for rows.Next() {
		var m SharedSummary
		if err := rows.Scan(&m.Post, &m.Step, &m.Lang, &m.Src, &m.Text, &m.Model, &m.Replies, &m.Cites, &m.TS, &next); err != nil {
			return nil, after, err
		}
		out = append(out, m)
	}
	return out, next, rows.Err()
}

// AcceptSummary keeps a summary taken from a peer, already checked by
// the caller, when it is the newest this hub knows of at that post,
// step and language. It is kept as the peer's, with the peer's ts, and
// not served on. It says whether it was kept.
func (s *Store) AcceptSummary(peer string, m SharedSummary) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO summaries (post, step, lang, src, text, model, replies, cites, status, tries, ts, origin, rev)
		SELECT ?,?,?,?,?,?,?,?,'ok',0,?,?,0 WHERE EXISTS (SELECT 1 FROM posts WHERE id=?)
		ON CONFLICT(post, step, lang) DO UPDATE SET src=excluded.src, text=excluded.text, model=excluded.model, replies=excluded.replies,
		cites=excluded.cites, status='ok', ts=excluded.ts, origin=excluded.origin, rev=0
		WHERE summaries.status <> 'ok' OR summaries.ts < excluded.ts`,
		m.Post, m.Step, m.Lang, m.Src, m.Text, m.Model, m.Replies, m.Cites, m.TS, peer, m.Post)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PendingSummary is a peer's summary waiting for its thread.
type PendingSummary struct {
	Peer string
	SharedSummary
}

// SetPendingSummary sets a peer's summary aside for a thread this hub
// does not hold whole yet, the newest per key standing, and keeps the
// peer to max of them, the longest-waiting dropped first.
func (s *Store) SetPendingSummary(peer string, m SharedSummary, max int) (dropped int64, err error) {
	b, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO pending_summaries (peer, post, step, lang, payload, ts, seen) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(peer, post, step, lang) DO UPDATE SET payload=excluded.payload, ts=excluded.ts
		WHERE excluded.ts > pending_summaries.ts`,
		peer, m.Post, m.Step, m.Lang, string(b), m.TS, time.Now().UnixMilli()); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM pending_summaries WHERE peer = ? AND rowid NOT IN (
		SELECT rowid FROM pending_summaries WHERE peer = ? ORDER BY seen DESC, rowid DESC LIMIT ?)`, peer, peer, max)
	if err != nil {
		return 0, err
	}
	if dropped, err = res.RowsAffected(); err != nil {
		return 0, err
	}
	return dropped, tx.Commit()
}

// PendingSummaries is what is set aside, from every peer, the newest
// first, the ones written from a thread before their translations.
func (s *Store) PendingSummaries(limit int) ([]PendingSummary, error) {
	rows, err := s.db.Query(`SELECT peer, payload FROM pending_summaries ORDER BY ts DESC, lang LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingSummary
	for rows.Next() {
		var n PendingSummary
		var payload string
		if err := rows.Scan(&n.Peer, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &n.SharedSummary); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	// the one written from the thread before its translations, which wait for it
	sort.SliceStable(out, func(i, j int) bool { return out[i].Src == "" && out[j].Src != "" })
	return out, rows.Err()
}

// DropPendingSummary forgets one set aside: it was tried.
func (s *Store) DropPendingSummary(peer, post string, step int, lang string) error {
	_, err := s.db.Exec(`DELETE FROM pending_summaries WHERE peer = ? AND post = ? AND step = ? AND lang = ?`, peer, post, step, lang)
	return err
}

// AgePendingSummaries forgets what was set aside before a time (unix
// ms) whose thread never came whole, and says how many.
func (s *Store) AgePendingSummaries(before int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM pending_summaries WHERE seen < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetPeerSummaryCursor records how far into a peer's summaries this hub
// has read.
func (s *Store) SetPeerSummaryCursor(hub string, cursor int64) error {
	_, err := s.db.Exec(`INSERT INTO peer_state (hub, sum_cursor) VALUES (?,?)
		ON CONFLICT(hub) DO UPDATE SET sum_cursor=excluded.sum_cursor`, hub, cursor)
	return err
}
