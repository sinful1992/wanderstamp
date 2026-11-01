package main

// Tests for the v1.14.0 server features (task 112, F2/F4/F6/F7/F9/F10).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// as wraps a handler with a signed-in family member, as auth() does.
func as(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, sessionUser{ID: 1, Username: name})))
	}
}

func featMux(a *app, user string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/map", as(user, a.handleMap))
	mux.HandleFunc("POST /api/holidays", as(user, a.handleCreateHoliday))
	mux.HandleFunc("PATCH /api/holidays/{id}", as(user, a.handleUpdateHoliday))
	mux.HandleFunc("DELETE /api/holidays/{id}", as(user, a.handleDeleteHoliday))
	mux.HandleFunc("POST /api/holidays/{id}/end", as(user, a.handleEndHoliday))
	mux.HandleFunc("POST /api/holidays/{id}/reopen", as(user, a.handleReopenHoliday))
	mux.HandleFunc("POST /api/undo/{token}", as(user, a.handleUndo))
	mux.HandleFunc("POST /api/pins", as(user, a.handleCreatePin))
	mux.HandleFunc("DELETE /api/pins/{id}", as(user, a.handleDeletePin))
	mux.HandleFunc("GET /api/stats", as(user, a.handleStats))
	mux.HandleFunc("POST /api/holidays/{id}/share", a.handleCreateShare)
	mux.HandleFunc("GET /api/share/{token}", a.handleShareData)
	return mux
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON (%d): %s", rec.Code, rec.Body)
	}
	return out
}

