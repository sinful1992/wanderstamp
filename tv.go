package main

// Wanderstamp on the TV (task 133): a second, read-only listener for the home
// LAN ("the door"), so a TV that isn't on the tailnet can show the trips.
//
// The door serves only the /tv page, the static files it shares with the app
// (Leaflet, the stamp font, icons), pairing and the TV reads below. No login,
// no admin, no /api/* of the main app, no originals or video.
//
// Pairing: the TV page asks the door for a 4-letter code and long-polls for
// the result; someone signed in to the app types the code; the server makes a
// device token, stored hashed in tv_devices, and the TV's next poll collects
// it once as an HttpOnly cookie. The token reads every trip that isn't
// planned, previews only, until the TV is unpaired. It is its own token type:
// pairing never touches share links (making a share replaces the old one).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	tvCookie       = "ws_tv"
	tvCookieMaxAge = 34560000                   // 400 days, the most a browser keeps a cookie
	tvCodeLetters  = "ABCDEFGHJKLMNPQRSTUVWXYZ" // no I or O: they read as 1 and 0
	tvCodeTTL      = 5 * time.Minute
	tvMaxOpen      = 20 // open codes, all devices together
	tvMaxOpenPeer  = 3  // open codes one device may hold, so it can't take every slot
	tvMaxNewPeer   = 10 // codes one device may ask for per minute
	tvMaxWrong     = 10 // wrong codes one account may type per tvWrongWindow
	tvWrongWindow  = 15 * time.Minute
	tvNameMax      = 40
	tvSeenEvery    = time.Minute
	tvOrphanAfter  = 15 * time.Minute // a device row whose token was never collected
)

// tvPollWait is how long GET /tv/pair/{poll} holds the request open; a var so
// tests can shorten it.
var tvPollWait = 30 * time.Second

type tvPairing struct {
	code, poll, peer string
	expires          time.Time
	bound            chan struct{} // closed once a signed-in phone typed the code
	token, name      string        // set when bound
}

type tvPairs struct {
	mu     sync.Mutex
	byCode map[string]*tvPairing
	byPoll map[string]*tvPairing
	asked  map[string][]time.Time // per peer: when it asked for a code
	wrong  map[int64][]time.Time  // per account: when it typed a wrong code
}

var tvInitMu sync.Mutex

// pairs is this app's pairing table, made on first use so tests' bare
// &app{db: ...} work.
func (a *app) pairs() *tvPairs {
	tvInitMu.Lock()
	defer tvInitMu.Unlock()
	if a.tv == nil {
		a.tv = newTVPairs()
	}
	return a.tv
}

func newTVPairs() *tvPairs {
	return &tvPairs{
		byCode: map[string]*tvPairing{},
		byPoll: map[string]*tvPairing{},
		asked:  map[string][]time.Time{},
		wrong:  map[int64][]time.Time{},
	}
}

func within(ts []time.Time, d time.Duration, now time.Time) []time.Time {
	kept := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < d {
			kept = append(kept, t)
		}
	}
	return kept
}

// gc drops expired codes and old counters; the caller holds mu.
func (p *tvPairs) gc(now time.Time) {
	for c, pr := range p.byCode {
		if !now.Before(pr.expires) {
			delete(p.byCode, c)
			delete(p.byPoll, pr.poll)
		}
	}
	for k, ts := range p.asked {
		if ts = within(ts, time.Minute, now); len(ts) == 0 {
			delete(p.asked, k)
		} else {
			p.asked[k] = ts
		}
	}
	for k, ts := range p.wrong {
		if ts = within(ts, tvWrongWindow, now); len(ts) == 0 {
			delete(p.wrong, k)
		} else {
			p.wrong[k] = ts
		}
	}
}

// doorPeer is the device a door request came from: its own address only.
// The door is never behind a proxy, so X-Forwarded-For is never believed
// (clientIP trusts it from the container gateway, which a LAN device could
// look like through docker's userland proxy).
func doorPeer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func randomCode() (string, error) {
	var b strings.Builder
	max := big.NewInt(int64(len(tvCodeLetters)))
	for range 4 {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(tvCodeLetters[n.Int64()])
	}
	return b.String(), nil
}

