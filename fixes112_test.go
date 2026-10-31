package main

// Tests for the v1.13.1 fixes (task 112). Each one fails on v1.13.0.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func loginFrom(a *app, user, pass, xff string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/login",
		bytes.NewBufferString(`{"username":"`+user+`","password":"`+pass+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:5000"
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)
	return rec
}

func addUser(t *testing.T, a *app, name string) {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte(name+"-password"), bcrypt.MinCost)
	if _, err := a.db.Exec(`INSERT INTO users (username, password_hash) VALUES (?, ?)`, name, hash); err != nil {
		t.Fatal(err)
	}
}

// B3: iOS capitalises the first letter, and autocomplete can add a space.
func TestLoginIgnoresCaseAndSpaces(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	addUser(t, a, "Kayleigh")
	for _, typed := range []string{"Kayleigh", "kayleigh", "KAYLEIGH", " kayleigh "} {
		rec := loginFrom(a, typed, "Kayleigh-password", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("login as %q: %d %s", typed, rec.Code, rec.Body)
		}
		var u sessionUser
		json.Unmarshal(rec.Body.Bytes(), &u)
		if u.Username != "Kayleigh" {
			t.Errorf("login as %q answered username %q, want the stored name", typed, u.Username)
		}
	}
	if rec := loginFrom(a, "kayleigh", "kayleigh-password", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("the password stays case-sensitive: got %d", rec.Code)
	}
}

// S3: one device can't walk through the family's names.
func TestLoginLimitPerDevice(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	a.ipLimiter = newIPLimiter()
	addUser(t, a, "mum")
	attacker := "198.51.100.7, 100.64.0.9" // the proxy appended the last entry
	for i := 0; i < ipMaxFails; i++ {
		// a different name each time, so no account lockout kicks in
		if rec := loginFrom(a, "guess"+string(rune('a'+i)), "x", attacker); rec.Code != http.StatusUnauthorized {
			t.Fatalf("try %d: %d", i, rec.Code)
		}
	}
	rec := loginFrom(a, "mum", "mum-password", attacker)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("21st try from the device: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	// a forged first entry doesn't buy a fresh allowance
	if rec := loginFrom(a, "mum", "mum-password", "203.0.113.1, 100.64.0.9"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("forged X-Forwarded-For prefix: %d, want 429", rec.Code)
	}
	if rec := loginFrom(a, "mum", "mum-password", "100.64.0.10"); rec.Code != http.StatusOK {
		t.Errorf("another family device: %d, want 200", rec.Code)
	}
	// no X-Forwarded-For: every caller looks like the proxy, so no shared budget
	b := newTestApp(t)
	b.limiter, b.ipLimiter = newLoginLimiter(), newIPLimiter()
	addUser(t, b, "dad")
	for i := 0; i < ipMaxFails+5; i++ {
		loginFrom(b, "guess"+string(rune('a'+i)), "x", "")
	}
	if rec := loginFrom(b, "dad", "dad-password", ""); rec.Code != http.StatusOK {
		t.Errorf("without X-Forwarded-For the family must not share one budget: %d", rec.Code)
	}
}

// S4: a cross-site form or fetch can't drive a POST with the family's cookie.
func TestCrossSiteWritesRefused(t *testing.T) {
	a := newTestApp(t)
	a.limiter = newLoginLimiter()
	addUser(t, a, "mum")
	cookie := loginFrom(a, "mum", "mum-password", "").Result().Cookies()[0]
	reached := 0
	h := a.auth(func(w http.ResponseWriter, r *http.Request) { reached++; w.WriteHeader(200) })
	cases := []struct {
		method, site, origin string
		want                 int
	}{
		{"POST", "cross-site", "", 403},
		{"POST", "same-site", "", 403}, // another app on the same tailnet name
		{"DELETE", "cross-site", "", 403},
		{"POST", "", "https://evil.example", 403},
		{"POST", "", "null", 403},
		{"POST", "same-origin", "", 200},
		{"POST", "none", "", 200},
		{"POST", "", "http://holiday.test", 200},
		{"POST", "", "https://home.example:8448", 200}, // via X-Forwarded-Host below
		{"POST", "", "", 200},           // curl, old WebViews
		{"GET", "cross-site", "", 200}, // reads are fine, CORS keeps the body
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://holiday.test/api/holidays/1/end", nil)
		req.AddCookie(cookie)
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		req.Header.Set("X-Forwarded-Host", "home.example:8448")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s site=%q origin=%q: %d, want %d", c.method, c.site, c.origin, rec.Code, c.want)
		}
	}
	if reached != 6 {
		t.Errorf("handler reached %d times, want 6", reached)
	}
}

