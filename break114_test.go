package main

// Break tests for v1.14.0 (F2/F4/F6/F7/F9/F10), written from the contract only.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func b114Mux(a *app, user string) *http.ServeMux {
	mux := featMux(a, user)
	mux.HandleFunc("POST /api/holidays/{id}/tracks", as(user, a.handleUploadTrack))
	mux.HandleFunc("GET /api/holidays/{id}/tracks", as(user, a.handleListTracks))
	mux.HandleFunc("DELETE /api/tracks/{id}", as(user, a.handleDeleteTrack))
	mux.HandleFunc("GET /api/holidays/{id}/ask", as(user, a.handleAskPhotos))
	mux.HandleFunc("GET /api/share/{token}/pins/{id}/photos", a.handleSharePinPhotos)
	mux.HandleFunc("GET /api/share/{token}/photo/{asset}/{kind}", a.handleSharePhoto)
	return mux
}

func b114Exec(t *testing.T, a *app, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func b114Count(a *app, q string, args ...any) int {
	var n int
	a.db.QueryRow(q, args...).Scan(&n)
	return n
}

// F2: undo must not overwrite a row another trip got meanwhile with a reused id
// (ids are INTEGER PRIMARY KEY without AUTOINCREMENT, so max+1 comes back).
func TestBreak114UndoChildPinIdReused(t *testing.T) {
	a := newTestApp(t)
	b114Exec(t, a,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Del', '#123456', '2026-05-01T00:00:00Z', '2026-05-03T00:00:00Z')`,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (2, 'Keep', '#123456', '2025-05-01T00:00:00Z', '2025-05-03T00:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title) VALUES (20, 2, 'manual', 50, 0, 'Keep')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title) VALUES (21, 1, 'manual', 41, 12, 'Gone')`,
	)
	mux := b114Mux(a, "mum")
	tok, _ := decode(t, serve(mux, "DELETE", "/api/holidays/1", ""))["undo"].(string)
	rec := serve(mux, "POST", "/api/pins", `{"holiday_id":2,"lat":51,"lng":1,"title":"New on Keep"}`)
	p := decode(t, rec)
	if rec.Code != 201 || p["id"].(float64) != 21 {
		t.Fatalf("setup: new pin %d %v (want id 21 reused)", rec.Code, p)
	}
	rec = serve(mux, "POST", "/api/undo/"+tok, "")
	n := b114Count(a, `SELECT COUNT(*) FROM pins WHERE holiday_id = 2 AND title = 'New on Keep'`)
	if rec.Code == 200 && n != 1 {
		t.Errorf("undo answered 200 but replaced trip 2's new pin (id 21): it now has %d such pins", n)
	}
	if rec.Code != 200 && rec.Code != 409 {
		t.Errorf("undo: %d %s", rec.Code, rec.Body)
	}
}

// Same for a track uploaded to another trip after the delete.
func TestBreak114UndoChildTrackIdReused(t *testing.T) {
	a := newTestApp(t)
	b114Exec(t, a,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Del', '#123456', '2026-05-01T00:00:00Z', '2026-05-03T00:00:00Z')`,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (2, 'Keep', '#123456', '2025-05-01T00:00:00Z', '2025-05-03T00:00:00Z')`,
		`INSERT INTO tracks (id, holiday_id, name, points) VALUES (6, 2, 'keep', '[[1,1],[2,2]]')`,
		`INSERT INTO tracks (id, holiday_id, name, points) VALUES (7, 1, 'old', '[[1,1],[2,2]]')`,
	)
	mux := b114Mux(a, "mum")
	tok, _ := decode(t, serve(mux, "DELETE", "/api/holidays/1", ""))["undo"].(string)
	rec := uploadGPX(t, mux, 2, "w.gpx", gpx(10))
	if o := decode(t, rec); rec.Code != 201 || o["id"].(float64) != 7 {
		t.Fatalf("setup: upload %d %v (want id 7 reused)", rec.Code, o)
	}
	rec = serve(mux, "POST", "/api/undo/"+tok, "")
	n := b114Count(a, `SELECT COUNT(*) FROM tracks WHERE holiday_id = 2`) - 1
	if rec.Code == 200 && n != 1 {
		t.Errorf("undo answered 200 but trip 2 lost its new track (has %d)", n)
	}
}

// F2: delete a photo pin, the resync (resyncSoon + next map load) files its
// photos again; undo within 30 s should still work.
func TestBreak114UndoPhotoPinAfterResync(t *testing.T) {
	a, _ := f7App(t, nil)
	mux := b114Mux(a, "mum")
	var pid int64
	var key string
	if err := a.db.QueryRow(`SELECT id, cluster_key FROM pins WHERE kind = 'photo' AND cluster_key IS NOT NULL LIMIT 1`).Scan(&pid, &key); err != nil {
		t.Fatalf("setup: no photo pin: %v", err)
	}
	tok, _ := decode(t, serve(mux, "DELETE", fmt.Sprintf("/api/pins/%d", pid), ""))["undo"].(string)
	if tok == "" {
		t.Fatal("no undo token")
	}
	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatal(err)
	}
	if b114Count(a, `SELECT COUNT(*) FROM pins WHERE holiday_id = 1 AND cluster_key = ?`, key) != 1 {
		t.Fatalf("setup: resync did not recreate a pin with key %s", key)
	}
	rec := serve(mux, "POST", "/api/undo/"+tok, "")
	if rec.Code != 200 {
		t.Errorf("undo after a resync: %d %s (want 200 within 30 s)", rec.Code, rec.Body)
	}
}