// cascadeTables walks pragma foreign_key_list from root, independently of
// undo.go, so a table added later is checked without editing this test.
func cascadeTables(t *testing.T, a *app, root string) []string {
	t.Helper()
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		rows, _ := a.db.Query(`SELECT m.name FROM sqlite_master m, pragma_foreign_key_list(m.name) f
			WHERE m.type = 'table' AND f."table" = ? AND upper(f.on_delete) = 'CASCADE'`, p)
		var kids []string
		for rows.Next() {
			var n string
			rows.Scan(&n)
			kids = append(kids, n)
		}
		rows.Close()
		for _, k := range kids {
			if !seen[k] {
				seen[k] = true
				queue = append(queue, k)
			}
		}
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dump is every row of each table as text, to compare before and after.
func dump(t *testing.T, a *app, tables []string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, tb := range tables {
		rows, err := a.db.Query(`SELECT * FROM "` + tb + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			out[tb] = append(out[tb], fmt.Sprint(vals...))
		}
		rows.Close()
		sort.Strings(out[tb])
	}
	return out
}

const uuidA = "aaaaaaaa-0000-0000-0000-000000000001"
const uuidB = "aaaaaaaa-0000-0000-0000-000000000002"

// a trip with a row in every table that hangs off it
func seedFullTrip(t *testing.T, a *app) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO holidays (id, name, color, start_at, end_at, journal) VALUES (1, 'Rome', '#123456', '2026-05-01T00:00:00Z', '2026-05-05T23:59:59Z', 'nice')`,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (2, 'Other', '#123456', '2025-05-01T00:00:00Z', '2025-05-02T23:59:59Z')`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, title) VALUES (10, 1, 'photo', 'k1', 41.9, 12.5, 'Colosseum')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title) VALUES (11, 1, 'manual', 41.8, 12.4, 'Hotel')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title) VALUES (20, 2, 'manual', 50, 0, 'Keep')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (10, '` + uuidA + `', '2026-05-02T10:00:00Z', 41.9, 12.5)`,
		`INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at) VALUES (1, '` + uuidB + `', '2026-05-03T10:00:00Z')`,
		`INSERT INTO set_aside (asset_id, holiday_id, taken_at, set_at) VALUES ('bbbbbbbb-0000-0000-0000-000000000003', 1, '2026-05-03T11:00:00Z', '2026-05-06T00:00:00Z')`,
		`INSERT INTO photo_choices (asset_id, holiday_id, pin_id) VALUES ('` + uuidA + `', 1, 10)`,
		`INSERT INTO packing_items (holiday_id, label, checked) VALUES (1, 'Hat', 1)`,
		`INSERT INTO shares (token, holiday_id, expires_at) VALUES ('hashed', 1, NULL)`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// F2: deleting a trip and undoing it gives back every row, ids included.
func TestUndoTripDeleteRestoresEveryRow(t *testing.T) {
	a := newTestApp(t)
	seedTracksTable(t, a)
	seedFullTrip(t, a)
	tables := cascadeTables(t, a, "holidays")
	if len(tables) < 8 {
		t.Fatalf("cascade from holidays found only %v", tables)
	}
	before := dump(t, a, tables)
	mux := featMux(a, "mum")

	rec := serve(mux, "DELETE", "/api/holidays/1", "")
	out := decode(t, rec)
	tok, _ := out["undo"].(string)
	if rec.Code != 200 || out["ok"] != true || len(tok) < 16 {
		t.Fatalf("delete: %d %v", rec.Code, out)
	}
	for _, tb := range tables {
		var n int
		a.db.QueryRow(`SELECT COUNT(*) FROM "` + tb + `" WHERE ` + map[bool]string{true: "id", false: "holiday_id"}[tb == "holidays"] + ` = 1`).Scan(&n)
		if tb != "pin_photos" && n != 0 {
			t.Errorf("%s still has %d rows of the deleted trip", tb, n)
		}
	}
	rec = serve(mux, "POST", "/api/undo/"+tok, "")
	if rec.Code != 200 {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body)
	}
	after := dump(t, a, tables)
	for _, tb := range tables {
		if strings.Join(before[tb], "\n") != strings.Join(after[tb], "\n") {
			t.Errorf("%s differs after undo:\nbefore %v\nafter  %v", tb, before[tb], after[tb])
		}
	}
	// a token works once
	if rec := serve(mux, "POST", "/api/undo/"+tok, ""); rec.Code != http.StatusGone {
		t.Errorf("second undo: %d, want 410", rec.Code)
	}
}

// seedTracksTable makes sure a tracks table exists, so the cascade walk is
// tested on a table undo.go never names.
func seedTracksTable(t *testing.T, a *app) {
	a.db.Exec(`CREATE TABLE IF NOT EXISTS tracks (id INTEGER PRIMARY KEY,
		holiday_id INTEGER NOT NULL REFERENCES holidays(id) ON DELETE CASCADE, name TEXT NOT NULL, points TEXT NOT NULL)`)
	a.db.Exec(`INSERT INTO tracks (holiday_id, name, points) VALUES (1, 'walk', '[]')`)
}

func TestUndoTooLateUnknownAndPinDelete(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	mux := featMux(a, "mum")

	if rec := serve(mux, "POST", "/api/undo/nope", ""); rec.Code != http.StatusGone {
		t.Errorf("unknown token: %d", rec.Code)
	}
	if rec := serve(mux, "POST", "/api/undo/", ""); rec.Code == 200 {
		t.Errorf("empty token answered 200")
	}

	rec := serve(mux, "DELETE", "/api/pins/10", "")
	tok, _ := decode(t, rec)["undo"].(string)
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM pin_photos WHERE pin_id = 10`).Scan(&n)
	if n != 0 || tok == "" {
		t.Fatalf("pin delete: photos %d, token %q", n, tok)
	}
	// age the snapshot past the window
	a.undo.mu.Lock()
	s := a.undo.snaps[tok]
	s.at = time.Now().Add(-undoWindow - time.Second)
	a.undo.snaps[tok] = s
	a.undo.mu.Unlock()
	if rec := serve(mux, "POST", "/api/undo/"+tok, ""); rec.Code != http.StatusGone || decode(t, rec)["error"] != "too late to undo" {
		t.Errorf("late undo: %d %s", rec.Code, rec.Body)
	}

	// pin delete + undo in time: pin and its photo are back
	rec = serve(mux, "DELETE", "/api/pins/11", "")
	tok, _ = decode(t, rec)["undo"].(string)
	if rec := serve(mux, "POST", "/api/undo/"+tok, ""); rec.Code != 200 {
		t.Fatalf("pin undo: %d %s", rec.Code, rec.Body)
	}
	a.db.QueryRow(`SELECT COUNT(*) FROM pins WHERE id = 11 AND title = 'Hotel'`).Scan(&n)
	if n != 1 {
		t.Error("pin 11 not restored")
	}

	// deleting nothing gives no token
	out := decode(t, serve(mux, "DELETE", "/api/pins/999", ""))
	if _, has := out["undo"]; has || out["ok"] != true {
		t.Errorf("delete of a missing pin: %v", out)
	}
}

