package main

// Break tests (round 3) for the #8189 fixes: bare-twin rule, other-trip
// skipping on undo, import BOM. Written from the contract + diff only.

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

const uuidC = "aaaaaaaa-0000-0000-0000-00000000000c"
const uuidN = "aaaaaaaa-0000-0000-0000-00000000000e"

func b3Same(t *testing.T, before, after map[string][]string, tables []string, what string) {
	t.Helper()
	for _, tb := range tables {
		if strings.Join(before[tb], "\n") != strings.Join(after[tb], "\n") {
			t.Errorf("%s: %s changed:\nbefore %v\nafter  %v", what, tb, before[tb], after[tb])
		}
	}
}

// photos of a trip filed in two places at once (pin_photos and unplaced, or
// pin_photos and set_aside)
func b3DoubleFiled(a *app) int {
	return b114Count(a, `SELECT COUNT(*) FROM pin_photos pp WHERE
		pp.asset_id IN (SELECT asset_id FROM unplaced_photos) OR pp.asset_id IN (SELECT asset_id FROM set_aside)`)
}

// 1. Bare-looking twin that holds a NEW photo the old pin never had: 409 and
// the whole DB unchanged (pins, pin_photos, unplaced, choices...).
func TestBreak3TwinWithNewPhotoConflictNoLoss(t *testing.T) {
	for _, twinID := range []int{12, 10} {
		a := newTestApp(t)
		seedFullTrip(t, a)
		tok := b2Undo(t, a, "/api/pins/10")
		a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (?, 1, 'photo', 'k1', 41.9, 12.5)`, twinID)
		a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (?, ?, '2026-05-02T10:00:00Z', 41.9, 12.5)`, twinID, uuidA)
		a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (?, ?, '2026-05-02T11:00:00Z', 41.9, 12.5)`, twinID, uuidN)
		tables := b2Tables(t, a)
		before := dump(t, a, tables)
		rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, "")
		if rec.Code != 409 {
			t.Errorf("twin id %d with a new photo: undo %d %s, want 409", twinID, rec.Code, rec.Body)
		}
		b3Same(t, before, dump(t, a, tables), tables, "refused undo")
	}
}

// 2. Bare twin holding a subset of the old photos (the rest went to unplaced):
// 200, one pin in the cell, old id/title back, all old photos back, no double filing.
func TestBreak3BareTwinRestoresExactly(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	b114Exec(t, a, `INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (10, '`+uuidC+`', '2026-05-02T11:00:00Z', 41.9, 12.5)`)
	tok := b2Undo(t, a, "/api/pins/10")
	b114Exec(t, a,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (12, 1, 'photo', 'k1', 41.9, 12.5)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (12, '`+uuidA+`', '2026-05-02T10:00:00Z', 41.9, 12.5)`,
		`INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at) VALUES (1, '`+uuidC+`', '2026-05-02T11:00:00Z')`,
	)
	rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, "")
	if rec.Code != 200 {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body)
	}
	if n := b114Count(a, `SELECT COUNT(*) FROM pins WHERE holiday_id = 1 AND cluster_key = 'k1'`); n != 1 {
		t.Errorf("pins in cell k1: %d", n)
	}
	if b114Count(a, `SELECT COUNT(*) FROM pins WHERE id = 10 AND title = 'Colosseum'`) != 1 {
		t.Error("pin 10 'Colosseum' not back")
	}
	if n := b114Count(a, `SELECT COUNT(*) FROM pin_photos WHERE pin_id = 10 AND asset_id IN (?, ?)`, uuidA, uuidC); n != 2 {
		t.Errorf("pin 10 holds %d of its 2 photos", n)
	}
	if n := b3DoubleFiled(a); n != 0 {
		t.Errorf("%d photos double filed", n)
	}
	if b114Count(a, `SELECT COUNT(*) FROM unplaced_photos WHERE holiday_id = 1 AND asset_id = ?`, uuidB) != 1 {
		t.Error("trip's own unplaced uuidB lost")
	}
}

