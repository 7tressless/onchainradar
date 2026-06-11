-- OCR token display metadata (symbol, logo, decimals) for the read-only JSON API, fetched
-- from GeckoTerminal so it can render human-readable swap actions with logos. Keyed by
-- token address (not pool) since tokens are shared across pools. Never read by detection
-- (same separation as pool_stats); the `decimals` here is a display hint only (the
-- authoritative per-pool decimals are pools.dec0/dec1). Upserted by the poolstats stage,
-- best-effort. No FK to `pools` (a token may be referenced by a pool not yet registered).
-- Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS token_meta (
    address    TEXT PRIMARY KEY,                     -- token contract address (lowercase hex); shared across pools
    symbol     TEXT,                                 -- token symbol as reported by the source (optional)
    logo_url   TEXT,                                 -- token logo image URL as reported by the source (optional)
    decimals   INT,                                  -- token decimals as reported by the source (display hint; not authoritative)
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),   -- when this metadata was last fetched
    source_url TEXT                                  -- the upstream URL this metadata came from (provenance)
);

COMMIT;
