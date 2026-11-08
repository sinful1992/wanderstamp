package main

// Task 136: a note belongs to one visit (a place on a local day).

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type notePin struct {
	ID         int64     `json:"id"`
	Note       string    `json:"note"`
	Visits     []visit   `json:"visits"`
	OtherNotes []dayNote `json:"other_notes"`
}

// newNotesFix is the TV fixture plus:
//   - pin 5, a hotel dropped by hand with the old single note, visited on two
//     days (the Premier Inn shape);
//   - pins 6 and 7 on one day: 6, then 7, then 6 again (two visits, one day).
func newNotesFix(t *testing.T) *tvFix {
	t.Helper()
	f := newTVFix(t)
	for _, q := range []string{
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title, note, created_at, created_by) VALUES (5, 1, 'manual', 52.5, -1.4, 'Premier Inn', 'Food horrible', '2026-07-05T07:00:00Z', 'giedrius')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (5, 'a5000000-0000-0000-0000-000000000001', '2026-07-05T09:00:00Z', 52.5, -1.4)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (5, 'a5000000-0000-0000-0000-000000000002', '2026-07-06T09:00:00Z', 52.5, -1.4)`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, created_at) VALUES (6, 1, 'photo', 'c6', 52.3, -1.6, '2026-07-07T09:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, created_at) VALUES (7, 1, 'photo', 'c7', 52.28, -1.58, '2026-07-07T10:00:00Z')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (6, 'a6000000-0000-0000-0000-000000000001', '2026-07-07T09:00:00Z', 52.3, -1.6)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (7, 'a7000000-0000-0000-0000-000000000001', '2026-07-07T10:00:00Z', 52.28, -1.58)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (6, 'a6000000-0000-0000-0000-000000000002', '2026-07-07T11:00:00Z', 52.3, -1.6)`,
	} {
		if _, err := f.a.db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	return f
}

func (f *tvFix) notePins(t *testing.T) map[int64]notePin {
	t.Helper()
	r := tvDo(t, f.main.URL, "GET", "/api/pins", "", f.signedIn())
	if r.code != 200 {
		t.Fatalf("GET /api/pins %d %s", r.code, r.body)
	}
	var ps []notePin
	r.json(t, &ps)
	out := map[int64]notePin{}
	for _, p := range ps {
		out[p.ID] = p
	}
	return out
}

func (f *tvFix) patchNote(t *testing.T, pin int64, body string) tvResp {
	t.Helper()
	return tvDo(t, f.main.URL, "PATCH", fmt.Sprintf("/api/pins/%d", pin), body, f.signedIn())
}

func visitNotes(p notePin) []string {
	var out []string
	for _, v := range p.Visits {
		out = append(out, v.Day+"="+v.Note)
	}
	return out
}

func (f *tvFix) rows(t *testing.T, pin int64) string {
	t.Helper()
	rs, err := f.a.db.Query(`SELECT day, note FROM visit_notes WHERE pin_id = ? ORDER BY day`, pin)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var d, n string
		rs.Scan(&d, &n)
		out = append(out, d+"="+n)
	}
	var legacy string
	f.a.db.QueryRow(`SELECT note FROM pins WHERE id = ?`, pin).Scan(&legacy)
	return strings.Join(out, ",") + " | pins.note=" + legacy
}

func TestNotesOldNoteShowsOnFirstVisitOnly(t *testing.T) {
	f := newNotesFix(t)
	ps := f.notePins(t)
	if got := strings.Join(visitNotes(ps[5]), ","); got != "2026-07-05=Food horrible,2026-07-06=" {
		t.Fatalf("legacy note: %s", got)
	}
	if ps[5].Note != "Food horrible" || len(ps[5].OtherNotes) != 0 || ps[5].OtherNotes == nil {
		t.Fatalf("pin note %q, other_notes %v", ps[5].Note, ps[5].OtherNotes)
	}
	if got := strings.Join(visitNotes(ps[3]), ","); got != "2026-07-04=windy" {
		t.Fatalf("one-visit pin: %s", got)
	}
	if got := f.rows(t, 5); got != " | pins.note=Food horrible" {
		t.Fatalf("a read must write nothing: %s", got)
	}
}

