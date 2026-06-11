-- OCR Telegram message-id tracking for async enrichment. The hot path attests + sends the
-- alert immediately and records the sent message's id here; the async enrich stage later
-- edits the analyst note into that message via editMessageText, addressed by this id.
--
-- TEXT (not BIGINT) on purpose: stored verbatim as returned and passed straight back to
-- editMessageText (never arithmetic), avoiding integer-width surprises. NULL/'' = no alert
-- was sent (Telegram disabled or the send failed); the edit is skipped. Idempotent.

BEGIN;

ALTER TABLE signals ADD COLUMN IF NOT EXISTS tg_message_id TEXT;

COMMIT;
