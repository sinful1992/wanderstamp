package main

// Tests for the v1.14.0 server features (task 112, F2/F4/F6/F7/F9/F10).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// --- F7 video ---

func TestImmichDurationSeconds(t *testing.T) {
	cases := []struct {
		typ, dur string
		want     float64
	}{
		{"VIDEO", "0:00:12.345678", 12.3},
		{"VIDEO", "1:02:03.000000", 3723},
		{"VIDEO", "00:05.5", 5.5},
		{"VIDEO", "", 0},
		{"VIDEO", "garbage", 0},
		{"VIDEO", "1:2:3:4", 0},
		{"VIDEO", "-0:00:05", 0},
		{"VIDEO", "NaN", 0},
		{"IMAGE", "0:00:00.00000", 0},
		{"IMAGE", "0:00:09", 0},
	}
	for _, c := range cases {
		if got := (immichAsset{Type: c.typ, Duration: c.dur}).seconds(); got != c.want {
			t.Errorf("%s %q: %v, want %v", c.typ, c.dur, got, c.want)
		}
	}
}

// fakeImmichF7 answers the search with two photos, two videos and an audio
// file, and serves video playback with Range (http.ServeContent).
func fakeImmichF7(t *testing.T, searchBody *string) *httptest.Server {
	t.Helper()
	film := []byte("0123456789abcdefghij")
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/search/metadata":
			if searchBody != nil {
				b, _ := io.ReadAll(r.Body)
				*searchBody = string(b)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"assets":{"items":[
				{"id":"00000000-0000-0000-0000-00000000000a","type":"IMAGE","duration":"0:00:00.00000","fileCreatedAt":"2026-07-01T10:00:00Z","exifInfo":{"latitude":48.85,"longitude":2.35,"city":"Paris","country":"France"}},
				{"id":"00000000-0000-0000-0000-00000000000b","type":"VIDEO","duration":"0:00:12.345678","fileCreatedAt":"2026-07-01T10:05:00Z","exifInfo":{"latitude":48.851,"longitude":2.351,"city":"Paris","country":"France"}},
				{"id":"00000000-0000-0000-0000-00000000000c","type":"VIDEO","duration":"0:01:30.000000","fileCreatedAt":"2026-07-01T11:00:00Z","exifInfo":{}},
				{"id":"00000000-0000-0000-0000-00000000000d","type":"IMAGE","fileCreatedAt":"2026-07-01T12:00:00Z"},
				{"id":"00000000-0000-0000-0000-00000000000e","type":"AUDIO","duration":"0:03:00.0","fileCreatedAt":"2026-07-01T12:30:00Z","exifInfo":{"latitude":48.85,"longitude":2.35}}
			],"nextPage":null}}`))
		case r.URL.Path == "/api/assets/00000000-0000-0000-0000-00000000000b/video/playback":
			if r.Header.Get("x-api-key") != "key" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(film))
		case strings.HasPrefix(r.URL.Path, "/api/assets/") && strings.HasSuffix(r.URL.Path, "/video/playback"):
			w.WriteHeader(400) // Immich: not a video
		case strings.HasPrefix(r.URL.Path, "/api/assets/"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("jpeg"))
		default:
			w.Write([]byte(`{}`))
		}
	}))
}

func f7App(t *testing.T, searchBody *string) (*app, *http.ServeMux) {
	t.Helper()
	srv := fakeImmichF7(t, searchBody)
	t.Cleanup(srv.Close)
	a := newTestApp(t)
	a.immich = newImmichClient(srv.URL, "key")
	a.lastSync = map[int64]time.Time{}
	if _, err := a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES
		(1, 'Paris', '#123456', '2026-07-01T00:00:00Z', '2026-07-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/pins/{id}/photos", as("ann", a.handlePinPhotos))
	mux.HandleFunc("GET /api/holidays/{id}/timeline", as("ann", a.handleHolidayTimeline))
	mux.HandleFunc("GET /api/holidays/{id}/unplaced", as("ann", a.handleUnplacedPhotos))
	mux.HandleFunc("GET /api/holidays/{id}/ask", as("ann", a.handleAskPhotos))
	mux.HandleFunc("POST /api/pins/{id}/attach", as("ann", a.handleAttachPhotos))
	mux.HandleFunc("GET /api/photo/{asset}/{kind}", as("ann", a.handlePhoto))
	mux.HandleFunc("GET /api/export", as("ann", a.handleExport))
	mux.HandleFunc("POST /api/holidays/{id}/share", a.handleCreateShare)
	mux.HandleFunc("GET /api/share/{token}/pins/{id}/photos", a.handleSharePinPhotos)
	mux.HandleFunc("GET /api/share/{token}/photo/{asset}/{kind}", a.handleSharePhoto)
	return a, mux
}

func TestSyncTakesVideos(t *testing.T) {
	var body string
	a, mux := f7App(t, &body)
	if strings.Contains(body, `"type"`) {
		t.Errorf("search still filters by type: %s", body)
	}
	type row struct {
		typ string
		dur float64
	}
	got := map[string]row{}
	rows, _ := a.db.Query(`SELECT asset_id, type, duration FROM pin_photos UNION ALL SELECT asset_id, type, duration FROM unplaced_photos`)
	for rows.Next() {
		var id string
		var r row
		rows.Scan(&id, &r.typ, &r.dur)
		got[id] = r
	}
	rows.Close()
	want := map[string]row{"00000000-0000-0000-0000-00000000000a": {"image", 0}, "00000000-0000-0000-0000-00000000000b": {"video", 12.3}, "00000000-0000-0000-0000-00000000000c": {"video", 90}, "00000000-0000-0000-0000-00000000000d": {"image", 0}}
	if len(got) != len(want) {
		t.Errorf("synced %v, want %v (audio left out)", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %+v, want %+v", id, got[id], w)
		}
	}

	var pinID int64
	a.db.QueryRow(`SELECT pin_id FROM pin_photos WHERE asset_id = '00000000-0000-0000-0000-00000000000b'`).Scan(&pinID)
	var list []photoOut
	for _, path := range []string{fmt.Sprintf("/api/pins/%d/photos", pinID), "/api/holidays/1/timeline", "/api/holidays/1/unplaced"} {
		rec := serve(mux, "GET", path, "")
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		// every entry carries both keys, even an image
		var raw []map[string]any
		json.Unmarshal(rec.Body.Bytes(), &raw)
		for _, p := range raw {
			if _, ok := p["type"]; !ok {
				t.Errorf("%s: no type in %v", path, p)
			}
			if _, ok := p["duration"]; !ok {
				t.Errorf("%s: no duration in %v", path, p)
			}
		}
		json.Unmarshal(rec.Body.Bytes(), &list)
		for _, p := range list {
			if w := want[p.AssetID]; p.Type != w.typ || p.Duration != w.dur {
				t.Errorf("%s: %+v, want %+v", path, p, w)
			}
		}
	}

	// a re-sync is idempotent and keeps the type
	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatal(err)
	}
	var typ string
	a.db.QueryRow(`SELECT type FROM pin_photos WHERE asset_id = '00000000-0000-0000-0000-00000000000b'`).Scan(&typ)
	if typ != "video" {
		t.Errorf("after re-sync 00000000-0000-0000-0000-00000000000b is %q", typ)
	}

	// attaching an unplaced video to a pin keeps it a video
	rec := serve(mux, "POST", fmt.Sprintf("/api/pins/%d/attach", pinID), `{"asset_ids":["00000000-0000-0000-0000-00000000000c"]}`)
	if rec.Code != 200 {
		t.Fatalf("attach: %d %s", rec.Code, rec.Body)
	}
	var dur float64
	a.db.QueryRow(`SELECT type, duration FROM pin_photos WHERE asset_id = '00000000-0000-0000-0000-00000000000c'`).Scan(&typ, &dur)
	if typ != "video" || dur != 90 {
		t.Errorf("attached 00000000-0000-0000-0000-00000000000c is %q %v, want video 90", typ, dur)
	}
	// and the export carries it, for F9 import
	if !strings.Contains(serve(mux, "GET", "/api/export", "").Body.String(), `"type":"video","duration":90`) {
		t.Error("export leaves out the video type")
	}
}

func TestOldPhotoRowsReadAsImages(t *testing.T) {
	a := newTestApp(t)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at) VALUES (1, 'Old', '#123456', '2025-07-01T00:00:00Z')`)
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng) VALUES (3, 1, 'photo', 1, 1)`)
	// an insert as 1.13 wrote it, naming no type
	if _, err := a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (3, 'old', '2025-07-01T10:00:00Z', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/pins/{id}/photos", as("ann", a.handlePinPhotos))
	body := serve(mux, "GET", "/api/pins/3/photos", "").Body.String()
	if !strings.Contains(body, `"type":"image","duration":0`) {
		t.Errorf("old row: %s", body)
	}
}

func TestVideoStreamHonoursRange(t *testing.T) {
	_, mux := f7App(t, nil)
	req := httptest.NewRequest("GET", "/api/photo/00000000-0000-0000-0000-00000000000b/video", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 206 || rec.Body.String() != "2345" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 2-5/20" {
		t.Errorf("Content-Range %q", cr)
	}
	if rec.Header().Get("Content-Type") != "video/mp4" || rec.Header().Get("Content-Length") != "4" {
		t.Errorf("headers %v", rec.Header())
	}
	// whole file without Range
	if rec := serve(mux, "GET", "/api/photo/00000000-0000-0000-0000-00000000000b/video", ""); rec.Code != 200 || rec.Body.Len() != 20 || rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("full: %d %d %v", rec.Code, rec.Body.Len(), rec.Header())
	}
	// past the end: 416 passes through, not a 502
	req = httptest.NewRequest("GET", "/api/photo/00000000-0000-0000-0000-00000000000b/video", nil)
	req.Header.Set("Range", "bytes=50-60")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 416 {
		t.Errorf("unsatisfiable range: %d", rec.Code)
	}
	// a photo has no video; an unknown asset is still unknown
	if rec := serve(mux, "GET", "/api/photo/00000000-0000-0000-0000-00000000000a/video", ""); rec.Code != 404 {
		t.Errorf("video of a photo: %d", rec.Code)
	}
	if rec := serve(mux, "GET", "/api/photo/00000000-0000-0000-0000-00000000000f/video", ""); rec.Code != 404 {
		t.Errorf("unknown asset: %d", rec.Code)
	}
	// a video's thumb is its poster frame, the usual thumbnail route
	if rec := serve(mux, "GET", "/api/photo/00000000-0000-0000-0000-00000000000b/thumb", ""); rec.Code != 200 || rec.Body.String() != "jpeg" {
		t.Errorf("poster: %d %q", rec.Code, rec.Body)
	}
}

func TestShareVideo(t *testing.T) {
	a, mux := f7App(t, nil)
	rec := serve(mux, "POST", "/api/holidays/1/share", `{"days":7}`)
	url, _ := decode(t, rec)["url"].(string)
	token := strings.TrimPrefix(url, "/share/")
	if token == "" {
		t.Fatalf("share: %d %s", rec.Code, rec.Body)
	}
	var pinID int64
	a.db.QueryRow(`SELECT pin_id FROM pin_photos WHERE asset_id = '00000000-0000-0000-0000-00000000000b'`).Scan(&pinID)
	body := serve(mux, "GET", fmt.Sprintf("/api/share/%s/pins/%d/photos", token, pinID), "").Body.String()
	if !strings.Contains(body, `"asset_id":"00000000-0000-0000-0000-00000000000b","taken_at":"2026-07-01T10:05:00Z","type":"video","duration":12.3`) {
		t.Errorf("share photos: %s", body)
	}
	// never the video file on a share: it can be the original, with its GPS
	for _, asset := range []string{"00000000-0000-0000-0000-00000000000b", "00000000-0000-0000-0000-00000000000a"} {
		if rec := serve(mux, "GET", "/api/share/"+token+"/photo/"+asset+"/video", ""); rec.Code != 403 {
			t.Errorf("share video of %s: %d", asset, rec.Code)
		}
	}
	// its poster still shows
	if rec := serve(mux, "GET", "/api/share/"+token+"/photo/00000000-0000-0000-0000-00000000000b/preview", ""); rec.Code != 200 {
		t.Errorf("share poster: %d", rec.Code)
	}
	if rec := serve(mux, "GET", "/api/share/"+token+"/photo/00000000-0000-0000-0000-00000000000b/original", ""); rec.Code != 403 {
		t.Errorf("share original: %d", rec.Code)
	}
	// an expired link stops the poster too
	a.db.Exec(`UPDATE shares SET expires_at = '2000-01-01T00:00:00Z'`)
	if rec := serve(mux, "GET", "/api/share/"+token+"/photo/00000000-0000-0000-0000-00000000000b/preview", ""); rec.Code != 410 {
		t.Errorf("expired share poster: %d", rec.Code)
	}
}

// --- F9 import ---

func asAdmin(admin bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, sessionUser{ID: 1, Username: "ann", IsAdmin: admin})))
	}
}

func importMux(a *app, isAdmin bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/import", asAdmin(isAdmin, admin(a.handleImport)))
	mux.HandleFunc("GET /api/export", as("ann", a.handleExport))
	return mux
}

func counts(t *testing.T, a *app) string {
	t.Helper()
	var h, p, ph, pk int
	a.db.QueryRow(`SELECT (SELECT COUNT(*) FROM holidays), (SELECT COUNT(*) FROM pins),
		(SELECT COUNT(*) FROM pin_photos), (SELECT COUNT(*) FROM packing_items)`).Scan(&h, &p, &ph, &pk)
	return fmt.Sprintf("%d/%d/%d/%d", h, p, ph, pk)
}

const vidA = "00000000-0000-0000-0000-0000000000a1"
const imgA = "00000000-0000-0000-0000-0000000000a2"

func seedExport(t *testing.T) string {
	t.Helper()
	a := newTestApp(t)
	for _, q := range []string{
		`INSERT INTO holidays (id, name, color, start_at, end_at, journal, dest_name, dest_lat, dest_lng, dest_area)
		 VALUES (1, 'Paris', '#123456', '2025-07-01T00:00:00Z', '2025-07-08T00:00:00Z', 'Lovely', 'Paris', 48.85, 2.35, '{"type":"Polygon"}')`,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (2, 'Rome', '#654321', '2024-05-01T00:00:00Z', '2024-05-03T00:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, title, country) VALUES (10, 1, 'photo', '2425,117', 48.85, 2.35, 'Paris, France', 'France')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title, note) VALUES (11, 1, 'manual', 48.86, 2.29, 'Hotel', 'nice')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title) VALUES (12, 2, 'manual', 41.9, 12.5, 'Colosseum')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng, type, duration) VALUES (10, '` + vidA + `', '2025-07-02T10:00:00Z', 48.85, 2.35, 'video', 12.5)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (10, '` + imgA + `', '2025-07-02T11:00:00Z', 48.85, 2.35)`,
		`INSERT INTO packing_items (holiday_id, label, checked, sort) VALUES (1, 'Passport', 1, 0), (1, 'Sun cream', 0, 1)`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return serve(importMux(a, true), "GET", "/api/export", "").Body.String()
}

