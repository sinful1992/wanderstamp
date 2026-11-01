package main

// Break tests (round 2) for the #8187 review fixes: undo, stats, import, tracks.

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

const bom = "\xEF\xBB\xBF"

func b2Undo(t *testing.T, a *app, path string) string {
	t.Helper()
	tok, _ := decode(t, serve(b114Mux(a, "mum"), "DELETE", path, ""))["undo"].(string)
	if tok == "" {
		t.Fatalf("no undo token for %s", path)
	}
	return tok
}

func b2Tables(t *testing.T, a *app) []string {
	return cascadeTables(t, a, "holidays")
}

// 1. A 409 must roll back everything: the trip row, its pins and the
// asset-keyed rows already OR-REPLACEd before the conflicting track id.
func TestBreak2UndoConflictRollsBackAll(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	b114Exec(t, a, `INSERT INTO tracks (id, holiday_id, name, points) VALUES (7, 1, 'old', '[[1,1],[2,2]]')`)
	tok := b2Undo(t, a, "/api/holidays/1")
	// since the delete: trip 2 got the unplaced photo, uuidA on its pin, and track id 7
	b114Exec(t, a,
		`INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at) VALUES (2, '`+uuidB+`', '2026-05-03T10:00:00Z')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (20, '`+uuidA+`', '2026-05-02T10:00:00Z', 50, 0)`,
		`INSERT INTO tracks (id, holiday_id, name, points) VALUES (7, 2, 'new', '[[1,1],[2,2]]')`,
	)
	tables := b2Tables(t, a)
	before := dump(t, a, tables)
	rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, "")
	if rec.Code != 409 {
		t.Errorf("undo with reused track id: %d %s, want 409", rec.Code, rec.Body)
	}
	after := dump(t, a, tables)
	for _, tb := range tables {
		if strings.Join(before[tb], "\n") != strings.Join(after[tb], "\n") {
			t.Errorf("%s changed by a refused undo:\nbefore %v\nafter  %v", tb, before[tb], after[tb])
		}
	}
}

// 2. Asset-keyed OR REPLACE: a photo re-filed onto ANOTHER trip's pin since
// the delete is silently pulled back by the undo.
func TestBreak2UndoStealsPhotoFromOtherTrip(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	tok := b2Undo(t, a, "/api/holidays/1")
	b114Exec(t, a,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (20, '`+uuidA+`', '2026-05-02T10:00:00Z', 50, 0)`,
		`INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at) VALUES (2, '`+uuidB+`', '2026-05-03T10:00:00Z')`,
		`INSERT INTO photo_choices (asset_id, holiday_id, pin_id) VALUES ('`+uuidA+`', 2, 20)`,
	)
	rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, "")
	if rec.Code == 200 {
		lost := []string{}
		if b114Count(a, `SELECT COUNT(*) FROM pin_photos WHERE pin_id = 20 AND asset_id = ?`, uuidA) == 0 {
			lost = append(lost, "pin_photos uuidA on trip 2's pin 20")
		}
		if b114Count(a, `SELECT COUNT(*) FROM unplaced_photos WHERE holiday_id = 2 AND asset_id = ?`, uuidB) == 0 {
			lost = append(lost, "unplaced uuidB of trip 2")
		}
		if b114Count(a, `SELECT COUNT(*) FROM photo_choices WHERE holiday_id = 2 AND asset_id = ?`, uuidA) == 0 {
			lost = append(lost, "photo_choices of trip 2")
		}
		if len(lost) > 0 {
			t.Errorf("undo 200 moved rows from trip 2 back to trip 1 silently: %v", lost)
		}
	}
}

// 3. Twin with a user title (no note): the undo deletes it and the title is lost.
func TestBreak2UndoTwinWithTitleLost(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	tok := b2Undo(t, a, "/api/pins/10")
	// resync made a twin in cell k1 (new id 12), then someone named it
	b114Exec(t, a, `INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, title) VALUES (12, 1, 'photo', 'k1', 41.9, 12.5, 'Our picnic spot')`)
	rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, "")
	if n := b114Count(a, `SELECT COUNT(*) FROM pins WHERE title = 'Our picnic spot'`); n != 1 {
		t.Errorf("undo %d: the twin's hand-written title was deleted (want 409 like a note)", rec.Code)
	}
}

// 4. Twin with a photo attached by hand (from unplaced): the undo deletes the
// twin and its pin_photos cascade; nothing records the attach.
func TestBreak2UndoTwinWithAttachedPhotoLost(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	tok := b2Undo(t, a, "/api/pins/10")
	b114Exec(t, a,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (12, 1, 'photo', 'k1', 41.9, 12.5)`,
		`INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at) VALUES (1, 'cccccccc-0000-0000-0000-000000000009', '2026-05-02T12:00:00Z')`,
	)
	mux := b114Mux(a, "mum")
	mux.HandleFunc("POST /api/pins/{id}/attach", as("mum", a.handleAttachPhotos))
	if rec := serve(mux, "POST", "/api/pins/12/attach", `{"asset_ids":["cccccccc-0000-0000-0000-000000000009"]}`); rec.Code != 200 {
		t.Fatalf("attach: %d %s", rec.Code, rec.Body)
	}
	rec := serve(mux, "POST", "/api/undo/"+tok, "")
	n := b114Count(a, `SELECT (SELECT COUNT(*) FROM pin_photos WHERE asset_id = 'cccccccc-0000-0000-0000-000000000009') +
		(SELECT COUNT(*) FROM unplaced_photos WHERE asset_id = 'cccccccc-0000-0000-0000-000000000009')`)
	if n != 1 {
		t.Errorf("undo %d: the hand-attached photo vanished from both pin_photos and unplaced", rec.Code)
	}
}

