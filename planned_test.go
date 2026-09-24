package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *app {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &app{db: db}
}

func createHoliday(t *testing.T, a *app, body map[string]any) (int, holidayOut) {
	t.Helper()
	body["color"] = "#aa3300"
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/holidays", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.handleCreateHoliday(rec, req)
	var h holidayOut
	json.Unmarshal(rec.Body.Bytes(), &h)
	return rec.Code, h
}

func listHolidays(t *testing.T, a *app) map[string]holidayOut {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleListHolidays(rec, httptest.NewRequest("GET", "/api/holidays", nil))
	var hs []holidayOut
	if err := json.Unmarshal(rec.Body.Bytes(), &hs); err != nil {
		t.Fatalf("list: %v: %s", err, rec.Body)
	}
	out := map[string]holidayOut{}
	for _, h := range hs {
		out[h.Name] = h
	}
	return out
}

func day(offset int) string {
	return time.Now().UTC().AddDate(0, 0, offset).Format("2006-01-02")
}

func TestPlannedTripsSitBesideALiveOne(t *testing.T) {
	a := newTestApp(t)
	if code, _ := createHoliday(t, a, map[string]any{"name": "Now"}); code != http.StatusCreated {
		t.Fatalf("live trip: %d", code)
	}
	for _, name := range []string{"Summer", "Christmas"} {
		off := map[string]int{"Summer": 30, "Christmas": 90}[name]
		code, h := createHoliday(t, a, map[string]any{"name": name, "start_at": day(off)})
		if code != http.StatusCreated {
			t.Fatalf("%s: got %d, want 201 (the one-live index must not count planned trips)", name, code)
		}
		if !h.Planned || h.Active {
			t.Errorf("%s: planned=%v active=%v, want planned and not active", name, h.Planned, h.Active)
		}
	}
	// a second live trip is still refused
	if code, _ := createHoliday(t, a, map[string]any{"name": "Another"}); code != http.StatusConflict {
		t.Errorf("second live trip: got %d, want 409", code)
	}
	hs := listHolidays(t, a)
	if !hs["Now"].Active || hs["Summer"].Active || !hs["Summer"].Planned {
		t.Errorf("list: Now=%+v Summer=%+v", hs["Now"], hs["Summer"])
	}
}

func TestPlannedTripRefusesALastDay(t *testing.T) {
	a := newTestApp(t)
	code, _ := createHoliday(t, a, map[string]any{"name": "Lisbon", "start_at": day(10), "end_at": day(17)})
	if code != http.StatusBadRequest {
		t.Errorf("create with future dates: got %d, want 400", code)
	}
	createHoliday(t, a, map[string]any{"name": "Lisbon", "start_at": day(10)})
	if code := patchHoliday(t, a, `{"end_at":"`+day(17)+`"}`); code != http.StatusBadRequest {
		t.Errorf("patch end_at onto planned trip: got %d, want 400", code)
	}
	// a past trip with both dates is still an import
	code, h := createHoliday(t, a, map[string]any{"name": "Rome", "start_at": day(-20), "end_at": day(-13)})
	if code != http.StatusCreated || h.Planned || h.Active {
		t.Errorf("past import: code=%d %+v", code, h)
	}
}

func TestPlannedTripGoesLiveOnTheDay(t *testing.T) {
	a := newTestApp(t)
	createHoliday(t, a, map[string]any{"name": "Now"})
	createHoliday(t, a, map[string]any{"name": "Lisbon", "start_at": day(5)})
	createHoliday(t, a, map[string]any{"name": "Oslo", "start_at": day(8)})
	// the days pass: both first days are now behind us
	a.db.Exec(`UPDATE holidays SET start_at = ? WHERE name = 'Lisbon'`, time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339))
	a.db.Exec(`UPDATE holidays SET start_at = ? WHERE name = 'Oslo'`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))

	// "Now" is still live, so Lisbon waits
	if hs := listHolidays(t, a); !hs["Lisbon"].Planned || !hs["Now"].Active {
		t.Fatalf("promoted over a live trip: %+v", hs)
	}
	a.db.Exec(`UPDATE holidays SET end_at = ? WHERE name = 'Now'`, time.Now().UTC().Format(time.RFC3339))

	// the earliest goes live; the other keeps waiting behind it
	hs := listHolidays(t, a)
	if !hs["Lisbon"].Active || hs["Lisbon"].Planned {
		t.Errorf("Lisbon not live after Now ended: %+v", hs["Lisbon"])
	}
	if !hs["Oslo"].Planned || hs["Oslo"].Active {
		t.Errorf("Oslo should still be waiting: %+v", hs["Oslo"])
	}
}

func TestEndAndSyncIgnorePlannedTrips(t *testing.T) {
	a := newTestApp(t)
	_, h := createHoliday(t, a, map[string]any{"name": "Lisbon", "start_at": day(5)})

	req := httptest.NewRequest("POST", "/", nil)
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	a.handleEndHoliday(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("ending a planned trip: got %d, want 404", rec.Code)
	}
	// a.immich is nil: reaching Immich would panic, so returning at all
	// proves the planned trip was skipped
	if pins, photos, err := a.syncHoliday(h.ID); err != nil || pins != 0 || photos != 0 {
		t.Errorf("sync of planned trip: %d %d %v", pins, photos, err)
	}
}

func TestOldActiveIndexIsReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// the 1.8.2 shape: the index that treated every open trip as live
	for _, stmt := range []string{
		`CREATE TABLE holidays (id INTEGER PRIMARY KEY, name TEXT NOT NULL, color TEXT NOT NULL, start_at TEXT NOT NULL, end_at TEXT)`,
		`CREATE UNIQUE INDEX one_active_holiday ON holidays ((end_at IS NULL)) WHERE end_at IS NULL`,
		`INSERT INTO holidays (name, color, start_at) VALUES ('Now', '#000000', '2026-09-01T00:00:00Z')`,
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	// twice: a second boot must not bring the old index back
	for boot := 1; boot <= 2; boot++ {
		db, err := openDB(path)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'one_active_holiday'`).Scan(&n)
		if n != 0 {
			t.Errorf("boot %d: old index still present", boot)
		}
		db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'one_live_holiday'`).Scan(&n)
		if n != 1 {
			t.Errorf("boot %d: new index missing", boot)
		}
		db.Close()
	}
	db, _ := openDB(path)
	defer db.Close()
	a := &app{db: db}
	if code, _ := createHoliday(t, a, map[string]any{"name": "Lisbon", "start_at": day(5)}); code != http.StatusCreated {
		t.Errorf("planned trip beside the migrated live one: got %d", code)
	}
}
