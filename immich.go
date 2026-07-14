package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// clusterCell groups photo GPS points into ~2.2 km grid cells (whole-town granularity).
	clusterCell = 0.02
	// startGrace/endGrace widen the photo query so airport shots just outside
	// the pressed start/end moments still land in the holiday.
	startGrace = 3 * time.Hour
	endGrace   = 6 * time.Hour
	// syncInterval is how stale the active holiday's photos may get before a
	// map load triggers a re-sync.
	syncInterval = 10 * time.Minute
)

type immichClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newImmichClient(baseURL, apiKey string) *immichClient {
	return &immichClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *immichClient) validate() error {
	req, _ := http.NewRequest("GET", c.baseURL+"/api/users/me", nil)
	req.Header.Set("x-api-key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("immich returned %s", resp.Status)
	}
	return nil
}

type immichAsset struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	FileCreatedAt time.Time `json:"fileCreatedAt"`
	ExifInfo      *struct {
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
		City      *string  `json:"city"`
		Country   *string  `json:"country"`
	} `json:"exifInfo"`
}

// searchRange pages through /api/search/metadata for IMAGE assets in [after, before].
func (c *immichClient) searchRange(after, before time.Time) ([]immichAsset, error) {
	var all []immichAsset
	for page := 1; ; page++ {
		body, _ := json.Marshal(map[string]any{
			"takenAfter":  after.UTC().Format(time.RFC3339),
			"takenBefore": before.UTC().Format(time.RFC3339),
			"withExif":    true,
			"type":        "IMAGE",
			"visibility":  "timeline",
			"size":        1000,
			"page":        page,
		})
		req, _ := http.NewRequest("POST", c.baseURL+"/api/search/metadata", bytes.NewReader(body))
		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		var result struct {
			Assets struct {
				Items    []immichAsset `json:"items"`
				NextPage *string       `json:"nextPage"`
			} `json:"assets"`
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode search response: %w", err)
		}
		all = append(all, result.Assets.Items...)
		if result.Assets.NextPage == nil {
			break
		}
	}
	return all, nil
}

// --- albums ---

func (c *immichClient) apiJSON(method, path string, body any, out any) error {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, c.baseURL+path, bytes.NewReader(data))
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("immich %s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *immichClient) createAlbum(name string, assetIDs []string) (string, error) {
	var album struct {
		ID string `json:"id"`
	}
	err := c.apiJSON("POST", "/api/albums", map[string]any{"albumName": name, "assetIds": assetIDs}, &album)
	return album.ID, err
}

func (c *immichClient) addToAlbum(albumID string, assetIDs []string) error {
	return c.apiJSON("PUT", "/api/albums/"+albumID+"/assets", map[string]any{"ids": assetIDs}, nil)
}

func (c *immichClient) renameAlbum(albumID, name string) error {
	return c.apiJSON("PATCH", "/api/albums/"+albumID, map[string]any{"albumName": name}, nil)
}

// syncAlbum mirrors the holiday's photos into an Immich album so trips exist
// natively in the photo app too. Album trouble never fails a sync — log only.
func (a *app) syncAlbum(holidayID int64, assetIDs []string) {
	if len(assetIDs) == 0 {
		return
	}
	var name, albumID string
	if err := a.db.QueryRow(`SELECT name, immich_album_id FROM holidays WHERE id = ?`, holidayID).
		Scan(&name, &albumID); err != nil {
		return
	}
	if albumID != "" {
		err := a.immich.addToAlbum(albumID, assetIDs)
		if err == nil {
			return
		}
		// Album may have been deleted in Immich; fall through and recreate.
		log.Printf("album %s add failed (%v), recreating", albumID, err)
	}
	newID, err := a.immich.createAlbum("Holiday: "+name, assetIDs)
	if err != nil {
		log.Printf("create album for holiday %d: %v", holidayID, err)
		return
	}
	a.db.Exec(`UPDATE holidays SET immich_album_id = ? WHERE id = ?`, newID, holidayID)
}

// --- sync: photos -> clustered pins ---

func clusterKey(lat, lng float64) string {
	return fmt.Sprintf("%d,%d", int(math.Floor(lat/clusterCell)), int(math.Floor(lng/clusterCell)))
}

// neighborKeys returns the photo's own cell key first, then the 8 surrounding
// cells. Attaching to an existing neighbor pin before creating a new one stops
// a town straddling a cell edge from splitting into overlapping pins.
func neighborKeys(lat, lng float64) []string {
	ix, iy := int(math.Floor(lat/clusterCell)), int(math.Floor(lng/clusterCell))
	keys := []string{fmt.Sprintf("%d,%d", ix, iy)}
	for dx := -1; dx <= 1; dx++ {
		for dy := -1; dy <= 1; dy++ {
			if dx != 0 || dy != 0 {
				keys = append(keys, fmt.Sprintf("%d,%d", ix+dx, iy+dy))
			}
		}
	}
	return keys
}