func TestImportRoundTrip(t *testing.T) {
	export := seedExport(t)
	b := newTestApp(t)
	mux := importMux(b, true)

	rec := serve(mux, "POST", "/api/import?dry_run=1", export)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"holidays":2,"pins":3,"photos":2,"packing":2,"skipped":0}` {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	if c := counts(t, b); c != "0/0/0/0" {
		t.Fatalf("dry run changed the DB: %s", c)
	}
	rec = serve(mux, "POST", "/api/import", export)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"holidays":2,"pins":3,"photos":2,"packing":2,"skipped":0}` {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if c := counts(t, b); c != "2/3/2/2" {
		t.Errorf("after import: %s", c)
	}
	// the same file again: both trips are there already
	rec = serve(mux, "POST", "/api/import", export)
	if strings.TrimSpace(rec.Body.String()) != `{"holidays":0,"pins":0,"photos":0,"packing":0,"skipped":2}` {
		t.Errorf("re-import: %s", rec.Body)
	}
	if c := counts(t, b); c != "2/3/2/2" {
		t.Errorf("re-import changed the DB: %s", c)
	}
	// what came back: the fields, the video, the photo pin's cluster key
	var journal, area, key, typ string
	var dur float64
	var checked bool
	b.db.QueryRow(`SELECT journal, dest_area FROM holidays WHERE name = 'Paris'`).Scan(&journal, &area)
	b.db.QueryRow(`SELECT cluster_key FROM pins WHERE kind = 'photo'`).Scan(&key)
	b.db.QueryRow(`SELECT type, duration FROM pin_photos WHERE asset_id = ?`, vidA).Scan(&typ, &dur)
	b.db.QueryRow(`SELECT checked FROM packing_items WHERE label = 'Passport'`).Scan(&checked)
	if journal != "Lovely" || area != `{"type":"Polygon"}` || key == "" || typ != "video" || dur != 12.5 || !checked {
		t.Errorf("imported: journal %q area %q key %q video %q %v passport %v", journal, area, key, typ, dur, checked)
	}
	// the export of the import matches the original, apart from ids and date
	strip := func(s string) string {
		var doc map[string]any
		json.Unmarshal([]byte(s), &doc)
		hs := doc["holidays"].([]any)
		for _, h := range hs {
			hm := h.(map[string]any)
			delete(hm, "id")
			for _, p := range hm["pins"].([]any) {
				delete(p.(map[string]any), "id")
				delete(p.(map[string]any), "created_at")
			}
		}
		out, _ := json.Marshal(hs)
		return string(out)
	}
	if again := serve(mux, "GET", "/api/export", "").Body.String(); strip(again) != strip(export) {
		t.Errorf("round trip differs:\n%s\n%s", strip(export), strip(again))
	}
}

