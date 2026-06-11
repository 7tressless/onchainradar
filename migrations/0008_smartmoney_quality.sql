-- OCR smart-money quality pass (type 3), two fixes (flow/whale untouched):
--   A. Per-actor dedup + cooldown (was firing per-pool and re-firing as swap count grew),
--      DB-backed via signals.actor so it survives restarts.
--   B. Net-direction gate: store each large swap's signed net change so churning arb/MM
--      bots (net ~= 0) are no longer flagged as accumulation.
-- Idempotent.

BEGIN;

-- A. per-actor dedup on signals.

-- actor records the resolved tx.origin behind a type-3 signal (lowercase hex), NULL for
-- every other type. Powers the per-actor cooldown (LastSignalTimeByActor).
ALTER TABLE signals ADD COLUMN IF NOT EXISTS actor TEXT;

-- Serves the cooldown query (newest created_at by actor, signal_type). NULL-actor rows
-- (all non type-3) are excluded to keep it small.
CREATE INDEX IF NOT EXISTS idx_signals_actor_type_created
    ON signals (actor, signal_type, created_at DESC)
    WHERE actor IS NOT NULL;

-- B. signed per-token net change on large_swaps.

-- net0 / net1 are the swap's signed net change of token0 / token1 from the actor's
-- perspective (positive = actor received), i.e. aggregate.DecodeSwap's Netflow0/1.
-- Nullable: pre-migration rows stay NULL and the Go side treats NULL as 0/unknown, so the
-- window self-fills within SMART_MONEY_WINDOW_HOURS without crashing on an old row.
ALTER TABLE large_swaps ADD COLUMN IF NOT EXISTS net0 NUMERIC(78);
ALTER TABLE large_swaps ADD COLUMN IF NOT EXISTS net1 NUMERIC(78);

COMMIT;
