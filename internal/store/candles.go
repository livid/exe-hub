package store

import (
	"database/sql"
	"errors"

	"exehub/internal/token"
)

// A pool's closed candles, kept for the token page's chart (PLAN.md, The
// token page — Candles kept). A closed candle is a fact that does not
// change, read from GeckoTerminal once: it is no message and derives
// from none, so Rebuild leaves these tables alone, and a hub that loses
// them asks GeckoTerminal again. token_candle_marks says what the kept
// candles of a pool and candle size cover: every candle that closed
// from since up to until is there, and since 0 is the pool's whole
// history.
const candleSchema = `
CREATE TABLE IF NOT EXISTS token_candles (
  pool   TEXT NOT NULL,
  frame  TEXT NOT NULL,    -- the candle's size: "minute/15", "day/1"
  t      INTEGER NOT NULL, -- when the candle began, unix seconds
  close  REAL NOT NULL,    -- the token's price in US dollars when it ended
  volume REAL NOT NULL,
  PRIMARY KEY (pool, frame, t)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS token_candle_marks (
  pool  TEXT NOT NULL,
  frame TEXT NOT NULL,
  since INTEGER NOT NULL,
  until INTEGER NOT NULL,
  PRIMARY KEY (pool, frame)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS token_chart_pools (
  mint TEXT PRIMARY KEY, -- the pool a mint's chart is drawn from: its deepest
  pool TEXT NOT NULL
) WITHOUT ROWID;`

// LoadCandles is the candles kept for a pool and candle size, the
// oldest first, and what they cover; until is 0 when none were ever
// kept.
func (s *Store) LoadCandles(pool, frame string) (cs []token.Candle, since, until int64, err error) {
	if _, err = s.db.Exec(candleSchema); err != nil {
		return nil, 0, 0, err
	}
	err = s.db.QueryRow(`SELECT since, until FROM token_candle_marks WHERE pool = ? AND frame = ?`, pool, frame).Scan(&since, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, 0, nil
	}
	if err != nil {
		return nil, 0, 0, err
	}
	rows, err := s.db.Query(`SELECT t, close, volume FROM token_candles WHERE pool = ? AND frame = ? ORDER BY t`, pool, frame)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var c token.Candle
		if err := rows.Scan(&c.T, &c.Close, &c.Volume); err != nil {
			return nil, 0, 0, err
		}
		cs = append(cs, c)
	}
	return cs, since, until, rows.Err()
}

// SaveCandles adds candles to those kept for a pool and candle size (one
// kept already is written over), drops the ones that began before
// dropBefore, and sets what is covered — all or nothing. reset drops
// every kept candle of the pool and size first.
func (s *Store) SaveCandles(pool, frame string, add []token.Candle, since, until, dropBefore int64, reset bool) error {
	if _, err := s.db.Exec(candleSchema); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if reset {
		if _, err := tx.Exec(`DELETE FROM token_candles WHERE pool = ? AND frame = ?`, pool, frame); err != nil {
			return err
		}
	} else if dropBefore > 0 {
		if _, err := tx.Exec(`DELETE FROM token_candles WHERE pool = ? AND frame = ? AND t < ?`, pool, frame, dropBefore); err != nil {
			return err
		}
	}
	for _, c := range add {
		if _, err := tx.Exec(`INSERT INTO token_candles (pool, frame, t, close, volume) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (pool, frame, t) DO UPDATE SET close = excluded.close, volume = excluded.volume`, pool, frame, c.T, c.Close, c.Volume); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO token_candle_marks (pool, frame, since, until) VALUES (?, ?, ?, ?)
		ON CONFLICT (pool, frame) DO UPDATE SET since = excluded.since, until = excluded.until`, pool, frame, since, until); err != nil {
		return err
	}
	return tx.Commit()
}

// ChartPool is the pool kept as the one a mint's chart is drawn from, ""
// when none was.
func (s *Store) ChartPool(mint string) (string, error) {
	if _, err := s.db.Exec(candleSchema); err != nil {
		return "", err
	}
	var pool string
	err := s.db.QueryRow(`SELECT pool FROM token_chart_pools WHERE mint = ?`, mint).Scan(&pool)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return pool, err
}

// SetChartPool keeps pool as the one mint's chart is drawn from.
func (s *Store) SetChartPool(mint, pool string) error {
	if _, err := s.db.Exec(candleSchema); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO token_chart_pools (mint, pool) VALUES (?, ?)
		ON CONFLICT (mint) DO UPDATE SET pool = excluded.pool`, mint, pool)
	return err
}