// 5. A photo pin in a DIFFERENT cell that got the reused id is deleted as a
// "twin" (the DELETE matches id = ? too), with its title.
func TestBreak2UndoDeletesOtherCellPinById(t *testing.T) {
	a := newTestApp(t)
	b114Exec(t, a,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'T', '#123456', '2026-05-01T00:00:00Z', '2026-05-05T00:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (10, 1, 'photo', 'k1', 41.9, 12.5)`,
	)
	tok := b2Undo(t, a, "/api/pins/10")
	// a new photo pin elsewhere (cell k9) took id 10, and was named
	b114Exec(t, a,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, title) VALUES (10, 1, 'photo', 'k9', 45.0, 9.0, 'Milan')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (10, 'dddddddd-0000-0000-0000-000000000001', '2026-05-04T10:00:00Z', 45, 9)`,
	)
	rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, "")
	if b114Count(a, `SELECT COUNT(*) FROM pins WHERE cluster_key = 'k9' AND title = 'Milan'`) != 1 {
		t.Errorf("undo %d replaced a different place's pin (k9 'Milan') that held the reused id", rec.Code)
	}
}

// 6. Twin in another trip with the same cell key is not touched.
func TestBreak2UndoTwinOtherTripUntouched(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	tok := b2Undo(t, a, "/api/pins/10")
	b114Exec(t, a, `INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (30, 2, 'photo', 'k1', 41.9, 12.5)`)
	rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, "")
	if rec.Code != 200 {
		t.Errorf("undo: %d %s", rec.Code, rec.Body)
	}
	if b114Count(a, `SELECT COUNT(*) FROM pins WHERE id = 30 AND holiday_id = 2`) != 1 {
		t.Error("trip 2's pin with the same cell key was deleted")
	}
	if b114Count(a, `SELECT COUNT(*) FROM pins WHERE id = 10 AND holiday_id = 1`) != 1 {
		t.Error("pin 10 not restored")
	}
}

func b2Stats(t *testing.T, a *app) (out struct {
	Years     []statYear
	Trips     []statTrip
	Countries []statCountry
}) {
	t.Helper()
	rec := serve(b114Mux(a, "mum"), "GET", "/api/stats", "")
	if rec.Code != 200 {
		t.Fatalf("stats %d", rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return
}

// 7. Stats: an old photo pin with no photos (no visits) counts at created_at;
// a single-stop trip is 0 km; a planned trip is left out everywhere.
func TestBreak2StatsNoVisitsSingleAndPlanned(t *testing.T) {
	a := newTestApp(t)
	b114Exec(t, a,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Mixed', '#123456', '2025-07-01T00:00:00Z', '2025-07-04T00:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, country) VALUES (1, 1, 'photo', 'ldn', 51.5074, -0.1278, 'United Kingdom')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (1, 'a1', '2025-07-01T12:00:00Z', 51.5074, -0.1278)`,
		// old photo pin, photos gone: no visits, created between London photos
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, created_at) VALUES (2, 1, 'photo', 'par', 48.8566, 2.3522, '2025-07-02T12:00:00Z')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (1, 'a3', '2025-07-03T12:00:00Z', 51.5074, -0.1278)`,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (2, 'One', '#123456', '2024-01-01T00:00:00Z', '2024-01-01T10:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, country) VALUES (3, 2, 'manual', 41.9, 12.5, 'Italy')`,
		`INSERT INTO holidays (id, name, color, start_at, planned) VALUES (3, 'Future', '#123456', '2031-01-01T00:00:00Z', 1)`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, country) VALUES (4, 3, 'manual', 35.6, 139.7, 'Japan')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, country) VALUES (5, 3, 'manual', 34.7, 135.5, 'Japan')`,
	)
	out := b2Stats(t, a)
	km := map[int64]float64{}
	for _, tr := range out.Trips {
		km[tr.ID] = tr.KM
	}
	leg := haversineKM(51.5074, -0.1278, 48.8566, 2.3522)
	if math.Abs(km[1]-math.Round(2*leg*10)/10) > 0.2 {
		t.Errorf("trip 1 km %.1f, want %.1f (London, old Paris pin, London)", km[1], 2*leg)
	}
	if v, ok := km[2]; !ok || v != 0 {
		t.Errorf("single-stop trip: %v present %v", v, ok)
	}
	if _, ok := km[3]; ok {
		t.Error("planned trip in trips")
	}
	for _, y := range out.Years {
		if y.Year == 2031 {
			t.Error("planned trip's year counted")
		}
	}
	for _, c := range out.Countries {
		if c.Country == "Japan" {
			t.Error("planned trip's country counted")
		}
	}
}

// 8. Stats ordering ties: same timestamp at two pins; km must match the
// visit order the map uses (fillVisits breaks ties by pin id).
func TestBreak2StatsTieMatchesVisits(t *testing.T) {
	for _, ids := range [][2]int{{1, 2}, {2, 1}} {
		a := newTestApp(t)
		ldn, par := ids[0], ids[1]
		b114Exec(t, a,
			`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Tie', '#123456', '2025-07-01T00:00:00Z', '2025-07-04T00:00:00Z')`,
		)
		a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (?, 1, 'photo', 'ldn', 51.5074, -0.1278)`, ldn)
		a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (?, 1, 'photo', 'par', 48.8566, 2.3522)`, par)
		a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (?, 'a1', '2025-07-01T12:00:00Z', 51.5, -0.1)`, ldn)
		a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (?, 'a2', '2025-07-02T12:00:00Z', 51.5, -0.1)`, ldn)
		a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (?, 'a3', '2025-07-02T12:00:00Z', 48.8, 2.3)`, par)
		_, _, pins := getMap(t, a)
		type st struct {
			at string
			id int64
		}
		var stops []st
		for _, p := range pins {
			for _, v := range p.Visits {
				stops = append(stops, st{v.At, p.ID})
			}
		}
		// map order: by time, ties by pin id (as fillVisits)
		for i := range stops {
			for j := i + 1; j < len(stops); j++ {
				if stops[j].at < stops[i].at || stops[j].at == stops[i].at && stops[j].id < stops[i].id {
					stops[i], stops[j] = stops[j], stops[i]
				}
			}
		}
		legs := 0
		for i := 1; i < len(stops); i++ {
			if stops[i].id != stops[i-1].id {
				legs++
			}
		}
		want := math.Round(float64(legs)*haversineKM(51.5074, -0.1278, 48.8566, 2.3522)*10) / 10
		out := b2Stats(t, a)
		if len(out.Trips) != 1 || math.Abs(out.Trips[0].KM-want) > 0.2 {
			t.Errorf("ids ldn=%d par=%d: km %+v, want %.1f (%d legs, visits %v)", ldn, par, out.Trips, want, legs, stops)
		}
	}
}

// 9. Import: trailing whitespace/newline is fine; a BOM; exactly 20 MB; 20 MB + 1.
func TestBreak2ImportWhitespaceBomAndSize(t *testing.T) {
	b := newTestApp(t)
	mux := importMux(b, true)
	for _, tail := range []string{"\n", "\r\n", " \t\n\n"} {
		if rec := serve(mux, "POST", "/api/import?dry_run=1", `{"holidays":[]}`+tail); rec.Code != 200 {
			t.Errorf("trailing %q: %d %s", tail, rec.Code, rec.Body)
		}
	}
	if rec := serve(mux, "POST", "/api/import?dry_run=1", bom+`{"holidays":[]}`); rec.Code != 200 {
		t.Errorf("UTF-8 BOM before the JSON: %d %s (judgement: Windows editors add one)", rec.Code, rec.Body)
	}
	val := `{"holidays":[]}`
	exact := val + strings.Repeat(" ", maxImportBytes-len(val))
	if rec := serve(mux, "POST", "/api/import?dry_run=1", exact); rec.Code != 200 {
		t.Errorf("exactly 20 MB: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(mux, "POST", "/api/import?dry_run=1", exact+" "); rec.Code != 413 {
		t.Errorf("20 MB + 1 byte: %d", rec.Code)
	}
	if rec := serve(mux, "POST", "/api/import?dry_run=1", `{"holidays":[]} {"holidays":[]}`); rec.Code != 400 {
		t.Errorf("two documents: %d", rec.Code)
	}
}

// 10. Tracks: file name and root variants.
func TestBreak2TracksNameAndRoot(t *testing.T) {
	a := newTestApp(t)
	b114Exec(t, a, `INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'T', '#123456', '2026-05-01T00:00:00Z', '2026-05-03T00:00:00Z')`)
	mux := b114Mux(a, "mum")
	pts := `<trkpt lat="50.0" lon="1.0"/><trkpt lat="50.1" lon="1.1"/>`
	cases := []struct {
		name, file, body string
		want             int
	}{
		{"upper .GPX", "WALK.GPX", gpx(5), 201},
		{"mixed .Gpx", "walk.Gpx", gpx(5), 201},
		{"x.gpx.exe", "x.gpx.exe", gpx(5), 400},
		{"no ext", "walk", gpx(5), 400},
		{"default ns", "a.gpx", `<?xml version="1.0"?><gpx xmlns="http://www.topografix.com/GPX/1/1"><trk><trkseg>` + pts + `</trkseg></trk></gpx>`, 201},
		{"prefixed root", "a.gpx", `<?xml version="1.0"?><g:gpx xmlns:g="http://www.topografix.com/GPX/1/1"><g:trk><g:trkseg><g:trkpt lat="50.0" lon="1.0"/><g:trkpt lat="50.1" lon="1.1"/></g:trkseg></g:trk></g:gpx>`, 201},
		{"no ns at all", "a.gpx", `<gpx><trk><trkseg>` + pts + `</trkseg></trk></gpx>`, 201},
		{"comment+doctype first", "a.gpx", `<?xml version="1.0"?><!-- exported --><!DOCTYPE gpx><gpx><trk><trkseg>` + pts + `</trkseg></trk></gpx>`, 201},
		{"gpx nested in other root", "a.gpx", `<kml><gpx><trk><trkseg>` + pts + `</trkseg></trk></gpx></kml>`, 400},
		{"BOM", "a.gpx", bom + `<?xml version="1.0"?><gpx><trk><trkseg>` + pts + `</trkseg></trk></gpx>`, 201},
	}
	for _, c := range cases {
		rec := uploadGPX(t, mux, 1, c.file, c.body)
		if rec.Code != c.want {
			t.Errorf("%s (%q): %d %s, want %d", c.name, c.file, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}
}
