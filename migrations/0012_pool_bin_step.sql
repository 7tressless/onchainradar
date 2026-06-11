-- OCR per-pool Liquidity Book bin step on the pool registry. A Merchant Moe pool's
-- getBinStep() value, read on-chain by `ocr discover` / the `ocr poolmeta` backfill and
-- stored so the API can build the add-liquidity deep-link. Display-only, never feeds
-- detection. NULL = unknown / not an LB pool (every Agni pool; an un-backfilled moe pool),
-- and the API just omits lp_url. Idempotent.

BEGIN;

-- bin_step: uint16 on-chain; INT here is ample and matches the nullable *int model field.
ALTER TABLE pools ADD COLUMN IF NOT EXISTS bin_step INT;

COMMIT;
