package main

// A trip's destination: a name, a point for the map stamp, and — when it came
// from place search — the place's real outline, fetched from OpenStreetMap
// when the destination is saved. The outline is what tells a city, a national
// park or a country apart from a single spot: a pin anywhere inside it has
// arrived. It stays on the server; clients get the answer (arrival_pin), not
// the geometry, so a country's border never rides along on every page load.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// arriveKM: a pin this close to the destination's point has arrived even
// outside any outline — the hotel across the road from a castle whose outline
// is its walls. Tight on purpose: Warwick Services on the M40 is 8.5 km from
// the castle, and a services stop is still the drive.
const arriveKM = 5

// maxAreaPoints caps a stored outline. Paris is 55 points and the Lake District
// 451; France with its overseas territories is 13,000 and gets simplified.
const maxAreaPoints = 4000

// area is a GeoJSON MultiPolygon's coordinates: polygons → rings → [lng, lat].
// The first ring of a polygon is its outline, the rest are holes.
type area [][][][2]float64

func (a area) points() int {
	n := 0
	for _, poly := range a {
		for _, ring := range poly {
			n += len(ring)
		}
	}
	return n
}

// contains is the even-odd rule per polygon: inside the outline and not in a hole.
func (a area) contains(lat, lng float64) bool {
	for _, poly := range a {
		in := false
		for _, ring := range poly {
			if ringHas(ring, lat, lng) {
				in = !in
			}
		}
		if in {
			return true
		}
	}
	return false
}

