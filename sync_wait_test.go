package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A map load answers with the pins it has and syncs in the background; the
// page learns that from X-Photo-Sync and waits on /api/sync/wait to reload.
func TestSyncWaitAnswersWhenTheBackgroundSyncEnds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		wantOK bool
	}{
		{"immich answers", http.StatusOK, true},
		{"immich fails", http.StatusInternalServerError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-release
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"assets":{"items":[],"nextPage":null}}`))
			}))
			defer srv.Close()
			db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			a := &app{db: db, immich: newImmichClient(srv.URL, "key"), lastSync: map[int64]time.Time{}}
			start := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
			if _, err := db.Exec(`INSERT INTO holidays (id, name, color, start_at, immich_album_id) VALUES (1, 'Live', '#123456', ?, 'album')`, start); err != nil {
				t.Fatal(err)
			}

			list := func() string {
				rec := httptest.NewRecorder()
				a.handleListPins(rec, httptest.NewRequest("GET", "/api/pins", nil))
				return rec.Header().Get("X-Photo-Sync")
			}
			if h := list(); h != "running" {
				t.Fatalf("first map load: X-Photo-Sync = %q, want running", h)
			}
			if h := list(); h != "running" {
				t.Fatalf("load during the sync: X-Photo-Sync = %q, want running", h)
			}

			waited := make(chan string)
			go func() {
				rec := httptest.NewRecorder()
				a.handleSyncWait(rec, httptest.NewRequest("GET", "/api/sync/wait", nil))
				waited <- rec.Body.String()
			}()
			select {
			case body := <-waited:
				t.Fatalf("wait answered before the sync ended: %s", body)
			case <-time.After(100 * time.Millisecond):
			}
			close(release)
			select {
			case body := <-waited:
				want := `"ok":false`
				if tc.wantOK {
					want = `"ok":true`
				}
				if !strings.Contains(body, want) {
					t.Errorf("wait answered %s, want %s", body, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("wait never answered")
			}

			if tc.wantOK {
				// the reload that follows must not start another sync (no loop)
				if h := list(); h != "" {
					t.Errorf("reload after a good sync: X-Photo-Sync = %q, want none", h)
				}
			}
		})
	}
}

func TestSyncWaitWithNothingRunning(t *testing.T) {
	a := &app{lastSync: map[int64]time.Time{}}
	rec := httptest.NewRecorder()
	a.handleSyncWait(rec, httptest.NewRequest("GET", "/api/sync/wait", nil))
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Errorf("idle wait answered %s", rec.Body.String())
	}
}
