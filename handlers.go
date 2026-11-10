package main

import (
	"database/sql"
	"errors"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// readJSON decodes a JSON body; the Content-Type requirement doubles as CSRF
// protection (cross-origin forms can't send application/json with SameSite=Lax).
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		httpError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// truncate caps s at max bytes without splitting a multi-byte character —
// place names like "Łódź" or "東京" would otherwise be stored as invalid UTF-8.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// back up to the start of the character that straddles the cut
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpError(w, http.StatusBadRequest, "bad id")
		return 0, false
	}
	return id, true
}

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// --- holidays ---

type holidayOut struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Color         string  `json:"color"`
	StartAt       string  `json:"start_at"`
	EndAt         *string `json:"end_at"`
	Active        bool    `json:"active"`
	Planned       bool    `json:"planned"`
	Journal       string  `json:"journal"`
	CoverAsset    string  `json:"cover_asset"`
	PinCount      int     `json:"pin_count"`
	PhotoCount    int     `json:"photo_count"`
	UnplacedCount int     `json:"unplaced_count"`
	AskCount      int     `json:"ask_count"`        // photos near a hand pin, waiting for "same place?"
	SetAsideCount int     `json:"set_aside_count"`  // photos taken off the trip by hand
	Shared        bool    `json:"shared"`           // a link that still works; false once it has expired
	ShareExpires  *string `json:"share_expires_at"` // nil = never, or no link
	PackTotal     int     `json:"pack_total"`
	PackDone      int     `json:"pack_done"`
	DestName      string  `json:"dest_name"`
	DestLat       float64 `json:"dest_lat"`
	DestLng       float64 `json:"dest_lng"`
	DestArea      bool    `json:"dest_area"`   // has an outline, not just a point
	ArrivalPin    int64   `json:"arrival_pin"` // first pin that reached the destination; 0 = none yet
	CreatedBy     string  `json:"created_by"`  // F6: who started the trip; '' = unknown
	JournalBy     string  `json:"journal_by"`  // F6: who last wrote the journal
}

// arrive fills in the destination fields computed from pins and outline.
func (h *holidayOut) arrive(areaJSON string, pins []pinOut) {
	h.DestArea = areaJSON != ""
	if h.DestName != "" {
		h.ArrivalPin = arrivalPin(h.DestLat, h.DestLng, loadArea(areaJSON), pins)
	}
}

func (a *app) handleListHolidays(w http.ResponseWriter, r *http.Request) {
	// A countdown that reached zero goes live the moment anyone looks,
	// rather than waiting up to an hour for housekeeping.
	a.promotePlanned()
	pins, err := a.queryPins(0)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	out, err := a.listHolidays(pins)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleMap is everything the map opens with, in one round trip: who is
// signed in, every trip and every pin. The pins are queried once and serve
// both the pin list and the trips' counts and arrivals.
func (a *app) handleMap(w http.ResponseWriter, r *http.Request) {
	a.promotePlanned()
	if a.maybeSyncActive() {
		w.Header().Set("X-Photo-Sync", "running")
	}
	pins, err := a.queryPins(0)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	holidays, err := a.listHolidays(pins)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"me":       meOut(r.Context().Value(userKey).(sessionUser)),
		"holidays": holidays,
		"pins":     pins,
	})
}

