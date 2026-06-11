-- OCR initial schema: raw logs, pool registry, buckets, signals, cursor.
-- Single chain (Mantle) but chain_id kept everywhere to leave the multi-chain
-- door open at zero cost.

BEGIN;

-- Raw on-chain logs for the registered pools. Idempotent ingest via the
-- (chain_id, tx_hash, log_index) business key.
CREATE TABLE IF NOT EXISTS raw_logs (
    chain_id      BIGINT      NOT NULL,
    block_number  BIGINT      NOT NULL,
    block_time    TIMESTAMPTZ NOT NULL,
    tx_hash       TEXT        NOT NULL,
    log_index     INTEGER     NOT NULL,
    address       TEXT        NOT NULL,          -- lowercase hex
    topic0        TEXT,
    topic1        TEXT,
    topic2        TEXT,
    topic3        TEXT,
    data          TEXT,
    PRIMARY KEY (chain_id, tx_hash, log_index)
);
CREATE INDEX IF NOT EXISTS idx_raw_logs_addr_time ON raw_logs (address, block_time DESC);
CREATE INDEX IF NOT EXISTS idx_raw_logs_block     ON raw_logs (chain_id, block_number);

-- Curated pool registry. Every address/ABI is verified from official docs +
-- MantleScan; source_url is mandatory. Never invent addresses.
CREATE TABLE IF NOT EXISTS pools (
    address     TEXT PRIMARY KEY,                -- lowercase hex
    dex         TEXT NOT NULL,
    token0      TEXT NOT NULL,
    token1      TEXT NOT NULL,
    dec0        INTEGER NOT NULL,
    dec1        INTEGER NOT NULL,
    label       TEXT,
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    source_url  TEXT
);

-- 5-minute aggregated buckets per pool. Metrics in raw token units (no USD).
CREATE TABLE IF NOT EXISTS buckets (
    pool        TEXT        NOT NULL,
    ts_bucket   TIMESTAMPTZ NOT NULL,            -- bucket start, floored to BUCKET_MIN
    swap_count  INTEGER     NOT NULL DEFAULT 0,
    vol0        NUMERIC(78) NOT NULL DEFAULT 0,  -- gross volume token0 (raw units)
    vol1        NUMERIC(78) NOT NULL DEFAULT 0,  -- gross volume token1 (raw units)
    netflow0    NUMERIC(78) NOT NULL DEFAULT 0,  -- signed net flow token0
    netflow1    NUMERIC(78) NOT NULL DEFAULT 0,  -- signed net flow token1
    PRIMARY KEY (pool, ts_bucket)
);
CREATE INDEX IF NOT EXISTS idx_buckets_pool_time ON buckets (pool, ts_bucket DESC);

-- Detector output. status lifecycle: new -> enriched -> submitted -> alerted
-- ("submitted" = attest tx accepted into the mempool, not yet confirmed).
--
-- bucket_ts is the source bucket's window start and the idempotency anchor: an anomaly is
-- keyed by (pool, signal_type, metric, bucket_ts), so re-running the detector never
-- duplicates a row or attestation. Nullable for non-bucket sources (NULLs compare
-- distinct, so those never collide on the unique index below).
CREATE TABLE IF NOT EXISTS signals (
    id             BIGSERIAL PRIMARY KEY,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    bucket_ts      TIMESTAMPTZ,                  -- source bucket window start; idempotency anchor
    pool           TEXT        NOT NULL,
    signal_type    SMALLINT    NOT NULL,         -- 1 = flow_anomaly, 2 = smart_money
    metric         TEXT        NOT NULL,         -- swap_count | vol0 | vol1 | netflow0 | netflow1
    zscore         NUMERIC     NOT NULL,
    window_buckets INTEGER     NOT NULL,
    payload        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    llm_note       TEXT,
    attest_tx      TEXT,
    status         TEXT        NOT NULL DEFAULT 'new'
);
CREATE INDEX IF NOT EXISTS idx_signals_created ON signals (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_signals_pool    ON signals (pool, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_signals_status  ON signals (status);
-- Idempotent signal emission: one row per (pool, signal_type, metric, bucket_ts), relied
-- on by INSERT ... ON CONFLICT DO NOTHING. NULL bucket_ts rows are exempt (NULLs compare
-- unequal), as intended for non-bucket sources.
CREATE UNIQUE INDEX IF NOT EXISTS uq_signals_anomaly
    ON signals (pool, signal_type, metric, bucket_ts);

-- Collector restart-safety: last fully-processed block per chain.
CREATE TABLE IF NOT EXISTS cursor (
    chain_id   BIGINT PRIMARY KEY,
    last_block BIGINT NOT NULL
);

COMMIT;
