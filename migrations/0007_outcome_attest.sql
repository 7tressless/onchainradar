-- OCR on-chain outcome-attestation marker. Records the tx hash of the OutcomeAttestor's
-- OutcomeRecorded event on the signal_outcomes row, exactly as signals.attest_tx does for
-- the signal. The attestor never re-attests a row with a non-empty attest_tx (and a
-- duplicate would emit an identical event anyway). NULL/'' = not yet attested; the pass
-- picks it up next time. Idempotent.

BEGIN;

ALTER TABLE signal_outcomes ADD COLUMN IF NOT EXISTS attest_tx TEXT;

COMMIT;
