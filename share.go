package main

// View-only share links: a family member creates a capability URL for one
// trip, and anyone holding it can read that trip's story — pins, notes and
// photos — with no account. Tokens are 128-bit random and stored hashed
// (like sessions), so creating a new link always replaces the old one:
// the raw token only exists in the URL that was handed out.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"time"
)

// shareDays are the lifetimes a link can be made with; 0 = never expires.
var shareDays = map[int]bool{0: true, 7: true, 30: true, 90: true, 365: true}

func (a *app) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// the body is optional: an older page posts none and gets a link that never expires
	var req struct {
		Days *int `json:"days"`
	}
	// an empty body, even a chunked one with no Content-Type, means "never"
	if r.ContentLength != 0 && (r.ContentLength > 0 || r.Header.Get("Content-Type") != "") {
		if !readJSON(w, r, &req) {
			return
		}
	}
	days := 0
	if req.Days != nil {
		days = *req.Days
	}
	if !shareDays[days] {
		httpError(w, http.StatusBadRequest, "days must be 0, 7, 30, 90 or 365")
		return
	}
	var expires *string
	if days > 0 {
		e := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		expires = &e
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
		INSERT INTO shares (token, holiday_id, expires_at) VALUES (?, ?, ?)
		ON CONFLICT (holiday_id) DO UPDATE SET
		  token = excluded.token,
		  expires_at = excluded.expires_at,
		  created_at = strftime('%Y-%m-%dT%H:%M:%SZ','now')`,
		hashToken(token), id, expires); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": "/share/" + token, "expires_at": expires})
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
// An unknown token is 404; a link past its expiry is 410, so the page can
// say "expired" rather than "never existed".
func (a *app) shareHoliday(w http.ResponseWriter, r *http.Request) (int64, bool) {
	var hid int64
	var expires sql.NullString
	err := a.db.QueryRow(`SELECT holiday_id, expires_at FROM shares WHERE token = ?`,
		hashToken(r.PathValue("token"))).Scan(&hid, &expires)
	if err != nil {
		httpError(w, http.StatusNotFound, "this share link is no longer active")
		return 0, false
	}
	if shareExpired(expires) {
		httpError(w, http.StatusGone, "this share link has expired")
		return 0, false
	}
	return hid, true
}

// shareExpired is true once a link's expiry has passed. NULL never expires;
// an unreadable value counts as expired, since a link must not outlive a
// date nobody can read.
func shareExpired(expires sql.NullString) bool {
	if !expires.Valid {
		return false
	}
	t, err := time.Parse(time.RFC3339, expires.String)
	return err != nil || !time.Now().Before(t)
}

// sharedTrip is a shared trip as a link-holder may see it: the photo pins on
// the way there are left out (they start at the family's front door), and
// the counts and cover are worked out from what is left.
type sharedTrip struct {
	holiday holidayOut
	pins    []pinOut
	assets  map[string]int64 // photo asset -> pin, only for pins still shown
}

var errNoTrip = errors.New("no such trip")

func (a *app) loadSharedTrip(hid int64) (*sharedTrip, error) {
	var h holidayOut
	var box string
	err := a.reader().QueryRow(`
		SELECT h.id, h.name, h.color, h.start_at, h.end_at, h.journal, h.cover_asset,
		       h.planned, h.dest_name, h.dest_lat, h.dest_lng, h.dest_area
		FROM holidays h WHERE h.id = ?`, hid).
		Scan(&h.ID, &h.Name, &h.Color, &h.StartAt, &h.EndAt, &h.Journal, &h.CoverAsset, &h.Planned,
			&h.DestName, &h.DestLat, &h.DestLng, &box)
	if err == sql.ErrNoRows {
		return nil, errNoTrip
	}
	if err != nil {
		return nil, err
	}
	h.Active = h.EndAt == nil && !h.Planned
	all, err := a.queryPins(hid)
	if err != nil {
		return nil, err
	}
	// the arrival is decided on every pin, exactly as the family's own map does
	h.arrive(box, all)
	drop := wayPhotoPins(h, all)
	t := &sharedTrip{assets: map[string]int64{}}
	for _, p := range all {
		if !drop[p.ID] {
			t.pins = append(t.pins, p)
		}
	}
	if t.pins == nil {
		t.pins = []pinOut{}
	}
	type photo struct {
		asset, at string
		pin       int64
	}
	rows, err := a.reader().Query(`
		SELECT pp.asset_id, pp.pin_id, pp.taken_at FROM pin_photos pp
		JOIN pins p ON p.id = pp.pin_id WHERE p.holiday_id = ?`, hid)
	if err != nil {
		return nil, err
	}
	var photos []photo
	for rows.Next() {
		var ph photo
		if err := rows.Scan(&ph.asset, &ph.pin, &ph.at); err != nil {
			rows.Close()
			return nil, err
		}
		photos = append(photos, ph)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(photos, func(i, j int) bool { return photos[i].at < photos[j].at })
	first := ""
	for _, ph := range photos {
		if drop[ph.pin] {
			continue
		}
		t.assets[ph.asset] = ph.pin
		if first == "" {
			first = ph.asset
		}
	}
	// the chosen cover only if the link may show it, else the first photo left
	if _, ok := t.assets[h.CoverAsset]; !ok {
		h.CoverAsset = first
	}
	h.PinCount, h.PhotoCount = 0, 0
	for _, p := range t.pins {
		h.PinCount++
		h.PhotoCount += p.PhotoCount
	}
	t.holiday = h
	return t, nil
}

// wayPhotoPins is the Go twin of wayPhotoIds/journey in static/app.js: on a
// trip with a destination, the stops before the arrival pin were on the way
// there (all of them, while a live trip hasn't arrived), and the photo pins
// among them are hidden, unless the trip went back to that pin after
// arriving. Hand pins always stay. Keep the two in step.
func wayPhotoPins(h holidayOut, pins []pinOut) map[int64]bool {
	drop := map[int64]bool{}
	if h.DestName == "" {
		return drop
	}
	type stop struct {
		pin *pinOut
		at  string
	}
	var stops []stop
	for i := range pins {
		p := &pins[i]
		if len(p.Visits) == 0 {
			stops = append(stops, stop{p, p.VisitedAt})
			continue
		}
		for _, v := range p.Visits {
			stops = append(stops, stop{p, v.At})
		}
	}
	sort.SliceStable(stops, func(i, j int) bool {
		if stops[i].at != stops[j].at {
			return stops[i].at < stops[j].at
		}
		return stops[i].pin.ID < stops[j].pin.ID
	})
	split := -1
	if h.ArrivalPin != 0 {
		for i, s := range stops {
			if s.pin.ID == h.ArrivalPin {
				split = i
				break
			}
		}
	}
	switch {
	case split >= 0:
	case h.Active:
		split = len(stops)
	default:
		return drop // a finished trip that never arrived isn't split
	}
	stayed := map[int64]bool{}
	for _, s := range stops[split:] {
		stayed[s.pin.ID] = true
	}
	for _, s := range stops[:split] {
		if s.pin.Kind == "photo" && !stayed[s.pin.ID] {
			drop[s.pin.ID] = true
		}
	}
	return drop
}

// handleShareData returns the shared trip and its pins in the same shapes
// the logged-in API uses, so the frontend renders it with the same code.
func (a *app) handleShareData(w http.ResponseWriter, r *http.Request) {
	hid, ok := a.shareHoliday(w, r)
	if !ok {
		return
	}
	t, err := a.loadSharedTrip(hid)
	if err == errNoTrip {
		httpError(w, http.StatusNotFound, "this share link is no longer active")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"holiday": t.holiday, "pins": t.pins})
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
	t, err := a.loadSharedTrip(hid)
	if err != nil {
		httpError(w, http.StatusNotFound, "unknown pin")
		return
	}
	shown := false
	for _, p := range t.pins {
		if p.ID == pinID {
			shown = true
			break
		}
	}
	if !shown {
		httpError(w, http.StatusNotFound, "unknown pin")
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

// handleSharePhoto streams a photo to an anonymous link-holder — only assets
// on the pins the link shows, and never the original file: that carries the
// camera's GPS and every other EXIF field. Thumbnails and previews don't.
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
	if r.PathValue("kind") == "original" {
		httpError(w, http.StatusForbidden, "not available on a share link")
		return
	}
	t, err := a.loadSharedTrip(hid)
	if err != nil {
		httpError(w, http.StatusNotFound, "unknown photo")
		return
	}
	if _, ok := t.assets[assetID]; !ok {
		httpError(w, http.StatusNotFound, "unknown photo")
		return
	}
	// a day, not for good: revoking the link should stop the photos too
	a.streamPhoto(w, r, assetID, "private, max-age=86400")
}

