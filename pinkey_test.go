package main

// Task 139, WS-5: POST /api/pins takes a client_id so a pin sent twice (the
// answer was lost on weak signal) is stored once.

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const keyBody = `{"holiday_id":1,"lat":54.55,"lng":-3.05,"title":"picnic","note":"","created_at":"2026-07-05T12:00:00Z"%s}`

func keyed(id string) string {
	if id == "" {
		return fmt.Sprintf(keyBody, "")
	}
	return fmt.Sprintf(keyBody, `,"client_id":"`+id+`"`)
}

func (f *tvFix) postPin(t *testing.T, body string, hdr map[string]string) tvResp {
	t.Helper()
	if hdr == nil {
		hdr = f.signedIn()
	}
	return tvDo(t, f.main.URL, "POST", "/api/pins", body, hdr)
}

func (f *tvFix) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.a.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
	return n
}

func pinID(t *testing.T, r tvResp) int64 {
	t.Helper()
	var p struct {
		ID int64 `json:"id"`
	}
	r.json(t, &p)
	return p.ID
}

// second family member, signed in
func (f *tvFix) secondUser(t *testing.T) map[string]string {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash) VALUES (2, 'kayleigh', 'x')`,
		`INSERT INTO sessions (token, user_id, expires_at) VALUES ('` + hashToken("sess-2") + `', 2, '2099-01-01T00:00:00Z')`,
	} {
		if _, err := f.a.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]string{"Cookie": sessionCookie + "=sess-2"}
}

// The audit's repro (docs/audit137 WS-5), with the key the page now sends.
func TestAudit137_WS5_SamePinPostedTwiceIsStoredOnce(t *testing.T) {
	f := newNotesFix(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		r := f.postPin(t, keyed("1794298543932:k3x9af"), nil)
		if r.code != 201 {
			t.Fatalf("post %d: %d %s", i+1, r.code, r.body)
		}
		ids = append(ids, pinID(t, r))
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'picnic'`); n != 1 {
		t.Errorf("the same pin sent three times is stored %d times, want 1", n)
	}
	if ids[0] == 0 || ids[1] != ids[0] || ids[2] != ids[0] {
		t.Errorf("repeats answer with pins %v, want the first pin each time", ids)
	}
}