// listHolidays builds the trip list from pins the caller already loaded
// (the database has one connection, so they can't be read while the
// holidays rows below are open). Pin and photo counts come from those pins
// rather than two more subqueries per trip.
func (a *app) listHolidays(pins []pinOut) ([]holidayOut, error) {
	byTrip := map[int64][]pinOut{}
	for _, p := range pins {
		byTrip[p.HolidayID] = append(byTrip[p.HolidayID], p)
	}
	rows, err := a.reader().Query(`
		SELECT h.id, h.name, h.color, h.start_at, h.end_at, h.journal,
		       COALESCE((SELECT pp.asset_id FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
		                 WHERE p.holiday_id = h.id AND pp.asset_id = h.cover_asset),
		                (SELECT up.asset_id FROM unplaced_photos up
		                 WHERE up.holiday_id = h.id AND up.asset_id = h.cover_asset),
		                (SELECT pp.asset_id FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
		                 WHERE p.holiday_id = h.id ORDER BY pp.taken_at LIMIT 1), ''),
		       (SELECT COUNT(*) FROM unplaced_photos up WHERE up.holiday_id = h.id),
		       (SELECT COUNT(*) FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
		        WHERE p.holiday_id = h.id AND pp.ask = 1),
		       (SELECT COUNT(*) FROM set_aside sa WHERE sa.holiday_id = h.id),
		       EXISTS (SELECT 1 FROM shares s WHERE s.holiday_id = h.id),
		       (SELECT s.expires_at FROM shares s WHERE s.holiday_id = h.id),
		       (SELECT COUNT(*) FROM packing_items pi WHERE pi.holiday_id = h.id),
		       (SELECT COUNT(*) FROM packing_items pi WHERE pi.holiday_id = h.id AND pi.checked = 1),
		       h.dest_name, h.dest_lat, h.dest_lng, h.planned, length(h.dest_area),
		       h.created_by, h.journal_by
		FROM holidays h ORDER BY h.start_at DESC`)
	if err != nil {
		return nil, err
	}
	out := []holidayOut{}
	areaLen := map[int64]int{}
	for rows.Next() {
		var h holidayOut
		var n int
		var expires sql.NullString
		if err := rows.Scan(&h.ID, &h.Name, &h.Color, &h.StartAt, &h.EndAt, &h.Journal, &h.CoverAsset, &h.UnplacedCount, &h.AskCount, &h.SetAsideCount, &h.Shared, &expires, &h.PackTotal, &h.PackDone, &h.DestName, &h.DestLat, &h.DestLng, &h.Planned, &n, &h.CreatedBy, &h.JournalBy); err != nil {
			rows.Close()
			return nil, err
		}
		if h.Shared && expires.Valid {
			h.ShareExpires = &expires.String
			h.Shared = !shareExpired(expires)
		}
		for _, p := range byTrip[h.ID] {
			h.PinCount++
			h.PhotoCount += p.PhotoCount
		}
		h.Active = h.EndAt == nil && !h.Planned
		areaLen[h.ID] = n
		out = append(out, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// outlines only after the rows are closed: a cache miss reads one
	for i := range out {
		h := &out[i]
		h.DestArea = areaLen[h.ID] > 0
		if h.DestName != "" {
			h.ArrivalPin = arrivalPin(h.DestLat, h.DestLng, a.cachedArea(h.ID, areaLen[h.ID]), byTrip[h.ID])
		}
	}
	return out, nil
}

func (a *app) handleCreateHoliday(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string  `json:"name"`
		Color    string  `json:"color"`
		StartAt  string  `json:"start_at"`  // optional RFC3339 or YYYY-MM-DD backdate
		EndAt    string  `json:"end_at"`    // optional: set to import a past trip (created already ended)
		DestName string  `json:"dest_name"` // optional destination (from place search or typed coords)
		DestLat  float64 `json:"dest_lat"`
		DestLng  float64 `json:"dest_lng"`
		DestOSM  string  `json:"dest_osm"` // optional: the place search hit, whose outline is fetched
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name required")
		return
	}
	if !colorRe.MatchString(req.Color) {
		httpError(w, http.StatusBadRequest, "color must be #RRGGBB")
		return
	}
	start := time.Now().UTC()
	if req.StartAt != "" {
		t, ok := parseWhen(req.StartAt, false)
		if !ok {
			httpError(w, http.StatusBadRequest, "start_at must be RFC3339 or YYYY-MM-DD")
			return
		}
		start = t
	}
	// A first day after today makes a planned trip: it counts down, collects
	// nothing, and goes live on the day. Its last day is set by ending it,
	// like any live trip — a future end_at would leave it ended before it began.
	planned := start.After(time.Now().UTC())
	if planned && req.EndAt != "" {
		httpError(w, http.StatusBadRequest, "a planned trip gets its last day when you end it — leave Last day empty")
		return
	}
	var endAt *string
	if req.EndAt != "" {
		t, ok := parseWhen(req.EndAt, true)
		if !ok {
			httpError(w, http.StatusBadRequest, "end_at must be RFC3339 or YYYY-MM-DD")
			return
		}
		if t.Before(start) {
			httpError(w, http.StatusBadRequest, "end_at is before start_at")
			return
		}
		s := t.Format(time.RFC3339)
		endAt = &s
	}
	destName, destLat, destLng, bad := cleanDest(req.DestName, req.DestLat, req.DestLng)
	if bad != "" {
		httpError(w, http.StatusBadRequest, bad)
		return
	}
	destArea := resolveArea(r.Context(), destName, req.DestOSM)
	by := userName(r)
	res, err := a.db.Exec(`INSERT INTO holidays (name, color, start_at, end_at, dest_name, dest_lat, dest_lng, dest_area, planned, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Name, req.Color, start.Format(time.RFC3339), endAt, destName, destLat, destLng, destArea, planned, by)
	if err != nil {
		httpError(w, http.StatusConflict, "a holiday is already active — end it first")
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusCreated, holidayOut{
		ID: id, Name: req.Name, Color: req.Color,
		StartAt: start.Format(time.RFC3339), EndAt: endAt, Active: endAt == nil && !planned, Planned: planned,
		DestName: destName, DestLat: destLat, DestLng: destLng, DestArea: destArea != "",
		CreatedBy: by,
	})
}

// promotePlanned puts the earliest planned trip whose first day has come live.
// While another trip is still live it waits; it goes the moment that one ends.
func (a *app) promotePlanned() {
	// a read first, so the map's every load isn't a write (P4); the write
	// below only runs on the rare load when a countdown has reached zero
	now := time.Now().UTC().Format(time.RFC3339)
	var due bool
	// "due" means the UPDATE below would change a row: a planned trip whose day
	// has come AND no other live trip, else it would run on every load while
	// the live trip lasts
	if err := a.reader().QueryRow(`SELECT EXISTS (SELECT 1 FROM holidays WHERE planned = 1 AND start_at <= ?)
		AND NOT EXISTS (SELECT 1 FROM holidays WHERE end_at IS NULL AND planned = 0)`, now).Scan(&due); err == nil && !due {
		return
	}
	if _, err := a.db.Exec(`
		UPDATE holidays SET planned = 0
		WHERE id = (SELECT id FROM holidays WHERE planned = 1 AND start_at <= ?
		            ORDER BY start_at LIMIT 1)
		  AND NOT EXISTS (SELECT 1 FROM holidays WHERE end_at IS NULL AND planned = 0)`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Printf("promote planned trip: %v", err)
	}
}

func (a *app) handleEndHoliday(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	end := time.Now().UTC().Format(time.RFC3339)
	res, err := a.db.Exec(`UPDATE holidays SET end_at = ? WHERE id = ? AND end_at IS NULL AND planned = 0`, end, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "no such active holiday")
		return
	}
	// Final sync so the trip's last photos land before it goes dormant.
	if _, _, err := a.syncHoliday(id); err != nil {
		// Photos can be re-synced later; ending must not fail.
		writeJSON(w, http.StatusOK, map[string]any{"end_at": end, "sync_error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"end_at": end})
}

func (a *app) handleUpdateHoliday(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name       *string `json:"name"`
		Color      *string `json:"color"`
		StartAt    *string `json:"start_at"`
		EndAt      *string `json:"end_at"`
		Journal    *string `json:"journal"`
		CoverAsset *string `json:"cover_asset"`
		// dest_name present = set the destination ("" removes it); the
		// point and box travel with it and are ignored without it
		DestName *string `json:"dest_name"`
		DestLat  float64 `json:"dest_lat"`
		DestLng  float64 `json:"dest_lng"`
		DestOSM  string  `json:"dest_osm"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// Checked before anything is written, like the dates below.
	var destName string
	var destLat, destLng float64
	if req.DestName != nil {
		var bad string
		destName, destLat, destLng, bad = cleanDest(*req.DestName, req.DestLat, req.DestLng)
		if bad != "" {
			httpError(w, http.StatusBadRequest, bad)
			return
		}
	}
	if req.StartAt != nil || req.EndAt != nil {
		// Either end can move, so check the order against whichever side
		// is staying put.
		var curStart string
		var curEnd *string
		var planned bool
		if err := a.db.QueryRow(`SELECT start_at, end_at, planned FROM holidays WHERE id = ?`, id).Scan(&curStart, &curEnd, &planned); err != nil {
			httpError(w, http.StatusNotFound, "no such holiday")
			return
		}
		if planned && req.EndAt != nil {
			httpError(w, http.StatusBadRequest, "a planned trip gets its last day when you end it")
			return
		}
		start, _ := time.Parse(time.RFC3339, curStart)
		if req.StartAt != nil {
			t, ok := parseWhen(*req.StartAt, false)
			if !ok {
				httpError(w, http.StatusBadRequest, "start_at must be RFC3339 or YYYY-MM-DD")
				return
			}
			start = t
		}
		var end *time.Time
		if curEnd != nil {
			if t, err := time.Parse(time.RFC3339, *curEnd); err == nil {
				end = &t
			}
		}
		if req.EndAt != nil {
			t, ok := parseWhen(*req.EndAt, true)
			if !ok {
				httpError(w, http.StatusBadRequest, "end_at must be RFC3339 or YYYY-MM-DD")
				return
			}
			end = &t
		}
		if end != nil && end.Before(start) {
			httpError(w, http.StatusBadRequest, "the trip can't end before it starts")
			return
		}
		if req.StartAt != nil {
			a.db.Exec(`UPDATE holidays SET start_at = ? WHERE id = ?`, start.Format(time.RFC3339), id)
			// An open trip moved to a first day still ahead goes back to
			// counting down; one moved into the past is promotePlanned's.
			if end == nil && start.After(time.Now().UTC()) {
				a.db.Exec(`UPDATE holidays SET planned = 1 WHERE id = ?`, id)
			}
		}
		if req.EndAt != nil {
			a.db.Exec(`UPDATE holidays SET end_at = ? WHERE id = ?`, end.Format(time.RFC3339), id)
		}
	}
	if req.Journal != nil {
		a.db.Exec(`UPDATE holidays SET journal = ?, journal_by = ? WHERE id = ?`, *req.Journal, userName(r), id)
	}
	if req.DestName != nil {
		// a new destination without a search hit (typed coordinates) drops
		// the old outline along with the old point
		a.db.Exec(`UPDATE holidays SET dest_name = ?, dest_lat = ?, dest_lng = ?, dest_area = ?, dest_bbox = '' WHERE id = ?`,
			destName, destLat, destLng, resolveArea(r.Context(), destName, req.DestOSM), id)
		a.forgetArea(id)
	}
	if req.CoverAsset != nil {
		// Empty clears the choice; otherwise the cover must be a photo from
		// this trip (on a pin or still unplaced).
		if *req.CoverAsset == "" {
			a.db.Exec(`UPDATE holidays SET cover_asset = '' WHERE id = ?`, id)
		} else {
			res, _ := a.db.Exec(`UPDATE holidays SET cover_asset = ? WHERE id = ?
				AND (EXISTS (SELECT 1 FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
				             WHERE p.holiday_id = ? AND pp.asset_id = ?)
				  OR EXISTS (SELECT 1 FROM unplaced_photos up
				             WHERE up.holiday_id = ? AND up.asset_id = ?))`,
				*req.CoverAsset, id, id, *req.CoverAsset, id, *req.CoverAsset)
			if n, _ := res.RowsAffected(); n == 0 {
				httpError(w, http.StatusBadRequest, "cover photo isn't on this trip")
				return
			}
		}
	}
	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			httpError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		a.db.Exec(`UPDATE holidays SET name = ? WHERE id = ?`, strings.TrimSpace(*req.Name), id)
	}
	if req.Color != nil {
		if !colorRe.MatchString(*req.Color) {
			httpError(w, http.StatusBadRequest, "color must be #RRGGBB")
			return
		}
		a.db.Exec(`UPDATE holidays SET color = ? WHERE id = ?`, *req.Color, id)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleDeleteHoliday(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tok, found, err := a.deleteWithUndo("holidays", id, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	a.stateMu.Lock()
	delete(a.lastSync, id)
	a.stateMu.Unlock()
	a.forgetArea(id)
	out := map[string]any{"ok": true}
	if found {
		out["undo"] = tok
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleSyncHoliday(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	pins, photos, err := a.syncHoliday(id)
	if err == sql.ErrNoRows {
		httpError(w, http.StatusNotFound, "no such holiday")
		return
	}
	if err != nil {
		httpError(w, http.StatusBadGateway, "sync failed: "+err.Error())
		return
	}
	var unplaced int
	a.db.QueryRow(`SELECT COUNT(*) FROM unplaced_photos WHERE holiday_id = ?`, id).Scan(&unplaced)
	writeJSON(w, http.StatusOK, map[string]int{"photo_pins": pins, "photos": photos, "unplaced": unplaced})
}

func (a *app) handleUnplacedPhotos(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(`
		SELECT asset_id, taken_at, type, duration FROM unplaced_photos WHERE holiday_id = ? ORDER BY taken_at`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []photoOut{}
	for rows.Next() {
		var p photoOut
		if err := rows.Scan(&p.AssetID, &p.TakenAt, &p.Type, &p.Duration); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAttachPhotos moves unplaced photos onto a pin. Used from the
// "photos without a location" gallery to file indoor shots on the right place.
func (a *app) handleAttachPhotos(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		AssetIDs []string `json:"asset_ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	var lat, lng float64
	var holidayID int64
	if err := a.db.QueryRow(`SELECT holiday_id, lat, lng FROM pins WHERE id = ?`, id).Scan(&holidayID, &lat, &lng); err != nil {
		httpError(w, http.StatusNotFound, "no such pin")
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer tx.Rollback()
	moved := 0
	for _, asset := range req.AssetIDs {
		// Only photos genuinely unplaced on this holiday are eligible.
		var takenAt, typ string
		var duration float64
		if err := tx.QueryRow(`SELECT taken_at, type, duration FROM unplaced_photos WHERE asset_id = ? AND holiday_id = ?`,
			asset, holidayID).Scan(&takenAt, &typ, &duration); err != nil {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng, type, duration)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (asset_id) DO NOTHING`,
			id, asset, takenAt, lat, lng, typ, duration); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		if _, err := tx.Exec(`DELETE FROM unplaced_photos WHERE asset_id = ?`, asset); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		moved++
	}
	if err := tx.Commit(); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"moved": moved})
}

// handleAskPhotos lists the photos sync filed on a hand-placed pin without
// being sure they belong there, grouped by that pin.
func (a *app) handleAskPhotos(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(`
		SELECT p.id, p.title, p.lat, p.lng, pp.asset_id, pp.taken_at, pp.type, pp.duration, pp.lat, pp.lng
		FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
		WHERE p.holiday_id = ? AND pp.ask = 1
		ORDER BY p.id, pp.taken_at`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	type photo struct {
		photoOut
		KM float64 `json:"km"`
	}
	type group struct {
		PinID  int64   `json:"pin_id"`
		Title  string  `json:"title"`
		Photos []photo `json:"photos"`
	}
	out := []*group{}
	for rows.Next() {
		var g group
		var plat, plng, lat, lng float64
		var ph photo
		if err := rows.Scan(&g.PinID, &g.Title, &plat, &plng, &ph.AssetID, &ph.TakenAt, &ph.Type, &ph.Duration, &lat, &lng); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		ph.KM = math.Round(haversineKM(lat, lng, plat, plng)*10) / 10
		if n := len(out); n == 0 || out[n-1].PinID != g.PinID {
			out = append(out, &g)
		}
		last := out[len(out)-1]
		last.Photos = append(last.Photos, ph)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAnswerPhotos records "same place" (they stay on the pin, for good)
// or "its own stop" (a re-sync files them as it would with no pin nearby).
func (a *app) handleAnswerPhotos(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		AssetIDs []string `json:"asset_ids"`
		Same     bool     `json:"same"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer tx.Rollback()
	answered := 0
	for _, asset := range req.AssetIDs {
		var pinID int64
		if err := tx.QueryRow(`
			SELECT pp.pin_id FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
			WHERE pp.asset_id = ? AND p.holiday_id = ? AND pp.ask = 1`, asset, id).Scan(&pinID); err != nil {
			continue // not a question on this trip
		}
		choice := pinID
		if !req.Same {
			choice = 0
		}
		if _, err := tx.Exec(`
			INSERT INTO photo_choices (asset_id, holiday_id, pin_id) VALUES (?, ?, ?)
			ON CONFLICT (asset_id) DO UPDATE SET holiday_id = excluded.holiday_id, pin_id = excluded.pin_id`,
			asset, id, choice); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		if _, err := tx.Exec(`UPDATE pin_photos SET ask = 0 WHERE asset_id = ?`, asset); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		answered++
	}
	if err := tx.Commit(); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if !req.Same && answered > 0 {
		// move them now; if Immich is away, the next sync does it
		if _, _, err := a.syncHoliday(id); err != nil {
			log.Printf("re-sync after photo answer, holiday %d: %v", id, err)
			a.resyncSoon(id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"answered": answered})
}

// handleHolidayTimeline returns every photo of a holiday (placed + unplaced)
// in taken order — the "just show me the trip in sequence" view.
func (a *app) handleHolidayTimeline(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(`
		SELECT pp.asset_id, pp.taken_at, pp.type, pp.duration FROM pin_photos pp
		  JOIN pins p ON p.id = pp.pin_id WHERE p.holiday_id = ?
		UNION ALL
		SELECT up.asset_id, up.taken_at, up.type, up.duration FROM unplaced_photos up WHERE up.holiday_id = ?
		ORDER BY taken_at`, id, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []photoOut{}
	for rows.Next() {
		var p photoOut
		if err := rows.Scan(&p.AssetID, &p.TakenAt, &p.Type, &p.Duration); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleStamps returns one entry per (holiday, country) visited — the
// passport page. Countries come from photo EXIF via Immich.
func (a *app) handleStamps(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`
		SELECT DISTINCT p.country, h.id, h.name, h.color, h.start_at, h.end_at
		FROM pins p JOIN holidays h ON h.id = p.holiday_id
		WHERE p.country != ''
		ORDER BY h.start_at DESC, p.country`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	type stamp struct {
		Country   string  `json:"country"`
		HolidayID int64   `json:"holiday_id"`
		Name      string  `json:"name"`
		Color     string  `json:"color"`
		StartAt   string  `json:"start_at"`
		EndAt     *string `json:"end_at"`
	}
	out := []stamp{}
	for rows.Next() {
		var s stamp
		if err := rows.Scan(&s.Country, &s.HolidayID, &s.Name, &s.Color, &s.StartAt, &s.EndAt); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, s)
	}
	writeJSON(w, http.StatusOK, out)
}

// parseWhen accepts RFC3339 or a bare date; bare dates mean start-of-day
// (or end-of-day for end dates) UTC.
func parseWhen(s string, endOfDay bool) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Second)
		}
		return t.UTC(), true
	}
	return time.Time{}, false
}

// --- pins ---

type pinOut struct {
	ID         int64     `json:"id"`
	HolidayID  int64     `json:"holiday_id"`
	Kind       string    `json:"kind"`
	Lat        float64   `json:"lat"`
	Lng        float64   `json:"lng"`
	Title      string    `json:"title"`
	Note       string    `json:"note"`
	CreatedAt  string    `json:"created_at"`
	VisitedAt  string    `json:"visited_at"`
	PhotoCount int       `json:"photo_count"`
	CoverAsset string    `json:"cover_asset"`
	Visits     []visit   `json:"visits"`
	CreatedBy  string    `json:"created_by"`  // F6; '' = unknown, and always '' on a share
	OtherNotes []dayNote `json:"other_notes"` // task 136: notes on a day with no visit now
}

// queryPins returns pins for one holiday, or every pin when holidayID is 0.
func (a *app) queryPins(holidayID int64) ([]pinOut, error) {
	rows, err := a.reader().Query(`
		SELECT p.id, p.holiday_id, p.kind, p.lat, p.lng, p.title, p.note, p.created_at, p.created_by,
		       COALESCE((SELECT MIN(pp.taken_at) FROM pin_photos pp WHERE pp.pin_id = p.id),
		                p.created_at),
		       (SELECT COUNT(*) FROM pin_photos pp WHERE pp.pin_id = p.id),
		       COALESCE((SELECT pp.asset_id FROM pin_photos pp WHERE pp.pin_id = p.id
		                 AND pp.asset_id = p.cover_asset),
		                (SELECT pp.asset_id FROM pin_photos pp WHERE pp.pin_id = p.id
		                 ORDER BY pp.taken_at LIMIT 1), '')
		FROM pins p WHERE ? = 0 OR p.holiday_id = ?`, holidayID, holidayID)
	if err != nil {
		return nil, err
	}
	out := []pinOut{}
	for rows.Next() {
		var p pinOut
		if err := rows.Scan(&p.ID, &p.HolidayID, &p.Kind, &p.Lat, &p.Lng, &p.Title, &p.Note, &p.CreatedAt, &p.CreatedBy, &p.VisitedAt, &p.PhotoCount, &p.CoverAsset); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// the photos' times, once the pin rows are closed (one connection)
	rows, err = a.reader().Query(`
		SELECT pp.pin_id, pp.taken_at FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
		WHERE ? = 0 OR p.holiday_id = ?`, holidayID, holidayID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var shots []shot
	for rows.Next() {
		var s shot
		if err := rows.Scan(&s.pin, &s.at); err != nil {
			return nil, err
		}
		shots = append(shots, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fillVisits(out, shots)
	notes, err := a.queryVisitNotes(holidayID)
	if err != nil {
		return nil, err
	}
	applyVisitNotes(out, notes)
	return out, nil
}

// queryVisitNotes is visit_notes by pin and day, for one holiday or all (0).
func (a *app) queryVisitNotes(holidayID int64) (map[int64]map[string]string, error) {
	rows, err := a.reader().Query(`
		SELECT vn.pin_id, vn.day, vn.note FROM visit_notes vn JOIN pins p ON p.id = vn.pin_id
		WHERE ? = 0 OR p.holiday_id = ?`, holidayID, holidayID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]string{}
	for rows.Next() {
		var pin int64
		var day, note string
		if err := rows.Scan(&pin, &day, &note); err != nil {
			return nil, err
		}
		if out[pin] == nil {
			out[pin] = map[string]string{}
		}
		out[pin][day] = note
	}
	return out, rows.Err()
}

// writeVisitNote (task 136) sets the note of one visit day of a pin, in one
// transaction: from_day's note goes first (a note moved to another visit),
// and a pin's old single note (pins.note, shown on its first visit while the
// pin has no rows) moves to a row on that first visit's day, so it is neither
// lost nor shown twice. An empty note deletes the day's row.
// errNoteConflict: a per-visit note write that would hide or erase a note.
var errNoteConflict = errors.New("note conflict")

func (a *app) writeVisitNote(pinID int64, day string, fromDay *string, note string) error {
	var hid int64
	if err := a.db.QueryRow(`SELECT holiday_id FROM pins WHERE id = ?`, pinID).Scan(&hid); err != nil {
		return err
	}
	firstDay := ""
	if pins, err := a.queryPins(hid); err != nil {
		return err
	} else {
		for _, p := range pins {
			if p.ID == pinID && len(p.Visits) > 0 {
				firstDay = p.Visits[0].Day
			}
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var legacy string
	var rows int
	if err := tx.QueryRow(`SELECT note, (SELECT COUNT(*) FROM visit_notes WHERE pin_id = pins.id) FROM pins WHERE id = ?`, pinID).Scan(&legacy, &rows); err != nil {
		return err
	}
	// ada #9791: an old note with no visit to sit on would be hidden by the
	// first row, so that write is refused (the phone sends a plain note there).
	if legacy != "" && rows == 0 && firstDay == "" {
		return errNoteConflict
	}
	// Moving a note onto a day that holds other words would erase them.
	if fromDay != nil && *fromDay != day {
		var there string
		tx.QueryRow(`SELECT note FROM visit_notes WHERE pin_id = ? AND day = ?`, pinID, day).Scan(&there)
		if there == "" && rows == 0 && day == firstDay {
			there = legacy // ada #9798: the old note still sits on the first visit
		}
		if there != "" && there != note {
			return errNoteConflict
		}
	}
	if legacy != "" && rows == 0 && firstDay != "" {
		if fromDay == nil || *fromDay != firstDay {
			if _, err := tx.Exec(`INSERT INTO visit_notes (pin_id, day, note) VALUES (?, ?, ?)`, pinID, firstDay, legacy); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE pins SET note = '' WHERE id = ?`, pinID); err != nil {
			return err
		}
	}
	if fromDay != nil && *fromDay != day {
		if _, err := tx.Exec(`DELETE FROM visit_notes WHERE pin_id = ? AND day = ?`, pinID, *fromDay); err != nil {
			return err
		}
	}
	if strings.TrimSpace(note) == "" {
		_, err = tx.Exec(`DELETE FROM visit_notes WHERE pin_id = ? AND day = ?`, pinID, day)
	} else {
		_, err = tx.Exec(`INSERT INTO visit_notes (pin_id, day, note) VALUES (?, ?, ?)
			ON CONFLICT (pin_id, day) DO UPDATE SET note = excluded.note`, pinID, day, note)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (a *app) handleListPins(w http.ResponseWriter, r *http.Request) {
	if a.maybeSyncActive() {
		w.Header().Set("X-Photo-Sync", "running")
	}
	out, err := a.queryPins(0)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// clientIDRe is the shape of a pin's client_id: the id the page gave the pin
// before its first post. Old queue ids look like "1794298543932:k3x9af".
var clientIDRe = regexp.MustCompile(`^[A-Za-z0-9_:.-]{1,64}$`)

// keyedPin answers a repeat of a keyed post: the pin the key made, as it is
// now. found is false when the key is new; gone is true when the key is known
// but its pin is not there any more. The pin's own client_id is compared too,
// because SQLite hands a deleted pin's id to the next pin.
func keyedPin(tx *sql.Tx, user, key string) (p pinOut, found, gone bool, err error) {
	var id int64
	err = tx.QueryRow(`SELECT pin_id FROM pin_keys WHERE created_by = ? AND client_id = ?`, user, key).Scan(&id)
	if err == sql.ErrNoRows {
		return p, false, false, nil
	}
	if err != nil {
		return p, false, false, err
	}
	err = tx.QueryRow(`SELECT id, holiday_id, kind, lat, lng, title, note, created_at, created_by
		FROM pins WHERE id = ? AND client_id = ?`, id, key).
		Scan(&p.ID, &p.HolidayID, &p.Kind, &p.Lat, &p.Lng, &p.Title, &p.Note, &p.CreatedAt, &p.CreatedBy)
	if err == sql.ErrNoRows {
		return p, true, true, nil
	}
	return p, true, false, err
}

func (a *app) handleCreatePin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		HolidayID int64   `json:"holiday_id"`
		Lat       float64 `json:"lat"`
		Lng       float64 `json:"lng"`
		Title     string  `json:"title"`
		Note      string  `json:"note"`
		CreatedAt string  `json:"created_at"` // optional: pins queued offline sync later but keep their real capture time
		ClientID  string  `json:"client_id"`  // optional (1.18.0): the same id sent again stores nothing new
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.ClientID != "" && !clientIDRe.MatchString(req.ClientID) {
		httpError(w, http.StatusBadRequest, "invalid client_id")
		return
	}
	// One transaction on the single write connection: two posts with one key
	// at the same moment run one after the other, and the second is a repeat.
	tx, err := a.db.Begin()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer tx.Rollback()
	if req.ClientID != "" {
		p, found, gone, err := keyedPin(tx, userName(r), req.ClientID)
		switch {
		case err != nil:
			httpError(w, http.StatusInternalServerError, "database error")
			return
		case gone:
			httpError(w, http.StatusGone, "pin was deleted")
			return
		case found:
			// the repeat's own body is ignored: the first post won
			writeJSON(w, http.StatusCreated, p)
			return
		}
	}
	if req.Lat < -90 || req.Lat > 90 || req.Lng < -180 || req.Lng > 180 {
		httpError(w, http.StatusBadRequest, "invalid coordinates")
		return
	}
	createdAt := time.Now().UTC()
	if req.CreatedAt != "" {
		t, err := time.Parse(time.RFC3339, req.CreatedAt)
		if err != nil || t.After(time.Now().Add(5*time.Minute)) {
			httpError(w, http.StatusBadRequest, "invalid created_at")
			return
		}
		createdAt = t.UTC()
	}
	if req.HolidayID == 0 {
		if err := tx.QueryRow(`SELECT id FROM holidays WHERE end_at IS NULL AND planned = 0`).Scan(&req.HolidayID); err != nil {
			httpError(w, http.StatusBadRequest, "no active holiday — start one or pass holiday_id")
			return
		}
	}
	res, err := tx.Exec(`
		INSERT INTO pins (holiday_id, kind, lat, lng, title, note, created_at, created_by, client_id)
		VALUES (?, 'manual', ?, ?, ?, ?, ?, ?, ?)`,
		req.HolidayID, req.Lat, req.Lng, strings.TrimSpace(req.Title), req.Note,
		createdAt.Format(time.RFC3339), userName(r), req.ClientID)
	if err != nil {
		httpError(w, http.StatusBadRequest, "no such holiday")
		return
	}
	id, _ := res.LastInsertId()
	if req.ClientID != "" {
		if _, err := tx.Exec(`INSERT INTO pin_keys (created_by, client_id, pin_id, created_at) VALUES (?, ?, ?, ?)`,
			userName(r), req.ClientID, id, time.Now().UTC().Format(time.RFC3339)); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	a.resyncSoon(req.HolidayID)
	writeJSON(w, http.StatusCreated, pinOut{
		ID: id, HolidayID: req.HolidayID, Kind: "manual",
		Lat: req.Lat, Lng: req.Lng, Title: strings.TrimSpace(req.Title), Note: req.Note,
		CreatedAt: createdAt.Format(time.RFC3339), CreatedBy: userName(r),
	})
}

func (a *app) handleUpdatePin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		Title      *string  `json:"title"`
		Note       *string  `json:"note"`
		Lat        *float64 `json:"lat"`
		Lng        *float64 `json:"lng"`
		CoverAsset *string  `json:"cover_asset"`
		NoteDay    *string  `json:"note_day"` // task 136: the note is this visit day's
		FromDay    *string  `json:"from_day"` // task 136: the visit day the note moves from
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.FromDay != nil && req.NoteDay == nil {
		httpError(w, http.StatusBadRequest, "from_day needs note_day")
		return
	}
	if req.NoteDay == nil && req.Note != nil {
		// ada #9791: once a pin has per-visit notes, a plain note would be
		// shown at no visit. Only a page from before 1.17.0 sends it.
		var rows int
		a.db.QueryRow(`SELECT COUNT(*) FROM visit_notes WHERE pin_id = ?`, id).Scan(&rows)
		if rows > 0 {
			httpError(w, http.StatusConflict, "notes are per visit now: reload the page")
			return
		}
	}
	if req.NoteDay != nil {
		if req.Note == nil {
			httpError(w, http.StatusBadRequest, "note_day needs note")
			return
		}
		if !validDay(*req.NoteDay) || req.FromDay != nil && !validDay(*req.FromDay) {
			httpError(w, http.StatusBadRequest, "day must be YYYY-MM-DD")
			return
		}
		if err := a.writeVisitNote(id, *req.NoteDay, req.FromDay, *req.Note); err == sql.ErrNoRows {
			httpError(w, http.StatusNotFound, "no such pin")
			return
		} else if err == errNoteConflict {
			httpError(w, http.StatusConflict, "that visit already has a note, or the old note has no visit: reload the page")
			return
		} else if err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		req.Note = nil // written to the visit, not to pins.note
	}
	if req.Title != nil {
		a.db.Exec(`UPDATE pins SET title = ? WHERE id = ?`, strings.TrimSpace(*req.Title), id)
	}
	if req.Note != nil {
		a.db.Exec(`UPDATE pins SET note = ? WHERE id = ?`, *req.Note, id)
	}
	if req.CoverAsset != nil {
		// Empty clears the choice (back to earliest photo); otherwise the
		// cover must be one of the pin's own photos.
		if *req.CoverAsset == "" {
			a.db.Exec(`UPDATE pins SET cover_asset = '' WHERE id = ?`, id)
		} else {
			res, _ := a.db.Exec(`UPDATE pins SET cover_asset = ? WHERE id = ?
				AND EXISTS (SELECT 1 FROM pin_photos WHERE pin_id = ? AND asset_id = ?)`,
				*req.CoverAsset, id, id, *req.CoverAsset)
			if n, _ := res.RowsAffected(); n == 0 {
				httpError(w, http.StatusBadRequest, "cover photo isn't on this pin")
				return
			}
		}
	}
	if req.Lat != nil && req.Lng != nil {
		// Only manual pins move by hand; photo pins follow their photos.
		a.db.Exec(`UPDATE pins SET lat = ?, lng = ? WHERE id = ? AND kind = 'manual'`, *req.Lat, *req.Lng, id)
		a.resyncPinSoon(id)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleDeletePin(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var holidayID int64
	a.db.QueryRow(`SELECT holiday_id FROM pins WHERE id = ?`, id).Scan(&holidayID)
	// Its photos go with it until the next sync files them again.
	tok, found, err := a.deleteWithUndo("pins", id, holidayID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	a.resyncSoon(holidayID)
	out := map[string]any{"ok": true}
	if found {
		out["undo"] = tok
	}
	writeJSON(w, http.StatusOK, out)
}

// userName is the signed-in family member, for "added by" (F6).
func userName(r *http.Request) string {
	if u, ok := r.Context().Value(userKey).(sessionUser); ok {
		return u.Username
	}
	return ""
}

// resyncSoon makes the next map load re-sync the trip, so a pin placed,
// moved or removed by hand gathers or releases its photos straight away.
func (a *app) resyncSoon(holidayID int64) {
	a.stateMu.Lock()
	delete(a.lastSync, holidayID)
	a.stateMu.Unlock()
}

func (a *app) resyncPinSoon(pinID int64) {
	var holidayID int64
	if a.db.QueryRow(`SELECT holiday_id FROM pins WHERE id = ?`, pinID).Scan(&holidayID) == nil {
		a.resyncSoon(holidayID)
	}
}

// photoOut is one entry of every photo list (F7: type and duration).
type photoOut struct {
	AssetID  string  `json:"asset_id"`
	TakenAt  string  `json:"taken_at"`
	Type     string  `json:"type"`     // "image" or "video"
	Duration float64 `json:"duration"` // seconds; 0 for an image
}

func (a *app) handlePinPhotos(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(`
		SELECT asset_id, taken_at, type, duration FROM pin_photos WHERE pin_id = ? ORDER BY taken_at`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []photoOut{}
	for rows.Next() {
		var p photoOut
		if err := rows.Scan(&p.AssetID, &p.TakenAt, &p.Type, &p.Duration); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleExport streams the whole travel log as one JSON document — data
// portability and an extra backup path. Photos are Immich asset references,
// not files; the image data itself lives (and is backed up) in Immich.
func (a *app) handleExport(w http.ResponseWriter, r *http.Request) {
	type photoExp struct {
		AssetID  string  `json:"asset_id"`
		TakenAt  string  `json:"taken_at"`
		Lat      float64 `json:"lat"`
		Lng      float64 `json:"lng"`
		Type     string  `json:"type"`
		Duration float64 `json:"duration"`
	}
	type pinExp struct {
		ID         int64      `json:"id"`
		Kind       string     `json:"kind"`
		Lat        float64    `json:"lat"`
		Lng        float64    `json:"lng"`
		Title      string     `json:"title"`
		Note       string     `json:"note"`
		Country    string     `json:"country"`
		CoverAsset string     `json:"cover_asset"`
		CreatedAt  string     `json:"created_at"`
		Photos     []photoExp `json:"photos"`
		VisitNotes []dayNote  `json:"visit_notes,omitempty"` // task 136
	}
	type packingExp struct {
		Label   string `json:"label"`
		Checked bool   `json:"checked"`
	}
	type holidayExp struct {
		ID         int64           `json:"id"`
		Name       string          `json:"name"`
		Color      string          `json:"color"`
		StartAt    string          `json:"start_at"`
		EndAt      *string         `json:"end_at"`
		Journal    string          `json:"journal"`
		CoverAsset string          `json:"cover_asset"`
		DestName   string          `json:"dest_name"`
		DestLat    float64         `json:"dest_lat"`
		DestLng    float64         `json:"dest_lng"`
		DestArea   json.RawMessage `json:"dest_area,omitempty"`
		Pins       []*pinExp       `json:"pins"`
		Packing    []packingExp    `json:"packing"`
	}

	rows, err := a.db.Query(`SELECT id, name, color, start_at, end_at, journal, cover_asset, dest_name, dest_lat, dest_lng, dest_area FROM holidays ORDER BY start_at`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	holidays := []*holidayExp{}
	byHoliday := map[int64]*holidayExp{}
	for rows.Next() {
		h := &holidayExp{Pins: []*pinExp{}, Packing: []packingExp{}}
		var box string
		if err := rows.Scan(&h.ID, &h.Name, &h.Color, &h.StartAt, &h.EndAt, &h.Journal, &h.CoverAsset, &h.DestName, &h.DestLat, &h.DestLng, &box); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		if box != "" {
			h.DestArea = json.RawMessage(box)
		}
		holidays = append(holidays, h)
		byHoliday[h.ID] = h
	}

	pinRows, err := a.db.Query(`SELECT id, holiday_id, kind, lat, lng, title, note, country, cover_asset, created_at FROM pins ORDER BY created_at, id`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer pinRows.Close()
	byPin := map[int64]*pinExp{}
	for pinRows.Next() {
		p := &pinExp{Photos: []photoExp{}}
		var hid int64
		if err := pinRows.Scan(&p.ID, &hid, &p.Kind, &p.Lat, &p.Lng, &p.Title, &p.Note, &p.Country, &p.CoverAsset, &p.CreatedAt); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		if h := byHoliday[hid]; h != nil {
			h.Pins = append(h.Pins, p)
		}
		byPin[p.ID] = p
	}

	phRows, err := a.db.Query(`SELECT pin_id, asset_id, taken_at, lat, lng, type, duration FROM pin_photos ORDER BY taken_at`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer phRows.Close()
	for phRows.Next() {
		var pid int64
		var ph photoExp
		if err := phRows.Scan(&pid, &ph.AssetID, &ph.TakenAt, &ph.Lat, &ph.Lng, &ph.Type, &ph.Duration); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		if p := byPin[pid]; p != nil {
			p.Photos = append(p.Photos, ph)
		}
	}

	vnRows, err := a.db.Query(`SELECT pin_id, day, note FROM visit_notes ORDER BY pin_id, day`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer vnRows.Close()
	for vnRows.Next() {
		var pid int64
		var vn dayNote
		if err := vnRows.Scan(&pid, &vn.Day, &vn.Note); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		if p := byPin[pid]; p != nil {
			p.VisitNotes = append(p.VisitNotes, vn)
		}
	}

	packRows, err := a.db.Query(`SELECT holiday_id, label, checked FROM packing_items ORDER BY holiday_id, sort, id`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer packRows.Close()
	for packRows.Next() {
		var hid int64
		var pk packingExp
		if err := packRows.Scan(&hid, &pk.Label, &pk.Checked); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		if h := byHoliday[hid]; h != nil {
			h.Packing = append(h.Packing, pk)
		}
	}

	w.Header().Set("Content-Disposition", `attachment; filename="travel-log-export.json"`)
	writeJSON(w, http.StatusOK, map[string]any{
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"version":     version,
		"holidays":    holidays,
	})
}