// POST /tv/pair (door, no auth): a fresh code for the pairing screen.
func (a *app) handleTVPairStart(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		httpError(w, http.StatusForbidden, "cross-site request")
		return
	}
	peer := doorPeer(r)
	p := a.pairs()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.gc(now)
	if len(p.asked[peer]) >= tvMaxNewPeer {
		w.Header().Set("Retry-After", "60")
		httpError(w, http.StatusTooManyRequests, "too many codes, wait a minute")
		return
	}
	// A device asking again (the TV app relaunched, the page reloaded) has no
	// use for its older codes: its oldest one not yet typed in makes room, so
	// a TV restarted a few times never waits minutes for a code.
	mine := 0
	var oldest *tvPairing
	for _, pr := range p.byCode {
		if pr.peer != peer {
			continue
		}
		mine++
		if pr.token == "" && (oldest == nil || pr.expires.Before(oldest.expires)) {
			oldest = pr
		}
	}
	if mine >= tvMaxOpenPeer && oldest != nil {
		delete(p.byCode, oldest.code)
		delete(p.byPoll, oldest.poll)
		mine--
	}
	if mine >= tvMaxOpenPeer || len(p.byCode) >= tvMaxOpen {
		w.Header().Set("Retry-After", "60")
		httpError(w, http.StatusTooManyRequests, "too many open codes, wait a minute")
		return
	}
	var code string
	for range 20 {
		c, err := randomCode()
		if err != nil {
			httpError(w, http.StatusInternalServerError, "entropy error")
			return
		}
		if p.byCode[c] == nil {
			code = c
			break
		}
	}
	poll, err := randomHex(24)
	if code == "" || err != nil {
		httpError(w, http.StatusInternalServerError, "entropy error")
		return
	}
	pr := &tvPairing{code: code, poll: poll, peer: peer, expires: now.Add(tvCodeTTL), bound: make(chan struct{})}
	p.byCode[code] = pr
	p.byPoll[poll] = pr
	p.asked[peer] = append(p.asked[peer], now)
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "poll": poll, "expires_in": int(tvCodeTTL.Seconds())})
}

// collect hands out a bound pairing's token exactly once; the caller holds mu.
// ok false means the poll is unknown, expired or already used.
func (p *tvPairs) collect(poll string, now time.Time) (pr *tvPairing, ready, ok bool) {
	pr = p.byPoll[poll]
	if pr == nil || !now.Before(pr.expires) {
		return nil, false, false
	}
	if pr.token == "" {
		return pr, false, true
	}
	delete(p.byPoll, poll)
	delete(p.byCode, pr.code)
	return pr, true, true
}

func (a *app) tvCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     tvCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   tvCookieMaxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// the door is plain HTTP on the home LAN, so never Secure
	}
}

// GET /tv/pair/{poll} (door, no auth): waits up to tvPollWait for the code to
// be typed. 200 + cookie once, 202 still waiting, 410 gone.
func (a *app) handleTVPairPoll(w http.ResponseWriter, r *http.Request) {
	poll := r.PathValue("poll")
	p := a.pairs()
	deadline := time.NewTimer(tvPollWait)
	defer deadline.Stop()
	for {
		p.mu.Lock()
		pr, ready, ok := p.collect(poll, time.Now())
		p.mu.Unlock()
		if !ok {
			httpError(w, http.StatusGone, "this code has expired, a new one is coming")
			return
		}
		if ready {
			now := time.Now().UTC().Format(time.RFC3339)
			// collected: the TV is real, so the row is no orphan any more
			a.db.Exec(`UPDATE tv_devices SET last_seen = ? WHERE token_hash = ?`, now, hashToken(pr.token))
			http.SetCookie(w, a.tvCookie(pr.token))
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, map[string]string{"name": pr.name})
			return
		}
		expiry := time.NewTimer(time.Until(pr.expires))
		select {
		case <-pr.bound:
			expiry.Stop()
			continue
		case <-expiry.C:
			continue // the loop answers 410
		case <-deadline.C:
			expiry.Stop()
			writeJSON(w, http.StatusAccepted, map[string]bool{"pending": true})
			return
		case <-r.Context().Done():
			expiry.Stop()
			return
		}
	}
}

// tvName is a TV's name as typed: trimmed, 1-40 characters, no control
// characters.
func tvName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n < 1 || n > tvNameMax || !utf8.ValidString(s) {
		return "", false
	}
	for _, c := range s {
		if unicode.IsControl(c) || c == ' ' || c == ' ' {
			return "", false
		}
	}
	return s, true
}

