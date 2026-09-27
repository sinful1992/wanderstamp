package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// A photo set aside leaves the trip and stays off through every re-sync;
// put back, it is filed again as if it had never gone. Immich is only read.
func TestSetAsidePhotoStaysOffUntilPutBack(t *testing.T) {
	const castle = "11111111-1111-1111-1111-111111111111"
	const selfie = "22222222-2222-2222-2222-222222222222"
	const noGPS = "33333333-3333-3333-3333-333333333333"
	var writes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/search/metadata" {
			if r.Method == "DELETE" {
				writes++
			}
			w.Write([]byte(`{}`))
			return
		}
		w.Write([]byte(`{"assets":{"items":[
			{"id":"` + castle + `","type":"IMAGE","fileCreatedAt":"2026-09-26T10:31:20Z","exifInfo":{"latitude":52.2803,"longitude":-1.5857}},
			{"id":"` + selfie + `","type":"IMAGE","fileCreatedAt":"2026-09-26T11:38:20Z","exifInfo":{"latitude":52.2794,"longitude":-1.5877}},
			{"id":"` + noGPS + `","type":"IMAGE","fileCreatedAt":"2026-09-26T12:00:00Z"}
		],"nextPage":null}}`))
	}))
	defer srv.Close()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, immich: newImmichClient(srv.URL, "key"), lastSync: map[int64]time.Time{}}
	db.Exec(`INSERT INTO holidays (id, name, color, start_at) VALUES (1, 'Warwick', '#123456', '2026-09-25T00:00:00Z')`)
	db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, title) VALUES (4, 1, 'manual', 52.2811, -1.5842, 'Warwick Castle')`)
	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatal(err)
	}

	do := func(method, asset string) int {
		req := httptest.NewRequest(method, "/api/photos/"+asset+"/aside", nil)
		req.SetPathValue("asset", asset)
		rec := httptest.NewRecorder()
		if method == "POST" {
			a.handleSetAside(rec, req)
		} else {
			a.handlePutBack(rec, req)
		}
		return rec.Code
	}
	count := func(q string, args ...any) (n int) {
		db.QueryRow(q, args...).Scan(&n)
		return
	}
	onTrip := func(asset string) bool {
		return count(`SELECT COUNT(*) FROM pin_photos WHERE asset_id = ?`, asset)+
			count(`SELECT COUNT(*) FROM unplaced_photos WHERE asset_id = ?`, asset) > 0
	}

	if code := do("POST", selfie); code != 200 {
		t.Fatalf("set aside: %d", code)
	}
	if code := do("POST", noGPS); code != 200 {
		t.Fatalf("set aside a photo with no location: %d", code)
	}
	if onTrip(selfie) || onTrip(noGPS) || !onTrip(castle) {
		t.Fatalf("after setting aside: selfie %v, no-GPS %v, castle %v", onTrip(selfie), onTrip(noGPS), onTrip(castle))
	}
	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatal(err)
	}
	if onTrip(selfie) || onTrip(noGPS) {
		t.Error("a re-sync put a set-aside photo back")
	}
	_, hs, pins := getMap(t, a)
	if hs[0].SetAsideCount != 2 || hs[0].PhotoCount != 1 || pins[0].Visits[len(pins[0].Visits)-1].N != 1 {
		t.Errorf("trip = %d set aside, %d photos; visits %+v", hs[0].SetAsideCount, hs[0].PhotoCount, pins[0].Visits)
	}
	// its thumb still loads, for the list to put it back from
	if count(`SELECT COUNT(*) FROM set_aside WHERE asset_id = ?`, selfie) != 1 {
		t.Error("selfie is not listed as set aside")
	}

	if code := do("DELETE", selfie); code != 200 {
		t.Fatalf("put back: %d", code)
	}
	if !onTrip(selfie) || count(`SELECT pin_id FROM pin_photos WHERE asset_id = ?`, selfie) != 4 {
		t.Error("the put-back photo isn't on the castle pin again")
	}
	if do("DELETE", selfie) != 404 || do("POST", "44444444-4444-4444-4444-444444444444") != 404 {
		t.Error("unknown photos should 404")
	}
	if writes != 0 {
		t.Errorf("%d deletes were sent to Immich", writes)
	}
}
