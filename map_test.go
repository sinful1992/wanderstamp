package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func getMap(t *testing.T, a *app) (me map[string]any, hs []holidayOut, pins []pinOut) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/map", nil)
	req = req.WithContext(context.WithValue(req.Context(), userKey, sessionUser{Username: "ada", IsAdmin: true}))
	rec := httptest.NewRecorder()
	a.handleMap(rec, req)
	var out struct {
		Me       map[string]any `json:"me"`
		Holidays []holidayOut   `json:"holidays"`
		Pins     []pinOut       `json:"pins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("map: %v: %s", err, rec.Body)
	}
	return out.Me, out.Holidays, out.Pins
}

func TestMapIsOneRoundTripWithCountsFromThePins(t *testing.T) {
	a := newTestApp(t)
	// the destination point is far off, so only the outline can decide arrival
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at, dest_name, dest_lat, dest_lng, dest_area)
		VALUES (1, 'Trip', '#123456', '2026-07-01T00:00:00Z', '2026-07-08T00:00:00Z', 'Somewhere', 30, 30,
		        '[[[[-1,49],[1,49],[1,51],[-1,51],[-1,49]]]]')`)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (2, 'Empty', '#654321', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')`)
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES (1, 1, 'manual', 0, 0, '2026-07-01T10:00:00Z')`)
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES (2, 1, 'photo', 50, 0, '2026-07-02T10:00:00Z')`)
	a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES
		(2, 'a1', '2026-07-02T10:00:00Z', 50, 0), (2, 'a2', '2026-07-02T11:00:00Z', 50, 0)`)

	me, hs, pins := getMap(t, a)
	if me["username"] != "ada" || me["is_admin"] != true {
		t.Errorf("me = %v", me)
	}
	if len(hs) != 2 || len(pins) != 2 {
		t.Fatalf("got %d trips, %d pins", len(hs), len(pins))
	}
	trip := hs[0]
	if trip.Name != "Trip" || trip.PinCount != 2 || trip.PhotoCount != 2 || !trip.DestArea || trip.ArrivalPin != 2 {
		t.Errorf("trip = %+v, want 2 pins, 2 photos, outline, arrival at pin 2", trip)
	}
	if e := hs[1]; e.PinCount != 0 || e.PhotoCount != 0 || e.DestArea || e.ArrivalPin != 0 {
		t.Errorf("empty trip = %+v", e)
	}

	// The outline is cached. A write that changes its length is picked up
	// even without forgetArea...
	a.db.Exec(`UPDATE holidays SET dest_area = '[[[[-1,-10],[1,-10],[1,10],[-1,10],[-1,-10]]]]' WHERE id = 1`)
	if _, hs, _ = getMap(t, a); hs[0].ArrivalPin != 1 {
		t.Errorf("after a longer outline: arrival %d, want 1", hs[0].ArrivalPin)
	}
	// ...and a same-length one after forgetArea, which every write path calls.
	a.db.Exec(`UPDATE holidays SET dest_area = '[[[[-1,49],[1,49],[1,51],[-1,51],[-1,49]]]]' WHERE id = 1`)
	if _, hs, _ = getMap(t, a); hs[0].ArrivalPin != 2 {
		t.Fatalf("back to the first outline: arrival %d, want 2", hs[0].ArrivalPin)
	}
	a.db.Exec(`UPDATE holidays SET dest_area = '[[[[-1,-9],[1,-9],[1,19],[-1,19],[-1,-9]]]]' WHERE id = 1`)
	if _, hs, _ = getMap(t, a); hs[0].ArrivalPin != 2 {
		t.Errorf("test setup: a same-length write should hit the cache (arrival 2), got %d", hs[0].ArrivalPin)
	}
	a.forgetArea(1)
	if _, hs, _ = getMap(t, a); hs[0].ArrivalPin != 1 {
		t.Errorf("after forgetArea: arrival %d, want 1", hs[0].ArrivalPin)
	}
	// clearing the outline clears it from the list
	a.db.Exec(`UPDATE holidays SET dest_area = '' WHERE id = 1`)
	if _, hs, _ = getMap(t, a); hs[0].DestArea || hs[0].ArrivalPin != 0 {
		t.Errorf("after clearing: %+v", hs[0])
	}
}
