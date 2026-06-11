// Package deliver renders detector signals as Telegram messages and ships them via the
// Telegram Bot API.
//
// This file is a minimal Bot API client (net/http + encoding/json, no SDK) wrapping
// sendMessage and editMessageText with backoff on 429/5xx and the API's retry_after hint.
// Both endpoints exist because enrichment is async: the hot path sends the alert with
// sendMessage and records the message_id; the enrich stage later edits the analyst note
// into that same message.
package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/rs/zerolog/log"
)

// Telegram posts messages to a single chat via the Telegram Bot API.
type Telegram struct {
	Token       string       // bot token (from BotFather)
	ChatID      string       // target chat id (supergroup id or @username)
	HTTP        *http.Client // HTTP client; defaults to a 20s-timeout client
	APIBase     string       // API root, e.g. https://api.telegram.org
	ParseMode   string       // message parse mode, e.g. HTML
	MaxAttempts int          // total send attempts before giving up
}

// NewTelegram constructs a Telegram client with sane defaults: the public
// API base, HTML parse mode, a 20s HTTP timeout, and 5 send attempts.
func NewTelegram(token, chatID string) *Telegram {
	return &Telegram{
		Token:       token,
		ChatID:      chatID,
		HTTP:        &http.Client{Timeout: 20 * time.Second},
		APIBase:     "https://api.telegram.org",
		ParseMode:   "HTML",
		MaxAttempts: 5,
	}
}

// sendMessageRequest is the Bot API sendMessage body.
type sendMessageRequest struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode,omitempty"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

// editMessageTextRequest is the Bot API editMessageText body. It targets a sent message
// by (chat_id, message_id) with the same parse_mode/preview as the original send.
type editMessageTextRequest struct {
	ChatID                string `json:"chat_id"`
	MessageID             int64  `json:"message_id"`
	Text                  string `json:"text"`
	ParseMode             string `json:"parse_mode,omitempty"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

type telegramAPIResponse struct {
	OK          bool            `json:"ok"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *telegramParams `json:"parameters,omitempty"`
	// Result carries the message object on success; we read only message_id from it.
	Result *telegramResult `json:"result,omitempty"`
}

type telegramResult struct {
	MessageID int64 `json:"message_id"`
}

type telegramParams struct {
	RetryAfter int `json:"retry_after,omitempty"`
}

// Send posts text to the configured chat and returns the new message's id (ready to
// persist via store.UpdateSignalTGMessageID and pass to EditMessageText). text is already
// formatted for ParseMode; callers escape dynamic fields via EscapeHTML. Retry policy and
// token redaction are handled in call. On any error it returns ("", err).
func (t *Telegram) Send(ctx context.Context, text string) (string, error) {
	// Guard the nil receiver before any t.* deref below (call also checks, but later).
	if t == nil {
		return "", errors.New("deliver: nil telegram client")
	}
	body, err := json.Marshal(sendMessageRequest{
		ChatID:                t.ChatID,
		Text:                  text,
		ParseMode:             t.ParseMode,
		DisableWebPagePreview: true,
	})
	if err != nil {
		return "", fmt.Errorf("deliver: marshal sendMessage: %w", err)
	}

	resp, err := t.call(ctx, "sendMessage", body)
	if err != nil {
		return "", err
	}
	if resp.Result == nil {
		// ok=true but no message object: surface it rather than return an empty id.
		return "", errors.New("deliver: telegram sendMessage ok but no message_id in result")
	}
	return strconv.FormatInt(resp.Result.MessageID, 10), nil
}

// EditMessageText replaces the text of a sent message, addressed by the id Send
// returned: how the enrich stage adds the analyst note to an alert sent without it.
// Same retry policy and token redaction as Send. A malformed/empty messageID returns
// immediately without an API call.
func (t *Telegram) EditMessageText(ctx context.Context, messageID, text string) error {
	if t == nil {
		return errors.New("deliver: nil telegram client")
	}
	id, err := strconv.ParseInt(strings.TrimSpace(messageID), 10, 64)
	if err != nil {
		return fmt.Errorf("deliver: edit message: invalid message id %q: %w", messageID, err)
	}

	body, err := json.Marshal(editMessageTextRequest{
		ChatID:                t.ChatID,
		MessageID:             id,
		Text:                  text,
		ParseMode:             t.ParseMode,
		DisableWebPagePreview: true,
	})
	if err != nil {
		return fmt.Errorf("deliver: marshal editMessageText: %w", err)
	}

	if _, err := t.call(ctx, "editMessageText", body); err != nil {
		return err
	}
	return nil
}

