package main

// Break tests for the task 112 backend change, written from the contract only.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func brkLogin(a *app, user, pass, remote, xff string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	req := httptest.NewRequest("POST", "/api/login", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remote
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)
	return rec
}

// ---------- B3 ----------

func TestBreakLoginMixedCaseTabsSpaces(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	addUser(t, a, "Kayleigh")
	for _, typed := range []string{"kAyLeIgH", "\tKayleigh\n", "  KAYLEIGH  "} {
		if rec := brkLogin(a, typed, "Kayleigh-password", "127.0.0.1:1", ""); rec.Code != 200 {
			t.Errorf("%q: %d %s", typed, rec.Code, rec.Body)
		}
	}
	if rec := brkLogin(a, "Kay leigh", "Kayleigh-password", "127.0.0.1:1", ""); rec.Code != 401 {
		t.Errorf("inner space must not match: %d", rec.Code)
	}
}

// Old databases can hold "kid" and "Kid" (UNIQUE is case-sensitive). Each
// must still reach its own account with its own password.
func TestBreakLoginCaseTwinAccounts(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	addUser(t, a, "kid")
	addUser(t, a, "Kid")
	if rec := brkLogin(a, "kid", "kid-password", "127.0.0.1:1", ""); rec.Code != 200 {
		t.Errorf("kid (older row): %d", rec.Code)
	}
	if rec := brkLogin(a, "Kid", "Kid-password", "127.0.0.1:1", ""); rec.Code != 200 {
		t.Errorf("Kid (newer row) typed exactly, own password: %d %s", rec.Code, rec.Body)
	}
}

func TestBreakLoginUnicodeCase(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	addUser(t, a, "Ünal")
	if rec := brkLogin(a, "ünal", "Ünal-password", "127.0.0.1:1", ""); rec.Code != 200 {
		t.Errorf("\"ünal\" for stored \"Ünal\": %d %s", rec.Code, rec.Body)
	}
}

// Sign-up's duplicate check uses the same lower(); with Unicode, "ünal" can
// be created beside "Ünal", and both then share one lockout key.
func TestBreakSignupUnicodeTwin(t *testing.T) {
	f := newFamily(t)
	f.add("boss", true)
	if c := f.signIn("boss", "boss-password"); c != 200 {
		t.Fatalf("admin sign in %d", c)
	}
	f.add("Ünal", false)
	code, body := f.do("boss", "POST", "/api/users", map[string]string{"username": "ünal", "password": "longenough123"})
	if code != http.StatusConflict {
		t.Errorf("create \"ünal\" beside \"Ünal\": %d %s, want 409", code, body)
	}
}

func TestBreakLockoutPerAccountStill(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	addUser(t, a, "mum")
	addUser(t, a, "dad")
	for _, typed := range []string{"MUM", " mum", "Mum ", "mUm", "mum"} {
		brkLogin(a, typed, "wrong", "127.0.0.1:1", "")
	}
	if rec := brkLogin(a, "mum", "mum-password", "127.0.0.1:1", ""); rec.Code != 429 {
		t.Errorf("mum after 5 mixed-case misses: %d, want 429", rec.Code)
	}
	if rec := brkLogin(a, "DAD", "dad-password", "127.0.0.1:1", ""); rec.Code != 200 {
		t.Errorf("dad: %d", rec.Code)
	}
}

// ---------- S3 ----------