func TestImportRefusals(t *testing.T) {
	export := seedExport(t)
	b := newTestApp(t)
	if rec := serve(importMux(b, false), "POST", "/api/import", export); rec.Code != 403 {
		t.Errorf("non-admin: %d", rec.Code)
	}
	mux := importMux(b, true)
	for name, body := range map[string]string{
		"not json":      `{"holidays":`,
		"no holidays":   `{"version":"x"}`,
		"bad color":     `{"holidays":[{"name":"A","color":"red","start_at":"2025-01-01T00:00:00Z","pins":[]}]}`,
		"no name":       `{"holidays":[{"name":"  ","color":"#123456","start_at":"2025-01-01T00:00:00Z"}]}`,
		"bad start":     `{"holidays":[{"name":"A","color":"#123456","start_at":"yesterday"}]}`,
		"end first":     `{"holidays":[{"name":"A","color":"#123456","start_at":"2025-01-05T00:00:00Z","end_at":"2025-01-01T00:00:00Z"}]}`,
		"bad kind":      `{"holidays":[{"name":"A","color":"#123456","start_at":"2025-01-01T00:00:00Z","pins":[{"kind":"evil","lat":1,"lng":1}]}]}`,
		"bad lat":       `{"holidays":[{"name":"A","color":"#123456","start_at":"2025-01-01T00:00:00Z","pins":[{"kind":"manual","lat":91,"lng":1}]}]}`,
		"bad asset":     `{"holidays":[{"name":"A","color":"#123456","start_at":"2025-01-01T00:00:00Z","pins":[{"kind":"photo","lat":1,"lng":1,"photos":[{"asset_id":"../../etc","taken_at":"2025-01-01T00:00:00Z"}]}]}]}`,
		"bad type":      `{"holidays":[{"name":"A","color":"#123456","start_at":"2025-01-01T00:00:00Z","pins":[{"kind":"photo","lat":1,"lng":1,"photos":[{"asset_id":"` + imgA + `","taken_at":"2025-01-01T00:00:00Z","type":"exe"}]}]}]}`,
		"bad area":      `{"holidays":[{"name":"A","color":"#123456","start_at":"2025-01-01T00:00:00Z","dest_area":"x"}]}`,
		"good then bad": `{"holidays":[{"name":"Ok","color":"#123456","start_at":"2025-01-01T00:00:00Z"},{"name":"B","color":"no","start_at":"2025-01-01T00:00:00Z"}]}`,
	} {
		if rec := serve(mux, "POST", "/api/import", body); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if c := counts(t, b); c != "0/0/0/0" {
		t.Errorf("a refused import changed the DB: %s", c)
	}
	// form posts are refused, and a body over 20 MB gets 413
	req := httptest.NewRequest("POST", "/api/import", strings.NewReader(export))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Errorf("text/plain: %d", rec.Code)
	}
	big := `{"holidays":[{"name":"` + strings.Repeat("x", 21<<20) + `"}]}`
	if rec := serve(mux, "POST", "/api/import", big); rec.Code != 413 {
		t.Errorf("21 MB: %d", rec.Code)
	}
	// an empty export is fine and changes nothing
	if rec := serve(mux, "POST", "/api/import", `{"holidays":[]}`); rec.Code != 200 {
		t.Errorf("empty: %d %s", rec.Code, rec.Body)
	}
}

