package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
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

// A pin placed by hand at the hotel holds the photos taken there: sync must
// not grow a town photo pin 25 m beside it, and must move photos off one that
// an older sync already made.
func TestSyncFilesPhotosOnNearbyHandPlacedPin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/search/metadata" {
			w.Write([]byte(`{}`))
			return
		}
		w.Write([]byte(`{"assets":{"items":[
			{"id":"hotel-1","type":"IMAGE","fileCreatedAt":"2026-09-25T16:21:00Z","exifInfo":{"latitude":52.53681,"longitude":-1.39994,"city":"Hinckley","country":"United Kingdom"}},
			{"id":"hotel-2","type":"IMAGE","fileCreatedAt":"2026-09-25T18:00:00Z","exifInfo":{"latitude":52.5370,"longitude":-1.4001,"city":"Hinckley","country":"United Kingdom"}},
			{"id":"town","type":"IMAGE","fileCreatedAt":"2026-09-25T19:00:00Z","exifInfo":{"latitude":52.5410,"longitude":-1.3740,"city":"Hinckley","country":"United Kingdom"}}
		],"nextPage":null}}`))
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
	mustExec(`INSERT INTO holidays (id, name, color, start_at) VALUES (1, 'Warwick', '#123456', '2026-09-25T00:00:00Z')`)
	mustExec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, title) VALUES (5, 1, 'manual', 52.5369686, -1.4002928, 'Premier Inn')`)
	// what the live trip looked like: an earlier sync had filed hotel-1 on its own photo pin
	mustExec(`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, title) VALUES (7, 1, 'photo', '2626,-70', 52.53681, -1.39994, 'Hinckley, United Kingdom')`)
	mustExec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (7, 'hotel-1', '2026-09-25T16:21:00Z', 52.53681, -1.39994)`)

	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatal(err)
	}
	pinOf := func(asset string) (id int64, kind string) {
		db.QueryRow(`SELECT p.id, p.kind FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id WHERE pp.asset_id = ?`, asset).Scan(&id, &kind)
		return
	}
	for _, asset := range []string{"hotel-1", "hotel-2"} {
		if id, _ := pinOf(asset); id != 5 {
			t.Errorf("%s is on pin %d, want the hotel pin 5", asset, id)
		}
	}
	if _, kind := pinOf("town"); kind != "photo" {
		t.Errorf("a photo 1.8 km from the hotel went to a %q pin, want a town photo pin", kind)
	}
	var lat float64
	db.QueryRow(`SELECT lat FROM pins WHERE id = 5`).Scan(&lat)
	if lat != 52.5369686 {
		t.Errorf("the hand-placed pin moved to %v", lat)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM pin_photos WHERE pin_id = 7`).Scan(&n)
	if n != 1 {
		t.Errorf("the old photo pin beside the hotel holds %d photos, want just the town one", n)
	}

	// removing the hand pin gives its photos back to a photo pin on the next sync
	mustExec(`DELETE FROM pins WHERE id = 5`)
	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatal(err)
	}
	if _, kind := pinOf("hotel-2"); kind != "photo" {
		t.Errorf("after deleting the hand pin hotel-2 is on a %q pin", kind)
	}
}

// Between "at the pin" and "clearly elsewhere" the trip asks. The photo waits
// on the pin meanwhile, and whichever answer is given survives every re-sync.
func TestSyncAsksAboutPhotosNearAHandPin(t *testing.T) {
	restaurant := `{"id":"restaurant","type":"IMAGE","fileCreatedAt":"2026-09-26T18:30:44Z","exifInfo":{"latitude":52.536877,"longitude":-1.391319}}`
	items := []string{
		`{"id":"hotel","type":"IMAGE","fileCreatedAt":"2026-09-26T17:15:00Z","exifInfo":{"latitude":52.5370,"longitude":-1.4003}}`,
		restaurant,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/search/metadata" {
			w.Write([]byte(`{}`))
			return
		}
		w.Write([]byte(`{"assets":{"items":[` + strings.Join(items, ",") + `],"nextPage":null}}`))
	}))
	defer srv.Close()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, immich: newImmichClient(srv.URL, "key"), lastSync: map[int64]time.Time{}}
	db.Exec(`INSERT INTO holidays (id, name, color, start_at) VALUES (1, 'Warwick', '#123456', '2026-09-25T00:00:00Z')`)
	db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, title) VALUES (5, 1, 'manual', 52.5369686, -1.4002928, 'Premier Inn')`)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/holidays/{id}/ask", a.handleAskPhotos)
	mux.HandleFunc("POST /api/holidays/{id}/ask", a.handleAnswerPhotos)
	answer := func(same bool) {
		t.Helper()
		body := `{"asset_ids":["restaurant"],"same":` + strconv.FormatBool(same) + `}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/holidays/1/ask", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"answered":1`) {
			t.Fatalf("answer: %d %s", rec.Code, rec.Body)
		}
	}
	sync := func() {
		t.Helper()
		if _, _, err := a.syncHoliday(1); err != nil {
			t.Fatal(err)
		}
	}
	where := func(asset string) (pin int64, kind string, ask int) {
		db.QueryRow(`SELECT p.id, p.kind, pp.ask FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id WHERE pp.asset_id = ?`, asset).Scan(&pin, &kind, &ask)
		return
	}

	sync()
	if pin, _, ask := where("hotel"); pin != 5 || ask != 0 {
		t.Errorf("hotel photo: pin %d ask %d, want pin 5 without a question", pin, ask)
	}
	if pin, _, ask := where("restaurant"); pin != 5 || ask != 1 {
		t.Fatalf("600 m photo: pin %d ask %d, want it waiting on pin 5 with a question", pin, ask)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/holidays/1/ask", nil))
	if !strings.Contains(rec.Body.String(), `"asset_id":"restaurant"`) || strings.Contains(rec.Body.String(), `"hotel"`) {
		t.Errorf("questions: %s", rec.Body)
	}

	answer(true)
	sync()
	if pin, _, ask := where("restaurant"); pin != 5 || ask != 0 {
		t.Errorf("after 'same place' + re-sync: pin %d ask %d", pin, ask)
	}

	// changing one's mind is a fresh question: put it back and say "own stop"
	db.Exec(`UPDATE pin_photos SET ask = 1 WHERE asset_id = 'restaurant'`)
	answer(false) // re-syncs inline
	if _, kind, ask := where("restaurant"); kind != "photo" || ask != 0 {
		t.Errorf("after 'its own stop': on a %q pin, ask %d", kind, ask)
	}
	sync()
	if _, kind, ask := where("restaurant"); kind != "photo" || ask != 0 {
		t.Errorf("'its own stop' undone by a re-sync: %q pin, ask %d", kind, ask)
	}

	// deleted in Immich: the photo and its answer both go
	items = items[:1]
	sync()
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM photo_choices`).Scan(&n)
	if n != 0 {
		t.Errorf("%d answers left for a photo that's gone", n)
	}
}
