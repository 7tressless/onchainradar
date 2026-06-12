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

	id, err := newTestTelegram(srv.URL).Send(context.Background(), "hello <b>world</b>", nil)
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

	id, err := newTestTelegram(srv.URL).Send(context.Background(), "x", nil)
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

	id, err := newTestTelegram(srv.URL).Send(context.Background(), "x", nil)
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

	if err := newTestTelegram(srv.URL).EditMessageText(context.Background(), "4242", "edited <b>note</b>", nil); err != nil {
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

	err := newTestTelegram(srv.URL).EditMessageText(context.Background(), "not-a-number", "x", nil)
	if err == nil {
		t.Error("invalid message id: want error, got nil")
	}
	if called {
		t.Error("invalid message id must not issue an API call")
	}
}

// TestSend_SerializesKeyboard: a non-nil keyboard is serialized into the reply_markup
// field as the inline_keyboard rows the channel post needs.
func TestSend_SerializesKeyboard(t *testing.T) {
	var gotBody sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer srv.Close()

	kb := &inlineKeyboard{Rows: [][]inlineButton{{
		{Text: "On-chain proof", URL: "https://mantlescan.xyz/tx/0xabc"},
	}}}
	if _, err := newTestTelegram(srv.URL).Send(context.Background(), "hi", kb); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if gotBody.ReplyMarkup == nil || len(gotBody.ReplyMarkup.Rows) != 1 || len(gotBody.ReplyMarkup.Rows[0]) != 1 {
		t.Fatalf("reply_markup not serialized as one button row: %+v", gotBody.ReplyMarkup)
	}
	if b := gotBody.ReplyMarkup.Rows[0][0]; b.Text != "On-chain proof" || b.URL != "https://mantlescan.xyz/tx/0xabc" {
		t.Errorf("serialized button = %+v", b)
	}
}

// TestSend_NilKeyboardOmitsReplyMarkup: a nil keyboard must leave reply_markup out of the
// request body entirely (omitempty), so a plain alert carries no empty markup object.
func TestSend_NilKeyboardOmitsReplyMarkup(t *testing.T) {
	var raw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &raw)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":8}}`))
	}))
	defer srv.Close()

	if _, err := newTestTelegram(srv.URL).Send(context.Background(), "hi", nil); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if _, ok := raw["reply_markup"]; ok {
		t.Error("nil keyboard must omit reply_markup from the request body")
	}
}

// TestSendPhoto_MultipartForm: the photo goes as multipart/form-data with the chat id,
// the HTML caption, the keyboard as a JSON field, and the PNG bytes intact; the returned
// message id is parsed like Send's.
func TestSendPhoto_MultipartForm(t *testing.T) {
	var gotPath, gotChat, gotCaption, gotParseMode, gotMarkup string
	var gotPhoto []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Errorf("sendPhoto request is not multipart: %v", err)
		}
		gotChat = r.FormValue("chat_id")
		gotCaption = r.FormValue("caption")
		gotParseMode = r.FormValue("parse_mode")
		gotMarkup = r.FormValue("reply_markup")
		if f, _, err := r.FormFile("photo"); err == nil {
			gotPhoto, _ = io.ReadAll(f)
			_ = f.Close()
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
	}))
	defer srv.Close()

	photo := []byte("\x89PNG fake bytes")
	kb := &inlineKeyboard{Rows: [][]inlineButton{{{Text: "On-chain proof", URL: "https://x/tx/0xabc"}}}}
	id, err := newTestTelegram(srv.URL).SendPhoto(context.Background(), photo, "<b>cap</b>", kb)
	if err != nil {
		t.Fatalf("SendPhoto error: %v", err)
	}
	if id != "99" {
		t.Errorf("message id = %q, want 99", id)
	}
	if !strings.HasSuffix(gotPath, "/sendPhoto") {
		t.Errorf("path = %q, want .../sendPhoto", gotPath)
	}
	if gotChat != "-1001234567890" || gotCaption != "<b>cap</b>" || gotParseMode != "HTML" {
		t.Errorf("form fields = (%q, %q, %q)", gotChat, gotCaption, gotParseMode)
	}
	if !strings.Contains(gotMarkup, `"inline_keyboard"`) || !strings.Contains(gotMarkup, "On-chain proof") {
		t.Errorf("reply_markup field = %q", gotMarkup)
	}
	if string(gotPhoto) != string(photo) {
		t.Errorf("photo bytes corrupted in transit: got %d bytes", len(gotPhoto))
	}
}

// TestSendPhoto_EmptyPhoto fails fast without an API call.
func TestSendPhoto_EmptyPhoto(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	if _, err := newTestTelegram(srv.URL).SendPhoto(context.Background(), nil, "cap", nil); err == nil {
		t.Error("empty photo: want error, got nil")
	}
	if called {
		t.Error("empty photo must not issue an API call")
	}
}

// TestEditMessageCaption hits editMessageCaption with the right body, reusing the send's
// parse mode and re-sending the keyboard.
func TestEditMessageCaption(t *testing.T) {
	var gotPath string
	var gotBody editMessageCaptionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":4242}}`))
	}))
	defer srv.Close()

	kb := &inlineKeyboard{Rows: [][]inlineButton{{{Text: "Live dashboard", URL: "https://d"}}}}
	if err := newTestTelegram(srv.URL).EditMessageCaption(context.Background(), "4242", "new <b>caption</b>", kb); err != nil {
		t.Fatalf("EditMessageCaption error: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/editMessageCaption") {
		t.Errorf("path = %q, want .../editMessageCaption", gotPath)
	}
	if gotBody.MessageID != 4242 || gotBody.Caption != "new <b>caption</b>" || gotBody.ParseMode != "HTML" {
		t.Errorf("edit caption body = %+v", gotBody)
	}
	if gotBody.ReplyMarkup == nil {
		t.Error("edit caption must re-send the keyboard")
	}
}

// TestEditMessageCaption_InvalidID fails fast without an API call.
func TestEditMessageCaption_InvalidID(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	if err := newTestTelegram(srv.URL).EditMessageCaption(context.Background(), "nope", "x", nil); err == nil {
		t.Error("invalid message id: want error, got nil")
	}
	if called {
		t.Error("invalid message id must not issue an API call")
	}
}

// TestEditSignal_FallsBackToTextEdit: when editMessageCaption is rejected (the alert was
// delivered via the text fallback), EditSignal retries the same view as a text edit.
func TestEditSignal_FallsBackToTextEdit(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/editMessageCaption") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: there is no caption in the message to edit"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer srv.Close()

	v := MessageView{Caption: "note added"}
	if err := EditSignal(context.Background(), newTestTelegram(srv.URL), "7", v); err != nil {
		t.Fatalf("EditSignal should succeed via the text-edit fallback: %v", err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/editMessageCaption") || !strings.HasSuffix(paths[1], "/editMessageText") {
		t.Errorf("call order = %v, want caption edit then text edit", paths)
	}
}
