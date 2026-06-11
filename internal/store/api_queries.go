package store

// This file holds the read-only queries behind the public JSON API and SSE feed
// (internal/api), kept separate from queries.go (the hot path) so the API's projections
// evolve independently. Every list is bounded by a capped limit. The only writers are
// UpsertPoolStats / UpsertTokenMeta (the poolstats stage's display-only snapshot).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// StatsSummary is the headline rollup behind GET /api/stats: lifetime per-family signal
// counts, attestation counts, the outcome report-card distribution, a 24h window, and
// registry facts. It is assembled from a handful of small indexed aggregates rather than
// one giant join, so each part is independently correct and cheap.
type StatsSummary struct {
	TotalSignals int64

	// Per-family lifetime counts (signal_type 1..7; see store.SignalType*).
	ByTypeFlow        int64
	ByTypeWhale       int64
	ByTypeSmartMoney  int64
	ByTypeLSTFlow     int64
	ByTypeLiquidation int64
	ByTypeBigBorrow   int64
	ByTypeDepeg       int64

	// On-chain record: how many signals / outcomes carry a non-empty attest_tx.
	SignalAttestations  int64
	OutcomeAttestations int64

	// Outcome report-card rollup.
	OutcomesTotal int64
	GradeA        int64
	GradeB        int64
	GradeC        int64
	GradeD        int64
	GradeF        int64
	AvgGrade      float64 // mean composite grade across all measured outcomes; 0 when none

	// last 24h activity window.
	Last24hSignals     int64
	Last24hFlow        int64
	Last24hWhale       int64
	Last24hSmartMoney  int64
	Last24hLSTFlow     int64
	Last24hLiquidation int64
	Last24hBigBorrow   int64
	Last24hDepeg       int64
	Last24hTopActor    string // most active resolved actor (by signal count) in the window; "" when none

	// last-24h outcome grades, keyed on measured_at: the momentum view of the track
	// record beside the stable all-time roll-up above. Zeroed when nothing graded.
	Last24hOutcomesTotal int64
	Last24hGradeA        int64
	Last24hGradeB        int64
	Last24hGradeC        int64
	Last24hGradeD        int64
	Last24hGradeF        int64
	Last24hAvgGrade      float64

	PoolsTracked int64 // enabled pools in the registry

	// FirstSignalAt is the created_at of the earliest signal; zero when none.
	FirstSignalAt time.Time
}

// StatsSummary assembles the headline rollup for GET /api/stats. A failure in any one
// component aggregate wraps and returns, so a partial summary is never reported.
func (d *DB) StatsSummary(ctx context.Context) (StatsSummary, error) {
	var s StatsSummary

	// Lifetime counts, per-family counts, attestation count, and first-signal time in one
	// pass over signals.
	const qSignals = `
		SELECT
			COUNT(*)                                                    AS total,
			COUNT(*) FILTER (WHERE signal_type = 1)                     AS flow,
			COUNT(*) FILTER (WHERE signal_type = 2)                     AS whale,
			COUNT(*) FILTER (WHERE signal_type = 3)                     AS smart,
			COUNT(*) FILTER (WHERE signal_type = 4)                     AS lst_flow,
			COUNT(*) FILTER (WHERE signal_type = 5)                     AS liquidation,
			COUNT(*) FILTER (WHERE signal_type = 6)                     AS big_borrow,
			COUNT(*) FILTER (WHERE signal_type = 7)                     AS depeg,
			COUNT(*) FILTER (WHERE attest_tx IS NOT NULL AND attest_tx <> '') AS attested,
			MIN(created_at)                                             AS first_at
		FROM signals`
	var firstAt *time.Time
	if err := d.Pool.QueryRow(ctx, qSignals).Scan(
		&s.TotalSignals, &s.ByTypeFlow, &s.ByTypeWhale, &s.ByTypeSmartMoney,
		&s.ByTypeLSTFlow, &s.ByTypeLiquidation, &s.ByTypeBigBorrow, &s.ByTypeDepeg,
		&s.SignalAttestations, &firstAt,
	); err != nil {
		return StatsSummary{}, fmt.Errorf("store: stats signals rollup: %w", err)
	}
	if firstAt != nil {
		s.FirstSignalAt = *firstAt
	}

	// Outcome distribution + average grade + on-chain outcome attestations.
	const qOutcomes = `
		SELECT
			COUNT(*)                                                    AS total,
			COUNT(*) FILTER (WHERE letter = 'A')                        AS a,
			COUNT(*) FILTER (WHERE letter = 'B')                        AS b,
			COUNT(*) FILTER (WHERE letter = 'C')                        AS c,
			COUNT(*) FILTER (WHERE letter = 'D')                        AS d,
			COUNT(*) FILTER (WHERE letter = 'F')                        AS f,
			COALESCE(AVG(grade), 0)::float8                             AS avg_grade,
			COUNT(*) FILTER (WHERE attest_tx IS NOT NULL AND attest_tx <> '') AS attested
		FROM signal_outcomes`
	if err := d.Pool.QueryRow(ctx, qOutcomes).Scan(
		&s.OutcomesTotal, &s.GradeA, &s.GradeB, &s.GradeC, &s.GradeD, &s.GradeF,
		&s.AvgGrade, &s.OutcomeAttestations,
	); err != nil {
		return StatsSummary{}, fmt.Errorf("store: stats outcomes rollup: %w", err)
	}

	// last-24h activity window, per-family.
	const qLast24h = `
		SELECT
			COUNT(*)                                AS total,
			COUNT(*) FILTER (WHERE signal_type = 1) AS flow,
			COUNT(*) FILTER (WHERE signal_type = 2) AS whale,
			COUNT(*) FILTER (WHERE signal_type = 3) AS smart,
			COUNT(*) FILTER (WHERE signal_type = 4) AS lst_flow,
			COUNT(*) FILTER (WHERE signal_type = 5) AS liquidation,
			COUNT(*) FILTER (WHERE signal_type = 6) AS big_borrow,
			COUNT(*) FILTER (WHERE signal_type = 7) AS depeg
		FROM signals
		WHERE created_at >= NOW() - INTERVAL '24 hours'`
	if err := d.Pool.QueryRow(ctx, qLast24h).Scan(
		&s.Last24hSignals, &s.Last24hFlow, &s.Last24hWhale, &s.Last24hSmartMoney,
		&s.Last24hLSTFlow, &s.Last24hLiquidation, &s.Last24hBigBorrow, &s.Last24hDepeg,
	); err != nil {
		return StatsSummary{}, fmt.Errorf("store: stats last-24h rollup: %w", err)
	}

	// Top actor in the window (by signal count); NULL actors excluded. A tie is
	// broken arbitrarily by COUNT ordering, acceptable for a headline figure.
	const qTopActor = `
		SELECT actor
		FROM signals
		WHERE created_at >= NOW() - INTERVAL '24 hours'
		  AND actor IS NOT NULL AND actor <> ''
		GROUP BY actor
		ORDER BY COUNT(*) DESC, MAX(created_at) DESC
		LIMIT 1`
	var topActor string
	err := d.Pool.QueryRow(ctx, qTopActor).Scan(&topActor)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return StatsSummary{}, fmt.Errorf("store: stats top actor: %w", err)
	}
	s.Last24hTopActor = topActor // "" when no actor signals in the window

	// last-24h outcome grades, keyed on measured_at so the figure moves as fresh
	// signals mature, unlike the all-time roll-up. Mirrors qOutcomes within the window.
	const qLast24hOutcomes = `
		SELECT
			COUNT(*)                             AS total,
			COUNT(*) FILTER (WHERE letter = 'A') AS a,
			COUNT(*) FILTER (WHERE letter = 'B') AS b,
			COUNT(*) FILTER (WHERE letter = 'C') AS c,
			COUNT(*) FILTER (WHERE letter = 'D') AS d,
			COUNT(*) FILTER (WHERE letter = 'F') AS f,
			COALESCE(AVG(grade), 0)::float8      AS avg_grade
		FROM signal_outcomes
		WHERE measured_at >= NOW() - INTERVAL '24 hours'`
	if err := d.Pool.QueryRow(ctx, qLast24hOutcomes).Scan(
		&s.Last24hOutcomesTotal, &s.Last24hGradeA, &s.Last24hGradeB, &s.Last24hGradeC,
		&s.Last24hGradeD, &s.Last24hGradeF, &s.Last24hAvgGrade,
	); err != nil {
		return StatsSummary{}, fmt.Errorf("store: stats last-24h outcomes: %w", err)
	}

	const qPools = `SELECT COUNT(*) FROM pools WHERE enabled = TRUE`
	if err := d.Pool.QueryRow(ctx, qPools).Scan(&s.PoolsTracked); err != nil {
		return StatsSummary{}, fmt.Errorf("store: stats pools tracked: %w", err)
	}

	return s, nil
}

