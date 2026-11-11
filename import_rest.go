package main

// Task 142 (audit 137 WS-1): import of the parts /api/export now carries
// (export_rest.go). Every field is optional, so an export from before 1.20.0
// still imports as it did.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// checkImportRest is part of checkImportHoliday: a bad file changes nothing.
func checkImportRest(h importHoliday) string {
	for i, t := range h.Tracks {
		var pts [][]float64
		if err := json.Unmarshal(t.Points, &pts); err != nil || len(pts) == 0 {
			return fmt.Sprintf("track %d: points must be a list of [lat,lng]", i+1)
		}
		if len(pts) > maxTrackPoints {
			return fmt.Sprintf("track %d: more than %d points", i+1, maxTrackPoints)
		}
		for _, p := range pts {
			if len(p) != 2 || !validLatLng(p[0], p[1]) {
				return fmt.Sprintf("track %d: bad point", i+1)
			}
		}
	}
	rfc := func(s string) bool { _, err := time.Parse(time.RFC3339, s); return err == nil }
	for i, x := range h.SetAside {
		if !assetIDRe.MatchString(x.AssetID) || !rfc(x.TakenAt) || !rfc(x.SetAt) {
			return fmt.Sprintf("set-aside photo %d: bad asset id or time", i+1)
		}
	}
	for i, x := range h.Unplaced {
		if !assetIDRe.MatchString(x.AssetID) || !rfc(x.TakenAt) {
			return fmt.Sprintf("unplaced photo %d: bad asset id or time", i+1)
		}
		if x.Type != "" && x.Type != "image" && x.Type != "video" {
			return fmt.Sprintf("unplaced photo %d: type must be image or video", i+1)
		}
	}
	for i, x := range h.PhotoChoices {
		if !assetIDRe.MatchString(x.AssetID) || x.PinID < 0 {
			return fmt.Sprintf("same-place answer %d: bad asset id or pin id", i+1)
		}
	}
	return ""
}

// filedAnywhere: each photo lives in at most one of pin_photos,
// unplaced_photos and set_aside on this server. One already filed stays put.
func filedAnywhere(tx *sql.Tx, asset string) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM pin_photos WHERE asset_id = ?)
		+ (SELECT COUNT(*) FROM unplaced_photos WHERE asset_id = ?)
		+ (SELECT COUNT(*) FROM set_aside WHERE asset_id = ?)`, asset, asset, asset).Scan(&n)
	return n > 0, err
}

func importAsideTx(tx *sql.Tx, hid int64, h importHoliday, res *importResult) error {
	for _, x := range h.SetAside {
		if filed, err := filedAnywhere(tx, x.AssetID); err != nil {
			return err
		} else if filed {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO set_aside (asset_id, holiday_id, taken_at, set_at) VALUES (?, ?, ?, ?)`,
			x.AssetID, hid, x.TakenAt, x.SetAt); err != nil {
			return err
		}
		res.SetAside++
	}
	for _, x := range h.Unplaced {
		if filed, err := filedAnywhere(tx, x.AssetID); err != nil {
			return err
		} else if filed {
			continue
		}
		typ, dur := x.Type, x.Duration
		if typ == "" {
			typ = "image"
		}
		if typ != "video" || dur < 0 || dur != dur || dur > 1e9 {
			dur = 0
		}
		if _, err := tx.Exec(`INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at, type, duration) VALUES (?, ?, ?, ?, ?)`,
			hid, x.AssetID, x.TakenAt, typ, dur); err != nil {
			return err
		}
		res.Unplaced++
	}
	return nil
}

// importChoicesTracksTx: a same-place answer points at a pin by its old id,
// mapped to the new one; 0 (its own stop) stays 0. An answer whose pin is
// not in this trip any more is dropped (the sync asks again), and a photo
// that already has an answer here keeps it.
func importChoicesTracksTx(tx *sql.Tx, hid int64, h importHoliday, pinIDs map[int64]int64, res *importResult) error {
	for _, c := range h.PhotoChoices {
		pid := int64(0)
		if c.PinID != 0 {
			var ok bool
			if pid, ok = pinIDs[c.PinID]; !ok {
				continue
			}
		}
		r, err := tx.Exec(`INSERT INTO photo_choices (asset_id, holiday_id, pin_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			c.AssetID, hid, pid)
		if err != nil {
			return err
		}
		if k, _ := r.RowsAffected(); k > 0 {
			res.Choices++
		}
	}
	for _, t := range h.Tracks {
		var pts [][]float64
		json.Unmarshal(t.Points, &pts) // checked in checkImportRest
		js, _ := json.Marshal(pts)
		if _, err := tx.Exec(`INSERT INTO tracks (holiday_id, name, points) VALUES (?, ?, ?)`,
			hid, truncate(strings.TrimSpace(t.Name), 120), string(js)); err != nil {
			return err
		}
		res.Tracks++
	}
	return nil
}

// importTemplatesTx: a master packing list with a name already here (case
// does not matter) gets only the items it lacks.
func importTemplatesTx(tx *sql.Tx, list []templateExport, res *importResult) error {
	for _, t := range list {
		name := truncate(strings.TrimSpace(t.Name), maxLabelLen)
		var id int64
		err := tx.QueryRow(`SELECT id FROM packing_templates WHERE name = ?`, name).Scan(&id)
		added := false
		if err == sql.ErrNoRows {
			r, err := tx.Exec(`INSERT INTO packing_templates (name) VALUES (?)`, name)
			if err != nil {
				return err
			}
			id, _ = r.LastInsertId()
			added = true
		} else if err != nil {
			return err
		}
		var sort int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(sort) + 1, 0) FROM packing_template_items WHERE template_id = ?`, id).Scan(&sort); err != nil {
			return err
		}
		for _, l := range t.Items {
			label := truncate(strings.TrimSpace(l), maxLabelLen)
			if label == "" {
				continue
			}
			r, err := tx.Exec(`INSERT INTO packing_template_items (template_id, label, sort) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, id, label, sort)
			if err != nil {
				return err
			}
			if k, _ := r.RowsAffected(); k > 0 {
				sort++
				added = true
			}
		}
		if added {
			res.Templates++
		}
	}
	return nil
}
