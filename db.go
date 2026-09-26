package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  is_admin      INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS sessions (
  token      TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS holidays (
  id       INTEGER PRIMARY KEY,
  name     TEXT NOT NULL,
  color    TEXT NOT NULL,
  start_at TEXT NOT NULL,
  end_at   TEXT,
  journal  TEXT NOT NULL DEFAULT '',
  immich_album_id TEXT NOT NULL DEFAULT '',
  cover_asset TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS pins (
  id          INTEGER PRIMARY KEY,
  holiday_id  INTEGER NOT NULL REFERENCES holidays(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL CHECK (kind IN ('manual','photo')),
  cluster_key TEXT,
  lat         REAL NOT NULL,
  lng         REAL NOT NULL,
  title       TEXT NOT NULL DEFAULT '',
  note        TEXT NOT NULL DEFAULT '',
  country     TEXT NOT NULL DEFAULT '',
  cover_asset TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  UNIQUE (holiday_id, cluster_key)
);

CREATE TABLE IF NOT EXISTS pin_photos (
  pin_id   INTEGER NOT NULL REFERENCES pins(id) ON DELETE CASCADE,
  asset_id TEXT NOT NULL UNIQUE,
  taken_at TEXT NOT NULL,
  lat      REAL NOT NULL,
  lng      REAL NOT NULL,
  PRIMARY KEY (pin_id, asset_id)
);

-- photos in a holiday's date range that carry no GPS EXIF
CREATE TABLE IF NOT EXISTS unplaced_photos (
  holiday_id INTEGER NOT NULL REFERENCES holidays(id) ON DELETE CASCADE,
  asset_id   TEXT NOT NULL UNIQUE,
  taken_at   TEXT NOT NULL,
  PRIMARY KEY (holiday_id, asset_id)
);

-- view-only share links, one per holiday; tokens stored hashed like sessions
CREATE TABLE IF NOT EXISTS shares (
  token      TEXT PRIMARY KEY,
  holiday_id INTEGER NOT NULL UNIQUE REFERENCES holidays(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- Manifest: master packing lists ("always pack this") and each holiday's
-- own tick-off copy. NOCASE keeps "Sun cream" and "sun cream" one item.
CREATE TABLE IF NOT EXISTS packing_templates (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL COLLATE NOCASE UNIQUE
);

CREATE TABLE IF NOT EXISTS packing_template_items (
  id          INTEGER PRIMARY KEY,
  template_id INTEGER NOT NULL REFERENCES packing_templates(id) ON DELETE CASCADE,
  label       TEXT NOT NULL COLLATE NOCASE,
  sort        INTEGER NOT NULL DEFAULT 0,
  UNIQUE (template_id, label)
);

CREATE TABLE IF NOT EXISTS packing_items (
  id         INTEGER PRIMARY KEY,
  holiday_id INTEGER NOT NULL REFERENCES holidays(id) ON DELETE CASCADE,
  label      TEXT NOT NULL COLLATE NOCASE,
  checked    INTEGER NOT NULL DEFAULT 0,
  sort       INTEGER NOT NULL DEFAULT 0,
  UNIQUE (holiday_id, label)
);

CREATE INDEX IF NOT EXISTS idx_pins_holiday ON pins(holiday_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
CREATE INDEX IF NOT EXISTS idx_packing_holiday ON packing_items(holiday_id);
`

// additive column migrations for databases created before the columns
// existed; "duplicate column" errors just mean it's already applied.
var migrations = []string{
	`ALTER TABLE holidays ADD COLUMN journal TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pins ADD COLUMN country TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE holidays ADD COLUMN immich_album_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE holidays ADD COLUMN cover_asset TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pins ADD COLUMN cover_asset TEXT NOT NULL DEFAULT ''`,
	// destination chosen at trip creation; dest_name = '' means none
	`ALTER TABLE holidays ADD COLUMN dest_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE holidays ADD COLUMN dest_lat REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE holidays ADD COLUMN dest_lng REAL NOT NULL DEFAULT 0`,
	// 1.10.0's rectangle; unused since outlines replaced it (a country's
	// rectangle swallowed half the globe), kept so 1.10.0 can still roll back
	`ALTER TABLE holidays ADD COLUMN dest_bbox TEXT NOT NULL DEFAULT ''`,
	// the destination's outline, JSON MultiPolygon coordinates; '' = none
	`ALTER TABLE holidays ADD COLUMN dest_area TEXT NOT NULL DEFAULT ''`,
	// planned = 1: a trip with a future first day, counting down. It has no
	// end_at yet, so the one-live-trip index must not count it. The index is
	// built here, not in schema, because it needs the planned column.
	`ALTER TABLE holidays ADD COLUMN planned INTEGER NOT NULL DEFAULT 0`,
	`DROP INDEX IF EXISTS one_active_holiday`,
	`CREATE UNIQUE INDEX IF NOT EXISTS one_live_holiday
	  ON holidays ((end_at IS NULL)) WHERE end_at IS NULL AND planned = 0`,
	// the family page: when each account was last used ('' = never), and
	// whether it holds a temporary password an admin set
	`ALTER TABLE users ADD COLUMN last_seen TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE users ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0`,
}

func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite serializes access per connection; a single connection
	// avoids SQLITE_BUSY between the writer and readers under WAL.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	for _, stmt := range migrations {
		db.Exec(stmt)
	}
	// Before planned trips existed (<= 1.8.2) a future first day made a live
	// trip. An open trip whose first day is still ahead is a planned one.
	if _, err := db.Exec(`UPDATE holidays SET planned = 1
		WHERE end_at IS NULL AND planned = 0 AND start_at > ?`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		return nil, fmt.Errorf("re-plan future trips: %w", err)
	}
	return db, nil
}
