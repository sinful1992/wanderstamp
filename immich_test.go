package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// Immich answers a bad API key with 401 and a JSON body, which decodes
// cleanly into an empty result. That must be an error, never "no photos".
func TestSearchRangeRejectsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Invalid API key"}`))
	}))
	defer srv.Close()

	c := newImmichClient(srv.URL, "bogus")
	assets, err := c.searchRange(time.Now().Add(-time.Hour), time.Now())
	if err == nil {
		t.Fatalf("searchRange returned %d assets and no error on a 401", len(assets))
	}
}

// A later page failing must not hand back the earlier pages as the full set —
// that would prune only the photos that happened to sort onto later pages.
func TestSearchRangeFailsOnLaterPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Page int `json:"page"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Page == 1 {
			w.Write([]byte(`{"assets":{"items":[{"id":"a","type":"IMAGE","fileCreatedAt":"2026-07-02T10:00:00Z"}],"nextPage":"2"}}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"message":"upstream"}`))
	}))
	defer srv.Close()

	c := newImmichClient(srv.URL, "key")
	if assets, err := c.searchRange(time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatalf("got %d assets and no error when page 2 failed", len(assets))
	}
}

// A failed sync must leave the trip exactly as it was: photo pins, their
// custom titles, and photos filed onto them by hand all survive.
func TestSyncHolidayKeepsDataWhenImmichFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"statusCode":500,"message":"Internal server error"}`))
	}))
	defer srv.Close()

	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, immich: newImmichClient(srv.URL, "key"), lastSync: map[int64]time.Time{}}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Trip', '#123456', '2026-07-01T00:00:00Z', '2026-07-08T00:00:00Z')`)
	mustExec(`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, title) VALUES (1, 1, 'photo', '2500,-90', 50.0, -1.8, 'Our campsite')`)
	mustExec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (1, 'gps-photo', '2026-07-02T10:00:00Z', 50.0, -1.8)`)
	mustExec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (1, 'hand-filed', '2026-07-02T11:00:00Z', 50.0, -1.8)`)
	mustExec(`INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at) VALUES (1, 'no-gps', '2026-07-03T10:00:00Z')`)

	if _, _, err := a.syncHoliday(1); err == nil {
		t.Error("syncHoliday reported success against a failing Immich")
	}

	var title string
	if err := db.QueryRow(`SELECT title FROM pins WHERE id = 1`).Scan(&title); err != nil || title != "Our campsite" {
		t.Errorf("photo pin lost: title=%q err=%v", title, err)
	}
	var photos, unplaced int
	db.QueryRow(`SELECT COUNT(*) FROM pin_photos`).Scan(&photos)
	db.QueryRow(`SELECT COUNT(*) FROM unplaced_photos`).Scan(&unplaced)
	if photos != 2 || unplaced != 1 {
		t.Errorf("after failed sync: %d pinned photos (want 2), %d unplaced (want 1)", photos, unplaced)
	}
}

func TestAlbumGoneOnlyForMissingAlbums(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&immichError{status: 400, msg: `immich PUT /api/albums/x/assets: 400 Bad Request: {"message":"Album not found"}`}, true},
		{&immichError{status: 404, msg: "immich PUT: 404"}, true},
		{&immichError{status: 502, msg: "immich PUT: 502 Bad Gateway"}, false},
		{&immichError{status: 400, msg: `{"message":"ids must be an array"}`}, false},
	} {
		if got := albumGone(tc.err); got != tc.want {
			t.Errorf("albumGone(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// Immich answering 200 with nothing (a key for the wrong account, a changed
// response shape) must not read as "every photo was deleted".
func TestSyncHolidayRefusesSuddenlyEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"assets":{"items":[],"nextPage":null}}`))
	}))
	defer srv.Close()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, immich: newImmichClient(srv.URL, "key"), lastSync: map[int64]time.Time{}}
	db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Trip', '#123456', '2026-07-01T00:00:00Z', '2026-07-08T00:00:00Z')`)
	db.Exec(`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (1, 1, 'photo', 'k', 50, -1.8)`)
	db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (1, 'p', '2026-07-02T10:00:00Z', 50, -1.8)`)

	if _, _, err := a.syncHoliday(1); err == nil {
		t.Error("an empty result for a trip with photos should be refused")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM pin_photos`).Scan(&n)
	if n != 1 {
		t.Errorf("photos pruned on an empty result: %d left", n)
	}

	// a trip with no photos yet syncs an empty result normally
	db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (2, 'New', '#123456', '2026-08-01T00:00:00Z', '2026-08-02T00:00:00Z')`)
	if _, _, err := a.syncHoliday(2); err != nil {
		t.Errorf("empty trip, empty result: %v", err)
	}
}