// SignalRow is one signal joined to its outcome (if any) for the API list/detail
// projections. The outcome fields are pointers so "no outcome yet" is
// distinguishable from a zero-valued outcome.
type SignalRow struct {
	Signal Signal

	// Outcome (LEFT JOIN; all nil when the signal has no graded outcome yet).
	OutcomeGrade    *int
	OutcomeLetter   *string
	OutcomeStatus   *string
	OutcomeAttestTx *string
}

// signalRowColumns is the projection for a signal joined to its (optional)
// outcome. It extends signalColumns with the four nullable outcome columns,
// LEFT JOINed so a signal with no outcome still appears.
const signalRowColumns = signalColumns + `,
	so.grade, so.letter, so.status, so.attest_tx`

// scanSignalRows scans a pgx.Rows produced by a SELECT of signalRowColumns
// (signals s LEFT JOIN signal_outcomes so) into SignalRows.
func scanSignalRows(rows pgx.Rows) ([]SignalRow, error) {
	var out []SignalRow
	for rows.Next() {
		var (
			r         SignalRow
			bucketTS  *time.Time
			sizeUSDTx *string // nullable NUMERIC, cast ::text
		)
		if err := rows.Scan(
			&r.Signal.ID, &r.Signal.CreatedAt, &bucketTS, &r.Signal.Pool,
			&r.Signal.SignalType, &r.Signal.Metric, &r.Signal.Zscore,
			&r.Signal.WindowBuckets, &r.Signal.Payload, &r.Signal.LLMNote,
			&r.Signal.AttestTx, &r.Signal.EventRef, &r.Signal.Status,
			&r.Signal.TGMessageID, &r.Signal.Actor, &sizeUSDTx,
			&r.OutcomeGrade, &r.OutcomeLetter, &r.OutcomeStatus, &r.OutcomeAttestTx,
		); err != nil {
			return nil, fmt.Errorf("store: scan signal row: %w", err)
		}
		if bucketTS != nil {
			r.Signal.BucketTS = *bucketTS
		}
		sz, perr := decimalPtr(sizeUSDTx)
		if perr != nil {
			return nil, fmt.Errorf("store: parse signal row size_usd: %w", perr)
		}
		r.Signal.SizeUSD = sz
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate signal rows: %w", err)
	}
	return out, nil
}

// SignalFilter is the optional filter set for ListSignalsFiltered. A zero value
// means "no filter" on that dimension. Limit is always applied (capped by the
// caller); Offset enables pagination.
type SignalFilter struct {
	Type   *int16 // signal_type (1/2/3); nil = any
	Pool   string // pool address (lowercased internally); "" = any
	Actor  string // resolved actor (lowercased internally); "" = any
	Graded bool   // true = only signals that carry a graded outcome (the track-record view)
	Limit  int
	Offset int
}

