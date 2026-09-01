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
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"exehub/internal/envelope"
)

var (
	ErrDuplicate = errors.New("duplicate message")
	ErrStaleSeq  = errors.New("stale seq")
	ErrNotOwner  = errors.New("not the author of that post")
	ErrNoPin     = errors.New("embed CID not uploaded to this hub")
	ErrNotAvatar = errors.New("avatar must be a CID from /v1/avatar")
	ErrNotFound  = errors.New("not found")
)

type Store struct {
	db *sql.DB
	// OnMessage, when set, is called after every successfully committed
	// ingest (direct or replicated; never for duplicates or Rebuild
	// replays) — the live-events hook. It runs on the ingesting
	// goroutine, so it must not block.
	OnMessage func(e *envelope.Envelope, op any, id string)
}

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
  cid       TEXT PRIMARY KEY,
  size      INTEGER NOT NULL,
  mime      TEXT NOT NULL,
  refs      INTEGER NOT NULL DEFAULT 0,
  is_avatar INTEGER NOT NULL DEFAULT 0,  -- minted by /v1/avatar: 128×128 PNG
  created   INTEGER NOT NULL
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
);

CREATE TABLE IF NOT EXISTS peers (
  hub   TEXT PRIMARY KEY,              -- remote hub id (key fingerprint)
  addr  TEXT NOT NULL,                 -- HTTP multiaddr
  added_by TEXT NOT NULL,              -- admin profile id
  ts    INTEGER NOT NULL
);

