package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// SSE live feed (GET /api/live). Protocol the frontend's EventSource consumes:
//   - Content-Type: text/event-stream (the gzip middleware bypasses it, so events
//     stream unbuffered).
//   - On connect, replays up to replayN recent signals (oldest-first) so a fresh
//     dashboard is not empty, then streams new signals live.
//   - Each event:    id: <signal id>\nevent: signal\ndata: <SignalDTO json>\n\n
//   - Reconnection:  a client sends Last-Event-ID: <id> (header) or ?last_event_id=<id>;
//     the server replays signals with id > that value, so none is missed across a drop.
//   - Heartbeat:     a ":heartbeat\n\n" comment every heartbeatInterval keeps the
//     connection (and any proxy idle timeout) alive.
//
// Delivery is via the Hub; a too-slow client is dropped and reconnects (self-healing).

const (
	// heartbeatInterval is how often a keep-alive comment is sent on an idle
	// stream. 15s is well under common 30-60s proxy idle timeouts.
	heartbeatInterval = 15 * time.Second

	// replayCap bounds a Last-Event-ID catch-up replay so a client that was away a
	// long time (or sends a tiny id) cannot trigger an unbounded backlog dump.
	replayCap = 500
)

// handleLive serves the SSE stream at GET /api/live.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, OPTIONS")
		writeJSON(w, http.StatusMethodNotAllowed, ErrorDTO{Error: "method not allowed"})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Cannot stream without a flusher. Should not happen (gzipResponseWriter
		// implements it), but fail loudly rather than hang on a buffered connection.
		log.Error().Msg("api: sse response writer is not a flusher")
		writeJSON(w, http.StatusInternalServerError, ErrorDTO{Error: "streaming unsupported"})
		return
	}

	// Subscribe before the 200 handshake so a capacity refusal can still return a clean
	// 503 (after headers we are committed to streaming), and so any signal arriving
	// mid-replay is queued rather than lost (de-duped by lastWritten below).
	c, ok := s.hub.subscribe()
	if !ok {
		log.Warn().Int("max", maxSSEClients).Msg("api: sse at capacity; refusing new client")
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, ErrorDTO{Error: "live stream at capacity, retry shortly"})
		return
	}
	defer s.hub.unsubscribe(c)

	// SSE handshake headers. text/event-stream also makes the gzip middleware bypass
	// compression, so writes stream unbuffered.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Disable proxy buffering (nginx) so events are not held back.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	// connect-time replay: a reconnecting client passes its last seen id (Last-Event-ID
	// or ?last_event_id=); otherwise replay the recent window. Compute afterID to query
	// forward from.
	lastSeen, hasLastSeen := lastEventID(r)

	var afterID int64
	if hasLastSeen {
		// Catch-up from the client's last id (bounded).
		afterID = lastSeen
	} else {
		// Default: replay the most recent replayN signals, using (maxID - replayN) as the
		// floor. If the max id read fails we just start live from 0; the stream still works.
		if maxID, ok, err := s.db.MaxSignalID(ctx); err != nil {
			log.Error().Err(err).Msg("api: sse seed max id failed; starting live with no replay")
			afterID = 0 // SignalsAfterID below will be skipped via replayN cap logic
		} else if ok {
			afterID = maxID - int64(s.replayN)
			if afterID < 0 {
				afterID = 0
			}
		}
	}

	// De-dup replay against the live stream by tracking the last id actually written,
	// skipping queued events with id <= that.
	limit := s.replayN
	if hasLastSeen {
		limit = replayCap
	}
	replayed, lastWritten, err := s.replaySignals(ctx, w, flusher, afterID, limit)
	if err != nil {
		// A replay read failure is logged; we still proceed to the live stream
		// (the client gets new signals even if the backlog read hiccuped).
		log.Error().Err(err).Msg("api: sse replay failed; continuing with live stream")
	}
	log.Debug().Int("replayed", replayed).Int64("last_written", lastWritten).Bool("reconnect", hasLastSeen).Msg("api: sse client streaming")

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			// Client disconnected or server shutting down.
			return
		case ev, open := <-c.ch:
			if !open {
				// Hub closed our channel: either we were dropped (slow reader) or the
				// server is shutting down. End the stream; the browser reconnects.
				return
			}
			// Skip anything we already delivered during replay (the Hub may have
			// queued events whose ids we just wrote).
			if ev.id <= lastWritten {
				continue
			}
			if werr := writeSSEEvent(w, ev.id, ev.name, ev.data); werr != nil {
				// Write failure means the client is gone; stop.
				log.Debug().Err(werr).Msg("api: sse write event failed; closing stream")
				return
			}
			flusher.Flush()
			lastWritten = ev.id
		case <-heartbeat.C:
			if _, werr := io_WriteString(w, ":heartbeat\n\n"); werr != nil {
				log.Debug().Err(werr).Msg("api: sse heartbeat write failed; closing stream")
				return
			}
			flusher.Flush()
		}
	}
}

// replaySignals streams signals with id > afterID (oldest-first, up to limit) to a fresh
// client, returning how many were written and the last id. Used for both the default
// recent-window replay and the Last-Event-ID catch-up; a marshal failure on one row is
// skipped, never aborting the replay.
func (s *Server) replaySignals(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, afterID int64, limit int) (int, int64, error) {
	if limit <= 0 {
		return 0, afterID, nil
	}
	rows, err := s.db.SignalsAfterID(ctx, afterID, limit)
	if err != nil {
		return 0, afterID, err
	}
	written := 0
	last := afterID
	for i := range rows {
		data, merr := marshalJSON(signalToDTO(rows[i], s.label, s.explorerBase))
		if merr != nil {
			log.Error().Err(merr).Int64("signal_id", rows[i].Signal.ID).Msg("api: sse replay marshal failed; skipping")
			last = rows[i].Signal.ID
			continue
		}
		if werr := writeSSEEvent(w, rows[i].Signal.ID, eventSignal, data); werr != nil {
			return written, last, werr
		}
		written++
		last = rows[i].Signal.ID
	}
	if written > 0 {
		flusher.Flush()
	}
	return written, last, nil
}

// writeSSEEvent writes one SSE event frame: id, event-name, and data lines, terminated
// by a blank line. The data is single-line JSON (encoding/json escapes newlines), but we
// split on any newline to stay spec-correct if a payload ever contained one.
func writeSSEEvent(w http.ResponseWriter, id int64, name string, data []byte) error {
	var b strings.Builder
	b.WriteString("id: ")
	b.WriteString(strconv.FormatInt(id, 10))
	b.WriteByte('\n')
	b.WriteString("event: ")
	b.WriteString(name)
	b.WriteByte('\n')
	for _, line := range strings.Split(string(data), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, err := io_WriteString(w, b.String())
	return err
}

// lastEventID extracts a reconnecting client's last seen id from the Last-Event-ID header
// (set by the browser EventSource on reconnect) or the ?last_event_id= query fallback.
// Returns (id, true) when a valid non-negative integer is present.
func lastEventID(r *http.Request) (int64, bool) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("last_event_id")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		return 0, false
	}
	return id, true
}

// io_WriteString writes a string to w, returning the bytes written, so the SSE writes go
// through one path (and to avoid importing io just for io.WriteString here).
func io_WriteString(w http.ResponseWriter, s string) (int, error) {
	n, err := w.Write([]byte(s))
	if err != nil {
		return n, fmt.Errorf("sse write: %w", err)
	}
	return n, nil
}
