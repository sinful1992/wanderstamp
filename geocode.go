package main

// Place search proxies Nominatim (OpenStreetMap) so the browser only ever
// talks to this server: no API key, no third-party requests from the client,
// and the usage policy's 1 req/s ceiling is enforced here.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	geoClient = &http.Client{Timeout: 8 * time.Second}
	geoMu     sync.Mutex
	geoLast   time.Time
)

func (a *app) handleGeocode(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		httpError(w, http.StatusBadRequest, "q required")
		return
	}

	geoMu.Lock()
	if d := time.Second - time.Since(geoLast); d > 0 {
		time.Sleep(d)
	}
	geoLast = time.Now()
	geoMu.Unlock()

	req, _ := http.NewRequestWithContext(r.Context(), "GET",
		"https://nominatim.openstreetmap.org/search?format=jsonv2&limit=5&q="+url.QueryEscape(q), nil)
	req.Header.Set("User-Agent", "holiday-map/"+version+" (self-hosted travel log)")
	resp, err := geoClient.Do(req)
	if err != nil {
		httpError(w, http.StatusBadGateway, "place search unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		httpError(w, http.StatusBadGateway, "place search unavailable")
		return
	}
	var raw []struct {
		DisplayName string   `json:"display_name"`
		Lat         string   `json:"lat"`
		Lon         string   `json:"lon"`
		BoundingBox []string `json:"boundingbox"` // Nominatim's order: south, north, west, east
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		httpError(w, http.StatusBadGateway, "place search unavailable")
		return
	}
	type hit struct {
		Name string  `json:"name"`
		Lat  float64 `json:"lat"`
		Lng  float64 `json:"lng"`
		BBox bbox    `json:"bbox,omitempty"` // [s,w,n,e]
	}
	out := []hit{}
	for _, h := range raw {
		lat, e1 := strconv.ParseFloat(h.Lat, 64)
		lng, e2 := strconv.ParseFloat(h.Lon, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		out = append(out, hit{Name: h.DisplayName, Lat: lat, Lng: lng, BBox: nominatimBBox(h.BoundingBox)})
	}
	writeJSON(w, http.StatusOK, out)
}

// nominatimBBox reorders Nominatim's [south, north, west, east] strings into
// this app's [south, west, north, east]; nil if any part is missing or off.
func nominatimBBox(raw []string) bbox {
	if len(raw) != 4 {
		return nil
	}
	var f [4]float64
	for i, s := range raw {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		f[i] = v
	}
	b := bbox{f[0], f[2], f[1], f[3]}
	if !b.valid() {
		return nil
	}
	return b
}
