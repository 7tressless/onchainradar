-- OCR per-pool detector tuning. Per-pool z distributions differ wildly (USDC/USDe median
-- z ~31 vs ~5-7 elsewhere), so a global Z_THRESHOLD over/under-fires per pool. These
-- nullable columns override the global defaults; NULL = use the config default. Idempotent.

BEGIN;

-- z_threshold overrides cfg.ZThreshold for the flow-anomaly detector (type 1)
-- on this pool. NULL -> use the global config default.
ALTER TABLE pools ADD COLUMN IF NOT EXISTS z_threshold DOUBLE PRECISION;

-- whale_min_multiple overrides cfg.WhaleMinMultiple for the whale detector
-- (type 2) on this pool. NULL -> use the global config default.
ALTER TABLE pools ADD COLUMN IF NOT EXISTS whale_min_multiple DOUBLE PRECISION;

COMMIT;