// 3. Bare twin that reused the old id 10: 200, title back.
func TestBreak3BareTwinSameId(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	tok := b2Undo(t, a, "/api/pins/10")
	b114Exec(t, a,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (10, 1, 'photo', 'k1', 41.9, 12.5)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (10, '`+uuidA+`', '2026-05-02T10:00:00Z', 41.9, 12.5)`,
	)
	if rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, ""); rec.Code != 200 {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body)
	}
	if b114Count(a, `SELECT COUNT(*) FROM pins WHERE id = 10 AND title = 'Colosseum'`) != 1 {
		t.Error("title not back")
	}
	if b114Count(a, `SELECT COUNT(*) FROM pin_photos WHERE pin_id = 10 AND asset_id = ?`, uuidA) != 1 {
		t.Error("photo not on pin 10")
	}
}

// 4. Twin made by hand (created_by set) or with a cover: 409, twin kept.
func TestBreak3TwinCreatedByOrCoverConflict(t *testing.T) {
	for _, set := range []string{"created_by = 'dad'", "cover_asset = '" + uuidA + "'", "note = 'x'"} {
		a := newTestApp(t)
		seedFullTrip(t, a)
		tok := b2Undo(t, a, "/api/pins/10")
		b114Exec(t, a,
			`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (12, 1, 'photo', 'k1', 41.9, 12.5)`,
			`UPDATE pins SET `+set+` WHERE id = 12`,
			`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (12, '`+uuidA+`', '2026-05-02T10:00:00Z', 41.9, 12.5)`,
		)
		rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, "")
		if rec.Code != 409 {
			t.Errorf("twin with %s: undo %d, want 409", set, rec.Code)
		}
		if b114Count(a, `SELECT COUNT(*) FROM pins WHERE id = 12`) != 1 || b114Count(a, `SELECT COUNT(*) FROM pin_photos WHERE pin_id = 12`) != 1 {
			t.Errorf("twin with %s lost", set)
		}
	}
}

// 5. Trip undo after a photo was filed on another trip: it stays there, and
// the restored trip has no photo-kind pin left with 0 photos.
func TestBreak3TripUndoPhotoElsewhereNoEmptyPin(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	tok := b2Undo(t, a, "/api/holidays/1")
	b114Exec(t, a,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (20, '`+uuidA+`', '2026-05-02T10:00:00Z', 50, 0)`,
		`INSERT INTO photo_choices (asset_id, holiday_id, pin_id) VALUES ('`+uuidA+`', 2, 20)`,
	)
	rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, "")
	if rec.Code != 200 {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body)
	}
	if b114Count(a, `SELECT COUNT(*) FROM pin_photos WHERE pin_id = 20 AND asset_id = ?`, uuidA) != 1 {
		t.Error("uuidA pulled off trip 2")
	}
	if b114Count(a, `SELECT COUNT(*) FROM photo_choices WHERE holiday_id = 2 AND asset_id = ? AND pin_id = 20`, uuidA) != 1 {
		t.Error("trip 2's choice lost")
	}
	if n := b114Count(a, `SELECT COUNT(*) FROM pins p WHERE holiday_id = 1 AND kind = 'photo'
		AND NOT EXISTS (SELECT 1 FROM pin_photos WHERE pin_id = p.id)`); n != 0 {
		t.Errorf("judgement: restored trip has %d photo pin(s) with 0 photos (pin 10 'Colosseum', empty print)", n)
	}
}

// 6. Set-aside / choices rows of another trip for the same asset win; the
// restored trip's own rows for other assets come back.
func TestBreak3TripUndoAsideOfOtherTrip(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	tok := b2Undo(t, a, "/api/holidays/1")
	b114Exec(t, a, `INSERT INTO set_aside (asset_id, holiday_id, taken_at, set_at) VALUES ('bbbbbbbb-0000-0000-0000-000000000003', 2, '2025-05-01T11:00:00Z', '2026-06-01T00:00:00Z')`)
	if rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, ""); rec.Code != 200 {
		t.Fatalf("undo: %d %s", rec.Code, rec.Body)
	}
	if b114Count(a, `SELECT COUNT(*) FROM set_aside WHERE asset_id = 'bbbbbbbb-0000-0000-0000-000000000003' AND holiday_id = 2`) != 1 {
		t.Error("trip 2's set_aside row replaced by trip 1's")
	}
	if b114Count(a, `SELECT COUNT(*) FROM photo_choices WHERE holiday_id = 1 AND asset_id = ?`, uuidA) != 1 ||
		b114Count(a, `SELECT COUNT(*) FROM unplaced_photos WHERE holiday_id = 1 AND asset_id = ?`, uuidB) != 1 {
		t.Error("trip 1's own choices/unplaced not back")
	}
}

