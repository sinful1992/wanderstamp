package main

// Task 136: a note belongs to one visit (a place on a local day).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// ada #9788: after the first per-visit write pins.note is '', so the note
// lives only in visit_notes. The empty-photo-pin cleanups must still treat
// that pin as annotated and keep it, with the note moved to other_notes.
const tvA7 = "a7000000-0000-0000-0000-000000000001"

func noteOnPin7(t *testing.T, f *tvFix) {
	t.Helper()
	if r := f.patchNote(t, 7, `{"note_day":"2026-07-07","note":"Ice cream"}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if got := f.rows(t, 7); got != "2026-07-07=Ice cream | pins.note=" {
		t.Fatalf("setup: %s", got)
	}
}

func keptInOtherNotes(t *testing.T, f *tvFix, pin int64) {
	t.Helper()
	p, ok := f.notePins(t)[pin]
	if !ok {
		t.Fatalf("pin %d with a visit note was deleted (rows now: %s)", pin, f.rows(t, pin))
	}
	if len(p.OtherNotes) != 1 || p.OtherNotes[0] != (dayNote{"2026-07-07", "Ice cream"}) {
		t.Fatalf("pin %d other_notes %v visits %v", pin, p.OtherNotes, visitNotes(p))
	}
}

func TestNotesSetAsideLastPhotoKeepsNotedPin(t *testing.T) {
	f := newNotesFix(t)
	noteOnPin7(t, f)
	if r := tvDo(t, f.main.URL, "POST", "/api/photos/"+tvA7+"/aside", "", f.signedIn()); r.code/100 != 2 {
		t.Fatalf("aside %d %s", r.code, r.body)
	}
	keptInOtherNotes(t, f, 7)
}

func TestNotesSyncEmptyAlbumKeepsNotedPin(t *testing.T) {
	f := newNotesFix(t)
	noteOnPin7(t, f)
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// a7 left the album; another of the trip's photos is still there
		w.Write([]byte(`{"assets":{"items":[{"id":"` + tvA2 + `","type":"IMAGE","fileCreatedAt":"2026-07-02T08:00:00Z","exifInfo":{"latitude":54.6,"longitude":-3.13}}],"nextPage":null}}`))
	}))
	defer empty.Close()
	f.a.immich = newImmichClient(empty.URL, "key")
	if _, _, err := f.a.syncHoliday(1); err != nil {
		t.Fatal(err)
	}
	var n int
	f.a.db.QueryRow(`SELECT COUNT(*) FROM pin_photos WHERE pin_id = 7`).Scan(&n)
	if n != 0 {
		t.Fatalf("sync left %d photos on pin 7; test needs an emptied pin", n)
	}
	keptInOtherNotes(t, f, 7)
}

func undoToken(t *testing.T, f *tvFix, path string) string {
	t.Helper()
	r := tvDo(t, f.main.URL, "DELETE", path, "", f.signedIn())
	var del struct {
		Undo string `json:"undo"`
	}
	r.json(t, &del)
	if del.Undo == "" {
		t.Fatalf("no undo token: %d %s", r.code, r.body)
	}
	return del.Undo
}

// Undo of a noted pin whose photo has since gone to a titled pin: the pin
// comes back empty but with its note, not refused and dropped.
func TestNotesUndoEmptyRestoreKeepsNotedPin(t *testing.T) {
	f := newNotesFix(t)
	noteOnPin7(t, f)
	tok := undoToken(t, f, "/api/pins/7")
	for _, q := range []string{
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title, created_at) VALUES (8, 1, 'manual', 52.28, -1.58, 'Kiosk', '2026-07-07T10:00:00Z')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (8, '` + tvA7 + `', '2026-07-07T10:00:00Z', 52.28, -1.58)`,
	} {
		if _, err := f.a.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if u := tvDo(t, f.main.URL, "POST", "/api/undo/"+tok, "", f.signedIn()); u.code != 200 {
		t.Fatalf("undo %d %s (rows: %s)", u.code, u.body, f.rows(t, 7))
	}
	keptInOtherNotes(t, f, 7)
}

// A twin pin the resync made, which then got a visit note, is someone's
// work: the undo is a conflict and the twin keeps its note.
func TestNotesUndoKeepsNotedTwin(t *testing.T) {
	f := newNotesFix(t)
	tok := undoToken(t, f, "/api/pins/7")
	for _, q := range []string{
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, created_at) VALUES (9, 1, 'photo', 'c7', 52.28, -1.58, '2026-07-07T10:00:00Z')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (9, '` + tvA7 + `', '2026-07-07T10:00:00Z', 52.28, -1.58)`,
	} {
		if _, err := f.a.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if r := f.patchNote(t, 9, `{"note_day":"2026-07-07","note":"Ice cream"}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	u := tvDo(t, f.main.URL, "POST", "/api/undo/"+tok, "", f.signedIn())
	if u.code != 409 {
		t.Fatalf("undo over a noted twin: %d %s", u.code, u.body)
	}
	if got := f.rows(t, 9); got != "2026-07-07=Ice cream | pins.note=" {
		t.Fatalf("twin note: %s", got)
	}
}

// ada #9791: a note whose day has no visit is shown to nobody on a share link
// or a TV, so it isn't sent there either; the family's own map still has it.
func TestNotesOtherNotesStayOffShareAndTV(t *testing.T) {
	f := newNotesFix(t)
	if r := f.patchNote(t, 2, `{"note_day":"2026-01-01","note":"secret orphan"}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if len(f.notePins(t)[2].OtherNotes) != 1 {
		t.Fatal("family map lost the orphan note")
	}
	tok := f.pair(t, "Den")
	tv := tvDo(t, f.door.URL, "GET", "/api/tv/trips/1", "", bearer(tok))
	sh := tvDo(t, f.main.URL, "GET", "/api/share/old-share", "", nil)
	for name, r := range map[string]tvResp{"tv": tv, "share": sh} {
		if r.code != 200 {
			t.Fatalf("%s %d %s", name, r.code, r.body)
		}
		if strings.Contains(string(r.body), "secret orphan") {
			t.Errorf("%s carries the orphan note: %s", name, r.body)
		}
	}
}

// --- ada's break tests (#9791, fresh subagent on contract + diff) ---

func (f *tvFix) rowCount(t *testing.T, pin int64) int {
	t.Helper()
	var n int
	if err := f.a.db.QueryRow(`SELECT COUNT(*) FROM visit_notes WHERE pin_id = ?`, pin).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *tvFix) mustExec(t *testing.T, q string) {
	t.Helper()
	if _, err := f.a.db.Exec(q); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

func (f *tvFix) deletePin(t *testing.T, pin int64) string {
	t.Helper()
	r := tvDo(t, f.main.URL, "DELETE", fmt.Sprintf("/api/pins/%d", pin), "", f.signedIn())
	var del struct {
		Undo string `json:"undo"`
	}
	r.json(t, &del)
	if del.Undo == "" {
		t.Fatalf("no undo token: %d %s", r.code, r.body)
	}
	return del.Undo
}

// --- pruning: "a row whose day matches no current visit is never dropped" ---

// Setting aside a photo pin's last photo must not delete a pin that holds a
// visit note. A pin with the same text in pins.note survives this path.
func TestBreakSetAsideKeepsPinWithVisitNote(t *testing.T) {
	f := newNotesFix(t)
	if r := f.patchNote(t, 1, `{"note_day":"2026-07-01","note":"keep me"}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if r := tvDo(t, f.main.URL, "POST", "/api/photos/"+tvA1+"/aside", "", f.signedIn()); r.code != 200 {
		t.Fatalf("aside %d %s", r.code, r.body)
	}
	if n := f.rowCount(t, 1); n != 1 {
		t.Fatalf("set aside dropped the visit note: %d rows", n)
	}
}

// Control for the test above: the same flow with the old single note.
func TestBreakSetAsideKeepsPinWithLegacyNoteControl(t *testing.T) {
	f := newNotesFix(t)
	f.mustExec(t, `UPDATE pins SET note = 'keep me' WHERE id = 1`)
	if r := tvDo(t, f.main.URL, "POST", "/api/photos/"+tvA1+"/aside", "", f.signedIn()); r.code != 200 {
		t.Fatalf("aside %d %s", r.code, r.body)
	}
	var n int
	f.a.db.QueryRow(`SELECT COUNT(*) FROM pins WHERE id = 1`).Scan(&n)
	if n != 1 {
		t.Fatal("control: a pin with pins.note should survive")
	}
}

// Undo brings a photo pin back empty when its photo was filed elsewhere; the
// clean-up after the restore must not treat a pin with visit notes as bare.
func TestBreakUndoEmptyPhotoPinKeepsVisitNote(t *testing.T) {
	f := newNotesFix(t)
	f.patchNote(t, 1, `{"note_day":"2026-07-01","note":"keep me"}`)
	tok := f.deletePin(t, 1)
	f.mustExec(t, `INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (2, '`+tvA1+`', '2026-07-01T08:00:00Z', 52.0, -1.0)`)
	u := tvDo(t, f.main.URL, "POST", "/api/undo/"+tok, "", f.signedIn())
	if u.code != 200 || f.rowCount(t, 1) != 1 {
		t.Fatalf("undo %d %s, rows %d", u.code, u.body, f.rowCount(t, 1))
	}
}

// A twin pin holding a visit note is someone's work, as one with pins.note is.
func TestBreakUndoKeepsTwinWithVisitNote(t *testing.T) {
	f := newNotesFix(t)
	tok := f.deletePin(t, 1)
	f.mustExec(t, `INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, created_at) VALUES (9, 1, 'photo', 'a', 52.0, -1.0, '2026-07-01T08:00:00Z')`)
	f.mustExec(t, `INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (9, '`+tvA1+`', '2026-07-01T08:00:00Z', 52.0, -1.0)`)
	if r := f.patchNote(t, 9, `{"note_day":"2026-07-01","note":"written on the twin"}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	u := tvDo(t, f.main.URL, "POST", "/api/undo/"+tok, "", f.signedIn())
	if f.rowCount(t, 9) != 1 {
		t.Fatalf("undo %d dropped the note on the twin", u.code)
	}
}

// --- writes that split a note between pins.note and rows ---

// A plain note on a pin that has rows is accepted, shown on no visit, and
// comes back on the first visit once the rows are gone.
func TestBreakPlainNoteOnPinWithRows(t *testing.T) {
	for name, body := range map[string]string{
		"absent": `{"note":"stale client"}`,
		"null":   `{"note_day":null,"from_day":null,"note":"stale client"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newNotesFix(t)
			f.patchNote(t, 5, `{"note_day":"2026-07-06","from_day":"2026-07-05","note":"Food horrible"}`)
			r := f.patchNote(t, 5, body)
			if r.code == 400 || r.code == 409 {
				if got := f.rows(t, 5); got != "2026-07-06=Food horrible | pins.note=" {
					t.Fatalf("refused %d but changed: %s", r.code, got)
				}
				return
			}
			if got := strings.Join(visitNotes(f.notePins(t)[5]), ","); !strings.Contains(got, "stale client") {
				t.Errorf("accepted %d but shown on no visit: %s", r.code, got)
			}
			f.patchNote(t, 5, `{"note_day":"2026-07-06","note":""}`)
			if got := strings.Join(visitNotes(f.notePins(t)[5]), ","); strings.Contains(got, "2026-07-05=stale client") {
				t.Errorf("a hidden note came back on the first visit: %s", got)
			}
		})
	}
}

// A photo pin with an old note and no visits: the note must stay reachable
// when a per-visit write came first and a visit appears later.
func TestBreakLegacyNoteOnPinWithoutVisits(t *testing.T) {
	f := newNotesFix(t)
	f.mustExec(t, `INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, note, created_at) VALUES (8, 1, 'photo', 'c8', 52.1, -1.1, 'old text', '2026-07-05T09:00:00Z')`)
	// linus: refused (409) and unchanged, since there is no visit to keep the old note on
	if r := f.patchNote(t, 8, `{"note_day":"2026-07-07","note":"new text"}`); r.code != 409 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if got := f.rows(t, 8); got != " | pins.note=old text" {
		t.Fatalf("refused but changed: %s", got)
	}
	f.mustExec(t, `INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (8, 'a8000000-0000-0000-0000-000000000001', '2026-07-05T09:00:00Z', 52.1, -1.1)`)
	p := f.notePins(t)[8]
	seen := false
	for _, v := range p.Visits {
		seen = seen || v.Note == "old text"
	}
	for _, o := range p.OtherNotes {
		seen = seen || o.Note == "old text"
	}
	if !seen {
		t.Fatalf("old note on no visit: visits %v, other %v", p.Visits, p.OtherNotes)
	}
}

// Moving a note onto a visit that has its own note must not erase that note
// silently: both kept, or the call refused.
func TestBreakMoveOntoOccupiedDay(t *testing.T) {
	f := newNotesFix(t)
	f.patchNote(t, 5, `{"note_day":"2026-07-06","note":"second"}`)
	r := f.patchNote(t, 5, `{"note_day":"2026-07-06","from_day":"2026-07-05","note":"Food horrible"}`)
	if r.code != 200 && r.code != 409 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if got := f.rows(t, 5); !strings.Contains(got, "second") {
		t.Fatalf("destination note lost: %s", got)
	}
}

// --- PATCH edge cases expected to hold ---

func TestBreakPatchEdges(t *testing.T) {
	f := newNotesFix(t)
	for _, b := range []string{
		`{"note_day":"2026-07-06","from_day":"","note":"x"}`,
		`{"note_day":"2026-13-01","note":"x"}`,
		`{"note_day":"0000-00-00","note":"x"}`,
		`{"note_day":7,"note":"x"}`,
	} {
		if r := f.patchNote(t, 5, b); r.code != 400 {
			t.Errorf("%s: %d %s", b, r.code, r.body)
		}
	}
	if got := f.rows(t, 5); got != " | pins.note=Food horrible" {
		t.Fatalf("a refused write changed %s", got)
	}

	// day outside every visit: kept as other_notes, the first visit keeps its note
	if r := f.patchNote(t, 5, `{"note_day":"2030-01-01","note":"far"}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	ps := f.notePins(t)
	if got := strings.Join(visitNotes(ps[5]), ","); got != "2026-07-05=Food horrible,2026-07-06=" {
		t.Fatalf("got %s", got)
	}
	if len(ps[5].OtherNotes) != 1 || ps[5].OtherNotes[0].Day != "2030-01-01" {
		t.Fatalf("other_notes %v", ps[5].OtherNotes)
	}

	// from_day with no such row changes nothing else
	f.patchNote(t, 5, `{"note_day":"2026-07-06","from_day":"2026-01-01","note":"y"}`)
	if got := f.rows(t, 5); got != "2026-07-05=Food horrible,2026-07-06=y,2030-01-01=far | pins.note=" {
		t.Fatalf("rows: %s", got)
	}
}

// One visit: an empty write on it, then the note must not return.
func TestBreakOneDayVisitDelete(t *testing.T) {
	f := newNotesFix(t)
	if r := f.patchNote(t, 3, `{"note_day":"2026-07-04","note":""}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if got := f.rows(t, 3); got != " | pins.note=" {
		t.Fatalf("rows: %s", got)
	}
	if got := strings.Join(visitNotes(f.notePins(t)[3]), ","); got != "2026-07-04=" {
		t.Fatalf("got %s", got)
	}
}

// A move across a visit of another pin leaves the two same-day visits as the
// read rule says: first of the day only.
func TestBreakMoveAcrossOtherVisits(t *testing.T) {
	f := newNotesFix(t)
	f.patchNote(t, 6, `{"note_day":"2026-07-01","note":"misfiled"}`)
	f.patchNote(t, 6, `{"note_day":"2026-07-07","from_day":"2026-07-01","note":"castle"}`)
	ps := f.notePins(t)
	if got := strings.Join(visitNotes(ps[6]), ","); got != "2026-07-07=castle,2026-07-07=" {
		t.Fatalf("got %s", got)
	}
	if len(ps[6].OtherNotes) != 0 || f.rowCount(t, 7) != 0 {
		t.Fatalf("other %v, pin 7 rows %d", ps[6].OtherNotes, f.rowCount(t, 7))
	}
}

// --- share and TV payloads ---

// other_notes holds notes for days whose photos were removed; an anonymous
// share link must not carry them.
func TestBreakShareDoesNotCarryOtherNotes(t *testing.T) {
	f := newNotesFix(t)
	f.patchNote(t, 5, `{"note_day":"2026-01-01","note":"SECRET-ORPHAN"}`)
	r := tvDo(t, f.main.URL, "GET", "/api/share/old-share", "", nil)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if strings.Contains(string(r.body), "SECRET-ORPHAN") {
		t.Fatal("share payload carries a note of a day with no visit")
	}
}

func TestBreakTVNoteOnlyAtItsVisit(t *testing.T) {
	f := newNotesFix(t)
	f.patchNote(t, 5, `{"note_day":"2026-07-06","from_day":"2026-07-05","note":"Food horrible"}`)
	tok := f.pair(t, "Den")
	var trip struct {
		Pins []notePin `json:"pins"`
	}
	tvDo(t, f.door.URL, "GET", "/api/tv/trips/1", "", bearer(tok)).json(t, &trip)
	for _, p := range trip.Pins {
		if p.ID == 5 && p.Note != "" {
			t.Fatalf("pin-level note still set: %q", p.Note)
		}
	}
}

// --- export and import ---

func exportWith(t *testing.T, f *tvFix, list string) string {
	t.Helper()
	f.patchNote(t, 5, `{"note_day":"2026-07-06","from_day":"2026-07-05","note":"Food horrible"}`)
	ex := tvDo(t, f.main.URL, "GET", "/api/export", "", f.signedIn())
	const was = `"visit_notes":[{"day":"2026-07-06","note":"Food horrible"}]`
	if !strings.Contains(string(ex.body), was) {
		t.Fatalf("export has no visit_notes: %s", ex.body)
	}
	return strings.ReplaceAll(string(ex.body), was, `"visit_notes":`+list)
}

func TestBreakImportDirtyVisitNotes(t *testing.T) {
	f := newNotesFix(t)
	body := exportWith(t, f, `[{"day":"2026-07-06","note":"one"},{"day":"2026-07-06","note":"two"},{"day":"bad","note":"x"},{"day":"2026-07-05","note":"  "}]`)
	f2 := newNotesFix(t)
	f2.mustExec(t, `DELETE FROM holidays WHERE id = 1`)
	if im := tvDo(t, f2.main.URL, "POST", "/api/import", body, f2.signedIn()); im.code != 200 {
		t.Fatalf("import %d %s", im.code, im.body)
	}
	var id int64
	f2.a.db.QueryRow(`SELECT id FROM pins WHERE title = 'Premier Inn'`).Scan(&id)
	if got := f2.rows(t, id); got != "2026-07-06=two | pins.note=" {
		t.Fatalf("imported: %s", got)
	}
}

func TestBreakImportOldFormatKeepsLegacyNote(t *testing.T) {
	f := newNotesFix(t)
	ex := tvDo(t, f.main.URL, "GET", "/api/export", "", f.signedIn())
	if strings.Contains(string(ex.body), `"visit_notes"`) {
		t.Fatal("no rows: the field should be left out")
	}
	f2 := newNotesFix(t)
	f2.mustExec(t, `DELETE FROM holidays WHERE id = 1`)
	if im := tvDo(t, f2.main.URL, "POST", "/api/import", string(ex.body), f2.signedIn()); im.code != 200 {
		t.Fatalf("import %d %s", im.code, im.body)
	}
	var id int64
	f2.a.db.QueryRow(`SELECT id FROM pins WHERE title = 'Premier Inn'`).Scan(&id)
	if got := f2.rows(t, id); got != " | pins.note=Food horrible" {
		t.Fatalf("imported: %s", got)
	}
}

// --- undo of a whole trip ---

func TestBreakUndoHolidayDeleteRestoresNotes(t *testing.T) {
	f := newNotesFix(t)
	f.patchNote(t, 5, `{"note_day":"2026-07-06","note":"Breakfast ok"}`)
	r := tvDo(t, f.main.URL, "DELETE", "/api/holidays/1", "", f.signedIn())
	var del struct {
		Undo string `json:"undo"`
	}
	r.json(t, &del)
	if f.rowCount(t, 5) != 0 {
		t.Fatal("rows should cascade")
	}
	if u := tvDo(t, f.main.URL, "POST", "/api/undo/"+del.Undo, "", f.signedIn()); u.code != 200 {
		t.Fatalf("undo %d %s", u.code, u.body)
	}
	if got := f.rows(t, 5); got != "2026-07-05=Food horrible,2026-07-06=Breakfast ok | pins.note=" {
		t.Fatalf("after undo: %s", got)
	}
}

// --- migration ---

func TestBreakBootTwiceKeepsRowsAndLegacyNotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO holidays (id, name, color, start_at) VALUES (1, 'T', '#000000', '2026-01-01T00:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, note, created_at) VALUES (1, 1, 'manual', 1, 1, 'old', '2026-01-01T00:00:00Z')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES (2, 1, 'manual', 1, 1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO visit_notes (pin_id, day, note) VALUES (2, '2026-01-01', 'row')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	db.Close()
	db, err = openDB(path)
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	defer db.Close()
	var note, row string
	var n int
	db.QueryRow(`SELECT note FROM pins WHERE id = 1`).Scan(&note)
	db.QueryRow(`SELECT note FROM visit_notes WHERE pin_id = 2`).Scan(&row)
	db.QueryRow(`SELECT COUNT(*) FROM visit_notes`).Scan(&n)
	if note != "old" || row != "row" || n != 1 {
		t.Fatalf("note %q row %q count %d", note, row, n)
	}
}

// --- concurrency ---

func TestBreakConcurrentMovesKeepLegacyOnce(t *testing.T) {
	f := newNotesFix(t)
	var wg sync.WaitGroup
	codes := make([]int, 10)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.patchNote(t, 5, `{"note_day":"2026-07-06","from_day":"2026-07-05","note":"Food horrible"}`).code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("move %d: %d", i, c)
		}
	}
	if got := f.rows(t, 5); got != "2026-07-06=Food horrible | pins.note=" {
		t.Fatalf("rows: %s", got)
	}
}

