package main

// Task 133: the TV door and pairing, tested through the same handlers main()
// runs (mainMux, tvDoor), over real HTTP so cookies and the long-poll are real.

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	tvA1 = "00000000-0000-4000-8000-000000000001" // way-there photo pin: hidden
	tvA2 = "00000000-0000-4000-8000-000000000002" // arrival pin
	tvA3 = "00000000-0000-4000-8000-000000000003" // arrival pin, a video
	tvA4 = "00000000-0000-4000-8000-000000000004" // hand pin
	tvA5 = "00000000-0000-4000-8000-000000000005" // planned trip
)

type tvFix struct {
	a          *app
	main, door *httptest.Server
	session    string
	immichHits []string
	mu         sync.Mutex
}

func newTVFix(t *testing.T) *tvFix {
	t.Helper()
	f := &tvFix{a: newTestApp(t)}
	immich := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.immichHits = append(f.immichHits, r.URL.String())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("jpeg:" + r.URL.String()))
	}))
	t.Cleanup(immich.Close)
	f.a.immich = newImmichClient(immich.URL, "key")
	f.a.lastSync = map[int64]time.Time{}
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, is_admin) VALUES (1, 'giedrius', 'x', 1)`,
		`INSERT INTO sessions (token, user_id, expires_at) VALUES ('` + hashToken("sess-1") + `', 1, '2099-01-01T00:00:00Z')`,
		`INSERT INTO holidays (id, name, color, start_at, end_at, dest_name, dest_lat, dest_lng, journal, created_by, journal_by)
		 VALUES (1, 'Lakes', '#123456', '2026-07-01T00:00:00Z', '2026-07-08T23:59:59Z', 'Keswick', 54.6, -3.13, 'lovely', 'giedrius', 'giedrius')`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, created_at, created_by) VALUES (1, 1, 'photo', 'a', 52.0, -1.0, '2026-07-01T08:00:00Z', 'giedrius')`,
		`INSERT INTO pins (id, holiday_id, kind, cluster_key, lat, lng, created_at, created_by) VALUES (2, 1, 'photo', 'b', 54.6, -3.13, '2026-07-02T08:00:00Z', 'giedrius')`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, title, note, created_at, created_by) VALUES (3, 1, 'manual', 54.5, -3.0, 'Catbells', 'windy', '2026-07-04T08:00:00Z', 'giedrius')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (1, '` + tvA1 + `', '2026-07-01T08:00:00Z', 52.0, -1.0)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (2, '` + tvA2 + `', '2026-07-02T08:00:00Z', 54.6, -3.13)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng, type, duration) VALUES (2, '` + tvA3 + `', '2026-07-03T08:00:00Z', 54.6, -3.13, 'video', 12.5)`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (3, '` + tvA4 + `', '2026-07-04T09:00:00Z', 54.5, -3.0)`,
		`INSERT INTO holidays (id, name, color, start_at, planned) VALUES (2, 'Future', '#654321', '2099-01-01T00:00:00Z', 1)`,
		`INSERT INTO pins (id, holiday_id, kind, lat, lng, created_at) VALUES (4, 2, 'manual', 40.0, 2.0, '2099-01-01T08:00:00Z')`,
		`INSERT INTO pin_photos (pin_id, asset_id, taken_at, lat, lng) VALUES (4, '` + tvA5 + `', '2099-01-01T08:00:00Z', 40.0, 2.0)`,
		`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (3, 'Empty', '#000000', '2025-05-01T00:00:00Z', '2025-05-02T00:00:00Z')`,
		// a share link the family already sent: pairing must never touch it
		`INSERT INTO shares (token, holiday_id) VALUES ('` + hashToken("old-share") + `', 1)`,
	} {
		if _, err := f.a.db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	static := newStaticServer()
	f.main = httptest.NewServer(requestLog(securityHeaders(gzipJSON(f.a.mainMux(static)))))
	f.door = httptest.NewServer(f.a.tvDoor(static))
	t.Cleanup(f.main.Close)
	t.Cleanup(f.door.Close)
	f.session = "sess-1"
	return f
}

type tvResp struct {
	code   int
	body   []byte
	header http.Header
	cookie []*http.Cookie
}

func (r tvResp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %d %s: %v", r.code, r.body, err)
	}
}

func tvDo(t *testing.T, base, method, path, body string, hdr map[string]string) tvResp {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return tvResp{resp.StatusCode, b, resp.Header, resp.Cookies()}
}

