package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite: no C compiler needed on any platform
)

var db *sql.DB

const schema = `
CREATE TABLE IF NOT EXISTS sites (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  domains    TEXT NOT NULL DEFAULT '',   -- comma-separated hostnames allowed to send data
  key        TEXT NOT NULL UNIQUE,       -- public key used in the tracking snippet
  privacy    TEXT NOT NULL DEFAULT 'cookieless', -- cookieless | consent | cookies
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_sessions (
  token      TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL
);

-- One row per browser (cookie) or per day-scoped cookieless id, per site.
CREATE TABLE IF NOT EXISTS visitors (
  site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  id         TEXT NOT NULL,
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  user_id    TEXT,               -- set by omega.identify()
  traits     TEXT,               -- JSON
  cookieless INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (site_id, id)
);
CREATE INDEX IF NOT EXISTS visitors_last_seen ON visitors(site_id, last_seen);
CREATE INDEX IF NOT EXISTS visitors_user ON visitors(site_id, user_id);

CREATE TABLE IF NOT EXISTS sessions (
  id           TEXT PRIMARY KEY,
  site_id      INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  visitor_id   TEXT NOT NULL,
  id_method    TEXT NOT NULL DEFAULT 'cookie', -- 'cookie' or 'hash' (cookieless)
  started_at   INTEGER NOT NULL,
  last_seen    INTEGER NOT NULL,
  entry_path   TEXT NOT NULL,
  exit_path    TEXT NOT NULL,
  exit_title   TEXT,
  pageviews    INTEGER NOT NULL DEFAULT 0,
  referrer     TEXT,
  source       TEXT NOT NULL,     -- "Direct", "Google", "news.example.com", or utm_source
  utm_source   TEXT,
  utm_medium   TEXT,
  utm_campaign TEXT,
  browser      TEXT,
  os           TEXT,
  device       TEXT,
  country      TEXT,
  language     TEXT,
  timezone     TEXT,
  screen       TEXT
);
CREATE INDEX IF NOT EXISTS sessions_started ON sessions(site_id, started_at);
CREATE INDEX IF NOT EXISTS sessions_last_seen ON sessions(site_id, last_seen);
CREATE INDEX IF NOT EXISTS sessions_visitor ON sessions(site_id, visitor_id);

CREATE TABLE IF NOT EXISTS pageviews (
  id         TEXT PRIMARY KEY,
  site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  session_id TEXT NOT NULL,
  visitor_id TEXT NOT NULL,
  ts         INTEGER NOT NULL,
  path       TEXT NOT NULL,
  title      TEXT,
  duration   INTEGER NOT NULL DEFAULT 0   -- engaged (visible) milliseconds on the page
);
CREATE INDEX IF NOT EXISTS pageviews_ts ON pageviews(site_id, ts);
CREATE INDEX IF NOT EXISTS pageviews_session ON pageviews(session_id, ts);

CREATE TABLE IF NOT EXISTS events (
  id         INTEGER PRIMARY KEY,
  site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
  session_id TEXT NOT NULL,
  visitor_id TEXT NOT NULL,
  ts         INTEGER NOT NULL,
  name       TEXT NOT NULL,
  path       TEXT,
  props      TEXT
);
CREATE INDEX IF NOT EXISTS events_ts ON events(site_id, ts);
CREATE INDEX IF NOT EXISTS events_session ON events(session_id, ts);

-- App-wide settings (key/value), e.g. the public address used in tracking snippets.
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- One random salt per UTC day for cookieless visitor ids. Old salts are deleted, so ids can't be linked across days.
CREATE TABLE IF NOT EXISTS salts (
  day  TEXT PRIMARY KEY,
  salt TEXT NOT NULL
);
`

func openDB(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	var err error
	if db, err = sql.Open("sqlite", dsn); err != nil {
		return err
	}
	db.SetMaxOpenConns(8)
	if _, err = db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	// Databases created by earlier versions.
	for _, m := range [][3]string{
		{"sites", "privacy", "TEXT NOT NULL DEFAULT 'cookieless'"},
		{"sessions", "id_method", "TEXT NOT NULL DEFAULT 'cookie'"},
		{"visitors", "cookieless", "INTEGER NOT NULL DEFAULT 0"},
		// Bot detection (see bots.go): signal bitmask, score, verdict, and whether a person interacted.
		{"sessions", "bot_signals", "INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "bot_score", "INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "bot", "INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "interacted", "INTEGER NOT NULL DEFAULT 0"},
		{"sessions", "network", "TEXT"}, // network owner (ASN organisation), e.g. "Comcast Cable" or "Amazon.com"
	} {
		if err := addColumn(m[0], m[1], m[2]); err != nil {
			return err
		}
	}
	return nil
}

func addColumn(table, column, definition string) error {
	cols, err := rowsOf(db, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return err
	}
	for _, c := range cols {
		if c["name"] == column {
			return nil
		}
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

// ---- Query helpers ----

// Row is one result row keyed by column name, ready to be encoded as JSON.
type Row = map[string]any

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	Exec(query string, args ...any) (sql.Result, error)
}

func rowsOf(q querier, query string, args ...any) ([]Row, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []Row{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := make(Row, len(cols))
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				r[c] = string(b)
			} else {
				r[c] = vals[i]
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// rowOf returns the first row, or nil when there is none.
func rowOf(q querier, query string, args ...any) (Row, error) {
	rows, err := rowsOf(q, query, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func withTx(fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Typed accessors for Row values (SQLite returns int64, float64, string or nil).
func str(r Row, k string) string {
	if r == nil {
		return ""
	}
	s, _ := r[k].(string)
	return s
}

func i64(r Row, k string) int64 {
	if r == nil {
		return 0
	}
	switch v := r[k].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

func f64(r Row, k string) float64 {
	if r == nil {
		return 0
	}
	switch v := r[k].(type) {
	case int64:
		return float64(v)
	case float64:
		return v
	}
	return 0
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
