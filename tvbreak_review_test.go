package main

// Reviewer break tests for commit 60b43c6 (task 133), against CONTRACT.md
// "Interfaces". Reuses the fixture in tv_test.go.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- the door ---

func TestReviewDoorStrict404(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	cases := []struct{ method, path string }{
		{"GET", "/tv/../index.html"}, {"GET", "/tv/%2e%2e/app.js"}, {"GET", "//api/map"},
		{"GET", "/TV"}, {"GET", "/Tv/"}, {"GET", "/tv/pair/"}, {"GET", "/tv/pair"},
		{"OPTIONS", "/tv"}, {"OPTIONS", "/api/tv/me"}, {"HEAD", "/api/map"}, {"HEAD", "/"},
		{"POST", "/api/tv/pair"}, {"GET", "/api/tv/devices"}, {"DELETE", "/api/tv/devices/1"},
		{"POST", "/tv"}, {"PUT", "/tv/pair"}, {"GET", "/api/tv/me/"}, {"GET", "/api/tv/trips/"},
		{"GET", "/tv/index.html/"}, {"GET", "/leaflet/"}, {"GET", "/icons/"},
		{"GET", "/tv/..%2findex.html"}, {"GET", "/./index.html"}, {"GET", "/healthz"},
	}
	for _, c := range cases {
		r := tvDo(t, f.door.URL, c.method, c.path, "", bearer(tok))
		if r.code != 404 {
			t.Errorf("door %s %s: %d (Location %q), contract: everything else -> 404",
				c.method, c.path, r.code, r.header.Get("Location"))
		}
	}
	// what must work, HEAD included (GET semantics)
	for _, p := range []string{"/tv", "/tv/", "/tv/tv.js", "/tv/tv.css", "/staatliches-latin.woff2"} {
		for _, m := range []string{"GET", "HEAD"} {
			if r := tvDo(t, f.door.URL, m, p, "", nil); r.code != 200 {
				t.Errorf("door %s %s: %d, want 200", m, p, r.code)
			}
		}
	}
}

// --- pairing ---

func TestReviewCodeAlphabetAndExpiry(t *testing.T) {
	f := newTVFix(t)
	seen := map[byte]bool{}
	for i := 0; i < 3; i++ { // 3 per peer
		code, _ := f.startCode(t)
		for j := 0; j < len(code); j++ {
			seen[code[j]] = true
		}
	}
	if seen['I'] || seen['O'] {
		t.Errorf("code uses I/O")
	}
	if strings.ContainsAny(tvCodeLetters, "IO01") || len(tvCodeLetters) != 24 {
		t.Errorf("alphabet %q", tvCodeLetters)
	}
	if tvCodeTTL != 5*time.Minute || tvPollWait != 30*time.Second || tvMaxOpen != 20 || tvMaxNewPeer > 10 {
		t.Errorf("limits: ttl %v wait %v open %d perIP %d", tvCodeTTL, tvPollWait, tvMaxOpen, tvMaxNewPeer)
	}
	// expire one code: phone 404, TV 410
	f2 := newTVFix(t)
	code, poll := f2.startCode(t)
	p := f2.a.pairs()
	p.mu.Lock()
	p.byCode[code].expires = time.Now().Add(-time.Second)
	p.mu.Unlock()
	b, _ := json.Marshal(map[string]string{"code": code, "name": "Den"})
	if r := tvDo(t, f2.main.URL, "POST", "/api/tv/pair", string(b), f2.signedIn()); r.code != 404 {
		t.Errorf("expired code pair: %d, want 404", r.code)
	}
	if r := tvDo(t, f2.door.URL, "GET", "/tv/pair/"+poll, "", nil); r.code != 410 {
		t.Errorf("expired poll: %d, want 410", r.code)
	}
}

func TestReviewOpenCodesAndPerIP(t *testing.T) {
	a := newTestApp(t)
	ok := 0
	for i := 0; i < 25; i++ {
		if doorReq(a, "POST", "/tv/pair", fmt.Sprintf("10.1.0.%d:1", i), nil).Code == 200 {
			ok++
		}
	}
	if ok != 20 {
		t.Errorf("open codes: %d, want 20", ok)
	}
	// IPv6 peers are one device per address, ports ignored
	// (since b7013bd a 4th ask replaces the device's oldest code, so it holds 3)
	b := newTestApp(t)
	for i := 0; i < 5; i++ {
		doorReq(b, "POST", "/tv/pair", fmt.Sprintf("[fe80::1]:%d", 1000+i), nil)
	}
	b.pairs().mu.Lock()
	n := len(b.pairs().byCode)
	b.pairs().mu.Unlock()
	if n != tvMaxOpenPeer {
		t.Errorf("one v6 device, many ports: %d open codes", n)
	}
}