func (f *tvFix) signedIn() map[string]string {
	return map[string]string{"Cookie": sessionCookie + "=" + f.session}
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func (f *tvFix) startCode(t *testing.T) (code, poll string) {
	t.Helper()
	r := tvDo(t, f.door.URL, "POST", "/tv/pair", "", nil)
	if r.code != 200 {
		t.Fatalf("POST /tv/pair: %d %s", r.code, r.body)
	}
	var out struct {
		Code      string `json:"code"`
		Poll      string `json:"poll"`
		ExpiresIn int    `json:"expires_in"`
	}
	r.json(t, &out)
	if len(out.Code) != 4 || strings.Trim(out.Code, tvCodeLetters) != "" || len(out.Poll) < 32 || out.ExpiresIn != 300 {
		t.Fatalf("pair start: %+v", out)
	}
	return out.Code, out.Poll
}

// pair runs the whole pairing and returns the TV token from the cookie.
func (f *tvFix) pair(t *testing.T, name string) string {
	t.Helper()
	code, poll := f.startCode(t)
	got := make(chan tvResp, 1)
	go func() { got <- tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil) }()
	time.Sleep(50 * time.Millisecond) // the poll is waiting
	b, _ := json.Marshal(map[string]string{"code": " " + strings.ToLower(code[:2]) + " " + code[2:] + " ", "name": name})
	r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn())
	if r.code != 201 {
		t.Fatalf("POST /api/tv/pair: %d %s", r.code, r.body)
	}
	p := <-got
	if p.code != 200 {
		t.Fatalf("poll: %d %s", p.code, p.body)
	}
	for _, c := range p.cookie {
		if c.Name == tvCookie {
			return c.Value
		}
	}
	t.Fatalf("no %s cookie: %v", tvCookie, p.header)
	return ""
}

func TestTVPairingFlow(t *testing.T) {
	f := newTVFix(t)
	code, poll := f.startCode(t)
	got := make(chan tvResp, 1)
	go func() { got <- tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil) }()
	time.Sleep(50 * time.Millisecond)
	r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"`+strings.ToLower(code)+`","name":"  Living room  "}`, f.signedIn())
	if r.code != 201 {
		t.Fatalf("pair: %d %s", r.code, r.body)
	}
	var made struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	r.json(t, &made)
	if made.ID == 0 || made.Name != "Living room" {
		t.Fatalf("pair response %+v", made)
	}
	p := <-got
	if p.code != 200 || !strings.Contains(string(p.body), `"name":"Living room"`) {
		t.Fatalf("poll: %d %s", p.code, p.body)
	}
	raw := p.header.Get("Set-Cookie")
	for _, want := range []string{"ws_tv=", "HttpOnly", "SameSite=Strict", "Path=/", "Max-Age=34560000"} {
		if !strings.Contains(raw, want) {
			t.Errorf("Set-Cookie %q lacks %q", raw, want)
		}
	}
	if strings.Contains(raw, "Secure") {
		t.Errorf("Set-Cookie %q is Secure on a plain-HTTP door", raw)
	}
	var tok string
	for _, c := range p.cookie {
		if c.Name == tvCookie {
			tok = c.Value
		}
	}
	if len(tok) < 32 {
		t.Fatalf("token %q", tok)
	}
	// stored hashed, never raw
	var n int
	f.a.db.QueryRow(`SELECT COUNT(*) FROM tv_devices WHERE token_hash = ?`, tok).Scan(&n)
	if n != 0 {
		t.Error("raw token stored")
	}
	// the code and poll are gone after one collection
	if r := tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil); r.code != 410 {
		t.Errorf("second poll: %d, want 410", r.code)
	}
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"`+code+`","name":"Again"}`, f.signedIn()); r.code != 404 {
		t.Errorf("code reused: %d, want 404", r.code)
	}
	// cookie and Bearer both open the door
	for _, h := range []map[string]string{{"Cookie": tvCookie + "=" + tok}, bearer(tok), {"Authorization": "bearer " + tok}} {
		r := tvDo(t, f.door.URL, "GET", "/api/tv/me", "", h)
		if r.code != 200 || !strings.Contains(string(r.body), `"Living room"`) {
			t.Errorf("me with %v: %d %s", h, r.code, r.body)
		}
	}
	// the devices list
	r = tvDo(t, f.main.URL, "GET", "/api/tv/devices", "", f.signedIn())
	var devs []tvDeviceOut
	r.json(t, &devs)
	if len(devs) != 1 || devs[0].Name != "Living room" || devs[0].CreatedBy != "giedrius" || devs[0].LastSeen == "" || devs[0].CreatedAt == "" {
		t.Fatalf("devices: %s", r.body)
	}
	// the share link the family sent is untouched
	var shares int
	f.a.db.QueryRow(`SELECT COUNT(*) FROM shares WHERE token = ?`, hashToken("old-share")).Scan(&shares)
	if shares != 1 {
		t.Error("pairing touched the trip's share link")
	}
	// unpair: the very next TV request is 401
	if r := tvDo(t, f.main.URL, "DELETE", fmt.Sprintf("/api/tv/devices/%d", made.ID), "", f.signedIn()); r.code != 204 {
		t.Fatalf("unpair: %d %s", r.code, r.body)
	}
	if r := tvDo(t, f.door.URL, "GET", "/api/tv/me", "", bearer(tok)); r.code != 401 || !strings.Contains(string(r.body), "not paired") {
		t.Errorf("after unpair: %d %s", r.code, r.body)
	}
	if r := tvDo(t, f.door.URL, "GET", "/api/tv/trips", "", bearer(tok)); r.code != 401 {
		t.Errorf("trips after unpair: %d", r.code)
	}
	if r := tvDo(t, f.main.URL, "DELETE", fmt.Sprintf("/api/tv/devices/%d", made.ID), "", f.signedIn()); r.code != 404 {
		t.Errorf("unpair twice: %d, want 404", r.code)
	}
	if r := tvDo(t, f.main.URL, "DELETE", "/api/tv/devices/abc", "", f.signedIn()); r.code != 400 {
		t.Errorf("unpair bad id: %d, want 400", r.code)
	}
}