// shareMux mounts the share routes the way main does.
func shareMux(a *app) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/holidays/{id}/share", a.handleCreateShare)
	mux.HandleFunc("GET /api/share/{token}", a.handleShareData)
	mux.HandleFunc("GET /api/share/{token}/pins/{id}/photos", a.handleSharePinPhotos)
	mux.HandleFunc("GET /api/share/{token}/photo/{asset}/{kind}", a.handleSharePhoto)
	return mux
}

func serve(mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func makeShare(t *testing.T, mux http.Handler, body string) (string, map[string]any) {
	t.Helper()
	rec := serve(mux, "POST", "/api/holidays/1/share", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("share %q: %d %s", body, rec.Code, rec.Body)
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return strings.TrimPrefix(out["url"].(string), "/share/"), out
}

// S5: links can expire, and an expired link says so.
func TestShareLinksExpire(t *testing.T) {
	a := newTestApp(t)
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Trip', '#123456', '2026-07-01T00:00:00Z', '2026-07-08T23:59:59Z')`)
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES (1, 1, 'manual', 50, 0, '2026-07-02T10:00:00Z')`)
	mux := shareMux(a)

	_, out := makeShare(t, mux, "")
	if v, ok := out["expires_at"]; !ok || v != nil {
		t.Errorf("no body: expires_at = %v (present %v), want null", v, ok)
	}
	for _, bad := range []string{`{"days":3}`, `{"days":-1}`, `{"days":"7"}`} {
		if rec := serve(mux, "POST", "/api/holidays/1/share", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, rec.Code)
		}
	}
	token, out := makeShare(t, mux, `{"days":7}`)
	exp, err := time.Parse(time.RFC3339, out["expires_at"].(string))
	if err != nil || exp.Sub(time.Now()) < 6*24*time.Hour || exp.Sub(time.Now()) > 7*24*time.Hour+time.Minute {
		t.Errorf("7 days: expires_at %v", out["expires_at"])
	}
	if rec := serve(mux, "GET", "/api/share/"+token, ""); rec.Code != http.StatusOK {
		t.Fatalf("fresh link: %d", rec.Code)
	}
	hs := listHolidays(t, a)
	if h := hs["Trip"]; !h.Shared || h.ShareExpires == nil {
		t.Errorf("trip list: shared %v, expires %v", h.Shared, h.ShareExpires)
	}

	a.db.Exec(`UPDATE shares SET expires_at = '2020-01-01T00:00:00Z'`)
	for _, p := range []string{"", "/pins/1/photos", "/photo/a1/thumb"} {
		if rec := serve(mux, "GET", "/api/share/"+token+p, ""); rec.Code != http.StatusGone {
			t.Errorf("expired %q: %d, want 410", p, rec.Code)
		}
	}
	if rec := serve(mux, "GET", "/api/share/ffffffffffffffffffffffffffffffff", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown token: %d, want 404", rec.Code)
	}
	if h := listHolidays(t, a)["Trip"]; h.Shared {
		t.Errorf("an expired link still shows as shared")
	}
	// a new link replaces the expired one and its expiry
	token, _ = makeShare(t, mux, `{"days":0}`)
	if rec := serve(mux, "GET", "/api/share/"+token, ""); rec.Code != http.StatusOK {
		t.Errorf("new never-expiring link after an expired one: %d", rec.Code)
	}
}

// S2: a link-holder sees neither the photos near home on the way there nor
// any original file.
func TestShareHidesTheWayThereAndOriginals(t *testing.T) {
	immich := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("jpeg"))
	}))
	defer immich.Close()
	a := newTestApp(t)
	a.immich = newImmichClient(immich.URL, "key")
	// home at 52,-1; the destination is 50,0 and pin 3 is the arrival
	a.db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at, dest_name, dest_lat, dest_lng, cover_asset)
		VALUES (1, 'Trip', '#123456', '2026-07-01T00:00:00Z', '2026-07-08T23:59:59Z', 'Seaside', 50, 0, '00000000-0000-4000-8000-000000000001')`)
	a.db.Exec(`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES
		(1, 1, 'photo', 52, -1, '2026-07-01T08:00:00Z'),
		(2, 1, 'manual', 51, -0.5, '2026-07-01T10:00:00Z'),
		(3, 1, 'photo', 50, 0, '2026-07-01T14:00:00Z')`)
	a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES
		(1, '00000000-0000-4000-8000-000000000001', '2026-07-01T08:00:00Z', 52, -1),
		(1, '00000000-0000-4000-8000-000000000002', '2026-07-01T08:05:00Z', 52, -1),
		(3, '00000000-0000-4000-8000-000000000003', '2026-07-01T14:00:00Z', 50, 0),
		(3, '00000000-0000-4000-8000-000000000004', '2026-07-02T10:00:00Z', 50, 0)`)
	mux := shareMux(a)
	token, _ := makeShare(t, mux, "")

	rec := serve(mux, "GET", "/api/share/"+token, "")
	var out struct {
		Holiday holidayOut `json:"holiday"`
		Pins    []pinOut   `json:"pins"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	ids := []int64{}
	for _, p := range out.Pins {
		ids = append(ids, p.ID)
	}
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 3 {
		t.Fatalf("shared pins %v, want [2 3]: the hand pin stays, the photo pin at home goes", ids)
	}
	h := out.Holiday
	if h.PinCount != 2 || h.PhotoCount != 2 || h.ArrivalPin != 3 {
		t.Errorf("shared trip counts: %d pins, %d photos, arrival %d", h.PinCount, h.PhotoCount, h.ArrivalPin)
	}
	if h.CoverAsset != "00000000-0000-4000-8000-000000000003" {
		t.Errorf("cover %q: the chosen cover is a photo at home, so it must fall back to 00000000-0000-4000-8000-000000000003", h.CoverAsset)
	}
	if strings.Contains(rec.Body.String(), "00000000-0000-4000-8000-000000000001") || strings.Contains(rec.Body.String(), "52") {
		t.Errorf("the share JSON still mentions the home pin: %s", rec.Body)
	}
	if rec := serve(mux, "GET", "/api/share/"+token+"/pins/1/photos", ""); rec.Code != http.StatusNotFound {
		t.Errorf("photos of the hidden pin: %d, want 404", rec.Code)
	}
	if rec := serve(mux, "GET", "/api/share/"+token+"/photo/00000000-0000-4000-8000-000000000001/thumb", ""); rec.Code != http.StatusNotFound {
		t.Errorf("a hidden pin's photo: %d, want 404", rec.Code)
	}
	if rec := serve(mux, "GET", "/api/share/"+token+"/photo/00000000-0000-4000-8000-000000000003/original", ""); rec.Code != http.StatusForbidden {
		t.Errorf("original on a share: %d, want 403", rec.Code)
	}
	for _, kind := range []string{"thumb", "preview"} {
		if rec := serve(mux, "GET", "/api/share/"+token+"/photo/00000000-0000-4000-8000-000000000003/"+kind, ""); rec.Code != http.StatusOK {
			t.Errorf("%s of a shown photo: %d, want 200", kind, rec.Code)
		}
	}

	// going back to the home-side pin after arriving puts it on the map
	a.db.Exec(`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (1, '00000000-0000-4000-8000-000000000005', '2026-07-05T12:00:00Z', 52, -1)`)
	json.Unmarshal(serve(mux, "GET", "/api/share/"+token, "").Body.Bytes(), &out)
	if len(out.Pins) != 3 {
		t.Errorf("a pin revisited after arriving: %d pins shown, want 3", len(out.Pins))
	}
	// a trip without a destination isn't split at all
	a.db.Exec(`DELETE FROM pin_photos WHERE asset_id = '00000000-0000-4000-8000-000000000005'`)
	a.db.Exec(`UPDATE holidays SET dest_name = ''`)
	json.Unmarshal(serve(mux, "GET", "/api/share/"+token, "").Body.Bytes(), &out)
	if len(out.Pins) != 3 {
		t.Errorf("no destination: %d pins shown, want all 3", len(out.Pins))
	}
	// the family's own map is untouched by any of this
	if _, _, pins := getMap(t, a); len(pins) != 3 {
		t.Errorf("own map: %d pins", len(pins))
	}
}

// P1: API JSON goes out gzipped to clients that take it.
func TestAPIJSONIsGzipped(t *testing.T) {
	big := strings.Repeat(`{"title":"a pin","lat":50.1,"lng":-1.2},`, 100)
	h := gzipJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/small" {
			writeJSON(w, http.StatusCreated, map[string]int{"n": 1})
			return
		}
		if strings.Contains(r.URL.Path, "/photo/") {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte(big))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"pins": big})
	}))
	get := func(path, enc string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if enc != "" {
			req.Header.Set("Accept-Encoding", enc)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := get("/api/map", "gzip, deflate, br")
	if rec.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("map: encoding %q vary %q", rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	var out map[string]string
	if err := json.Unmarshal(plain, &out); err != nil || out["pins"] != big {
		t.Errorf("gunzipped body doesn't round-trip: %v", err)
	}
	if rec.Body.Len() > len(plain)/4 {
		t.Errorf("gzip %d bytes vs %d plain", rec.Body.Len(), len(plain))
	}
	if rec := get("/api/map", ""); rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("no Accept-Encoding still got gzip")
	}
	if rec := get("/api/map", "gzip;q=0"); rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("gzip;q=0 still got gzip")
	}
	if rec := get("/api/small", "gzip"); rec.Header().Get("Content-Encoding") != "" || rec.Code != http.StatusCreated {
		t.Errorf("small body: encoding %q status %d", rec.Header().Get("Content-Encoding"), rec.Code)
	}
	if rec := get("/api/photo/x/thumb", "gzip"); rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != big {
		t.Errorf("photos must stream untouched")
	}
}

