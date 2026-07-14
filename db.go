package main

import (
	"database/sql"
	"fmt"

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

CREATE UNIQUE INDEX IF NOT EXISTS one_active_holiday
  ON holidays ((end_at IS NULL)) WHERE end_at IS NULL;

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

CREATE INDEX IF NOT EXISTS idx_pins_holiday ON pins(holiday_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);
`

// additive column migrations for databases created before the columns
// existed; "duplicate column" errors just mean it's already applied.
var migrations = []string{
	`ALTER TABLE holidays ADD COLUMN journal TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pins ADD COLUMN country TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE holidays ADD COLUMN immich_album_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE holidays ADD COLUMN cover_asset TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pins ADD COLUMN cover_asset TEXT NOT NULL DEFAULT ''`,
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
	return db, nil
}