func TestTVManagementNeedsSignIn(t *testing.T) {
	f := newTVFix(t)
	code, _ := f.startCode(t)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/tv/pair", `{"code":"` + code + `","name":"x"}`},
		{"GET", "/api/tv/devices", ""},
		{"DELETE", "/api/tv/devices/1", ""},
	} {
		if r := tvDo(t, f.main.URL, c.method, c.path, c.body, nil); r.code != 401 {
			t.Errorf("%s %s signed out: %d, want 401", c.method, c.path, r.code)
		}
	}
	// a cross-site page can't pair a TV with the family's session
	h := f.signedIn()
	h["Sec-Fetch-Site"] = "cross-site"
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"`+code+`","name":"x"}`, h); r.code != 403 {
		t.Errorf("cross-site pair: %d, want 403", r.code)
	}
	// and the code still works for the real phone
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"`+code+`","name":"x"}`, f.signedIn()); r.code != 201 {
		t.Errorf("real pair after cross-site try: %d %s", r.code, r.body)
	}
}

func TestTVReadsNeedToken(t *testing.T) {
	f := newTVFix(t)
	for _, p := range []string{"/api/tv/me", "/api/tv/trips", "/api/tv/trips/1", "/api/tv/photo/" + tvA2 + "/preview"} {
		for _, h := range []map[string]string{nil, bearer("nope"), bearer(""), {"Cookie": tvCookie + "="}, {"Cookie": tvCookie + "=x"},
			{"Cookie": sessionCookie + "=" + f.session}, {"Authorization": "Basic Z2llZHJpdXM6eA=="}} {
			r := tvDo(t, f.door.URL, "GET", p, "", h)
			if r.code != 401 || !strings.Contains(string(r.body), "not paired") {
				t.Errorf("%s %v: %d %s, want 401 not paired", p, h, r.code, r.body)
			}
		}
	}
	if len(f.immichHits) != 0 {
		t.Errorf("Immich asked without a token: %v", f.immichHits)
	}
}