// F2: delete a pin, then its trip; undo the trip, then the pin: both in time.
func TestBreak114UndoPinThenTripOrder(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	mux := b114Mux(a, "mum")
	tp, _ := decode(t, serve(mux, "DELETE", "/api/pins/11", ""))["undo"].(string)
	th, _ := decode(t, serve(mux, "DELETE", "/api/holidays/1", ""))["undo"].(string)
	if rec := serve(mux, "POST", "/api/undo/"+th, ""); rec.Code != 200 {
		t.Fatalf("trip undo: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(mux, "POST", "/api/undo/"+tp, ""); rec.Code != 200 {
		t.Errorf("pin undo after trip undo: %d %s", rec.Code, rec.Body)
	}
	if b114Count(a, `SELECT COUNT(*) FROM pins WHERE id IN (10, 11)`) != 2 {
		t.Error("pins 10/11 not both back")
	}
}

// F2: undo of an orphan pin (its trip deleted, not undone) leaves no orphan.
func TestBreak114UndoPinOfDeletedTrip(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	mux := b114Mux(a, "mum")
	tp, _ := decode(t, serve(mux, "DELETE", "/api/pins/11", ""))["undo"].(string)
	serve(mux, "DELETE", "/api/holidays/1", "")
	rec := serve(mux, "POST", "/api/undo/"+tp, "")
	if rec.Code == 200 || rec.Code >= 500 {
		t.Errorf("undo pin of a gone trip: %d %s", rec.Code, rec.Body)
	}
	if b114Count(a, `SELECT COUNT(*) FROM pins WHERE holiday_id = 1`) != 0 {
		t.Error("orphan pin restored")
	}
}

// F2: only a restored LIVE trip conflicts with a live one; ended/planned do not.
func TestBreak114UndoEndedAndPlannedWhileLive(t *testing.T) {
	a := newTestApp(t)
	b114Exec(t, a,
		`INSERT INTO holidays (id, name, color, start_at) VALUES (1, 'Live', '#123456', '2026-10-01T00:00:00Z')`,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (2, 'Ended', '#123456', '2025-05-01T00:00:00Z', '2025-05-03T00:00:00Z')`,
		`INSERT INTO holidays (id, name, color, start_at, planned) VALUES (3, 'Plan', '#123456', '2031-05-01T00:00:00Z', 1)`,
	)
	mux := b114Mux(a, "mum")
	for _, id := range []int{2, 3} {
		tok, _ := decode(t, serve(mux, "DELETE", fmt.Sprintf("/api/holidays/%d", id), ""))["undo"].(string)
		if rec := serve(mux, "POST", "/api/undo/"+tok, ""); rec.Code != 200 {
			t.Errorf("undo trip %d while 1 is live: %d %s", id, rec.Code, rec.Body)
		}
	}
}

// F2: concurrent undos of one token: exactly one 200.
func TestBreak114UndoConcurrentSameToken(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	mux := b114Mux(a, "mum")
	tok, _ := decode(t, serve(mux, "DELETE", "/api/holidays/1", ""))["undo"].(string)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = serve(mux, "POST", "/api/undo/"+tok, "").Code
		}(i)
	}
	wg.Wait()
	ok, gone := 0, 0
	for _, c := range codes {
		switch c {
		case 200:
			ok++
		case 410:
			gone++
		}
	}
	if ok != 1 || gone != 7 {
		t.Errorf("codes %v", codes)
	}
	if rec := serve(mux, "POST", "/api/undo/"+tok, ""); strings.TrimSpace(rec.Body.String()) != `{"error":"too late to undo"}` {
		t.Errorf("410 body: %s", rec.Body)
	}
}

// F4: km follows visits — a revisit (London, Paris, back to the London pin) is two legs.
func TestBreak114StatsRevisitTwoLegs(t *testing.T) {
	a := newTestApp(t)
	b114Exec(t, a,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Loop', '#123456', '2025-07-01T00:00:00Z', '2025-07-04T00:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, country) VALUES (1, 1, 'photo', 'ldn', 51.5074, -0.1278, 'United Kingdom')`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, country) VALUES (2, 1, 'photo', 'par', 48.8566, 2.3522, 'France')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (1, 'a1', '2025-07-01T12:00:00Z', 51.5074, -0.1278)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (2, 'a2', '2025-07-02T12:00:00Z', 48.8566, 2.3522)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (1, 'a3', '2025-07-03T12:00:00Z', 51.5074, -0.1278)`,
	)
	_, _, pins := getMap(t, a)
	visits := 0
	for _, p := range pins {
		visits += len(p.Visits)
	}
	if visits != 3 {
		t.Fatalf("setup: the map shows %d visits, want 3", visits)
	}
	rec := serve(b114Mux(a, "mum"), "GET", "/api/stats", "")
	var out struct{ Trips []statTrip }
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Trips) != 1 || out.Trips[0].KM < 680 || out.Trips[0].KM > 700 {
		t.Errorf("revisit trip km: %+v, want ~688 (two legs in visit order)", out.Trips)
	}
}

// F4: visit order is not id order; days/year are UTC calendar days.
func TestBreak114StatsOrderAndUTC(t *testing.T) {
	a := newTestApp(t)
	b114Exec(t, a,
		// 2026-01-01T00:30+02:00 is 2025-12-31 22:30 UTC: a 2025 trip, 2 UTC days
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'NYE', '#123456', '2026-01-01T00:30:00+02:00', '2026-01-01T12:00:00+02:00')`,
		// ids out of visit order: A(3) London, B(1) Paris, C(2) Rome
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES (3, 1, 'manual', 51.5074, -0.1278, '2025-12-31T22:40:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES (1, 1, 'manual', 48.8566, 2.3522, '2025-12-31T23:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES (2, 1, 'manual', 41.9028, 12.4964, '2026-01-01T09:00:00Z')`,
	)
	rec := serve(b114Mux(a, "mum"), "GET", "/api/stats", "")
	var out struct {
		Years []statYear
		Trips []statTrip
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	want := haversineKM(51.5074, -0.1278, 48.8566, 2.3522) + haversineKM(48.8566, 2.3522, 41.9028, 12.4964)
	if len(out.Trips) != 1 || math.Abs(out.Trips[0].KM-want) > 0.2 {
		t.Errorf("km %+v, want %.1f", out.Trips, want)
	}
	if len(out.Years) != 1 || out.Years[0].Year != 2025 || out.Years[0].Days != 2 {
		t.Errorf("years %+v, want 2025 with 2 UTC days", out.Years)
	}
}

// F7: share video is 403 with the exact body; other kinds are not streams.
func TestBreak114ShareVideoExactBody(t *testing.T) {
	_, mux := f7App(t, nil)
	rec := serve(mux, "POST", "/api/holidays/1/share", `{"days":7}`)
	tok := strings.TrimPrefix(decode(t, rec)["url"].(string), "/share/")
	rec = serve(mux, "GET", "/api/share/"+tok+"/photo/00000000-0000-0000-0000-00000000000b/video", "")
	if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != `{"error":"not available on a share link"}` {
		t.Errorf("share video: %d %s", rec.Code, rec.Body)
	}
	for _, k := range []string{"VIDEO", "Video", "playback", "fullsize", "original%2F"} {
		rec := serve(mux, "GET", "/api/share/"+tok+"/photo/00000000-0000-0000-0000-00000000000b/"+k, "")
		if rec.Code == 200 {
			t.Errorf("share kind %q streamed: %d", k, rec.Code)
		}
	}
	if rec := serve(mux, "GET", "/api/share/nope/photo/00000000-0000-0000-0000-00000000000b/video", ""); rec.Code != 404 {
		t.Errorf("unknown token video: %d", rec.Code)
	}
}

// F7: ask list and share photo list carry type/duration, old rows image/0.
func TestBreak114AskAndShareListsTyped(t *testing.T) {
	a := newTestApp(t)
	b114Exec(t, a,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'T', '#123456', '2025-07-01T00:00:00Z', '2025-07-03T00:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title) VALUES (1, 1, 'manual', 50, 0, 'Hotel')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng, ask) VALUES (1, '00000000-0000-0000-0000-0000000000c1', '2025-07-01T10:00:00Z', 50.001, 0, 1)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng, ask, type, duration) VALUES (1, '00000000-0000-0000-0000-0000000000c2', '2025-07-01T11:00:00Z', 50.001, 0, 1, 'video', 4.5)`,
	)
	mux := b114Mux(a, "mum")
	ask := getRaw(t, mux, "GET", "/api/holidays/1/ask")
	if !strings.Contains(ask, `"asset_id":"00000000-0000-0000-0000-0000000000c1","taken_at":"2025-07-01T10:00:00Z","type":"image","duration":0`) ||
		!strings.Contains(ask, `"type":"video","duration":4.5`) {
		t.Errorf("ask list: %s", ask)
	}
	tok, _ := makeShareFor(t, mux, 1)
	sh := getRaw(t, mux, "GET", "/api/share/"+tok+"/pins/1/photos")
	if !strings.Contains(sh, `"type":"image","duration":0`) || !strings.Contains(sh, `"type":"video","duration":4.5`) {
		t.Errorf("share list: %s", sh)
	}
}

// F6: name-only PATCH keeps journal_by; share JSON carries "" for all three.
func TestBreak114AddedByShareAndPatch(t *testing.T) {
	a := newTestApp(t)
	rec := serve(featMux(a, "mum"), "POST", "/api/holidays", `{"name":"Paris","color":"#112233","start_at":"2026-04-01","end_at":"2026-04-03"}`)
	id := int(decode(t, rec)["id"].(float64))
	serve(featMux(a, "dad"), "POST", "/api/pins", fmt.Sprintf(`{"holiday_id":%d,"lat":48.8,"lng":2.3,"title":"Louvre"}`, id))
	serve(featMux(a, "kid"), "PATCH", fmt.Sprintf("/api/holidays/%d", id), `{"journal":"great"}`)
	serve(featMux(a, "gran"), "PATCH", fmt.Sprintf("/api/holidays/%d", id), `{"name":"Paris!"}`)
	_, hs, _ := getMap(t, a)
	if len(hs) != 1 || hs[0].JournalBy != "kid" || hs[0].CreatedBy != "mum" {
		t.Errorf("after a name edit: %+v", hs)
	}
	mux := b114Mux(a, "mum")
	tok, _ := makeShareFor(t, mux, id)
	var sh struct {
		Holiday map[string]any   `json:"holiday"`
		Pins    []map[string]any `json:"pins"`
	}
	json.Unmarshal([]byte(getRaw(t, mux, "GET", "/api/share/"+tok)), &sh)
	if sh.Holiday["created_by"] != "" || sh.Holiday["journal_by"] != "" {
		t.Errorf("share holiday: created_by %v journal_by %v", sh.Holiday["created_by"], sh.Holiday["journal_by"])
	}
	for _, p := range sh.Pins {
		if v, ok := p["created_by"]; !ok || v != "" {
			t.Errorf("share pin created_by %v (present %v)", v, ok)
		}
	}
}

// F9: ids are new and pins point at the new trip; existing rows untouched.
func TestBreak114ImportRemapsIds(t *testing.T) {
	export := seedExport(t) // holidays 1,2; pins 10,11,12
	b := newTestApp(t)
	b114Exec(t, b,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Mine', '#123456', '2023-01-01T00:00:00Z', '2023-01-02T00:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title) VALUES (10, 1, 'manual', 1, 1, 'MyPin')`,
	)
	rec := serve(importMux(b, true), "POST", "/api/import", export)
	if rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if b114Count(b, `SELECT COUNT(*) FROM pins WHERE id = 10 AND holiday_id = 1 AND title = 'MyPin'`) != 1 ||
		b114Count(b, `SELECT COUNT(*) FROM holidays WHERE id = 1 AND name = 'Mine'`) != 1 {
		t.Error("an existing row was overwritten")
	}
	if n := b114Count(b, `SELECT COUNT(*) FROM pins p JOIN holidays h ON h.id = p.holiday_id WHERE h.name = 'Paris'`); n != 2 {
		t.Errorf("Paris has %d pins, want 2", n)
	}
	if n := b114Count(b, `SELECT COUNT(*) FROM pins WHERE holiday_id = 1`); n != 1 {
		t.Errorf("imported pins landed on the existing trip 1: %d", n)
	}
}

// F9: a dest_area the app would never write must not break the map afterwards.
func TestBreak114ImportOddAreaKeepsMapWorking(t *testing.T) {
	b := newTestApp(t)
	for _, area := range []string{`[1,2]`, `{"type":"Polygon","coordinates":"x"}`, `{}`} {
		body := `{"holidays":[{"name":"A` + area + `","color":"#123456","start_at":"2020-01-01T00:00:00Z","end_at":"2020-01-02T00:00:00Z","dest_name":"X","dest_lat":1,"dest_lng":1,"dest_area":` + area + `,"pins":[{"kind":"manual","lat":1,"lng":1}]}]}`
		rec := serve(importMux(b, true), "POST", "/api/import", body)
		if rec.Code != 200 && rec.Code != 400 {
			t.Errorf("import area %s: %d %s", area, rec.Code, rec.Body)
		}
	}
	rec := serveMap(b)
	if rec != 200 {
		t.Errorf("/api/map after odd areas: %d", rec)
	}
}

func serveMap(a *app) int {
	mux := featMux(a, "mum")
	return serve(mux, "GET", "/api/map", "").Code
}

// F9: trailing data / oversize after a valid value (minor: decoder stops early).
func TestBreak114ImportTrailingData(t *testing.T) {
	b := newTestApp(t)
	mux := importMux(b, true)
	if rec := serve(mux, "POST", "/api/import", `{"holidays":[]}garbage`); rec.Code != 400 {
		t.Errorf("trailing garbage: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(mux, "POST", "/api/import", `{"holidays":[]}`+strings.Repeat(" ", 21<<20)); rec.Code != 413 {
		t.Errorf("21 MB body (valid value first): %d", rec.Code)
	}
}

// F10: not a .gpx file name; exactly 2000/2001 points.
func TestBreak114TracksNameAndLimits(t *testing.T) {
	a := newTestApp(t)
	b114Exec(t, a, `INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'T', '#123456', '2026-05-01T00:00:00Z', '2026-05-03T00:00:00Z')`)
	mux := b114Mux(a, "mum")
	if rec := uploadGPX(t, mux, 1, "photo.jpg", gpx(5)); rec.Code != 400 {
		t.Errorf(".jpg file name with GPX content: %d %s (contract: .gpx)", rec.Code, rec.Body)
	}
	for _, n := range []int{2000, 2001} {
		rec := uploadGPX(t, mux, 1, "x.gpx", gpx(n))
		o := decode(t, rec)
		if rec.Code != 201 || o["points"].(float64) > 2000 {
			t.Errorf("%d points: %d %v", n, rec.Code, o)
		}
	}
	// a <kml> root with trk inside should not pass as GPX
	if rec := uploadGPX(t, mux, 1, "x.gpx", `<kml><trk><trkseg><trkpt lat="1" lon="1"/><trkpt lat="2" lon="2"/></trkseg></trk></kml>`); rec.Code != 400 {
		t.Errorf("non-gpx root accepted: %d", rec.Code)
	}
	var list []struct{ Points [][2]float64 }
	json.Unmarshal([]byte(getRaw(t, mux, "GET", "/api/holidays/1/tracks")), &list)
	for i, tr := range list {
		if len(tr.Points) > 2000 {
			t.Errorf("track %d has %d points", i, len(tr.Points))
		}
	}
}
