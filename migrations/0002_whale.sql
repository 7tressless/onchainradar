-- OCR whale detector: adds per-event signals (type 2, "whale_swap") alongside the
-- per-bucket flow signals (type 1). Idempotent (IF [NOT] EXISTS).

BEGIN;

-- event_ref identifies the originating on-chain event for a per-event signal,
-- "txhash:logindex". NULL for bucket signals (type 1), which anchor on bucket_ts.
ALTER TABLE signals ADD COLUMN IF NOT EXISTS event_ref TEXT;

-- Make the bucket-signal uniqueness partial (event_ref IS NULL), so whale signals can
-- dedup on the event instead. Existing type=1 rows are unaffected (all event_ref NULL).
DROP INDEX IF EXISTS uq_signals_anomaly;
CREATE UNIQUE INDEX IF NOT EXISTS uq_signals_anomaly
    ON signals (pool, signal_type, metric, bucket_ts)
    WHERE event_ref IS NULL;

-- Idempotent per-event emission: one row per (pool, signal_type, event_ref) for whale
-- signals, relied on by INSERT ... ON CONFLICT DO NOTHING. Partial so it ignores bucket
-- signals (the index above governs those).
CREATE UNIQUE INDEX IF NOT EXISTS uq_signals_event
    ON signals (pool, signal_type, event_ref)
    WHERE event_ref IS NOT NULL;

COMMIT;