// POST /api/tv/pair (main listener, signed in): binds a code to a new TV.
func (a *app) handleTVPair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	u := r.Context().Value(userKey).(sessionUser)
	name, ok := tvName(req.Name)
	if !ok {
		httpError(w, http.StatusBadRequest, "name the TV, 1 to 40 characters")
		return
	}
	code := strings.ToUpper(strings.Join(strings.Fields(req.Code), ""))
	p := a.pairs()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.gc(now)
	if len(p.wrong[u.ID]) >= tvMaxWrong {
		w.Header().Set("Retry-After", "900")
		httpError(w, http.StatusTooManyRequests, "too many wrong codes, try again later")
		return
	}
	pr := p.byCode[code]
	if pr == nil || pr.token != "" || !now.Before(pr.expires) {
		p.wrong[u.ID] = append(p.wrong[u.ID], now)
		httpError(w, http.StatusNotFound, "no TV is showing that code")
		return
	}
	token, err := randomHex(32)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "entropy error")
		return
	}
	res, err := a.db.Exec(`INSERT INTO tv_devices (name, token_hash, created_by) VALUES (?, ?, ?)`,
		name, hashToken(token), u.Username)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	id, _ := res.LastInsertId()
	pr.token, pr.name = token, name
	close(pr.bound)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": name})
}

type tvDeviceOut struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	LastSeen  string `json:"last_seen"`
}

// GET /api/tv/devices (main listener, signed in): the paired TVs.
func (a *app) handleTVDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := a.reader().Query(`SELECT id, name, created_by, created_at, last_seen FROM tv_devices ORDER BY id`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []tvDeviceOut{}
	for rows.Next() {
		var d tvDeviceOut
		if err := rows.Scan(&d.ID, &d.Name, &d.CreatedBy, &d.CreatedAt, &d.LastSeen); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, d)
	}
	if rows.Err() != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /api/tv/devices/{id} (main listener, signed in): unpair a TV. Its
