package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A square-ish "Paris" relation and a France-shaped MultiPolygon: a mainland
// plus a far-off island, with the mainland's bounding box covering places
// that aren't in it (the thing a rectangle got wrong).
const parisGeo = `{"type":"Polygon","coordinates":[[[2.22,48.81],[2.47,48.81],[2.47,48.91],[2.22,48.91],[2.22,48.81]]]}`
const franceGeo = `{"type":"MultiPolygon","coordinates":[
  [[[-4.8,48.4],[-1.5,43.4],[3.1,42.4],[7.6,43.8],[8.2,49.0],[2.5,51.1],[-4.8,48.4]]],
  [[[55.2,-21.4],[55.8,-21.4],[55.8,-20.9],[55.2,-20.9],[55.2,-21.4]]]]}`

func fakeNominatim(t *testing.T) *int {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/reverse" {
			if r.URL.Query().Get("lat") == "0.000000" {
				fmt.Fprint(w, `{"error":"Unable to geocode"}`) // the open sea
				return
			}
			fmt.Fprint(w, `{"display_name":"Bransgore, New Forest, Hampshire, England, United Kingdom"}`)
			return
		}
		geo := map[string]string{"R7444": parisGeo, "R2202162": franceGeo, "N1": `{"type":"Point","coordinates":[1,1]}`}[r.URL.Query().Get("osm_ids")]
		if geo == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `[{"geojson":%s}]`, geo)
	}))
	old := nominatimBase
	nominatimBase = srv.URL
	t.Cleanup(func() { srv.Close(); nominatimBase = old })
	return &calls
}

func addPin(a *app, id, hid int64, lat, lng float64, at string) {
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, title, created_at) VALUES (?, ?, 'manual', ?, ?, '', ?)`, id, hid, lat, lng, at)
}

func TestOutlineDecidesArrival(t *testing.T) {
	fakeNominatim(t)
	a := newTestApp(t)
	code, h := createHoliday(t, a, map[string]any{
		"name": "Paris", "start_at": day(-2),
		"dest_name": "Paris, France", "dest_lat": 48.8589, "dest_lng": 2.3469, "dest_osm": "R7444",
	})
	if code != http.StatusCreated || !h.DestArea {
		t.Fatalf("create: %d, dest_area %v", code, h.DestArea)
	}
	addPin(a, 1, h.ID, 50.95, 1.85, "2026-09-20T08:00:00Z")     // Calais: the drive
	addPin(a, 2, h.ID, 48.84, 2.24, "2026-09-20T13:00:00Z")     // western edge of Paris, 8 km from the centre
	addPin(a, 3, h.ID, 48.8584, 2.2945, "2026-09-21T10:00:00Z") // Eiffel Tower
	if got := listHolidays(t, a)["Paris"].ArrivalPin; got != 2 {
		t.Errorf("arrival pin = %d, want 2 (inside the outline, outside 5 km)", got)
	}
}

func TestCountryOutlineIsNotItsRectangle(t *testing.T) {
	fakeNominatim(t)
	a := newTestApp(t)
	_, h := createHoliday(t, a, map[string]any{
		"name": "France", "start_at": day(-2),
		"dest_name": "France", "dest_lat": 46.6, "dest_lng": 1.9, "dest_osm": "R2202162",
	})
	addPin(a, 1, h.ID, 50.77, -1.73, "2026-09-20T08:00:00Z") // Bransgore: inside France's rectangle, not France
	if got := listHolidays(t, a)["France"].ArrivalPin; got != 0 {
		t.Fatalf("home counted as arriving in France (pin %d)", got)
	}
	addPin(a, 2, h.ID, 55.45, -21.1, "2026-09-21T08:00:00Z") // lat/lng swapped: the sea
	addPin(a, 3, h.ID, -21.1, 55.45, "2026-09-22T08:00:00Z") // Réunion
	if got := listHolidays(t, a)["France"].ArrivalPin; got != 3 {
		t.Errorf("arrival pin = %d, want 3 (the island polygon)", got)
	}
}

func TestDestinationCanBeSetChangedAndRemoved(t *testing.T) {
	calls := fakeNominatim(t)
	a := newTestApp(t)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Bransgore', '#123456', '2026-05-23T00:00:00Z', '2026-05-24T23:59:59Z')`)
	read := func() (name string, lat float64, area string) {
		a.db.QueryRow(`SELECT dest_name, dest_lat, dest_area FROM holidays WHERE id = 1`).Scan(&name, &lat, &area)
		return
	}

	if code := patchHoliday(t, a, `{"dest_name":"Paris, France","dest_lat":48.85,"dest_lng":2.35,"dest_osm":"R7444"}`); code != http.StatusOK {
		t.Fatalf("set destination: got %d", code)
	}
	if n, lat, ar := read(); n != "Paris, France" || lat != 48.85 || !strings.HasPrefix(ar, "[[[[2.22,48.81]") {
		t.Errorf("after set: %q %v %q", n, lat, ar)
	}

	// typed coordinates: a point with no search hit drops the old outline
	if code := patchHoliday(t, a, `{"dest_name":"50.7, -1.7","dest_lat":50.7,"dest_lng":-1.7}`); code != http.StatusOK {
		t.Fatalf("typed coords: got %d", code)
	}
	if _, _, ar := read(); ar != "" {
		t.Errorf("outline survived a point-only destination")
	}

	// a lookup that fails, or finds only a point, still saves the destination
	for _, osm := range []string{"R999", "N1", "not-an-id"} {
		if code := patchHoliday(t, a, `{"dest_name":"Somewhere","dest_lat":1,"dest_lng":1,"dest_osm":"`+osm+`"}`); code != http.StatusOK {
			t.Errorf("%s: got %d", osm, code)
		}
		if n, _, ar := read(); n != "Somewhere" || ar != "" {
			t.Errorf("%s: %q %q", osm, n, ar)
		}
	}

	// a bad point is refused and writes nothing, not even the rename with it
	if code := patchHoliday(t, a, `{"name":"Renamed","dest_name":"X","dest_lat":95,"dest_lng":0}`); code != http.StatusBadRequest {
		t.Errorf("bad point: got %d", code)
	}
	var name string
	a.db.QueryRow(`SELECT name FROM holidays WHERE id = 1`).Scan(&name)
	if name != "Bransgore" {
		t.Errorf("rejected patch renamed the trip to %q", name)
	}

	// a rename without dest_name leaves the destination alone and asks nobody
	before := *calls
	patchHoliday(t, a, `{"name":"Bransgore caravan park"}`)
	if n, _, _ := read(); n != "Somewhere" || *calls != before {
		t.Errorf("rename touched the destination: %q, %d lookups", n, *calls-before)
	}

	patchHoliday(t, a, `{"dest_name":""}`)
	if n, lat, ar := read(); n != "" || lat != 0 || ar != "" {
		t.Errorf("after remove: %q %v %q", n, lat, ar)
	}
}

