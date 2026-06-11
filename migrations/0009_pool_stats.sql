-- OCR external pool market stats: display-only market context (TVL, 24h volume, spot
-- price, 24h change) from GeckoTerminal so the API can show a pool's real-world size next
-- to our on-chain activity. Never read by the detector/aggregator/baseline path (its own
-- table, not on `pools`, makes that explicit). 1:1 with pools, latest snapshot only
-- (no history), upserted by the `poolstats` stage. Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS pool_stats (
    pool             TEXT PRIMARY KEY                       -- the pool this snapshot describes (1:1)
                     REFERENCES pools(address) ON DELETE CASCADE,
    tvl_usd          NUMERIC,                               -- reserve_in_usd (total value locked), display-only
    vol24h_usd       NUMERIC,                               -- volume_usd.h24 (24h traded volume), display-only
    price_usd        NUMERIC,                               -- base_token_price_usd (spot), display-only
    price_change_24h NUMERIC,                               -- price_change_percentage.h24 (percent), display-only
    base_token       TEXT,                                  -- base-token symbol as reported by the source (optional)
    quote_token      TEXT,                                  -- quote-token symbol as reported by the source (optional)
    fetched_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),    -- when this snapshot was fetched
    source_url       TEXT                                   -- the upstream URL the snapshot came from (provenance)
);

COMMIT;
