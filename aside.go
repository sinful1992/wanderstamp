package main

import (
	"database/sql"
	"log"
	"net/http"
	"time"
)

// --- setting photos aside ---
//
// Not every photo taken on a trip belongs on its map. Setting one aside takes
// it off the trip here and keeps it off: sync skips it from then on. Nothing
// happens to the photo in Immich. Putting it back deletes the row and re-syncs,
// so it is filed exactly as it would have been.

// handleSetAside takes one photo off whichever trip holds it.
func (a *app) handleSetAside(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("asset")
	if !assetIDRe.MatchString(assetID) {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	var hid int64
	var takenAt string
	err := a.db.QueryRow(`
		SELECT p.holiday_id, pp.taken_at FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
		WHERE pp.asset_id = ?
		UNION ALL SELECT holiday_id, taken_at FROM unplaced_photos WHERE asset_id = ?
		LIMIT 1`, assetID, assetID).Scan(&hid, &takenAt)
	if err == sql.ErrNoRows {
		httpError(w, http.StatusNotFound, "unknown photo")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer tx.Rollback()
	var pinID int64
	tx.QueryRow(`SELECT pin_id FROM pin_photos WHERE asset_id = ?`, assetID).Scan(&pinID)
	if _, err := tx.Exec(`
		INSERT INTO set_aside (asset_id, holiday_id, taken_at, set_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (asset_id) DO NOTHING`,
		assetID, hid, takenAt, time.Now().UTC().Format(time.RFC3339)); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	for _, table := range []string{"pin_photos", "unplaced_photos", "photo_choices"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE asset_id = ?`, assetID); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
	}
	// A photo stop that just lost its only photo goes now, as sync would
	// remove it, rather than lingering as an empty print until then.
	if _, err := tx.Exec(`
		DELETE FROM pins WHERE id = ? AND kind = 'photo' AND note = ''
		  AND NOT EXISTS (SELECT 1 FROM pin_photos WHERE pin_id = pins.id)`, pinID); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if err := tx.Commit(); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	a.resyncSoon(hid) // re-centres a photo stop that kept its other photos
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "holiday_id": hid})
}

// handlePutBack returns a set-aside photo to its trip, filed by a fresh sync.
func (a *app) handlePutBack(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("asset")
	if !assetIDRe.MatchString(assetID) {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	var hid int64
	if err := a.db.QueryRow(`SELECT holiday_id FROM set_aside WHERE asset_id = ?`, assetID).Scan(&hid); err != nil {
		httpError(w, http.StatusNotFound, "unknown photo")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM set_aside WHERE asset_id = ?`, assetID); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	// Ended trips aren't re-synced on their own, so file it now. If Immich
	// is away the photo is still released, and the next sync picks it up.
	a.resyncSoon(hid)
	if _, _, err := a.syncHoliday(hid); err != nil {
		log.Printf("put back %s: sync holiday %d: %v", assetID, hid, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "holiday_id": hid})
}

func (a *app) handleListSetAside(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(`
		SELECT asset_id, taken_at FROM set_aside WHERE holiday_id = ? ORDER BY taken_at`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	type photo struct {
		AssetID string `json:"asset_id"`
		TakenAt string `json:"taken_at"`
	}
	out := []photo{}
	for rows.Next() {
		var p photo
		if err := rows.Scan(&p.AssetID, &p.TakenAt); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}