// ListSignalsFiltered returns a page of signals (newest first) matching the filter,
// each joined to its outcome, plus the total matching count (ignoring limit/offset) for
// pagination. Filters use parameterised predicates (no value interpolation). A
// non-positive Limit yields an empty page with the total still computed.
func (d *DB) ListSignalsFiltered(ctx context.Context, f SignalFilter) ([]SignalRow, int64, error) {
	// Shared WHERE clause and positional args, built once for both the count and the page
	// query.
	var (
		conds []string
		args  []any
	)
	if f.Type != nil {
		args = append(args, *f.Type)
		conds = append(conds, fmt.Sprintf("s.signal_type = $%d", len(args)))
	}
	if f.Pool != "" {
		args = append(args, strings.ToLower(f.Pool))
		conds = append(conds, fmt.Sprintf("s.pool = $%d", len(args)))
	}
	if f.Actor != "" {
		args = append(args, strings.ToLower(f.Actor))
		conds = append(conds, fmt.Sprintf("s.actor = $%d", len(args)))
	}
	if f.Graded {
		// A fixed predicate (no positional arg), so it applies to both queries unchanged.
		conds = append(conds, "EXISTS (SELECT 1 FROM signal_outcomes so2 WHERE so2.signal_id = s.id)")
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	var total int64
	countQ := "SELECT COUNT(*) FROM signals s " + where
	if err := d.Pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count filtered signals: %w", err)
	}

	if f.Limit <= 0 {
		return nil, total, nil
	}

	// LIMIT/OFFSET are appended as the last two positional args, after the shared preds.
	pageArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	pageQ := `
		SELECT ` + signalRowColumns + `
		FROM signals s
		LEFT JOIN signal_outcomes so ON so.signal_id = s.id
		` + where + `
		ORDER BY s.created_at DESC, s.id DESC
		LIMIT $` + fmt.Sprint(len(args)+1) + ` OFFSET $` + fmt.Sprint(len(args)+2)
	rows, err := d.Pool.Query(ctx, pageQ, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: query filtered signals: %w", err)
	}
	defer rows.Close()

	out, err := scanSignalRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// OpenLSTSignalByActor returns the most recent type-4 (lst_flow) signal for the actor
// created at or after `since` (the aggregation window start): its id and payload, the
// open aggregate a later transfer by the same actor folds into. found is false when the
// actor has no in-window LST signal, so a fresh one should be opened.
func (d *DB) OpenLSTSignalByActor(ctx context.Context, actor string, since time.Time) (int64, []byte, bool, error) {
	const q = `
		SELECT id, payload
		FROM signals
		WHERE signal_type = $1 AND actor = $2 AND created_at >= $3
		ORDER BY created_at DESC, id DESC
		LIMIT 1`
	var (
		id      int64
		payload []byte
	)
	err := d.Pool.QueryRow(ctx, q, SignalTypeLSTFlow, strings.ToLower(actor), since).Scan(&id, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, fmt.Errorf("store: open lst signal actor=%s: %w", strings.ToLower(actor), err)
	}
	return id, payload, true, nil
}

// UpdateSignalAggregate rewrites a signal's payload and display-only size_usd in place,
// used to fold a later LST transfer into an open aggregate. It touches neither the
// event-keyed identity nor the attested score, so the on-chain record is unaffected.
func (d *DB) UpdateSignalAggregate(ctx context.Context, id int64, payload []byte, sizeUSD *decimal.Decimal) error {
	const q = `UPDATE signals SET payload = $2, size_usd = $3 WHERE id = $1`
	if _, err := d.Pool.Exec(ctx, q, id, payload, decStr(sizeUSD)); err != nil {
		return fmt.Errorf("store: update signal aggregate id=%d: %w", id, err)
	}
	return nil
}

// SignalByID returns one signal joined to its outcome, or (zero, false, nil)
// when no signal with that id exists.
func (d *DB) SignalByID(ctx context.Context, id int64) (SignalRow, bool, error) {
	const q = `
		SELECT ` + signalRowColumns + `
		FROM signals s
		LEFT JOIN signal_outcomes so ON so.signal_id = s.id
		WHERE s.id = $1`
	rows, err := d.Pool.Query(ctx, q, id)
	if err != nil {
		return SignalRow{}, false, fmt.Errorf("store: query signal %d: %w", id, err)
	}
	defer rows.Close()

	out, err := scanSignalRows(rows)
	if err != nil {
		return SignalRow{}, false, err
	}
	if len(out) == 0 {
		return SignalRow{}, false, nil
	}
	return out[0], true, nil
}

// SignalsAfterID returns signals with id > afterID (oldest-first, capped by limit),
// each joined to its outcome. It is the SSE DB-tail query; oldest-first so signals
// broadcast in id order and the caller can advance its cursor to the last row's id.
func (d *DB) SignalsAfterID(ctx context.Context, afterID int64, limit int) ([]SignalRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	const q = `
		SELECT ` + signalRowColumns + `
		FROM signals s
		LEFT JOIN signal_outcomes so ON so.signal_id = s.id
		WHERE s.id > $1
		ORDER BY s.id ASC
		LIMIT $2`
	rows, err := d.Pool.Query(ctx, q, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query signals after id %d: %w", afterID, err)
	}
	defer rows.Close()
	return scanSignalRows(rows)
}

// MaxSignalID returns the largest signal id currently present, or (0, false, nil)
// when the table is empty. The SSE tail seeds its cursor from this so a fresh
// stream does not replay the entire history as "new".
func (d *DB) MaxSignalID(ctx context.Context) (int64, bool, error) {
	const q = `SELECT MAX(id) FROM signals`
	var id *int64
	if err := d.Pool.QueryRow(ctx, q).Scan(&id); err != nil {
		return 0, false, fmt.Errorf("store: max signal id: %w", err)
	}
	if id == nil {
		return 0, false, nil
	}
	return *id, true, nil
}

// ActorRow is one actor's aggregate footprint for the leaderboard / detail
// projections. Counts come from signals (type 3, where the actor lives) and the
// large_swaps ledger (the raw accumulation activity).
type ActorRow struct {
	Actor           string
	TotalSignals    int64
	LargeSwaps24h   int64
	PoolsTouched24h int64
	LatestScore     float64 // zscore of the actor's most recent signal (smart-money score)
	BestScore       float64 // max zscore across the actor's signals
	GradesCount     int64   // graded outcomes among the actor's signals
	GradesAvg       float64 // mean grade across those outcomes; 0 when none
	FirstSeen       time.Time
	LastSeen        time.Time
}

// ActorLeaderboard returns up to `limit` actors ranked by recent activity (24h
// large-swap count desc, then most-recent signal). Only actors that fired at least one
// signal (signals.actor set, the EOAs the smart-money detector surfaced) are included,
// each row aggregating the actor's signal history and 24h ledger activity.
func (d *DB) ActorLeaderboard(ctx context.Context, limit int) ([]ActorRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	// Lifetime per-actor signal aggregates LEFT JOINed to 24h ledger activity, so an
	// actor with signals but no swaps in the window still appears (counts 0).
	const q = `
		WITH sig AS (
			SELECT
				actor,
				COUNT(*)                       AS total_signals,
				MAX(zscore)::float8            AS best_score,
				MIN(created_at)                AS first_seen,
				MAX(created_at)                AS last_seen,
				(ARRAY_AGG(zscore ORDER BY created_at DESC))[1]::float8 AS latest_score
			FROM signals
			WHERE actor IS NOT NULL AND actor <> ''
			GROUP BY actor
		),
		grades AS (
			SELECT s.actor, COUNT(*) AS gc, AVG(so.grade)::float8 AS ga
			FROM signals s
			JOIN signal_outcomes so ON so.signal_id = s.id
			WHERE s.actor IS NOT NULL AND s.actor <> ''
			GROUP BY s.actor
		),
		ledger AS (
			SELECT actor,
			       COUNT(*)               AS swaps_24h,
			       COUNT(DISTINCT pool)   AS pools_24h
			FROM large_swaps
			WHERE block_time >= NOW() - INTERVAL '24 hours'
			GROUP BY actor
		)
		SELECT
			sig.actor,
			sig.total_signals,
			COALESCE(ledger.swaps_24h, 0),
			COALESCE(ledger.pools_24h, 0),
			sig.latest_score,
			sig.best_score,
			COALESCE(grades.gc, 0),
			COALESCE(grades.ga, 0),
			sig.first_seen,
			sig.last_seen
		FROM sig
		LEFT JOIN ledger ON ledger.actor = sig.actor
		LEFT JOIN grades ON grades.actor = sig.actor
		ORDER BY COALESCE(ledger.swaps_24h, 0) DESC, sig.last_seen DESC
		LIMIT $1`
	rows, err := d.Pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query actor leaderboard: %w", err)
	}
	defer rows.Close()
	return scanActorRows(rows)
}

