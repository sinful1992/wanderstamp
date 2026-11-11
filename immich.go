package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WS-6: asking Immich about photos the search left out, before pruning
// (variables so the tests can shorten them).
var (
	assetCheckTimeout  = 10 * time.Second // one photo
	assetCheckDeadline = 60 * time.Second // all of them, inside the manual sync's 2 min write timeout
	assetCheckMax      = 100              // more than this many left out: refuse, ask nothing
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
	// A photo this close to a pin placed by hand is filed on it: the hotel
	// and its grounds.
	manualPinKM = 0.4
	// Out to askKM it may still be the same place — a hotel restaurant photo
	// came in with a GPS fix 600 m off — or the pub down the road. It goes on
	// the pin for now and the trip asks which; the answer is kept.
	askKM = 1.5
)

type immichClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
	stream  *http.Client // video playback: no overall timeout, ends with the request
}

func newImmichClient(baseURL, apiKey string) *immichClient {
	return &immichClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 60 * time.Second, Transport: immichTransport{http.DefaultTransport}},
		stream:  &http.Client{Transport: immichTransport{http.DefaultTransport}},
	}
}

// immichTransport logs every failed Immich call (R5) and retries once when
// the name doesn't resolve yet: after a reboot holiday-map can be up before
// Docker's DNS knows immich-server (seen 10-19). A DNS failure means nothing
// was sent, so even a POST is safe to send again.
type immichTransport struct{ next http.RoundTripper }

func (t immichTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	var dnsErr *net.DNSError
	if err != nil && errors.As(err, &dnsErr) {
		select {
		case <-time.After(2 * time.Second):
		case <-req.Context().Done():
			return nil, err
		}
		retry := req.Clone(req.Context())
		if req.GetBody != nil {
			if retry.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
		resp, err = t.next.RoundTrip(retry)
	}
	ms := time.Since(start).Milliseconds()
	switch {
	case err != nil:
		log.Printf("immich %s %s failed after %d ms: %v", req.Method, req.URL.Path, ms, err)
	case resp.StatusCode >= 400:
		log.Printf("immich %s %s: %d in %d ms", req.Method, req.URL.Path, resp.StatusCode, ms)
	}
	return resp, err
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
	Duration      string    `json:"duration"` // "0:00:12.345678" for a video
	FileCreatedAt time.Time `json:"fileCreatedAt"`
	ExifInfo      *struct {
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
		City      *string  `json:"city"`
		Country   *string  `json:"country"`
	} `json:"exifInfo"`
}

// mediaType is the stored type of an asset: "video" or "image".
func (a immichAsset) mediaType() string {
	if a.Type == "VIDEO" {
		return "video"
	}
	return "image"
}

// seconds turns Immich's "H:MM:SS.ffffff" duration into seconds; 0 for an
// image or anything it can't read.
func (a immichAsset) seconds() float64 {
	if a.Type != "VIDEO" {
		return 0
	}
	parts := strings.Split(strings.TrimSpace(a.Duration), ":")
	if len(parts) == 0 || len(parts) > 3 {
		return 0
	}
	total := 0.0
	for _, part := range parts {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil || strings.HasPrefix(part, "-") || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
			return 0
		}
		total = total*60 + v
	}
	return math.Round(total*10) / 10
}

