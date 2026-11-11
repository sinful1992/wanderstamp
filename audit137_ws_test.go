package main

// Task 137 audit repro for the Wanderstamp server, bugs WS-1 to WS-4, moved into
// the project in task 142 with the fixes (each FAILed on v1.19.0), plus edge tests.

import (
	"net"
	"strings"
	"testing"
)

// WS-1: /api/export is documented as the way to move to a new server, but it
// leaves out GPX tracks, photos set aside, "same place?" answers and the
// master packing lists. After export + import on a new server the set-aside
// photos come back onto the map (and onto share links and the TV) at the next
// sync, and the tracks are gone.
func TestAudit137_WS1_ExportLeavesOutTracksAndSetAside(t *testing.T) {
	f := newNotesFix(t)
	const aside = "a9000000-0000-0000-0000-000000000009"
	for _, q := range []string{
		`INSERT INTO tracks (holiday_id, name, points) VALUES (1, 'audit137-walk', '[[54.5,-3.0],[54.6,-3.1]]')`,
		`INSERT INTO set_aside (asset_id, holiday_id, taken_at, set_at) VALUES ('` + aside + `', 1, '2026-07-05T09:00:00Z', '2026-07-06T09:00:00Z')`,
		`INSERT INTO photo_choices (asset_id, holiday_id, pin_id) VALUES ('a5000000-0000-0000-0000-000000000001', 1, 5)`,
		`INSERT INTO packing_templates (id, name) VALUES (1, 'audit137-always')`,
	} {
		if _, err := f.a.db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	r := tvDo(t, f.main.URL, "GET", "/api/export", "", f.signedIn())
	if r.code != 200 {
		t.Fatalf("export %d %s", r.code, r.body)
	}
	body := string(r.body)
	for what, needle := range map[string]string{
		"GPX track":           "audit137-walk",
		"set-aside photo":     aside,
		"master packing list": "audit137-always",
		"same-place answer":   "photo_choices",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("export has no %s (%q not in the document)", what, needle)
		}
	}
}

// WS-2: PATCH /api/pins/{id} stores any lat/lng (POST checks the range). The
// export then holds a pin that the import refuses, so the whole file is
// refused: one bad pin breaks the backup path.
func TestAudit137_WS2_PatchPinTakesImpossiblePosition(t *testing.T) {
	f := newNotesFix(t)
	r := tvDo(t, f.main.URL, "PATCH", "/api/pins/5", `{"lat":999,"lng":-999}`, f.signedIn())
	if r.code != 400 {
		t.Errorf("PATCH lat=999 lng=-999: want 400, got %d %s", r.code, r.body)
	}
	var lat, lng float64
	f.a.db.QueryRow(`SELECT lat, lng FROM pins WHERE id = 5`).Scan(&lat, &lng)
	if lat != 52.5 || lng != -1.4 {
		t.Errorf("pin 5 now sits at %v,%v", lat, lng)
	}
	msg := checkImportHoliday(importHoliday{Name: "x", Color: "#123456", StartAt: "2026-07-01T00:00:00Z",
		Pins: []importPin{{Kind: "manual", Lat: lat, Lng: lng}}})
	if msg != "" {
		t.Errorf("an export with this pin is refused by the import: %q", msg)
	}
}

// WS-3: PATCH /api/holidays/{id} writes field by field and checks name and
// colour last, so a refused request (400) has already changed the journal,
// dates and destination. An unknown id answers 200 {"ok":true}.
func TestAudit137_WS3_PatchHolidayHalfApplied(t *testing.T) {
	f := newNotesFix(t)
	r := tvDo(t, f.main.URL, "PATCH", "/api/holidays/1", `{"journal":"audit137 overwrite","color":"red"}`, f.signedIn())
	if r.code != 400 {
		t.Fatalf("want 400 for a bad colour, got %d", r.code)
	}
	var journal string
	f.a.db.QueryRow(`SELECT journal FROM holidays WHERE id = 1`).Scan(&journal)
	if journal != "lovely" {
		t.Errorf("the request was refused (400) but the journal is now %q", journal)
	}
	r = tvDo(t, f.main.URL, "PATCH", "/api/holidays/987654", `{"journal":"x"}`, f.signedIn())
	if r.code != 404 {
		t.Errorf("PATCH of a trip that does not exist: want 404, got %d %s", r.code, r.body)
	}
}

