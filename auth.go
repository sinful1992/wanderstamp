package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net"
	"net/http"
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
	ID       int64  `json:"-"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
}

// --- login rate limiting ---

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

func (l *loginLimiter) locked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.attempts[ip]
	return a != nil && time.Now().Before(a.lockedUntil)
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.attempts[ip]
	if a == nil {
		a = &loginAttempts{}
		l.attempts[ip] = a
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

func (l *loginLimiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, ip)
}

// gc drops entries whose last failure is old, so the map can't grow forever.
func (l *loginLimiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	for ip, a := range l.attempts {
		if a.lastFail.Before(cutoff) && time.Now().After(a.lockedUntil) {
			delete(l.attempts, ip)
		}
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- handlers ---

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.limiter.locked(ip) {
		httpError(w, http.StatusTooManyRequests, "too many failed logins, try again later")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}

	var (
		id      int64
		hash    string
		isAdmin bool
	)
	err := a.db.QueryRow(`SELECT id, password_hash, is_admin FROM users WHERE username = ?`, req.Username).
		Scan(&id, &hash, &isAdmin)
	if err == sql.ErrNoRows {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(req.Password))
		a.limiter.fail(ip)
		httpError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		a.limiter.fail(ip)
		httpError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	a.limiter.success(ip)

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
	writeJSON(w, http.StatusOK, sessionUser{Username: req.Username, IsAdmin: isAdmin})
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
		"username": u.Username,
		"is_admin": u.IsAdmin,
		"version":  version,
	})
}

func (a *app) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if !r.Context().Value(userKey).(sessionUser).IsAdmin {
		httpError(w, http.StatusForbidden, "admin only")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Username == "" || len(req.Username) > 64 || len(req.Password) < 8 {
		httpError(w, http.StatusBadRequest, "username required, password must be at least 8 characters")
		return
	}
	if len(req.Password) > maxPassword {
		httpError(w, http.StatusBadRequest, "password must be at most 72 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "hash error")
		return
	}
	if _, err := a.db.Exec(`INSERT INTO users (username, password_hash) VALUES (?, ?)`, req.Username, hash); err != nil {
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
	if len(req.NewPassword) < 8 {
		httpError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	if len(req.NewPassword) > maxPassword {
		httpError(w, http.StatusBadRequest, "new password must be at most 72 characters")
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
	if _, err := a.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, newHash, u.ID); err != nil {
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
		)
		lookup := `
			SELECT u.id, u.username, u.is_admin, s.expires_at
			FROM sessions s JOIN users u ON u.id = s.user_id
			WHERE s.token = ?`
		err = a.db.QueryRow(lookup, key).Scan(&u.ID, &u.Username, &u.IsAdmin, &expiresAt)
		if err == sql.ErrNoRows {
			// Session created before tokens were hashed at rest: accept once
			// and upgrade the row in place, so nobody gets logged out.
			if a.db.QueryRow(lookup, c.Value).Scan(&u.ID, &u.Username, &u.IsAdmin, &expiresAt) == nil {
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
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}