func TestTVTripsShape(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	r := tvDo(t, f.door.URL, "GET", "/api/tv/trips", "", bearer(tok))
	if r.code != 200 {
		t.Fatalf("trips: %d %s", r.code, r.body)
	}
	var trips []map[string]any
	r.json(t, &trips)
	if len(trips) != 2 || trips[0]["name"] != "Lakes" || trips[1]["name"] != "Empty" {
		t.Fatalf("trips (planned left out, newest first, empty kept): %s", r.body)
	}
	l := trips[0]
	if l["pin_count"] != 2.0 || l["photo_count"] != 3.0 {
		t.Errorf("Lakes counts without the way-there pin: pins %v photos %v", l["pin_count"], l["photo_count"])
	}
	if l["created_by"] != "" || l["journal_by"] != "" || l["shared"] != false || l["share_expires_at"] != nil {
		t.Errorf("share shape: created_by %v journal_by %v shared %v", l["created_by"], l["journal_by"], l["shared"])
	}
	if l["cover_asset"] == tvA1 {
		t.Error("cover is a hidden photo")
	}
	if strings.Contains(string(r.body), tvA1) || strings.Contains(string(r.body), tvA5) || strings.Contains(string(r.body), "Future") {
		t.Errorf("trips leak hidden or planned: %s", r.body)
	}

	r = tvDo(t, f.door.URL, "GET", "/api/tv/trips/1", "", bearer(tok))
	if r.code != 200 {
		t.Fatalf("trip 1: %d %s", r.code, r.body)
	}
	var trip struct {
		Holiday map[string]any   `json:"holiday"`
		Pins    []map[string]any `json:"pins"`
		Photos  []struct {
			AssetID  string  `json:"asset_id"`
			PinID    int64   `json:"pin_id"`
			TakenAt  string  `json:"taken_at"`
			Type     string  `json:"type"`
			Duration float64 `json:"duration"`
		} `json:"photos"`
	}
	r.json(t, &trip)
	if trip.Holiday["name"] != "Lakes" || len(trip.Pins) != 2 {
		t.Fatalf("trip 1: %s", r.body)
	}
	for _, p := range trip.Pins {
		if p["id"] == 1.0 || p["created_by"] != "" {
			t.Errorf("pin %v created_by %q", p["id"], p["created_by"])
		}
	}
	want := []string{tvA2, tvA3, tvA4}
	if len(trip.Photos) != 3 {
		t.Fatalf("photos: %+v", trip.Photos)
	}
	for i, p := range trip.Photos {
		if p.AssetID != want[i] {
			t.Errorf("photo %d = %s, want %s (taken_at order)", i, p.AssetID, want[i])
		}
	}
	if trip.Photos[1].Type != "video" || trip.Photos[1].Duration != 12.5 || trip.Photos[0].PinID != 2 || trip.Photos[2].PinID != 3 {
		t.Errorf("photo fields: %+v", trip.Photos)
	}
	if !strings.Contains(string(r.body), "windy") || !strings.Contains(string(r.body), "Catbells") {
		t.Error("the stop's title/note is missing")
	}
	for _, p := range []string{"/api/tv/trips/2", "/api/tv/trips/999", "/api/tv/trips/0"} {
		if r := tvDo(t, f.door.URL, "GET", p, "", bearer(tok)); r.code != 404 {
			t.Errorf("%s: %d, want 404", p, r.code)
		}
	}
	if r := tvDo(t, f.door.URL, "GET", "/api/tv/trips/x", "", bearer(tok)); r.code != 400 {
		t.Errorf("trip x: %d, want 400", r.code)
	}
	r = tvDo(t, f.door.URL, "GET", "/api/tv/trips/3", "", bearer(tok))
	if r.code != 200 || !strings.Contains(string(r.body), `"photos":[]`) || !strings.Contains(string(r.body), `"pins":[]`) {
		t.Errorf("empty trip: %d %s", r.code, r.body)
	}
}

func TestTVPhotoRules(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	cases := []struct {
		asset, kind string
		want        int
	}{
		{tvA2, "preview", 200},
		{tvA2, "thumb", 200},
		{tvA3, "preview", 200}, // a video's still
		{tvA4, "thumb", 200},
		{tvA2, "original", 403},
		{tvA2, "video", 403},
		{tvA3, "video", 403},
		{tvA2, "thumbnail", 400},
		{tvA2, "", 404}, // no such route
		{tvA1, "preview", 404},
		{tvA5, "preview", 404},
		{"00000000-0000-4000-8000-00000000ffff", "preview", 404},
		{"not-an-id", "preview", 400},
	}
	for _, c := range cases {
		r := tvDo(t, f.door.URL, "GET", "/api/tv/photo/"+c.asset+"/"+c.kind, "", bearer(tok))
		if r.code != c.want {
			t.Errorf("%s/%s: %d %s, want %d", c.asset, c.kind, r.code, r.body, c.want)
			continue
		}
		if c.want == 200 && r.header.Get("Cache-Control") != "private, max-age=86400" {
			t.Errorf("%s/%s Cache-Control %q", c.asset, c.kind, r.header.Get("Cache-Control"))
		}
	}
	for _, h := range f.immichHits {
		if strings.Contains(h, "original") || strings.Contains(h, "video") || strings.Contains(h, tvA1) || strings.Contains(h, tvA5) {
			t.Errorf("Immich was asked for %s", h)
		}
	}
	// the same asset moved to the planned trip is no longer shown
	f.a.db.Exec(`UPDATE pin_photos SET pin_id = 4 WHERE asset_id = ?`, tvA4)
	if r := tvDo(t, f.door.URL, "GET", "/api/tv/photo/"+tvA4+"/thumb", "", bearer(tok)); r.code != 404 {
		t.Errorf("photo moved to a planned trip: %d, want 404", r.code)
	}
}