func TestReviewCodeNormalisation(t *testing.T) {
	for _, form := range []func(string) string{
		strings.ToLower,
		func(c string) string { return "  " + c + "\t" },
		func(c string) string { return c[:1] + " " + c[1:3] + " " + strings.ToLower(c[3:]) },
	} {
		f := newTVFix(t)
		code, _ := f.startCode(t)
		b, _ := json.Marshal(map[string]string{"code": form(code), "name": "Den"})
		if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn()); r.code != 201 {
			t.Errorf("code %q: %d %s", form(code), r.code, r.body)
		}
	}
}

func TestReviewPollOnceCookieThen410(t *testing.T) {
	f := newTVFix(t)
	old := tvPollWait
	tvPollWait = 200 * time.Millisecond
	defer func() { tvPollWait = old }()
	code, poll := f.startCode(t)
	if r := tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil); r.code != 202 || !strings.Contains(string(r.body), `"pending":true`) {
		t.Errorf("unpaired poll: %d %s, want 202 pending", r.code, r.body)
	}
	b, _ := json.Marshal(map[string]string{"code": code, "name": "Living room"})
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn()); r.code != 201 {
		t.Fatalf("pair: %d %s", r.code, r.body)
	}
	r := tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil)
	if r.code != 200 || !strings.Contains(string(r.body), `"name":"Living room"`) {
		t.Fatalf("poll after pair: %d %s", r.code, r.body)
	}
	sc := r.header.Values("Set-Cookie")
	if len(sc) != 1 {
		t.Fatalf("Set-Cookie: %v", sc)
	}
	for _, want := range []string{"ws_tv=", "HttpOnly", "SameSite=Strict", "Path=/", "Max-Age=34560000"} {
		if !strings.Contains(sc[0], want) {
			t.Errorf("cookie %q lacks %s", sc[0], want)
		}
	}
	if strings.Contains(sc[0], "Secure") {
		t.Errorf("cookie Secure on a plain HTTP door: %q", sc[0])
	}
	if r := tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil); r.code != 410 || len(r.cookie) != 0 {
		t.Errorf("second poll: %d cookies %d, want 410 no cookie", r.code, len(r.cookie))
	}
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn()); r.code != 404 {
		t.Errorf("used code paired again: %d", r.code)
	}
	if r := tvDo(t, f.door.URL, "GET", "/tv/pair/nope", "", nil); r.code != 410 {
		t.Errorf("unknown poll: %d", r.code)
	}
}

// A code bound by the phone (201) must be collectable by the TV, even if the
// 5 min clock runs out before the TV's next poll lands.
func TestReviewBoundAtExpiryStillCollects(t *testing.T) {
	f := newTVFix(t)
	code, poll := f.startCode(t)
	b, _ := json.Marshal(map[string]string{"code": code, "name": "Den"})
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn()); r.code != 201 {
		t.Fatalf("pair: %d", r.code)
	}
	p := f.a.pairs()
	p.mu.Lock()
	p.byPoll[poll].expires = time.Now().Add(-time.Millisecond) // the poll arrives just after
	p.mu.Unlock()
	r := tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil)
	var n int
	f.a.db.QueryRow(`SELECT COUNT(*) FROM tv_devices`).Scan(&n)
	if r.code != 200 {
		t.Errorf("phone got 201 but TV poll got %d; %d device row(s) listed with no TV", r.code, n)
	}
}

func TestReviewWrongCodeLimit(t *testing.T) {
	f := newTVFix(t)
	code, _ := f.startCode(t)
	wrong := "ZZZZ"
	if code == wrong {
		wrong = "YYYY"
	}
	for i := 0; i < tvMaxWrong; i++ {
		b, _ := json.Marshal(map[string]string{"code": wrong, "name": "Den"})
		if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn()); r.code != 404 {
			t.Fatalf("wrong %d: %d", i, r.code)
		}
	}
	b, _ := json.Marshal(map[string]string{"code": code, "name": "Den"})
	if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn()); r.code != 429 {
		t.Errorf("after %d wrong: %d, want 429", tvMaxWrong, r.code)
	}
}

func TestReviewNames(t *testing.T) {
	cases := []struct {
		name   string
		want   int
		stored string
	}{
		{strings.Repeat("😀", 40), 201, strings.Repeat("😀", 40)},
		{strings.Repeat("😀", 41), 400, ""},
		{"  " + strings.Repeat("é", 40) + "  ", 201, strings.Repeat("é", 40)},
		{" Den ", 201, "Den"},
		{"x", 201, "x"},
		{" \t ", 400, ""},
		{"Living room", 201, "Living room"},
	}
	for _, c := range cases {
		f := newTVFix(t)
		code, _ := f.startCode(t)
		b, _ := json.Marshal(map[string]string{"code": code, "name": c.name})
		r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn())
		if r.code != c.want {
			t.Errorf("name %q: %d %s, want %d", c.name, r.code, r.body, c.want)
			continue
		}
		if c.want == 201 {
			var got string
			f.a.db.QueryRow(`SELECT name FROM tv_devices`).Scan(&got)
			if got != c.stored {
				t.Errorf("name %q stored %q", c.name, got)
			}
		}
	}
}

