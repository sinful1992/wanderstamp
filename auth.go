package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "hm_session"
	sessionTTL    = 30 * 24 * time.Hour
	bcryptCost    = 12
	maxPassword   = 72 // bcrypt ignores everything past 72 bytes
)

// dummyHash keeps login timing constant for unknown usernames.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcryptCost)

// hashToken is what actually lands in the sessions table, so a leaked
// database copy (backups travel to other disks) can't be replayed as a cookie.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type ctxKey int

const userKey ctxKey = 0

type sessionUser struct {
	ID         int64  `json:"-"`
	Username   string `json:"username"`
	IsAdmin    bool   `json:"is_admin"`
	MustChange bool   `json:"must_change_password"`
}

// --- login rate limiting ---

// Failures are counted per account, not per client address: behind
// tailscale serve every remote device arrives from the same address, so an
// IP-keyed lockout let one person's typos lock the whole family out.

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempts
}

type loginAttempts struct {
	fails       int
	lastFail    time.Time
	lockedUntil time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string]*loginAttempts)}
}

func (l *loginLimiter) locked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.attempts[key]
	return a != nil && time.Now().Before(a.lockedUntil)
}

// lockedFor is how long the account stays locked; zero when it isn't.
func (l *loginLimiter) lockedFor(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.attempts[key]; a != nil {
		if d := time.Until(a.lockedUntil); d > 0 {
			return d
		}
	}
	return 0
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.attempts[key]
	if a == nil {
		a = &loginAttempts{}
		l.attempts[key] = a
	}
	a.fails++
	a.lastFail = time.Now()
	if a.fails >= 5 {
		lock := time.Duration(1<<uint(min(a.fails-5, 4))) * time.Minute
		if lock > 15*time.Minute {
			lock = 15 * time.Minute
		}
		a.lockedUntil = time.Now().Add(lock)
	}
}

func (l *loginLimiter) success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// gc drops entries whose last failure is old, so the map can't grow forever.
func (l *loginLimiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	for key, a := range l.attempts {
		if a.lastFail.Before(cutoff) && time.Now().After(a.lockedUntil) {
			delete(l.attempts, key)
		}
	}
}

// --- handlers ---

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// iOS types "Kayleigh" for "kayleigh" and pads autocompleted names with a space
	req.Username = strings.TrimSpace(req.Username)
	ip := clientIP(r)
	if d := a.ipLimiter.blockedFor(ip); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
		httpError(w, http.StatusTooManyRequests, "too many failed logins from this device, try again later")
		return
	}
	// case-folded so "Giedrius" and "giedrius" can't get ten tries between them
	key := limiterKey(req.Username)
	if len(key) > 64 { // no such account can exist; don't let junk names grow the limiter
		httpError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if a.limiter.locked(key) {
		httpError(w, http.StatusTooManyRequests, "too many failed logins for this account, try again in a few minutes")
		return
	}

	var (
		id         int64
		name       string
		hash       string
		isAdmin    bool
		mustChange bool
	)
	// the name exactly as typed first, so two old accounts that differ only
	// by case ("kid", "Kid") both still sign in; then any case, folded in Go
	// because SQLite's lower() only knows ASCII ("Ünal" / "ünal")
	err := a.db.QueryRow(`SELECT id, username, password_hash, is_admin, must_change_password FROM users
		WHERE username = ?`, req.Username).
		Scan(&id, &name, &hash, &isAdmin, &mustChange)
	if err == sql.ErrNoRows {
		if match, ok := a.usernameFold(req.Username); ok {
			err = a.db.QueryRow(`SELECT id, username, password_hash, is_admin, must_change_password FROM users
				WHERE username = ?`, match).Scan(&id, &name, &hash, &isAdmin, &mustChange)
		}
	}
	if err == sql.ErrNoRows {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(req.Password))
		a.limiter.fail(key)
		a.ipLimiter.fail(ip)
		httpError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		a.limiter.fail(key)
		a.ipLimiter.fail(ip)
		httpError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	a.limiter.success(key)

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		httpError(w, http.StatusInternalServerError, "entropy error")
		return
	}
	token := hex.EncodeToString(buf)
	expires := time.Now().UTC().Add(sessionTTL)
	if _, err := a.db.Exec(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(token), id, expires.Format(time.RFC3339)); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	http.SetCookie(w, a.sessionCookie(token, int(sessionTTL.Seconds())))
	a.db.Exec(`UPDATE users SET last_seen = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), id)
	writeJSON(w, http.StatusOK, sessionUser{Username: name, IsAdmin: isAdmin, MustChange: mustChange})
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		// pre-hashing sessions stored the raw token; clear either form
		a.db.Exec(`DELETE FROM sessions WHERE token IN (?, ?)`, hashToken(c.Value), c.Value)
	}
	http.SetCookie(w, a.sessionCookie("", -1))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, meOut(r.Context().Value(userKey).(sessionUser)))
}

func meOut(u sessionUser) map[string]any {
	return map[string]any{
		"username":             u.Username,
		"is_admin":             u.IsAdmin,
		"must_change_password": u.MustChange,
		"version":              version,
	}
}

func (a *app) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Username) > 64 {
		httpError(w, http.StatusBadRequest, "username required, at most 64 characters")
		return
	}
	if msg := passwordProblem(req.Password); msg != "" {
		httpError(w, http.StatusBadRequest, msg)
		return
	}
	// the limiter already treats "Kid" and "kid" as one account; so does this
	if _, taken := a.usernameFold(req.Username); taken {
		httpError(w, http.StatusConflict, "username already exists")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "hash error")
		return
	}
	// the admin chose this password, so it's temporary: the first sign-in
	// asks for their own
	if _, err := a.db.Exec(`INSERT INTO users (username, password_hash, must_change_password) VALUES (?, ?, 1)`, req.Username, hash); err != nil {
		httpError(w, http.StatusConflict, "username already exists")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"username": req.Username})
}

func (a *app) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(userKey).(sessionUser)
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if msg := passwordProblem(req.NewPassword); msg != "" {
		httpError(w, http.StatusBadRequest, "new "+msg)
		return
	}
	var hash string
	if err := a.db.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, u.ID).Scan(&hash); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.CurrentPassword)) != nil {
		// 403, not 401: the session is fine, and a 401 sends the page to the
		// sign-in cover instead of saying the password was mistyped
		httpError(w, http.StatusForbidden, "current password is wrong")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcryptCost)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "hash error")
		return
	}
	if req.NewPassword == req.CurrentPassword {
		httpError(w, http.StatusBadRequest, "choose a password different from the current one")
		return
	}
	if _, err := a.db.Exec(`UPDATE users SET password_hash = ?, must_change_password = 0 WHERE id = ?`, newHash, u.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	// Sign out every other device; the session that changed the password stays.
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token NOT IN (?, ?)`,
			u.ID, hashToken(c.Value), c.Value)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

