package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
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
		hash       string
		isAdmin    bool
		mustChange bool
	)
	err := a.db.QueryRow(`SELECT id, password_hash, is_admin, must_change_password FROM users WHERE username = ?`, req.Username).
		Scan(&id, &hash, &isAdmin, &mustChange)
	if err == sql.ErrNoRows {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(req.Password))
		a.limiter.fail(key)
		httpError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		a.limiter.fail(key)
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
	writeJSON(w, http.StatusOK, sessionUser{Username: req.Username, IsAdmin: isAdmin, MustChange: mustChange})
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
	u := r.Context().Value(userKey).(sessionUser)
	writeJSON(w, http.StatusOK, map[string]any{
		"username":             u.Username,
		"is_admin":             u.IsAdmin,
		"must_change_password": u.MustChange,
		"version":              version,
	})
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
	var taken int
	a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE lower(username) = lower(?)`, req.Username).Scan(&taken)
	if taken > 0 {
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
		httpError(w, http.StatusUnauthorized, "current password is wrong")
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

// mustChangeAllowed lists what a temporary password may reach.
func mustChangeAllowed(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/me", "POST /api/password", "POST /api/logout":
		return true
	}
	return false
}
