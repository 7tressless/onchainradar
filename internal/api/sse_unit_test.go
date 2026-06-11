package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Hermetic unit tests for the SSE framing + reconnect-id parsing (no DB).

func TestWriteSSEEventFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := writeSSEEvent(rec, 42, eventSignal, []byte(`{"id":42}`)); err != nil {
		t.Fatalf("writeSSEEvent: %v", err)
	}
	got := rec.Body.String()
	want := "id: 42\nevent: signal\ndata: {\"id\":42}\n\n"
	if got != want {
		t.Fatalf("frame =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteSSEEventMultilineData(t *testing.T) {
	// Defensive: if data ever contained a newline, each line must be prefixed
	// "data: " (SSE spec) so the stream stays valid.
	rec := httptest.NewRecorder()
	if err := writeSSEEvent(rec, 1, eventSignal, []byte("a\nb")); err != nil {
		t.Fatalf("writeSSEEvent: %v", err)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "data: a\n") || !strings.Contains(got, "data: b\n") {
		t.Fatalf("multiline data not split into data: lines: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("frame must end with a blank line: %q", got)
	}
}

func TestLastEventID(t *testing.T) {
	cases := []struct {
		name   string
		header string
		query  string
		wantID int64
		wantOK bool
	}{
		{"header", "58", "", 58, true},
		{"query_fallback", "", "60", 60, true},
		{"header_wins", "10", "99", 10, true},
		{"none", "", "", 0, false},
		{"negative", "-5", "", 0, false},
		{"junk", "abc", "", 0, false},
		{"zero", "0", "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := "/api/live"
			if c.query != "" {
				url += "?last_event_id=" + c.query
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			if c.header != "" {
				req.Header.Set("Last-Event-ID", c.header)
			}
			id, ok := lastEventID(req)
			if id != c.wantID || ok != c.wantOK {
				t.Fatalf("lastEventID = (%d,%v), want (%d,%v)", id, ok, c.wantID, c.wantOK)
			}
		})
	}
}