// A sync may file the deleted pin's photo on another pin within the 30 s;
// the undo takes it back rather than failing.
func TestUndoPinAfterPhotoRefiled(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	mux := featMux(a, "mum")
	tok, _ := decode(t, serve(mux, "DELETE", "/api/pins/10", ""))["undo"].(string)
	a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (11, ?, '2026-05-02T10:00:00Z', 41.9, 12.5)`, uuidA)
	if rec := serve(mux, "POST", "/api/undo/"+tok, ""); rec.Code != 200 {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body)
	}
	var pin int64
	a.db.QueryRow(`SELECT pin_id FROM pin_photos WHERE asset_id = ?`, uuidA).Scan(&pin)
	if pin != 10 {
		t.Errorf("photo on pin %d after undo, want 10", pin)
	}
}

// Restoring a live trip while another went live meanwhile is a 409, and the
// snapshot isn't half-applied.
func TestUndoLiveTripConflict(t *testing.T) {
	a := newTestApp(t)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at) VALUES (1, 'Live', '#123456', '2026-05-01T00:00:00Z')`)
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng) VALUES (5, 1, 'manual', 1, 1)`)
	mux := featMux(a, "mum")
	tok, _ := decode(t, serve(mux, "DELETE", "/api/holidays/1", ""))["undo"].(string)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at) VALUES (2, 'New live', '#123456', '2026-05-02T00:00:00Z')`)
	if rec := serve(mux, "POST", "/api/undo/"+tok, ""); rec.Code != http.StatusConflict {
		t.Fatalf("undo over a live trip: %d %s", rec.Code, rec.Body)
	}
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM pins WHERE id = 5`).Scan(&n)
	if n != 0 {
		t.Error("pin restored although the trip was refused")
	}
}

func TestReopenTrip(t *testing.T) {
	a := newTestApp(t)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Done', '#123456', '2026-05-01T00:00:00Z', '2026-05-05T23:59:59Z')`)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (2, 'Older', '#123456', '2025-05-01T00:00:00Z', '2025-05-05T23:59:59Z')`)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, planned) VALUES (3, 'Plan', '#123456', '2030-05-01T00:00:00Z', 1)`)
	mux := featMux(a, "mum")

	rec := serve(mux, "POST", "/api/holidays/1/reopen", "")
	var h holidayOut
	json.Unmarshal(rec.Body.Bytes(), &h)
	if rec.Code != 200 || h.ID != 1 || h.EndAt != nil || !h.Active {
		t.Fatalf("reopen: %d %s", rec.Code, rec.Body)
	}
	for path, want := range map[string]int{
		"/api/holidays/1/reopen":   400, // already live
		"/api/holidays/2/reopen":   409, // another trip is live
		"/api/holidays/3/reopen":   400, // planned
		"/api/holidays/99/reopen":  404,
		"/api/holidays/abc/reopen": 400,
	} {
		if rec := serve(mux, "POST", path, ""); rec.Code != want {
			t.Errorf("%s: %d, want %d (%s)", path, rec.Code, want, rec.Body)
		}
	}
}