// WS-4: the TV door says it answers IPv6 link-local peers (fe80::/10 is in
// homeNets), but Go reports such a peer with its zone ("fe80::1%eth0"), which
// net.ParseIP refuses, so every link-local TV gets 404.
func TestAudit137_WS4_DoorRefusesLinkLocalWithZone(t *testing.T) {
	if !homePeer("fe80::1") {
		t.Fatalf("fe80::1 without a zone should be a home peer")
	}
	if !homePeer("fe80::1%eth0") {
		t.Errorf("homePeer(\"fe80::1%%eth0\") = false: a link-local TV is refused at the door")
	}
	a := newTestApp(t)
	if rec := doorReq(a, "GET", "/tv", "[fe80::1%eth0]:5555", nil); rec.Code != 200 {
		t.Errorf("GET /tv from [fe80::1%%eth0]: want 200, got %d", rec.Code)
	}
}

// WS-5 (the same pin posted twice) was fixed in task 139: its test moved into
// the project as TestAudit137_WS5 in pinkey_test.go (wanderstamp 761a700).

// WS-4 edges: a zone is dropped only from an IPv6 link-local address.
func TestWS4_ZoneOnlyOnLinkLocal(t *testing.T) {
	for addr, want := range map[string]bool{
		"fe80::1%eth0":         true,
		"fe80::abcd:1%wlan0":   true,
		"fe80::1%":             false, // empty zone
		"192.168.1.20%eth0":    false, // IPv4 never has a zone
		"fd00::1%eth0":         false, // a zone on a non-link-local address
		"2001:db8::1%eth0":     false,
		"%eth0":                false,
		"fe80::1%eth0%x":       false,
		"100.64.0.1":           false, // tailnet stays refused
		"192.168.1.20":         true,
	} {
		if got := homePeer(addr); got != want {
			t.Errorf("homePeer(%q) = %v, want %v", addr, got, want)
		}
	}
	if !refusedPeer("fe80::1%eth0", []net.IP{net.ParseIP("fe80::1")}) {
		t.Errorf("a refused gateway on link-local must stay refused with its zone")
	}
}

