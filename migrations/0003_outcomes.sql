-- OCR signal outcome report-card schema: after a signal's horizon elapses, the graded
-- multi-facet card for what actually happened in the pool. Off-chain (OutcomeAttestor
-- commits report_hash separately); 1:1 with signals, inserted once. Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS signal_outcomes (
    signal_id     BIGINT PRIMARY KEY                     -- the graded signal (1:1)
                  REFERENCES signals(id) ON DELETE CASCADE,
    horizon_hours INTEGER     NOT NULL,                  -- post-window length H used to grade
    grade         INTEGER     NOT NULL,                  -- composite score 0..100
    letter        TEXT        NOT NULL,                  -- A/B/C/D/F bucket of grade
    status        TEXT        NOT NULL,                  -- coarse rollup: confirmed | transient
    report_hash   TEXT        NOT NULL,                  -- keccak256 of the canonical card JSON (0x-hex)
    card          TEXT        NOT NULL,                  -- canonical report-card JSON, stored byte-exact as TEXT (not jsonb, which normalizes numbers/whitespace) so it re-hashes to report_hash
    measured_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()     -- when the measurement was taken
);

-- Grade index for the future stats/API (distribution + leaderboards by grade).
CREATE INDEX IF NOT EXISTS idx_signal_outcomes_grade ON signal_outcomes (grade);

COMMIT;
