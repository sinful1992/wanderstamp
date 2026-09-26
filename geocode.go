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

	resp, err := nominatimGet(r.Context(), "/search?format=jsonv2&limit=5&q="+url.QueryEscape(q))
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
		DisplayName string `json:"display_name"`
		Lat         string `json:"lat"`
		Lon         string `json:"lon"`
		OSMType     string `json:"osm_type"`
		OSMID       int64  `json:"osm_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		httpError(w, http.StatusBadGateway, "place search unavailable")
		return
	}
	type hit struct {
		Name string  `json:"name"`
		Lat  float64 `json:"lat"`
		Lng  float64 `json:"lng"`
		OSM  string  `json:"osm,omitempty"` // "R7444": saved with the destination, the server fetches its outline
	}
	out := []hit{}
	for _, h := range raw {
		lat, e1 := strconv.ParseFloat(h.Lat, 64)
		lng, e2 := strconv.ParseFloat(h.Lon, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		out = append(out, hit{Name: h.DisplayName, Lat: lat, Lng: lng, OSM: osmRef(h.OSMType, h.OSMID)})
	}
	writeJSON(w, http.StatusOK, out)
}

// osmRef is the lookup key: "relation" 7444 -> "R7444".
func osmRef(kind string, id int64) string {
	if kind == "" || id <= 0 {
		return ""
	}
	return strings.ToUpper(kind[:1]) + strconv.FormatInt(id, 10)
}

// handleReverse names a typed "lat, lng": the town or village it falls in,
// at the same two-part length as a search pick ("Bransgore, New Forest").
// zoom=14 asks for the village or town (10 answers with the district —
// "New Forest" for Bransgore), not the nearest house or road.
func (a *app) handleReverse(w http.ResponseWriter, r *http.Request) {
	lat, e1 := strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
	lng, e2 := strconv.ParseFloat(r.URL.Query().Get("lng"), 64)
	if e1 != nil || e2 != nil || lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		httpError(w, http.StatusBadRequest, "lat and lng required")
		return
	}
	resp, err := nominatimGet(r.Context(), "/reverse?format=jsonv2&zoom=14&lat="+
		strconv.FormatFloat(lat, 'f', 6, 64)+"&lon="+strconv.FormatFloat(lng, 'f', 6, 64))
	if err != nil {
		httpError(w, http.StatusBadGateway, "place lookup unavailable")
		return
	}
	defer resp.Body.Close()
	var out struct {
		DisplayName string `json:"display_name"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		httpError(w, http.StatusBadGateway, "place lookup unavailable")
		return
	}
	// the open sea has no name: an empty answer, and the caller keeps the numbers
	name := out.DisplayName
	if parts := strings.Split(name, ","); len(parts) > 2 {
		name = strings.Join(parts[:2], ",")
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": strings.TrimSpace(name)})
}