func TestBreakConcurrentWriteAndDelete(t *testing.T) {
	f := newNotesFix(t)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.patchNote(t, 5, fmt.Sprintf(`{"note_day":"2026-07-06","note":"n%d"}`, i)).code
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		tvDo(t, f.main.URL, "DELETE", "/api/pins/5", "", f.signedIn())
	}()
	wg.Wait()
	for i, c := range codes {
		if c != 200 && c != 404 {
			t.Errorf("write %d: %d", i, c)
		}
	}
	if n := f.rowCount(t, 5); n != 0 {
		t.Fatalf("rows left after the pin went: %d", n)
	}
}

// USER #9792 (a test for every line): two notes on days with no visit come
// back oldest first.
func TestNotesOtherNotesOldestFirst(t *testing.T) {
	f := newNotesFix(t)
	for _, d := range []string{"2026-03-03", "2026-01-01", "2026-02-02"} {
		if r := f.patchNote(t, 1, `{"note_day":"`+d+`","note":"n`+d+`"}`); r.code != 200 {
			t.Fatalf("%d %s", r.code, r.body)
		}
	}
	got := f.notePins(t)[1].OtherNotes
	if len(got) != 3 || got[0].Day != "2026-01-01" || got[1].Day != "2026-02-02" || got[2].Day != "2026-03-03" {
		t.Fatalf("other_notes order %v", got)
	}
}