// F6: who added a trip, a pin and the journal; shares never say.
func TestAddedBy(t *testing.T) {
	a := newTestApp(t)
	mux := featMux(a, "mum")
	rec := serve(mux, "POST", "/api/holidays", `{"name":"Paris","color":"#112233","start_at":"2026-04-01","end_at":"2026-04-03"}`)
	h := decode(t, rec)
	if rec.Code != 201 || h["created_by"] != "mum" {
		t.Fatalf("create trip: %d %v", rec.Code, h)
	}
	id := int(h["id"].(float64))
	rec = serve(featMux(a, "dad"), "POST", "/api/pins", fmt.Sprintf(`{"holiday_id":%d,"lat":48.8,"lng":2.3,"title":"Louvre"}`, id))
	if p := decode(t, rec); rec.Code != 201 || p["created_by"] != "dad" {
		t.Fatalf("create pin: %d %v", rec.Code, p)
	}
	serve(featMux(a, "kid"), "PATCH", fmt.Sprintf("/api/holidays/%d", id), `{"journal":"great"}`)
	a.db.Exec(`INSERT INTO pins (holiday_id, kind, lat, lng) VALUES (?, 'manual', 1, 1)`, id) // an old row

	_, hs, pins := getMap(t, a)
	if len(hs) != 1 || hs[0].CreatedBy != "mum" || hs[0].JournalBy != "kid" {
		t.Errorf("map trip: %+v", hs)
	}
	by := map[string]int{}
	for _, p := range pins {
		by[p.CreatedBy]++
	}
	if by["dad"] != 1 || by[""] != 1 {
		t.Errorf("map pins created_by: %v", by)
	}
	raw := getRaw(t, mux, "GET", "/api/map")
	if !strings.Contains(raw, `"created_by":""`) || !strings.Contains(raw, `"journal_by":"kid"`) {
		t.Errorf("map JSON lacks the fields: %.300s", raw)
	}

	tok, _ := makeShareFor(t, mux, id)
	share := getRaw(t, mux, "GET", "/api/share/"+tok)
	for _, who := range []string{"mum", "dad", "kid"} {
		if strings.Contains(share, `"`+who+`"`) {
			t.Errorf("share names %s: %.400s", who, share)
		}
	}
}

