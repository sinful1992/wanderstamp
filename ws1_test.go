package main

// Task 142 (audit 137 WS-1): export + import on a new server keeps tracks,
// set-aside and unplaced photos, same-place answers (mapped to the new pin
// ids), master packing lists, who wrote what, and the Immich album.

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	ws1Aside    = "b1000000-0000-0000-0000-000000000001"
	ws1Unplaced = "b1000000-0000-0000-0000-000000000002"
	ws1OnHotel  = "b1000000-0000-0000-0000-000000000003" // on the hand-placed pin, "same place?" answered yes
	ws1Asking   = "b1000000-0000-0000-0000-000000000004" // on the hand-placed pin, still asking
	ws1OwnStop  = "b1000000-0000-0000-0000-000000000005" // answered "its own stop", on the photo pin
	ws1Album    = "c1000000-0000-0000-0000-000000000001"
)

func ws1Source(t *testing.T) string {
	t.Helper()
	a := newTestApp(t)
	for _, q := range []string{
		`INSERT INTO holidays (id, name, color, start_at, end_at, journal, created_by, journal_by, immich_album_id)
		 VALUES (1, 'Lakes', '#123456', '2025-07-01T00:00:00Z', '2025-07-08T00:00:00Z', 'Rain', 'ann', 'bob', '` + ws1Album + `')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title, created_by) VALUES (40, 1, 'manual', 54.5, -3.0, 'Hotel', 'bob')`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, title) VALUES (41, 1, 'photo', 'k41', 54.6, -3.1, 'Fell')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (40, '` + ws1OnHotel + `', '2025-07-02T10:00:00Z', 54.5, -3.0)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng, ask) VALUES (40, '` + ws1Asking + `', '2025-07-02T11:00:00Z', 54.5, -3.0, 1)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (41, '` + ws1OwnStop + `', '2025-07-03T10:00:00Z', 54.6, -3.1)`,
		`INSERT INTO photo_choices (asset_id, holiday_id, pin_id) VALUES ('` + ws1OnHotel + `', 1, 40), ('` + ws1OwnStop + `', 1, 0),
		 ('b1000000-0000-0000-0000-000000000009', 1, 999)`, // its pin was deleted: dropped on import
		`INSERT INTO set_aside (asset_id, holiday_id, taken_at, set_at) VALUES ('` + ws1Aside + `', 1, '2025-07-02T09:00:00Z', '2025-07-09T09:00:00Z')`,
		`INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at, type, duration) VALUES (1, '` + ws1Unplaced + `', '2025-07-04T09:00:00Z', 'video', 3.5)`,
		`INSERT INTO tracks (holiday_id, name, points) VALUES (1, 'Walk', '[[54.5,-3],[54.6,-3.1]]')`,
		`INSERT INTO packing_templates (id, name) VALUES (1, 'Always'), (2, 'Empty list')`,
		`INSERT INTO packing_template_items (template_id, label, sort) VALUES (1, 'Passport', 0), (1, 'Charger', 1)`,
	} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	rec := serve(importMux(a, true), "GET", "/api/export", "")
	if rec.Code != 200 {
		t.Fatalf("export %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func TestWS1_ExportImportRoundTrip(t *testing.T) {
	export := ws1Source(t)
	b := newTestApp(t)
	mux := importMux(b, true)
	dry := serve(mux, "POST", "/api/import?dry_run=1", export)
	real := serve(mux, "POST", "/api/import", export)
	if dry.Code != 200 || real.Code != 200 || dry.Body.String() != real.Body.String() {
		t.Fatalf("dry %d %s\nreal %d %s", dry.Code, dry.Body, real.Code, real.Body)
	}
	want := `{"holidays":1,"pins":2,"photos":3,"packing":0,"skipped":0,"tracks":1,"set_aside":1,"unplaced":1,"photo_choices":2,"packing_templates":2}`
	if got := strings.TrimSpace(real.Body.String()); got != want {
		t.Errorf("import counts\n got %s\nwant %s", got, want)
	}
	q := func(sql string, args ...any) string {
		var s string
		if err := b.db.QueryRow(sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	if got := q(`SELECT created_by || '/' || journal_by || '/' || immich_album_id FROM holidays`); got != "ann/bob/"+ws1Album {
		t.Errorf("trip authors/album: %q", got)
	}
	hotel := q(`SELECT id FROM pins WHERE title = 'Hotel'`)
	if got := q(`SELECT created_by FROM pins WHERE id = ?`, hotel); got != "bob" {
		t.Errorf("pin created_by %q", got)
	}
	if got := q(`SELECT pin_id FROM photo_choices WHERE asset_id = ?`, ws1OnHotel); got != hotel {
		t.Errorf("same-place answer points at pin %s, the hotel is now %s", got, hotel)
	}
	if got := q(`SELECT pin_id FROM photo_choices WHERE asset_id = ?`, ws1OwnStop); got != "0" {
		t.Errorf("own-stop answer: %s", got)
	}
	if got := q(`SELECT COUNT(*) FROM photo_choices`); got != "2" {
		t.Errorf("a dangling answer came in: %s rows", got)
	}
	if got := q(`SELECT ask FROM pin_photos WHERE asset_id = ?`, ws1Asking); got != "1" {
		t.Errorf("an open \"same place?\" question was lost: ask=%s", got)
	}
	if got := q(`SELECT holiday_id || ' ' || set_at FROM set_aside WHERE asset_id = ?`, ws1Aside); !strings.HasSuffix(got, " 2025-07-09T09:00:00Z") {
		t.Errorf("set aside: %q", got)
	}
	if got := q(`SELECT type || ' ' || duration FROM unplaced_photos WHERE asset_id = ?`, ws1Unplaced); got != "video 3.5" {
		t.Errorf("unplaced: %q", got)
	}
	if got := q(`SELECT name || ' ' || points FROM tracks`); got != "Walk [[54.5,-3],[54.6,-3.1]]" {
		t.Errorf("track: %q", got)
	}
	if got := q(`SELECT group_concat(label, ',') FROM (SELECT label FROM packing_template_items i JOIN packing_templates t ON t.id = i.template_id
		WHERE t.name = 'Always' ORDER BY sort)`); got != "Passport,Charger" {
		t.Errorf("master list: %q", got)
	}
	if got := q(`SELECT COUNT(*) FROM packing_templates WHERE name = 'Empty list'`); got != "1" {
		t.Errorf("an empty master list is a list too: %s", got)
	}
	// the set-aside photo stays off the map: nothing of it on a pin
	if got := q(`SELECT COUNT(*) FROM pin_photos WHERE asset_id = ?`, ws1Aside); got != "0" {
		t.Errorf("set-aside photo is on a pin")
	}

	// the same file again: the trip is skipped, nothing doubles, the lists are already complete
	again := serve(mux, "POST", "/api/import", export)
	if got := strings.TrimSpace(again.Body.String()); got != `{"holidays":0,"pins":0,"photos":0,"packing":0,"skipped":1}` {
		t.Errorf("second import: %s", got)
	}
	if got := q(`SELECT (SELECT COUNT(*) FROM tracks) || '/' || (SELECT COUNT(*) FROM set_aside) || '/' ||
		(SELECT COUNT(*) FROM packing_templates) || '/' || (SELECT COUNT(*) FROM packing_template_items)`); got != "1/1/2/2" {
		t.Errorf("after the second import: %s", got)
	}
}

// A photo set aside in the file but already on a pin here (or the other way
// round) stays where it is: each photo lives in one table only.
func TestWS1_ImportNeverFilesAPhotoTwice(t *testing.T) {
	export := ws1Source(t)
	b := newTestApp(t)
	for _, q := range []string{
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (7, 'Other', '#000000', '2024-01-01T00:00:00Z', '2024-01-02T00:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng) VALUES (70, 7, 'manual', 1, 1)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (70, '` + ws1Aside + `', '2024-01-01T10:00:00Z', 1, 1)`,
		`INSERT INTO set_aside (asset_id, holiday_id, taken_at, set_at) VALUES ('` + ws1Unplaced + `', 7, '2024-01-01T10:00:00Z', '2024-01-02T10:00:00Z')`,
		`INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at) VALUES (7, '` + ws1OnHotel + `', '2024-01-01T10:00:00Z')`,
	} {
		if _, err := b.db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	rec := serve(importMux(b, true), "POST", "/api/import", export)
	if rec.Code != 200 {
		t.Fatalf("import %d %s", rec.Code, rec.Body)
	}
	for _, asset := range []string{ws1Aside, ws1Unplaced, ws1OnHotel} {
		var n int
		b.db.QueryRow(`SELECT (SELECT COUNT(*) FROM pin_photos WHERE asset_id = ?) + (SELECT COUNT(*) FROM unplaced_photos WHERE asset_id = ?)
			+ (SELECT COUNT(*) FROM set_aside WHERE asset_id = ?)`, asset, asset, asset).Scan(&n)
		if n != 1 {
			t.Errorf("%s is filed %d times", asset, n)
		}
	}
}

