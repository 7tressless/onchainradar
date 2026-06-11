package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Hermetic middleware tests: CORS headers + OPTIONS preflight, and the gzip
// wrapper compressing JSON while bypassing SSE (so the live feed streams
// unbuffered). No DB.

func TestCORSHeadersOnGet(t *testing.T) {
	h := withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Fatalf("Allow-Methods = %q, want to contain GET", got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
}

func TestCORSPreflightOptions(t *testing.T) {
	called := false
	h := withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/signals", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want 204", rec.Code)
	}
	if called {
		t.Fatal("preflight must not call the inner handler")
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Last-Event-ID") {
		t.Fatalf("Allow-Headers = %q, want to include Last-Event-ID", got)
	}
}

func TestGzipCompressesJSON(t *testing.T) {
	payload := strings.Repeat(`{"k":"v"},`, 200) // compressible
	h := withGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/signals", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	// Body must actually be gzip and decompress to the original.
	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("decompressed body mismatch")
	}
}

func TestGzipBypassesSSE(t *testing.T) {
	// An event-stream response must not be gzipped, even when the client accepts
	// gzip; gzip buffering would break streaming.
	h := withGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("id: 1\nevent: signal\ndata: {}\n\n"))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got == "gzip" {
		t.Fatal("SSE response must not be gzip-encoded")
	}
	if !strings.HasPrefix(rec.Body.String(), "id: 1") {
		t.Fatalf("SSE body should be plain, got %q", rec.Body.String())
	}
}

func TestGzipSkippedWithoutAcceptEncoding(t *testing.T) {
	h := withGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil) // no Accept-Encoding
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty (no gzip requested)", got)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// TestGzipPreservesFlusher: the gzip wrapper must still expose http.Flusher so SSE
// can flush. (We assert the wrapped writer implements Flusher.)
func TestGzipPreservesFlusher(t *testing.T) {
	var isFlusher bool
	h := withGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, isFlusher = w.(http.Flusher)
	}))
	rec := httptest.NewRecorder() // httptest.ResponseRecorder implements Flusher
	req := httptest.NewRequest(http.MethodGet, "/api/live", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)
	if !isFlusher {
		t.Fatal("gzipResponseWriter must implement http.Flusher for SSE")
	}
}
