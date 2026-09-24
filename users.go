package main

import (
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// The family page: an admin sees every account, resets a forgotten password,
// signs a lost phone out, lifts a login lock, and shares the admin role.
// Accounts are logins only — trips, pins and lists belong to the family — so
// removing someone never touches what they recorded.

// accountRoutes mounts signing in and out and the family page's endpoints.
func (a *app) accountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.auth(a.handleLogout))
	mux.HandleFunc("GET /api/me", a.auth(a.handleMe))
	mux.HandleFunc("POST /api/password", a.auth(a.handleChangePassword))
	mux.HandleFunc("GET /api/users", a.auth(admin(a.handleListUsers)))
	mux.HandleFunc("POST /api/users", a.auth(admin(a.handleCreateUser)))
	mux.HandleFunc("POST /api/users/{id}/password", a.auth(admin(a.handleResetPassword)))
	mux.HandleFunc("POST /api/users/{id}/signout", a.auth(admin(a.handleSignOutUser)))
	mux.HandleFunc("POST /api/users/{id}/unlock", a.auth(admin(a.handleUnlockUser)))
	mux.HandleFunc("PATCH /api/users/{id}", a.auth(admin(a.handleSetAdmin)))
	mux.HandleFunc("DELETE /api/users/{id}", a.auth(admin(a.handleDeleteUser)))
}

type userOut struct {
	ID            int64  `json:"id"`
	Username      string `json:"username"`
	IsAdmin       bool   `json:"is_admin"`
	CreatedAt     string `json:"created_at"`
	LastSeen      string `json:"last_seen"` // "" = never signed in
	MustChange    bool   `json:"must_change_password"`
	Sessions      int    `json:"sessions"`
	LockedSeconds int    `json:"locked_seconds"`
	You           bool   `json:"you"`
}

// admin wraps an already-authenticated handler, refusing non-admins.
func admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !r.Context().Value(userKey).(sessionUser).IsAdmin {
			httpError(w, http.StatusForbidden, "admin only")
			return
		}
		next(w, r)
	}
}

func (a *app) handleListUsers(w http.ResponseWriter, r *http.Request) {
	me := r.Context().Value(userKey).(sessionUser)
	rows, err := a.db.Query(`
		SELECT u.id, u.username, u.is_admin, u.created_at, u.last_seen, u.must_change_password,
		       (SELECT COUNT(*) FROM sessions s WHERE s.user_id = u.id AND s.expires_at > ?)
		FROM users u ORDER BY u.is_admin DESC, lower(u.username)`,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []userOut{}
	for rows.Next() {
		var u userOut
		if err := rows.Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt, &u.LastSeen, &u.MustChange, &u.Sessions); err != nil {
			httpError(w, http.StatusInternalServerError, "database error")
			return
		}
		u.You = u.ID == me.ID
		u.LockedSeconds = int(a.limiter.lockedFor(limiterKey(u.Username)).Round(time.Second).Seconds())
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, out)
}

// target loads the account named in the path. Acting on yourself is refused
// for everything except signing out your other devices: your own password
// goes through "Change my password", and you can't remove or demote yourself.
func (a *app) target(w http.ResponseWriter, r *http.Request, selfOK bool) (userOut, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return userOut{}, false
	}
	var u userOut
	u.ID = id
	if err := a.db.QueryRow(`SELECT username, is_admin FROM users WHERE id = ?`, id).Scan(&u.Username, &u.IsAdmin); err != nil {
		httpError(w, http.StatusNotFound, "no such account")
		return userOut{}, false
	}
	u.You = id == r.Context().Value(userKey).(sessionUser).ID
	if u.You && !selfOK {
		httpError(w, http.StatusBadRequest, "that's your own account — use Change my password, and another admin for the rest")
		return userOut{}, false
	}
	return u, true
}

// lastAdmin reports whether u is the only admin left. The self-guard already
// covers this (only an admin gets here, so a second admin exists whenever the
// target is one), but the rule is what matters, so it's checked as the rule.
func (a *app) lastAdmin(u userOut) bool {
	if !u.IsAdmin {
		return false
	}
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&n)
	return n <= 1
}

func (a *app) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := a.target(w, r, false)
	if !ok {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if msg := passwordProblem(req.Password); msg != "" {
		httpError(w, http.StatusBadRequest, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "hash error")
		return
	}
	// The temporary password is one someone else has seen: it works once,
	// to choose their own, and every device the old one opened is signed out.
	if _, err := a.db.Exec(`UPDATE users SET password_hash = ?, must_change_password = 1 WHERE id = ?`, hash, u.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	a.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, u.ID)
	a.limiter.success(limiterKey(u.Username))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleSignOutUser(w http.ResponseWriter, r *http.Request) {
	u, ok := a.target(w, r, true)
	if !ok {
		return
	}
	var n int64
	if u.You {
		// your own: every device but this one
		c, _ := r.Cookie(sessionCookie)
		res, _ := a.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token NOT IN (?, ?)`,
			u.ID, hashToken(c.Value), c.Value)
		n, _ = res.RowsAffected()
	} else {
		res, _ := a.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, u.ID)
		n, _ = res.RowsAffected()
	}
	writeJSON(w, http.StatusOK, map[string]int64{"signed_out": n})
}

func (a *app) handleUnlockUser(w http.ResponseWriter, r *http.Request) {
	u, ok := a.target(w, r, true)
	if !ok {
		return
	}
	a.limiter.success(limiterKey(u.Username))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleSetAdmin(w http.ResponseWriter, r *http.Request) {
	u, ok := a.target(w, r, false)
	if !ok {
		return
	}
	var req struct {
		IsAdmin *bool `json:"is_admin"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.IsAdmin == nil {
		httpError(w, http.StatusBadRequest, "is_admin required")
		return
	}
	if !*req.IsAdmin && a.lastAdmin(u) {
		httpError(w, http.StatusBadRequest, "the last admin stays an admin — make someone else one first")
		return
	}
	a.db.Exec(`UPDATE users SET is_admin = ? WHERE id = ?`, *req.IsAdmin, u.ID)
	writeJSON(w, http.StatusOK, map[string]bool{"is_admin": *req.IsAdmin})
}

func (a *app) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	u, ok := a.target(w, r, false)
	if !ok {
		return
	}
	if a.lastAdmin(u) {
		httpError(w, http.StatusBadRequest, "the last admin can't be removed")
		return
	}
	// sessions first and by hand: the cascade needs foreign_keys on
	a.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, u.ID)
	if _, err := a.db.Exec(`DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "database error")
		return
	}
	a.limiter.success(limiterKey(u.Username))
	w.WriteHeader(http.StatusNoContent)
}

// passwordProblem is the one password rule, shared by every form that sets one.
func passwordProblem(p string) string {
	if len(p) < 8 {
		return "password must be at least 8 characters"
	}
	if len(p) > maxPassword {
		return "password must be at most 72 characters"
	}
	return ""
}

// limiterKey is how the login limiter names an account: case-folded, so
// "Giedrius" and "giedrius" share one count.
func limiterKey(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}
