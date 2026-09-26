package main

import (
	"net/http"
	"testing"
)

func TestDestinationCanBeSetChangedAndRemoved(t *testing.T) {
	a := newTestApp(t)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Bransgore', '#123456', '2026-05-23T00:00:00Z', '2026-05-24T23:59:59Z')`)
	read := func() (name string, lat float64, box string) {
		a.db.QueryRow(`SELECT dest_name, dest_lat, dest_bbox FROM holidays WHERE id = 1`).Scan(&name, &lat, &box)
		return
	}

	if code := patchHoliday(t, a, `{"dest_name":"Bransgore, Dorset","dest_lat":50.77,"dest_lng":-1.73,"dest_bbox":[50.75,-1.76,50.79,-1.70]}`); code != http.StatusOK {
		t.Fatalf("set destination: got %d", code)
	}
	if n, lat, box := read(); n != "Bransgore, Dorset" || lat != 50.77 || box != "[50.75,-1.76,50.79,-1.7]" {
		t.Errorf("after set: %q %v %q", n, lat, box)
	}
	if h := listHolidays(t, a)["Bransgore"]; len(h.DestBBox) != 4 || h.DestBBox[3] != -1.7 {
		t.Errorf("list carries the box: %v", h.DestBBox)
	}

	// typed coordinates: a point with no box clears the old box
	if code := patchHoliday(t, a, `{"dest_name":"50.7, -1.7","dest_lat":50.7,"dest_lng":-1.7}`); code != http.StatusOK {
		t.Fatalf("typed coords: got %d", code)
	}
	if _, _, box := read(); box != "" {
		t.Errorf("box survived a point-only destination: %q", box)
	}

	// a malformed box or point is refused and writes nothing
	for _, body := range []string{
		`{"dest_name":"X","dest_lat":95,"dest_lng":0}`,
		`{"dest_name":"X","dest_lat":1,"dest_lng":1,"dest_bbox":[2,0,1,0]}`,
		`{"dest_name":"X","dest_lat":1,"dest_lng":1,"dest_bbox":[1,2,3]}`,
		`{"name":"Renamed","dest_name":"X","dest_lat":1,"dest_lng":1,"dest_bbox":[0,0,100,0]}`,
	} {
		if code := patchHoliday(t, a, body); code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", body, code)
		}
	}
	if n, _, _ := read(); n != "50.7, -1.7" {
		t.Errorf("rejected patch wrote the destination: %q", n)
	}
	var name string
	a.db.QueryRow(`SELECT name FROM holidays WHERE id = 1`).Scan(&name)
	if name != "Bransgore" {
		t.Errorf("rejected patch still renamed the trip to %q", name)
	}

	// a rename without dest_name leaves the destination alone
	patchHoliday(t, a, `{"name":"Bransgore caravan park"}`)
	if n, _, _ := read(); n != "50.7, -1.7" {
		t.Errorf("rename touched the destination: %q", n)
	}

	// "" removes it, point and all
	patchHoliday(t, a, `{"dest_name":""}`)
	if n, lat, box := read(); n != "" || lat != 0 || box != "" {
		t.Errorf("after remove: %q %v %q", n, lat, box)
	}
}

func TestCreateStoresTheBox(t *testing.T) {
	a := newTestApp(t)
	code, h := createHoliday(t, a, map[string]any{
		"name": "Lakes", "color": "#2e8b57", "start_at": day(-1),
		"dest_name": "Lake District", "dest_lat": 54.5, "dest_lng": -3.1,
		"dest_bbox": []float64{54.1, -3.6, 54.8, -2.6},
	})
	if code != http.StatusCreated || len(h.DestBBox) != 4 {
		t.Fatalf("create: %d %v", code, h.DestBBox)
	}
	if code, _ := createHoliday(t, a, map[string]any{
		"name": "Bad", "color": "#2e8b57", "start_at": day(3), "dest_name": "X", "dest_lat": 1, "dest_lng": 1, "dest_bbox": []float64{9, 0, 1, 0},
	}); code != http.StatusBadRequest {
		t.Errorf("bad box on create: got %d", code)
	}
}

func TestNominatimBoxIsReordered(t *testing.T) {
	b := nominatimBBox([]string{"54.1", "54.8", "-3.6", "-2.6"})
	if len(b) != 4 || b[0] != 54.1 || b[1] != -3.6 || b[2] != 54.8 || b[3] != -2.6 {
		t.Errorf("got %v", b)
	}
	if nominatimBBox([]string{"1", "x", "2", "3"}) != nil || nominatimBBox(nil) != nil {
		t.Error("bad input should give no box")
	}
}