func TestSimplifyFitsTheCap(t *testing.T) {
	// a 20,000-point wobbly circle
	var ring [][2]float64
	for i := 0; i <= 20000; i++ {
		f := float64(i) / 20000 * 6.283185307
		r := 5 + 0.01*float64(i%7)
		ring = append(ring, [2]float64{r * cos(f), r * sin(f)})
	}
	ring[len(ring)-1] = ring[0]
	s := simplify(area{{ring}}, maxAreaPoints)
	if s.points() > maxAreaPoints || s.points() < 20 {
		t.Fatalf("simplified to %d points", s.points())
	}
	if !s.contains(0, 0) || s.contains(0, 6) {
		t.Error("simplified outline lost its shape")
	}
	j, _ := json.Marshal(s)
	if len(j) > 200_000 {
		t.Errorf("stored outline is %d bytes", len(j))
	}
}

func TestOSMRef(t *testing.T) {
	if osmRef("relation", 7444) != "R7444" || osmRef("way", 5) != "W5" || osmRef("", 1) != "" {
		t.Error("osmRef")
	}
}

func cos(f float64) float64 { return math.Cos(f) }
func sin(f float64) float64 { return math.Sin(f) }

func TestReverseNamesTypedCoordinates(t *testing.T) {
	fakeNominatim(t)
	a := newTestApp(t)
	get := func(q string) (int, string) {
		rec := httptest.NewRecorder()
		a.handleReverse(rec, httptest.NewRequest("GET", "/api/geocode/reverse?"+q, nil))
		var out struct{ Name string }
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Name
	}
	if code, name := get("lat=50.77&lng=-1.73"); code != 200 || name != "Bransgore, New Forest" {
		t.Errorf("got %d %q", code, name)
	}
	if code, name := get("lat=0&lng=0"); code != 200 || name != "" {
		t.Errorf("open sea: got %d %q, want an empty name", code, name)
	}
	if code, _ := get("lat=95&lng=0"); code != http.StatusBadRequest {
		t.Errorf("bad lat: got %d", code)
	}
}