// ActorByAddress returns one actor's aggregate footprint, or (zero, false, nil)
// when the actor has no signals. It mirrors one row of ActorLeaderboard, scoped to
// a single address.
func (d *DB) ActorByAddress(ctx context.Context, actor string) (ActorRow, bool, error) {
	const q = `
		WITH sig AS (
			SELECT
				actor,
				COUNT(*)                       AS total_signals,
				MAX(zscore)::float8            AS best_score,
				MIN(created_at)                AS first_seen,
				MAX(created_at)                AS last_seen,
				(ARRAY_AGG(zscore ORDER BY created_at DESC))[1]::float8 AS latest_score
			FROM signals
			WHERE actor = $1
			GROUP BY actor
		),
		grades AS (
			SELECT s.actor, COUNT(*) AS gc, AVG(so.grade)::float8 AS ga
			FROM signals s
			JOIN signal_outcomes so ON so.signal_id = s.id
			WHERE s.actor = $1
			GROUP BY s.actor
		),
		ledger AS (
			SELECT actor,
			       COUNT(*)               AS swaps_24h,
			       COUNT(DISTINCT pool)   AS pools_24h
			FROM large_swaps
			WHERE actor = $1 AND block_time >= NOW() - INTERVAL '24 hours'
			GROUP BY actor
		)
		SELECT
			sig.actor,
			sig.total_signals,
			COALESCE(ledger.swaps_24h, 0),
			COALESCE(ledger.pools_24h, 0),
			sig.latest_score,
			sig.best_score,
			COALESCE(grades.gc, 0),
			COALESCE(grades.ga, 0),
			sig.first_seen,
			sig.last_seen
		FROM sig
		LEFT JOIN ledger ON ledger.actor = sig.actor
		LEFT JOIN grades ON grades.actor = sig.actor`
	rows, err := d.Pool.Query(ctx, q, strings.ToLower(actor))
	if err != nil {
		return ActorRow{}, false, fmt.Errorf("store: query actor %s: %w", strings.ToLower(actor), err)
	}
	defer rows.Close()
	out, err := scanActorRows(rows)
	if err != nil {
		return ActorRow{}, false, err
	}
	if len(out) == 0 {
		return ActorRow{}, false, nil
	}
	return out[0], true, nil
}