func getRaw(t *testing.T, mux http.Handler, method, path string) string {
	t.Helper()
	rec := serve(mux, method, path, "")
	if rec.Code != 200 {
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func makeShareFor(t *testing.T, mux http.Handler, id int) (string, map[string]any) {
	t.Helper()
	rec := serve(mux, "POST", fmt.Sprintf("/api/holidays/%d/share", id), `{"days":30}`)
	if rec.Code != 200 {
		t.Fatalf("share: %d %s", rec.Code, rec.Body)
	}
	out := decode(t, rec)
	return strings.TrimPrefix(out["url"].(string), "/share/"), out
}

// F4: per-year trips, calendar days and km; countries by trips; planned left out.
func TestStats(t *testing.T) {
	a := newTestApp(t)
	for _, q := range []string{
		// 23:00 on the 1st to 01:00 on the 3rd is 3 calendar days
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Paris', '#123456', '2025-07-01T23:00:00Z', '2025-07-03T01:00:00Z')`,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (2, 'Lyon', '#123456', '2025-09-10T00:00:00Z', '2025-09-10T20:00:00Z')`,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (3, 'Rome', '#123456', '2026-05-01T00:00:00Z', '2026-05-02T00:00:00Z')`,
		`INSERT INTO holidays (id, name, color, start_at, planned) VALUES (4, 'Future', '#123456', '2031-01-01T00:00:00Z', 1)`,
		// London -> Paris -> back to London: ~344 km each way
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, country, created_at) VALUES (1, 1, 'manual', 51.5074, -0.1278, 'United Kingdom', '2025-07-01T23:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, country, created_at) VALUES (2, 1, 'manual', 48.8566, 2.3522, 'France', '2025-07-02T10:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, country, created_at) VALUES (3, 1, 'manual', 51.5074, -0.1278, 'United Kingdom', '2025-07-03T00:30:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, country) VALUES (4, 2, 'manual', 45.76, 4.83, 'France')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, country) VALUES (5, 3, 'manual', 41.9, 12.5, 'Italy')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, country) VALUES (6, 4, 'manual', 40, -3, 'Spain')`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	rec := serve(featMux(a, "mum"), "GET", "/api/stats", "")
	var out struct {
		Years     []statYear
		Trips     []statTrip
		Countries []statCountry
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(out.Years) != 2 || out.Years[0].Year != 2026 || out.Years[1].Year != 2025 {
		t.Fatalf("years: %+v", out.Years)
	}
	y := out.Years[1]
	if y.Trips != 2 || y.Days != 4 || y.KM < 680 || y.KM > 700 {
		t.Errorf("2025: %+v, want 2 trips, 4 days, ~688 km", y)
	}
	if out.Years[0].Days != 2 || out.Years[0].KM != 0 {
		t.Errorf("2026: %+v", out.Years[0])
	}
	if len(out.Trips) != 3 {
		t.Errorf("trips (planned must be left out): %+v", out.Trips)
	}
	want := []statCountry{
		{"France", 2, 1, "2025-07-01T23:00:00Z"},
		{"Italy", 1, 3, "2026-05-01T00:00:00Z"},
		{"United Kingdom", 1, 1, "2025-07-01T23:00:00Z"},
	}
	if fmt.Sprint(out.Countries) != fmt.Sprint(want) {
		t.Errorf("countries:\n got %+v\nwant %+v", out.Countries, want)
	}
	// an empty database answers empty lists, not null
	empty := getRaw(t, featMux(newTestApp(t), "mum"), "GET", "/api/stats")
	if strings.TrimSpace(empty) != `{"years":[],"trips":[],"countries":[]}` {
		t.Errorf("empty: %s", empty)
	}
}

func uploadGPX(t *testing.T, mux http.Handler, id int, filename, body string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write([]byte(body))
	mw.Close()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/holidays/%d/tracks", id), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func gpx(n int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><gpx version="1.1" xmlns="http://www.topografix.com/GPX/1/1"><trk><name>Coast walk</name><trkseg>`)
	for i := 0; i < n; i++ {
		// a wiggly line, so simplifying has real shape to keep
		fmt.Fprintf(&b, `<trkpt lat="%.6f" lon="%.6f"><ele>10</ele></trkpt>`, 50+float64(i)*0.0001, -1+0.001*math.Sin(float64(i)/7))
	}
	b.WriteString(`</trkseg></trk></gpx>`)
	return b.String()
}

// F10: upload, list (simplified to ≤ 2,000 points), delete; gone with the
// trip, back with an undo, and never on a share.
func TestTracks(t *testing.T) {
	a := newTestApp(t)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Dorset', '#123456', '2026-05-01T00:00:00Z', '2026-05-03T00:00:00Z')`)
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng) VALUES (1, 1, 'manual', 50, -1)`)
	mux := featMux(a, "mum")
	mux.HandleFunc("POST /api/holidays/{id}/tracks", a.handleUploadTrack)
	mux.HandleFunc("GET /api/holidays/{id}/tracks", a.handleListTracks)
	mux.HandleFunc("DELETE /api/tracks/{id}", a.handleDeleteTrack)

	rec := uploadGPX(t, mux, 1, "walk.gpx", gpx(20000))
	out := decode(t, rec)
	if rec.Code != 201 || out["name"] != "Coast walk" || out["points"].(float64) > 2000 || out["points"].(float64) < 100 {
		t.Fatalf("upload: %d %v", rec.Code, out)
	}
	// a route-only file, no name: the file name is used
	rec = uploadGPX(t, mux, 1, "Day two.gpx", `<gpx><rte><rtept lat="50.1" lon="-1.1"/><rtept lat="50.2" lon="-1.2"/></rte></gpx>`)
	if o := decode(t, rec); rec.Code != 201 || o["name"] != "Day two" || o["points"].(float64) != 2 {
		t.Fatalf("route upload: %d %v", rec.Code, o)
	}
	for name, body := range map[string]string{
		"not xml":    "hello",
		"no points":  `<gpx><trk><trkseg></trkseg></trk></gpx>`,
		"one point":  `<gpx><trk><trkseg><trkpt lat="1" lon="1"/></trkseg></trk></gpx>`,
		"bad coords": `<gpx><trk><trkseg><trkpt lat="91" lon="1"/><trkpt lat="1" lon="200"/></trkseg></trk></gpx>`,
	} {
		if rec := uploadGPX(t, mux, 1, "x.gpx", body); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := uploadGPX(t, mux, 99, "x.gpx", gpx(5)); rec.Code != 404 {
		t.Errorf("missing trip: %d", rec.Code)
	}
	if rec := uploadGPX(t, mux, 1, "big.gpx", strings.Repeat(" ", maxGPXBytes+1)); rec.Code != 413 {
		t.Errorf("11 MB file: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(mux, "POST", "/api/holidays/1/tracks", `{"x":1}`); rec.Code != 400 {
		t.Errorf("JSON instead of a file: %d", rec.Code)
	}

	var list []struct {
		ID     int64
		Name   string
		Points [][2]float64
	}
	json.Unmarshal([]byte(getRaw(t, mux, "GET", "/api/holidays/1/tracks")), &list)
	if len(list) != 2 || len(list[0].Points) > 2000 || list[0].Points[0] != [2]float64{50, -1} {
		t.Fatalf("list: %d tracks, first %v", len(list), list)
	}
	if last := list[0].Points[len(list[0].Points)-1]; math.Abs(last[0]-(50+19999*0.0001)) > 1e-6 {
		t.Errorf("simplify lost the end point: %v", last)
	}
	if strings.TrimSpace(getRaw(t, mux, "GET", "/api/holidays/7/tracks")) != "[]" {
		t.Error("tracks of a trip without any should be []")
	}

	tok, _ := makeShareFor(t, mux, 1)
	if share := getRaw(t, mux, "GET", "/api/share/"+tok); strings.Contains(share, "Coast walk") || strings.Contains(share, "track") {
		t.Errorf("share includes tracks: %.300s", share)
	}

	if rec := serve(mux, "DELETE", fmt.Sprintf("/api/tracks/%d", list[1].ID), ""); rec.Code != 200 {
		t.Errorf("delete track: %d", rec.Code)
	}
	if rec := serve(mux, "DELETE", "/api/tracks/999", ""); rec.Code != 404 {
		t.Errorf("delete missing track: %d", rec.Code)
	}
	undo, _ := decode(t, serve(mux, "DELETE", "/api/holidays/1", ""))["undo"].(string)
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM tracks`).Scan(&n)
	if n != 0 {
		t.Errorf("%d tracks left after the trip was deleted", n)
	}
	serve(mux, "POST", "/api/undo/"+undo, "")
	a.db.QueryRow(`SELECT COUNT(*) FROM tracks WHERE holiday_id = 1`).Scan(&n)
	if n != 1 {
		t.Errorf("undo brought back %d tracks, want 1", n)
	}
}

func TestSimplifyStaysUnderMax(t *testing.T) {
	// noise defeats Douglas-Peucker; the stride fallback still caps it
	pts := make([][2]float64, 50000)
	for i := range pts {
		pts[i] = [2]float64{float64(i%2) * 0.5, float64(i) * 0.00001}
	}
	if got := simplifyTrack(pts, 2000); len(got) > 2000 || got[0] != pts[0] || got[len(got)-1] != pts[len(pts)-1] {
		t.Errorf("simplify: %d points, ends %v %v", len(got), got[0], got[len(got)-1])
	}
	if got := simplifyTrack(pts[:10], 2000); len(got) != 10 {
		t.Errorf("short track changed: %d", len(got))
	}
}
