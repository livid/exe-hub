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
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"exehub/internal/envelope"
	"exehub/internal/mention"
)

var (
	ErrDuplicate = errors.New("duplicate message")
	ErrStaleSeq  = errors.New("stale seq")
	ErrNotOwner  = errors.New("not the author of that post")
	ErrNoPin     = errors.New("embed CID not uploaded to this hub")
	ErrFacts     = errors.New("embed player facts differ from the conversion's")
	ErrNotAvatar = errors.New("avatar must be a CID from /v1/avatar")
	ErrNotFound  = errors.New("not found")
	ErrAmbiguous = errors.New("more than one post begins that way")
)

type Store struct {
	db *sql.DB
	// OnMessage, when set, is called after every successfully committed
	// ingest (direct or replicated; never for duplicates or Rebuild
	// replays) — the live-events hook. It runs on the ingesting
	// goroutine, so it must not block.
	OnMessage func(e *envelope.Envelope, op any, id string)
	// PageAuthor, when set, says whose HTML embeds are pages — read in a
	// sandboxed window instead of downloaded (PLAN.md, Pages). It is asked
	// at read time, so a demoted key's pages turn back into files.
	PageAuthor func(author string) bool
}

// DB is the open database, for a package that keeps tables of its own
// in it (the stats desk keeps hits and hits_salt there). The store's own
// tables are not its to touch.
func (s *Store) DB() *sql.DB { return s.db }

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
  id         TEXT PRIMARY KEY,        -- the post.create message id
  author     TEXT NOT NULL,           -- profile id
  text       TEXT NOT NULL,
  reply_to   TEXT NOT NULL DEFAULT '',
  ts         INTEGER NOT NULL,
  received   INTEGER NOT NULL,
  activity   INTEGER NOT NULL DEFAULT 0, -- the thread's last touch (roots; see Feed)
  last_reply TEXT NOT NULL DEFAULT ''    -- the newest reply in the tree (roots)
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
CREATE TABLE IF NOT EXISTS push_subs (
  endpoint TEXT PRIMARY KEY,           -- the push service URL the browser minted; a secret
  p256dh   BLOB NOT NULL,              -- the browser's P-256 public key, 65 bytes
  auth     BLOB NOT NULL,              -- the browser's 16-byte auth secret
  base     TEXT NOT NULL DEFAULT '',   -- the hub as the subscriber reached it: the VAPID subject
  created  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS peer_state (
  hub    TEXT PRIMARY KEY,
  pubkey TEXT NOT NULL DEFAULT '',     -- cached, fingerprint-verified
  cursor INTEGER NOT NULL DEFAULT 0    -- remote messages rowid high-water
);

-- Link cards are derived from external fetches, not from the log (like
-- pins, they cannot be rebuilt by replay); a failed row records the
-- attempt so a dead link is never refetched in a loop.
CREATE TABLE IF NOT EXISTS cards (
  post   TEXT PRIMARY KEY,             -- the post the card sits under
  url    TEXT NOT NULL,                -- the link as posted
  host   TEXT NOT NULL DEFAULT '',
  title  TEXT NOT NULL DEFAULT '',
  descr  TEXT NOT NULL DEFAULT '',
  image  TEXT NOT NULL DEFAULT '',     -- pinned CID, refcounted like an embed
  status TEXT NOT NULL,                -- 'ok' | 'failed'
  ts     INTEGER NOT NULL
);

-- Linked pictures (see PLAN.md): the pictures a post's IPFS links name,
-- fetched by the hub and kept under its own CID. Derived like cards, so
-- outside the envelope and not rebuilt from the log; a failed row
-- counts its tries, since a gateway may not answer the first time.
CREATE TABLE IF NOT EXISTS pictures (
  post   TEXT NOT NULL,                -- the post whose text links it
  url    TEXT NOT NULL,                -- the IPFS link as posted
  idx    INTEGER NOT NULL,             -- its place among the post's IPFS links
  cid    TEXT NOT NULL DEFAULT '',     -- the hub's copy, refcounted like an embed
  mime   TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,                -- 'ok' | 'failed'
  tries  INTEGER NOT NULL DEFAULT 0,
  ts     INTEGER NOT NULL,
  PRIMARY KEY (post, url)
);

-- A post's natural language (see PLAN.md, Post language), named by a
-- model: derived like cards, so outside the envelope and not rebuilt
-- from the log. A failed row counts the answers that were no tag; an
-- Ollama that did not answer leaves no row at all.
CREATE TABLE IF NOT EXISTS langs (
  post   TEXT PRIMARY KEY,
  lang   TEXT NOT NULL DEFAULT '',     -- BCP 47: en, ja, zh-Hans; zxx = no words, und = could not tell
  model  TEXT NOT NULL DEFAULT '',     -- who named it; '' = nobody asked (a post without words)
  status TEXT NOT NULL,                -- 'ok' | 'failed'
  tries  INTEGER NOT NULL DEFAULT 0,
  ts     INTEGER NOT NULL
);

-- A post put into a language its readers read (see PLAN.md,
-- Translations), by the same model and kept the same way: beside the
-- post, outside the envelope, not rebuilt from the log. lang is the
-- language it was put into, never the post's own.
CREATE TABLE IF NOT EXISTS translations (
  post   TEXT NOT NULL,
  lang   TEXT NOT NULL,                -- one of lang.Targets: zh-Hans, en, ja
  text   TEXT NOT NULL DEFAULT '',
  model  TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,                -- 'ok' | 'failed'
  tries  INTEGER NOT NULL DEFAULT 0,
  ts     INTEGER NOT NULL,
  PRIMARY KEY (post, lang)
);

-- An editor's note on a post, for its translator (exe-hub -retranslate
-- -note): what a terse or ambiguous line means. The hub's operator
-- writes it, not the model and not the author, so it is neither derived
-- nor signed; it stays with the post across Rebuild and goes with it.
CREATE TABLE IF NOT EXISTS translation_notes (
  post TEXT PRIMARY KEY,
  note TEXT NOT NULL,
  ts   INTEGER NOT NULL
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
		// a card's archived copy (see PLAN.md, Archived copies): the
		// Wayback URL once found or saved, and the rounds spent on it
		`ALTER TABLE cards ADD COLUMN archive TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE cards ADD COLUMN archive_tries INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE cards ADD COLUMN archive_ts INTEGER NOT NULL DEFAULT 0`,
		// an embed's player facts (PLAN.md, Media), as its post was signed
		`ALTER TABLE embeds ADD COLUMN poster TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE embeds ADD COLUMN width INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE embeds ADD COLUMN height INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE embeds ADD COLUMN duration REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE embeds ADD COLUMN loop INTEGER NOT NULL DEFAULT 0`,
		// what a /v1/media conversion measured for its output (media=1),
		// which a post naming that file must repeat
		`ALTER TABLE pins ADD COLUMN media INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE pins ADD COLUMN poster TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE pins ADD COLUMN width INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE pins ADD COLUMN height INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE pins ADD COLUMN duration REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE pins ADD COLUMN loop INTEGER NOT NULL DEFAULT 0`,
		// a thread's last touch and its newest reply, kept on the root
		// (see Feed: a reply bumps its thread in the home feed)
		`ALTER TABLE posts ADD COLUMN activity INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE posts ADD COLUMN last_reply TEXT NOT NULL DEFAULT ''`,
		// translations ride aggregation (PLAN.md, Translations — one hub
		// pays): origin is '' for one this hub made, else the peer it was
		// taken from; rev numbers the ones it made as it keeps them, the
		// cursor peers page by (0 = not served: a taken one, a failed try)
		`ALTER TABLE translations ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE translations ADD COLUMN rev INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS translations_rev ON translations(rev) WHERE rev > 0`,
		// where the revs come from: AUTOINCREMENT never gives a number
		// twice, even after its row is gone, and the table is kept empty.
		// MAX(rev)+1 would: -retranslate deletes a row, and had it held
		// the highest rev the redone one would take that number again,
		// behind the cursor of a peer that had read that far
		`CREATE TABLE IF NOT EXISTS translation_revs (rev INTEGER PRIMARY KEY AUTOINCREMENT)`,
		// a peer's translation of a post this hub does not hold yet, set
		// aside until the post comes (PLAN.md, Translations — a translation
		// that comes before its post waits for it). ts is the peer's, the
		// newest per key standing; seen is when this hub first set it
		// aside, what it is aged and capped by. Neither derived nor
		// signed: it stays across Rebuild
		`CREATE TABLE IF NOT EXISTS pending_translations (
			peer TEXT NOT NULL, post TEXT NOT NULL, lang TEXT NOT NULL,
			text TEXT NOT NULL, model TEXT NOT NULL DEFAULT '',
			ts INTEGER NOT NULL, seen INTEGER NOT NULL,
			PRIMARY KEY (peer, post, lang))`,
		`CREATE INDEX IF NOT EXISTS pending_translations_post ON pending_translations(post)`,
		`ALTER TABLE peer_state ADD COLUMN tr_cursor INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	// a db from before the bump columns: activity 0 never occurs after
	// them (every insert writes it), so its presence means backfill
	var cold int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM posts WHERE activity = 0`).Scan(&cold); err != nil {
		return err
	}
	if cold > 0 {
		if err := s.backfillActivity(); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS posts_activity ON posts(activity) WHERE reply_to = ''`); err != nil {
		return err
	}
	if err := s.numberTranslations(); err != nil {
		return err
	}
	return nil
}

// backfillActivity gives every post its own arrival as activity, then
// replays the replies oldest-first so each thread's root ends carrying
// its newest reply and that reply's arrival — what ingest maintains
// from here on.
func (s *Store) backfillActivity() error {
	if _, err := s.db.Exec(`UPDATE posts SET activity = received`); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT id, reply_to FROM posts WHERE reply_to <> '' ORDER BY received, id`)
	if err != nil {
		return err
	}
	parent := map[string]string{}
	var order []string
	for rows.Next() {
		var id, up string
		if err := rows.Scan(&id, &up); err != nil {
			rows.Close()
			return err
		}
		parent[id] = up
		order = append(order, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range order {
		root := parent[id]
		for i := 0; i < 40; i++ {
			up, ok := parent[root]
			if !ok || up == "" {
				break
			}
			root = up
		}
		if _, err := s.db.Exec(`UPDATE posts SET activity = (SELECT received FROM posts WHERE id = ?), last_reply = ? WHERE id = ? AND reply_to = ''`,
			id, id, root); err != nil {
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
		if _, err = tx.Exec(`INSERT INTO posts (id, author, text, reply_to, ts, received, activity) VALUES (?,?,?,?,?,?,?)`,
			id, pid, v.Text, v.ReplyTo, e.TS, received, received); err != nil {
			return nil, err
		}
		// a reply bumps its thread: walk to the root and make this the
		// thread's last touch, so the home feed surfaces the thread
		// instead of burying the follow-up (a parent this hub does not
		// hold ends the walk, and nothing bumps)
		if v.ReplyTo != "" {
			root := v.ReplyTo
			for i := 0; i < 40; i++ {
				var up string
				err := tx.QueryRow(`SELECT reply_to FROM posts WHERE id=?`, root).Scan(&up)
				if err == sql.ErrNoRows {
					root = ""
					break
				}
				if err != nil {
					return nil, err
				}
				if up == "" {
					break
				}
				root = up
			}
			if root != "" {
				if _, err := tx.Exec(`UPDATE posts SET activity=?, last_reply=? WHERE id=?`, received, id, root); err != nil {
					return nil, err
				}
			}
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
			if em.Poster != "" {
				res, err := tx.Exec(`UPDATE pins SET refs=refs+1 WHERE cid=?`, em.Poster)
				if err != nil {
					return nil, err
				}
				if k, _ := res.RowsAffected(); k == 0 && !replay {
					return nil, ErrNoPin
				}
			}
			if !replay {
				if err := checkFacts(tx, em); err != nil {
					return nil, err
				}
			}
			if _, err = tx.Exec(`INSERT INTO embeds (post, idx, cid, mime, filename, alt, poster, width, height, duration, loop) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
				id, i, em.CID, em.MIME, em.Filename, em.Alt, em.Poster, em.Width, em.Height, em.Duration, em.Loop); err != nil {
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
		rows, err := tx.Query(`SELECT cid, poster FROM embeds WHERE post=?`, v.Post)
		if err != nil {
			return nil, err
		}
		var cids []string
		for rows.Next() {
			var c, poster string
			if err := rows.Scan(&c, &poster); err != nil {
				rows.Close()
				return nil, err
			}
			cids = append(cids, c)
			if poster != "" {
				cids = append(cids, poster)
			}
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
		// The post's link card goes with it, its picture's pin released.
		var cimg string
		if err := tx.QueryRow(`SELECT image FROM cards WHERE post=?`, v.Post).Scan(&cimg); err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if cimg != "" {
			if unpin, err = dropRef(tx, cimg, unpin); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(`DELETE FROM cards WHERE post=?`, v.Post); err != nil {
			return nil, err
		}
		// and its linked pictures, each copy's pin released
		rows, err = tx.Query(`SELECT cid FROM pictures WHERE post=? AND cid<>''`, v.Post)
		if err != nil {
			return nil, err
		}
		cids = cids[:0]
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
		if _, err := tx.Exec(`DELETE FROM pictures WHERE post=?`, v.Post); err != nil {
			return nil, err
		}
		// and the language it was named, and what it was put into
		if _, err := tx.Exec(`DELETE FROM langs WHERE post=?`, v.Post); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM translations WHERE post=?`, v.Post); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM translation_notes WHERE post=?`, v.Post); err != nil {
			return nil, err
		}
		// the thread whose newest reply this was: after the delete its
		// pointer moves back to the newest remaining reply in the tree
		// (or clears, the root's own arrival becoming the activity
		// again); the bump itself is left standing otherwise
		var rootID string
		if err := tx.QueryRow(`SELECT id FROM posts WHERE last_reply=?`, v.Post).Scan(&rootID); err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if _, err = tx.Exec(`DELETE FROM posts WHERE id=?`, v.Post); err != nil {
			return nil, err
		}
		if rootID != "" {
			var nid string
			var nrecv int64
			err := tx.QueryRow(`WITH RECURSIVE tree(id) AS (
				SELECT id FROM posts WHERE reply_to = ?
				UNION ALL
				SELECT p.id FROM posts p JOIN tree t ON p.reply_to = t.id
			) SELECT p.id, p.received FROM posts p JOIN tree t ON p.id = t.id ORDER BY p.received DESC, p.id DESC LIMIT 1`,
				rootID).Scan(&nid, &nrecv)
			switch {
			case err == sql.ErrNoRows:
				_, err = tx.Exec(`UPDATE posts SET last_reply='', activity=received WHERE id=?`, rootID)
			case err == nil:
				_, err = tx.Exec(`UPDATE posts SET last_reply=?, activity=? WHERE id=?`, nid, nrecv, rootID)
			}
			if err != nil {
				return nil, err
			}
		}
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

// checkFacts holds a post to what /v1/media measured: an embed naming a
// converted file either leaves the player facts out or repeats them
// exactly, so a signed post cannot give another's video a wrong shape
// or poster. Uploads without a conversion record declare their own.
func checkFacts(tx *sql.Tx, em envelope.Embed) error {
	var media, loop int
	var poster string
	var width, height int
	var duration float64
	err := tx.QueryRow(`SELECT media, poster, width, height, duration, loop FROM pins WHERE cid=?`, em.CID).
		Scan(&media, &poster, &width, &height, &duration, &loop)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil || media == 0 {
		return err
	}
	if em.Poster == "" && em.Width == 0 && em.Duration == 0 && !em.Loop {
		return nil
	}
	if em.Poster != poster || em.Width != width || em.Height != height ||
		math.Abs(em.Duration-duration) > 0.01 || em.Loop != (loop == 1) {
		return fmt.Errorf("%w: poster %q %dx%d %.3fs loop=%v", ErrFacts, poster, width, height, duration, loop == 1)
	}
	return nil
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
	// Cards survive a rebuild like pins do (they come from external
	// fetches, not the log), but replay neither recreates one for a post
	// that is gone nor re-increments its picture's pin ref — so drop the
	// orphans and put the surviving refs back.
	if _, err := tx.Exec(`DELETE FROM cards WHERE post NOT IN (SELECT id FROM posts)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE pins SET refs = refs + (SELECT COUNT(*) FROM cards WHERE cards.image = pins.cid AND cards.status='ok')`); err != nil {
		return err
	}
	// Linked pictures likewise.
	if _, err := tx.Exec(`DELETE FROM pictures WHERE post NOT IN (SELECT id FROM posts)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE pins SET refs = refs + (SELECT COUNT(*) FROM pictures WHERE pictures.cid = pins.cid AND pictures.status='ok')`); err != nil {
		return err
	}
	// The posts' languages and translations survive too, orphans dropped.
	if _, err := tx.Exec(`DELETE FROM langs WHERE post NOT IN (SELECT id FROM posts)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM translations WHERE post NOT IN (SELECT id FROM posts)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM translation_notes WHERE post NOT IN (SELECT id FROM posts)`); err != nil {
		return err
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
	Lang       string           `json:"lang,omitempty"` // the language it is written in, once named (see PLAN.md, Post language)
	ReplyTo    string           `json:"reply_to,omitempty"`
	TS         int64            `json:"ts"`
	Received   int64            `json:"received"`
	Embeds     []envelope.Embed `json:"embeds,omitempty"`
	Card       *Card            `json:"card,omitempty"`     // the first link, unfurled (see PLAN.md, Link cards)
	Pictures   []Picture        `json:"pictures,omitempty"` // the pictures its IPFS links name (see PLAN.md, Linked pictures)
	PageCIDs   []string         `json:"pages,omitempty"`    // the embeds that open as pages (see PLAN.md, Pages)
	Replies    int              `json:"replies"`
	Depth      int              `json:"depth,omitempty"` // set by Thread: steps below the root, 1 = a direct reply
	// the thread's last touch and its newest reply (roots with replies;
	// see Feed — a reply bumps its thread in the home feed)
	Activity  int64     `json:"activity,omitempty"`
	LastReply *ReplyRef `json:"last_reply,omitempty"`
	// the profiles its text mentions ("@" and a profile id), id to the
	// name each goes by today — its newest reply's too (see PLAN.md,
	// Mentions)
	Mentions map[string]string `json:"mentions,omitempty"`
}

// ReplyRef is the newest reply in a thread, as the root carries it.
type ReplyRef struct {
	ID         string `json:"id"`
	Author     string `json:"author"`
	AuthorName string `json:"author_name,omitempty"`
	Text       string `json:"text"`
	Received   int64  `json:"received"`
}

// Card is one post's link card as the feed serves it.
type Card struct {
	URL   string `json:"url"`
	Host  string `json:"host,omitempty"`
	Title string `json:"title"`
	Desc  string `json:"desc,omitempty"`
	Image string `json:"image,omitempty"` // pinned CID, served by /v1/embed
	// the page's copy in the Wayback Machine, https://web.archive.org/web/<timestamp>/<link>
	Archive string `json:"archive,omitempty"`
}

// Picture is one linked picture as the feed serves it: the IPFS link as
// posted, and the hub's copy of what it served.
type Picture struct {
	URL  string `json:"url"`
	CID  string `json:"cid"` // pinned CID, served by /v1/embed
	MIME string `json:"mime"`
}

// Name is what a viewer titles the picture: the file its link ends in,
// when the link names one (a gateway path can end in the CID alone),
// else "Picture".
func (p Picture) Name() string {
	if m := pictureFile.FindStringSubmatch(p.URL); m != nil {
		if name, err := url.PathUnescape(m[1]); err == nil {
			return name
		}
	}
	return "Picture"
}

var pictureFile = regexp.MustCompile(`(?i)/([^/?#]+\.[a-z0-9]{2,5})(?:[?#]|$)`)

// ArchiveDate is the archived copy's capture day as YYYY-MM-DD, read
// from the Wayback URL's timestamp; "" when there is no copy.
func (c Card) ArchiveDate() string {
	const prefix = "https://web.archive.org/web/"
	ts, ok := strings.CutPrefix(c.Archive, prefix)
	if !ok || len(ts) < 8 {
		return ""
	}
	for _, r := range ts[:8] {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return ts[:4] + "-" + ts[4:6] + "-" + ts[6:8]
}

const feedCols = `p.id, p.author, IFNULL(pr.name,''), IFNULL(pr.avatar,''), p.text, p.reply_to, p.ts, p.received, p.activity, p.last_reply,
  (SELECT COUNT(*) FROM posts r WHERE r.reply_to = p.id),
  IFNULL(cd.url,''), IFNULL(cd.host,''), IFNULL(cd.title,''), IFNULL(cd.descr,''), IFNULL(cd.image,''), IFNULL(cd.archive,''),
  IFNULL((SELECT lg.lang FROM langs lg WHERE lg.post = p.id AND lg.status = 'ok'),'')`

const feedQuery = `
SELECT ` + feedCols + `
FROM posts p LEFT JOIN profiles pr ON pr.id = p.author
LEFT JOIN cards cd ON cd.post = p.id AND cd.status = 'ok' `

func (s *Store) scanFeed(rows *sql.Rows) ([]FeedPost, error) {
	out := []FeedPost{}
	var lastIDs []string // the last_reply column, resolved to posts below
	for rows.Next() {
		var p FeedPost
		var c Card
		var lr string
		if err := rows.Scan(&p.ID, &p.Author, &p.AuthorName, &p.Avatar, &p.Text, &p.ReplyTo, &p.TS, &p.Received, &p.Activity, &lr, &p.Replies,
			&c.URL, &c.Host, &c.Title, &c.Desc, &c.Image, &c.Archive, &p.Lang); err != nil {
			return nil, err
		}
		if c.URL != "" {
			p.Card = &c
		}
		out = append(out, p)
		lastIDs = append(lastIDs, lr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		// a root's count is the whole conversation, not its first level:
		// the direct count from feedCols is replaced by the tree's
		if out[i].ReplyTo == "" && out[i].Replies > 0 {
			err := s.db.QueryRow(`WITH RECURSIVE tree(tid) AS (
				SELECT id FROM posts WHERE reply_to = ?
				UNION ALL
				SELECT p.id FROM posts p JOIN tree t ON p.reply_to = t.tid
			) SELECT COUNT(*) FROM tree`, out[i].ID).Scan(&out[i].Replies)
			if err != nil {
				return nil, err
			}
		}
		// a stale pointer (its post gone mid-write) simply stays absent
		if lastIDs[i] != "" {
			var r ReplyRef
			err := s.db.QueryRow(`SELECT p.id, p.author, IFNULL(pr.name,''), p.text, p.received
				FROM posts p LEFT JOIN profiles pr ON pr.id = p.author WHERE p.id=?`, lastIDs[i]).
				Scan(&r.ID, &r.Author, &r.AuthorName, &r.Text, &r.Received)
			if err != nil && err != sql.ErrNoRows {
				return nil, err
			}
			if err == nil {
				out[i].LastReply = &r
			}
		}
		embeds, err := s.postEmbeds(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Embeds = embeds
		if out[i].Pictures, err = s.postPictures(out[i].ID); err != nil {
			return nil, err
		}
		if s.PageAuthor == nil {
			continue
		}
		for _, e := range embeds {
			if strings.HasPrefix(e.MIME, "text/html") && s.PageAuthor(out[i].Author) {
				out[i].PageCIDs = append(out[i].PageCIDs, e.CID)
			}
		}
	}
	if err := s.nameMentions(out); err != nil {
		return nil, err
	}
	return out, nil
}

// nameMentions gives each post the names of the profiles its text
// mentions, as they are now: one lookup for the page, and none when no
// post holds an "@".
func (s *Store) nameMentions(posts []FeedPost) error {
	ids := make([][]string, len(posts))
	var all []string
	for i, p := range posts {
		if p.LastReply != nil {
			ids[i] = mention.IDs(p.Text, p.LastReply.Text)
		} else {
			ids[i] = mention.IDs(p.Text)
		}
		all = append(all, ids[i]...)
	}
	if len(all) == 0 {
		return nil
	}
	names, err := s.ProfileNames(all)
	if err != nil {
		return err
	}
	for i := range posts {
		for _, id := range ids[i] {
			if name := names[id]; name != "" {
				if posts[i].Mentions == nil {
					posts[i].Mentions = map[string]string{}
				}
				posts[i].Mentions[id] = name
			}
		}
	}
	return nil
}

// ProfileNames is the name each of ids goes by, for those that are
// profiles here with a name.
func (s *Store) ProfileNames(ids []string) (map[string]string, error) {
	out := map[string]string{}
	for len(ids) > 0 {
		n := min(len(ids), 500)
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		rows, err := s.db.Query(`SELECT id, name FROM profiles WHERE name != '' AND id IN (?`+strings.Repeat(",?", n-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = name
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		ids = ids[n:]
	}
	return out, nil
}

// ProfileHit is one profile a composer offers to mention.
type ProfileHit struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Avatar string `json:"avatar,omitempty"`
}

// FindProfiles is the named profiles a composer offers for what has been
// typed after an "@": those whose name holds q, or whose id begins with
// it, the ones that posted last first — whoever is in the conversation
// is who gets mentioned. An empty q is everyone, in that order. Banned
// profiles are left out. LIKE folds ASCII case and no other, as the
// search does.
func (s *Store) FindProfiles(q string, limit int) ([]ProfileHit, error) {
	esc := likeEscaper.Replace(q)
	rows, err := s.db.Query(`SELECT pr.id, pr.name, pr.avatar
		FROM profiles pr
		WHERE pr.name != '' AND (pr.name LIKE ? ESCAPE '\' OR pr.id LIKE ? ESCAPE '\')
		  AND NOT EXISTS (SELECT 1 FROM bans b WHERE b.target = pr.id)
		ORDER BY IFNULL((SELECT MAX(p.received) FROM posts p WHERE p.author = pr.id), 0) DESC, pr.updated DESC, pr.id
		LIMIT ?`, "%"+esc+"%", strings.ToLower(esc)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProfileHit{}
	for rows.Next() {
		var h ProfileHit
		if err := rows.Scan(&h.ID, &h.Name, &h.Avatar); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) postEmbeds(post string) ([]envelope.Embed, error) {
	rows, err := s.db.Query(`SELECT cid, mime, filename, alt, poster, width, height, duration, loop FROM embeds WHERE post=? ORDER BY idx`, post)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envelope.Embed
	for rows.Next() {
		var e envelope.Embed
		if err := rows.Scan(&e.CID, &e.MIME, &e.Filename, &e.Alt, &e.Poster, &e.Width, &e.Height, &e.Duration, &e.Loop); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetCard records a post's link card. An ok card's picture is pinned
// here in the same transaction (refs start at 1 — never 0, so the staged-
// upload sweep can't take it); a failed attempt is recorded so the link
// is not refetched forever. Replacing a card releases the old picture:
// the caller unpins the returned CIDs, like Ingest's.
func (s *Store) SetCard(post string, c Card, imageSize int64, imageMIME string, ok bool) (unpin []string, err error) {
	status := "ok"
	if !ok {
		status, c.Host, c.Title, c.Desc, c.Image = "failed", "", "", "", ""
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var old string
	if err := tx.QueryRow(`SELECT image FROM cards WHERE post=?`, post).Scan(&old); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if old != "" && old != c.Image {
		if unpin, err = dropRef(tx, old, unpin); err != nil {
			return nil, err
		}
	}
	if c.Image != "" && c.Image != old {
		if _, err := tx.Exec(`INSERT INTO pins (cid, size, mime, refs, is_avatar, created) VALUES (?,?,?,1,0,?)
			ON CONFLICT(cid) DO UPDATE SET refs=refs+1`, c.Image, imageSize, imageMIME, time.Now().UnixMilli()); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO cards (post, url, host, title, descr, image, status, ts) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(post) DO UPDATE SET url=excluded.url, host=excluded.host, title=excluded.title,
		descr=excluded.descr, image=excluded.image, status=excluded.status, ts=excluded.ts`,
		post, c.URL, c.Host, c.Title, c.Desc, c.Image, status, time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	return unpin, tx.Commit()
}

// HasCard says whether the post's card was already attempted, ok or not.
func (s *Store) HasCard(post string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM cards WHERE post=?`, post).Scan(&n)
	return n > 0, err
}

// CardsMisread lists the posts whose stored card the misread test flags
// (card.Misread: text that is not UTF-8), for the backfill to derive again.
func (s *Store) CardsMisread(misread func(title, desc string) bool) ([]CardPost, error) {
	rows, err := s.db.Query(`SELECT p.id, p.author, p.text, c.title, c.descr FROM cards c
		JOIN posts p ON p.id = c.post WHERE c.status = 'ok'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CardPost
	for rows.Next() {
		var p CardPost
		var title, desc string
		if err := rows.Scan(&p.ID, &p.Author, &p.Text, &title, &desc); err != nil {
			return nil, err
		}
		if misread(title, desc) {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// CardPost is one backfill candidate: a post never attempted for a card.
type CardPost struct{ ID, Author, Text string }

// PostsWithoutCards lists posts with no card attempt yet, newest first —
// the backfill's worklist. Posts with embeds are out: a card is for a
// bare link, a post already showing something needs none.
func (s *Store) PostsWithoutCards(limit int) ([]CardPost, error) {
	rows, err := s.db.Query(`SELECT p.id, p.author, p.text FROM posts p
		LEFT JOIN cards c ON c.post = p.id
		WHERE c.post IS NULL AND NOT EXISTS (SELECT 1 FROM embeds e WHERE e.post = p.id)
		ORDER BY p.received DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CardPost
	for rows.Next() {
		var p CardPost
		if err := rows.Scan(&p.ID, &p.Author, &p.Text); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- linked pictures (see PLAN.md, Linked pictures) ----

func (s *Store) postPictures(post string) ([]Picture, error) {
	rows, err := s.db.Query(`SELECT url, cid, mime FROM pictures WHERE post=? AND status='ok' ORDER BY idx`, post)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Picture
	for rows.Next() {
		var p Picture
		if err := rows.Scan(&p.URL, &p.CID, &p.MIME); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PictureTry is what became of one of a post's IPFS links so far.
type PictureTry struct {
	OK    bool
	Tries int
}

// PictureTries lists the post's IPFS links already tried, by link.
func (s *Store) PictureTries(post string) (map[string]PictureTry, error) {
	rows, err := s.db.Query(`SELECT url, status, tries FROM pictures WHERE post=?`, post)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PictureTry{}
	for rows.Next() {
		var u, status string
		var t PictureTry
		if err := rows.Scan(&u, &status, &t.Tries); err != nil {
			return nil, err
		}
		t.OK = status == "ok"
		out[u] = t
	}
	return out, rows.Err()
}

// SetPicture records a try at a post's IPFS link, with the tries it now
// stands at (the worker sets the cap for a link that will never be a
// picture, so the sweep leaves it). A picture that landed is pinned in
// the same transaction (refs start at 1, like a card's picture).
// Replacing a copy releases the old one: the caller unpins the returned
// CIDs, like Ingest's.
func (s *Store) SetPicture(post string, idx int, url, cid string, size int64, mime string, tries int, ok bool) (unpin []string, err error) {
	status := "ok"
	if !ok {
		status, cid, mime = "failed", "", ""
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var old string
	if err := tx.QueryRow(`SELECT cid FROM pictures WHERE post=? AND url=?`, post, url).Scan(&old); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if old != "" && old != cid {
		if unpin, err = dropRef(tx, old, unpin); err != nil {
			return nil, err
		}
	}
	if cid != "" && cid != old {
		if _, err := tx.Exec(`INSERT INTO pins (cid, size, mime, refs, is_avatar, created) VALUES (?,?,?,1,0,?)
			ON CONFLICT(cid) DO UPDATE SET refs=refs+1`, cid, size, mime, time.Now().UnixMilli()); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO pictures (post, url, idx, cid, mime, status, tries, ts) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(post, url) DO UPDATE SET idx=excluded.idx, cid=excluded.cid, mime=excluded.mime,
		status=excluded.status, tries=excluded.tries, ts=excluded.ts`,
		post, url, idx, cid, mime, status, tries, time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	return unpin, tx.Commit()
}

// PostsWithoutPictures lists the posts that may link pictures — their
// text mentions ipfs — and were never tried for any, newest first: the
// backfill's worklist, which the worker narrows to real IPFS links.
func (s *Store) PostsWithoutPictures(limit int) ([]CardPost, error) {
	rows, err := s.db.Query(`SELECT p.id, p.author, p.text FROM posts p
		WHERE p.text LIKE '%ipfs%' AND NOT EXISTS (SELECT 1 FROM pictures x WHERE x.post = p.id)
		ORDER BY p.received DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CardPost
	for rows.Next() {
		var p CardPost
		if err := rows.Scan(&p.ID, &p.Author, &p.Text); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PicturesToRetry lists the posts with a linked picture that failed,
// has tries left and was last tried before (unix ms) — the sweep's
// worklist, newest post first.
func (s *Store) PicturesToRetry(maxTries int, before int64, limit int) ([]CardPost, error) {
	rows, err := s.db.Query(`SELECT p.id, p.author, p.text FROM posts p
		WHERE EXISTS (SELECT 1 FROM pictures x WHERE x.post = p.id AND x.status = 'failed' AND x.tries < ? AND x.ts < ?)
		ORDER BY p.received DESC LIMIT ?`, maxTries, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CardPost
	for rows.Next() {
		var p CardPost
		if err := rows.Scan(&p.ID, &p.Author, &p.Text); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BeginArchive opens an archive round for a post (see PLAN.md, Archived
// copies): when its card is ok, has no copy yet, has rounds left and
// began none since before (unix ms), the round is counted and stamped
// now and the card's link returned; else the link is "" and there is
// nothing to do.
func (s *Store) BeginArchive(post string, maxTries int, before int64) (link string, err error) {
	err = s.db.QueryRow(`UPDATE cards SET archive_tries = archive_tries + 1, archive_ts = ?
		WHERE post = ? AND status = 'ok' AND archive = '' AND archive_tries < ? AND archive_ts < ?
		RETURNING url`, time.Now().UnixMilli(), post, maxTries, before).Scan(&link)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return link, err
}

// SetArchive records the card's archived copy.
func (s *Store) SetArchive(post, archive string) error {
	_, err := s.db.Exec(`UPDATE cards SET archive = ? WHERE post = ?`, archive, post)
	return err
}

// CardsToArchive lists the posts whose ok card still has no copy, rounds
// left, and no round begun since before (unix ms) — the sweep's worklist,
// oldest round first. Text carries the card's link.
func (s *Store) CardsToArchive(maxTries int, before int64, limit int) ([]CardPost, error) {
	rows, err := s.db.Query(`SELECT p.id, p.author, c.url FROM cards c JOIN posts p ON p.id = c.post
		WHERE c.status = 'ok' AND c.archive = '' AND c.archive_tries < ? AND c.archive_ts < ?
		ORDER BY c.archive_ts, p.received DESC LIMIT ?`, maxTries, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CardPost
	for rows.Next() {
		var p CardPost
		if err := rows.Scan(&p.ID, &p.Author, &p.Text); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- post language (see PLAN.md, Post language) ----

// SetLang records what a post was named: its BCP 47 tag and the model
// that said it (empty when nobody was asked), or, with ok false, one more
// answer that was no tag. A post deleted meanwhile gets no row.
func (s *Store) SetLang(post, lang, model string, ok bool) error {
	status := "ok"
	if !ok {
		status, lang = "failed", ""
	}
	_, err := s.db.Exec(`INSERT INTO langs (post, lang, model, status, tries, ts)
		SELECT ?,?,?,?,1,? WHERE EXISTS (SELECT 1 FROM posts WHERE id=?)
		ON CONFLICT(post) DO UPDATE SET lang=excluded.lang, model=excluded.model,
		status=excluded.status, tries=langs.tries+1, ts=excluded.ts`,
		post, lang, model, status, time.Now().UnixMilli(), post)
	return err
}

// PostLang is the post's language, empty while it has none.
func (s *Store) PostLang(post string) (string, error) {
	var lang string
	err := s.db.QueryRow(`SELECT lang FROM langs WHERE post=? AND status='ok'`, post).Scan(&lang)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return lang, err
}

// PostsWithoutLang lists the posts still to name, newest first — the
// language worker's worklist: never asked about, or answered with no tag,
// tries left and the last one before (unix ms).
func (s *Store) PostsWithoutLang(maxTries int, before int64, limit int) ([]CardPost, error) {
	rows, err := s.db.Query(`SELECT p.id, p.author, p.text FROM posts p
		LEFT JOIN langs l ON l.post = p.id
		WHERE l.post IS NULL OR (l.status = 'failed' AND l.tries < ? AND l.ts < ?)
		ORDER BY p.received DESC, p.id DESC LIMIT ?`, maxTries, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CardPost
	for rows.Next() {
		var p CardPost
		if err := rows.Scan(&p.ID, &p.Author, &p.Text); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- translations (see PLAN.md, Translations) ----

// OwedTranslation is one piece of the translator's work: a post, the
// language it is written in, one it is still to be put into, and the
// editor's note on the post, if it has one.
type OwedTranslation struct{ ID, Author, Text, From, To, Note string }

// PostsToTranslate lists what the translator still owes, newest post
// first: every post with words and a language, for each of targets it
// is not written in, that has no translation yet — never tried, or an
// answer that was none, tries left and the last one before (unix ms).
func (s *Store) PostsToTranslate(targets []string, maxTries int, before int64, limit int) ([]OwedTranslation, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(targets)+3)
	for _, t := range targets {
		args = append(args, t)
	}
	args = append(args, maxTries, before, limit)
	rows, err := s.db.Query(`WITH want(lang) AS (VALUES `+strings.TrimSuffix(strings.Repeat("(?),", len(targets)), ",")+`)
		SELECT p.id, p.author, p.text, l.lang, w.lang, IFNULL(n.note, '') FROM posts p
		JOIN langs l ON l.post = p.id AND l.status = 'ok' AND l.lang NOT IN ('', 'zxx', 'und')
		JOIN want w ON w.lang <> l.lang
		LEFT JOIN translations t ON t.post = p.id AND t.lang = w.lang
		LEFT JOIN translation_notes n ON n.post = p.id
		WHERE t.post IS NULL OR (t.status = 'failed' AND t.tries < ? AND t.ts < ?)
		ORDER BY p.received DESC, p.id DESC, w.lang LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwedTranslation
	for rows.Next() {
		var o OwedTranslation
		if err := rows.Scan(&o.ID, &o.Author, &o.Text, &o.From, &o.To, &o.Note); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetTranslation records a post put into lang, and the model that did
// it, or, with ok false, one more answer that was none. A post deleted
// meanwhile gets no row.
func (s *Store) SetTranslation(post, lang, text, model string, ok bool) error {
	status := "ok"
	if !ok {
		status, text = "failed", ""
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// a kept one is this hub's own and takes the next rev, so peers
	// paging by it see it, a redone one again; a failed try is not served
	var rev int64
	if ok {
		if rev, err = nextRev(tx); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO translations (post, lang, text, model, status, tries, ts, origin, rev)
		SELECT ?,?,?,?,?,1,?,'',? WHERE EXISTS (SELECT 1 FROM posts WHERE id=?)
		ON CONFLICT(post, lang) DO UPDATE SET text=excluded.text, model=excluded.model,
		status=excluded.status, tries=translations.tries+1, ts=excluded.ts, origin='', rev=excluded.rev`,
		post, lang, text, model, status, time.Now().UnixMilli(), rev, post); err != nil {
		return err
	}
	return tx.Commit()
}

// nextRev is the next number for a translation this hub keeps: one more
// than any it ever gave, deleted ones included.
func nextRev(tx *sql.Tx) (int64, error) {
	res, err := tx.Exec(`INSERT INTO translation_revs DEFAULT VALUES`)
	if err != nil {
		return 0, err
	}
	rev, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`DELETE FROM translation_revs`) // sqlite_sequence remembers
	return rev, err
}

// numberTranslations gives the translations this hub kept before they
// were numbered their revs, in the order they were made. Every one kept
// since has its own, so after the first start this finds nothing.
func (s *Store) numberTranslations() error {
	rows, err := s.db.Query(`SELECT post, lang FROM translations WHERE status = 'ok' AND origin = '' AND rev = 0 ORDER BY ts, post, lang`)
	if err != nil {
		return err
	}
	var keys [][2]string
	for rows.Next() {
		var k [2]string
		if err := rows.Scan(&k[0], &k[1]); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(keys) == 0 {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, k := range keys {
		rev, err := nextRev(tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE translations SET rev = ? WHERE post = ? AND lang = ?`, rev, k[0], k[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SharedTranslation is a translation as one hub serves it to its peers.
type SharedTranslation struct {
	Post  string `json:"post"`
	Lang  string `json:"lang"`
	Text  string `json:"text"`
	Model string `json:"model"`
	TS    int64  `json:"ts"` // when it was made, by the hub that made it: the newest wins
}

// TranslationsPage is the translations this hub made itself, in the
// order it kept them, after the rev a peer holds as its cursor; next is
// the cursor for the page after. One hop, like ReplicationPage: what was
// taken from a peer is not served on.
func (s *Store) TranslationsPage(after int64, limit int) (out []SharedTranslation, next int64, err error) {
	rows, err := s.db.Query(`SELECT post, lang, text, model, ts, rev FROM translations
		WHERE rev > ? AND origin = '' AND status = 'ok' ORDER BY rev LIMIT ?`, after, limit)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	out, next = []SharedTranslation{}, after
	for rows.Next() {
		var t SharedTranslation
		if err := rows.Scan(&t.Post, &t.Lang, &t.Text, &t.Model, &t.TS, &next); err != nil {
			return nil, after, err
		}
		out = append(out, t)
	}
	return out, next, rows.Err()
}

// PostText is a post's words and the language they were named, for a
// peer's translation to be checked against; ok is false for a post this
// hub does not hold.
func (s *Store) PostText(id string) (text, lang string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT p.text, IFNULL((SELECT l.lang FROM langs l WHERE l.post = p.id AND l.status = 'ok'), '')
		FROM posts p WHERE p.id = ?`, id).Scan(&text, &lang)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return text, lang, err == nil, err
}

// AcceptTranslation keeps a translation taken from a peer, already
// checked by the caller, when it is the newest this hub knows of: there
// is none kept yet, or the one kept, whoever made it, is older. It is
// kept as the peer's, with the peer's ts, and not served on. It says
// whether it was kept.
func (s *Store) AcceptTranslation(peer string, t SharedTranslation) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO translations (post, lang, text, model, status, tries, ts, origin, rev)
		SELECT ?,?,?,?,'ok',0,?,?,0 WHERE EXISTS (SELECT 1 FROM posts WHERE id=?)
		ON CONFLICT(post, lang) DO UPDATE SET text=excluded.text, model=excluded.model, status='ok',
		ts=excluded.ts, origin=excluded.origin, rev=0
		WHERE translations.status <> 'ok' OR translations.ts < excluded.ts`,
		t.Post, t.Lang, t.Text, t.Model, t.TS, peer, t.Post)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PendingTranslation is a peer's translation waiting for its post.
type PendingTranslation struct {
	Peer string
	SharedTranslation
}

// SetPendingTranslation sets a peer's translation aside for a post this
// hub does not hold, the newest per peer, post and language standing,
// and keeps the peer to max of them, the longest-waiting dropped first:
// a peer can name posts that will never come. It says how many went.
func (s *Store) SetPendingTranslation(peer string, t SharedTranslation, max int) (dropped int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO pending_translations (peer, post, lang, text, model, ts, seen) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(peer, post, lang) DO UPDATE SET text=excluded.text, model=excluded.model, ts=excluded.ts
		WHERE excluded.ts > pending_translations.ts`,
		peer, t.Post, t.Lang, t.Text, t.Model, t.TS, time.Now().UnixMilli()); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM pending_translations WHERE peer = ? AND rowid NOT IN (
		SELECT rowid FROM pending_translations WHERE peer = ? ORDER BY seen DESC, rowid DESC LIMIT ?)`, peer, peer, max)
	if err != nil {
		return 0, err
	}
	if dropped, err = res.RowsAffected(); err != nil {
		return 0, err
	}
	return dropped, tx.Commit()
}

// PendingTranslations is what is set aside for one post, from every
// peer, the newest first; with post "" it is what is set aside for any
// post this hub holds by now, however the post came.
func (s *Store) PendingTranslations(post string, limit int) ([]PendingTranslation, error) {
	q := `SELECT n.peer, n.post, n.lang, n.text, n.model, n.ts FROM pending_translations n WHERE n.post = ? ORDER BY n.ts DESC LIMIT ?`
	args := []any{post, limit}
	if post == "" {
		q = `SELECT n.peer, n.post, n.lang, n.text, n.model, n.ts FROM pending_translations n
			JOIN posts p ON p.id = n.post ORDER BY n.ts DESC LIMIT ?`
		args = []any{limit}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingTranslation
	for rows.Next() {
		var n PendingTranslation
		if err := rows.Scan(&n.Peer, &n.Post, &n.Lang, &n.Text, &n.Model, &n.TS); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DropPendingTranslation forgets one set aside: it was tried.
func (s *Store) DropPendingTranslation(peer, post, lang string) error {
	_, err := s.db.Exec(`DELETE FROM pending_translations WHERE peer = ? AND post = ? AND lang = ?`, peer, post, lang)
	return err
}

// AgePendingTranslations forgets what was set aside before a time (unix
// ms) and whose post never came, and says how many.
func (s *Store) AgePendingTranslations(before int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM pending_translations WHERE seen < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetPeerTranslationCursor records how far into a peer's translations
// this hub has read.
func (s *Store) SetPeerTranslationCursor(hub string, cursor int64) error {
	_, err := s.db.Exec(`INSERT INTO peer_state (hub, tr_cursor) VALUES (?,?)
		ON CONFLICT(hub) DO UPDATE SET tr_cursor=excluded.tr_cursor`, hub, cursor)
	return err
}

// KeptTranslation is a translation with the post it is of, for a rule
// run over what was kept before it (lang.FullWidth).
type KeptTranslation struct{ Post, Source, Text string }

// KeptTranslations is every kept translation into lang.
func (s *Store) KeptTranslations(lang string) ([]KeptTranslation, error) {
	rows, err := s.db.Query(`SELECT t.post, p.text, t.text FROM translations t JOIN posts p ON p.id = t.post
		WHERE t.lang = ? AND t.status = 'ok'`, lang)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeptTranslation
	for rows.Next() {
		var k KeptTranslation
		if err := rows.Scan(&k.Post, &k.Source, &k.Text); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RewriteTranslation replaces a kept translation's words (the
// punctuation rule, lang.Tidy): who made it and its tries stand. This
// hub's own row is given a new rev and time with them, so peers that
// took it take it again; a peer's row is set right in place.
func (s *Store) RewriteTranslation(post, lang, text string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var origin string
	switch err := tx.QueryRow(`SELECT origin FROM translations WHERE post = ? AND lang = ? AND status = 'ok'`, post, lang).Scan(&origin); {
	case errors.Is(err, sql.ErrNoRows):
		return nil // nothing kept to rewrite
	case err != nil:
		return err
	}
	if origin != "" {
		// a peer's: set right here, and left for the peer to serve again
		_, err = tx.Exec(`UPDATE translations SET text = ? WHERE post = ? AND lang = ? AND status = 'ok'`, text, post, lang)
	} else {
		// this hub's own: a new rev and time, so peers that took it take
		// it again (newest wins on their side, TranslationsPage serves by rev)
		var rev int64
		if rev, err = nextRev(tx); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE translations SET text = ?, ts = ?, rev = ? WHERE post = ? AND lang = ? AND status = 'ok'`,
			text, time.Now().UnixMilli(), rev, post, lang)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SetTranslationNote keeps the editor's note on a post, replacing the
// one before; the translator is given it with the post from then on. A
// post that is not there gets none.
func (s *Store) SetTranslationNote(post, note string) error {
	_, err := s.db.Exec(`INSERT INTO translation_notes (post, note, ts)
		SELECT ?,?,? WHERE EXISTS (SELECT 1 FROM posts WHERE id=?)
		ON CONFLICT(post) DO UPDATE SET note=excluded.note, ts=excluded.ts`,
		post, note, time.Now().UnixMilli(), post)
	return err
}

// DropTranslations forgets what a post was put into, every language or
// the one named, tries and all, so the translator owes it again
// (exe-hub -retranslate). It says how many rows went.
func (s *Store) DropTranslations(post, lang string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM translations WHERE post = ? AND (? = '' OR lang = ?)`, post, lang, lang)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Translation is a post as it reads in another language, and From the
// language it was written in.
type Translation struct{ Text, From string }

// Translations is the posts among ids that have been put into lang, by
// id — one read for a page of posts.
func (s *Store) Translations(ids []string, lang string) (map[string]Translation, error) {
	out := map[string]Translation{}
	if len(ids) == 0 || lang == "" {
		return out, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, lang)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT t.post, t.text, l.lang FROM translations t JOIN langs l ON l.post = t.post
		WHERE t.lang = ? AND t.status = 'ok' AND t.post IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var t Translation
		if err := rows.Scan(&id, &t.Text, &t.From); err != nil {
			return nil, err
		}
		out[id] = t
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

// Feed is the aggregated timeline, keyset-paginated. Without replies —
// the home feed — it follows activity: a reply bumps its thread's root,
// so an answered thread stands where its newest reply happened instead
// of burying it (the root carries that reply as LastReply). With
// replies the order stays arrival, oldest cursor semantics unchanged —
// pollers walk it to miss nothing.
func (s *Store) Feed(before string, limit int, withReplies bool) ([]FeedPost, error) {
	if withReplies {
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
	act, bid, err := s.activityCursor(before)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(feedQuery+`WHERE p.reply_to = '' AND (p.activity, p.id) < (?, ?) ORDER BY p.activity DESC, p.id DESC LIMIT ?`,
		act, bid, limit)
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
	if withReplies {
		recv, aid, err := s.cursor(after)
		if err != nil {
			return nil, err
		}
		rows, err := s.db.Query(feedQuery+`WHERE (p.received, p.id) > (?, ?) ORDER BY p.received, p.id LIMIT ?`,
			recv, aid, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return s.scanFeed(rows)
	}
	act, aid, err := s.activityCursor(after)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(feedQuery+`WHERE p.reply_to = '' AND (p.activity, p.id) > (?, ?) ORDER BY p.activity, p.id LIMIT ?`,
		act, aid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanFeed(rows)
}

// activityCursor is cursor for the activity-ordered home feed: the
// page boundary is the cursor post's last touch, not its arrival. A
// bump moves a post across pages; the first page is the live one, so
// a cursor into the past only ever repeats a post, never loses one.
func (s *Store) activityCursor(before string) (int64, string, error) {
	if before == "" {
		return 1 << 62, "￿", nil
	}
	var act int64
	err := s.db.QueryRow(`SELECT activity FROM posts WHERE id=?`, before).Scan(&act)
	if err == sql.ErrNoRows {
		return 0, "", ErrNotFound
	}
	return act, before, err
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

// searchWhere is the search's filter: one LIKE per word of q, all of
// which must hold. SQLite's LIKE folds ASCII case, and the word's own
// `%`, `_` and `\` are escaped so it is looked for literally; a word is
// a substring, not a token, so CJK prose — which has no word breaks for
// a tokenizer to find — is searched the same way. The clause starts
// with AND, for a WHERE that already has a term.
func searchWhere(q string) (string, []any) {
	var b strings.Builder
	var args []any
	for _, w := range strings.Fields(q) {
		b.WriteString(` AND p.text LIKE ? ESCAPE '\'`)
		args = append(args, "%"+likeEscaper.Replace(w)+"%")
	}
	return b.String(), args
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Search is the posts whose text holds every word of q, replies included
// (a reply is found by its words like any post), newest first and
// keyset-paginated like Feed. SearchNewer is its other direction, like
// FeedNewer; SearchCount the total, for the search page's pager.
func (s *Store) Search(q, before string, limit int) ([]FeedPost, error) {
	recv, bid, err := s.cursor(before)
	if err != nil {
		return nil, err
	}
	where, args := searchWhere(q)
	rows, err := s.db.Query(feedQuery+`WHERE 1`+where+` AND (p.received, p.id) < (?, ?) ORDER BY p.received DESC, p.id DESC LIMIT ?`,
		append(args, recv, bid, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanFeed(rows)
}

func (s *Store) SearchNewer(q, after string, limit int) ([]FeedPost, error) {
	recv, aid, err := s.cursor(after)
	if err != nil {
		return nil, err
	}
	where, args := searchWhere(q)
	rows, err := s.db.Query(feedQuery+`WHERE 1`+where+` AND (p.received, p.id) > (?, ?) ORDER BY p.received, p.id LIMIT ?`,
		append(args, recv, aid, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanFeed(rows)
}

func (s *Store) SearchCount(q string) (int, error) {
	where, args := searchWhere(q)
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM posts p WHERE 1`+where, args...).Scan(&n)
	return n, err
}

// Post returns one post; Replies its children oldest-first (thread order).
// PostPrefixMin is the fewest characters of an id that may stand for the
// whole: eight hex, the length an id is written at in a post or a commit
// message and so the length a link gets cut to (Livid, 2026-09-19: "i
// expect that 8 char short id can resolve too"; it was twelve for a few
// hours). Safety never rested on the length: ResolvePrefix answers only
// when exactly one post ever began that way. A shorter floor cannot find
// the wrong post, it only lets a given short link turn ambiguous, a 404,
// sooner as the hub grows: at 32 bits about one link in 40,000 on a hub
// of 100,000 posts.
const PostPrefixMin = 8

// ResolvePrefix is the whole id of the post whose id begins with prefix
// (lower-case hex, PostPrefixMin characters or more; see PLAN.md, Public
// pages — a short id finds its post). It answers only when exactly one
// post ever began that way: the test is made against the log, every
// post.create in messages, deleted posts included, and only then is the
// one match required to still be a post. Made against the posts alone,
// deleting A would hand A's old short link to a B with the same prefix;
// an old link fails rather than change its target. ErrAmbiguous for more
// than one, ErrNotFound for none or a post that is gone. The lookup is a
// range on the log's primary key, never a walk: every id under the
// prefix sorts from the prefix itself to below the prefix with a 'g',
// the character after hex's last.
func (s *Store) ResolvePrefix(prefix string) (string, error) {
	if len(prefix) < PostPrefixMin || len(prefix) >= 64 || strings.Trim(prefix, "0123456789abcdef") != "" {
		return "", ErrNotFound
	}
	rows, err := s.db.Query(prefixQuery, prefix, prefix+"g")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	rows.Close()
	switch len(ids) {
	case 0:
		return "", ErrNotFound
	case 1:
	default:
		return "", ErrAmbiguous
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM posts WHERE id = ?`, ids[0]).Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrNotFound
	}
	return ids[0], nil
}

// two are enough to know there is more than one
const prefixQuery = `SELECT id FROM messages WHERE id >= ? AND id < ? AND type = 'post.create' LIMIT 2`

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

// Thread is every reply under a post, however deep, in reading order: a
// reply is followed by the replies to it, siblings oldest first, and Depth
// counts the steps from the root (1 = a direct reply). Replies keeps the
// one-level, keyset-paged view; this is the whole tree for a thread page
// or a client that wants to show a reply under the reply it answers. The
// walk stops at limit posts and 32 levels; a reply whose parent fell past
// the limit (or arrived by replication before its parent) is kept at the
// end as a direct reply rather than lost.
func (s *Store) Thread(id string, limit int) ([]FeedPost, error) {
	rows, err := s.db.Query(`WITH RECURSIVE sub(id, depth) AS (
  SELECT c.id, 1 FROM posts c WHERE c.reply_to = ?
  UNION ALL
  SELECT c.id, sub.depth + 1 FROM posts c JOIN sub ON c.reply_to = sub.id WHERE sub.depth < 32)
SELECT `+feedCols+`
FROM sub JOIN posts p ON p.id = sub.id LEFT JOIN profiles pr ON pr.id = p.author
LEFT JOIN cards cd ON cd.post = p.id AND cd.status = 'ok'
ORDER BY p.received, p.id LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all, err := s.scanFeed(rows)
	if err != nil {
		return nil, err
	}
	kids := map[string][]FeedPost{}
	for _, p := range all {
		kids[p.ReplyTo] = append(kids[p.ReplyTo], p)
	}
	out := make([]FeedPost, 0, len(all))
	placed := map[string]bool{}
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, p := range kids[parent] {
			p.Depth = depth
			placed[p.ID] = true
			out = append(out, p)
			walk(p.ID, depth+1)
		}
	}
	walk(id, 1)
	for _, p := range all {
		if !placed[p.ID] {
			p.Depth = 1
			out = append(out, p)
		}
	}
	return out, nil
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

// AdoptPin records a pin mirrored after the message that names it was
// ingested (the puller's heal pass). The references are already there, so
// the refcount is their number, not the 0 a staged upload starts at — the
// sweep would take a 0 for a never-used upload and drop the pin again.
func (s *Store) AdoptPin(cid string, size int64, mime string, avatar bool) error {
	av := 0
	if avatar {
		av = 1
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO pins (cid, size, mime, refs, is_avatar, created) VALUES (?,?,?,0,?,?)
		ON CONFLICT(cid) DO UPDATE SET is_avatar = MAX(is_avatar, excluded.is_avatar)`,
		cid, size, mime, av, time.Now().UnixMilli()); err != nil {
		return err
	}
	// every kind of reference a pin can have, counted as Rebuild counts them
	if _, err := tx.Exec(`UPDATE pins SET refs =
		(SELECT COUNT(*) FROM embeds WHERE embeds.cid = pins.cid) +
		(SELECT COUNT(*) FROM embeds WHERE embeds.poster = pins.cid) +
		(SELECT COUNT(*) FROM profiles WHERE profiles.avatar = pins.cid) +
		(SELECT COUNT(*) FROM cards WHERE cards.image = pins.cid AND cards.status='ok') +
		(SELECT COUNT(*) FROM pictures WHERE pictures.cid = pins.cid AND pictures.status='ok')
		WHERE cid=?`, cid); err != nil {
		return err
	}
	return tx.Commit()
}

// MissingMirror is an embed, poster or avatar that a replicated message
// names and this hub holds no pin for: its mirror failed when the message
// came in, and the message landed without it.
type MissingMirror struct {
	CID    string
	Origin string // the peer hub the message was pulled from
	Avatar bool
}

// MissingMirrors lists them, for the puller to try their peer again.
func (s *Store) MissingMirrors() ([]MissingMirror, error) {
	rows, err := s.db.Query(`
		SELECT e.cid, m.origin, 0 FROM embeds e JOIN messages m ON m.id = e.post
		 WHERE m.origin != '' AND NOT EXISTS (SELECT 1 FROM pins WHERE pins.cid = e.cid)
		UNION
		SELECT e.poster, m.origin, 0 FROM embeds e JOIN messages m ON m.id = e.post
		 WHERE m.origin != '' AND e.poster != '' AND NOT EXISTS (SELECT 1 FROM pins WHERE pins.cid = e.poster)
		UNION
		SELECT p.avatar, COALESCE((SELECT origin FROM messages WHERE author = p.pubkey AND type = 'profile.set'
		         ORDER BY rowid DESC LIMIT 1), ''), 1 FROM profiles p
		 WHERE p.avatar != '' AND NOT EXISTS (SELECT 1 FROM pins WHERE pins.cid = p.avatar)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingMirror
	for rows.Next() {
		var m MissingMirror
		if err := rows.Scan(&m.CID, &m.Origin, &m.Avatar); err != nil {
			return nil, err
		}
		if m.Origin != "" { // a local message never lands without its pin
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

// Facts is what a /v1/media conversion measured for its output file.
type Facts struct {
	Poster   string
	Width    int
	Height   int
	Duration float64
	Loop     bool
}

// AddMediaPin records a conversion's output with its facts; a post that
// names the file must repeat them (checkFacts). Its poster is pinned on
// its own, with AddPin.
func (s *Store) AddMediaPin(cid string, size int64, mime string, f Facts) error {
	_, err := s.db.Exec(`INSERT INTO pins (cid, size, mime, refs, is_avatar, created, media, poster, width, height, duration, loop) VALUES (?,?,?,0,0,?,1,?,?,?,?,?)
		ON CONFLICT(cid) DO UPDATE SET media=1, poster=excluded.poster, width=excluded.width, height=excluded.height, duration=excluded.duration, loop=excluded.loop`,
		cid, size, mime, time.Now().UnixMilli(), f.Poster, f.Width, f.Height, f.Duration, f.Loop)
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

// PinCIDs lists every upload the hub holds a pin row for, referenced or
// staged, so the pins can be reconciled with what kubo actually pins.
func (s *Store) PinCIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT cid FROM pins ORDER BY created`)
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
	// how far into the peer's translations this hub has read (PLAN.md,
	// Translations — one hub pays)
	TrCursor int64 `json:"tr_cursor,omitempty"`
}

func (s *Store) Peers() ([]Peer, error) {
	rows, err := s.db.Query(`SELECT p.hub, p.addr, IFNULL(ps.pubkey,''), IFNULL(ps.cursor,0), IFNULL(ps.tr_cursor,0)
		FROM peers p LEFT JOIN peer_state ps ON ps.hub = p.hub ORDER BY p.ts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Peer{}
	for rows.Next() {
		var p Peer
		if err := rows.Scan(&p.Hub, &p.Addr, &p.PubKey, &p.Cursor, &p.TrCursor); err != nil {
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

// PushSub is one browser's Web Push subscription (PLAN.md
// "Notifications"): where to POST and the keys the payload is
// encrypted to. Anonymous — nothing ties it to a profile.
type PushSub struct {
	Endpoint     string
	P256dh, Auth []byte
	Base         string
}

// PushAdd stores a subscription; the same endpoint again replaces it.
func (s *Store) PushAdd(sub PushSub) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO push_subs(endpoint, p256dh, auth, base, created) VALUES (?, ?, ?, ?, ?)`,
		sub.Endpoint, sub.P256dh, sub.Auth, sub.Base, time.Now().UnixMilli())
	return err
}

func (s *Store) PushRemove(endpoint string) error {
	_, err := s.db.Exec(`DELETE FROM push_subs WHERE endpoint = ?`, endpoint)
	return err
}

func (s *Store) PushCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM push_subs`).Scan(&n)
	return n, err
}

// PushSubs is every subscription, oldest first.
func (s *Store) PushSubs() ([]PushSub, error) {
	rows, err := s.db.Query(`SELECT endpoint, p256dh, auth, base FROM push_subs ORDER BY created, endpoint`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSub
	for rows.Next() {
		var sub PushSub
		if err := rows.Scan(&sub.Endpoint, &sub.P256dh, &sub.Auth, &sub.Base); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}