// USER #9792: every database-error branch of the notes code answers 500 (or
// fails the import) and leaves the data as it was. Faults come from SQLite
// triggers, a dropped table, or a NULL where a note must be.
func TestNotesDatabaseFaults(t *testing.T) {
	boom := func(table, when string) string {
		return `CREATE TRIGGER boom_` + table + `_` + strings.Fields(when)[0] + ` BEFORE ` + when + ` ON ` + table +
			` BEGIN SELECT RAISE(ABORT, 'injected fault'); END`
	}
	nullNotes := []string{
		`DROP TABLE visit_notes`,
		`CREATE TABLE visit_notes (pin_id INTEGER, day TEXT, note TEXT, PRIMARY KEY (pin_id, day))`,
		`INSERT INTO visit_notes VALUES (2, '2026-07-02', NULL)`,
	}
	cases := []struct {
		name   string
		faults []string
		method string
		path   string
		body   string
		want   int
	}{
		// the legacy note's copy to the first visit fails (handlers.go writeVisitNote)
		{"legacy copy insert", []string{boom("visit_notes", "INSERT")}, "PATCH", "/api/pins/5", `{"note_day":"2026-07-06","note":"x"}`, 500},
		// clearing pins.note fails
		{"legacy clear", []string{boom("pins", "UPDATE")}, "PATCH", "/api/pins/5", `{"note_day":"2026-07-06","from_day":"2026-07-05","note":"x"}`, 500},
		// deleting the from_day row fails (pin 2 has no legacy note)
		{"move delete", []string{`INSERT INTO visit_notes VALUES (2, '2026-07-02', 'a')`, boom("visit_notes", "DELETE")}, "PATCH", "/api/pins/2", `{"note_day":"2026-07-03","from_day":"2026-07-02","note":"a"}`, 500},
		// the write of the note itself fails
		{"note insert", []string{boom("visit_notes", "INSERT")}, "PATCH", "/api/pins/2", `{"note_day":"2026-07-03","note":"a"}`, 500},
		// reading pins for the first visit day fails (no table)
		{"write, no table", []string{`DROP TABLE visit_notes`}, "PATCH", "/api/pins/2", `{"note_day":"2026-07-03","note":"a"}`, 500},
		// map read: query fails, then a row that can't be read
		{"map, no table", []string{`DROP TABLE visit_notes`}, "GET", "/api/pins", "", 500},
		{"map, NULL note", nullNotes, "GET", "/api/pins", "", 500},
		// export: query fails, then a row that can't be read
		{"export, no table", []string{`DROP TABLE visit_notes`}, "GET", "/api/export", "", 500},
		{"export, NULL note", nullNotes, "GET", "/api/export", "", 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newNotesFix(t)
			var before string
			f.a.db.QueryRow(`SELECT group_concat(id || ':' || note, ',') FROM pins`).Scan(&before)
			for _, q := range c.faults {
				if _, err := f.a.db.Exec(q); err != nil {
					t.Fatalf("%v\n%s", err, q)
				}
			}
			r := tvDo(t, f.main.URL, c.method, c.path, c.body, f.signedIn())
			if r.code != c.want {
				t.Fatalf("%s %s: %d %s, want %d", c.method, c.path, r.code, r.body, c.want)
			}
			var after string
			f.a.db.QueryRow(`SELECT group_concat(id || ':' || note, ',') FROM pins`).Scan(&after)
			if before != after {
				t.Fatalf("pins changed on a failed call:\n%s\n%s", before, after)
			}
		})
	}
	// import: inserting a pin's visit notes fails, so the whole import fails
	t.Run("import insert", func(t *testing.T) {
		f := newNotesFix(t)
		f.patchNote(t, 5, `{"note_day":"2026-07-06","from_day":"2026-07-05","note":"Food horrible"}`)
		ex := tvDo(t, f.main.URL, "GET", "/api/export", "", f.signedIn())
		f2 := newNotesFix(t)
		f2.a.db.Exec(`DELETE FROM holidays WHERE id = 1`)
		f2.a.db.Exec(boom("visit_notes", "INSERT"))
		im := tvDo(t, f2.main.URL, "POST", "/api/import", string(ex.body), f2.signedIn())
		if im.code/100 == 2 {
			t.Fatalf("import with a failing note insert: %d %s", im.code, im.body)
		}
		var n int
		f2.a.db.QueryRow(`SELECT COUNT(*) FROM pins WHERE title = 'Premier Inn'`).Scan(&n)
		if n != 0 {
			t.Fatalf("failed import left %d pins behind", n)
		}
	})
}

// ada #9798: a move onto the first visit of a pin whose old note still sits
// there (no rows yet) must not overwrite that old note.
func TestNotesMoveOntoLegacyFirstDayRefused(t *testing.T) {
	f := newNotesFix(t)
	r := f.patchNote(t, 5, `{"note_day":"2026-07-05","from_day":"2026-07-06","note":"other words"}`)
	if r.code != 409 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if got := f.rows(t, 5); got != " | pins.note=Food horrible" {
		t.Fatalf("refused but changed: %s", got)
	}
	// the same words are no conflict
	if r := f.patchNote(t, 5, `{"note_day":"2026-07-05","from_day":"2026-07-06","note":"Food horrible"}`); r.code != 200 {
		t.Fatalf("same words: %d %s", r.code, r.body)
	}
	if got := f.rows(t, 5); got != "2026-07-05=Food horrible | pins.note=" {
		t.Fatalf("same words: %s", got)
	}
}