// P3: the lookups by owner/trip have indexes.
func TestFixIndexesExist(t *testing.T) {
	a := newTestApp(t)
	for _, idx := range []string{"idx_sessions_user", "idx_set_aside_holiday", "idx_photo_choices_holiday"} {
		var n int
		a.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n)
		if n != 1 {
			t.Errorf("index %s missing", idx)
		}
	}
}

// P4: the map answers from the read pool while the writer is busy (a photo
// sync holds it for seconds), and opening it writes nothing.
func TestMapReadsWhileWriterBusy(t *testing.T) {
	path := t.TempDir() + "/test.db"
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rdb, err := openReadDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rdb.Close() })
	a := &app{db: db, rdb: rdb, immich: newImmichClient("http://127.0.0.1:1", ""), lastSync: map[int64]time.Time{}}
	db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Trip', '#123456', '2026-07-01T00:00:00Z', '2026-07-08T23:59:59Z')`)

	// hold the write lock on another connection, as a long sync would
	w, err := openReadDB(path) // a plain connection; query_only is lifted below
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	conn, _ := w.Conn(context.Background())
	defer conn.Close()
	conn.ExecContext(context.Background(), `PRAGMA query_only = 0`)
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	conn.ExecContext(context.Background(), `UPDATE holidays SET name = 'Trip' WHERE id = 1`)

	start := time.Now()
	_, hs, _ := getMap(t, a)
	if d := time.Since(start); d > time.Second || len(hs) != 1 {
		t.Errorf("map with the writer busy: %v, %d trips; want under 1 s", d, len(hs))
	}
	conn.ExecContext(context.Background(), `ROLLBACK`)

	// and a planned trip whose day has come still goes live on the next look
	db.Exec(`UPDATE holidays SET end_at = NULL, planned = 1, start_at = '2026-01-01T00:00:00Z'`)
	if _, hs, _ := getMap(t, a); !hs[0].Active {
		t.Errorf("a due planned trip didn't go live: %+v", hs[0])
	}
}

