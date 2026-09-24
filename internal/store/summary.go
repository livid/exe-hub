package store

import (
	"database/sql"
	"encoding/json"
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
		SELECT p.id, p.author, l.lang, s.step, c.n FROM counted c
		JOIN posts p ON p.id = c.root
		JOIN langs l ON l.post = p.id AND l.status = 'ok' AND l.lang NOT IN ('', 'zxx', 'und')
		JOIN ladder s ON s.step <= c.n
		LEFT JOIN summaries m ON m.post = p.id AND m.step = s.step AND m.src = ''
		WHERE m.post IS NULL OR (m.status = 'failed' AND m.tries < ? AND m.ts < ?)
		ORDER BY p.activity DESC, p.id DESC, s.step LIMIT ?`, args...)
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
// false, one more answer that was none. A root deleted meanwhile gets
// no row.
func (s *Store) SetSummary(post string, step int, lang, text, model string, replies int, cites map[int]string, ok bool) error {
	status, c := "ok", ""
	if !ok {
		status, text = "failed", ""
	} else if len(cites) > 0 {
		b, err := json.Marshal(cites)
		if err != nil {
			return err
		}
		c = string(b)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rev int64
	if ok {
		if rev, err = nextRev(tx); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO summaries (post, step, lang, src, text, model, replies, cites, status, tries, ts, origin, rev)
		SELECT ?,?,?,'',?,?,?,?,?,1,?,'',? WHERE EXISTS (SELECT 1 FROM posts WHERE id=?)
		ON CONFLICT(post, step, lang) DO UPDATE SET text=excluded.text, model=excluded.model, replies=excluded.replies,
		cites=excluded.cites, status=excluded.status, tries=summaries.tries+1, ts=excluded.ts, origin='', rev=excluded.rev`,
		post, step, lang, text, model, replies, c, status, time.Now().UnixMilli(), rev, post); err != nil {
		return err
	}
	return tx.Commit()
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
