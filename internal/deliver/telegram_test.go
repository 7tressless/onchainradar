package deliver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestTelegram returns a Telegram client pointed at a test server, with a
// short timeout and a single attempt so failure-mode tests don't wait on backoff.
func newTestTelegram(apiBase string) *Telegram {
	return &Telegram{
		Token:       "test-token",
		ChatID:      "-1001234567890",
		HTTP:        &http.Client{Timeout: 2 * time.Second},
		APIBase:     apiBase,
		ParseMode:   "HTML",
		MaxAttempts: 1,
	}
}

// TestSend_ParsesMessageID is the headline test: Send must parse the message_id
// out of a real-shaped sendMessage success response and return it as a string,
// so the caller can persist it and later edit the same message.
func TestSend_ParsesMessageID(t *testing.T) {
	var gotPath string
	var gotBody sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		// A representative Bot API sendMessage success payload.
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":4242,"chat":{"id":-1001234567890},"text":"hi"}}`))
	}))
	defer srv.Close()

	id, err := newTestTelegram(srv.URL).Send(context.Background(), "hello <b>world</b>")
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if id != "4242" {
		t.Errorf("message id = %q, want %q", id, "4242")
	}
	// Sanity: it hit sendMessage and forwarded the configured chat + parse mode.
	if !strings.HasSuffix(gotPath, "/sendMessage") {
		t.Errorf("path = %q, want .../sendMessage", gotPath)
	}
	if gotBody.ChatID != "-1001234567890" {
		t.Errorf("chat_id = %q, want %q", gotBody.ChatID, "-1001234567890")
	}
	if gotBody.ParseMode != "HTML" {
		t.Errorf("parse_mode = %q, want HTML", gotBody.ParseMode)
	}
	if !gotBody.DisableWebPagePreview {
		t.Error("disable_web_page_preview = false, want true")
	}
}

// TestSend_OKButNoResult: an ok=true response with no result object must error
// (an empty message id is never silently returned).
func TestSend_OKButNoResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	id, err := newTestTelegram(srv.URL).Send(context.Background(), "x")
	if err == nil {
		t.Errorf("ok-but-no-result: want error, got id %q", id)
	}
	if id != "" {
		t.Errorf("ok-but-no-result: id = %q, want empty", id)
	}
}

// TestSend_PermanentError: a 4xx (e.g. bad chat id) is permanent and surfaces as
// an error with an empty id; the bot token must not leak into the error string.
func TestSend_PermanentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer srv.Close()

	id, err := newTestTelegram(srv.URL).Send(context.Background(), "x")
	if err == nil || id != "" {
		t.Fatalf("400: got (%q, %v), want (\"\", err)", id, err)
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Errorf("error string leaked the bot token: %v", err)
	}
}

// TestEditMessageText hits the editMessageText endpoint with the right body and
// succeeds on an ok=true response (which carries the edited message object).
func TestEditMessageText(t *testing.T) {
	var gotPath string
	var gotBody editMessageTextRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":4242,"text":"edited"}}`))
	}))
	defer srv.Close()

	if err := newTestTelegram(srv.URL).EditMessageText(context.Background(), "4242", "edited <b>note</b>"); err != nil {
		t.Fatalf("EditMessageText error: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/editMessageText") {
		t.Errorf("path = %q, want .../editMessageText", gotPath)
	}
	if gotBody.MessageID != 4242 {
		t.Errorf("message_id = %d, want 4242", gotBody.MessageID)
	}
	if gotBody.Text != "edited <b>note</b>" {
		t.Errorf("text = %q, want the edited text", gotBody.Text)
	}
	if gotBody.ParseMode != "HTML" || !gotBody.DisableWebPagePreview {
		t.Errorf("edit must reuse send's parse_mode/preview: parse_mode=%q preview=%v", gotBody.ParseMode, gotBody.DisableWebPagePreview)
	}
}

// TestEditMessageText_InvalidID: a non-numeric message id is a programming error
// and must fail fast without issuing an API call.
func TestEditMessageText_InvalidID(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	err := newTestTelegram(srv.URL).EditMessageText(context.Background(), "not-a-number", "x")
	if err == nil {
		t.Error("invalid message id: want error, got nil")
	}
	if called {
		t.Error("invalid message id must not issue an API call")
	}
}
