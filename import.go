package main

// F9 import: load an /api/export document back in — moving to a new server,
// or bringing back a trip deleted long ago from an old export.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// maxImportBytes caps the request body (an export of years of trips is ~1 MB).
const maxImportBytes = 20 << 20

type importPhoto struct {
	AssetID  string  `json:"asset_id"`
	TakenAt  string  `json:"taken_at"`
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
	Type     string  `json:"type"`
	Duration float64 `json:"duration"`
}

type importPin struct {
	Kind       string        `json:"kind"`
	Lat        float64       `json:"lat"`
	Lng        float64       `json:"lng"`
	Title      string        `json:"title"`
	Note       string        `json:"note"`
	Country    string        `json:"country"`
	CoverAsset string        `json:"cover_asset"`
	CreatedAt  string        `json:"created_at"`
	Photos     []importPhoto `json:"photos"`
}

type importHoliday struct {
	Name       string          `json:"name"`
	Color      string          `json:"color"`
	StartAt    string          `json:"start_at"`
	EndAt      *string         `json:"end_at"`
	Journal    string          `json:"journal"`
	CoverAsset string          `json:"cover_asset"`
	DestName   string          `json:"dest_name"`
	DestLat    float64         `json:"dest_lat"`
	DestLng    float64         `json:"dest_lng"`
	DestArea   json.RawMessage `json:"dest_area"`
	Pins       []importPin     `json:"pins"`
	Packing    []struct {
		Label   string `json:"label"`
		Checked bool   `json:"checked"`
	} `json:"packing"`
}

type importResult struct {
	Holidays int `json:"holidays"`
	Pins     int `json:"pins"`
	Photos   int `json:"photos"`
	Packing  int `json:"packing"`
	Skipped  int `json:"skipped"`
}

// handleImport: POST /api/import (admin), body = an /api/export JSON.
// A trip already here (same name + start_at) is skipped whole, as is a live
// trip while another is live. Ids are new; photos keep their asset ids, and
// one already filed on another trip stays where it is. ?dry_run=1 does the
// whole import inside a transaction and rolls it back, so the counts are exact.
func (a *app) handleImport(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		httpError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	var doc struct {
		Holidays []importHoliday `json:"holidays"`
	}
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&doc)
	if err == nil {
		// one JSON document and nothing after it (reading on also enforces
		// the size cap on trailing padding)
		if err = dec.Decode(&struct{}{}); err == io.EOF {
			err = nil
		} else if err == nil {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpError(w, http.StatusRequestEntityTooLarge, "an import can be at most 20 MB")
			return
		}
		httpError(w, http.StatusBadRequest, "not an export file")
		return
	}
	if doc.Holidays == nil {
		httpError(w, http.StatusBadRequest, "not an export file: no holidays")
		return
	}
	for i, h := range doc.Holidays {
		if msg := checkImportHoliday(h); msg != "" {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("trip %d: %s", i+1, msg))
			return
		}
	}

	tx, err := a.db.Begin()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer tx.Rollback()
	var res importResult
	for _, h := range doc.Holidays {
		if err := importHolidayTx(tx, h, &res); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
	}
	if r.URL.Query().Get("dry_run") == "1" {
		writeJSON(w, http.StatusOK, res) // the deferred Rollback undoes it all
		return
	}
	if err := tx.Commit(); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func validLatLng(lat, lng float64) bool {
	return !math.IsNaN(lat) && !math.IsNaN(lng) && lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180
}

// checkImportHoliday rejects what the app itself would never have written,
// before anything is inserted, so a bad file changes nothing.
func checkImportHoliday(h importHoliday) string {
	if strings.TrimSpace(h.Name) == "" {
		return "no name"
	}
	if !colorRe.MatchString(h.Color) {
		return "color must be #RRGGBB"
	}
	start, err := time.Parse(time.RFC3339, h.StartAt)
	if err != nil {
		return "start_at is not RFC3339"
	}
	if h.EndAt != nil {
		end, err := time.Parse(time.RFC3339, *h.EndAt)
		if err != nil {
			return "end_at is not RFC3339"
		}
		if end.Before(start) {
			return "end_at is before start_at"
		}
	}
	if !validLatLng(h.DestLat, h.DestLng) {
		return "bad destination position"
	}
	if len(h.DestArea) > 0 && string(h.DestArea) != "null" {
		if c := h.DestArea[0]; c != '{' && c != '[' {
			return "bad dest_area"
		}
	}
	for j, p := range h.Pins {
		if p.Kind != "photo" && p.Kind != "manual" {
			return fmt.Sprintf("pin %d: kind must be photo or manual", j+1)
		}
		if !validLatLng(p.Lat, p.Lng) {
			return fmt.Sprintf("pin %d: bad position", j+1)
		}
		for _, ph := range p.Photos {
			if !assetIDRe.MatchString(ph.AssetID) {
				return fmt.Sprintf("pin %d: bad asset id", j+1)
			}
			if _, err := time.Parse(time.RFC3339, ph.TakenAt); err != nil {
				return fmt.Sprintf("pin %d: photo taken_at is not RFC3339", j+1)
			}
			if !validLatLng(ph.Lat, ph.Lng) {
				return fmt.Sprintf("pin %d: bad photo position", j+1)
			}
			if ph.Type != "" && ph.Type != "image" && ph.Type != "video" {
				return fmt.Sprintf("pin %d: photo type must be image or video", j+1)
			}
		}
	}
	return ""
}