// searchRange pages through /api/search/metadata for the photos and videos
// (F7) in [after, before]. Live-photo motion clips are hidden in Immich, so
// visibility "timeline" leaves them out; any other type (audio) is dropped.
func (c *immichClient) searchRange(after, before time.Time) ([]immichAsset, error) {
	var all []immichAsset
	for page := 1; ; page++ {
		body, _ := json.Marshal(map[string]any{
			"takenAfter":  after.UTC().Format(time.RFC3339),
			"takenBefore": before.UTC().Format(time.RFC3339),
			"withExif":    true,
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
		// Immich error bodies are JSON too ({"message":...}) and decode cleanly
		// into an empty page — which the sync would read as "every photo is
		// gone" and prune the whole trip. Any non-200 page fails the search.
		if resp.StatusCode != http.StatusOK {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			resp.Body.Close()
			return nil, fmt.Errorf("immich search page %d: %s: %s", page, resp.Status, strings.TrimSpace(string(snippet)))
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
		for _, it := range result.Assets.Items {
			if it.Type == "IMAGE" || it.Type == "VIDEO" {
				all = append(all, it)
			}
		}
		if result.Assets.NextPage == nil {
			break
		}
	}
	return all, nil
}

// assetGone asks Immich about one photo the search did not return (WS-6).
// It is gone when Immich says it does not have it (404, or a 400 whose body
// says "not found", as albumGone reads albums), it is in the trash, it is no
// longer on the timeline (archived, hidden, locked), or it is now dated
// outside [after, before] (both ends inclusive, as Immich's search is). An
// offline photo (its library cannot be reached) is never gone, trashed or
// not: the files come back with the library. Anything else, including a
// photo Immich has but the search missed, is kept. Any other answer about this
// one photo (a 500, a 400 that is not "not found", an unreadable body) is an
// *assetAnswerError: the caller keeps that photo unchecked. A transport failure
// (no answer, a time limit) is a plain error, so the sync prunes nothing. Each
// call has its own short time limit.
// assetAnswerError: Immich answered about one photo, but not in a way that says
// whether it is gone (B3 of task 142, ada #10301). That photo is kept unchecked
// this time; a lasting answer like this (a corrupt file's 500) must not stop the
// trip's syncs, and so its new photos, for good.
type assetAnswerError struct{ msg string }

func (e *assetAnswerError) Error() string { return e.msg }

func (c *immichClient) assetGone(ctx context.Context, id string, after, before time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, assetCheckTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/assets/"+url.PathEscape(id), nil)
	req.Header.Set("x-api-key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// Immich's own 404 is JSON; an HTML page (a proxy, or a moved route) says nothing about this photo
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		if t := strings.TrimSpace(string(body)); t == "" || strings.HasPrefix(t, "{") {
			return true, nil
		}
		return false, &assetAnswerError{fmt.Sprintf("immich asset %s: 404 that is not Immich's (%.60q)", id, strings.TrimSpace(string(body)))}
	case http.StatusBadRequest:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		if strings.Contains(strings.ToLower(string(body)), "not found") {
			return true, nil
		}
		return false, &assetAnswerError{fmt.Sprintf("immich asset %s: 400: %s", id, strings.TrimSpace(string(body)))}
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return false, &assetAnswerError{fmt.Sprintf("immich asset %s: %s: %s", id, resp.Status, strings.TrimSpace(string(snippet)))}
	}
	var a struct {
		ID            string `json:"id"`
		IsTrashed     bool   `json:"isTrashed"`
		IsOffline     bool   `json:"isOffline"`
		Visibility    string `json:"visibility"`
		FileCreatedAt string `json:"fileCreatedAt"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&a); err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("decode asset %s: %w", id, err) // cut off by the time limit: transport
		}
		return false, &assetAnswerError{fmt.Sprintf("decode asset %s: %v", id, err)}
	}
	if a.ID != id {
		return false, &assetAnswerError{fmt.Sprintf("immich asset %s: answer is for %q", id, a.ID)}
	}
	if a.IsOffline {
		return false, nil
	}
	if a.IsTrashed || (a.Visibility != "" && a.Visibility != "timeline") {
		return true, nil
	}
	if t, err := time.Parse(time.RFC3339, a.FileCreatedAt); err == nil && (t.Before(after) || t.After(before)) {
		return true, nil
	}
	return false, nil
}

// --- albums ---

type immichError struct {
	status int
	msg    string
}

func (e *immichError) Error() string { return e.msg }

// albumGone reports whether Immich said the album doesn't exist (it answers
// 400 "Album not found", verified on 3.2.2; 404 is accepted too). Anything
// else — a restart, a 5xx, a timeout — is transient, and recreating the album
// then would leave a duplicate "Holiday: …" album behind every hiccup.
func albumGone(err error) bool {
	var ie *immichError
	return errors.As(err, &ie) && (ie.status == http.StatusNotFound ||
		(ie.status == http.StatusBadRequest && strings.Contains(strings.ToLower(ie.msg), "not found")))
}

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
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return &immichError{status: resp.StatusCode, msg: fmt.Sprintf("immich %s %s: %s: %s",
			method, path, resp.Status, strings.TrimSpace(string(msg)))}
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
		if !albumGone(err) {
			log.Printf("album %s add failed, will retry next sync: %v", albumID, err)
			return
		}
		// Deleted in Immich; recreate it.
		log.Printf("album %s is gone (%v), recreating", albumID, err)
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
	var planned bool
	err := a.db.QueryRow(`SELECT start_at, end_at, planned FROM holidays WHERE id = ?`, holidayID).Scan(&startStr, &endStr, &planned)
	if err != nil {
		return 0, 0, err
	}
	if planned {
		// nothing has been taken yet, and its window would run backwards
		return 0, 0, nil
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

	// A trip that already has photos coming back empty is far likelier to be
	// Immich answering for the wrong account, or a changed response shape,
	// than every photo being deleted at once — so refuse rather than prune.
	// Someone who really did delete them all can remove the trip's pins by hand.
	if len(assets) == 0 {
		var have int
		a.db.QueryRow(`
			SELECT (SELECT COUNT(*) FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id
			        WHERE p.holiday_id = ?)
			     + (SELECT COUNT(*) FROM unplaced_photos WHERE holiday_id = ?)`,
			holidayID, holidayID).Scan(&have)
		if have > 0 {
			return 0, 0, fmt.Errorf("immich returned no photos for a trip that has %d; not pruning", have)
		}
	}

	// WS-6: a photo the trip has but the search did not return is pruned only
	// if Immich, asked about that photo, says it is gone. A partial answer (a
	// library offline, an index being rebuilt) then keeps pins and titles.
	// Asked before the transaction, so no database lock waits on Immich.
	present := make(map[string]bool)
	{
		returned := make(map[string]bool, len(assets))
		for _, as := range assets {
			returned[as.ID] = true
		}
		rows, err := a.db.Query(`
			SELECT pp.asset_id FROM pin_photos pp JOIN pins p ON p.id = pp.pin_id WHERE p.holiday_id = ?
			UNION SELECT asset_id FROM unplaced_photos WHERE holiday_id = ?`, holidayID, holidayID)
		if err != nil {
			return 0, 0, err
		}
		var missing []string
		known := 0
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, 0, err
			}
			known++
			if !returned[id] {
				missing = append(missing, id)
			}
		}
		rows.Close()
		// keepAll: prune nothing this time, but still add what the search found,
		// so a lasting condition never stops the trip's new photos (ada #10282).
		keepAll := func(why string) {
			log.Printf("sync holiday %d: %s; pruning nothing this time", holidayID, why)
			for _, id := range missing {
				present[id] = true
			}
		}
		// Bounded: a search that keeps leaving many photos out (a wrong key,
		// an index being rebuilt) is not asked about one by one forever.
		if len(missing) > assetCheckMax {
			keepAll(fmt.Sprintf("the search left out %d of the trip's photos (more than %d to check)", len(missing), assetCheckMax))
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), assetCheckDeadline)
			defer cancel()
			gone := 0
			for _, id := range missing {
				g, err := a.immich.assetGone(ctx, id, start.Add(-startGrace), before)
				var odd *assetAnswerError
				if errors.As(err, &odd) {
					// an answer about this photo that says neither yes nor no: keep it, check it next time
					log.Printf("sync holiday %d: %v; kept unchecked", holidayID, err)
					present[id] = true
					continue
				}
				if err != nil {
					// transport failure (no answer, the time limit): transient, so stop and change nothing
					return 0, 0, fmt.Errorf("immich check before pruning: %w", err)
				}
				if g {
					gone++
				} else {
					present[id] = true
				}
			}
			// More than half of a trip gone in one sync is far likelier a key for
			// another account (every photo "not found") than a real clear-out.
			// A real one is removed by hand.
			if gone >= 2 && gone*2 > known {
				keepAll(fmt.Sprintf("Immich says %d of the trip's %d photos are gone", gone, known))
			}
		}
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

	// Pins placed by hand claim the photos taken at them, so a hotel pin
	// holds the hotel photos instead of a town photo pin appearing beside it.
	type spot struct {
		id       int64
		lat, lng float64
	}
	var manual []spot
	{
		rows, err := tx.Query(`SELECT id, lat, lng FROM pins WHERE holiday_id = ? AND kind = 'manual'`, holidayID)
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			var m spot
			if err := rows.Scan(&m.id, &m.lat, &m.lng); err != nil {
				rows.Close()
				return 0, 0, err
			}
			manual = append(manual, m)
		}
		rows.Close()
	}
	nearestManual := func(lat, lng float64) (int64, float64) {
		var id int64
		best := askKM
		for _, m := range manual {
			if d := haversineKM(lat, lng, m.lat, m.lng); d <= best {
				id, best = m.id, d
			}
		}
		return id, best
	}
	isManual := func(id int64) bool {
		for _, m := range manual {
			if m.id == id {
				return true
			}
		}
		return false
	}

	// What's already been answered: the chosen hand pin, or 0 for its own stop.
	choices := make(map[string]int64)
	{
		rows, err := tx.Query(`SELECT asset_id, pin_id FROM photo_choices WHERE holiday_id = ?`, holidayID)
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			var id string
			var pin int64
			if err := rows.Scan(&id, &pin); err != nil {
				rows.Close()
				return 0, 0, err
			}
			choices[id] = pin
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

	// Photos set aside by hand stay off the trip. Skipped before anything
	// else, so the prunes below also clear any copy still filed.
	aside := make(map[string]bool)
	{
		rows, err := tx.Query(`SELECT asset_id FROM set_aside WHERE holiday_id = ?`, holidayID)
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, 0, err
			}
			aside[id] = true
		}
		rows.Close()
	}

	// Oldest first so cluster growth is deterministic across re-syncs.
	sort.Slice(assets, func(i, j int) bool { return assets[i].FileCreatedAt.Before(assets[j].FileCreatedAt) })

	seen := make(map[string]bool)
	seenUnplaced := make(map[string]bool)
	matched := 0
	for _, asset := range assets {
		if aside[asset.ID] {
			continue
		}
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
				INSERT INTO unplaced_photos (holiday_id, asset_id, taken_at, type, duration)
				VALUES (?, ?, ?, ?, ?) ON CONFLICT (asset_id) DO UPDATE SET
				  type = excluded.type, duration = excluded.duration
				WHERE unplaced_photos.holiday_id = excluded.holiday_id`,
				holidayID, asset.ID, takenAt, asset.mediaType(), asset.seconds()); err != nil {
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
		ask := false
		if choice, ok := choices[asset.ID]; ok && (choice == 0 || isManual(choice)) {
			pinID = choice // 0: its own stop, so straight to clustering
		} else {
			// unanswered, or the pin it was answered for has since gone
			var d float64
			pinID, d = nearestManual(lat, lng)
			ask = pinID != 0 && d > manualPinKM
		}
		for _, key := range neighborKeys(lat, lng) {
			if pinID != 0 {
				break
			}
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
			INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng, ask, type, duration)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (asset_id) DO UPDATE SET pin_id = excluded.pin_id, ask = excluded.ask,
			  type = excluded.type, duration = excluded.duration
			WHERE pin_photos.pin_id IN (SELECT id FROM pins WHERE holiday_id = ?)`,
			pinID, asset.ID, takenAt, lat, lng, ask, asset.mediaType(), asset.seconds(), holidayID); err != nil {
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
			if !seenUnplaced[id] && !present[id] {
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
		WHERE p.holiday_id = ?`, holidayID)
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
		if !seen[id] && !present[id] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := tx.Exec(`DELETE FROM pin_photos WHERE asset_id = ?`, id); err != nil {
			return 0, 0, err
		}
	}
	// an answer about a photo that's gone is nothing to keep
	if _, err := tx.Exec(`
		DELETE FROM photo_choices WHERE holiday_id = ?
		  AND asset_id NOT IN (SELECT pp.asset_id FROM pin_photos pp
		                       JOIN pins p ON p.id = pp.pin_id WHERE p.holiday_id = ?)`,
		holidayID, holidayID); err != nil {
		return 0, 0, err
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
		  AND NOT EXISTS (SELECT 1 FROM visit_notes WHERE pin_id = pins.id)
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
// of hanging on Immich — and failures are logged, never surfaced. It reports
// whether a sync is running, so the page that asked can wait for it
// (GET /api/sync/wait) and pick up what it found.
func (a *app) maybeSyncActive() bool {
	var id int64
	err := a.db.QueryRow(`SELECT id FROM holidays WHERE end_at IS NULL AND planned = 0`).Scan(&id)
	if err != nil {
		return false
	}
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.syncDone != nil {
		return true
	}
	if time.Since(a.lastSync[id]) < syncInterval {
		return false
	}
	done := make(chan struct{})
	a.syncDone = done
	go func() {
		_, _, err := a.syncHoliday(id)
		if err != nil {
			log.Printf("auto-sync holiday %d: %v", id, err)
		}
		a.stateMu.Lock()
		a.syncOK = err == nil
		a.syncDone = nil
		a.stateMu.Unlock()
		close(done)
	}()
	return true
}

// syncWaitLimit caps how long GET /api/sync/wait holds a request open; it
// stays well inside the server's WriteTimeout.
var syncWaitLimit = 90 * time.Second

// handleSyncWait answers once the running background sync has finished.
// ok=false (it failed, or is still going after syncWaitLimit) tells the page
// not to reload — the pins it has are still the latest.
func (a *app) handleSyncWait(w http.ResponseWriter, r *http.Request) {
	a.stateMu.Lock()
	done := a.syncDone
	a.stateMu.Unlock()
	if done == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": false})
		return
	}
	var ok bool
	select {
	case <-done:
		a.stateMu.Lock()
		ok = a.syncOK
		a.stateMu.Unlock()
	case <-time.After(syncWaitLimit):
		ok = false
	case <-r.Context().Done():
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": ok})
}

// --- photo proxy ---

var assetIDRe = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

var photoSizes = map[string]string{
	"thumb":    "/thumbnail?size=thumbnail",
	"preview":  "/thumbnail?size=preview",
	"original": "/original",
	"video":    "/video/playback", // F7: Immich's transcoded, seekable copy
}

// videoWriteWindow is how long one video response may keep writing.
const videoWriteWindow = time.Hour

// handlePhoto streams an Immich image to a logged-in family member. Only
// assets already attached to a pin are reachable — the app never acts as an
// open proxy into the whole Immich library.
func (a *app) handlePhoto(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("asset")
	if !assetIDRe.MatchString(assetID) {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	var exists int
	if err := a.reader().QueryRow(`
		SELECT 1 FROM pin_photos WHERE asset_id = ?
		UNION ALL SELECT 1 FROM unplaced_photos WHERE asset_id = ?
		UNION ALL SELECT 1 FROM set_aside WHERE asset_id = ? LIMIT 1`,
		assetID, assetID, assetID).Scan(&exists); err != nil {
		httpError(w, http.StatusNotFound, "unknown photo")
		return
	}
	if r.PathValue("kind") == "video" && !a.isVideo(assetID) {
		httpError(w, http.StatusNotFound, "not a video")
		return
	}
	// an asset id never changes what it shows, so the browser keeps it
	a.streamPhoto(w, r, assetID, "private, max-age=31536000, immutable")
}

// isVideo reports whether a filed asset is a video (F7).
func (a *app) isVideo(assetID string) bool {
	var n int
	a.reader().QueryRow(`
		SELECT COUNT(*) FROM (
		  SELECT 1 FROM pin_photos WHERE asset_id = ? AND type = 'video'
		  UNION ALL SELECT 1 FROM unplaced_photos WHERE asset_id = ? AND type = 'video')`,
		assetID, assetID).Scan(&n)
	return n > 0
}

// streamPhoto proxies one Immich asset. Callers must have authorized the
// asset already — this only translates kind → Immich URL and streams.
// kind "video" (F7) is the transcoded playback: Range passes through both
// ways so the browser can seek, and there is no 60 s client timeout.
func (a *app) streamPhoto(w http.ResponseWriter, r *http.Request, assetID, cacheControl string) {
	kind := r.PathValue("kind")
	suffix, ok := photoSizes[kind]
	if !ok {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	video := kind == "video"
	req, _ := http.NewRequestWithContext(r.Context(), "GET",
		a.immich.baseURL+"/api/assets/"+assetID+suffix, nil)
	req.Header.Set("x-api-key", a.immich.apiKey)
	client := a.immich.http
	if video {
		for _, h := range []string{"Range", "If-Range"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		client = a.immich.stream
		// a film plays for longer than the server's 2 min write timeout
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(videoWriteWindow)); err != nil {
			log.Printf("video %s: can't extend the write deadline: %v", assetID, err)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		httpError(w, http.StatusBadGateway, "photo service unavailable")
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case video && resp.StatusCode == http.StatusPartialContent:
	case video && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			w.Header().Set("Content-Range", cr)
		}
		w.WriteHeader(resp.StatusCode)
		return
	default:
		httpError(w, http.StatusBadGateway, "photo service error")
		return
	}
	headers := []string{"Content-Type", "Content-Length"}
	if video {
		headers = append(headers, "Content-Range", "Accept-Ranges", "ETag", "Last-Modified")
	}
	for _, h := range headers {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Cache-Control", cacheControl)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