// scanActorRows scans the shared 10-column actor projection.
func scanActorRows(rows pgx.Rows) ([]ActorRow, error) {
	var out []ActorRow
	for rows.Next() {
		var a ActorRow
		if err := rows.Scan(
			&a.Actor, &a.TotalSignals, &a.LargeSwaps24h, &a.PoolsTouched24h,
			&a.LatestScore, &a.BestScore, &a.GradesCount, &a.GradesAvg,
			&a.FirstSeen, &a.LastSeen,
		); err != nil {
			return nil, fmt.Errorf("store: scan actor row: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate actor rows: %w", err)
	}
	return out, nil
}

// ActorAccumulationSince returns the actor's large-swap and distinct-pool counts since
// `since` for the actor detail view. Equivalent to ActorActivitySince but named for the
// API caller and kept here so the API store surface is self-contained.
func (d *DB) ActorAccumulationSince(ctx context.Context, actor string, since time.Time) (swaps int64, pools int64, err error) {
	const q = `
		SELECT COUNT(*), COUNT(DISTINCT pool)
		FROM large_swaps
		WHERE actor = $1 AND block_time >= $2`
	if err := d.Pool.QueryRow(ctx, q, strings.ToLower(actor), since.UTC()).Scan(&swaps, &pools); err != nil {
		return 0, 0, fmt.Errorf("store: actor accumulation since actor=%s: %w", strings.ToLower(actor), err)
	}
	return swaps, pools, nil
}

// RecentSignalsByActor returns up to `limit` of an actor's signals (newest
// first), each joined to its outcome, for the actor detail view.
func (d *DB) RecentSignalsByActor(ctx context.Context, actor string, limit int) ([]SignalRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	const q = `
		SELECT ` + signalRowColumns + `
		FROM signals s
		LEFT JOIN signal_outcomes so ON so.signal_id = s.id
		WHERE s.actor = $1
		ORDER BY s.created_at DESC, s.id DESC
		LIMIT $2`
	rows, err := d.Pool.Query(ctx, q, strings.ToLower(actor), limit)
	if err != nil {
		return nil, fmt.Errorf("store: query signals by actor %s: %w", strings.ToLower(actor), err)
	}
	defer rows.Close()
	return scanSignalRows(rows)
}

// PoolRow is one pool's registry entry joined to its 24h on-chain activity (buckets,
// our data) and its latest external market snapshot (pool_stats, display-only). The two
// stay separate fields so the display-only stats are not confused for detection inputs.
type PoolRow struct {
	Pool Pool

	// 24h on-chain activity (our data, from buckets). Always present (zeros when
	// the pool had no buckets in the window).
	Swaps24h     int64
	Vol0_24h     decimal.Decimal
	Vol1_24h     decimal.Decimal
	LastBucketTS *time.Time

	RecentSignals24h int64

	// External market snapshot (pool_stats); nil when never fetched.
	Stats *PoolStats
}

// PoolsWithActivityAndStats returns every enabled pool with its 24h on-chain activity,
// 24h signal count, and latest external market snapshot (if any), ordered by address.
// Bounded by the registry size (small, curated set).
func (d *DB) PoolsWithActivityAndStats(ctx context.Context) ([]PoolRow, error) {
	const q = `
		SELECT
			p.address, p.dex, p.token0, p.token1, p.dec0, p.dec1,
			COALESCE(p.label,''), p.enabled, COALESCE(p.source_url,''),
			p.z_threshold, p.whale_min_multiple, p.bin_step,
			COALESCE(b.swaps_24h, 0),
			COALESCE(b.vol0_24h, 0)::text,
			COALESCE(b.vol1_24h, 0)::text,
			b.last_bucket_ts,
			COALESCE(sg.sig_24h, 0),
			ps.tvl_usd::text, ps.vol24h_usd::text, ps.price_usd::text,
			ps.price_change_24h::text,
			COALESCE(ps.base_token,''), COALESCE(ps.quote_token,''),
			ps.fetched_at, COALESCE(ps.source_url,''),
			(ps.pool IS NOT NULL) AS has_stats
		FROM pools p
		LEFT JOIN (
			SELECT pool,
			       SUM(swap_count)  AS swaps_24h,
			       SUM(vol0)        AS vol0_24h,
			       SUM(vol1)        AS vol1_24h,
			       MAX(ts_bucket)   AS last_bucket_ts
			FROM buckets
			WHERE ts_bucket >= NOW() - INTERVAL '24 hours'
			GROUP BY pool
		) b ON b.pool = p.address
		LEFT JOIN (
			SELECT pool, COUNT(*) AS sig_24h
			FROM signals
			WHERE created_at >= NOW() - INTERVAL '24 hours'
			GROUP BY pool
		) sg ON sg.pool = p.address
		LEFT JOIN pool_stats ps ON ps.pool = p.address
		WHERE p.enabled = TRUE
		ORDER BY p.address ASC`
	rows, err := d.Pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: query pools with activity: %w", err)
	}
	defer rows.Close()
	return scanPoolRows(rows)
}

// PoolByAddress returns one pool (enabled OR disabled) with the same activity +
// stats projection as PoolsWithActivityAndStats, or (zero, false, nil) when no
// such pool exists.
func (d *DB) PoolByAddress(ctx context.Context, addr string) (PoolRow, bool, error) {
	const q = `
		SELECT
			p.address, p.dex, p.token0, p.token1, p.dec0, p.dec1,
			COALESCE(p.label,''), p.enabled, COALESCE(p.source_url,''),
			p.z_threshold, p.whale_min_multiple, p.bin_step,
			COALESCE(b.swaps_24h, 0),
			COALESCE(b.vol0_24h, 0)::text,
			COALESCE(b.vol1_24h, 0)::text,
			b.last_bucket_ts,
			COALESCE(sg.sig_24h, 0),
			ps.tvl_usd::text, ps.vol24h_usd::text, ps.price_usd::text,
			ps.price_change_24h::text,
			COALESCE(ps.base_token,''), COALESCE(ps.quote_token,''),
			ps.fetched_at, COALESCE(ps.source_url,''),
			(ps.pool IS NOT NULL) AS has_stats
		FROM pools p
		LEFT JOIN (
			SELECT pool,
			       SUM(swap_count)  AS swaps_24h,
			       SUM(vol0)        AS vol0_24h,
			       SUM(vol1)        AS vol1_24h,
			       MAX(ts_bucket)   AS last_bucket_ts
			FROM buckets
			WHERE pool = $1 AND ts_bucket >= NOW() - INTERVAL '24 hours'
			GROUP BY pool
		) b ON b.pool = p.address
		LEFT JOIN (
			SELECT pool, COUNT(*) AS sig_24h
			FROM signals
			WHERE pool = $1 AND created_at >= NOW() - INTERVAL '24 hours'
			GROUP BY pool
		) sg ON sg.pool = p.address
		LEFT JOIN pool_stats ps ON ps.pool = p.address
		WHERE p.address = $1`
	rows, err := d.Pool.Query(ctx, q, strings.ToLower(addr))
	if err != nil {
		return PoolRow{}, false, fmt.Errorf("store: query pool %s: %w", strings.ToLower(addr), err)
	}
	defer rows.Close()
	out, err := scanPoolRows(rows)
	if err != nil {
		return PoolRow{}, false, err
	}
	if len(out) == 0 {
		return PoolRow{}, false, nil
	}
	return out[0], true, nil
}

