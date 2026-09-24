package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsCharactersWhole(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"Łódź", 2, "Ł"}, // Ł is 2 bytes: fits exactly
		{"Łódź", 3, "Ł"}, // ó would be split: dropped whole
		{"東京", 4, "東"},   // 3-byte characters
		{"東京", 2, ""},
	} {
		got := truncate(tc.in, tc.max)
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func patchHoliday(t *testing.T, a *app, body string) int {
	t.Helper()
	req := httptest.NewRequest("PATCH", "/api/holidays/1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	a.handleUpdateHoliday(rec, req)
	return rec.Code
}

func TestUpdateHolidayRejectsEndBeforeStart(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	db.Exec(`INSERT INTO holidays (id, name, color, start_at, end_at) VALUES (1, 'Trip', '#123456', '2026-07-01T09:30:00Z', '2026-07-08T18:00:00Z')`)

	// moving either end past the other is refused, and nothing is written
	if code := patchHoliday(t, a, `{"name":"Renamed","end_at":"2026-06-20"}`); code != http.StatusBadRequest {
		t.Errorf("end before start: got %d, want 400", code)
	}
	if code := patchHoliday(t, a, `{"start_at":"2026-07-10"}`); code != http.StatusBadRequest {
		t.Errorf("start after end: got %d, want 400", code)
	}
	var name, start string
	db.QueryRow(`SELECT name, start_at FROM holidays WHERE id = 1`).Scan(&name, &start)
	if name != "Trip" || start != "2026-07-01T09:30:00Z" {
		t.Errorf("rejected patch still wrote: name=%q start=%q", name, start)
	}

	// moving both together is fine even though the new start is after the old end
	if code := patchHoliday(t, a, `{"start_at":"2026-08-01","end_at":"2026-08-05"}`); code != http.StatusOK {
		t.Errorf("shifting the whole trip: got %d, want 200", code)
	}
	// a rename alone leaves the exact start/end moments untouched
	db.Exec(`UPDATE holidays SET start_at = '2026-08-01T09:30:00Z' WHERE id = 1`)
	if code := patchHoliday(t, a, `{"name":"Again"}`); code != http.StatusOK {
		t.Errorf("rename: got %d", code)
	}
	db.QueryRow(`SELECT start_at FROM holidays WHERE id = 1`).Scan(&start)
	if start != "2026-08-01T09:30:00Z" {
		t.Errorf("rename moved start_at to %q", start)
	}
}
