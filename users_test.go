package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// family is a server with the account routes mounted, and a helper that acts
// as one person: each name keeps its own session cookie.
type family struct {
	t       *testing.T
	a       *app
	mux     *http.ServeMux
	cookies map[string]*http.Cookie
}

func newFamily(t *testing.T) *family {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := &app{db: db, limiter: newLoginLimiter()}
	mux := http.NewServeMux()
	a.accountRoutes(mux)
	return &family{t: t, a: a, mux: mux, cookies: map[string]*http.Cookie{}}
}

// add puts an account straight into the database, the way the first admin
// arrives; must_change stays 0 so they can sign in normally.
func (f *family) add(name string, isAdmin bool) int64 {
	hash, _ := bcrypt.GenerateFromPassword([]byte(name+"-password"), bcrypt.MinCost)
	res, err := f.a.db.Exec(`INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, ?)`, name, hash, isAdmin)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// do sends a request as who ("" = nobody) and returns the status and body.
func (f *family) do(who, method, path string, body any) (int, []byte) {
	f.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c := f.cookies[who]; c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge >= 0 && c.Value != "" {
			f.cookies[who] = c
		}
	}
	return rec.Code, rec.Body.Bytes()
}

func (f *family) signIn(who, pass string) int {
	f.t.Helper()
	code, _ := f.do(who, "POST", "/api/login", map[string]string{"username": who, "password": pass})
	return code
}

func (f *family) list(who string) map[string]userOut {
	f.t.Helper()
	code, body := f.do(who, "GET", "/api/users", nil)
	if code != http.StatusOK {
		f.t.Fatalf("list as %s: %d %s", who, code, body)
	}
	var us []userOut
	json.Unmarshal(body, &us)
	out := map[string]userOut{}
	for _, u := range us {
		out[u.Username] = u
	}
	return out
}

func want(t *testing.T, what string, got, wanted int) {
	t.Helper()
	if got != wanted {
		t.Errorf("%s: got %d, want %d", what, got, wanted)
	}
}

func TestFamilyPageIsAdminOnly(t *testing.T) {
	f := newFamily(t)
	f.add("dad", true)
	kid := f.add("kid", false)
	f.signIn("kid", "kid-password")
	code, _ := f.do("kid", "GET", "/api/users", nil)
	want(t, "kid lists accounts", code, http.StatusForbidden)
	code, _ = f.do("kid", "POST", fmt.Sprintf("/api/users/%d/password", kid), map[string]string{"password": "whatever-123"})
	want(t, "kid resets a password", code, http.StatusForbidden)
	code, _ = f.do("", "GET", "/api/users", nil)
	want(t, "nobody lists accounts", code, http.StatusUnauthorized)

	f.signIn("dad", "dad-password")
	us := f.list("dad")
	if len(us) != 2 || !us["dad"].You || us["kid"].You || us["kid"].Sessions != 1 || us["kid"].LastSeen == "" {
		t.Errorf("list: %+v", us)
	}
}

