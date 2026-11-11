package main

// F9 import: load an /api/export document back in — moving to a new server,
// or bringing back a trip deleted long ago from an old export.

import (
	"bufio"
	"bytes"
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
	Ask      bool    `json:"ask"` // task 142; absent before 1.20.0
}

type importPin struct {
	ID         int64         `json:"id"` // the pin's id on the old server: photo_choices point at it
	Kind       string        `json:"kind"`
	Lat        float64       `json:"lat"`
	Lng        float64       `json:"lng"`
	Title      string        `json:"title"`
	Note       string        `json:"note"`
	Country    string        `json:"country"`
	CoverAsset string        `json:"cover_asset"`
	CreatedAt  string        `json:"created_at"`
	Photos     []importPhoto `json:"photos"`
	VisitNotes []dayNote     `json:"visit_notes"` // task 136; absent in older exports
	CreatedBy  string        `json:"created_by"`  // task 142; absent before 1.20.0
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
	// task 142 (audit 137 WS-1); all absent in exports before 1.20.0
	CreatedBy     string           `json:"created_by"`
	JournalBy     string           `json:"journal_by"`
	ImmichAlbumID string           `json:"immich_album_id"`
	Tracks        []trackExport    `json:"tracks"`
	SetAside      []asideExport    `json:"set_aside"`
	Unplaced      []unplacedExport `json:"unplaced"`
	PhotoChoices  []choiceExport   `json:"photo_choices"`
}

type importResult struct {
	Holidays int `json:"holidays"`
	Pins     int `json:"pins"`
	Photos   int `json:"photos"`
	Packing  int `json:"packing"`
	Skipped  int `json:"skipped"`
	// task 142 (WS-1); left out when 0, so an old export gets the old answer
	Tracks    int `json:"tracks,omitempty"`
	SetAside  int `json:"set_aside,omitempty"`
	Unplaced  int `json:"unplaced,omitempty"`
	Choices   int `json:"photo_choices,omitempty"`
	Templates int `json:"packing_templates,omitempty"`
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
		Holidays  []importHoliday  `json:"holidays"`
		Templates []templateExport `json:"packing_templates"` // task 142; absent before 1.20.0
	}
	// a file saved by a Windows editor can start with a UTF-8 BOM
	body := bufio.NewReader(r.Body)
	if b, _ := body.Peek(3); bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
		body.Discard(3)
	}
	dec := json.NewDecoder(body)
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
	for i, t := range doc.Templates {
		if strings.TrimSpace(t.Name) == "" {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("packing list %d: no name", i+1))
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
	// master packing lists belong to no trip: they come in even when every
	// trip was skipped, and a list already here gets only its missing items
	if err := importTemplatesTx(tx, doc.Templates, &res); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
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
	if h.ImmichAlbumID != "" && !assetIDRe.MatchString(h.ImmichAlbumID) {
		return "bad immich_album_id"
	}
	if msg := checkImportRest(h); msg != "" {
		return msg
	}
	ids := map[int64]bool{}
	for j, p := range h.Pins {
		if p.ID != 0 {
			if ids[p.ID] {
				return fmt.Sprintf("pin %d: id %d twice in one trip", j+1, p.ID)
			}
			ids[p.ID] = true
		}
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
		dest_name, dest_lat, dest_lng, dest_area, planned, created_by, journal_by, immich_album_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, h.Color, h.StartAt, h.EndAt, h.Journal, h.CoverAsset, destName, destLat, destLng, area, planned,
		truncate(h.CreatedBy, 64), truncate(h.JournalBy, 64), h.ImmichAlbumID)
	if err != nil {
		return err
	}
	hid, _ := r.LastInsertId()
	res.Holidays++
	// set-aside and unplaced photos first, so a photo set aside by hand is
	// never filed on a pin by this same import
	if err := importAsideTx(tx, hid, h, res); err != nil {
		return err
	}
	pinIDs := map[int64]int64{} // old pin id -> new

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
		r, err := tx.Exec(`INSERT INTO pins (holiday_id, kind, cluster_key, lat, lng, title, note, country, cover_asset, created_at, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			hid, p.Kind, key, p.Lat, p.Lng, p.Title, p.Note, p.Country, p.CoverAsset, created, truncate(p.CreatedBy, 64))
		if err != nil {
			return err
		}
		pid, _ := r.LastInsertId()
		if p.ID != 0 {
			pinIDs[p.ID] = pid
		}
		res.Pins++
		for _, vn := range p.VisitNotes {
			if !validDay(vn.Day) || strings.TrimSpace(vn.Note) == "" {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO visit_notes (pin_id, day, note) VALUES (?, ?, ?)
				ON CONFLICT (pin_id, day) DO UPDATE SET note = excluded.note`, pid, vn.Day, vn.Note); err != nil {
				return err
			}
		}
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
			if filed, err := filedAnywhere(tx, ph.AssetID); err != nil {
				return err
			} else if filed {
				continue
			}
			ask := ph.Ask && p.Kind == "manual" // only a hand-placed pin asks "same place?"
			r, err := tx.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng, type, duration, ask)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				pid, ph.AssetID, ph.TakenAt, ph.Lat, ph.Lng, typ, dur, ask)
			if err != nil {
				return err
			}
			if k, _ := r.RowsAffected(); k > 0 {
				res.Photos++
			}
		}
	}
	if err := importChoicesTracksTx(tx, hid, h, pinIDs, res); err != nil {
		return err
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