// auth wraps a handler, requiring a valid session; the user lands in the context.
func (a *app) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			httpError(w, http.StatusForbidden, "cross-site request")
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		key := hashToken(c.Value)
		var (
			u         sessionUser
			expiresAt string
			lastSeen  string
		)
		lookup := `
			SELECT u.id, u.username, u.is_admin, u.must_change_password, u.last_seen, s.expires_at
			FROM sessions s JOIN users u ON u.id = s.user_id
			WHERE s.token = ?`
		err = a.db.QueryRow(lookup, key).Scan(&u.ID, &u.Username, &u.IsAdmin, &u.MustChange, &lastSeen, &expiresAt)
		if err == sql.ErrNoRows {
			// Session created before tokens were hashed at rest: accept once
			// and upgrade the row in place, so nobody gets logged out.
			if a.db.QueryRow(lookup, c.Value).Scan(&u.ID, &u.Username, &u.IsAdmin, &u.MustChange, &lastSeen, &expiresAt) == nil {
				a.db.Exec(`UPDATE sessions SET token = ? WHERE token = ?`, key, c.Value)
				err = nil
			}
		}
		if err != nil {
			httpError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		exp, err := time.Parse(time.RFC3339, expiresAt)
		if err != nil || time.Now().After(exp) {
			a.db.Exec(`DELETE FROM sessions WHERE token = ?`, key)
			httpError(w, http.StatusUnauthorized, "session expired")
			return
		}
		// Sliding expiry: refresh when less than half the TTL remains.
		if time.Until(exp) < sessionTTL/2 {
			newExp := time.Now().UTC().Add(sessionTTL)
			a.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token = ?`, newExp.Format(time.RFC3339), key)
			http.SetCookie(w, a.sessionCookie(c.Value, int(sessionTTL.Seconds())))
		}
		// Last seen, for the family page: written at most every five minutes
		// so a burst of requests isn't a burst of writes.
		if t, err := time.Parse(time.RFC3339, lastSeen); err != nil || time.Since(t) > 5*time.Minute {
			a.db.Exec(`UPDATE users SET last_seen = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), u.ID)
		}
		// A temporary password opens one door: choosing your own. Checked
		// here rather than in the page, so the API holds the rule.
		if u.MustChange && !mustChangeAllowed(r) {
			httpError(w, http.StatusPreconditionRequired, "choose your own password first")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

// usernameFold finds the account a name means in any case, Unicode-aware.
// The family has a handful of accounts, so reading them all is nothing.
func (a *app) usernameFold(name string) (string, bool) {
	rows, err := a.db.Query(`SELECT username FROM users ORDER BY id`)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil && strings.EqualFold(u, name) {
			return u, true
		}
	}
	return "", false
}