// A reset password works once, to choose your own; until then the API is shut.
func TestResetPasswordForcesAChangeAndSignsOut(t *testing.T) {
	f := newFamily(t)
	f.add("dad", true)
	kid := f.add("kid", false)
	f.signIn("dad", "dad-password")
	f.signIn("kid", "kid-password")

	code, _ := f.do("dad", "POST", fmt.Sprintf("/api/users/%d/password", kid), map[string]string{"password": "short"})
	want(t, "too-short temporary password", code, http.StatusBadRequest)
	code, _ = f.do("dad", "POST", fmt.Sprintf("/api/users/%d/password", kid), map[string]string{"password": "temp-pass-42"})
	want(t, "reset", code, http.StatusOK)

	code, _ = f.do("kid", "GET", "/api/me", nil)
	want(t, "kid's old session after reset", code, http.StatusUnauthorized)
	want(t, "old password", f.signIn("kid", "kid-password"), http.StatusUnauthorized)
	want(t, "temporary password", f.signIn("kid", "temp-pass-42"), http.StatusOK)

	code, body := f.do("kid", "GET", "/api/me", nil)
	want(t, "me while owing a change", code, http.StatusOK)
	if !bytes.Contains(body, []byte(`"must_change_password":true`)) {
		t.Errorf("me should say must_change_password: %s", body)
	}
	code, _ = f.do("kid", "GET", "/api/users", nil)
	want(t, "anything else while owing a change", code, http.StatusPreconditionRequired)

	code, _ = f.do("kid", "POST", "/api/password", map[string]string{"current_password": "temp-pass-42", "new_password": "temp-pass-42"})
	want(t, "keeping the temporary password", code, http.StatusBadRequest)
	code, _ = f.do("kid", "POST", "/api/password", map[string]string{"current_password": "temp-pass-42", "new_password": "kids-own-pass"})
	want(t, "choosing their own", code, http.StatusOK)
	code, body = f.do("kid", "GET", "/api/me", nil)
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"must_change_password":false`)) {
		t.Errorf("after the change: %d %s", code, body)
	}
}

func TestNewAccountsStartWithATemporaryPassword(t *testing.T) {
	f := newFamily(t)
	f.add("dad", true)
	f.signIn("dad", "dad-password")
	code, _ := f.do("dad", "POST", "/api/users", map[string]string{"username": "Gran", "password": "welcome-gran"})
	want(t, "create", code, http.StatusCreated)
	code, _ = f.do("dad", "POST", "/api/users", map[string]string{"username": "gran", "password": "welcome-gran"})
	want(t, "same name in another case", code, http.StatusConflict)
	if !f.list("dad")["Gran"].MustChange {
		t.Error("a new account should owe a password change")
	}
}

func TestUnlockLiftsALoginLock(t *testing.T) {
	f := newFamily(t)
	f.add("dad", true)
	kid := f.add("kid", false)
	f.signIn("dad", "dad-password")
	for i := 0; i < 5; i++ {
		f.signIn("kid", "wrong")
	}
	if f.list("dad")["kid"].LockedSeconds <= 0 {
		t.Fatal("kid should show as locked")
	}
	code, _ := f.do("dad", "POST", fmt.Sprintf("/api/users/%d/unlock", kid), nil)
	want(t, "unlock", code, http.StatusOK)
	want(t, "sign in after unlock", f.signIn("kid", "kid-password"), http.StatusOK)
}

func TestSignOutEverywhere(t *testing.T) {
	f := newFamily(t)
	dad := f.add("dad", true)
	kid := f.add("kid", false)
	f.signIn("dad", "dad-password")
	f.signIn("kid", "kid-password")
	code, _ := f.do("dad", "POST", fmt.Sprintf("/api/users/%d/signout", kid), nil)
	want(t, "sign kid out", code, http.StatusOK)
	code, _ = f.do("kid", "GET", "/api/me", nil)
	want(t, "kid after sign-out", code, http.StatusUnauthorized)

	// your own: the other devices go, this one stays
	f.cookies["dad-phone"] = nil
	f.do("dad-phone", "POST", "/api/login", map[string]string{"username": "dad", "password": "dad-password"})
	code, _ = f.do("dad", "POST", fmt.Sprintf("/api/users/%d/signout", dad), nil)
	want(t, "sign my other devices out", code, http.StatusOK)
	code, _ = f.do("dad", "GET", "/api/me", nil)
	want(t, "this device", code, http.StatusOK)
	code, _ = f.do("dad-phone", "GET", "/api/me", nil)
	want(t, "the other device", code, http.StatusUnauthorized)
}

func TestAdminRoleGuardrails(t *testing.T) {
	f := newFamily(t)
	dad := f.add("dad", true)
	mum := f.add("mum", false)
	f.signIn("dad", "dad-password")

	code, _ := f.do("dad", "PATCH", fmt.Sprintf("/api/users/%d", dad), map[string]bool{"is_admin": false})
	want(t, "demote myself", code, http.StatusBadRequest)
	code, _ = f.do("dad", "DELETE", fmt.Sprintf("/api/users/%d", dad), nil)
	want(t, "remove myself", code, http.StatusBadRequest)
	code, _ = f.do("dad", "POST", fmt.Sprintf("/api/users/%d/password", dad), map[string]string{"password": "whatever-123"})
	want(t, "reset my own password here", code, http.StatusBadRequest)

	code, _ = f.do("dad", "PATCH", fmt.Sprintf("/api/users/%d", mum), map[string]bool{"is_admin": true})
	want(t, "make mum admin", code, http.StatusOK)
	f.signIn("mum", "mum-password")
	code, _ = f.do("mum", "GET", "/api/users", nil)
	want(t, "mum lists accounts", code, http.StatusOK)
	code, _ = f.do("mum", "PATCH", fmt.Sprintf("/api/users/%d", dad), map[string]bool{"is_admin": false})
	want(t, "mum demotes dad", code, http.StatusOK)
	code, _ = f.do("dad", "GET", "/api/users", nil)
	want(t, "dad after demotion", code, http.StatusForbidden)

	// the rule itself, reached directly: the last admin is never demoted or removed
	if !f.a.lastAdmin(userOut{ID: mum, IsAdmin: true}) {
		t.Error("mum is now the last admin")
	}
}

func TestRemoveAccountKeepsTheirTrips(t *testing.T) {
	f := newFamily(t)
	f.add("dad", true)
	kid := f.add("kid", false)
	f.a.db.Exec(`INSERT INTO holidays (name, color, start_at, end_at) VALUES ('Lisbon', '#1d6fb8', '2024-10-03T00:00:00Z', '2024-10-08T23:59:59Z')`)
	f.signIn("dad", "dad-password")
	f.signIn("kid", "kid-password")
	code, _ := f.do("dad", "DELETE", fmt.Sprintf("/api/users/%d", kid), nil)
	want(t, "remove kid", code, http.StatusNoContent)
	code, _ = f.do("kid", "GET", "/api/me", nil)
	want(t, "kid's session after removal", code, http.StatusUnauthorized)
	var sessions, trips int
	f.a.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, kid).Scan(&sessions)
	f.a.db.QueryRow(`SELECT COUNT(*) FROM holidays`).Scan(&trips)
	want(t, "kid's sessions left", sessions, 0)
	want(t, "trips left", trips, 1)
	if _, ok := f.list("dad")["kid"]; ok {
		t.Error("kid still listed")
	}
}
