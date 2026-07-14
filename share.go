package main

// View-only share links: a family member creates a capability URL for one
// trip, and anyone holding it can read that trip's story — pins, notes and
// photos — with no account. Tokens are 128-bit random and stored hashed
// (like sessions), so creating a new link always replaces the old one:
// the raw token only exists in the URL that was handed out.

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

func (a *app) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var exists int
	if err := a.db.QueryRow(`SELECT 1 FROM holidays WHERE id = ?`, id).Scan(&exists); err != nil {
		httpError(w, http.StatusNotFound, "no such holiday")
		return
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		httpError(w, http.StatusInternalServerError, "entropy error")
		return
	}
	token := hex.EncodeToString(buf)
	if _, err := a.db.Exec(`
		INSERT INTO shares (token, holiday_id) VALUES (?, ?)
		ON CONFLICT (holiday_id) DO UPDATE SET
		  token = excluded.token,
		  created_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`,
		hashToken(token), id); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": "/share/" + token})
}

func (a *app) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.db.Exec(`DELETE FROM shares WHERE holiday_id = ?`, id); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// shareHoliday resolves a raw token from the URL to the holiday it unlocks.
func (a *app) shareHoliday(w http.ResponseWriter, r *http.Request) (int64, bool) {
	var hid int64
	err := a.db.QueryRow(`SELECT holiday_id FROM shares WHERE token = ?`,
		hashToken(r.PathValue("token"))).Scan(&hid)
	if err != nil {
		httpError(w, http.StatusNotFound, "this share link is no longer active")
		return 0, false
	}
	return hid, true
}

// handleShareData returns the shared trip and its pins in the same shapes
// the logged-in API uses, so the frontend renders it with the same code.
func (a *app) handleShareData(w http.ResponseWriter, r *http.Request) {
	hid, ok := a.shareHoliday(w, r)
	if !ok {
		return
	}
	var h holidayOut
	err := a.db.QueryRow(`
		SELECT h.id, h.name, h.color, h.start_at, h.end_at, h.journal,
		       COALESCE((SELECT pp.asset_id FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
		                 WHERE p.holiday_id = h.id AND pp.asset_id = h.cover_asset),
		                (SELECT pp.asset_id FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
		                 WHERE p.holiday_id = h.id ORDER BY pp.taken_at LIMIT 1), ''),
		       (SELECT COUNT(*) FROM pins p WHERE p.holiday_id = h.id),
		       (SELECT COUNT(*) FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id WHERE p.holiday_id = h.id)
		FROM holidays h WHERE h.id = ?`, hid).
		Scan(&h.ID, &h.Name, &h.Color, &h.StartAt, &h.EndAt, &h.Journal, &h.CoverAsset, &h.PinCount, &h.PhotoCount)
	if err != nil {
		httpError(w, http.StatusNotFound, "this share link is no longer active")
		return
	}
	h.Active = h.EndAt == nil
	pins, err := a.queryPins(hid)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"holiday": h, "pins": pins})
}

func (a *app) handleSharePinPhotos(w http.ResponseWriter, r *http.Request) {
	hid, ok := a.shareHoliday(w, r)
	if !ok {
		return
	}
	pinID, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(`
		SELECT pp.asset_id, pp.taken_at FROM pin_photos pp
		JOIN pins p ON p.id = pp.pin_id
		WHERE pp.pin_id = ? AND p.holiday_id = ?
		ORDER BY pp.taken_at`, pinID, hid)
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

// handleSharePhoto streams a photo to an anonymous link-holder — but only
// assets pinned to the shared trip; the link is never a proxy into anything
// else in Immich.
func (a *app) handleSharePhoto(w http.ResponseWriter, r *http.Request) {
	hid, ok := a.shareHoliday(w, r)
	if !ok {
		return
	}
	assetID := r.PathValue("asset")
	if !assetIDRe.MatchString(assetID) {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	var exists int
	if err := a.db.QueryRow(`
		SELECT 1 FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
		WHERE pp.asset_id = ? AND p.holiday_id = ?`, assetID, hid).Scan(&exists); err != nil {
		httpError(w, http.StatusNotFound, "unknown photo")
		return
	}
	a.streamPhoto(w, r, assetID)
}