// WS-2/WS-3 edges on the pin side: a refused PATCH writes nothing, a
// missing pin is 404, lat and lng go together, a photo pin does not move.
func TestWS3_PatchPinAllOrNothing(t *testing.T) {
	f := newNotesFix(t)
	get := func() (title, cover string, lat, lng float64) {
		f.a.db.QueryRow(`SELECT title, cover_asset, lat, lng FROM pins WHERE id = 5`).Scan(&title, &cover, &lat, &lng)
		return
	}
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"title":"changed","cover_asset":"not-on-this-pin"}`, 400},
		{`{"title":"changed","lat":91,"lng":0}`, 400},
		{`{"title":"changed","lat":0,"lng":180.5}`, 400},
		{`{"title":"changed","lat":10}`, 400},
		{`{"title":"changed","lng":10}`, 400},
		{`{"title":"changed","from_day":"2026-07-05"}`, 400},
		{`{"title":"changed","note_day":"2026-07-05"}`, 400},
		{`{"title":"changed","note":"x","note_day":"July"}`, 400},
	} {
		r := tvDo(t, f.main.URL, "PATCH", "/api/pins/5", c.body, f.signedIn())
		if r.code != c.code {
			t.Errorf("PATCH %s: want %d, got %d %s", c.body, c.code, r.code, r.body)
		}
		if title, _, lat, lng := get(); title != "Premier Inn" || lat != 52.5 || lng != -1.4 {
			t.Fatalf("after refused %s the pin is %q at %v,%v", c.body, title, lat, lng)
		}
	}
	if r := tvDo(t, f.main.URL, "PATCH", "/api/pins/987654", `{"title":"x"}`, f.signedIn()); r.code != 404 {
		t.Errorf("PATCH a missing pin: want 404, got %d %s", r.code, r.body)
	}
	// a good request still writes every field, edges of the range included
	r := tvDo(t, f.main.URL, "PATCH", "/api/pins/5",
		`{"title":"  New name ","cover_asset":"a5000000-0000-0000-0000-000000000002","lat":-90,"lng":180}`, f.signedIn())
	if r.code != 200 {
		t.Fatalf("good PATCH: %d %s", r.code, r.body)
	}
	if title, cover, lat, lng := get(); title != "New name" || cover != "a5000000-0000-0000-0000-000000000002" || lat != -90 || lng != 180 {
		t.Errorf("after the good PATCH: %q %q %v,%v", title, cover, lat, lng)
	}
	// a visit note and a title land together
	r = tvDo(t, f.main.URL, "PATCH", "/api/pins/5", `{"title":"With note","note":"nice","note_day":"2026-07-06"}`, f.signedIn())
	if r.code != 200 {
		t.Fatalf("note+title PATCH: %d %s", r.code, r.body)
	}
	var note string
	f.a.db.QueryRow(`SELECT note FROM visit_notes WHERE pin_id = 5 AND day = '2026-07-06'`).Scan(&note)
	if title, _, _, _ := get(); title != "With note" || note != "nice" {
		t.Errorf("title %q note %q", title, note)
	}
	// a photo pin follows its photos: a move is accepted but changes nothing
	r = tvDo(t, f.main.URL, "PATCH", "/api/pins/6", `{"lat":1,"lng":1}`, f.signedIn())
	var plat float64
	f.a.db.QueryRow(`SELECT lat FROM pins WHERE id = 6`).Scan(&plat)
	if r.code != 200 || plat != 52.3 {
		t.Errorf("photo pin move: %d, lat now %v", r.code, plat)
	}
}

// WS-3 edges on the trip side: every refusal leaves the trip as it was.
func TestWS3_PatchHolidayAllOrNothing(t *testing.T) {
	f := newNotesFix(t)
	read := func() (name, journal, color, start string) {
		f.a.db.QueryRow(`SELECT name, journal, color, start_at FROM holidays WHERE id = 1`).Scan(&name, &journal, &color, &start)
		return
	}
	n0, j0, c0, s0 := read()
	for _, body := range []string{
		`{"journal":"x","name":"   "}`,
		`{"start_at":"2026-01-01","color":"#12345"}`,
		`{"journal":"x","cover_asset":"not-on-this-trip"}`,
		`{"journal":"x","start_at":"tomorrow"}`,
		`{"journal":"x","start_at":"2027-01-01","end_at":"2026-01-01"}`,
	} {
		r := tvDo(t, f.main.URL, "PATCH", "/api/holidays/1", body, f.signedIn())
		if r.code != 400 {
			t.Errorf("PATCH %s: want 400, got %d %s", body, r.code, r.body)
		}
		if n, j, c, s := read(); n != n0 || j != j0 || c != c0 || s != s0 {
			t.Fatalf("after refused %s: %q %q %q %q", body, n, j, c, s)
		}
	}
	r := tvDo(t, f.main.URL, "PATCH", "/api/holidays/1", `{"name":" Renamed ","color":"#00aa00","journal":"new words"}`, f.signedIn())
	if r.code != 200 {
		t.Fatalf("good PATCH: %d %s", r.code, r.body)
	}
	if n, j, c, _ := read(); n != "Renamed" || j != "new words" || c != "#00aa00" {
		t.Errorf("after the good PATCH: %q %q %q", n, j, c)
	}
}
