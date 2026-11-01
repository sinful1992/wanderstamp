package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"strings"
)

// F10: GPX tracks drawn on a trip's map — the walk, the drive. They are the
// family's own lines and start at home, so shares never include them.

const (
	maxGPXBytes    = 10 << 20
	maxTrackPoints = 2000
)

type gpxDoc struct {
	Name   string `xml:"metadata>name"`
	Tracks []struct {
		Name     string `xml:"name"`
		Segments []struct {
			Points []gpxPoint `xml:"trkpt"`
		} `xml:"trkseg"`
	} `xml:"trk"`
	Routes []struct {
		Name   string     `xml:"name"`
		Points []gpxPoint `xml:"rtept"`
	} `xml:"rte"`
}

type gpxPoint struct {
	Lat float64 `xml:"lat,attr"`
	Lon float64 `xml:"lon,attr"`
}

var errNoPoints = errors.New("no track points in that file")

// parseGPX returns the file's name and every track and route point in order.
func parseGPX(r io.Reader) (string, [][2]float64, error) {
	var doc gpxDoc
	dec := xml.NewDecoder(r)
	dec.Strict = false
	if err := dec.Decode(&doc); err != nil {
		return "", nil, errors.New("not a GPX file")
	}
	var pts [][2]float64
	add := func(p gpxPoint) {
		if p.Lat >= -90 && p.Lat <= 90 && p.Lon >= -180 && p.Lon <= 180 && !(p.Lat == 0 && p.Lon == 0) {
			pts = append(pts, [2]float64{p.Lat, p.Lon})
		}
	}
	name := doc.Name
	for _, t := range doc.Tracks {
		if name == "" {
			name = t.Name
		}
		for _, s := range t.Segments {
			for _, p := range s.Points {
				add(p)
			}
		}
	}
	for _, rt := range doc.Routes {
		if name == "" {
			name = rt.Name
		}
		for _, p := range rt.Points {
			add(p)
		}
	}
	if len(pts) < 2 {
		return "", nil, errNoPoints
	}
	return strings.TrimSpace(name), pts, nil
}

// simplifyTrack keeps the shape with at most max points: Douglas-Peucker with a
// tolerance raised until it fits, then an even stride as a last resort.
func simplifyTrack(pts [][2]float64, max int) [][2]float64 {
	if len(pts) <= max {
		return pts
	}
	// a very long recording is thinned evenly first: Douglas-Peucker is
	// quadratic on a jagged line, and 4x the limit still keeps every bend
	if len(pts) > 4*max {
		pts = strideTrack(pts, 4*max)
	}
	for tol := 0.00001; tol < 1; tol *= 2 { // degrees: ~1 m upwards
		keep := make([]bool, len(pts))
		keep[0], keep[len(pts)-1] = true, true
		trackDP(pts, 0, len(pts)-1, tol, keep)
		var out [][2]float64
		for i, k := range keep {
			if k {
				out = append(out, pts[i])
			}
		}
		if len(out) <= max {
			return out
		}
	}
	return strideTrack(pts, max)
}

// strideTrack keeps max points evenly spaced, the first and last included.
func strideTrack(pts [][2]float64, max int) [][2]float64 {
	out := make([][2]float64, 0, max)
	step := float64(len(pts)-1) / float64(max-1)
	for i := 0; i < max; i++ {
		out = append(out, pts[int(math.Round(float64(i)*step))])
	}
	return out
}

// trackDP marks the points to keep between a and b (iterative: a long track
// would recurse deep).
func trackDP(pts [][2]float64, a, b int, tol float64, keep []bool) {
	stack := [][2]int{{a, b}}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		i0, i1 := s[0], s[1]
		far, at := 0.0, -1
		for i := i0 + 1; i < i1; i++ {
			if d := trackSegDist(pts[i], pts[i0], pts[i1]); d > far {
				far, at = d, i
			}
		}
		if at >= 0 && far > tol {
			keep[at] = true
			stack = append(stack, [2]int{i0, at}, [2]int{at, i1})
		}
	}
}

func trackSegDist(p, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	if dx == 0 && dy == 0 {
		return math.Hypot(p[0]-a[0], p[1]-a[1])
	}
	t := ((p[0]-a[0])*dx + (p[1]-a[1])*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(p[0]-(a[0]+t*dx), p[1]-(a[1]+t*dy))
}

func (a *app) handleUploadTrack(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var exists bool
	a.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM holidays WHERE id = ?)`, id).Scan(&exists)
	if !exists {
		httpError(w, http.StatusNotFound, "no such holiday")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxGPXBytes+64<<10)
	f, hdr, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpError(w, http.StatusRequestEntityTooLarge, "GPX files can be up to 10 MB")
			return
		}
		httpError(w, http.StatusBadRequest, "send the .gpx file as the form field \"file\"")
		return
	}
	defer f.Close()
	if hdr.Size > maxGPXBytes {
		httpError(w, http.StatusRequestEntityTooLarge, "GPX files can be up to 10 MB")
		return
	}
	name, pts, err := parseGPX(io.LimitReader(f, maxGPXBytes))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(hdr.Filename), filepath.Ext(hdr.Filename))
	}
	name = truncate(name, 120)
	pts = simplifyTrack(pts, maxTrackPoints)
	js, _ := json.Marshal(pts)
	res, err := a.db.Exec(`INSERT INTO tracks (holiday_id, name, points) VALUES (?, ?, ?)`, id, name, string(js))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	tid, _ := res.LastInsertId()
	writeJSON(w, http.StatusCreated, map[string]any{"id": tid, "name": name, "points": len(pts)})
}

type trackOut struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Points json.RawMessage `json:"points"`
}

func (a *app) handleListTracks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := a.reader().Query(`SELECT id, name, points FROM tracks WHERE holiday_id = ? ORDER BY id`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []trackOut{}
	for rows.Next() {
		var t trackOut
		var pts string
		if err := rows.Scan(&t.ID, &t.Name, &pts); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		t.Points = json.RawMessage(pts)
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleDeleteTrack(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := a.db.Exec(`DELETE FROM tracks WHERE id = ?`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "no such track")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