// sameOrigin backs up SameSite=Lax for requests that change something: POSTs
// with no body (end trip, sync, sign out a user) need no preflight, so the
// browser's own word on where the request came from is checked. A client
// that sends neither header (curl, an old WebView) isn't a browser being
// tricked, and passes.
func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	// tailscale serve may hand the request on with Host = 127.0.0.1:8095; the
	// name the browser used then arrives as X-Forwarded-Host
	return strings.EqualFold(u.Host, r.Host) ||
		(r.Header.Get("X-Forwarded-Host") != "" && strings.EqualFold(u.Host, r.Header.Get("X-Forwarded-Host")))
}

// --- per-device login limit ---

// clientIP is the device a login came from. Behind tailscale serve every
// request arrives from the proxy, which appends the real address to
// X-Forwarded-For, so the last entry is the one the proxy vouches for
// (anything before it the client could have typed itself). With no
// X-Forwarded-For there is no device to tell apart, only the proxy or the
// Docker gateway, and "" turns the per-device limit off: one shared budget
// would let a stranger's typos lock the whole family out again.
func clientIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !trustedProxy(peer) {
		// straight from a device (the LAN port): its own address is the key,
		// and whatever X-Forwarded-For it typed is ignored
		return peer
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			if host, _, err := net.SplitHostPort(ip); err == nil {
				return host
			}
			return strings.Trim(ip, "[]")
		}
	}
	return ""
}

// trustedProxy is a peer whose X-Forwarded-For is believed: loopback, or the
// container's network gateway, which is where tailscale serve's requests
// come from once docker-proxy has passed them in (LAN connections keep their
// real source address, so they never look like the gateway).
func trustedProxy(peer string) bool {
	ip := net.ParseIP(peer)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	gw := containerGateway()
	return gw != nil && gw.Equal(ip)
}

var (
	gatewayOnce sync.Once
	gatewayIP   net.IP
)

// containerGateway reads the default route's gateway from /proc/net/route
// once; nil when there is none (tests, a host without /proc).
func containerGateway() net.IP {
	gatewayOnce.Do(func() {
		data, err := os.ReadFile("/proc/net/route")
		if err == nil {
			gatewayIP = parseDefaultGateway(string(data))
		}
	})
	return gatewayIP
}

// parseDefaultGateway picks the 0.0.0.0 destination's gateway out of
// /proc/net/route, where addresses are little-endian hex.
func parseDefaultGateway(table string) net.IP {
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	return nil
}

const (
	ipMaxFails = 20
	ipWindow   = 15 * time.Minute
)

// ipLimiter caps failed logins per device across all usernames, so nobody
// can walk through the family's names or lock them all out in turn. It sits
// beside the per-account lockout, which still guards each account.
type ipLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func newIPLimiter() *ipLimiter { return &ipLimiter{fails: map[string][]time.Time{}} }

func (l *ipLimiter) recent(ip string, now time.Time) []time.Time {
	kept := l.fails[ip][:0]
	for _, t := range l.fails[ip] {
		if now.Sub(t) < ipWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, ip)
		return nil
	}
	l.fails[ip] = kept
	return kept
}

// blockedFor is how long this device must wait; zero when it may try.
func (l *ipLimiter) blockedFor(ip string) time.Duration {
	if l == nil || ip == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	f := l.recent(ip, now)
	if len(f) < ipMaxFails {
		return 0
	}
	return ipWindow - now.Sub(f[len(f)-ipMaxFails])
}

func (l *ipLimiter) fail(ip string) {
	if l == nil || ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, known := l.fails[ip]; !known && len(l.fails) >= 10000 {
		// a flood of addresses can't grow this forever: make room by dropping
		// the address whose last failure is oldest, never a blocked one first
		oldest, at := "", time.Now()
		for k, f := range l.fails {
			if last := f[len(f)-1]; len(f) < ipMaxFails && last.Before(at) {
				oldest, at = k, last
			}
		}
		if oldest == "" {
			return // everyone tracked is blocked; the newcomer waits its turn
		}
		delete(l.fails, oldest)
	}
	l.fails[ip] = append(l.recent(ip, time.Now()), time.Now())
}

func (l *ipLimiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for ip := range l.fails {
		l.recent(ip, now)
	}
}

// mustChangeAllowed lists what a temporary password may reach.
func mustChangeAllowed(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/me", "POST /api/password", "POST /api/logout":
		return true
	}
	return false
}