// The Premier Inn move: one call, the note leaves day 1 and is on day 2 only.
func TestNotesMoveLegacyNoteToSecondVisit(t *testing.T) {
	f := newNotesFix(t)
	if r := f.patchNote(t, 5, `{"note_day":"2026-07-06","from_day":"2026-07-05","note":"Food horrible"}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	ps := f.notePins(t)
	if got := strings.Join(visitNotes(ps[5]), ","); got != "2026-07-05=,2026-07-06=Food horrible" {
		t.Fatalf("after move: %s", got)
	}
	if got := f.rows(t, 5); got != "2026-07-06=Food horrible | pins.note=" {
		t.Fatalf("rows: %s", got)
	}
}

// Writing another visit's note keeps the old note on the first visit.
func TestNotesSecondVisitKeepsFirst(t *testing.T) {
	f := newNotesFix(t)
	if r := f.patchNote(t, 5, `{"note_day":"2026-07-06","note":"Breakfast ok"}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	ps := f.notePins(t)
	if got := strings.Join(visitNotes(ps[5]), ","); got != "2026-07-05=Food horrible,2026-07-06=Breakfast ok" {
		t.Fatalf("got %s", got)
	}
	// edit day 1 in place (from_day = note_day), then empty both: nothing comes back
	f.patchNote(t, 5, `{"note_day":"2026-07-05","from_day":"2026-07-05","note":"Food bad"}`)
	if got := f.rows(t, 5); got != "2026-07-05=Food bad,2026-07-06=Breakfast ok | pins.note=" {
		t.Fatalf("rows: %s", got)
	}
	f.patchNote(t, 5, `{"note_day":"2026-07-05","note":""}`)
	f.patchNote(t, 5, `{"note_day":"2026-07-06","note":"   "}`)
	ps = f.notePins(t)
	if got := strings.Join(visitNotes(ps[5]), ","); got != "2026-07-05=,2026-07-06=" {
		t.Fatalf("the old note must not come back once deleted: %s", got)
	}
}

// Moving the legacy note away from its own first day to... its first day, by
// from_day = first day and note_day = first day: edited, not duplicated.
func TestNotesFirstWriteOnFirstDay(t *testing.T) {
	f := newNotesFix(t)
	f.patchNote(t, 5, `{"note_day":"2026-07-05","from_day":"2026-07-05","note":"Food horrible!"}`)
	if got := f.rows(t, 5); got != "2026-07-05=Food horrible! | pins.note=" {
		t.Fatalf("rows: %s", got)
	}
	f2 := newNotesFix(t)
	f2.patchNote(t, 5, `{"note_day":"2026-07-05","note":"Food horrible!"}`)
	if got := f2.rows(t, 5); got != "2026-07-05=Food horrible! | pins.note=" {
		t.Fatalf("rows without from_day: %s", got)
	}
}

func TestNotesTwoVisitsOneDay(t *testing.T) {
	f := newNotesFix(t)
	ps := f.notePins(t)
	if len(ps[6].Visits) != 2 {
		t.Fatalf("fixture: pin 6 visits %v", ps[6].Visits)
	}
	f.patchNote(t, 6, `{"note_day":"2026-07-07","note":"castle"}`)
	ps = f.notePins(t)
	if got := strings.Join(visitNotes(ps[6]), ","); got != "2026-07-07=castle,2026-07-07=" {
		t.Fatalf("got %s", got)
	}
}

func TestNotesOrphanDayKept(t *testing.T) {
	f := newNotesFix(t)
	if r := f.patchNote(t, 1, `{"note_day":"2026-01-01","note":"before photos moved"}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	ps := f.notePins(t)
	if len(ps[1].OtherNotes) != 1 || ps[1].OtherNotes[0] != (dayNote{"2026-01-01", "before photos moved"}) {
		t.Fatalf("other_notes %v", ps[1].OtherNotes)
	}
	for _, v := range ps[1].Visits {
		if v.Note != "" {
			t.Fatalf("orphan note shown on %v", v)
		}
	}
}

func TestNotesBadWrites(t *testing.T) {
	f := newNotesFix(t)
	for _, b := range []string{
		`{"note_day":"2026-7-6","note":"x"}`, `{"note_day":"2026-02-30","note":"x"}`, `{"note_day":"","note":"x"}`,
		`{"note_day":"2026-07-06T00:00:00Z","note":"x"}`, `{"note_day":"2026-07-06"}`,
		`{"from_day":"2026-07-05","note":"x"}`, `{"note_day":"2026-07-06","from_day":"yesterday","note":"x"}`,
		`{"note_day":"2026-07-06","note":"x","from_day":"2026-07-05 "}`,
	} {
		if r := f.patchNote(t, 5, b); r.code != 400 {
			t.Errorf("%s: %d %s", b, r.code, r.body)
		}
	}
	if r := f.patchNote(t, 999, `{"note_day":"2026-07-06","note":"x"}`); r.code != 404 {
		t.Errorf("unknown pin: %d %s", r.code, r.body)
	}
	if got := f.rows(t, 5); got != " | pins.note=Food horrible" {
		t.Fatalf("a refused write changed %s", got)
	}
	// note without note_day: pins.note, as before (a pin with no visits)
	if r := f.patchNote(t, 4, `{"note":"plan"}`); r.code != 200 {
		t.Fatal(r.code)
	}
	var n string
	f.a.db.QueryRow(`SELECT note FROM pins WHERE id = 4`).Scan(&n)
	if n != "plan" {
		t.Fatalf("pins.note %q", n)
	}
}

// Many phones saving the first per-visit note at once: no error, and the old
// note ends on day 1 exactly once.
func TestNotesConcurrentFirstWrites(t *testing.T) {
	f := newNotesFix(t)
	var wg sync.WaitGroup
	codes := make([]int, 12)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.patchNote(t, 5, fmt.Sprintf(`{"note_day":"2026-07-06","note":"n%d"}`, i)).code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("write %d: %d", i, c)
		}
	}
	got := f.rows(t, 5)
	if !strings.HasPrefix(got, "2026-07-05=Food horrible,2026-07-06=n") || !strings.HasSuffix(got, "| pins.note=") {
		t.Fatalf("rows: %s", got)
	}
}

// The TV and a share see the same per-visit notes.
func TestNotesOnTVAndShare(t *testing.T) {
	f := newNotesFix(t)
	f.patchNote(t, 5, `{"note_day":"2026-07-06","from_day":"2026-07-05","note":"Food horrible"}`)
	tok := f.pair(t, "Den")
	r := tvDo(t, f.door.URL, "GET", "/api/tv/trips/1", "", bearer(tok))
	var trip struct {
		Pins []notePin `json:"pins"`
	}
	r.json(t, &trip)
	sh := tvDo(t, f.main.URL, "GET", "/api/share/old-share", "", nil)
	var share struct {
		Pins []notePin `json:"pins"`
	}
	sh.json(t, &share)
	for name, pins := range map[string][]notePin{"tv": trip.Pins, "share": share.Pins} {
		found := false
		for _, p := range pins {
			if p.ID == 5 {
				found = true
				if got := strings.Join(visitNotes(p), ","); got != "2026-07-05=,2026-07-06=Food horrible" {
					t.Errorf("%s: %s", name, got)
				}
			}
		}
		if !found {
			t.Errorf("%s: no pin 5 in %s", name, r.body)
		}
	}
}

// Undo of a pin delete brings its visit notes back (undo reads cascades from
// the schema).
func TestNotesUndoPinDelete(t *testing.T) {
	f := newNotesFix(t)
	f.patchNote(t, 5, `{"note_day":"2026-07-06","note":"Breakfast ok"}`)
	r := tvDo(t, f.main.URL, "DELETE", "/api/pins/5", "", f.signedIn())
	var del struct {
		Undo string `json:"undo"`
	}
	r.json(t, &del)
	if got := f.rows(t, 5); got != " | pins.note=" {
		t.Fatalf("after delete: %s", got)
	}
	if u := tvDo(t, f.main.URL, "POST", "/api/undo/"+del.Undo, "", f.signedIn()); u.code != 200 {
		t.Fatalf("undo %d %s", u.code, u.body)
	}
	if got := f.rows(t, 5); got != "2026-07-05=Food horrible,2026-07-06=Breakfast ok | pins.note=" {
		t.Fatalf("after undo: %s", got)
	}
}

func TestNotesExportImport(t *testing.T) {
	f := newNotesFix(t)
	f.patchNote(t, 5, `{"note_day":"2026-07-06","from_day":"2026-07-05","note":"Food horrible"}`)
	f.patchNote(t, 1, `{"note_day":"2026-01-01","note":"orphan"}`)
	ex := tvDo(t, f.main.URL, "GET", "/api/export", "", f.signedIn())
	if ex.code != 200 || !strings.Contains(string(ex.body), `"visit_notes":[{"day":"2026-07-06","note":"Food horrible"}]`) {
		t.Fatalf("export %d: no visit_notes", ex.code)
	}
	f2 := newNotesFix(t)
	if _, err := f2.a.db.Exec(`DELETE FROM holidays WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	im := tvDo(t, f2.main.URL, "POST", "/api/import", string(ex.body), f2.signedIn())
	if im.code != 200 {
		t.Fatalf("import %d %s", im.code, im.body)
	}
	var newID int64
	f2.a.db.QueryRow(`SELECT id FROM pins WHERE title = 'Premier Inn'`).Scan(&newID)
	if got := f2.rows(t, newID); got != "2026-07-06=Food horrible | pins.note=" {
		t.Fatalf("imported: %s", got)
	}
	ps := f2.notePins(t)
	var orphan int
	for _, p := range ps {
		orphan += len(p.OtherNotes)
	}
	if orphan != 1 {
		t.Fatalf("the orphan note must survive import too: %d", orphan)
	}
	// an export from before 1.17.0 has no visit_notes: imports as before
	var old map[string]any
	json.Unmarshal(ex.body, &old)
	b := strings.ReplaceAll(string(ex.body), `"visit_notes":`, `"visit_notes_x":`)
	f3 := newNotesFix(t)
	f3.a.db.Exec(`DELETE FROM holidays WHERE id = 1`)
	if im := tvDo(t, f3.main.URL, "POST", "/api/import", b, f3.signedIn()); im.code != 200 {
		t.Fatalf("old import %d %s", im.code, im.body)
	}
}