// An export from before 1.20.0 (none of the new fields) imports as before.
func TestWS1_OldExportStillImports(t *testing.T) {
	var doc map[string]any
	json.Unmarshal([]byte(ws1Source(t)), &doc)
	delete(doc, "packing_templates")
	for _, h := range doc["holidays"].([]any) {
		hm := h.(map[string]any)
		for _, k := range []string{"tracks", "set_aside", "unplaced", "photo_choices", "created_by", "journal_by", "immich_album_id"} {
			delete(hm, k)
		}
		for _, p := range hm["pins"].([]any) {
			delete(p.(map[string]any), "created_by")
			delete(p.(map[string]any), "id")
		}
	}
	old, _ := json.Marshal(doc)
	rec := serve(importMux(newTestApp(t), true), "POST", "/api/import", string(old))
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 200 || got != `{"holidays":1,"pins":2,"photos":3,"packing":0,"skipped":0}` {
		t.Errorf("old export: %d %s", rec.Code, got)
	}
}

// A bad new field refuses the whole file before anything is written.
func TestWS1_BadNewFieldsChangeNothing(t *testing.T) {
	base := ws1Source(t)
	for name, edit := range map[string]func(h map[string]any){
		"track point off the map": func(h map[string]any) {
			h["tracks"] = []any{map[string]any{"name": "x", "points": [][]float64{{91, 0}}}}
		},
		"track not a list":     func(h map[string]any) { h["tracks"] = []any{map[string]any{"name": "x", "points": "nope"}} },
		"track with no points": func(h map[string]any) { h["tracks"] = []any{map[string]any{"name": "x", "points": [][]float64{}}} },
		"track point of 3": func(h map[string]any) {
			h["tracks"] = []any{map[string]any{"name": "x", "points": [][]float64{{1, 2, 3}}}}
		},
		"set aside bad id": func(h map[string]any) {
			h["set_aside"] = []any{map[string]any{"asset_id": "x", "taken_at": "2025-07-02T09:00:00Z", "set_at": "2025-07-02T09:00:00Z"}}
		},
		"set aside bad time": func(h map[string]any) {
			h["set_aside"] = []any{map[string]any{"asset_id": ws1Aside, "taken_at": "July", "set_at": "2025-07-02T09:00:00Z"}}
		},
		"unplaced bad type": func(h map[string]any) {
			h["unplaced"] = []any{map[string]any{"asset_id": ws1Unplaced, "taken_at": "2025-07-02T09:00:00Z", "type": "gif"}}
		},
		"choice negative pin": func(h map[string]any) {
			h["photo_choices"] = []any{map[string]any{"asset_id": ws1OnHotel, "pin_id": -1}}
		},
		"album not an id": func(h map[string]any) { h["immich_album_id"] = "../../x" },
		"pin id twice": func(h map[string]any) {
			ps := h["pins"].([]any)
			ps[1].(map[string]any)["id"] = ps[0].(map[string]any)["id"]
		},
	} {
		var doc map[string]any
		json.Unmarshal([]byte(base), &doc)
		edit(doc["holidays"].([]any)[0].(map[string]any))
		body, _ := json.Marshal(doc)
		b := newTestApp(t)
		rec := serve(importMux(b, true), "POST", "/api/import", string(body))
		if rec.Code != 400 {
			t.Errorf("%s: want 400, got %d %s", name, rec.Code, rec.Body)
		}
		if c := counts(t, b); c != "0/0/0/0" {
			t.Errorf("%s: a refused file changed the DB: %s", name, c)
		}
	}
	var doc map[string]any
	json.Unmarshal([]byte(base), &doc)
	doc["packing_templates"] = []any{map[string]any{"name": "  ", "items": []string{"x"}}}
	body, _ := json.Marshal(doc)
	if rec := serve(importMux(newTestApp(t), true), "POST", "/api/import", string(body)); rec.Code != 400 {
		t.Errorf("a master list with no name: want 400, got %d", rec.Code)
	}
}

// A master list with a name already here (other case) gets only what it lacks.
func TestWS1_MasterListMerges(t *testing.T) {
	export := ws1Source(t)
	b := newTestApp(t)
	b.db.Exec(`INSERT INTO packing_templates (id, name) VALUES (5, 'ALWAYS')`)
	b.db.Exec(`INSERT INTO packing_template_items (template_id, label, sort) VALUES (5, 'passport', 0), (5, 'Hat', 1)`)
	if rec := serve(importMux(b, true), "POST", "/api/import", export); rec.Code != 200 {
		t.Fatalf("import %d %s", rec.Code, rec.Body)
	}
	var got string
	b.db.QueryRow(`SELECT group_concat(label, ',') FROM (SELECT label FROM packing_template_items WHERE template_id = 5 ORDER BY sort)`).Scan(&got)
	if got != "passport,Hat,Charger" {
		t.Errorf("merged list: %q", got)
	}
	var n int
	b.db.QueryRow(`SELECT COUNT(*) FROM packing_templates`).Scan(&n)
	if n != 2 {
		t.Errorf("templates: %d", n)
	}
}
