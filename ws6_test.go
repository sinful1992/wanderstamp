package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// WS-6 (audit 137, team task 142): a sync whose search returns only part of a
// trip's photos (a library offline, a search index being rebuilt) must not
// delete the missing photos and the titled pins that held them. A photo the
// search did not return is removed only when Immich, asked about that one
// photo, says it is gone: not found, in the trash, archived or hidden, or now
// taken outside the trip.

type ws6Asset struct {
	status     int // answer to GET /api/assets/{id}; 0 = 200
	trashed    bool
	visibility string
	created    string
}

func ws6Server(t *testing.T, search []string, assets map[string]ws6Asset) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/search/metadata":
			items := []map[string]any{}
			for i, id := range search {
				items = append(items, map[string]any{"id": id, "type": "IMAGE",
					"fileCreatedAt": time.Date(2026, 7, 2, 10, i, 0, 0, time.UTC).Format(time.RFC3339),
					"exifInfo":      map[string]any{"latitude": 50.0, "longitude": -1.8}})
			}
			json.NewEncoder(w).Encode(map[string]any{"assets": map[string]any{"items": items, "nextPage": nil}})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/assets/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/assets/")
			mu.Lock()
			asked = append(asked, id)
			mu.Unlock()
			a, ok := assets[id]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"message":"Not found or no asset.read access"}`))
				return
			}
			if a.status != 0 && a.status != 200 {
				w.WriteHeader(a.status)
				w.Write([]byte(`{"message":"x"}`))
				return
			}
			vis := a.visibility
			if vis == "" {
				vis = "timeline"
			}
			created := a.created
			if created == "" {
				created = "2026-07-02T10:00:00.000Z"
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "type": "IMAGE", "isTrashed": a.trashed,
				"visibility": vis, "fileCreatedAt": created})
		default:
			// albums and the rest: accept quietly
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func ws6Trip(t *testing.T, srvURL string, photos int) *app {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := &app{db: db, immich: newImmichClient(srvURL, "key"), lastSync: map[int64]time.Time{}}
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	must(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Trip', '#123456', '2026-07-01T00:00:00Z', '2026-07-08T00:00:00Z')`)
	// 3 titled photo pins far apart, photos spread over them
	for p := 1; p <= 3; p++ {
		must(`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, title) VALUES (?, 1, 'photo', ?, ?, ?, ?)`,
			p, "k"+string(rune('0'+p)), 50.0+float64(p), -1.8, "Title "+string(rune('0'+p)))
	}
	for i := 0; i < photos; i++ {
		p := 1 + i%3
		must(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (?, ?, '2026-07-02T10:00:00Z', ?, -1.8)`,
			p, "ph"+string(rune('a'+i)), 50.0+float64(p))
	}
	return a
}

func ws6Count(t *testing.T, a *app, q string) int {
	t.Helper()
	var n int
	if err := a.db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAudit137_WS6_PartialAnswerKeepsPinsAndTitles(t *testing.T) {
	present := map[string]ws6Asset{}
	for i := 0; i < 10; i++ {
		present["ph"+string(rune('a'+i))] = ws6Asset{}
	}
	srv, asked := ws6Server(t, []string{"pha"}, present)
	a := ws6Trip(t, srv.URL, 10)
	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n := ws6Count(t, a, `SELECT COUNT(*) FROM pin_photos`); n != 10 {
		t.Errorf("photos left after a partial answer: %d, want 10", n)
	}
	if n := ws6Count(t, a, `SELECT COUNT(*) FROM pins WHERE title LIKE 'Title %'`); n != 3 {
		t.Errorf("titled pins left: %d, want 3", n)
	}
	if len(*asked) != 9 {
		t.Errorf("asked Immich about %d photos, want the 9 missing ones", len(*asked))
	}
}

func TestAudit137_WS6_ConfirmedGonePhotosArePruned(t *testing.T) {
	assets := map[string]ws6Asset{
		"pha": {},                                // in the search
		"phb": {status: 404},                     // deleted
		"phc": {},                                // missing from search, still there -> kept
		"phd": {trashed: true},                   // in the trash
		"phe": {visibility: "archive"},           // archived
		"phf": {visibility: "hidden"},            // hidden
		"phg": {created: "2026-09-01T10:00:00Z"}, // date moved outside the trip
		"phh": {created: "2026-07-07T23:00:00Z"}, // inside the trip window (end grace) -> kept
		// "phi" not listed: 400 not found -> gone
	}
	srv, _ := ws6Server(t, []string{"pha"}, assets)
	a := ws6Trip(t, srv.URL, 9)
	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatalf("sync: %v", err)
	}
	rows, err := a.db.Query(`SELECT asset_id FROM pin_photos ORDER BY asset_id`)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		left = append(left, id)
	}
	rows.Close()
	if got := strings.Join(left, ","); got != "pha,phc,phh" {
		t.Errorf("photos left = %s, want pha,phc,phh", got)
	}
}

func TestAudit137_WS6_AssetCheckFailureKeepsEverything(t *testing.T) {
	assets := map[string]ws6Asset{"pha": {}, "phb": {status: 500}, "phc": {status: 404}}
	srv, _ := ws6Server(t, []string{"pha"}, assets)
	a := ws6Trip(t, srv.URL, 3)
	if _, _, err := a.syncHoliday(1); err == nil {
		t.Error("sync reported success while Immich failed to answer about a photo")
	}
	if n := ws6Count(t, a, `SELECT COUNT(*) FROM pin_photos`); n != 3 {
		t.Errorf("photos left after a failed check: %d, want 3 (nothing pruned)", n)
	}
}

func TestAudit137_WS6_FullAnswerAsksNothing(t *testing.T) {
	present := map[string]ws6Asset{"pha": {}, "phb": {}, "phc": {}}
	srv, asked := ws6Server(t, []string{"pha", "phb", "phc"}, present)
	a := ws6Trip(t, srv.URL, 3)
	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(*asked) != 0 {
		t.Errorf("asked about %v though the search returned every photo", *asked)
	}
}