// 7. Pin delete, resync twin, user sets the photo aside (twin goes), undo:
// the photo must not be both set aside and on the restored pin.
func TestBreak3PinUndoAfterSetAside(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	tok := b2Undo(t, a, "/api/pins/10")
	b114Exec(t, a,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng) VALUES (12, 1, 'photo', 'k1', 41.9, 12.5)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (12, '`+uuidA+`', '2026-05-02T10:00:00Z', 41.9, 12.5)`,
	)
	mux := b114Mux(a, "mum")
	mux.HandleFunc("POST /api/photos/{asset}/aside", as("mum", a.handleSetAside))
	if rec := serve(mux, "POST", "/api/photos/"+uuidA+"/aside", ""); rec.Code != 200 {
		t.Fatalf("set aside: %d %s", rec.Code, rec.Body)
	}
	rec := serve(mux, "POST", "/api/undo/"+tok, "")
	inAside := b114Count(a, `SELECT COUNT(*) FROM set_aside WHERE asset_id = ?`, uuidA)
	onPin := b114Count(a, `SELECT COUNT(*) FROM pin_photos WHERE asset_id = ?`, uuidA)
	if inAside+onPin != 1 {
		t.Errorf("undo %d: uuidA set_aside=%d pin_photos=%d (want exactly one place)", rec.Code, inAside, onPin)
	}
}

// 8. Same trip: resync filed the photo on ANOTHER photo pin (other cell) that
// someone then titled; undo must not empty that pin silently.
func TestBreak3PinUndoStealsFromSameTripPin(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	tok := b2Undo(t, a, "/api/pins/10")
	b114Exec(t, a,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, title) VALUES (12, 1, 'photo', 'k2', 41.95, 12.55, 'Forum')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (12, '`+uuidA+`', '2026-05-02T10:00:00Z', 41.9, 12.5)`,
	)
	rec := serve(b114Mux(a, "mum"), "POST", "/api/undo/"+tok, "")
	if rec.Code == 200 && b114Count(a, `SELECT COUNT(*) FROM pin_photos WHERE pin_id = 12`) == 0 {
		t.Errorf("judgement: undo 200 moved uuidA off same-trip pin 12 'Forum', leaving it with 0 photos")
	}
}

// 9. Same token, many concurrent undos: exactly one 200, the rest 410.
func TestBreak3RacingUndos(t *testing.T) {
	a := newTestApp(t)
	seedFullTrip(t, a)
	tok := b2Undo(t, a, "/api/pins/10")
	mux := b114Mux(a, "mum")
	var wg sync.WaitGroup
	codes := make([]int, 16)
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
		case http.StatusGone:
			gone++
		}
	}
	if ok != 1 || gone != len(codes)-1 {
		t.Errorf("codes %v: %d x 200, %d x 410", codes, ok, gone)
	}
	if b114Count(a, `SELECT COUNT(*) FROM pins WHERE id = 10`) != 1 {
		t.Error("pin 10 not back once")
	}
}

// 10. Import BOM variants.
func TestBreak3ImportBomVariants(t *testing.T) {
	a := newTestApp(t)
	mux := importMux(a, true)
	cases := []struct {
		name, body string
		want       int
	}{
		{"BOM + trailing newline", bom + `{"holidays":[]}` + "\r\n", 200},
		{"BOM + leading space", bom + "  \n" + `{"holidays":[]}`, 200},
		{"BOM only", bom, 400},
		{"BOM + newline only", bom + "\n", 400},
		{"double BOM", bom + bom + `{"holidays":[]}`, 400},
		{"BOM after space", " " + bom + `{"holidays":[]}`, 400},
		{"UTF-16 LE BOM", "\xFF\xFE{\x00}\x00", 400},
		{"UTF-16 BE BOM", "\xFE\xFF\x00{\x00}", 400},
		{"partial BOM", "\xEF\xBB" + `{"holidays":[]}`, 400},
	}
	for _, c := range cases {
		if rec := serve(mux, "POST", "/api/import?dry_run=1", c.body); rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}
	// a real (non dry) import with a BOM writes the trip
	body := bom + `{"holidays":[{"name":"Bom trip","color":"#123456","start_at":"2020-01-01T00:00:00Z","end_at":"2020-01-02T00:00:00Z","pins":[]}]}` + "\n"
	rec := serve(mux, "POST", "/api/import", body)
	if rec.Code != 200 || b114Count(a, `SELECT COUNT(*) FROM holidays WHERE name = 'Bom trip'`) != 1 {
		t.Errorf("real import with BOM: %d %s", rec.Code, rec.Body)
	}
}