// Everything the main listener answers must 404 on the door.
func TestTVDoorServesNothingElse(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	paths := []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/index.html"}, {"GET", "/app.js"}, {"GET", "/style.css"}, {"GET", "/sw.js"},
		{"GET", "/manifest.webmanifest"}, {"GET", "/healthz"}, {"GET", "/share/abc"},
		{"GET", "/api/me"}, {"POST", "/api/login"}, {"GET", "/api/map"}, {"GET", "/api/holidays"},
		{"GET", "/api/export"}, {"GET", "/api/users"}, {"GET", "/api/share/abc"},
		{"GET", "/api/photo/" + tvA2 + "/preview"}, {"GET", "/api/photo/" + tvA2 + "/original"},
		{"GET", "/api/tv/devices"}, {"POST", "/api/tv/pair"}, {"DELETE", "/api/tv/devices/1"},
		{"GET", "/tv/../index.html"}, {"GET", "/tv/%2e%2e/app.js"}, {"GET", "/leaflet/../app.js"},
		{"GET", "/TV"}, {"GET", "/api/tv/me/../../api/map"},
	}
	for _, p := range paths {
		for _, h := range []map[string]string{nil, bearer(tok), f.signedIn()} {
			r := tvDo(t, f.door.URL, p.method, p.path, "", h)
			if r.code != 404 && r.code != 405 {
				t.Errorf("door %s %s: %d %.80s, want 404", p.method, p.path, r.code, r.body)
			}
			if r.code >= 300 && r.code < 400 {
				// the mux cleans "..": the clean path must 404 on the door too
				loc := r.header.Get("Location")
				if !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") {
					t.Errorf("door %s %s redirects off the door: %q", p.method, p.path, loc)
				} else if r2 := tvDo(t, f.door.URL, p.method, loc, "", h); r2.code != 404 && r2.code != 405 {
					t.Errorf("door %s %s -> %s: %d, want 404", p.method, p.path, loc, r2.code)
				}
			}
			if strings.Contains(string(r.body), "Lakes") || strings.Contains(string(r.body), "giedrius") {
				t.Errorf("door %s %s leaked data: %.120s", p.method, p.path, r.body)
			}
		}
	}
	// the TV API isn't on the main listener
	for _, p := range []string{"/api/tv/me", "/api/tv/trips", "/api/tv/photo/" + tvA2 + "/preview"} {
		if r := tvDo(t, f.main.URL, "GET", p, "", bearer(tok)); r.code != 404 {
			t.Errorf("main %s: %d, want 404", p, r.code)
		}
	}
	if r := tvDo(t, f.main.URL, "POST", "/tv/pair", "", nil); r.code == 200 {
		t.Errorf("main POST /tv/pair: 200")
	}
	// what the door does serve, with the app's security headers
	sub, _ := fs.Sub(staticFiles, "static")
	var leaf string
	fs.WalkDir(sub, "leaflet", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && leaf == "" {
			leaf = "/" + p
		}
		return nil
	})
	for _, p := range []string{leaf, "/staatliches-latin.woff2", "/staatliches-latin-ext.woff2"} {
		r := tvDo(t, f.door.URL, "GET", p, "", nil)
		if r.code != 200 {
			t.Errorf("door %s: %d, want 200", p, r.code)
		}
		if r.header.Get("Content-Security-Policy") != csp || r.header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("door %s lacks the security headers", p)
		}
	}
	_, hasTV := fs.Stat(sub, "tv/index.html")
	r := tvDo(t, f.door.URL, "GET", "/tv", "", nil)
	r2 := tvDo(t, f.door.URL, "GET", "/tv/", "", nil)
	if hasTV == nil && (r.code != 200 || r2.code != 200 || !strings.Contains(r.header.Get("Content-Type"), "text/html")) {
		t.Errorf("/tv %d /tv/ %d with static/tv/index.html present", r.code, r2.code)
	}
	if hasTV != nil && (r.code != 404 || r2.code != 404) {
		t.Errorf("/tv %d /tv/ %d without static/tv/index.html", r.code, r2.code)
	}
	if !strings.Contains(csp, "img-src 'self' data: https://server.arcgisonline.com") {
		t.Error("CSP no longer allows the map tiles the TV page draws")
	}
}