-- Replication runtime state, NOT derived: losing it only re-pulls from
-- zero, and content-hash dedup makes that idempotent.
CREATE TABLE IF NOT EXISTS peer_state (
  hub    TEXT PRIMARY KEY,
  pubkey TEXT NOT NULL DEFAULT '',     -- cached, fingerprint-verified
  cursor INTEGER NOT NULL DEFAULT 0    -- remote messages rowid high-water
);`)
	if err != nil {
		return err
	}
	// migrations for older databases; harmless once applied
	for _, ddl := range []string{
		`ALTER TABLE pins ADD COLUMN is_avatar INTEGER NOT NULL DEFAULT 0`,
		// '' = ingested locally, else the peer hub id it was pulled from;
		// /v1/replicate serves only '' rows, keeping aggregation one-hop
		`ALTER TABLE messages ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,
		// seqs are per-author per hub, so the same author's messages
		// replicated from another hub legitimately reuse seq numbers:
		// uniqueness holds per origin (the origin hub enforced plain
		// author+seq for everything it serves)
		`DROP INDEX IF EXISTS messages_author_seq`,
		`CREATE UNIQUE INDEX IF NOT EXISTS messages_author_seq_origin ON messages(author, seq, origin)`,
	} {
		if _, err := s.db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// Ingest applies one verified message atomically: seq check, semantic
// checks, derived-table updates, and the append to the log. The caller has
// already verified the signature and enforced policy (gate, bans, admin).
// It returns the message id and any CIDs whose refcount dropped to zero
// (the caller unpins those best-effort, outside the transaction).
func (s *Store) Ingest(raw, sig []byte, e *envelope.Envelope, op any) (id string, unpin []string, err error) {
	return s.ingest(raw, sig, e, op, "")
}

// IngestReplicated ingests a message pulled from a peer, recording that
// peer as its origin (so it is never re-served to other peers) and using
// the replay-relaxed pin checks — an embed whose mirror failed degrades to
// a local 404 rather than blocking the post.
func (s *Store) IngestReplicated(raw, sig []byte, e *envelope.Envelope, op any, origin string) (id string, unpin []string, err error) {
	return s.ingest(raw, sig, e, op, origin)
}

func (s *Store) ingest(raw, sig []byte, e *envelope.Envelope, op any, origin string) (id string, unpin []string, err error) {
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
	// Monotonic seq guards direct ingest only. Seqs are per-author per
	// hub, so a replicated message from an author who also writes here
	// legitimately reuses numbers — its replay safety is the id dedup
	// above plus the origin hub's own enforcement.
	if origin == "" {
		var last int64
		if err = tx.QueryRow(`SELECT seq FROM seqs WHERE author=?`, e.Author).Scan(&last); err != nil && err != sql.ErrNoRows {
			return "", nil, err
		}
		if e.Seq <= last {
			return "", nil, fmt.Errorf("%w: got %d, have %d", ErrStaleSeq, e.Seq, last)
		}
	}

	if unpin, err = apply(tx, id, e, op, now, origin != ""); err != nil {
		return "", nil, err
	}

	if _, err = tx.Exec(`INSERT INTO messages (id, author, seq, type, ts, received, raw, sig, origin) VALUES (?,?,?,?,?,?,?,?,?)`,
		id, e.Author, e.Seq, e.Type, e.TS, now, raw, sig, origin); err != nil {
		return "", nil, err
	}
	// MAX, not overwrite: a replicated message may carry a lower seq than
	// this hub's high-water for the author, and must never regress it.
	if _, err = tx.Exec(`INSERT INTO seqs (author, seq) VALUES (?,?) ON CONFLICT(author) DO UPDATE SET seq=MAX(seq, excluded.seq)`,
		e.Author, e.Seq); err != nil {
		return "", nil, err
	}
	if err = tx.Commit(); err != nil {
		return "", nil, err
	}
	if s.OnMessage != nil {
		s.OnMessage(e, op, id)
	}
	return id, unpin, nil
}

// apply materializes one op into the derived tables. Shared by Ingest and
// Rebuild so replay can never drift from live ingestion. replay relaxes
// pin-existence checks: a logged message passed them on the day it was
// accepted, and its pins may since have been legitimately released — the
// refcount math still lands on the live totals because increments and
// decrements are both skipped for rows that no longer exist.
func apply(tx *sql.Tx, id string, e *envelope.Envelope, op any, received int64, replay bool) (unpin []string, err error) {
	pid := e.ProfileID()
	switch v := op.(type) {
	case *envelope.ProfileSet:
		var old string
		if err := tx.QueryRow(`SELECT avatar FROM profiles WHERE id=?`, pid).Scan(&old); err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		// The avatar must be a hub-minted 128×128 PNG (an avatar-flagged
		// pin), refcounted like a post embed so GC stays honest.
		if v.Avatar != "" && v.Avatar != old {
			if !replay {
				var isAvatar int
				err := tx.QueryRow(`SELECT is_avatar FROM pins WHERE cid=?`, v.Avatar).Scan(&isAvatar)
				if err == sql.ErrNoRows {
					return nil, ErrNoPin
				}
				if err != nil {
					return nil, err
				}
				if isAvatar == 0 {
					return nil, ErrNotAvatar
				}
			}
			if _, err := tx.Exec(`UPDATE pins SET refs=refs+1 WHERE cid=?`, v.Avatar); err != nil {
				return nil, err
			}
		}
		if _, err = tx.Exec(`INSERT INTO profiles (id, pubkey, name, bio, avatar, created, updated) VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name, bio=excluded.bio, avatar=excluded.avatar, updated=excluded.updated`,
			pid, e.Author, v.Name, v.Bio, v.Avatar, received, received); err != nil {
			return nil, err
		}
		if old != "" && old != v.Avatar {
			unpin, err = dropRef(tx, old, unpin)
		}
		return unpin, err
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
			if k, _ := res.RowsAffected(); k == 0 && !replay {
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
			if unpin, err = dropRef(tx, c, unpin); err != nil {
				return nil, err
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
	case *envelope.PeerAdd:
		_, err = tx.Exec(`INSERT INTO peers (hub, addr, added_by, ts) VALUES (?,?,?,?)
			ON CONFLICT(hub) DO UPDATE SET addr=excluded.addr, added_by=excluded.added_by, ts=excluded.ts`,
			v.Hub, v.Addr, pid, received)
		return nil, err
	case *envelope.PeerRemove:
		if _, err = tx.Exec(`DELETE FROM peers WHERE hub=?`, v.Hub); err != nil {
			return nil, err
		}
		// peer_state is runtime state, not derived; a removed peer's cursor
		// and cached key must not survive a re-add under the same id.
		_, err = tx.Exec(`DELETE FROM peer_state WHERE hub=?`, v.Hub)
		return nil, err
	}
	return nil, fmt.Errorf("apply: unhandled op %T", op)
}

// dropRef decrements a pin's refcount, deleting the row and queuing the
// CID for unpinning when it hits zero.
func dropRef(tx *sql.Tx, cid string, unpin []string) ([]string, error) {
	var refs int
	if err := tx.QueryRow(`UPDATE pins SET refs=refs-1 WHERE cid=? RETURNING refs`, cid).Scan(&refs); err != nil {
		if err == sql.ErrNoRows {
			return unpin, nil // already gone; nothing to release
		}
		return nil, err
	}
	if refs <= 0 {
		if _, err := tx.Exec(`DELETE FROM pins WHERE cid=?`, cid); err != nil {
			return nil, err
		}
		unpin = append(unpin, cid)
	}
	return unpin, nil
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
	for _, t := range []string{"profiles", "posts", "embeds", "bans", "seqs", "peers"} {
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
		if _, err := apply(tx, m.id, e, op, m.received, true); err != nil {
			return fmt.Errorf("rebuild %s: %w", m.id, err)
		}
		if _, err := tx.Exec(`INSERT INTO seqs (author, seq) VALUES (?,?) ON CONFLICT(author) DO UPDATE SET seq=MAX(seq, excluded.seq)`,
			e.Author, e.Seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- reads ----

type FeedPost struct {
	ID         string           `json:"id"`
	Author     string           `json:"author"`
	AuthorName string           `json:"author_name,omitempty"`
	Avatar     string           `json:"avatar,omitempty"`
	Text       string           `json:"text"`
	ReplyTo    string           `json:"reply_to,omitempty"`
	TS         int64            `json:"ts"`
	Received   int64            `json:"received"`
	Embeds     []envelope.Embed `json:"embeds,omitempty"`
	Replies    int              `json:"replies"`
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
		return 1 << 62, "￿", nil
	}
	var recv int64
	err := s.db.QueryRow(`SELECT received FROM posts WHERE id=?`, before).Scan(&recv)
	if err == sql.ErrNoRows {
		return 0, "", ErrNotFound
	}
	return recv, before, err
}

// Feed is the aggregated timeline, newest first, keyset-paginated.
// Replies are excluded unless withReplies — they belong to their thread,
// not the home feed.
func (s *Store) Feed(before string, limit int, withReplies bool) ([]FeedPost, error) {
	recv, bid, err := s.cursor(before)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(feedQuery+`WHERE (? OR p.reply_to = '') AND (p.received, p.id) < (?, ?) ORDER BY p.received DESC, p.id DESC LIMIT ?`,
		withReplies, recv, bid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanFeed(rows)
}

// FeedNewer is the feed's other direction: up to limit posts newer than
// after, oldest-first (the ones nearest to after come first) — the
// public pages' Prev button. Callers reverse for display.
func (s *Store) FeedNewer(after string, limit int, withReplies bool) ([]FeedPost, error) {
	recv, aid, err := s.cursor(after)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(feedQuery+`WHERE (? OR p.reply_to = '') AND (p.received, p.id) > (?, ?) ORDER BY p.received, p.id LIMIT ?`,
		withReplies, recv, aid, limit)
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

// ProfileFeedNewer is ProfileFeed's other direction, like FeedNewer.
func (s *Store) ProfileFeedNewer(author, after string, limit int) ([]FeedPost, error) {
	recv, aid, err := s.cursor(after)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(feedQuery+`WHERE p.author = ? AND (p.received, p.id) > (?, ?) ORDER BY p.received, p.id LIMIT ?`,
		author, recv, aid, limit)
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

// LastPost is when the author's latest post.create was accepted (0 if
// never). It reads the append-only log, not the posts table, so deleting
// a post can't reset a cooldown.
func (s *Store) LastPost(author string) (int64, error) {
	var t sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(received) FROM messages WHERE author=? AND type='post.create'`, author).Scan(&t)
	return t.Int64, err
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

// AddPin records an upload (refs 0 until a post or profile references it).
// avatar marks a hub-minted 128×128 PNG, the only kind profile.set accepts.
func (s *Store) AddPin(cid string, size int64, mime string, avatar bool) error {
	av := 0
	if avatar {
		av = 1
	}
	// An avatar re-mint of bytes already pinned as a plain upload must end
	// up avatar-flagged, or profile.set would reject the hub's own output.
	_, err := s.db.Exec(`INSERT INTO pins (cid, size, mime, refs, is_avatar, created) VALUES (?,?,?,0,?,?)
		ON CONFLICT(cid) DO UPDATE SET is_avatar = MAX(is_avatar, excluded.is_avatar)`,
		cid, size, mime, av, time.Now().UnixMilli())
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

// ---- replication ----

// ReplMsg is one log entry as served to a pulling peer: the stored bytes,
// verbatim — the puller re-verifies the author signature itself.
type ReplMsg struct {
	Envelope []byte `json:"envelope"`
	Sig      []byte `json:"sig"`
}

// replicated content ops; moderation and peer curation are local policy
// and never leave the hub
var contentTypes = map[string]bool{"profile.set": true, "post.create": true, "post.delete": true}

// ReplicationPage returns local-origin content messages after the given
// rowid cursor. limit bounds rows scanned, not returned, so a stretch of
// non-content ops still advances next; next == after means fully drained.
func (s *Store) ReplicationPage(after int64, limit int) (msgs []ReplMsg, next int64, err error) {
	rows, err := s.db.Query(`SELECT rowid, type, raw, sig FROM messages WHERE rowid > ? AND origin = '' ORDER BY rowid LIMIT ?`,
		after, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	next = after
	msgs = []ReplMsg{}
	for rows.Next() {
		var typ string
		var m ReplMsg
		if err := rows.Scan(&next, &typ, &m.Envelope, &m.Sig); err != nil {
			return nil, 0, err
		}
		if contentTypes[typ] {
			msgs = append(msgs, m)
		}
	}
	return msgs, next, rows.Err()
}

// Peer is one replication source with its runtime state joined in.
type Peer struct {
	Hub    string `json:"hub"`
	Addr   string `json:"addr"`
	PubKey string `json:"pubkey,omitempty"` // cached; '' until first contact
	Cursor int64  `json:"cursor"`
}

func (s *Store) Peers() ([]Peer, error) {
	rows, err := s.db.Query(`SELECT p.hub, p.addr, IFNULL(ps.pubkey,''), IFNULL(ps.cursor,0)
		FROM peers p LEFT JOIN peer_state ps ON ps.hub = p.hub ORDER BY p.ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Peer{}
	for rows.Next() {
		var p Peer
		if err := rows.Scan(&p.Hub, &p.Addr, &p.PubKey, &p.Cursor); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SetPeerPubkey(hub, pubkey string) error {
	_, err := s.db.Exec(`INSERT INTO peer_state (hub, pubkey) VALUES (?,?)
		ON CONFLICT(hub) DO UPDATE SET pubkey=excluded.pubkey`, hub, pubkey)
	return err
}

func (s *Store) SetPeerCursor(hub string, cursor int64) error {
	_, err := s.db.Exec(`INSERT INTO peer_state (hub, cursor) VALUES (?,?)
		ON CONFLICT(hub) DO UPDATE SET cursor=excluded.cursor`, hub, cursor)
	return err
}

// Counts are the hub-info totals: profiles ("users") and posts currently
// live on this hub (replicated content included, deletes excluded).
func (s *Store) Counts() (profiles, posts int, err error) {
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM profiles`).Scan(&profiles); err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM posts`).Scan(&posts)
	return profiles, posts, err
}