// scanPoolRows scans the shared pool+activity+stats projection. The NUMERIC
// columns are cast ::text in the query and parsed back to decimal.Decimal here;
// the has_stats boolean controls whether the external snapshot is attached.
func scanPoolRows(rows pgx.Rows) ([]PoolRow, error) {
	var out []PoolRow
	for rows.Next() {
		var (
			r       PoolRow
			vol0Txt string
			vol1Txt string
			// pool_stats columns (nullable -> *string for the ::text NUMERICs)
			tvlTxt, vol24Txt, priceTxt, change24Txt *string
			baseTok, quoteTok                       string
			fetchedAt                               *time.Time
			statsSrc                                string
			hasStats                                bool
		)
		if err := rows.Scan(
			&r.Pool.Address, &r.Pool.Dex, &r.Pool.Token0, &r.Pool.Token1,
			&r.Pool.Dec0, &r.Pool.Dec1, &r.Pool.Label, &r.Pool.Enabled,
			&r.Pool.SourceURL, &r.Pool.ZThreshold, &r.Pool.WhaleMinMultiple,
			&r.Pool.BinStep,
			&r.Swaps24h, &vol0Txt, &vol1Txt, &r.LastBucketTS,
			&r.RecentSignals24h,
			&tvlTxt, &vol24Txt, &priceTxt, &change24Txt,
			&baseTok, &quoteTok, &fetchedAt, &statsSrc, &hasStats,
		); err != nil {
			return nil, fmt.Errorf("store: scan pool row: %w", err)
		}

		var err error
		if r.Vol0_24h, err = decimal.NewFromString(vol0Txt); err != nil {
			return nil, fmt.Errorf("store: parse pool vol0_24h %q: %w", vol0Txt, err)
		}
		if r.Vol1_24h, err = decimal.NewFromString(vol1Txt); err != nil {
			return nil, fmt.Errorf("store: parse pool vol1_24h %q: %w", vol1Txt, err)
		}

		if hasStats {
			ps := &PoolStats{
				Pool:       r.Pool.Address,
				BaseToken:  baseTok,
				QuoteToken: quoteTok,
				SourceURL:  statsSrc,
			}
			if fetchedAt != nil {
				ps.FetchedAt = *fetchedAt
			}
			if ps.TVLUSD, err = decimalPtr(tvlTxt); err != nil {
				return nil, fmt.Errorf("store: parse pool tvl_usd: %w", err)
			}
			if ps.Vol24hUSD, err = decimalPtr(vol24Txt); err != nil {
				return nil, fmt.Errorf("store: parse pool vol24h_usd: %w", err)
			}
			if ps.PriceUSD, err = decimalPtr(priceTxt); err != nil {
				return nil, fmt.Errorf("store: parse pool price_usd: %w", err)
			}
			if ps.PriceChange24h, err = decimalPtr(change24Txt); err != nil {
				return nil, fmt.Errorf("store: parse pool price_change_24h: %w", err)
			}
			r.Stats = ps
		}

		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pool rows: %w", err)
	}
	return out, nil
}

// decimalPtr parses an optional ::text NUMERIC into a *decimal.Decimal: nil stays
// nil (NULL column), a value parses; a parse failure is an error (never silent).
func decimalPtr(s *string) (*decimal.Decimal, error) {
	if s == nil {
		return nil, nil
	}
	dec, err := decimal.NewFromString(*s)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", *s, err)
	}
	return &dec, nil
}

// SeriesPoint is one (bucket time, value) sample for a pool metric time series.
type SeriesPoint struct {
	TS    time.Time
	Value decimal.Decimal
}

// allowedSeriesMetrics is the set of bucket columns SeriesForPool will project. The
// metric name is interpolated into the SQL, so it is validated against this fixed set.
var allowedSeriesMetrics = map[string]struct{}{
	"swap_count": {},
	"vol0":       {},
	"vol1":       {},
	"netflow0":   {},
	"netflow1":   {},
}

