package main

// A trip's destination: a name, a point for the map stamp, and — when it came
// from place search — the place's bounding box. The box is what tells a city
// or a region apart from a single spot: a pin anywhere inside it has arrived,
// where a point alone could only say "within a few km of the middle".

import (
	"encoding/json"
	"strings"
)

// bbox is [south, west, north, east]. West greater than east means the box
// crosses the antimeridian.
type bbox []float64

func (b bbox) valid() bool {
	if len(b) != 4 {
		return false
	}
	s, w, n, e := b[0], b[1], b[2], b[3]
	return s >= -90 && n <= 90 && s <= n && w >= -180 && w <= 180 && e >= -180 && e <= 180
}

// storeBBox is the column value: empty for none, else the JSON array.
func storeBBox(b bbox) string {
	if len(b) == 0 {
		return ""
	}
	j, _ := json.Marshal([]float64(b))
	return string(j)
}

// loadBBox reads the column back; anything unreadable is simply no box.
func loadBBox(s string) bbox {
	if s == "" {
		return nil
	}
	var b bbox
	if json.Unmarshal([]byte(s), &b) != nil || !b.valid() {
		return nil
	}
	return b
}

// cleanDest validates a destination from a request. An empty name means no
// destination: coordinates and box are dropped with it. A box is optional
// (typed coordinates have none) but must be well-formed if sent.
func cleanDest(name string, lat, lng float64, box bbox) (string, float64, float64, string, string) {
	name = truncate(strings.TrimSpace(name), 120)
	if name == "" {
		return "", 0, 0, "", ""
	}
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return "", 0, 0, "", "invalid destination coordinates"
	}
	if len(box) > 0 && !box.valid() {
		return "", 0, 0, "", "invalid destination area"
	}
	return name, lat, lng, storeBBox(box), ""
}
