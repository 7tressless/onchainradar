package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// This file holds the HTTP middleware: CORS (public read-only API, so GET from any
// origin + OPTIONS preflight) and gzip (JSON responses, bypassed for SSE so streaming is
// not buffered).

// withCORS adds permissive read-only CORS headers and short-circuits an OPTIONS
// preflight with 204. Any origin is fine: the API exposes only public data and never
// mutates state, so there is no state-changing request to forge.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		// Allow the headers a browser EventSource / fetch may send. Last-Event-ID is
		// what an SSE client sends on reconnect.
		h.Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID, Cache-Control")
		// Let the browser read the SSE id on the client if it inspects headers.
		h.Set("Access-Control-Expose-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "600")

		if r.Method == http.MethodOptions {
			// Preflight: no body, just the CORS headers above.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// gzipPool reuses gzip.Writers across requests to avoid per-request allocation.
var gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// gzipResponseWriter compresses the body lazily: it inspects the Content-Type on the
// first write and engages gzip only for compressible types. It bypasses
// text/event-stream so SSE streams unbuffered, and preserves http.Flusher so SSE
// handlers can flush each event.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz       *gzip.Writer
	decided  bool // whether we've inspected Content-Type and chosen a path
	useGzip  bool
	status   int
	wroteHdr bool
}

// shouldGzip reports whether a content type is worth compressing. JSON and
// HTML/text yes; event streams and already-compressed types no.
func shouldGzip(ct string) bool {
	ct = strings.ToLower(ct)
	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		return false
	case strings.Contains(ct, "application/json"):
		return true
	case strings.HasPrefix(ct, "text/"):
		return true
	default:
		return false
	}
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	g.status = status
	// Defer the actual header write until the first body write, so we can read the
	// Content-Type the handler set and decide whether to add Content-Encoding.
}

func (g *gzipResponseWriter) decide() {
	if g.decided {
		return
	}
	g.decided = true
	ct := g.Header().Get("Content-Type")
	g.useGzip = shouldGzip(ct)
	if g.useGzip {
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Add("Vary", "Accept-Encoding")
		// Any handler-set Content-Length is for the uncompressed body; drop it.
		g.Header().Del("Content-Length")
		gz := gzipPool.Get().(*gzip.Writer)
		gz.Reset(g.ResponseWriter)
		g.gz = gz
	}
}

func (g *gzipResponseWriter) writeHeaderOnce() {
	if g.wroteHdr {
		return
	}
	g.wroteHdr = true
	status := g.status
	if status == 0 {
		status = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	g.decide()
	g.writeHeaderOnce()
	if g.useGzip {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so SSE events reach the client immediately,
// flushing the gzip writer first when engaged. SSE never engages gzip, so it flushes
// straight through.
func (g *gzipResponseWriter) Flush() {
	if g.useGzip && g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		// Ensure headers are out before the first flush (e.g. SSE handshake).
		g.decide()
		g.writeHeaderOnce()
		f.Flush()
	}
}

// close finalizes the gzip stream (if engaged) and returns the writer to the pool.
func (g *gzipResponseWriter) close() {
	if g.useGzip && g.gz != nil {
		_ = g.gz.Close()
		gzipPool.Put(g.gz)
		g.gz = nil
	}
}

// withGzip gzips compressible responses when the client advertises Accept-Encoding:
// gzip; a client that does not is served uncompressed. SSE is bypassed inside
// gzipResponseWriter, so the live feed streams unbuffered.
func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
		// Emit the header even if no Write happened (e.g. 204), so the status is sent.
		gw.writeHeaderOnce()
	})
}