// SeriesForPool returns a pool's metric as a (ts, value) series over the last `hours`
// hours, oldest-first. metric is re-checked against allowedSeriesMetrics so the
// interpolated column name stays one of the five known names. swap_count is an integer
// column returned as a decimal for a uniform series shape.
func (d *DB) SeriesForPool(ctx context.Context, pool, metric string, hours int) (string, []SeriesPoint, error) {
	if _, ok := allowedSeriesMetrics[metric]; !ok {
		return metric, nil, fmt.Errorf("store: series unknown metric %q", metric)
	}
	if hours <= 0 {
		hours = 24
	}
	// metric is allow-list-checked above; values are still parameterised.
	q := fmt.Sprintf(`
		SELECT ts_bucket, %s::text
		FROM buckets
		WHERE pool = $1 AND ts_bucket >= NOW() - ($2 || ' hours')::interval
		ORDER BY ts_bucket ASC`, metric)
	rows, err := d.Pool.Query(ctx, q, strings.ToLower(pool), fmt.Sprint(hours))
	if err != nil {
		return metric, nil, fmt.Errorf("store: query series pool=%s metric=%s: %w", strings.ToLower(pool), metric, err)
	}
	defer rows.Close()

	var pts []SeriesPoint
	for rows.Next() {
		var (
			ts  time.Time
			val string
		)
		if err := rows.Scan(&ts, &val); err != nil {
			return metric, nil, fmt.Errorf("store: scan series point: %w", err)
		}
		dec, derr := decimal.NewFromString(val)
		if derr != nil {
			return metric, nil, fmt.Errorf("store: parse series value %q: %w", val, derr)
		}
		pts = append(pts, SeriesPoint{TS: ts, Value: dec})
	}
	if err := rows.Err(); err != nil {
		return metric, nil, fmt.Errorf("store: iterate series points: %w", err)
	}
	return metric, pts, nil
}

// RecentBucketsForPool returns up to `limit` most-recent buckets for a pool,
// oldest-first, for the pool detail mini-series. It is a thin wrapper over
// GetRecentBuckets so the API store surface is self-contained.
func (d *DB) RecentBucketsForPool(ctx context.Context, pool string, limit int) ([]Bucket, error) {
	return d.GetRecentBuckets(ctx, pool, limit)
}

// RecentSignalsByPool returns up to `limit` of a pool's signals (newest first),
// each joined to its outcome, for the pool detail view.
func (d *DB) RecentSignalsByPool(ctx context.Context, pool string, limit int) ([]SignalRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	const q = `
		SELECT ` + signalRowColumns + `
		FROM signals s
		LEFT JOIN signal_outcomes so ON so.signal_id = s.id
		WHERE s.pool = $1
		ORDER BY s.created_at DESC, s.id DESC
		LIMIT $2`
	rows, err := d.Pool.Query(ctx, q, strings.ToLower(pool), limit)
	if err != nil {
		return nil, fmt.Errorf("store: query signals by pool %s: %w", strings.ToLower(pool), err)
	}
	defer rows.Close()
	return scanSignalRows(rows)
}

// UpsertPoolStats inserts or replaces a pool's external market snapshot (display-only,
// never read by detection), called by the poolstats stage after fetching GeckoTerminal.
// Nullable NUMERIC fields pass as *decimal.Decimal (nil -> NULL) so a missing upstream
// field stays NULL rather than a bogus 0. An FK violation (pool not in registry) is
// wrapped and returned.
func (d *DB) UpsertPoolStats(ctx context.Context, ps PoolStats) error {
	const q = `
		INSERT INTO pool_stats
			(pool, tvl_usd, vol24h_usd, price_usd, price_change_24h,
			 base_token, quote_token, fetched_at, source_url)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (pool) DO UPDATE SET
			tvl_usd          = EXCLUDED.tvl_usd,
			vol24h_usd       = EXCLUDED.vol24h_usd,
			price_usd        = EXCLUDED.price_usd,
			price_change_24h = EXCLUDED.price_change_24h,
			base_token       = EXCLUDED.base_token,
			quote_token      = EXCLUDED.quote_token,
			fetched_at       = EXCLUDED.fetched_at,
			source_url       = EXCLUDED.source_url`
	fetchedAt := ps.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	}
	_, err := d.Pool.Exec(ctx, q,
		strings.ToLower(ps.Pool),
		decStr(ps.TVLUSD),
		decStr(ps.Vol24hUSD),
		decStr(ps.PriceUSD),
		decStr(ps.PriceChange24h),
		nilIfEmpty(ps.BaseToken),
		nilIfEmpty(ps.QuoteToken),
		fetchedAt.UTC(),
		nilIfEmpty(ps.SourceURL),
	)
	if err != nil {
		return fmt.Errorf("store: upsert pool_stats pool=%s: %w", strings.ToLower(ps.Pool), err)
	}
	return nil
}

// decStr renders an optional decimal for a NUMERIC column: nil -> NULL, else the
// canonical string (full precision, no float rounding).
func decStr(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return d.String()
}

// intPtrOrNil renders an optional int for an INT column: nil -> NULL, else the value
// (e.g. pools.bin_step, token_meta.decimals).
func intPtrOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// UpsertTokenMeta inserts or replaces one token's display-only metadata (symbol, logo,
// decimals), keyed by token address, called by the poolstats stage per token seen.
// Never read by detection. Empty symbol/logo/source NULL out (nilIfEmpty) and decimals
// NULLs when nil, so an omitted field stays NULL rather than a bogus value; an empty
// address is rejected. There is no FK to `pools` (a token may be referenced by a pool
// not yet in the registry), so unlike UpsertPoolStats this never raises an FK violation.
func (d *DB) UpsertTokenMeta(ctx context.Context, tm TokenMeta) error {
	addr := strings.ToLower(strings.TrimSpace(tm.Address))
	if addr == "" {
		return fmt.Errorf("store: upsert token_meta: empty address")
	}
	const q = `
		INSERT INTO token_meta
			(address, symbol, logo_url, decimals, fetched_at, source_url)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (address) DO UPDATE SET
			symbol     = EXCLUDED.symbol,
			logo_url   = EXCLUDED.logo_url,
			decimals   = EXCLUDED.decimals,
			fetched_at = EXCLUDED.fetched_at,
			source_url = EXCLUDED.source_url`
	fetchedAt := tm.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	}
	if _, err := d.Pool.Exec(ctx, q,
		addr,
		nilIfEmpty(tm.Symbol),
		nilIfEmpty(tm.LogoURL),
		intPtrOrNil(tm.Decimals),
		fetchedAt.UTC(),
		nilIfEmpty(tm.SourceURL),
	); err != nil {
		return fmt.Errorf("store: upsert token_meta address=%s: %w", addr, err)
	}
	return nil
}