// R5: errors and slow requests log one line, and never a share token.
func TestRequestLogLine(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(io.Discard)
	h := requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ok" {
			w.Write([]byte("ok"))
			return
		}
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	for _, p := range []string{"/api/ok", "/api/share/0123456789abcdef0123456789abcdef/photo/a/thumb"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	out := buf.String()
	if strings.Contains(out, "/api/ok") {
		t.Errorf("a fast 200 was logged: %s", out)
	}
	if !strings.Contains(out, "GET /api/share/…/photo/a/thumb 502") {
		t.Errorf("the 502 line is missing or leaks the token: %q", out)
	}
	if strings.Contains(out, "0123456789abcdef") {
		t.Errorf("share token in the log: %s", out)
	}
}

// R6: the binary can answer Docker's healthcheck itself.
func TestHealthcheckFlag(t *testing.T) {
	healthy := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" || !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	t.Setenv("LISTEN_ADDR", strings.TrimPrefix(srv.URL, "http://"))
	if code := runHealthcheck(); code != 0 {
		t.Errorf("healthy server: exit %d", code)
	}
	healthy = false
	if code := runHealthcheck(); code != 1 {
		t.Errorf("503: exit %d, want 1", code)
	}
	srv.Close()
	if code := runHealthcheck(); code != 1 {
		t.Errorf("nothing listening: exit %d, want 1", code)
	}
}