func TestTVPollTimesOutAndExpires(t *testing.T) {
	f := newTVFix(t)
	old := tvPollWait
	tvPollWait = 150 * time.Millisecond
	defer func() { tvPollWait = old }()
	_, poll := f.startCode(t)
	start := time.Now()
	r := tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil)
	if r.code != 202 || !strings.Contains(string(r.body), `"pending":true`) || time.Since(start) < 100*time.Millisecond {
		t.Errorf("waiting poll: %d %s after %v", r.code, r.body, time.Since(start))
	}
	if r.header.Get("Set-Cookie") != "" {
		t.Error("cookie on a pending poll")
	}
	for _, p := range []string{"nope", strings.Repeat("a", 48), poll + "x"} {
		if r := tvDo(t, f.door.URL, "GET", "/tv/pair/"+p, "", nil); r.code != 410 {
			t.Errorf("unknown poll %q: %d, want 410", p, r.code)
		}
	}
	// a code that runs out while the TV is waiting answers 410 at once
	tvPollWait = 5 * time.Second
	code, poll2 := f.startCode(t)
	p := f.a.pairs()
	p.mu.Lock()
	p.byPoll[poll2].expires = time.Now().Add(100 * time.Millisecond)
	p.mu.Unlock()
	start = time.Now()
	r = tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll2, "", nil)
	if r.code != 410 || time.Since(start) > 2*time.Second {
		t.Errorf("expiring poll: %d after %v, want 410 at expiry", r.code, time.Since(start))
	}
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"`+code+`","name":"x"}`, f.signedIn()); r.code != 404 {
		t.Errorf("expired code typed: %d, want 404", r.code)
	}
}

// Two polls racing for the same bound code: exactly one gets the token.
func TestTVTokenCollectedOnce(t *testing.T) {
	f := newTVFix(t)
	for round := 0; round < 5; round++ {
		code, poll := f.startCode(t)
		var wg sync.WaitGroup
		res := make([]tvResp, 4)
		for i := range res {
			wg.Add(1)
			go func(i int) { defer wg.Done(); res[i] = tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil) }(i)
		}
		time.Sleep(50 * time.Millisecond)
		if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"`+code+`","name":"race"}`, f.signedIn()); r.code != 201 {
			t.Fatalf("pair: %d %s", r.code, r.body)
		}
		wg.Wait()
		ok, gone := 0, 0
		for _, r := range res {
			switch r.code {
			case 200:
				ok++
			case 410:
				gone++
			}
		}
		if ok != 1 || gone != 3 {
			t.Fatalf("round %d: %d got the token, %d got 410", round, ok, gone)
		}
		// the open-code slot is free again
		f.a.pairs().mu.Lock()
		n := len(f.a.pairs().byCode)
		f.a.pairs().mu.Unlock()
		if n != 0 {
			t.Fatalf("round %d: %d codes left open", round, n)
		}
	}
}

func doorReq(a *app, method, path, peer string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = peer
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.tvDoor(newStaticServer()).ServeHTTP(rec, req)
	return rec
}

func TestTVCodeLimits(t *testing.T) {
	a := newTestApp(t)
	// one device holds at most 3 open codes, whatever X-Forwarded-For it types:
	// asking again replaces its oldest code, so a relaunched TV never waits
	var polls []string
	for i := 0; i < 5; i++ {
		rec := doorReq(a, "POST", "/tv/pair", "192.168.1.50:4000", map[string]string{"X-Forwarded-For": fmt.Sprintf("10.0.0.%d", i)})
		if rec.Code != 200 {
			t.Fatalf("code %d from one device: %d, want 200", i, rec.Code)
		}
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		polls = append(polls, out["poll"].(string))
		time.Sleep(2 * time.Millisecond) // distinct expiry times
	}
	p := a.pairs()
	p.mu.Lock()
	mine := len(p.byCode)
	_, firstLeft := p.byPoll[polls[0]]
	_, secondLeft := p.byPoll[polls[1]]
	_, lastLeft := p.byPoll[polls[4]]
	p.mu.Unlock()
	if mine != tvMaxOpenPeer || firstLeft || secondLeft || !lastLeft {
		t.Errorf("one device: %d open (want %d), first %v second %v last %v", mine, tvMaxOpenPeer, firstLeft, secondLeft, lastLeft)
	}
	if rec := doorReq(a, "GET", "/tv/pair/"+polls[0], "192.168.1.50:4000", nil); rec.Code != 410 {
		t.Errorf("replaced code's poll: %d, want 410", rec.Code)
	}
	// a code someone already typed is never the one replaced
	d := newTestApp(t)
	var bound string
	for i := 0; i < tvMaxOpenPeer; i++ {
		rec := doorReq(d, "POST", "/tv/pair", "192.168.1.51:4000", nil)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		if i == 0 {
			bound = out["poll"].(string)
		}
		time.Sleep(2 * time.Millisecond)
	}
	dp := d.pairs()
	dp.mu.Lock()
	for _, pr := range dp.byCode {
		pr.token = "t" // all three typed in, none collected yet
		pr.collectBy = time.Now().Add(time.Minute)
	}
	dp.mu.Unlock()
	if rec := doorReq(d, "POST", "/tv/pair", "192.168.1.51:4000", nil); rec.Code != 429 {
		t.Errorf("4th code with 3 typed-in ones: %d, want 429", rec.Code)
	}
	dp.mu.Lock()
	_, ok := dp.byPoll[bound]
	dp.mu.Unlock()
	if !ok {
		t.Error("a typed-in code was replaced")
	}
	a = newTestApp(t)
	for i := 0; i < tvMaxOpenPeer; i++ {
		doorReq(a, "POST", "/tv/pair", "192.168.1.50:4000", nil)
	}
	// all devices together hold at most 20
	got := 0
	for i := 0; i < 10; i++ {
		for j := 0; j < 3; j++ {
			if doorReq(a, "POST", "/tv/pair", fmt.Sprintf("192.168.1.%d:5000", 100+i), nil).Code == 200 {
				got++
			}
		}
	}
	if got != tvMaxOpen-tvMaxOpenPeer {
		t.Errorf("open codes from many devices: %d more, want %d", got, tvMaxOpen-tvMaxOpenPeer)
	}
	if rec := doorReq(a, "POST", "/tv/pair", "192.168.1.250:5000", nil); rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("21st open code: %d, want 429 + Retry-After", rec.Code)
	}
	// a device that keeps asking (collecting each) is held to 10 a minute
	b := newTestApp(t)
	n := 0
	for i := 0; i < 15; i++ {
		rec := doorReq(b, "POST", "/tv/pair", "192.168.1.60:4000", nil)
		if rec.Code == 200 {
			n++
			var out map[string]any
			json.Unmarshal(rec.Body.Bytes(), &out)
			p := b.pairs()
			p.mu.Lock()
			delete(p.byCode, out["code"].(string)) // as if expired
			p.mu.Unlock()
		}
	}
	if n != tvMaxNewPeer {
		t.Errorf("codes a minute from one device: %d, want %d", n, tvMaxNewPeer)
	}
	// cross-site pages can't eat the slots
	c := newTestApp(t)
	for _, h := range []map[string]string{{"Sec-Fetch-Site": "cross-site"}, {"Origin": "http://evil.example"}} {
		if rec := doorReq(c, "POST", "/tv/pair", "192.168.1.70:1", h); rec.Code != 403 {
			t.Errorf("cross-site %v: %d, want 403", h, rec.Code)
		}
	}
	if rec := doorReq(c, "POST", "/tv/pair", "192.168.1.70:1", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://example.com"}); rec.Code != 200 {
		t.Errorf("same-origin: %d", rec.Code)
	}
}