// TokenMetaByAddresses returns display-only metadata for the given token addresses as a
// map keyed by lowercase address; a never-fetched token is simply absent (the caller
// renders null symbol/logo). Empty input yields an empty (non-nil) map without a query.
// Addresses are lowercased and de-duplicated; the set is bounded (a handful of pools).
func (d *DB) TokenMetaByAddresses(ctx context.Context, addrs []string) (map[string]TokenMeta, error) {
	out := make(map[string]TokenMeta)
	if len(addrs) == 0 {
		return out, nil
	}
	// De-duplicate + lowercase so the ANY($1) set is minimal and case-correct.
	seen := make(map[string]struct{}, len(addrs))
	uniq := make([]string, 0, len(addrs))
	for _, a := range addrs {
		la := strings.ToLower(strings.TrimSpace(a))
		if la == "" {
			continue
		}
		if _, ok := seen[la]; ok {
			continue
		}
		seen[la] = struct{}{}
		uniq = append(uniq, la)
	}
	if len(uniq) == 0 {
		return out, nil
	}
	const q = `
		SELECT address, COALESCE(symbol,''), COALESCE(logo_url,''),
		       decimals, fetched_at, COALESCE(source_url,'')
		FROM token_meta
		WHERE address = ANY($1)`
	rows, err := d.Pool.Query(ctx, q, uniq)
	if err != nil {
		return nil, fmt.Errorf("store: query token_meta by addresses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			tm   TokenMeta
			dec  *int
			when time.Time
		)
		if err := rows.Scan(&tm.Address, &tm.Symbol, &tm.LogoURL, &dec, &when, &tm.SourceURL); err != nil {
			return nil, fmt.Errorf("store: scan token_meta: %w", err)
		}
		tm.Decimals = dec
		tm.FetchedAt = when
		out[strings.ToLower(tm.Address)] = tm
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate token_meta: %w", err)
	}
	return out, nil
}

// RawLogByRef returns the single raw_logs row identified by (tx_hash, log_index) for
// the configured chain, or (zero, false, nil) when none. It backs the API's on-demand
// swap-action decode (a per-event signal's event_ref "txhash:logindex" loaded and
// decoded on the detail endpoint). A point lookup on the PK (chain_id, tx_hash,
// log_index); tx_hash is lowercased to match storage.
func (d *DB) RawLogByRef(ctx context.Context, chainID int64, txHash string, logIndex int) (RawLog, bool, error) {
	const q = `
		SELECT chain_id, block_number, block_time, tx_hash, log_index,
		       address, COALESCE(topic0,''), COALESCE(topic1,''),
		       COALESCE(topic2,''), COALESCE(topic3,''), COALESCE(data,'')
		FROM raw_logs
		WHERE chain_id = $1 AND tx_hash = $2 AND log_index = $3`
	var l RawLog
	err := d.Pool.QueryRow(ctx, q, chainID, strings.ToLower(strings.TrimSpace(txHash)), logIndex).Scan(
		&l.ChainID, &l.BlockNumber, &l.BlockTime, &l.TxHash, &l.LogIndex,
		&l.Address, &l.Topic0, &l.Topic1, &l.Topic2, &l.Topic3, &l.Data,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RawLog{}, false, nil
	}
	if err != nil {
		return RawLog{}, false, fmt.Errorf("store: query raw_log ref tx=%s idx=%d: %w",
			strings.ToLower(txHash), logIndex, err)
	}
	return l, true, nil
}

// RecentLargeSwapsByActor returns up to `limit` of an actor's large swaps (newest
// first) for the actor detail view's decoded activity path. Each row carries the pool,
// signed net0/net1 (actor perspective; see LargeSwap), tx_hash/log_index, and
// block_time; the API derives in/out direction from net0/net1 directly (no raw_logs
// re-read). net0/net1 COALESCE to 0 so a pre-0008 row contributes a zero-amount entry
// rather than erroring. Ordered block_time DESC, log_index DESC for a stable sequence
// within a block.
func (d *DB) RecentLargeSwapsByActor(ctx context.Context, actor string, limit int) ([]LargeSwap, error) {
	if limit <= 0 {
		return nil, nil
	}
	const q = `
		SELECT tx_hash, log_index, pool, actor,
		       size_token0::text, COALESCE(net0, 0)::text, COALESCE(net1, 0)::text, block_time
		FROM large_swaps
		WHERE actor = $1
		ORDER BY block_time DESC, log_index DESC
		LIMIT $2`
	rows, err := d.Pool.Query(ctx, q, strings.ToLower(actor), limit)
	if err != nil {
		return nil, fmt.Errorf("store: query large_swaps by actor %s: %w", strings.ToLower(actor), err)
	}
	defer rows.Close()

	var out []LargeSwap
	for rows.Next() {
		var (
			ls               LargeSwap
			size, net0, net1 string
		)
		if err := rows.Scan(&ls.TxHash, &ls.LogIndex, &ls.Pool, &ls.Actor, &size, &net0, &net1, &ls.BlockTime); err != nil {
			return nil, fmt.Errorf("store: scan large_swap by actor: %w", err)
		}
		var perr error
		if ls.SizeToken0, perr = decimal.NewFromString(size); perr != nil {
			return nil, fmt.Errorf("store: parse large_swap size %q: %w", size, perr)
		}
		if ls.Net0, perr = decimal.NewFromString(net0); perr != nil {
			return nil, fmt.Errorf("store: parse large_swap net0 %q: %w", net0, perr)
		}
		if ls.Net1, perr = decimal.NewFromString(net1); perr != nil {
			return nil, fmt.Errorf("store: parse large_swap net1 %q: %w", net1, perr)
		}
		out = append(out, ls)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate large_swaps by actor: %w", err)
	}
	return out, nil
}