type fakeRT struct {
	calls atomic.Int32
	fail  error
}

func (f *fakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	n := f.calls.Add(1)
	if n == 1 && f.fail != nil {
		return nil, f.fail
	}
	body, _ := io.ReadAll(r.Body)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
}

// R3: right after a reboot immich-server may not resolve yet; one retry
// covers it, and other failures are not retried.
func TestImmichRetriesOnceOnDNS(t *testing.T) {
	log.SetOutput(io.Discard)
	dns := &fakeRT{fail: &net.DNSError{Err: "no such host", Name: "immich-server", IsNotFound: true}}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://immich-server:2283/api/search/metadata", strings.NewReader(`{"page":1}`))
	resp, err := immichTransport{dns}.RoundTrip(req)
	if err != nil || dns.calls.Load() != 2 {
		t.Fatalf("DNS failure: err %v after %d calls, want success after 2", err, dns.calls.Load())
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != `{"page":1}` {
		t.Errorf("the retry lost the POST body: %q", body)
	}
	refused := &fakeRT{fail: errors.New("connection refused")}
	req, _ = http.NewRequest("GET", "http://immich-server:2283/api/x", nil)
	if _, err := (immichTransport{refused}).RoundTrip(req); err == nil || refused.calls.Load() != 1 {
		t.Errorf("non-DNS error: err %v, %d calls, want 1 and the error", err, refused.calls.Load())
	}
}

// S3: inside Docker, tailscale serve's requests arrive from the network
// gateway; that address is read from /proc/net/route.
func TestParseDefaultGateway(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t010012AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t000012AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	if gw := parseDefaultGateway(table); gw == nil || gw.String() != "172.18.0.1" {
		t.Errorf("gateway %v, want 172.18.0.1", gw)
	}
	if gw := parseDefaultGateway("Iface\tDestination\tGateway\n"); gw != nil {
		t.Errorf("no default route: %v", gw)
	}
	// a LAN device's own X-Forwarded-For is ignored
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.RemoteAddr = "192.168.1.50:4444"
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	if ip := clientIP(r); ip != "192.168.1.50" {
		t.Errorf("LAN peer with forged XFF keyed as %q", ip)
	}
}