func TestBreakIPLimitRetryAfter(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	a.ipLimiter = newIPLimiter()
	for i := 0; i < ipMaxFails; i++ {
		if rec := brkLogin(a, fmt.Sprintf("nobody%d", i), "x", "127.0.0.1:1", "100.64.0.5"); rec.Code != 401 {
			t.Fatalf("try %d: %d", i, rec.Code)
		}
	}
	rec := brkLogin(a, "nobody99", "x", "127.0.0.1:1", "100.64.0.5")
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if rec.Code != 429 || err != nil || ra < 1 || ra > 901 {
		t.Errorf("21st: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := brkLogin(a, "nobody", "x", "127.0.0.1:1", "100.64.0.6"); rec.Code != 401 {
		t.Errorf("another IP shares the budget: %d", rec.Code)
	}
}

// Informational: does a success from the device reset its failures?
func TestBreakIPLimitSuccessThenFail(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	a.ipLimiter = newIPLimiter()
	addUser(t, a, "mum")
	for i := 0; i < ipMaxFails-1; i++ {
		brkLogin(a, fmt.Sprintf("g%d", i), "x", "127.0.0.1:1", "100.64.0.5")
	}
	if rec := brkLogin(a, "mum", "mum-password", "127.0.0.1:1", "100.64.0.5"); rec.Code != 200 {
		t.Fatalf("success at 19 fails: %d", rec.Code)
	}
	brkLogin(a, "g99", "x", "127.0.0.1:1", "100.64.0.5")
	rec := brkLogin(a, "mum", "mum-password", "127.0.0.1:1", "100.64.0.5")
	t.Logf("after 19 fails + success + 1 fail, right password: %d (429 = success does not reset)", rec.Code)
}

// A client talking to the app directly (no proxy) picks its own last XFF entry.
func TestBreakIPLimitXFFSpoofDirect(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	a.ipLimiter = newIPLimiter()
	blocked := false
	for i := 0; i < 30; i++ {
		rec := brkLogin(a, fmt.Sprintf("g%d", i), "x", "192.168.1.50:4444", fmt.Sprintf("10.9.%d.%d", i/250, i%250))
		if rec.Code == 429 {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("30 fails from RemoteAddr 192.168.1.50 with a fresh X-Forwarded-For each time: never 429")
	}
}

// Via the proxy (append semantics), a client-typed prefix doesn't help.
func TestBreakIPLimitXFFViaProxy(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	a.ipLimiter = newIPLimiter()
	for i := 0; i < 25; i++ {
		brkLogin(a, fmt.Sprintf("g%d", i), "x", "127.0.0.1:1", fmt.Sprintf("1.2.3.%d, 100.64.0.7", i))
	}
	if rec := brkLogin(a, "g", "x", "127.0.0.1:1", "9.9.9.9, 100.64.0.7"); rec.Code != 429 {
		t.Errorf("forged prefix: %d", rec.Code)
	}
}

// Over 10000 distinct addresses wipe every entry, including a blocked one.
func TestBreakIPLimiterFloodUnblocks(t *testing.T) {
	l := newIPLimiter()
	for i := 0; i < ipMaxFails; i++ {
		l.fail("6.6.6.6")
	}
	if l.blockedFor("6.6.6.6") == 0 {
		t.Fatal("not blocked")
	}
	for i := 0; i <= 10001; i++ {
		l.fail(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if l.blockedFor("6.6.6.6") == 0 {
		t.Errorf("10002 other addresses failing unblocked 6.6.6.6")
	}
}

// ---------- S4 ----------

func TestBreakCrossSiteMatrix(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	addUser(t, a, "mum")
	cookie := loginFrom(a, "mum", "mum-password", "").Result().Cookies()[0]
	h := a.auth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	cases := []struct {
		method, site, origin string
		want                 int
	}{
		{"POST", "same-site", "", 403},
		{"POST", "", "null", 403},
		{"POST", "", "http://holiday.test:8443", 403},
		{"POST", "", "https://holiday.test.evil.example", 403},
		{"POST", "", "http://evilholiday.test", 403},
		{"POST", "", "HTTP://HOLIDAY.TEST", 200},
		{"POST", "", "http://Holiday.Test", 200},
		{"PATCH", "cross-site", "", 403},
		{"PUT", "cross-site", "", 403},
		{"DELETE", "", "https://evil.example", 403},
		{"GET", "cross-site", "https://evil.example", 200},
		{"HEAD", "cross-site", "", 200},
		{"POST", "same-origin", "https://evil.example", 200}, // Sec-Fetch-Site wins
		{"POST", "cross-site", "http://holiday.test", 403},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://holiday.test/api/x", nil)
		req.AddCookie(cookie)
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s site=%q origin=%q: %d, want %d", c.method, c.site, c.origin, rec.Code, c.want)
		}
		if rec.Code == 403 && !strings.Contains(rec.Body.String(), `"cross-site request"`) {
			t.Errorf("403 body %s", rec.Body)
		}
	}
}

// Login and logout through the real account routes.
func TestBreakCrossSiteLoginLogout(t *testing.T) {
	f := newFamily(t)
	f.add("mum", false)
	b := []byte(`{"username":"mum","password":"mum-password"}`)
	req := httptest.NewRequest("POST", "http://holiday.test/api/login", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	t.Logf("cross-site login POST (unauthenticated): %d (not covered by S4; JSON content-type needs a CORS preflight)", rec.Code)

	f.signIn("mum", "mum-password")
	req = httptest.NewRequest("POST", "http://holiday.test/api/logout", nil)
	req.AddCookie(f.cookies["mum"])
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("cross-site logout: %d, want 403", rec.Code)
	}
}

// ---------- S5 ----------

func brkShareApp(t *testing.T) (*app, *http.ServeMux) {
	a := newTestApp(t)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Trip', '#123456', '2026-07-01T00:00:00Z', '2026-07-08T23:59:59Z')`)
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES (1, 1, 'manual', 50, 0, '2026-07-02T10:00:00Z')`)
	a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (1, '00000000-0000-4000-8000-000000000001', '2026-07-02T10:00:00Z', 50, 0)`)
	return a, shareMux(a)
}

func TestBreakShareDays(t *testing.T) {
	_, mux := brkShareApp(t)
	for _, d := range []int{7, 30, 90, 365} {
		_, out := makeShare(t, mux, fmt.Sprintf(`{"days":%d}`, d))
		s, _ := out["expires_at"].(string)
		exp, err := time.Parse(time.RFC3339, s)
		want := time.Now().UTC().Add(time.Duration(d) * 24 * time.Hour)
		if err != nil || exp.Sub(want) > time.Minute || want.Sub(exp) > time.Minute || !strings.HasSuffix(s, "Z") {
			t.Errorf("days %d: expires_at %v", d, out["expires_at"])
		}
	}
	for _, body := range []string{`{"days":0}`, `{}`, `{"days":null}`, ""} {
		_, out := makeShare(t, mux, body)
		if v, ok := out["expires_at"]; !ok || v != nil {
			t.Errorf("%q: expires_at %v present %v", body, v, ok)
		}
	}
	for _, bad := range []string{`{"days":1}`, `{"days":-7}`, `{"days":"30"}`, `{"days":3650}`, `{"days":7.5}`, `{"days":true}`, `not json`} {
		if rec := serve(mux, "POST", "/api/holidays/1/share", bad); rec.Code != 400 {
			t.Errorf("%s: %d, want 400", bad, rec.Code)
		}
	}
	// a fetch with a streamed empty body: ContentLength -1
	req := httptest.NewRequest("POST", "/api/holidays/1/share", io.NopCloser(strings.NewReader("")))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("empty body of unknown length, no Content-Type: %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestBreakShareExpiry(t *testing.T) {
	a, mux := brkShareApp(t)
	token, _ := makeShare(t, mux, `{"days":30}`)
	routes := []string{"", "/pins/1/photos", "/photo/00000000-0000-4000-8000-000000000001/thumb", "/photo/00000000-0000-4000-8000-000000000001/original"}
	a.db.Exec(`UPDATE shares SET expires_at = ?`, time.Now().UTC().Add(-time.Second).Format(time.RFC3339))
	for _, p := range routes {
		rec := serve(mux, "GET", "/api/share/"+token+p, "")
		if rec.Code != 410 || !strings.Contains(rec.Body.String(), "this share link has expired") {
			t.Errorf("expired %q: %d %s", p, rec.Code, rec.Body)
		}
	}
	for _, p := range routes {
		if rec := serve(mux, "GET", "/api/share/0123456789abcdef0123456789abcdef"+p, ""); rec.Code != 404 {
			t.Errorf("unknown %q: %d", p, rec.Code)
		}
	}
	if h := listHolidays(t, a)["Trip"]; h.Shared {
		t.Errorf("/api/holidays still shared after expiry")
	}
	if _, hs, _ := getMap(t, a); len(hs) != 1 || hs[0].Shared {
		t.Errorf("/api/map still shared after expiry: %+v", hs)
	}
	token2, _ := makeShare(t, mux, `{"days":7}`)
	if rec := serve(mux, "GET", "/api/share/"+token2, ""); rec.Code != 200 {
		t.Errorf("re-share after expiry: %d", rec.Code)
	}
	if rec := serve(mux, "GET", "/api/share/"+token, ""); rec.Code != 404 {
		t.Errorf("old expired token after re-share: %d, want 404", rec.Code)
	}
	if h := listHolidays(t, a)["Trip"]; !h.Shared || h.ShareExpires == nil {
		t.Errorf("after re-share: shared %v expires %v", h.Shared, h.ShareExpires)
	}
	// a never link reports null
	makeShare(t, mux, "")
	if h := listHolidays(t, a)["Trip"]; !h.Shared || h.ShareExpires != nil {
		t.Errorf("never link: shared %v expires %v", h.Shared, h.ShareExpires)
	}
	// raw JSON has the key even when null
	rec := httptest.NewRecorder()
	a.handleListHolidays(rec, httptest.NewRequest("GET", "/api/holidays", nil))
	if !strings.Contains(rec.Body.String(), `"share_expires_at":null`) {
		t.Errorf("share_expires_at missing: %s", rec.Body)
	}
}

// ---------- S2 ----------

func TestBreakShareShapeAndOriginal(t *testing.T) {
	immich := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("jpeg"))
	}))
	defer immich.Close()
	a := newTestApp(t)
	a.immich = newImmichClient(immich.URL, "key")
	// live trip that hasn't arrived: every photo pin is on the way there
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, dest_name, dest_lat, dest_lng)
		VALUES (1, 'Live', '#123456', '2026-07-01T00:00:00Z', 'Faraway', 10, 10)`)
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES
		(1, 1, 'photo', 52, -1, '2026-07-01T08:00:00Z'),
		(2, 1, 'manual', 51, -0.5, '2026-07-01T10:00:00Z')`)
	a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES
		(1, '00000000-0000-4000-8000-000000000001', '2026-07-01T08:00:00Z', 52, -1)`)
	mux := shareMux(a)
	token, _ := makeShare(t, mux, "")
	var out struct {
		Holiday holidayOut `json:"holiday"`
		Pins    []pinOut   `json:"pins"`
	}
	rec := serve(mux, "GET", "/api/share/"+token, "")
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Pins) != 1 || out.Pins[0].ID != 2 {
		t.Errorf("live, not arrived: pins %+v, want only hand pin 2", out.Pins)
	}
	if out.Holiday.CoverAsset != "" || out.Holiday.PhotoCount != 0 {
		t.Errorf("cover %q photos %d, want none", out.Holiday.CoverAsset, out.Holiday.PhotoCount)
	}
	if rec := serve(mux, "GET", "/api/share/"+token+"/pins/1/photos", ""); rec.Code != 404 {
		t.Errorf("dropped pin photos: %d", rec.Code)
	}
	if rec := serve(mux, "GET", "/api/share/"+token+"/pins/2/photos", ""); rec.Code != 200 {
		t.Errorf("shown pin photos: %d", rec.Code)
	}
	if rec := serve(mux, "GET", "/api/share/"+token+"/pins/999/photos", ""); rec.Code != 404 {
		t.Errorf("unknown pin photos: %d", rec.Code)
	}
	for _, kind := range []string{"thumb", "preview"} {
		if rec := serve(mux, "GET", "/api/share/"+token+"/photo/00000000-0000-4000-8000-000000000001/"+kind, ""); rec.Code != 404 {
			t.Errorf("%s of hidden photo: %d", kind, rec.Code)
		}
	}
	// arrive: pin 3 at the destination; pin 1 stays hidden, 3 shows
	a.db.Exec(`UPDATE holidays SET dest_lat = 50, dest_lng = 0`)
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES (3, 1, 'photo', 50, 0, '2026-07-01T14:00:00Z')`)
	a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (3, '00000000-0000-4000-8000-000000000003', '2026-07-01T14:00:00Z', 50, 0)`)
	out.Pins = nil
	json.Unmarshal(serve(mux, "GET", "/api/share/"+token, "").Body.Bytes(), &out)
	if len(out.Pins) != 2 {
		t.Errorf("after arrival: %d pins, want 2 (hand 2 + arrival 3)", len(out.Pins))
	}
	for _, kind := range []string{"thumb", "preview"} {
		if rec := serve(mux, "GET", "/api/share/"+token+"/photo/00000000-0000-4000-8000-000000000003/"+kind, ""); rec.Code != 200 {
			t.Errorf("%s: %d", kind, rec.Code)
		}
	}
	for _, kind := range []string{"original", "Original", "ORIGINAL", "video", "x"} {
		rec := serve(mux, "GET", "/api/share/"+token+"/photo/00000000-0000-4000-8000-000000000003/"+kind, "")
		if rec.Code == 200 {
			t.Errorf("share kind %q streamed: %d", kind, rec.Code)
		}
		if kind == "original" && (rec.Code != 403 || !strings.Contains(rec.Body.String(), "not available on a share link")) {
			t.Errorf("original: %d %s", rec.Code, rec.Body)
		}
	}
	// logged-in original still works
	req := httptest.NewRequest("GET", "/api/photo/00000000-0000-4000-8000-000000000003/original", nil)
	req.SetPathValue("asset", "00000000-0000-4000-8000-000000000003")
	req.SetPathValue("kind", "original")
	rec = httptest.NewRecorder()
	a.handlePhoto(rec, req)
	if rec.Code != 200 {
		t.Errorf("logged-in original: %d", rec.Code)
	}
}

// ---------- P1 ----------

func brkServer(t *testing.T) *httptest.Server {
	big := strings.Repeat(`{"title":"a pin","lat":50.1,"lng":-1.2},`, 200)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/big", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"pins": big})
	})
	mux.HandleFunc("POST /api/big", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]string{"pins": big})
	})
	mux.HandleFunc("GET /api/small", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]int{"n": 1})
	})
	mux.Handle("GET /", newStaticServer())
	srv := httptest.NewServer(requestLog(securityHeaders(gzipJSON(mux))))
	t.Cleanup(srv.Close)
	return srv
}

func brkFetch(t *testing.T, srv *httptest.Server, method, path, enc string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	if enc != "" {
		req.Header.Set("Accept-Encoding", enc)
	}
	c := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func TestBreakGzipRealServer(t *testing.T) {
	srv := brkServer(t)
	resp, raw := brkFetch(t, srv, "GET", "/api/big", "gzip")
	if resp.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("big: CE %q Vary %q", resp.Header.Get("Content-Encoding"), resp.Header.Get("Vary"))
	}
	if resp.ContentLength != -1 && resp.ContentLength != int64(len(raw)) {
		t.Errorf("Content-Length %d, body %d", resp.ContentLength, len(raw))
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if !json.Valid(plain) {
		t.Errorf("single gunzip isn't JSON: %.40q", plain)
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("security headers lost")
	}

	resp, raw = brkFetch(t, srv, "POST", "/api/big", "gzip")
	if resp.StatusCode != 201 || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("POST big: %d CE %q", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}

	resp, raw = brkFetch(t, srv, "GET", "/api/small", "gzip")
	if resp.Header.Get("Content-Encoding") != "" || !json.Valid(raw) {
		t.Errorf("small gzipped: CE %q", resp.Header.Get("Content-Encoding"))
	}
	if resp.ContentLength != -1 && resp.ContentLength != int64(len(raw)) {
		t.Errorf("small Content-Length %d, body %d", resp.ContentLength, len(raw))
	}

	resp, raw = brkFetch(t, srv, "HEAD", "/api/big", "gzip")
	if resp.StatusCode != 200 || len(raw) != 0 {
		t.Errorf("HEAD: %d body %d", resp.StatusCode, len(raw))
	}
	t.Logf("HEAD big: CE %q CL %d", resp.Header.Get("Content-Encoding"), resp.ContentLength)

	for _, enc := range []string{"gzip;q=0.0", "gzip; q=0.000", "identity"} {
		resp, _ = brkFetch(t, srv, "GET", "/api/big", enc)
		if resp.Header.Get("Content-Encoding") != "" {
			t.Errorf("Accept-Encoding %q still got gzip", enc)
		}
	}

	// static: the pre-gzipped file goes out gzipped once, not twice
	resp, raw = brkFetch(t, srv, "GET", "/", "gzip")
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		plain, _ := io.ReadAll(zr)
		if !bytes.Contains(bytes.ToLower(plain[:min(len(plain), 200)]), []byte("<!doctype")) {
			t.Errorf("index.html after one gunzip: %.40q", plain)
		}
	}
	if resp.ContentLength != -1 && resp.ContentLength != int64(len(raw)) {
		t.Errorf("static Content-Length %d, body %d", resp.ContentLength, len(raw))
	}
}

// ---------- P4 ----------

func brkP4App(t *testing.T) *app {
	p := filepath.Join(t.TempDir(), "p4 test.db")
	db, err := openDB(p)
	if err != nil {
		t.Fatal(err)
	}
	rdb, err := openReadDB(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Close(); db.Close() })
	a := &app{db: db, rdb: rdb, immich: newImmichClient("http://127.0.0.1:1", ""), lastSync: map[int64]time.Time{}}
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Trip', '#123456', '2026-07-01T00:00:00Z', '2026-07-08T23:59:59Z')`)
	return a
}