func ringHas(ring [][2]float64, lat, lng float64) bool {
	in := false
	for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
		xi, yi := ring[i][0], ring[i][1]
		xj, yj := ring[j][0], ring[j][1]
		if (yi > lat) != (yj > lat) && lng < (xj-xi)*(lat-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

// parseArea reads a GeoJSON geometry. Only areas count: a place that is a
// point or a line (a node, a road) has no inside, so it gets no outline.
func parseArea(raw json.RawMessage) area {
	var g struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if json.Unmarshal(raw, &g) != nil {
		return nil
	}
	var a area
	switch g.Type {
	case "Polygon":
		var p [][][2]float64
		if json.Unmarshal(g.Coordinates, &p) != nil {
			return nil
		}
		a = area{p}
	case "MultiPolygon":
		if json.Unmarshal(g.Coordinates, &a) != nil {
			return nil
		}
	default:
		return nil
	}
	return clean(a)
}

// clean drops rings too small to enclose anything, and polygons whose
// outline went with them.
func clean(a area) area {
	out := area{}
	for _, poly := range a {
		if len(poly) == 0 || len(poly[0]) < 4 {
			continue
		}
		keep := [][][2]float64{poly[0]}
		for _, hole := range poly[1:] {
			if len(hole) >= 4 {
				keep = append(keep, hole)
			}
		}
		out = append(out, keep)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// simplify thins the outline (Douglas-Peucker, in degrees) with a tolerance
// that doubles until it fits under max points. Islands smaller than the
// tolerance fall away, which is what a trip's arrival check can afford.
func simplify(a area, max int) area {
	for eps := 0.0005; a != nil && a.points() > max; eps *= 2 {
		next := area{}
		for _, poly := range a {
			var p [][][2]float64
			for _, ring := range poly {
				p = append(p, dp(ring, eps))
			}
			next = append(next, p)
		}
		a = clean(next)
	}
	return a
}

func dp(pts [][2]float64, eps float64) [][2]float64 {
	if len(pts) < 3 {
		return pts
	}
	first, last := pts[0], pts[len(pts)-1]
	idx, far := 0, 0.0
	for i := 1; i < len(pts)-1; i++ {
		if d := segDist(pts[i], first, last); d > far {
			idx, far = i, d
		}
	}
	if far <= eps {
		return [][2]float64{first, last}
	}
	left := dp(pts[:idx+1], eps)
	right := dp(pts[idx:], eps)
	return append(left[:len(left)-1], right...)
}

func segDist(p, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	if dx == 0 && dy == 0 {
		return math.Hypot(p[0]-a[0], p[1]-a[1])
	}
	t := math.Max(0, math.Min(1, ((p[0]-a[0])*dx+(p[1]-a[1])*dy)/(dx*dx+dy*dy)))
	return math.Hypot(p[0]-(a[0]+t*dx), p[1]-(a[1]+t*dy))
}

func storeArea(a area) string {
	if a == nil {
		return ""
	}
	j, _ := json.Marshal(a)
	return string(j)
}

func loadArea(s string) area {
	if s == "" {
		return nil
	}
	var a area
	if json.Unmarshal([]byte(s), &a) != nil {
		return nil
	}
	return a
}

// nominatimBase is swapped for a local server in tests.
var nominatimBase = "https://nominatim.openstreetmap.org"

var osmRe = regexp.MustCompile(`^[NWR][0-9]{1,15}$`)

// nominatimGet waits its turn (the usage policy's 1 req/s) and fetches.
func nominatimGet(ctx context.Context, path string) (*http.Response, error) {
	geoMu.Lock()
	if d := time.Second - time.Since(geoLast); d > 0 {
		time.Sleep(d)
	}
	geoLast = time.Now()
	geoMu.Unlock()
	req, _ := http.NewRequestWithContext(ctx, "GET", nominatimBase+path, nil)
	req.Header.Set("User-Agent", "holiday-map/"+version+" (self-hosted travel log)")
	return geoClient.Do(req)
}

// fetchArea looks up one OSM object ("R7444" = relation 7444, Paris) and
// returns its outline, simplified to fit. nil with no error = the place is a
// point or a line.
func fetchArea(ctx context.Context, osm string) (area, error) {
	if !osmRe.MatchString(osm) {
		return nil, errors.New("bad osm id")
	}
	resp, err := nominatimGet(ctx, "/lookup?format=jsonv2&polygon_geojson=1&polygon_threshold=0.0005&osm_ids="+osm)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lookup: HTTP %d", resp.StatusCode)
	}
	var out []struct {
		GeoJSON json.RawMessage `json:"geojson"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("lookup: not found")
	}
	return simplify(parseArea(out[0].GeoJSON), maxAreaPoints), nil
}

// cleanDest validates a destination from a request. An empty name means no
// destination, and the point goes with it.
func cleanDest(name string, lat, lng float64) (string, float64, float64, string) {
	name = truncate(strings.TrimSpace(name), 120)
	if name == "" {
		return "", 0, 0, ""
	}
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return "", 0, 0, "invalid destination coordinates"
	}
	return name, lat, lng, ""
}

// arrivalPin is the first pin, in visit order, inside the destination's
// outline or within arriveKM of its point; 0 when none has arrived.
func arrivalPin(destLat, destLng float64, a area, pins []pinOut) int64 {
	ps := append([]pinOut(nil), pins...)
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].VisitedAt != ps[j].VisitedAt {
			return ps[i].VisitedAt < ps[j].VisitedAt
		}
		return ps[i].ID < ps[j].ID
	})
	for _, p := range ps {
		if a.contains(p.Lat, p.Lng) || haversineKM(p.Lat, p.Lng, destLat, destLng) <= arriveKM {
			return p.ID
		}
	}
	return 0
}

func haversineKM(lat1, lng1, lat2, lng2 float64) float64 {
	r := math.Pi / 180
	x := math.Pow(math.Sin((lat2-lat1)*r/2), 2) +
		math.Cos(lat1*r)*math.Cos(lat2*r)*math.Pow(math.Sin((lng2-lng1)*r/2), 2)
	return 12742 * math.Asin(math.Sqrt(x))
}

// resolveArea fetches the outline for a destination being saved. A failed
// lookup doesn't fail the save: the trip keeps its point and the 5 km rule.
func resolveArea(ctx context.Context, name, osm string) string {
	if name == "" || osm == "" {
		return ""
	}
	a, err := fetchArea(ctx, osm)
	if err != nil {
		log.Printf("destination outline for %q (%s): %v — using the 5 km rule", name, osm, err)
		return ""
	}
	return storeArea(a)
}
