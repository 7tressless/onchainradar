-- OCR raw_logs retention support. The aggregate stage prunes raw_logs older than a margin
-- behind the baseline window, filtering on block_time alone, which the composite
-- idx_raw_logs_addr_time (address, block_time DESC) cannot serve. This single-column index
-- keeps the retention DELETE and the capped scan index-driven, not a seq scan. Idempotent.

BEGIN;

CREATE INDEX IF NOT EXISTS idx_raw_logs_block_time ON raw_logs (block_time);

COMMIT;
