package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointFoldsWALIntoDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO holidays (name, color, start_at) VALUES ('Trip', '#123456', '2026-07-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("expected the write to sit in the WAL first (err=%v)", err)
	}

	if err := checkpoint(db); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() != 0 {
		t.Errorf("WAL not truncated after checkpoint (err=%v)", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM holidays`).Scan(&n); err != nil || n != 1 {
		t.Errorf("row lost across checkpoint: n=%d err=%v", n, err)
	}
}