// call posts a pre-marshalled body to the named Bot API method, applying the shared
// retry policy, and returns the parsed (ok=true) response. It is the single transport
// path for sendMessage and editMessageText, so backoff, 429/5xx classification, and
// token redaction live in one place. On failure it returns a token-redacted error.
func (t *Telegram) call(ctx context.Context, method string, body []byte) (*telegramAPIResponse, error) {
	if t == nil {
		return nil, errors.New("deliver: nil telegram client")
	}
	if t.Token == "" {
		return nil, errors.New("deliver: telegram token is empty")
	}
	if t.ChatID == "" {
		return nil, errors.New("deliver: telegram chat id is empty")
	}

	httpClient := t.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	apiBase := t.APIBase
	if apiBase == "" {
		apiBase = "https://api.telegram.org"
	}
	attempts := t.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	endpoint := fmt.Sprintf("%s/bot%s/%s", apiBase, t.Token, method)

	// MaxAttempts counts total tries; backoff retry count is attempts-1.
	bo := backoff.WithContext(
		backoff.WithMaxRetries(backoff.NewExponentialBackOff(), uint64(attempts-1)),
		ctx,
	)

	// backoff.Retry returns only an error, so the parsed body comes back via this var.
	var result *telegramAPIResponse

	op := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			// Construction errors will not fix themselves on retry.
			return backoff.Permanent(fmt.Errorf("deliver: build request: %w", err))
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			// Transient; retry. Do not log the raw error: net/http's *url.Error embeds
			// the full request URL (bot token included), so log static + return redacted.
			log.Warn().Str("method", method).Msg("deliver: telegram request transport error, retrying")
			return fmt.Errorf("deliver: telegram request failed: %s", t.redactErr(err))
		}
		defer resp.Body.Close()

		payload, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			log.Warn().Err(readErr).Int("status", resp.StatusCode).Str("method", method).
				Msg("deliver: telegram response read error, retrying")
			return fmt.Errorf("deliver: read telegram response: %w", readErr)
		}

		var parsed telegramAPIResponse
		if unmarshalErr := json.Unmarshal(payload, &parsed); unmarshalErr != nil {
			// Could not parse the body; decide purely on the HTTP status.
			log.Warn().Err(unmarshalErr).Int("status", resp.StatusCode).Str("method", method).
				Str("body", string(payload)).Msg("deliver: telegram response not JSON")
			return classifyByStatus(resp.StatusCode, unmarshalErr)
		}

		switch {
		case resp.StatusCode == http.StatusOK && parsed.OK:
			result = &parsed
			return nil

		case resp.StatusCode == http.StatusTooManyRequests:
			retryAfter := 0
			if parsed.Parameters != nil {
				retryAfter = parsed.Parameters.RetryAfter
			}
			log.Warn().Int("retry_after_s", retryAfter).Str("desc", parsed.Description).Str("method", method).
				Msg("deliver: telegram 429 rate limited")
			if retryAfter > 0 {
				// Honor the server-requested cooldown before the next attempt.
				select {
				case <-ctx.Done():
					return backoff.Permanent(fmt.Errorf("deliver: context cancelled during retry_after: %w", ctx.Err()))
				case <-time.After(time.Duration(retryAfter) * time.Second):
				}
			}
			return fmt.Errorf("deliver: telegram 429: %s", parsed.Description)

		case resp.StatusCode >= 500 && resp.StatusCode < 600:
			log.Warn().Int("status", resp.StatusCode).Str("desc", parsed.Description).Str("method", method).
				Msg("deliver: telegram 5xx, retrying")
			return fmt.Errorf("deliver: telegram %d: %s", resp.StatusCode, parsed.Description)

		default:
			// Any other 4xx (bad token, bad chat id, bad payload) is permanent.
			return backoff.Permanent(fmt.Errorf("deliver: telegram %d: %s", resp.StatusCode, parsed.Description))
		}
	}

	if err := backoff.Retry(op, bo); err != nil {
		// Redact before returning so a caller logging the error cannot leak the token
		// embedded in a wrapped *url.Error.
		return nil, fmt.Errorf("deliver: telegram %s failed: %s", method, t.redactErr(err))
	}
	return result, nil
}

// redactErr returns err.Error() with the bot token replaced by "<redacted>", used
// before any site that propagates a network error, since *url.Error embeds the
// token-in-path URL.
func (t *Telegram) redactErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if t.Token != "" {
		s = strings.ReplaceAll(s, t.Token, "<redacted>")
	}
	return s
}

// classifyByStatus decides retryability from the HTTP status alone, used
// when the response body cannot be parsed as JSON. The supplied cause is
// wrapped so the original parse error is never swallowed.
func classifyByStatus(status int, cause error) error {
	switch {
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("deliver: telegram 429 (unparseable body): %w", cause)
	case status >= 500 && status < 600:
		return fmt.Errorf("deliver: telegram %d (unparseable body): %w", status, cause)
	case status == http.StatusOK:
		// 200 but not parseable / not ok=true: do not retry blindly.
		return backoff.Permanent(fmt.Errorf("deliver: telegram 200 with unparseable body: %w", cause))
	default:
		return backoff.Permanent(fmt.Errorf("deliver: telegram %d (unparseable body): %w", status, cause))
	}
}

// EscapeHTML escapes the characters Telegram HTML parse mode treats as markup
// (&, <, >). Apply it to every dynamic value spliced into an HTML message.
func EscapeHTML(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(s)
}
