package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Stats (PLAN.md, Stats): one row per page view of the public pages,
// written by the collector in internal/stats. Like pins and cards the
// table is not derived from the log — it is the hub's own record of its
// readers — and Rebuild leaves it alone. Nothing in a row names a
// person: the visitor id is the day's salted hash, the address and the
// user agent were read and dropped, and what remains is a country, a
// device, a browser, a page and a time.

func (s *Store) initStats() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS hits (
  id       INTEGER PRIMARY KEY,
  ts       INTEGER NOT NULL,            -- ms
  vid      TEXT NOT NULL,               -- the visitor: today's salted hash
  sid      TEXT NOT NULL,               -- the session
  entry    INTEGER NOT NULL DEFAULT 0,  -- 1: the session's first page
  path     TEXT NOT NULL,
  kind     TEXT NOT NULL,               -- home | thread | profile | search | skill
  ref      TEXT NOT NULL DEFAULT '',    -- the source's name; '' direct
  chan     TEXT NOT NULL DEFAULT '',    -- direct | search | social | ai | referral | campaign
  country  TEXT NOT NULL DEFAULT '',    -- ISO 3166-1 alpha-2
  region   TEXT NOT NULL DEFAULT '',
  city     TEXT NOT NULL DEFAULT '',
  device   TEXT NOT NULL DEFAULT '',    -- desktop | mobile | tablet | agent
  browser  TEXT NOT NULL DEFAULT '',
  os       TEXT NOT NULL DEFAULT '',
  lang     TEXT NOT NULL DEFAULT '',
  utm_source   TEXT NOT NULL DEFAULT '',
  utm_medium   TEXT NOT NULL DEFAULT '',
  utm_campaign TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS hits_ts ON hits(ts);
CREATE INDEX IF NOT EXISTS hits_sid ON hits(sid, ts);

-- the day's salt for visitor ids: kept for the day so a restart keeps
-- the day whole, deleted with the day
CREATE TABLE IF NOT EXISTS hits_salt (
  day  TEXT PRIMARY KEY,
  salt BLOB NOT NULL
);`)
	return err
}

// Hit is one page view.
type Hit struct {
	TS                    int64
	VID, SID              string
	Entry                 bool
	Path, Kind            string
	Ref, Channel          string
	Country, Region, City string
	Device, Browser, OS   string
	Lang                  string
	UTMSource, UTMMedium  string
	UTMCampaign           string
}

// StatsAdd writes a batch of hits in one transaction.
func (s *Store) StatsAdd(hits []Hit) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO hits (ts, vid, sid, entry, path, kind, ref, chan, country, region, city,
		device, browser, os, lang, utm_source, utm_medium, utm_campaign) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, h := range hits {
		entry := 0
		if h.Entry {
			entry = 1
		}
		if _, err := stmt.Exec(h.TS, h.VID, h.SID, entry, h.Path, h.Kind, h.Ref, h.Channel, h.Country, h.Region, h.City,
			h.Device, h.Browser, h.OS, h.Lang, h.UTMSource, h.UTMMedium, h.UTMCampaign); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// StatsSalt is the day's salt, minted on its first call for the day;
// every other day's salt is dropped, so yesterday's ids can never be
// recomputed.
func (s *Store) StatsSalt(day string) ([]byte, error) {
	var salt []byte
	err := s.db.QueryRow(`SELECT salt FROM hits_salt WHERE day=?`, day).Scan(&salt)
	if err == nil {
		return salt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	salt = make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM hits_salt WHERE day<>?`, day); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO hits_salt (day, salt) VALUES (?,?)`, day, salt); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(`SELECT salt FROM hits_salt WHERE day=?`, day).Scan(&salt); err != nil {
		return nil, err
	}
	return salt, tx.Commit()
}

// OpenSession is a visitor's session with a hit since the gap: what the
// collector resumes after a restart.
type OpenSession struct {
	VID, SID                          string
	Last                              int64
	Ref, Channel                      string
	UTMSource, UTMMedium, UTMCampaign string
}

func (s *Store) StatsOpenSessions(since int64) ([]OpenSession, error) {
	rows, err := s.db.Query(`SELECT h.vid, h.sid, h.ts, h.ref, h.chan, h.utm_source, h.utm_medium, h.utm_campaign
		FROM hits h JOIN (SELECT vid, MAX(id) AS id FROM hits WHERE ts >= ? GROUP BY vid) last ON last.id = h.id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenSession
	for rows.Next() {
		var o OpenSession
		if err := rows.Scan(&o.VID, &o.SID, &o.Last, &o.Ref, &o.Channel, &o.UTMSource, &o.UTMMedium, &o.UTMCampaign); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// StatsOnline is how many visitors had a hit since the given time.
func (s *Store) StatsOnline(since int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(DISTINCT vid) FROM hits WHERE ts >= ?`, since).Scan(&n)
	return n, err
}

// StatsFilter selects the hits a report is over: a time span, and any
// of the dimensions held to one value (a click on a row of the page).
type StatsFilter struct {
	From, To                        int64 // ms, [From, To)
	Country, Region, City, Lang     string
	Path, Source, Channel, Campaign string
	Device, Browser, OS             string
}

func (f StatsFilter) where() (string, []any) {
	w := []string{"ts >= ?", "ts < ?"}
	args := []any{f.From, f.To}
	for _, c := range []struct{ col, v string }{
		{"country", f.Country}, {"region", f.Region}, {"city", f.City}, {"lang", f.Lang},
		{"path", f.Path}, {"ref", f.Source}, {"chan", f.Channel}, {"utm_campaign", f.Campaign},
		{"device", f.Device}, {"browser", f.Browser}, {"os", f.OS},
	} {
		if c.v != "" {
			w = append(w, c.col+" = ?")
			args = append(args, c.v)
		}
	}
	return strings.Join(w, " AND "), args
}

// StatsSummary is the headline numbers over a span: visitors (distinct
// per day, as the ids rotate), page views, sessions, the sessions that
// saw one page (bounces), and the mean session length in seconds
// (first page to last; a one-page session is 0).
type StatsSummary struct {
	Visitors  int     `json:"visitors"`
	Pageviews int     `json:"pageviews"`
	Sessions  int     `json:"sessions"`
	Bounces   int     `json:"bounces"`
	Duration  float64 `json:"duration"`
}

func (s *Store) StatsSummary(f StatsFilter) (StatsSummary, error) {
	var out StatsSummary
	w, args := f.where()
	if err := s.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT vid), COUNT(DISTINCT sid) FROM hits WHERE `+w, args...).
		Scan(&out.Pageviews, &out.Visitors, &out.Sessions); err != nil {
		return out, err
	}
	var bounces sql.NullInt64
	var dur sql.NullFloat64
	if err := s.db.QueryRow(`SELECT SUM(n = 1), AVG(dur) FROM
		(SELECT COUNT(*) AS n, (MAX(ts) - MIN(ts)) / 1000.0 AS dur FROM hits WHERE `+w+` GROUP BY sid)`, args...).
		Scan(&bounces, &dur); err != nil {
		return out, err
	}
	out.Bounces, out.Duration = int(bounces.Int64), dur.Float64
	return out, nil
}

