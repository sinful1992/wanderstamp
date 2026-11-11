package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdaB3_429OnEveryPhotoPrunesNothing(t *testing.T) {
	assets := map[string]ws6Asset{"pha": {}, "phb": {status: 429}, "phc": {status: 429}}
	srv, _ := ws6Server(t, []string{"pha"}, assets)
	a := ws6Trip(t, srv.URL, 3)
	if _, _, err := a.syncHoliday(1); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n := ws6Count(t, a, `SELECT COUNT(*) FROM pin_photos WHERE asset_id IN ('phb','phc')`); n != 2 {
		t.Errorf("429 photos removed: %d left of 2", n)
	}
}

func TestAdaB3_403WrongKeyPrunesNothing(t *testing.T) {
	assets := map[string]ws6Asset{"pha": {}, "phb": {status: 403}, "phc": {status: 401}}
	srv, _ := ws6Server(t, []string{"pha"}, assets)
	a := ws6Trip(t, srv.URL, 3)
	a.syncHoliday(1)
	if n := ws6Count(t, a, `SELECT COUNT(*) FROM pin_photos WHERE asset_id IN ('phb','phc')`); n != 2 {
		t.Errorf("401/403 photos removed: %d left of 2", n)
	}
}

// the connection drops in the middle of a 200 body (not the time limit): kept, never pruned
func TestAdaB3_BodyCutMidStreamKeepsPhoto(t *testing.T) {
	inner, _ := ws6Server(t, []string{"pha"}, map[string]ws6Asset{"pha": {}, "phc": {status: 404}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/assets/phb" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "500")
			w.WriteHeader(200)
			w.Write([]byte(`{"id":"phb","isTrashed":tr`))
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		req, _ := http.NewRequest(r.Method, inner.URL+r.URL.String(), r.Body)
		req.Header = r.Header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		buf := new(strings.Builder)
		b := make([]byte, 4096)
		for {
			n, e := resp.Body.Read(b)
			buf.Write(b[:n])
			if e != nil {
				break
			}
		}
		w.Write([]byte(buf.String()))
	}))
	t.Cleanup(srv.Close)
	a := ws6Trip(t, srv.URL, 3)
	_, _, err := a.syncHoliday(1)
	t.Logf("sync err: %v", err)
	if n := ws6Count(t, a, `SELECT COUNT(*) FROM pin_photos WHERE asset_id = 'phb'`); n != 1 {
		t.Error("photo whose answer was cut off was removed")
	}
}