func brkMap(a *app) (int, []byte) {
	req := httptest.NewRequest("GET", "/api/map", nil)
	req = req.WithContext(context.WithValue(req.Context(), userKey, sessionUser{Username: "ada", IsAdmin: true}))
	rec := httptest.NewRecorder()
	a.handleMap(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func brkPin(a *app, i int) (int, int64, string) {
	b := fmt.Sprintf(`{"holiday_id":1,"lat":50,"lng":%f,"title":"p%d"}`, float64(i)/1000, i)
	req := httptest.NewRequest("POST", "/api/pins", strings.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.handleCreatePin(rec, req)
	var p pinOut
	json.Unmarshal(rec.Body.Bytes(), &p)
	return rec.Code, p.ID, rec.Body.String()
}

func TestBreakReadYourWrites(t *testing.T) {
	a := brkP4App(t)
	if _, err := a.rdb.Exec(`INSERT INTO holidays (name, color, start_at) VALUES ('x', '#000000', '2026-01-01T00:00:00Z')`); err == nil {
		t.Errorf("read pool accepted a write")
	}
	for i := 0; i < 50; i++ {
		code, id, body := brkPin(a, i)
		if code != 201 {
			t.Fatalf("pin %d: %d %s", i, code, body)
		}
		code, m := brkMap(a)
		var out struct {
			Pins []pinOut `json:"pins"`
		}
		json.Unmarshal(m, &out)
		found := false
		for _, p := range out.Pins {
			if p.ID == id {
				found = true
			}
		}
		if code != 200 || !found || len(out.Pins) != i+1 {
			t.Fatalf("loop %d: map %d, pin %d found %v, %d pins", i, code, id, found, len(out.Pins))
		}
	}
}

func TestBreakConcurrentWritesReads(t *testing.T) {
	a := brkP4App(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	note := func(s string) { mu.Lock(); errs = append(errs, s); mu.Unlock() }
	for w := 0; w < 6; w++ {
		wg.Add(2)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if code, _, body := brkPin(a, w*100+i); code != 201 {
					note(fmt.Sprintf("write %d: %s", code, body))
				}
			}
		}(w)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if code, body := brkMap(a); code != 200 {
					note(fmt.Sprintf("read %d: %s", code, body))
				}
			}
		}()
	}
	wg.Wait()
	for _, e := range errs {
		t.Error(e)
	}
	_, m := brkMap(a)
	var out struct {
		Pins []pinOut `json:"pins"`
	}
	json.Unmarshal(m, &out)
	if len(out.Pins) != 120 {
		t.Errorf("final map: %d pins, want 120", len(out.Pins))
	}
}

// ---------- R6 ----------

func TestBreakHealthcheckEdges(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
		w.Write([]byte("ok"))
	}))
	defer slow.Close()
	t.Setenv("LISTEN_ADDR", strings.TrimPrefix(slow.URL, "http://"))
	start := time.Now()
	if code := runHealthcheck(); code != 1 || time.Since(start) > 4*time.Second {
		t.Errorf("slow server: exit %d after %v", code, time.Since(start))
	}
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(404)
	}))
	defer ok.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(ok.URL, "http://"), ":")
	t.Setenv("LISTEN_ADDR", ":"+port)
	if code := runHealthcheck(); code != 0 {
		t.Errorf("LISTEN_ADDR \":%s\": exit %d", port, code)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	defer redirect.Close()
	t.Setenv("LISTEN_ADDR", strings.TrimPrefix(redirect.URL, "http://"))
	if code := runHealthcheck(); code != 1 {
		t.Errorf("204 (not 200): exit %d, want 1", code)
	}
}
