-- OCR display-only USD size on signals: an approximate USD notional for a per-event
-- signal (whale type 2, smart_money type 3), computed once at signal time so the feed can
-- show "how big" without re-decoding. Derived only from our own decoded swap amounts plus
-- the stable≈$1 assumption (the stable side's amount, decimals-adjusted), with no external
-- price, so never confused with pool_stats / token_meta. NULL when no side is a known
-- stable or for aggregate (flow) signals. Never read by detection. Idempotent.

BEGIN;

-- size_usd: NUMERIC (no fixed scale) so the full decimals-adjusted value survives.
ALTER TABLE signals ADD COLUMN IF NOT EXISTS size_usd NUMERIC;

COMMIT;
