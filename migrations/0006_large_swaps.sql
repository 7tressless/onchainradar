-- OCR large-swaps ledger for the smart-money accumulation detector (type 3). It flags an
-- EOA doing repeated large swaps in a window, so it needs a record keyed by the resolved
-- actor (tx.origin), not the router that appears as the log sender; "accumulation" then
-- reduces to a windowed COUNT / COUNT-DISTINCT by actor. The admission bar
-- (SMART_MONEY_SIZE_MULTIPLE) is below the whale gate for wide recall; the multi-factor
-- score decides which actors fire (precision). Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS large_swaps (
    tx_hash     TEXT          NOT NULL,            -- lowercase tx hash of the swap
    log_index   INTEGER       NOT NULL,            -- log index of the swap within the tx
    pool        TEXT          NOT NULL,            -- lowercase pool address the swap hit
    actor       TEXT          NOT NULL,            -- resolved signing EOA (tx.origin), lowercase hex
    size_token0 NUMERIC(78)   NOT NULL,            -- gross token0 size of the swap (raw units)
    block_time  TIMESTAMPTZ   NOT NULL,            -- swap event time (UTC); the accumulation-window axis
    PRIMARY KEY (tx_hash, log_index)               -- one ledger row per on-chain swap log (idempotent ingest)
);

-- Serves the accumulation query "this actor's large swaps since T" (both COUNT(*) and
-- COUNT(DISTINCT pool) over (actor, block_time >= T)).
CREATE INDEX IF NOT EXISTS idx_large_swaps_actor ON large_swaps (actor, block_time DESC);

COMMIT;
