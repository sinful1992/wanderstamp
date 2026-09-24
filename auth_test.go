package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func login(a *app, user, pass string) int {
	req := httptest.NewRequest("POST", "/api/login",
		bytes.NewBufferString(`{"username":"`+user+`","password":"`+pass+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:5000" // everyone arrives via tailscale serve
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)
	return rec.Code
}

// One family member's typos lock their own account, never everyone
// arriving from the same proxy address.
func TestLoginLockoutIsPerAccount(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, u := range []string{"mum", "dad"} {
		hash, _ := bcrypt.GenerateFromPassword([]byte(u+"-password"), bcrypt.MinCost)
		db.Exec(`INSERT INTO users (username, password_hash) VALUES (?, ?)`, u, hash)
	}
	a := &app{db: db, limiter: newLoginLimiter()}

	for i := 0; i < 5; i++ {
		login(a, "mum", "wrong")
	}
	if code := login(a, "mum", "mum-password"); code != http.StatusTooManyRequests {
		t.Errorf("locked account with the right password: got %d, want 429", code)
	}
	if code := login(a, "MUM", "mum-password"); code != http.StatusTooManyRequests {
		t.Errorf("a case change must not dodge the lock: got %d", code)
	}
	if code := login(a, "dad", "dad-password"); code != http.StatusOK {
		t.Errorf("another account from the same address: got %d, want 200", code)
	}
}