// No key (a page from before 1.18.0): exactly today's behaviour.
func TestPinKey_NoKeyStoresEveryPost(t *testing.T) {
	f := newNotesFix(t)
	for _, body := range []string{keyed(""), keyed(""), fmt.Sprintf(keyBody, `,"client_id":""`)} {
		if r := f.postPin(t, body, nil); r.code != 201 {
			t.Fatalf("%d %s", r.code, r.body)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'picnic'`); n != 3 {
		t.Errorf("3 posts without a key stored %d pins, want 3", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pin_keys`); n != 0 {
		t.Errorf("posts without a key stored %d keys, want 0", n)
	}
}

func TestPinKey_Shape(t *testing.T) {
	f := newNotesFix(t)
	good := []string{"a", "t1", "1794298543932:k3x9af", "3b241101-e2bb-4255-8caf-4136c566a962", "A_b.c:d-9", strings.Repeat("x", 64)}
	for _, id := range good {
		if r := f.postPin(t, keyed(id), nil); r.code != 201 {
			t.Errorf("client_id %q: %d %s, want 201", id, r.code, r.body)
		}
	}
	before := f.count(t, `SELECT COUNT(*) FROM pins`)
	bad := []string{" ", "a b", "a/b", "ąžuolas", strings.Repeat("x", 65), "a\\nb", "x'; DROP TABLE pins;--", "a,b", "%41"}
	for _, id := range bad {
		r := f.postPin(t, keyed(id), nil)
		if r.code != 400 || !strings.Contains(string(r.body), "invalid client_id") {
			t.Errorf("client_id %q: %d %s, want 400 invalid client_id", id, r.code, r.body)
		}
	}
	// not a string at all
	for _, raw := range []string{`,"client_id":12345678`, `,"client_id":["a"]`, `,"client_id":{"a":1}`} {
		if r := f.postPin(t, fmt.Sprintf(keyBody, raw), nil); r.code != 400 {
			t.Errorf("client_id %s: %d, want 400", raw, r.code)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pins`); n != before {
		t.Errorf("refused posts stored %d pins", n-before)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pin_keys`); n != len(good) {
		t.Errorf("%d keys stored, want %d", n, len(good))
	}
}

// A repeat answers with the pin as it is now, and its own body is ignored.
func TestPinKey_RepeatReturnsThePinAsItIsNow(t *testing.T) {
	f := newNotesFix(t)
	id := pinID(t, f.postPin(t, keyed("k-edit"), nil))
	if r := tvDo(t, f.main.URL, "PATCH", fmt.Sprintf("/api/pins/%d", id), `{"title":"lunch stop","lat":54.6,"lng":-3.1}`, f.signedIn()); r.code != 200 {
		t.Fatalf("patch %d %s", r.code, r.body)
	}
	other := `{"holiday_id":2,"lat":1,"lng":2,"title":"something else","note":"n","client_id":"k-edit"}`
	r := f.postPin(t, other, nil)
	if r.code != 201 {
		t.Fatalf("repeat %d %s", r.code, r.body)
	}
	var p struct {
		ID        int64   `json:"id"`
		HolidayID int64   `json:"holiday_id"`
		Kind      string  `json:"kind"`
		Lat       float64 `json:"lat"`
		Title     string  `json:"title"`
		CreatedAt string  `json:"created_at"`
		CreatedBy string  `json:"created_by"`
	}
	r.json(t, &p)
	if p.ID != id || p.Title != "lunch stop" || p.Lat != 54.6 || p.HolidayID != 1 || p.Kind != "manual" ||
		p.CreatedAt != "2026-07-05T12:00:00Z" || p.CreatedBy != "giedrius" {
		t.Errorf("repeat answered %+v, want pin %d as edited", p, id)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'something else'`); n != 0 {
		t.Errorf("the repeat's different body was stored")
	}
}

// Deleted on another device, then the queued copy arrives: gone stays gone.
func TestPinKey_RepeatAfterDeleteIs410AndUndoBringsItBack(t *testing.T) {
	f := newNotesFix(t)
	id := pinID(t, f.postPin(t, keyed("k-del"), nil))
	del := tvDo(t, f.main.URL, "DELETE", fmt.Sprintf("/api/pins/%d", id), "", f.signedIn())
	if del.code != 200 {
		t.Fatalf("delete %d %s", del.code, del.body)
	}
	var d struct {
		Undo string `json:"undo"`
	}
	del.json(t, &d)
	for i := 0; i < 2; i++ {
		r := f.postPin(t, keyed("k-del"), nil)
		if r.code != 410 || !strings.Contains(string(r.body), "pin was deleted") {
			t.Fatalf("repeat after delete: %d %s, want 410 pin was deleted", r.code, r.body)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'picnic'`); n != 0 {
		t.Fatalf("a deleted pin came back %d times", n)
	}
	if r := tvDo(t, f.main.URL, "POST", "/api/undo/"+d.Undo, "", f.signedIn()); r.code != 200 {
		t.Fatalf("undo %d %s", r.code, r.body)
	}
	r := f.postPin(t, keyed("k-del"), nil)
	if r.code != 201 || pinID(t, r) != id {
		t.Errorf("repeat after undo: %d %s, want 201 with pin %d", r.code, r.body, id)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'picnic'`); n != 1 {
		t.Errorf("after undo %d pins, want 1", n)
	}
}

// SQLite hands a deleted pin's number to the next pin. A repeat must never
// answer with that stranger.
func TestPinKey_RepeatNeverReturnsAnotherPinWithAReusedID(t *testing.T) {
	f := newNotesFix(t)
	id := pinID(t, f.postPin(t, keyed("k-reuse"), nil))
	if r := tvDo(t, f.main.URL, "DELETE", fmt.Sprintf("/api/pins/%d", id), "", f.signedIn()); r.code != 200 {
		t.Fatalf("delete %d", r.code)
	}
	stranger := `{"holiday_id":1,"lat":50,"lng":-2,"title":"stranger","created_at":"2026-07-05T12:00:00Z"}`
	sid := pinID(t, f.postPin(t, stranger, nil))
	if sid != id {
		t.Skipf("id %d was not reused (got %d): nothing to check", id, sid)
	}
	r := f.postPin(t, keyed("k-reuse"), nil)
	if r.code != 410 {
		t.Errorf("repeat answered %d %s, want 410 (pin %d is now another pin)", r.code, r.body, id)
	}
	// the same with a keyed stranger
	if r := tvDo(t, f.main.URL, "DELETE", fmt.Sprintf("/api/pins/%d", sid), "", f.signedIn()); r.code != 200 {
		t.Fatalf("delete %d", r.code)
	}
	sid2 := pinID(t, f.postPin(t, strings.Replace(stranger, `"title"`, `"client_id":"k-other","title"`, 1), nil))
	if sid2 == id {
		if r := f.postPin(t, keyed("k-reuse"), nil); r.code != 410 {
			t.Errorf("repeat answered %d %s, want 410 (pin %d belongs to key k-other)", r.code, r.body, id)
		}
	}
}

// A key is per user: someone else's key never returns their pin.
func TestPinKey_PerUser(t *testing.T) {
	f := newNotesFix(t)
	k := f.secondUser(t)
	a := pinID(t, f.postPin(t, keyed("same-key"), nil))
	rb := f.postPin(t, keyed("same-key"), k)
	if rb.code != 201 {
		t.Fatalf("second user %d %s", rb.code, rb.body)
	}
	b := pinID(t, rb)
	if a == b {
		t.Fatalf("the second user got the first user's pin %d", a)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'picnic'`); n != 2 {
		t.Errorf("%d pins, want one per user", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE id = ? AND created_by = 'kayleigh'`, b); n != 1 {
		t.Errorf("second user's pin is not theirs")
	}
	// each repeat finds its own
	if got := pinID(t, f.postPin(t, keyed("same-key"), k)); got != b {
		t.Errorf("second user's repeat got pin %d, want %d", got, b)
	}
	if got := pinID(t, f.postPin(t, keyed("same-key"), nil)); got != a {
		t.Errorf("first user's repeat got pin %d, want %d", got, a)
	}
}

// The phone retries while the first post is still on its way.
func TestPinKey_TwentyPostsAtOnceStoreOnePin(t *testing.T) {
	f := newNotesFix(t)
	const n = 20
	codes := make([]int, n)
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := tvDo(t, f.main.URL, "POST", "/api/pins", keyed("k-race"), f.signedIn())
			codes[i] = r.code
			if r.code == 201 {
				var p struct {
					ID int64 `json:"id"`
				}
				r.json(t, &p)
				ids[i] = p.ID
			}
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 201 || ids[i] != ids[0] || ids[i] == 0 {
			t.Fatalf("codes %v ids %v: want 201 and one pin for all", codes, ids)
		}
	}
	if c := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'picnic'`); c != 1 {
		t.Errorf("%d pins, want 1", c)
	}
}

// A post that fails stores no key, so the corrected post goes through.
func TestPinKey_FailedPostLeavesNoKey(t *testing.T) {
	f := newNotesFix(t)
	for _, body := range []string{
		`{"holiday_id":999,"lat":1,"lng":2,"title":"picnic","client_id":"k-fail"}`,
		`{"holiday_id":1,"lat":91,"lng":2,"title":"picnic","client_id":"k-fail"}`,
		`{"holiday_id":1,"lat":1,"lng":2,"title":"picnic","created_at":"nonsense","client_id":"k-fail"}`,
	} {
		if r := f.postPin(t, body, nil); r.code != 400 {
			t.Fatalf("%s: %d %s, want 400", body, r.code, r.body)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pin_keys`); n != 0 {
		t.Fatalf("%d keys stored by failed posts", n)
	}
	if r := f.postPin(t, keyed("k-fail"), nil); r.code != 201 {
		t.Errorf("the corrected post: %d %s, want 201", r.code, r.body)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'picnic'`); n != 1 {
		t.Errorf("%d pins, want 1", n)
	}
}

// The whole trip was deleted: its pins went with it.
func TestPinKey_RepeatAfterTripDeleteIs410(t *testing.T) {
	f := newNotesFix(t)
	body := `{"holiday_id":3,"lat":1,"lng":2,"title":"picnic","client_id":"k-trip"}`
	if r := f.postPin(t, body, nil); r.code != 201 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if r := tvDo(t, f.main.URL, "DELETE", "/api/holidays/3", "", f.signedIn()); r.code != 200 {
		t.Fatalf("delete trip %d %s", r.code, r.body)
	}
	if r := f.postPin(t, body, nil); r.code != 410 {
		t.Errorf("repeat after the trip was deleted: %d %s, want 410", r.code, r.body)
	}
}

// A database opened by 1.17.0 gets the table and the column at start, and
// every row it held is unchanged.
func TestPinKey_OldDatabaseGetsTheTable(t *testing.T) {
	f := newNotesFix(t)
	for _, q := range []string{`DROP TABLE pin_keys`, `ALTER TABLE pins DROP COLUMN client_id`} {
		if _, err := f.a.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := f.count(t, `SELECT COUNT(*) FROM pins`)
	migrate(f.a.db)
	migrate(f.a.db) // a second start changes nothing
	if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE client_id = ''`); n != before {
		t.Errorf("%d of %d old pins have an empty client_id", n, before)
	}
	if r := f.postPin(t, keyed("k-old"), nil); r.code != 201 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if r := f.postPin(t, keyed("k-old"), nil); r.code != 201 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'picnic'`); n != 1 {
		t.Errorf("%d pins, want 1", n)
	}
}

// Every database fault answers 500 and stores nothing, least of all a pin
// without its key (that pin would be doubled by the retry).
func TestPinKey_DatabaseFaultsStoreNothing(t *testing.T) {
	t.Run("key insert fails: the pin is rolled back", func(t *testing.T) {
		f := newNotesFix(t)
		if _, err := f.a.db.Exec(`CREATE TRIGGER no_keys BEFORE INSERT ON pin_keys BEGIN SELECT RAISE(ABORT, 'forced'); END`); err != nil {
			t.Fatal(err)
		}
		if r := f.postPin(t, keyed("k-fault"), nil); r.code != 500 {
			t.Fatalf("%d %s, want 500", r.code, r.body)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'picnic'`); n != 0 {
			t.Errorf("%d pins stored without their key", n)
		}
		// without a key the trigger is never reached
		if r := f.postPin(t, keyed(""), nil); r.code != 201 {
			t.Errorf("no key: %d %s", r.code, r.body)
		}
	})
	t.Run("key table unreadable", func(t *testing.T) {
		f := newNotesFix(t)
		if _, err := f.a.db.Exec(`DROP TABLE pin_keys`); err != nil {
			t.Fatal(err)
		}
		if r := f.postPin(t, keyed("k-fault"), nil); r.code != 500 {
			t.Fatalf("%d %s, want 500", r.code, r.body)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM pins WHERE title = 'picnic'`); n != 0 {
			t.Errorf("%d pins stored", n)
		}
	})
	t.Run("pin unreadable on a repeat", func(t *testing.T) {
		f := newNotesFix(t)
		if r := f.postPin(t, keyed("k-fault"), nil); r.code != 201 {
			t.Fatalf("%d %s", r.code, r.body)
		}
		// text where the answer needs a number makes the read fail
		if _, err := f.a.db.Exec(`UPDATE pins SET lat = 'north' WHERE client_id = 'k-fault'`); err != nil {
			t.Fatal(err)
		}
		if r := f.postPin(t, keyed("k-fault"), nil); r.code != 500 {
			t.Errorf("%d %s, want 500", r.code, r.body)
		}
	})
	t.Run("database closed", func(t *testing.T) {
		f := newNotesFix(t)
		f.a.db.Close()
		// straight to the handler: the sign-in check needs the database too
		req := httptest.NewRequest("POST", "/api/pins", strings.NewReader(keyed("k-fault")))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.a.handleCreatePin(rec, req)
		if rec.Code != 500 {
			t.Errorf("%d %s, want 500", rec.Code, rec.Body)
		}
	})
}

// With no live trip and no holiday_id a keyed post is refused and leaves no key.
func TestPinKey_NoLiveTrip(t *testing.T) {
	f := newNotesFix(t)
	body := `{"lat":1,"lng":2,"title":"picnic","client_id":"k-nolive"}`
	if r := f.postPin(t, body, nil); r.code != 400 {
		t.Fatalf("%d %s, want 400", r.code, r.body)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM pin_keys`); n != 0 {
		t.Errorf("%d keys", n)
	}
	if _, err := f.a.db.Exec(`UPDATE holidays SET end_at = NULL WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	a := f.postPin(t, body, nil)
	b := f.postPin(t, body, nil)
	if a.code != 201 || b.code != 201 || pinID(t, a) != pinID(t, b) {
		t.Errorf("live trip: %d %s then %d %s, want one pin twice", a.code, a.body, b.code, b.body)
	}
}
