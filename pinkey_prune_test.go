package main

import (
	"testing"
	"time"
)

// Task 142: pin_keys rows older than 90 days go; younger ones (and the dedupe they give) stay.
func TestPinKeysPruned(t *testing.T) {
	f := newNotesFix(t)
	now := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	for _, q := range []string{
		`INSERT INTO pin_keys (created_by, client_id, pin_id, created_at) VALUES ('giedrius', 'old', 5, '2026-08-12T02:59:59Z')`,  // 90 days + 1 s
		`INSERT INTO pin_keys (created_by, client_id, pin_id, created_at) VALUES ('giedrius', 'edge', 5, '2026-08-13T03:00:00Z')`, // exactly 90 days
		`INSERT INTO pin_keys (created_by, client_id, pin_id, created_at) VALUES ('ann', 'new', 6, '2026-11-10T09:00:00Z')`,
	} {
		if _, err := f.a.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.a.prunePinKeys(now); n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pin_keys WHERE client_id IN ('edge', 'new')`); n != 2 {
		t.Errorf("young keys left: %d, want 2", n)
	}
	if n := f.a.prunePinKeys(now); n != 0 {
		t.Errorf("second run removed %d", n)
	}
	// a key the page sends now is stored and still dedupes after a prune
	r1 := f.postPin(t, keyed("1794298543932:prune1"), nil)
	f.a.prunePinKeys(time.Now())
	r2 := f.postPin(t, keyed("1794298543932:prune1"), nil)
	if r1.code != 201 || r2.code != 201 || pinID(t, r1) != pinID(t, r2) {
		t.Errorf("a fresh key no longer dedupes after a prune: %d %d", r1.code, r2.code)
	}
}