// syncHoliday pulls the holiday's photos from Immich and reconciles photo pins.
// Returns (pins with photos, photos matched).
func (a *app) syncHoliday(holidayID int64) (int, int, error) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()

	var startStr string
	var endStr *string
	err := a.db.QueryRow(`SELECT start_at, end_at FROM holidays WHERE id = ?`, holidayID).Scan(&startStr, &endStr)
	if err != nil {
		return 0, 0, err
	}
	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		return 0, 0, fmt.Errorf("bad start_at: %w", err)
	}
	before := time.Now().UTC().Add(5 * time.Minute)
	if endStr != nil {
		end, err := time.Parse(time.RFC3339, *endStr)
		if err != nil {
			return 0, 0, fmt.Errorf("bad end_at: %w", err)
		}
		before = end.Add(endGrace)
	}

	assets, err := a.immich.searchRange(start.Add(-startGrace), before)
	if err != nil {
		return 0, 0, fmt.Errorf("immich search: %w", err)
	}

	tx, err := a.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	// Existing photo pins by cluster key, so re-syncs and neighbor-cell
	// lookups reuse pins instead of splitting or duplicating them.
	pinByKey := make(map[string]int64)
	{
		rows, err := tx.Query(`
			SELECT cluster_key, id FROM pins
			WHERE holiday_id = ? AND kind = 'photo' AND cluster_key IS NOT NULL`, holidayID)
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			var key string
			var id int64
			if err := rows.Scan(&key, &id); err != nil {
				rows.Close()
				return 0, 0, err
			}
			pinByKey[key] = id
		}
		rows.Close()
	}

	// Photos already attached to this holiday's pins by hand (from the
	// unplaced gallery) have no GPS but must survive the prune below.
	attached := make(map[string]bool)
	{
		rows, err := tx.Query(`
			SELECT pp.asset_id FROM pin_photos pp
			JOIN pins p ON p.id = pp.pin_id WHERE p.holiday_id = ?`, holidayID)
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, 0, err
			}
			attached[id] = true
		}
		rows.Close()
	}

	// Oldest first so cluster growth is deterministic across re-syncs.
	sort.Slice(assets, func(i, j int) bool { return assets[i].FileCreatedAt.Before(assets[j].FileCreatedAt) })

	seen := make(map[string]bool)
	seenUnplaced := make(map[string]bool)
	matched := 0
	for _, asset := range assets {
		e := asset.ExifInfo
		takenAt := asset.FileCreatedAt.UTC().Format(time.RFC3339)
		if e == nil || e.Latitude == nil || e.Longitude == nil || (*e.Latitude == 0 && *e.Longitude == 0) {
			if attached[asset.ID] {
				seen[asset.ID] = true
				matched++
				continue
			}
			// No GPS: keep the photo on the holiday anyway so nothing is lost.
			if _, err := tx.Exec(`
				INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at)
				VALUES (?, ?, ?) ON CONFLICT (asset_id) DO NOTHING`,
				holidayID, asset.ID, takenAt); err != nil {
				return 0, 0, err
			}
			seenUnplaced[asset.ID] = true
			continue
		}
		lat, lng := *e.Latitude, *e.Longitude
		country := ""
		if e.Country != nil {
			country = *e.Country
		}
		var pinID int64
		for _, key := range neighborKeys(lat, lng) {
			if id, ok := pinByKey[key]; ok {
				pinID = id
				break
			}
		}
		if pinID == 0 {
			key := clusterKey(lat, lng)
			title := ""
			if e.City != nil && *e.City != "" {
				title = *e.City
				if country != "" {
					title += ", " + country
				}
			}
			res, err := tx.Exec(`
				INSERT INTO pins (holiday_id, kind, cluster_key, lat, lng, title, country)
				VALUES (?, 'photo', ?, ?, ?, ?, ?)`,
				holidayID, key, lat, lng, title, country)
			if err != nil {
				return 0, 0, err
			}
			pinID, _ = res.LastInsertId()
			pinByKey[key] = pinID
		} else if country != "" {
			// Backfill country onto pins created before it was recorded.
			if _, err := tx.Exec(`UPDATE pins SET country = ? WHERE id = ? AND country = ''`, country, pinID); err != nil {
				return 0, 0, err
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (asset_id) DO NOTHING`,
			pinID, asset.ID, takenAt, lat, lng); err != nil {
			return 0, 0, err
		}
		seen[asset.ID] = true
		matched++
	}

	// Prune unplaced photos that vanished from Immich or gained GPS since.
	{
		rows, err := tx.Query(`SELECT asset_id FROM unplaced_photos WHERE holiday_id = ?`, holidayID)
		if err != nil {
			return 0, 0, err
		}
		var staleUnplaced []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, 0, err
			}
			if !seenUnplaced[id] {
				staleUnplaced = append(staleUnplaced, id)
			}
		}
		rows.Close()
		for _, id := range staleUnplaced {
			if _, err := tx.Exec(`DELETE FROM unplaced_photos WHERE asset_id = ?`, id); err != nil {
				return 0, 0, err
			}
		}
	}

	// Prune photos that disappeared from Immich (deleted/archived there).
	rows, err := tx.Query(`
		SELECT pp.asset_id FROM pin_photos pp
		JOIN pins p ON p.id = pp.pin_id
		WHERE p.holiday_id = ? AND p.kind = 'photo'`, holidayID)
	if err != nil {
		return 0, 0, err
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if !seen[id] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := tx.Exec(`DELETE FROM pin_photos WHERE asset_id = ?`, id); err != nil {
			return 0, 0, err
		}
	}

	// Photo pins sit at the mean of their photos; annotated empty pins survive.
	if _, err := tx.Exec(`
		UPDATE pins SET
			lat = (SELECT AVG(lat) FROM pin_photos WHERE pin_id = pins.id),
			lng = (SELECT AVG(lng) FROM pin_photos WHERE pin_id = pins.id)
		WHERE holiday_id = ? AND kind = 'photo'
		  AND EXISTS (SELECT 1 FROM pin_photos WHERE pin_id = pins.id)`, holidayID); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(`
		DELETE FROM pins WHERE holiday_id = ? AND kind = 'photo' AND note = ''
		  AND NOT EXISTS (SELECT 1 FROM pin_photos WHERE pin_id = pins.id)`, holidayID); err != nil {
		return 0, 0, err
	}

	var pinCount int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM pins WHERE holiday_id = ? AND kind = 'photo'`, holidayID).Scan(&pinCount); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	a.stateMu.Lock()
	a.lastSync[holidayID] = time.Now()
	a.stateMu.Unlock()

	allAssets := make([]string, 0, len(seen)+len(seenUnplaced))
	for id := range seen {
		allAssets = append(allAssets, id)
	}
	for id := range seenUnplaced {
		allAssets = append(allAssets, id)
	}
	a.syncAlbum(holidayID, allAssets)

	return pinCount, matched, nil
}

// maybeSyncActive refreshes the active holiday's photos if they are stale.
// It runs in the background — the map serves the pins it already has instead
// of hanging on Immich — and failures are logged, never surfaced.
func (a *app) maybeSyncActive() {
	var id int64
	err := a.db.QueryRow(`SELECT id FROM holidays WHERE end_at IS NULL`).Scan(&id)
	if err != nil {
		return
	}
	a.stateMu.Lock()
	if a.syncing || time.Since(a.lastSync[id]) < syncInterval {
		a.stateMu.Unlock()
		return
	}
	a.syncing = true
	a.stateMu.Unlock()
	go func() {
		defer func() {
			a.stateMu.Lock()
			a.syncing = false
			a.stateMu.Unlock()
		}()
		if _, _, err := a.syncHoliday(id); err != nil {
			log.Printf("auto-sync holiday %d: %v", id, err)
		}
	}()
}

// --- photo proxy ---

var assetIDRe = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

var photoSizes = map[string]string{
	"thumb":    "/thumbnail?size=thumbnail",
	"preview":  "/thumbnail?size=preview",
	"original": "/original",
}

// handlePhoto streams an Immich image to a logged-in family member. Only
// assets already attached to a pin are reachable — the app never acts as an
// open proxy into the whole Immich library.
func (a *app) handlePhoto(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("asset")
	suffix, ok := photoSizes[r.PathValue("kind")]
	if !ok || !assetIDRe.MatchString(assetID) {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	var exists int
	if err := a.db.QueryRow(`
		SELECT 1 FROM pin_photos WHERE asset_id = ?
		UNION ALL SELECT 1 FROM unplaced_photos WHERE asset_id = ? LIMIT 1`,
		assetID, assetID).Scan(&exists); err != nil {
		httpError(w, http.StatusNotFound, "unknown photo")
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), "GET",
		a.immich.baseURL+"/api/assets/"+assetID+suffix, nil)
	req.Header.Set("x-api-key", a.immich.apiKey)
	resp, err := a.immich.http.Do(req)
	if err != nil {
		httpError(w, http.StatusBadGateway, "photo service unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		httpError(w, http.StatusBadGateway, "photo service error")
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	io.Copy(w, resp.Body)
}