// next request gets 401 and the page goes back to the code screen.
func (a *app) handleTVUnpair(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	res, err := a.db.Exec(`DELETE FROM tv_devices WHERE id = ?`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, http.StatusNotFound, "no such TV")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type tvDevice struct {
	ID   int64
	Name string
}

const tvKey ctxKey = 1

// tvAuth wraps a door read: the ws_tv cookie or a Bearer token must belong to
// a paired TV.
func (a *app) tvAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			token = strings.TrimSpace(h[7:])
		} else if c, err := r.Cookie(tvCookie); err == nil {
			token = c.Value
		}
		if token == "" {
			httpError(w, http.StatusUnauthorized, "not paired")
			return
		}
		var d tvDevice
		var seen string
		err := a.db.QueryRow(`SELECT id, name, last_seen FROM tv_devices WHERE token_hash = ?`,
			hashToken(token)).Scan(&d.ID, &d.Name, &seen)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "not paired")
			return
		}
		if t, err := time.Parse(time.RFC3339, seen); err != nil || time.Since(t) >= tvSeenEvery {
			a.db.Exec(`UPDATE tv_devices SET last_seen = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), d.ID)
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r.WithContext(context.WithValue(r.Context(), tvKey, d)))
	}
}

func (a *app) handleTVMe(w http.ResponseWriter, r *http.Request) {
	d := r.Context().Value(tvKey).(tvDevice)
	writeJSON(w, http.StatusOK, map[string]string{"name": d.Name})
}

// tvTripIDs are the trips a TV may see: every one that isn't planned.
func (a *app) tvTripIDs() ([]int64, error) {
	a.promotePlanned()
	rows, err := a.reader().Query(`SELECT id FROM holidays WHERE planned = 0 ORDER BY start_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// tvTrip is loadSharedTrip for a TV: the share shape, and a planned trip is
// no trip at all.
func (a *app) tvTrip(id int64) (*sharedTrip, error) {
	t, err := a.loadSharedTrip(id)
	if err != nil {
		return nil, err
	}
	if t.holiday.Planned {
		return nil, errNoTrip
	}
	return t, nil
}

func (a *app) handleTVTrips(w http.ResponseWriter, r *http.Request) {
	ids, err := a.tvTripIDs()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	out := []holidayOut{}
	for _, id := range ids {
		t, err := a.tvTrip(id)
		if err == errNoTrip {
			continue // deleted or planned between the two reads
		}
		if err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, t.holiday)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleTVTrip(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	t, err := a.tvTrip(id)
	if err == errNoTrip {
		httpError(w, http.StatusNotFound, "no such trip")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	rows, err := a.reader().Query(`
		SELECT pp.asset_id, pp.pin_id, pp.taken_at, pp.type, pp.duration FROM pin_photos pp
		JOIN pins p ON p.id = pp.pin_id
		WHERE p.holiday_id = ? ORDER BY pp.taken_at, pp.asset_id`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	type tvPhoto struct {
		AssetID  string  `json:"asset_id"`
		PinID    int64   `json:"pin_id"`
		TakenAt  string  `json:"taken_at"`
		Type     string  `json:"type"`
		Duration float64 `json:"duration"`
	}
	photos := []tvPhoto{}
	for rows.Next() {
		var p tvPhoto
		if err := rows.Scan(&p.AssetID, &p.PinID, &p.TakenAt, &p.Type, &p.Duration); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		// only photos on the pins the trip shows (way-there photo pins are left out)
		if pin, ok := t.assets[p.AssetID]; ok && pin == p.PinID {
			photos = append(photos, p)
		}
	}
	if rows.Err() != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"holiday": t.holiday, "pins": t.pins, "photos": photos})
}

// GET /api/tv/photo/{asset}/{kind}: a preview or thumbnail of a photo on a
// shown pin of a trip that isn't planned. Never the original (GPS, EXIF) and
// never the video (Immich's playback can be the original file).
func (a *app) handleTVPhoto(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("asset")
	if !assetIDRe.MatchString(assetID) {
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	switch r.PathValue("kind") {
	case "thumb", "preview":
	case "original", "video":
		httpError(w, http.StatusForbidden, "not available on the TV")
		return
	default:
		httpError(w, http.StatusBadRequest, "bad request")
		return
	}
	// only the trips that hold this asset are loaded, not every trip
	rows, err := a.reader().Query(`
		SELECT DISTINCT p.holiday_id FROM pin_photos pp
		JOIN pins p ON p.id = pp.pin_id
		JOIN holidays h ON h.id = p.holiday_id
		WHERE pp.asset_id = ? AND h.planned = 0`, assetID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	var trips []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			trips = append(trips, id)
		}
	}
	rows.Close()
	for _, id := range trips {
		t, err := a.tvTrip(id)
		if err != nil {
			continue
		}
		if _, ok := t.assets[assetID]; ok {
			a.streamPhoto(w, r, assetID, "private, max-age=86400")
			return
		}
	}
	httpError(w, http.StatusNotFound, "unknown photo")
}

// tvHousekeeping drops expired codes and device rows whose token no TV ever
// collected (the TV was switched off mid-pairing, or the server restarted).
func (a *app) tvHousekeeping() {
	p := a.pairs()
	p.mu.Lock()
	p.gc(time.Now())
	p.mu.Unlock()
	a.db.Exec(`DELETE FROM tv_devices WHERE last_seen = '' AND created_at < ?`,
		time.Now().UTC().Add(-tvOrphanAfter).Format(time.RFC3339))
}

// tvRoutes are the main listener's TV management calls (signed in).
func (a *app) tvRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/tv/pair", a.auth(a.handleTVPair))
	mux.HandleFunc("GET /api/tv/devices", a.auth(a.handleTVDevices))
	mux.HandleFunc("DELETE /api/tv/devices/{id}", a.auth(a.handleTVUnpair))
}

// doorStatic is the only static the door serves: the /tv page and what it
// shares with the app. Everything else, index.html and app.js included, 404s.
func doorStatic(static http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/tv" || p == "/tv/":
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/tv/index.html"
			static.ServeHTTP(w, r2)
		case strings.HasPrefix(p, "/tv/"), strings.HasPrefix(p, "/leaflet/"), strings.HasPrefix(p, "/icons/"),
			p == "/staatliches-latin.woff2", p == "/staatliches-latin-ext.woff2":
			static.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

// tvDoor is the whole door as it runs: its own mux, wrapped like the main one.
func (a *app) tvDoor(static http.Handler) http.Handler {
	mux := http.NewServeMux()
	s := doorStatic(static)
	mux.HandleFunc("GET /tv", s)
	mux.HandleFunc("GET /tv/", s)
	mux.HandleFunc("GET /leaflet/", s)
	mux.HandleFunc("GET /icons/", s)
	mux.HandleFunc("GET /staatliches-latin.woff2", s)
	mux.HandleFunc("GET /staatliches-latin-ext.woff2", s)
	mux.HandleFunc("POST /tv/pair", a.handleTVPairStart)
	mux.HandleFunc("GET /tv/pair/{poll}", a.handleTVPairPoll)
	mux.HandleFunc("GET /api/tv/me", a.tvAuth(a.handleTVMe))
	mux.HandleFunc("GET /api/tv/trips", a.tvAuth(a.handleTVTrips))
	mux.HandleFunc("GET /api/tv/trips/{id}", a.tvAuth(a.handleTVTrip))
	mux.HandleFunc("GET /api/tv/photo/{asset}/{kind}", a.tvAuth(a.handleTVPhoto))
	mux.HandleFunc("/", http.NotFound)
	return requestLog(securityHeaders(gzipJSON(mux)))
}