// --- tokens ---

func TestReviewTokens(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	var hash string
	f.a.db.QueryRow(`SELECT token_hash FROM tv_devices`).Scan(&hash)
	if hash == tok || hash != hashToken(tok) {
		t.Errorf("token not stored hashed")
	}
	for _, h := range []map[string]string{bearer(tok), {"Authorization": "bearer " + tok}, {"Cookie": tvCookie + "=" + tok}} {
		if r := tvDo(t, f.door.URL, "GET", "/api/tv/me", "", h); r.code != 200 || string(r.body) != `{"name":"Den"}`+"\n" && strings.TrimSpace(string(r.body)) != `{"name":"Den"}` {
			t.Errorf("auth %v: %d %s", h, r.code, r.body)
		}
	}
	for _, h := range []map[string]string{nil, bearer("x"), bearer(f.session), bearer("old-share"),
		{"Cookie": sessionCookie + "=" + f.session}, {"Cookie": tvCookie + "="}, bearer(hash)} {
		for _, p := range []string{"/api/tv/me", "/api/tv/trips", "/api/tv/trips/1", "/api/tv/photo/" + tvA2 + "/preview"} {
			r := tvDo(t, f.door.URL, "GET", p, "", h)
			if r.code != 401 || strings.TrimSpace(string(r.body)) != `{"error":"not paired"}` {
				t.Errorf("%s %v: %d %s, want 401 not paired", p, h, r.code, r.body)
			}
		}
	}
	var id int64
	f.a.db.QueryRow(`SELECT id FROM tv_devices`).Scan(&id)
	if r := tvDo(t, f.main.URL, "DELETE", fmt.Sprintf("/api/tv/devices/%d", id+99), "", f.signedIn()); r.code != 404 {
		t.Errorf("delete other id: %d", r.code)
	}
	if r := tvDo(t, f.door.URL, "GET", "/api/tv/me", "", bearer(tok)); r.code != 200 {
		t.Errorf("still paired after wrong delete: %d", r.code)
	}
	if r := tvDo(t, f.main.URL, "DELETE", fmt.Sprintf("/api/tv/devices/%d", id), "", f.signedIn()); r.code != 204 {
		t.Errorf("unpair: %d", r.code)
	}
	if r := tvDo(t, f.door.URL, "GET", "/api/tv/trips", "", bearer(tok)); r.code != 401 {
		t.Errorf("after unpair: %d", r.code)
	}
	if r := tvDo(t, f.main.URL, "DELETE", fmt.Sprintf("/api/tv/devices/%d", id), "", f.signedIn()); r.code != 404 {
		t.Errorf("unpair twice: %d", r.code)
	}
}

func TestReviewLastSeenOncePerMinute(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	f.a.db.Exec(`UPDATE tv_devices SET last_seen = ''`)
	tvDo(t, f.door.URL, "GET", "/api/tv/me", "", bearer(tok))
	var s1, s2 string
	f.a.db.QueryRow(`SELECT last_seen FROM tv_devices`).Scan(&s1)
	if _, err := time.Parse(time.RFC3339, s1); err != nil {
		t.Fatalf("last_seen %q", s1)
	}
	old := time.Now().UTC().Add(-61 * time.Second).Format(time.RFC3339)
	f.a.db.Exec(`UPDATE tv_devices SET last_seen = ?`, old)
	tvDo(t, f.door.URL, "GET", "/api/tv/me", "", bearer(tok))
	f.a.db.QueryRow(`SELECT last_seen FROM tv_devices`).Scan(&s2)
	if s2 == old {
		t.Errorf("last_seen not updated after a minute")
	}
}

// --- reads ---