func TestTVWrongCodesAndNames(t *testing.T) {
	f := newTVFix(t)
	code, _ := f.startCode(t)
	for _, name := range []string{"", "   ", strings.Repeat("x", 41), "a\nb", "tab\there", "nul\x00", "line sep"} {
		b, _ := json.Marshal(map[string]string{"code": code, "name": name})
		if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn()); r.code != 400 {
			t.Errorf("name %q: %d, want 400", name, r.code)
		}
	}
	for _, body := range []string{`not json`, `{"code":1,"name":"x"}`} {
		if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", body, f.signedIn()); r.code != 400 {
			t.Errorf("body %s: %d, want 400", body, r.code)
		}
	}
	// bad names don't count as wrong codes, and don't use the code up
	for i := 0; i < tvMaxWrong; i++ {
		if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"ZZZZ","name":"x"}`, f.signedIn()); r.code != 404 && !(code == "ZZZZ") {
			t.Errorf("wrong code %d: %d, want 404", i, r.code)
		}
	}
	b, _ := json.Marshal(map[string]string{"code": code, "name": strings.Repeat("é", 40)})
	r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn())
	if r.code != 429 {
		t.Errorf("right code after %d wrong: %d, want 429", tvMaxWrong, r.code)
	}
	// once the window has passed, the 40-character name is fine
	p := f.a.pairs()
	p.mu.Lock()
	p.wrong = map[int64][]time.Time{}
	p.mu.Unlock()
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn()); r.code != 201 {
		t.Errorf("40 é: %d %s", r.code, r.body)
	}
}

func TestTVOrphanRowsAndLogs(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Kept")
	// bound but never collected (TV switched off mid-pairing)
	code, _ := f.startCode(t)
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"`+code+`","name":"Orphan"}`, f.signedIn()); r.code != 201 {
		t.Fatal(r.code)
	}
	f.a.db.Exec(`UPDATE tv_devices SET created_at = '2020-01-01T00:00:00Z'`)
	f.a.tvHousekeeping()
	var names []string
	rows, _ := f.a.db.Query(`SELECT name FROM tv_devices`)
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if len(names) != 1 || names[0] != "Kept" {
		t.Errorf("after housekeeping: %v, want [Kept]", names)
	}
	if r := tvDo(t, f.door.URL, "GET", "/api/tv/me", "", bearer(tok)); r.code != 200 {
		t.Errorf("collected TV after housekeeping: %d", r.code)
	}
	if got := logPath("/tv/pair/0123456789abcdef"); strings.Contains(got, "0123") {
		t.Errorf("logPath leaks the poll id: %s", got)
	}
	if !isStream("/tv/pair/x") || !isStream("/api/tv/photo/"+tvA2+"/preview") {
		t.Error("long-poll or TV photo is buffered")
	}
}

