package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// gzipMinBytes: below this a gzip header costs more than it saves.
const gzipMinBytes = 1024

// isStream is a response that must flow as it is written: photo and video
// bytes from Immich, and the sync long-poll.
func isStream(p string) bool {
	return strings.Contains(p, "/photo/") || p == "/api/sync/wait"
}

// gzipJSON compresses API JSON for clients that accept it (P1): /api/map is
// ~317 bytes a pin and compresses 8-10x. The static files are pre-gzipped
// already and photos are JPEGs, so only /api/ JSON passes through here.
func gzipJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || isStream(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		bw := &bufferedWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(bw, r)
		body := bw.buf.Bytes()
		h := w.Header()
		if len(body) >= gzipMinBytes && strings.HasPrefix(h.Get("Content-Type"), "application/json") &&
			h.Get("Content-Encoding") == "" {
			var zb bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&zb, gzip.DefaultCompression)
			zw.Write(body)
			zw.Close()
			h.Set("Content-Encoding", "gzip")
			h.Del("Content-Length")
			body = zb.Bytes()
		}
		w.WriteHeader(bw.status)
		w.Write(body)
	})
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			return strings.ReplaceAll(strings.TrimSpace(q), " ", "") != "q=0"
		}
	}
	return false
}

// bufferedWriter holds a JSON response until the handler is done, so the
// gzip decision can see its size.
type bufferedWriter struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
	wrote  bool
}

func (b *bufferedWriter) WriteHeader(code int) {
	if !b.wrote {
		b.status, b.wrote = code, true
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	b.wrote = true
	return b.buf.Write(p)
}

// statusRecorder notes what a handler answered, for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

// Flush keeps streaming responses streaming through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

var shareTokenRe = regexp.MustCompile(`(/share/)[^/]+`)

// logPath never writes a share token to the log: the token is the link.
func logPath(p string) string {
	return shareTokenRe.ReplaceAllString(p, "${1}…")
}

// slowRequest is when a request is worth a log line on its own.
const slowRequest = time.Second

// requestLog writes one line per server error and per slow request (R5);
// a quiet app otherwise logs nothing, as before. Photo streams and the sync
// long-poll are slow by nature and only log on errors.
func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		d := time.Since(start)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		if status >= 500 || (d > slowRequest && !isStream(r.URL.Path)) {
			log.Printf("%s %s %d %d ms", r.Method, logPath(r.URL.Path), status, d.Milliseconds())
		}
	})
}

// runHealthcheck is the -healthcheck flag (R6): the image is FROM scratch,
// with no curl or wget, so the binary checks itself for Docker.
func runHealthcheck() int {
	addr := env("LISTEN_ADDR", ":8095")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy: /healthz", resp.Status)
		return 1
	}
	return 0
}