func TestImportKeepsLiveTripAndFiledPhotos(t *testing.T) {
	b := newTestApp(t)
	b.db.Exec(`INSERT INTO holidays (id, name, color, start_at) VALUES (1, 'Now', '#123456', '2026-10-30T00:00:00Z')`)
	b.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng) VALUES (5, 1, 'manual', 1, 1)`)
	b.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (5, ?, '2026-10-30T10:00:00Z', 1, 1)`, imgA)
	b.db.Exec(`INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at) VALUES (1, ?, '2026-10-30T11:00:00Z')`, vidA)
	mux := importMux(b, true)
	body := `{"holidays":[
		{"name":"Other live","color":"#123456","start_at":"2026-10-01T00:00:00Z","end_at":null},
		{"name":"Future","color":"#123456","start_at":"2030-01-01T00:00:00Z","end_at":null},
		{"name":"Old","color":"#123456","start_at":"2020-01-01T00:00:00Z","end_at":"2020-01-05T00:00:00Z","pins":[
			{"kind":"photo","lat":2,"lng":2,"photos":[
				{"asset_id":"` + imgA + `","taken_at":"2020-01-02T00:00:00Z","lat":2,"lng":2},
				{"asset_id":"` + vidA + `","taken_at":"2020-01-02T00:00:00Z","lat":2,"lng":2},
				{"asset_id":"00000000-0000-0000-0000-0000000000a3","taken_at":"2020-01-02T00:00:00Z","lat":2,"lng":2}]}]}]}`
	rec := serve(mux, "POST", "/api/import", body)
	if strings.TrimSpace(rec.Body.String()) != `{"holidays":2,"pins":1,"photos":1,"packing":0,"skipped":1}` {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	var planned bool
	var typ string
	b.db.QueryRow(`SELECT planned FROM holidays WHERE name = 'Future'`).Scan(&planned)
	b.db.QueryRow(`SELECT type FROM pin_photos WHERE asset_id = '00000000-0000-0000-0000-0000000000a3'`).Scan(&typ)
	if !planned || typ != "image" {
		t.Errorf("future trip planned=%v, old-export photo type %q", planned, typ)
	}
	var pin int64
	b.db.QueryRow(`SELECT pin_id FROM pin_photos WHERE asset_id = ?`, imgA).Scan(&pin)
	if pin != 5 {
		t.Errorf("a photo already filed moved to pin %d", pin)
	}
}

// The video route extends its write deadline past the server's 2 min
// WriteTimeout; that only works if every middleware wrapper unwraps.
func TestWriteDeadlineReachesConnThroughMiddleware(t *testing.T) {
	var got error = fmt.Errorf("handler not reached")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/photo/{asset}/{kind}", func(w http.ResponseWriter, r *http.Request) {
		got = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(videoWriteWindow))
		w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(requestLog(securityHeaders(gzipJSON(mux))))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/photo/00000000-0000-0000-0000-00000000000b/video", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != nil {
		t.Errorf("SetWriteDeadline through the middleware: %v", got)
	}
}