func importHolidayTx(tx *sql.Tx, h importHoliday, res *importResult) error {
	name := truncate(strings.TrimSpace(h.Name), 120)
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM holidays WHERE name = ? AND start_at = ?`, name, h.StartAt).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		res.Skipped++
		return nil
	}
	// an open trip whose first day is ahead is a planned one, as openDB decides
	planned := h.EndAt == nil && h.StartAt > time.Now().UTC().Format(time.RFC3339)
	if h.EndAt == nil && !planned {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM holidays WHERE end_at IS NULL AND planned = 0`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			res.Skipped++ // only one trip can be live
			return nil
		}
	}
	area := ""
	if len(h.DestArea) > 0 && string(h.DestArea) != "null" {
		area = string(h.DestArea)
	}
	destName, destLat, destLng, _ := cleanDest(h.DestName, h.DestLat, h.DestLng)
	r, err := tx.Exec(`INSERT INTO holidays (name, color, start_at, end_at, journal, cover_asset,
		dest_name, dest_lat, dest_lng, dest_area, planned) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, h.Color, h.StartAt, h.EndAt, h.Journal, h.CoverAsset, destName, destLat, destLng, area, planned)
	if err != nil {
		return err
	}
	hid, _ := r.LastInsertId()
	res.Holidays++

	for _, p := range h.Pins {
		// photo pins get their cluster key back, so the next sync reuses them
		// instead of growing a second pin beside each one
		var key any
		if p.Kind == "photo" {
			k := clusterKey(p.Lat, p.Lng)
			tx.QueryRow(`SELECT COUNT(*) FROM pins WHERE holiday_id = ? AND cluster_key = ?`, hid, k).Scan(&n)
			if n == 0 {
				key = k
			}
		}
		created := p.CreatedAt
		if _, err := time.Parse(time.RFC3339, created); err != nil {
			created = time.Now().UTC().Format(time.RFC3339)
		}
		r, err := tx.Exec(`INSERT INTO pins (holiday_id, kind, cluster_key, lat, lng, title, note, country, cover_asset, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			hid, p.Kind, key, p.Lat, p.Lng, p.Title, p.Note, p.Country, p.CoverAsset, created)
		if err != nil {
			return err
		}
		pid, _ := r.LastInsertId()
		res.Pins++
		for _, ph := range p.Photos {
			typ := ph.Type
			if typ == "" {
				typ = "image" // exports before 1.14.0 had only photos
			}
			dur := ph.Duration
			if typ != "video" || dur < 0 || math.IsNaN(dur) || math.IsInf(dur, 0) {
				dur = 0
			}
			// filed already (on a pin, unplaced or set aside): it stays there
			tx.QueryRow(`SELECT (SELECT COUNT(*) FROM unplaced_photos WHERE asset_id = ?)
				+ (SELECT COUNT(*) FROM set_aside WHERE asset_id = ?)`, ph.AssetID, ph.AssetID).Scan(&n)
			if n > 0 {
				continue
			}
			r, err := tx.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng, type, duration)
				VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				pid, ph.AssetID, ph.TakenAt, ph.Lat, ph.Lng, typ, dur)
			if err != nil {
				return err
			}
			if k, _ := r.RowsAffected(); k > 0 {
				res.Photos++
			}
		}
	}
	for i, pk := range h.Packing {
		label := truncate(strings.TrimSpace(pk.Label), maxLabelLen)
		if label == "" {
			continue
		}
		r, err := tx.Exec(`INSERT INTO packing_items (holiday_id, label, checked, sort) VALUES (?, ?, ?, ?)
			ON CONFLICT DO NOTHING`, hid, label, pk.Checked, i)
		if err != nil {
			return err
		}
		if k, _ := r.RowsAffected(); k > 0 {
			res.Packing++
		}
	}
	return nil
}