// last_seen is written at most once a minute, not on every request.
func TestTVLastSeenThrottled(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	f.a.db.Exec(`UPDATE tv_devices SET last_seen = '2020-01-01T00:00:00Z'`)
	tvDo(t, f.door.URL, "GET", "/api/tv/me", "", bearer(tok))
	var seen string
	f.a.db.QueryRow(`SELECT last_seen FROM tv_devices`).Scan(&seen)
	if seen == "2020-01-01T00:00:00Z" {
		t.Fatal("last_seen not updated")
	}
	recent := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	f.a.db.Exec(`UPDATE tv_devices SET last_seen = ?`, recent)
	tvDo(t, f.door.URL, "GET", "/api/tv/trips", "", bearer(tok))
	f.a.db.QueryRow(`SELECT last_seen FROM tv_devices`).Scan(&seen)
	if seen != recent {
		t.Errorf("last_seen rewritten within a minute: %s", seen)
	}
}

// A typed-in code the TV never comes for is gone once the grace has passed.
func TestTVBoundCodeGraceEnds(t *testing.T) {
	f := newTVFix(t)
	code, poll := f.startCode(t)
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"`+code+`","name":"Den"}`, f.signedIn()); r.code != 201 {
		t.Fatal(r.code)
	}
	p := f.a.pairs()
	p.mu.Lock()
	pr := p.byPoll[poll]
	if d := time.Until(pr.collectBy); d < 4*time.Minute {
		t.Errorf("typed in at once: collectable for %v, want the code's own 5 minutes", d)
	}
	pr.expires = time.Now().Add(-time.Second)
	pr.collectBy = time.Now().Add(-time.Millisecond)
	p.mu.Unlock()
	if r := tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil); r.code != 410 {
		t.Errorf("poll after the grace: %d, want 410", r.code)
	}
	// and a typed-in code is never typed in twice, even inside the grace
	code2, _ := f.startCode(t)
	tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"`+code2+`","name":"A"}`, f.signedIn())
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", `{"code":"`+code2+`","name":"B"}`, f.signedIn()); r.code != 404 {
		t.Errorf("second phone on a typed-in code: %d, want 404", r.code)
	}
}

// The door answers the home network only: tailnet and public peers get 404,
// even with a valid token, so it can be published on 0.0.0.0.
func TestTVDoorHomeNetworkOnly(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	door := f.a.tvDoor(newStaticServer())
	cases := []struct {
		peer string
		ok   bool
	}{
		{"192.168.1.27:40000", true}, {"10.0.0.5:1", true}, {"172.16.0.1:1", true}, {"172.31.255.254:1", true},
		{"127.0.0.1:1", true}, {"[::1]:1", true}, {"[fe80::1]:1", true}, {"[fd00::5]:1", true},
		{"[::ffff:192.168.1.27]:1", true},
		{"100.122.101.74:1", false}, {"100.64.0.1:1", false}, {"100.127.255.255:1", false},
		{"[fd7a:115c:a1e0::1]:1", false}, {"8.8.8.8:1", false}, {"172.32.0.1:1", false},
		{"[2a00:23c5::1]:1", false}, {"[::ffff:8.8.8.8]:1", false}, {"garbage", false}, {"", false},
	}
	for _, c := range cases {
		for _, p := range []string{"/api/tv/me", "/tv/tv.js", "/leaflet/leaflet.js"} {
			req := httptest.NewRequest("GET", p, nil)
			req.RemoteAddr = c.peer
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			door.ServeHTTP(rec, req)
			if c.ok && rec.Code != 200 {
				t.Errorf("home peer %s %s: %d, want 200", c.peer, p, rec.Code)
			}
			if !c.ok && rec.Code != 404 {
				t.Errorf("outside peer %s %s: %d, want 404", c.peer, p, rec.Code)
			}
		}
		req := httptest.NewRequest("POST", "/tv/pair", nil)
		req.RemoteAddr = c.peer
		rec := httptest.NewRecorder()
		door.ServeHTTP(rec, req)
		if !c.ok && rec.Code != 404 {
			t.Errorf("outside peer %s POST /tv/pair: %d, want 404", c.peer, rec.Code)
		}
	}
	// a tailnet peer typing X-Forwarded-For with a LAN address is still outside
	req := httptest.NewRequest("GET", "/api/tv/me", nil)
	req.RemoteAddr = "100.122.101.74:1"
	req.Header.Set("X-Forwarded-For", "192.168.1.27")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	door.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("tailnet peer with LAN XFF: %d, want 404", rec.Code)
	}
}