// HourRow is one hour's page views and visitors (distinct within the
// hour): the finest bucket a chart draws.
type HourRow struct {
	Hour      int64 // unix hours
	Pageviews int
	Visitors  int
}

func (s *Store) StatsHours(f StatsFilter) ([]HourRow, error) {
	w, args := f.where()
	rows, err := s.db.Query(`SELECT ts / 3600000, COUNT(*), COUNT(DISTINCT vid) FROM hits WHERE `+w+` GROUP BY 1 ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HourRow
	for rows.Next() {
		var r HourRow
		if err := rows.Scan(&r.Hour, &r.Pageviews, &r.Visitors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StatsVisitorFirst is each visitor's first hit time in the span: a
// visitor id belongs to one day, so bucketing these by day or month
// counts distinct visitors per bucket.
func (s *Store) StatsVisitorFirst(f StatsFilter) ([]int64, error) {
	w, args := f.where()
	rows, err := s.db.Query(`SELECT MIN(ts) FROM hits WHERE `+w+` GROUP BY vid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StatsRow is one line of a ranked list: a key and its count.
type StatsRow struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"` // a thread's words, a profile's name (set by the page)
	N     int    `json:"n"`
}

// statsTopMax caps a ranked list: a hub's paths over a year can run long.
const statsTopMax = 500

// StatsTop ranks one dimension over the span. Sources, channels and
// campaigns count sessions (by their first page); entry and exit pages
// count sessions too; every other dimension counts visitors.
func (s *Store) StatsTop(f StatsFilter, dim string) ([]StatsRow, error) {
	w, args := f.where()
	var q string
	switch dim {
	case "source":
		q = `SELECT ref, COUNT(*) FROM hits WHERE entry = 1 AND ` + w + ` GROUP BY ref`
	case "channel":
		q = `SELECT chan, COUNT(*) FROM hits WHERE entry = 1 AND ` + w + ` GROUP BY chan`
	case "campaign":
		q = `SELECT utm_campaign, COUNT(*) FROM hits WHERE entry = 1 AND utm_campaign <> '' AND ` + w + ` GROUP BY utm_campaign`
	case "entry":
		q = `SELECT path, COUNT(*) FROM hits WHERE entry = 1 AND ` + w + ` GROUP BY path`
	case "exit":
		q = `SELECT path, COUNT(*) FROM hits WHERE id IN (SELECT MAX(id) FROM hits WHERE ` + w + ` GROUP BY sid) GROUP BY path`
	case "path", "country", "region", "city", "device", "browser", "os", "lang":
		q = `SELECT ` + dim + `, COUNT(DISTINCT vid) FROM hits WHERE ` + dim + ` <> '' AND ` + w + ` GROUP BY ` + dim
	default:
		return nil, fmt.Errorf("stats: no dimension %q", dim)
	}
	rows, err := s.db.Query(q+` ORDER BY 2 DESC, 1 LIMIT ?`, append(args, statsTopMax)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatsRow
	for rows.Next() {
		var r StatsRow
		if err := rows.Scan(&r.Key, &r.N); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LiveHit is one of the latest page views, for the Live list.
type LiveHit struct {
	TS              int64
	VID, Path, Kind string
	Country, Device string
}

// StatsRecent is the latest n hits, newest first.
func (s *Store) StatsRecent(n int) ([]LiveHit, error) {
	rows, err := s.db.Query(`SELECT ts, vid, path, kind, country, device FROM hits ORDER BY ts DESC, id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveHit
	for rows.Next() {
		var h LiveHit
		if err := rows.Scan(&h.TS, &h.VID, &h.Path, &h.Kind, &h.Country, &h.Device); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// StatsSweep drops the hits before a time (the retention) and reports
// how many went.
func (s *Store) StatsSweep(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM hits WHERE ts < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
