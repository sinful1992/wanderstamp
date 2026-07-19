package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed static
var staticFiles embed.FS

type app struct {
	db           *sql.DB
	immich       *immichClient
	limiter      *loginLimiter
	cookieSecure bool
	syncMu       sync.Mutex // serializes actual sync work against Immich
	stateMu      sync.Mutex // guards lastSync + syncing
	syncing      bool
	lastSync     map[int64]time.Time
}

// version is stamped by release builds via -ldflags "-X main.version=v1.x.x";
// source builds show "dev".
var version = "dev"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	db, err := openDB(env("DB_PATH", "./holidaymap.db"))
	if err != nil {
		log.Fatalf("open database: %v", err)
	}

	a := &app{
		db:           db,
		immich:       newImmichClient(env("IMMICH_URL", "http://immich-server:2283"), os.Getenv("IMMICH_API_KEY")),
		limiter:      newLoginLimiter(),
		cookieSecure: env("COOKIE_SECURE", "true") == "true",
		lastSync:     make(map[int64]time.Time),
	}

	a.bootstrapAdmin()

	if a.immich.apiKey == "" {
		log.Print("WARNING: IMMICH_API_KEY not set — photo sync disabled")
	} else if err := a.immich.validate(); err != nil {
		log.Printf("WARNING: Immich API check failed (photo sync will retry on demand): %v", err)
	} else {
		log.Print("Immich API key validated")
	}

	go a.housekeeping()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.auth(a.handleLogout))
	mux.HandleFunc("GET /api/me", a.auth(a.handleMe))
	mux.HandleFunc("POST /api/users", a.auth(a.handleCreateUser))
	mux.HandleFunc("POST /api/password", a.auth(a.handleChangePassword))

	mux.HandleFunc("GET /api/holidays", a.auth(a.handleListHolidays))
	mux.HandleFunc("POST /api/holidays", a.auth(a.handleCreateHoliday))
	mux.HandleFunc("POST /api/holidays/{id}/end", a.auth(a.handleEndHoliday))
	mux.HandleFunc("POST /api/holidays/{id}/sync", a.auth(a.handleSyncHoliday))
	mux.HandleFunc("PATCH /api/holidays/{id}", a.auth(a.handleUpdateHoliday))
	mux.HandleFunc("DELETE /api/holidays/{id}", a.auth(a.handleDeleteHoliday))
	mux.HandleFunc("GET /api/holidays/{id}/unplaced", a.auth(a.handleUnplacedPhotos))
	mux.HandleFunc("GET /api/holidays/{id}/timeline", a.auth(a.handleHolidayTimeline))
	mux.HandleFunc("GET /api/stamps", a.auth(a.handleStamps))
	mux.HandleFunc("GET /api/geocode", a.auth(a.handleGeocode))

	mux.HandleFunc("GET /api/pins", a.auth(a.handleListPins))
	mux.HandleFunc("POST /api/pins", a.auth(a.handleCreatePin))
	mux.HandleFunc("PATCH /api/pins/{id}", a.auth(a.handleUpdatePin))
	mux.HandleFunc("DELETE /api/pins/{id}", a.auth(a.handleDeletePin))
	mux.HandleFunc("GET /api/pins/{id}/photos", a.auth(a.handlePinPhotos))
	mux.HandleFunc("POST /api/pins/{id}/attach", a.auth(a.handleAttachPhotos))

	mux.HandleFunc("GET /api/photo/{asset}/{kind}", a.auth(a.handlePhoto))

	// Manifest: master packing lists + each holiday's tick-off copy
	mux.HandleFunc("GET /api/packing/templates", a.auth(a.handleListTemplates))
	mux.HandleFunc("POST /api/packing/templates", a.auth(a.handleCreateTemplate))
	mux.HandleFunc("DELETE /api/packing/templates/{id}", a.auth(a.handleDeleteTemplate))
	mux.HandleFunc("POST /api/packing/templates/{id}/items", a.auth(a.handleAddTemplateItem))
	mux.HandleFunc("DELETE /api/packing/templates/{id}/items/{itemID}", a.auth(a.handleDeleteTemplateItem))
	mux.HandleFunc("GET /api/holidays/{id}/packing", a.auth(a.handleListPacking))
	mux.HandleFunc("POST /api/holidays/{id}/packing", a.auth(a.handleAddPackingItem))
	mux.HandleFunc("POST /api/holidays/{id}/packing/apply", a.auth(a.handleApplyTemplate))
	mux.HandleFunc("PATCH /api/packing/{id}", a.auth(a.handleUpdatePackingItem))
	mux.HandleFunc("DELETE /api/packing/{id}", a.auth(a.handleDeletePackingItem))
	mux.HandleFunc("POST /api/packing/{id}/promote", a.auth(a.handlePromoteItem))

	mux.HandleFunc("GET /api/export", a.auth(a.handleExport))

	// view-only trip sharing: management needs a login, reading needs the token
	mux.HandleFunc("POST /api/holidays/{id}/share", a.auth(a.handleCreateShare))
	mux.HandleFunc("DELETE /api/holidays/{id}/share", a.auth(a.handleRevokeShare))
	mux.HandleFunc("GET /api/share/{token}", a.handleShareData)
	mux.HandleFunc("GET /api/share/{token}/pins/{id}/photos", a.handleSharePinPhotos)
	mux.HandleFunc("GET /api/share/{token}/photo/{asset}/{kind}", a.handleSharePhoto)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// An honest check: prove the database answers, not just the process.
		var one int
		if err := a.db.QueryRow(`SELECT 1`).Scan(&one); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	static := newStaticServer()
	mux.Handle("GET /", static)
	// share links serve the same SPA; app.js reads the token from the path
	mux.HandleFunc("GET /share/{token}", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		static.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              env("LISTEN_ADDR", ":8095"),
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      2 * time.Minute, // generous: original-size photos stream through here
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("holiday-map listening on %s", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}

// housekeeping purges expired sessions and stale login-limiter entries so
// neither grows without bound.
func (a *app) housekeeping() {
	for {
		a.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339))
		a.limiter.gc()
		time.Sleep(time.Hour)
	}
}