func TestReviewReads(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	h := bearer(tok)
	r := tvDo(t, f.door.URL, "GET", "/api/tv/trips", "", h)
	if strings.Contains(string(r.body), `"Future"`) {
		t.Errorf("planned trip listed")
	}
	if r := tvDo(t, f.door.URL, "GET", "/api/tv/trips/2", "", h); r.code != 404 {
		t.Errorf("planned trip: %d", r.code)
	}
	if r := tvDo(t, f.door.URL, "GET", "/api/tv/trips/999", "", h); r.code != 404 {
		t.Errorf("unknown trip: %d", r.code)
	}
	r = tvDo(t, f.door.URL, "GET", "/api/tv/trips/1", "", h)
	var trip struct {
		Holiday map[string]any   `json:"holiday"`
		Pins    []map[string]any `json:"pins"`
		Photos  []struct {
			AssetID string `json:"asset_id"`
			PinID   int64  `json:"pin_id"`
			TakenAt string `json:"taken_at"`
			Type    string `json:"type"`
		} `json:"photos"`
	}
	r.json(t, &trip)
	if trip.Holiday["created_by"] != "" || trip.Holiday["journal_by"] != "" {
		t.Errorf("holiday share shape: %v %v", trip.Holiday["created_by"], trip.Holiday["journal_by"])
	}
	for _, p := range trip.Pins {
		if p["created_by"] != "" || p["id"] == 1.0 {
			t.Errorf("pin %v leaks (created_by %v)", p["id"], p["created_by"])
		}
	}
	var ids []string
	for i, p := range trip.Photos {
		ids = append(ids, p.AssetID)
		if i > 0 && trip.Photos[i-1].TakenAt > p.TakenAt {
			t.Errorf("photos not ordered by taken_at")
		}
	}
	if strings.Join(ids, ",") != tvA2+","+tvA3+","+tvA4 {
		t.Errorf("photos %v", ids)
	}
	// compare with loadSharedTrip directly
	st, _ := f.a.loadSharedTrip(1)
	if len(st.pins) != len(trip.Pins) {
		t.Errorf("pins %d vs share %d", len(trip.Pins), len(st.pins))
	}
}

func TestReviewPhotoRules(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	h := bearer(tok)
	cases := []struct {
		asset, kind string
		want        int
	}{
		{tvA2, "thumb", 200}, {tvA2, "preview", 200}, {tvA3, "preview", 200},
		{tvA2, "original", 403}, {tvA3, "video", 403},
		{tvA1, "preview", 404}, {tvA5, "preview", 404}, {tvA5, "thumb", 404},
		{"00000000-0000-4000-8000-0000000000ff", "preview", 404},
		{"not-an-id", "preview", 400}, {"abc!", "preview", 400}, {tvA2 + "x", "thumb", 400},
	}
	for _, c := range cases {
		r := tvDo(t, f.door.URL, "GET", "/api/tv/photo/"+c.asset+"/"+c.kind, "", h)
		if r.code != c.want {
			t.Errorf("photo %s/%s: %d %.60s, want %d", c.asset, c.kind, r.code, r.body, c.want)
		}
		if c.want == 200 && r.header.Get("Cache-Control") != "private, max-age=86400" {
			t.Errorf("photo %s/%s Cache-Control %q", c.asset, c.kind, r.header.Get("Cache-Control"))
		}
	}
	f.mu.Lock()
	for _, hit := range f.immichHits {
		if strings.Contains(hit, "original") || strings.Contains(hit, "video") || strings.Contains(hit, tvA1) || strings.Contains(hit, tvA5) {
			t.Errorf("Immich asked for %s", hit)
		}
	}
	f.mu.Unlock()
}

// --- concurrency ---

func TestReviewTwoPhonesOneCode(t *testing.T) {
	for round := 0; round < 10; round++ {
		f := newTVFix(t)
		code, _ := f.startCode(t)
		var wg sync.WaitGroup
		codes := make([]int, 8)
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				b, _ := json.Marshal(map[string]string{"code": code, "name": fmt.Sprintf("TV%d", i)})
				codes[i] = tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn()).code
			}(i)
		}
		wg.Wait()
		n201 := 0
		for _, c := range codes {
			if c == 201 {
				n201++
			} else if c != 404 {
				t.Errorf("round %d: status %d", round, c)
			}
		}
		var rows int
		f.a.db.QueryRow(`SELECT COUNT(*) FROM tv_devices`).Scan(&rows)
		if n201 != 1 || rows != 1 {
			t.Errorf("round %d: %d x 201, %d rows", round, n201, rows)
		}
	}
}

func TestReviewPollRacesPair(t *testing.T) {
	for round := 0; round < 10; round++ {
		f := newTVFix(t)
		code, poll := f.startCode(t)
		var wg sync.WaitGroup
		res := make([]tvResp, 3)
		for i := range res {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res[i] = tvDo(t, f.door.URL, "GET", "/tv/pair/"+poll, "", nil)
			}(i)
		}
		b, _ := json.Marshal(map[string]string{"code": code, "name": "Den"})
		if r := tvDo(t, f.main.URL, "POST", "/api/tv/pair", string(b), f.signedIn()); r.code != 201 {
			t.Fatalf("pair: %d", r.code)
		}
		wg.Wait()
		ok, cookies := 0, 0
		for _, r := range res {
			if r.code == http.StatusOK {
				ok++
			} else if r.code != 410 {
				t.Errorf("round %d: concurrent poll %d", round, r.code)
			}
			cookies += len(r.cookie)
		}
		if ok != 1 || cookies != 1 {
			t.Errorf("round %d: %d x 200, %d cookies; want exactly one", round, ok, cookies)
		}
	}
}
