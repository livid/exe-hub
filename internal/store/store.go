// Package store is exe-hub's SQLite layer. The messages table — raw signed
// envelopes, append-only — is the source of truth; profiles, posts, embeds,
// bans and seqs are derived indexes, rebuilt from it by Rebuild. Pins track
// IPFS refcounts and are the one table that is not purely derived (uploads
// create rows before any message references them).
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"exehub/internal/envelope"
)

var (
	ErrDuplicate = errors.New("duplicate message")
	ErrStaleSeq  = errors.New("stale seq")
	ErrNotOwner  = errors.New("not the author of that post")
	ErrNoPin     = errors.New("embed CID not uploaded to this hub")
	ErrNotFound  = errors.New("not found")
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	// One writer connection; SQLite serializes writes anyway and a single
	// conn avoids SQLITE_BUSY juggling. WAL keeps readers unblocked.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS messages (
  id       TEXT PRIMARY KEY,          -- hex sha256 of raw
  author   TEXT NOT NULL,             -- base64 pubkey
  seq      INTEGER NOT NULL,
  type     TEXT NOT NULL,
  ts       INTEGER NOT NULL,          -- client claim, ms
  received INTEGER NOT NULL,          -- hub receive time, ms
  raw      BLOB NOT NULL,
  sig      BLOB NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS messages_author_seq ON messages(author, seq);

CREATE TABLE IF NOT EXISTS profiles (
  id      TEXT PRIMARY KEY,           -- pubkey fingerprint
  pubkey  TEXT NOT NULL,
  name    TEXT NOT NULL,
  bio     TEXT NOT NULL DEFAULT '',
  avatar  TEXT NOT NULL DEFAULT '',
  created INTEGER NOT NULL,
  updated INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS posts (
  id       TEXT PRIMARY KEY,          -- the post.create message id
  author   TEXT NOT NULL,             -- profile id
  text     TEXT NOT NULL,
  reply_to TEXT NOT NULL DEFAULT '',
  ts       INTEGER NOT NULL,
  received INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS posts_author ON posts(author, received);
CREATE INDEX IF NOT EXISTS posts_reply ON posts(reply_to, received);

CREATE TABLE IF NOT EXISTS embeds (
  post     TEXT NOT NULL,
  idx      INTEGER NOT NULL,
  cid      TEXT NOT NULL,
  mime     TEXT NOT NULL,
  filename TEXT NOT NULL DEFAULT '',
  alt      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (post, idx)
);

CREATE TABLE IF NOT EXISTS pins (
  cid     TEXT PRIMARY KEY,
  size    INTEGER NOT NULL,
  mime    TEXT NOT NULL,
  refs    INTEGER NOT NULL DEFAULT 0,
  created INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS bans (
  target TEXT PRIMARY KEY,            -- profile id
  reason TEXT NOT NULL DEFAULT '',
  by     TEXT NOT NULL,               -- admin profile id
  ts     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS seqs (
  author TEXT PRIMARY KEY,
  seq    INTEGER NOT NULL
);`)
	return err
}

// Ingest applies one verified message atomically: seq check, semantic
// checks, derived-table updates, and the append to the log. The caller has
// already verified the signature and enforced policy (gate, bans, admin).
// It returns the message id and any CIDs whose refcount dropped to zero
// (the caller unpins those best-effort, outside the transaction).
func (s *Store) Ingest(raw, sig []byte, e *envelope.Envelope, op any) (id string, unpin []string, err error) {
	id = envelope.MsgID(raw)
	now := time.Now().UnixMilli()

	tx, err := s.db.Begin()
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()

	var n int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=?`, id).Scan(&n); err != nil {
		return "", nil, err
	}
	if n > 0 {
		return id, nil, ErrDuplicate
	}
	var last int64
	if err = tx.QueryRow(`SELECT seq FROM seqs WHERE author=?`, e.Author).Scan(&last); err != nil && err != sql.ErrNoRows {
		return "", nil, err
	}
	if e.Seq <= last {
		return "", nil, fmt.Errorf("%w: got %d, have %d", ErrStaleSeq, e.Seq, last)
	}

	if unpin, err = apply(tx, id, e, op, now); err != nil {
		return "", nil, err
	}

	if _, err = tx.Exec(`INSERT INTO messages (id, author, seq, type, ts, received, raw, sig) VALUES (?,?,?,?,?,?,?,?)`,
		id, e.Author, e.Seq, e.Type, e.TS, now, raw, sig); err != nil {
		return "", nil, err
	}
	if _, err = tx.Exec(`INSERT INTO seqs (author, seq) VALUES (?,?) ON CONFLICT(author) DO UPDATE SET seq=excluded.seq`,
		e.Author, e.Seq); err != nil {
		return "", nil, err
	}
	return id, unpin, tx.Commit()
}

// apply materializes one op into the derived tables. Shared by Ingest and
// Rebuild so replay can never drift from live ingestion.
func apply(tx *sql.Tx, id string, e *envelope.Envelope, op any, received int64) (unpin []string, err error) {
	pid := e.ProfileID()
	switch v := op.(type) {
	case *envelope.ProfileSet:
		_, err = tx.Exec(`INSERT INTO profiles (id, pubkey, name, bio, avatar, created, updated) VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name, bio=excluded.bio, avatar=excluded.avatar, updated=excluded.updated`,
			pid, e.Author, v.Name, v.Bio, v.Avatar, received, received)
		return nil, err
	case *envelope.PostCreate:
		if _, err = tx.Exec(`INSERT INTO posts (id, author, text, reply_to, ts, received) VALUES (?,?,?,?,?,?)`,
			id, pid, v.Text, v.ReplyTo, e.TS, received); err != nil {
			return nil, err
		}
		for i, em := range v.Embeds {
			// v1 is hub-mediated upload only: the CID must already be
			// pinned here. The refcount keeps garbage collection honest.
			res, err := tx.Exec(`UPDATE pins SET refs=refs+1 WHERE cid=?`, em.CID)
			if err != nil {
				return nil, err
			}
			if k, _ := res.RowsAffected(); k == 0 {
				return nil, ErrNoPin
			}
			if _, err = tx.Exec(`INSERT INTO embeds (post, idx, cid, mime, filename, alt) VALUES (?,?,?,?,?,?)`,
				id, i, em.CID, em.MIME, em.Filename, em.Alt); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case *envelope.PostDelete:
		var owner string
		err = tx.QueryRow(`SELECT author FROM posts WHERE id=?`, v.Post).Scan(&owner)
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if owner != pid {
			return nil, ErrNotOwner
		}
		rows, err := tx.Query(`SELECT cid FROM embeds WHERE post=?`, v.Post)
		if err != nil {
			return nil, err
		}
		var cids []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return nil, err
			}
			cids = append(cids, c)
		}
		rows.Close()
		for _, c := range cids {
			var refs int
			if err := tx.QueryRow(`UPDATE pins SET refs=refs-1 WHERE cid=? RETURNING refs`, c).Scan(&refs); err != nil {
				return nil, err
			}
			if refs <= 0 {
				if _, err := tx.Exec(`DELETE FROM pins WHERE cid=?`, c); err != nil {
					return nil, err
				}
				unpin = append(unpin, c)
			}
		}
		if _, err := tx.Exec(`DELETE FROM embeds WHERE post=?`, v.Post); err != nil {
			return nil, err
		}
		_, err = tx.Exec(`DELETE FROM posts WHERE id=?`, v.Post)
		return unpin, err
	case *envelope.BanSet:
		_, err = tx.Exec(`INSERT INTO bans (target, reason, by, ts) VALUES (?,?,?,?)
			ON CONFLICT(target) DO UPDATE SET reason=excluded.reason, by=excluded.by, ts=excluded.ts`,
			v.Target, v.Reason, pid, received)
		return nil, err
	case *envelope.BanLift:
		_, err = tx.Exec(`DELETE FROM bans WHERE target=?`, v.Target)
		return nil, err
	}
	return nil, fmt.Errorf("apply: unhandled op %T", op)
}

// Rebuild drops every derived table (pins excepted — uploads aren't in the
// log) and replays the message log through the same apply path as live
// ingestion. Ban/admin policy is not re-checked: a message in the log was
// accepted under the policy of its day.
func (s *Store) Rebuild() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range []string{"profiles", "posts", "embeds", "bans", "seqs"} {
		if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
			return err
		}
	}
	// Replaying post.create re-increments pin refs, so zero them first.
	if _, err := tx.Exec(`UPDATE pins SET refs=0`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, received, raw FROM messages ORDER BY rowid`)
	if err != nil {
		return err
	}
	type msg struct {
		id       string
		received int64
		raw      []byte
	}
	var msgs []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.id, &m.received, &m.raw); err != nil {
			rows.Close()
			return err
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	for _, m := range msgs {
		e, err := envelope.Parse(m.raw)
		if err != nil {
			return fmt.Errorf("rebuild %s: %w", m.id, err)
		}
		op, err := e.Op()
		if err != nil {
			return fmt.Errorf("rebuild %s: %w", m.id, err)
		}
		if _, err := apply(tx, m.id, e, op, m.received); err != nil {
			return fmt.Errorf("rebuild %s: %w", m.id, err)
		}
		if _, err := tx.Exec(`INSERT INTO seqs (author, seq) VALUES (?,?) ON CONFLICT(author) DO UPDATE SET seq=excluded.seq`,
			e.Author, e.Seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- reads ----

type FeedPost struct {
	ID         string          `json:"id"`
	Author     string          `json:"author"`
	AuthorName string          `json:"author_name,omitempty"`
	Avatar     string          `json:"avatar,omitempty"`
	Text       string          `json:"text"`
	ReplyTo    string          `json:"reply_to,omitempty"`
	TS         int64           `json:"ts"`
	Received   int64           `json:"received"`
	Embeds     []envelope.Embed `json:"embeds,omitempty"`
	Replies    int             `json:"replies"`
}

const feedQuery = `
SELECT p.id, p.author, IFNULL(pr.name,''), IFNULL(pr.avatar,''), p.text, p.reply_to, p.ts, p.received,
  (SELECT COUNT(*) FROM posts r WHERE r.reply_to = p.id)
FROM posts p LEFT JOIN profiles pr ON pr.id = p.author `

func (s *Store) scanFeed(rows *sql.Rows) ([]FeedPost, error) {
	out := []FeedPost{}
	for rows.Next() {
		var p FeedPost
		if err := rows.Scan(&p.ID, &p.Author, &p.AuthorName, &p.Avatar, &p.Text, &p.ReplyTo, &p.TS, &p.Received, &p.Replies); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		embeds, err := s.postEmbeds(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Embeds = embeds
	}
	return out, nil
}

func (s *Store) postEmbeds(post string) ([]envelope.Embed, error) {
	rows, err := s.db.Query(`SELECT cid, mime, filename, alt FROM embeds WHERE post=? ORDER BY idx`, post)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envelope.Embed
	for rows.Next() {
		var e envelope.Embed
		if err := rows.Scan(&e.CID, &e.MIME, &e.Filename, &e.Alt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// cursor resolves a post id into its keyset position; a missing/empty id
// means "from the top".
func (s *Store) cursor(before string) (int64, string, error) {
	if before == "" {
		return 1<<62, "￿", nil
	}
	var recv int64
	err := s.db.QueryRow(`SELECT received FROM posts WHERE id=?`, before).Scan(&recv)
	if err == sql.ErrNoRows {
		return 0, "", ErrNotFound
	}
	return recv, before, err
}

// Feed is the aggregated timeline, newest first, keyset-paginated.
func (s *Store) Feed(before string, limit int) ([]FeedPost, error) {
	recv, bid, err := s.cursor(before)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(feedQuery+`WHERE (p.received, p.id) < (?, ?) ORDER BY p.received DESC, p.id DESC LIMIT ?`,
		recv, bid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanFeed(rows)
}

func (s *Store) ProfileFeed(author, before string, limit int) ([]FeedPost, error) {
	recv, bid, err := s.cursor(before)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(feedQuery+`WHERE p.author = ? AND (p.received, p.id) < (?, ?) ORDER BY p.received DESC, p.id DESC LIMIT ?`,
		author, recv, bid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanFeed(rows)
}

// Post returns one post; Replies its children oldest-first (thread order).
func (s *Store) Post(id string) (*FeedPost, error) {
	rows, err := s.db.Query(feedQuery+`WHERE p.id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	posts, err := s.scanFeed(rows)
	if err != nil {
		return nil, err
	}
	if len(posts) == 0 {
		return nil, ErrNotFound
	}
	return &posts[0], nil
}

func (s *Store) Replies(id, after string, limit int) ([]FeedPost, error) {
	var recv int64
	aid := ""
	if after != "" {
		if err := s.db.QueryRow(`SELECT received FROM posts WHERE id=?`, after).Scan(&recv); err != nil {
			if err == sql.ErrNoRows {
				return nil, ErrNotFound
			}
			return nil, err
		}
		aid = after
	}
	rows, err := s.db.Query(feedQuery+`WHERE p.reply_to = ? AND (p.received, p.id) > (?, ?) ORDER BY p.received, p.id LIMIT ?`,
		id, recv, aid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanFeed(rows)
}

type Profile struct {
	ID      string `json:"id"`
	PubKey  string `json:"pubkey"`
	Name    string `json:"name"`
	Bio     string `json:"bio,omitempty"`
	Avatar  string `json:"avatar,omitempty"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	Posts   int    `json:"posts"`
}

func (s *Store) Profile(id string) (*Profile, error) {
	p := &Profile{}
	err := s.db.QueryRow(`SELECT id, pubkey, name, bio, avatar, created, updated FROM profiles WHERE id=?`, id).
		Scan(&p.ID, &p.PubKey, &p.Name, &p.Bio, &p.Avatar, &p.Created, &p.Updated)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM posts WHERE author=?`, id).Scan(&p.Posts)
	return p, err
}

// Seq is the last accepted seq for an author pubkey (0 if none) — clients
// fetch it to number their next message.
func (s *Store) Seq(author string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT seq FROM seqs WHERE author=?`, author).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

func (s *Store) Banned(profileID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM bans WHERE target=?`, profileID).Scan(&n)
	return n > 0, err
}

// ---- pins ----

type Pin struct {
	CID  string
	Size int64
	MIME string
}

// AddPin records an upload (refs 0 until a post references it).
func (s *Store) AddPin(cid string, size int64, mime string) error {
	_, err := s.db.Exec(`INSERT INTO pins (cid, size, mime, refs, created) VALUES (?,?,?,0,?)
		ON CONFLICT(cid) DO NOTHING`, cid, size, mime, time.Now().UnixMilli())
	return err
}

func (s *Store) PinInfo(cid string) (*Pin, error) {
	p := &Pin{}
	err := s.db.QueryRow(`SELECT cid, size, mime FROM pins WHERE cid=?`, cid).Scan(&p.CID, &p.Size, &p.MIME)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return p, err
}

// SweepStaged removes never-referenced uploads older than cutoff and
// returns their CIDs for unpinning.
func (s *Store) SweepStaged(cutoff time.Time) ([]string, error) {
	rows, err := s.db.Query(`DELETE FROM pins WHERE refs=0 AND created < ? RETURNING cid`, cutoff.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cids []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cids = append(cids, c)
	}
	return cids, rows.Err()
}