const csp = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " + // Leaflet div-icons carry style attributes
	"img-src 'self' data: https://*.basemaps.cartocdn.com https://tile.openstreetmap.org https://*.tile.openstreetmap.org; " +
	// the tile CDN must be in connect-src too: the service worker inherits this
	// CSP, and its tile-cache fetch() calls are connect-src, not img-src
	"connect-src 'self' https://*.basemaps.cartocdn.com; " +
	"base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "geolocation=(self), camera=(), microphone=()")
		next.ServeHTTP(w, r)
	})
}

// staticServer serves the embedded frontend from memory with ETags,
// cache headers, and pre-gzipped text assets. embed.FS files carry no
// modtime, so http.FileServerFS alone would re-send every byte every visit.
type staticFile struct {
	body, gz    []byte
	ctype, etag string
	longCache   bool
}

type staticServer struct {
	files map[string]*staticFile
}

func newStaticServer() *staticServer {
	// not in Go's built-in table; without it the manifest would go out as text/plain
	mime.AddExtensionType(".webmanifest", "application/manifest+json")
	sub, _ := fs.Sub(staticFiles, "static")
	s := &staticServer{files: make(map[string]*staticFile)}
	fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		f := &staticFile{
			body:  body,
			ctype: mime.TypeByExtension(path.Ext(p)),
			etag:  `"` + hex.EncodeToString(sum[:8]) + `"`,
			// vendored/immutable-ish assets: cache hard, revalidate rarely
			longCache: strings.HasPrefix(p, "leaflet/") || strings.HasSuffix(p, ".woff2"),
		}
		if f.ctype == "" {
			f.ctype = http.DetectContentType(body)
		}
		switch path.Ext(p) {
		case ".js", ".css", ".html", ".svg", ".webmanifest":
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(body)
			zw.Close()
			if buf.Len() < len(body) {
				f.gz = buf.Bytes()
			}
		}
		s.files["/"+p] = f
		return nil
	})
	s.files["/"] = s.files["/index.html"]
	return s
}

func (s *staticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f, ok := s.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("ETag", f.etag)
	if f.longCache {
		h.Set("Cache-Control", "public, max-age=604800")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", f.ctype)
	body := f.body
	if f.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		h.Set("Vary", "Accept-Encoding")
		body = f.gz
	}
	w.Write(body)
}

// bootstrapAdmin creates the first account from env vars on an empty database.
func (a *app) bootstrapAdmin() {
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		log.Fatalf("count users: %v", err)
	}
	if n > 0 {
		return
	}
	user, pass := os.Getenv("ADMIN_USERNAME"), os.Getenv("ADMIN_PASSWORD")
	if user == "" || pass == "" {
		log.Print("WARNING: empty users table and no ADMIN_USERNAME/ADMIN_PASSWORD set — nobody can log in")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcryptCost)
	if err != nil {
		log.Fatalf("hash admin password: %v", err)
	}
	if _, err := a.db.Exec(`INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, 1)`, user, hash); err != nil {
		log.Fatalf("create admin: %v", err)
	}
	log.Printf("created admin user %q", user)
}
