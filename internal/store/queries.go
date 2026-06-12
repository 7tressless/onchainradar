package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// InsertRawLogs batch-inserts logs, ignoring rows that collide on the
// (chain_id, tx_hash, log_index) business key. Returns the number of rows
// actually inserted. Addresses and topics are lowercased.
func (d *DB) InsertRawLogs(ctx context.Context, logs []RawLog) (int64, error) {
	if len(logs) == 0 {
		return 0, nil
	}

	const q = `
		INSERT INTO raw_logs
			(chain_id, block_number, block_time, tx_hash, log_index,
			 address, topic0, topic1, topic2, topic3, data)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (chain_id, tx_hash, log_index) DO NOTHING`

	batch := &pgx.Batch{}
	for _, l := range logs {
		batch.Queue(q,
			l.ChainID,
			l.BlockNumber,
			l.BlockTime,
			strings.ToLower(l.TxHash),
			l.LogIndex,
			strings.ToLower(l.Address),
			strings.ToLower(l.Topic0),
			lowerOrNil(l.Topic1),
			lowerOrNil(l.Topic2),
			lowerOrNil(l.Topic3),
			l.Data,
		)
	}

	br := d.Pool.SendBatch(ctx, batch)
	defer br.Close()

	var inserted int64
	for i := 0; i < len(logs); i++ {
		ct, err := br.Exec()
		if err != nil {
			return inserted, fmt.Errorf("store: insert raw_logs row %d: %w", i, err)
		}
		inserted += ct.RowsAffected()
	}
	return inserted, nil
}

// rawLogsSinceCap bounds how many rows one RawLogsSince call loads into memory: a safety
// rail against a pathological window (a pool with no buckets yet, or a long collector
// outage) pulling an unbounded result set. Hitting it is logged at WARN, and the next
// aggregation pass drains the remainder.
const rawLogsSinceCap = 200_000

// RawLogsSince returns logs at or after `since`, ordered by block_time ascending,
// capped at rawLogsSinceCap rows (the oldest, with a WARN). If pools is non-empty,
// results are restricted to those addresses (matched against the lowercase column).
func (d *DB) RawLogsSince(ctx context.Context, since time.Time, pools []string) ([]RawLog, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if len(pools) == 0 {
		const q = `
			SELECT chain_id, block_number, block_time, tx_hash, log_index,
			       address, COALESCE(topic0,''), COALESCE(topic1,''),
			       COALESCE(topic2,''), COALESCE(topic3,''), COALESCE(data,'')
			FROM raw_logs
			WHERE block_time >= $1
			ORDER BY block_time ASC
			LIMIT $2`
		rows, err = d.Pool.Query(ctx, q, since, rawLogsSinceCap)
	} else {
		const q = `
			SELECT chain_id, block_number, block_time, tx_hash, log_index,
			       address, COALESCE(topic0,''), COALESCE(topic1,''),
			       COALESCE(topic2,''), COALESCE(topic3,''), COALESCE(data,'')
			FROM raw_logs
			WHERE block_time >= $1 AND address = ANY($2)
			ORDER BY block_time ASC
			LIMIT $3`
		rows, err = d.Pool.Query(ctx, q, since, lowerAll(pools), rawLogsSinceCap)
	}
	if err != nil {
		return nil, fmt.Errorf("store: query raw_logs since: %w", err)
	}
	defer rows.Close()

	var out []RawLog
	for rows.Next() {
		var l RawLog
		if err := rows.Scan(
			&l.ChainID, &l.BlockNumber, &l.BlockTime, &l.TxHash, &l.LogIndex,
			&l.Address, &l.Topic0, &l.Topic1, &l.Topic2, &l.Topic3, &l.Data,
		); err != nil {
			return nil, fmt.Errorf("store: scan raw_log: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate raw_logs: %w", err)
	}
	if len(out) == rawLogsSinceCap {
		log.Warn().Int("cap", rawLogsSinceCap).Time("since", since).
			Msg("store: RawLogsSince hit the row cap; result truncated to the oldest rows (next pass drains the rest)")
	}
	return out, nil
}

// PruneRawLogsBefore deletes raw_logs older than cutoff and returns the rows removed.
// Once a row's window is bucketed the raw row is dead weight; pruning the aged tail keeps
// the table (and the RawLogsSince scan) bounded. The caller picks a cutoff comfortably
// behind the baseline window so no live aggregation still needs them.
func (d *DB) PruneRawLogsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `DELETE FROM raw_logs WHERE block_time < $1`
	ct, err := d.Pool.Exec(ctx, q, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune raw_logs before %s: %w", cutoff.Format(time.RFC3339), err)
	}
	return ct.RowsAffected(), nil
}

// GetCursor returns the last fully-processed block for a chain. The bool is
// false (with a nil error) when no cursor row exists yet.
func (d *DB) GetCursor(ctx context.Context, chainID int64) (int64, bool, error) {
	const q = `SELECT last_block FROM cursor WHERE chain_id = $1`
	var lastBlock int64
	err := d.Pool.QueryRow(ctx, q, chainID).Scan(&lastBlock)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: get cursor chain %d: %w", chainID, err)
	}
	return lastBlock, true, nil
}

// SetCursor upserts the last fully-processed block for a chain.
func (d *DB) SetCursor(ctx context.Context, chainID, lastBlock int64) error {
	const q = `
		INSERT INTO cursor (chain_id, last_block)
		VALUES ($1, $2)
		ON CONFLICT (chain_id) DO UPDATE SET last_block = EXCLUDED.last_block`
	if _, err := d.Pool.Exec(ctx, q, chainID, lastBlock); err != nil {
		return fmt.Errorf("store: set cursor chain %d: %w", chainID, err)
	}
	return nil
}

// ListEnabledPools returns all pools with enabled = TRUE. The nullable tuning columns
// (z_threshold, whale_min_multiple) scan into *float64 (NULL -> nil = use the global
// default); bin_step scans into *int (NULL -> nil = unknown / not a Liquidity Book pool).
func (d *DB) ListEnabledPools(ctx context.Context) ([]Pool, error) {
	const q = `
		SELECT address, dex, token0, token1, dec0, dec1,
		       COALESCE(label,''), enabled, COALESCE(source_url,''),
		       z_threshold, whale_min_multiple, bin_step
		FROM pools
		WHERE enabled = TRUE
		ORDER BY address ASC`
	rows, err := d.Pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: query enabled pools: %w", err)
	}
	defer rows.Close()

	var out []Pool
	for rows.Next() {
		var p Pool
		if err := rows.Scan(
			&p.Address, &p.Dex, &p.Token0, &p.Token1, &p.Dec0, &p.Dec1,
			&p.Label, &p.Enabled, &p.SourceURL,
			&p.ZThreshold, &p.WhaleMinMultiple, &p.BinStep,
		); err != nil {
			return nil, fmt.Errorf("store: scan pool: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pools: %w", err)
	}
	return out, nil
}

// GetPool returns the registry entry for a single pool by address (lowercased), or
// (zero, false, nil) when no such pool exists. It serves the delivery path, which needs a
// signal's pool label and dex to render a readable alert instead of a raw address.
func (d *DB) GetPool(ctx context.Context, address string) (Pool, bool, error) {
	const q = `
		SELECT address, dex, token0, token1, dec0, dec1,
		       COALESCE(label,''), enabled, COALESCE(source_url,''),
		       z_threshold, whale_min_multiple, bin_step
		FROM pools
		WHERE address = $1`
	var p Pool
	err := d.Pool.QueryRow(ctx, q, strings.ToLower(strings.TrimSpace(address))).Scan(
		&p.Address, &p.Dex, &p.Token0, &p.Token1, &p.Dec0, &p.Dec1,
		&p.Label, &p.Enabled, &p.SourceURL,
		&p.ZThreshold, &p.WhaleMinMultiple, &p.BinStep,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pool{}, false, nil
	}
	if err != nil {
		return Pool{}, false, fmt.Errorf("store: get pool %s: %w", address, err)
	}
	return p, true, nil
}

// PoolPricesUSD returns the latest display price per pool (keyed by lowercase pool
// address), skipping pools with no price recorded yet. pool_stats.price_usd is token0's
// market price, so a detector can value a non-stable swap's token0 leg at it to fill the
// display-only size_usd. It reaches only size_usd, never a detection decision, so the
// detector reads it as plain data here rather than importing the market-data packages.
func (d *DB) PoolPricesUSD(ctx context.Context) (map[string]decimal.Decimal, error) {
	const q = `SELECT pool, price_usd::text FROM pool_stats WHERE price_usd IS NOT NULL`
	rows, err := d.Pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: query pool prices: %w", err)
	}
	defer rows.Close()

	out := make(map[string]decimal.Decimal)
	for rows.Next() {
		var pool, priceTxt string
		if err := rows.Scan(&pool, &priceTxt); err != nil {
			return nil, fmt.Errorf("store: scan pool price: %w", err)
		}
		price, err := decimal.NewFromString(priceTxt)
		if err != nil {
			return nil, fmt.Errorf("store: parse pool price %q: %w", priceTxt, err)
		}
		out[strings.ToLower(pool)] = price
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pool prices: %w", err)
	}
	return out, nil
}

// TokenPricesUSD returns the latest market price per token (keyed by lowercase token
// address), derived from pool_stats: price_usd there is the pool's token0 price, so the
// freshest price for each token0 is that token's market price. It lets a token-keyed
// signal (an LST transfer, whose subject is a token, not a pool) put a USD size on its
// amount. Display-only like PoolPricesUSD: it reaches only size_usd, never detection.
func (d *DB) TokenPricesUSD(ctx context.Context) (map[string]decimal.Decimal, error) {
	const q = `
		SELECT DISTINCT ON (lower(p.token0)) lower(p.token0), ps.price_usd::text
		FROM pools p
		JOIN pool_stats ps ON lower(ps.pool) = lower(p.address)
		WHERE ps.price_usd IS NOT NULL
		ORDER BY lower(p.token0), ps.fetched_at DESC`
	rows, err := d.Pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: query token prices: %w", err)
	}
	defer rows.Close()

	out := make(map[string]decimal.Decimal)
	for rows.Next() {
		var token, priceTxt string
		if err := rows.Scan(&token, &priceTxt); err != nil {
			return nil, fmt.Errorf("store: scan token price: %w", err)
		}
		price, err := decimal.NewFromString(priceTxt)
		if err != nil {
			return nil, fmt.Errorf("store: parse token price %q: %w", priceTxt, err)
		}
		out[token] = price
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate token prices: %w", err)
	}
	return out, nil
}

// ListPoolAddresses returns the lowercase address of every pool (enabled or not) as a
// set, which the discover command consults to skip pools it already knows, so discovery
// never duplicates a row or downgrades an already-enabled pool by re-proposing it.
func (d *DB) ListPoolAddresses(ctx context.Context) (map[string]struct{}, error) {
	const q = `SELECT address FROM pools`
	rows, err := d.Pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: query pool addresses: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, fmt.Errorf("store: scan pool address: %w", err)
		}
		out[strings.ToLower(addr)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pool addresses: %w", err)
	}
	return out, nil
}

// UpsertPool inserts or updates a pool by address (lowercased). The nullable tuning
// overrides (z_threshold, whale_min_multiple) pass as *float64 (nil -> NULL = use the
// global default); bin_step as *int.
//
// bin_step alone uses COALESCE(EXCLUDED.bin_step, pools.bin_step) so a nil-BinStep upsert
// never clears a backfilled value: seedPools re-upserts the curated YAML (no bin_step) on
// every startup, and without the COALESCE each restart would wipe the merchant_moe pools'
// bin step and break their lp_url until `ocr poolmeta` reran.
func (d *DB) UpsertPool(ctx context.Context, p Pool) error {
	const q = `
		INSERT INTO pools
			(address, dex, token0, token1, dec0, dec1, label, enabled, source_url,
			 z_threshold, whale_min_multiple, bin_step)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (address) DO UPDATE SET
			dex                = EXCLUDED.dex,
			token0             = EXCLUDED.token0,
			token1             = EXCLUDED.token1,
			dec0               = EXCLUDED.dec0,
			dec1               = EXCLUDED.dec1,
			label              = EXCLUDED.label,
			enabled            = EXCLUDED.enabled,
			source_url         = EXCLUDED.source_url,
			z_threshold        = EXCLUDED.z_threshold,
			whale_min_multiple = EXCLUDED.whale_min_multiple,
			bin_step           = COALESCE(EXCLUDED.bin_step, pools.bin_step)`
	_, err := d.Pool.Exec(ctx, q,
		strings.ToLower(p.Address),
		p.Dex,
		strings.ToLower(p.Token0),
		strings.ToLower(p.Token1),
		p.Dec0,
		p.Dec1,
		p.Label,
		p.Enabled,
		p.SourceURL,
		p.ZThreshold,
		p.WhaleMinMultiple,
		intPtrOrNil(p.BinStep),
	)
	if err != nil {
		return fmt.Errorf("store: upsert pool %s: %w", strings.ToLower(p.Address), err)
	}
	return nil
}

// UpsertBucket inserts or replaces a bucket aggregate by (pool, ts_bucket).
// NUMERIC(78) values are passed as strings to preserve full precision.
func (d *DB) UpsertBucket(ctx context.Context, b Bucket) error {
	const q = `
		INSERT INTO buckets
			(pool, ts_bucket, swap_count, vol0, vol1, netflow0, netflow1)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (pool, ts_bucket) DO UPDATE SET
			swap_count = EXCLUDED.swap_count,
			vol0       = EXCLUDED.vol0,
			vol1       = EXCLUDED.vol1,
			netflow0   = EXCLUDED.netflow0,
			netflow1   = EXCLUDED.netflow1`
	_, err := d.Pool.Exec(ctx, q,
		strings.ToLower(b.Pool),
		b.TsBucket,
		b.SwapCount,
		b.Vol0.String(),
		b.Vol1.String(),
		b.Netflow0.String(),
		b.Netflow1.String(),
	)
	if err != nil {
		return fmt.Errorf("store: upsert bucket pool=%s ts=%s: %w",
			strings.ToLower(b.Pool), b.TsBucket.Format(time.RFC3339), err)
	}
	return nil
}

// GetRecentBuckets returns up to `limit` most-recent buckets for a pool, with
// the result re-ordered ts_bucket ASC (oldest first) so callers can run windowed
// math left-to-right.
func (d *DB) GetRecentBuckets(ctx context.Context, pool string, limit int) ([]Bucket, error) {
	if limit <= 0 {
		return nil, nil
	}
	const q = `
		SELECT pool, ts_bucket, swap_count,
		       vol0::text, vol1::text, netflow0::text, netflow1::text
		FROM (
			SELECT pool, ts_bucket, swap_count, vol0, vol1, netflow0, netflow1
			FROM buckets
			WHERE pool = $1
			ORDER BY ts_bucket DESC
			LIMIT $2
		) recent
		ORDER BY ts_bucket ASC`
	rows, err := d.Pool.Query(ctx, q, strings.ToLower(pool), limit)
	if err != nil {
		return nil, fmt.Errorf("store: query recent buckets pool=%s: %w", strings.ToLower(pool), err)
	}
	defer rows.Close()
	return scanBuckets(rows)
}

// bucketColumns is the canonical SELECT column list (and order) for a bucket row,
// NUMERIC(78) columns cast ::text to preserve precision. Shared by every bucket query
// so the projection and scanBuckets stay in lock-step.
const bucketColumns = `pool, ts_bucket, swap_count,
	       vol0::text, vol1::text, netflow0::text, netflow1::text`

// scanBuckets scans a SELECT of bucketColumns into a slice of Bucket, parsing the
// ::text NUMERIC(78) columns back into decimal.Decimal. The caller owns rows.Close.
func scanBuckets(rows pgx.Rows) ([]Bucket, error) {
	var out []Bucket
	for rows.Next() {
		var (
			b                      Bucket
			vol0, vol1, net0, net1 string
		)
		if err := rows.Scan(
			&b.Pool, &b.TsBucket, &b.SwapCount, &vol0, &vol1, &net0, &net1,
		); err != nil {
			return nil, fmt.Errorf("store: scan bucket: %w", err)
		}
		var err error
		if b.Vol0, err = decimal.NewFromString(vol0); err != nil {
			return nil, fmt.Errorf("store: parse vol0 %q: %w", vol0, err)
		}
		if b.Vol1, err = decimal.NewFromString(vol1); err != nil {
			return nil, fmt.Errorf("store: parse vol1 %q: %w", vol1, err)
		}
		if b.Netflow0, err = decimal.NewFromString(net0); err != nil {
			return nil, fmt.Errorf("store: parse netflow0 %q: %w", net0, err)
		}
		if b.Netflow1, err = decimal.NewFromString(net1); err != nil {
			return nil, fmt.Errorf("store: parse netflow1 %q: %w", net1, err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate buckets: %w", err)
	}
	return out, nil
}

// BucketsBetween returns a pool's buckets in the half-open window (from, to],
// oldest-first for left-to-right windowed math. The outcome scorer uses it for both the
// post-signal window (T, T+H] and the pre-signal window (T-H, T].
func (d *DB) BucketsBetween(ctx context.Context, pool string, from, to time.Time) ([]Bucket, error) {
	const q = `
		SELECT ` + bucketColumns + `
		FROM buckets
		WHERE pool = $1 AND ts_bucket > $2 AND ts_bucket <= $3
		ORDER BY ts_bucket ASC`
	rows, err := d.Pool.Query(ctx, q, strings.ToLower(pool), from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: query buckets between pool=%s: %w", strings.ToLower(pool), err)
	}
	defer rows.Close()
	return scanBuckets(rows)
}

// LastBucketTime returns the newest ts_bucket for a pool. The bool is false
// (with a nil error) when the pool has no buckets yet.
func (d *DB) LastBucketTime(ctx context.Context, pool string) (time.Time, bool, error) {
	const q = `SELECT ts_bucket FROM buckets WHERE pool = $1 ORDER BY ts_bucket DESC LIMIT 1`
	var ts time.Time
	err := d.Pool.QueryRow(ctx, q, strings.ToLower(pool)).Scan(&ts)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: last bucket time pool=%s: %w", strings.ToLower(pool), err)
	}
	return ts, true, nil
}

// InsertSignal persists a detector finding idempotently and returns its generated id.
// On a business-key collision the insert is a no-op (inserted false, id 0), so
// re-running a detector never duplicates a signal row or a downstream attestation. The
// key depends on the family (see migrations/0001_init.sql and 0002_whale.sql):
//   - Bucket signals (EventRef == "", e.g. flow): (pool, signal_type, metric, bucket_ts).
//     A zero BucketTS stores NULL and is exempt (NULLs compare unequal).
//   - Per-event signals (EventRef != "", e.g. whale): (pool, signal_type, event_ref).
func (d *DB) InsertSignal(ctx context.Context, s Signal) (id int64, inserted bool, err error) {
	payload := s.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}

	// Two variants differing only in the ON CONFLICT target. Both unique indexes are
	// partial (see migrations/0002_whale.sql), so the conflict target must repeat the index
	// predicate (WHERE event_ref IS [NOT] NULL): Postgres infers a partial index as the
	// arbiter only when the predicate matches, else it raises "no unique or exclusion
	// constraint matching the ON CONFLICT specification" at runtime.
	const qBucket = `
		INSERT INTO signals
			(pool, signal_type, metric, zscore, window_buckets,
			 bucket_ts, payload, llm_note, attest_tx, event_ref, actor, size_usd, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,COALESCE(NULLIF($13,''),'new'))
		ON CONFLICT (pool, signal_type, metric, bucket_ts) WHERE event_ref IS NULL DO NOTHING
		RETURNING id`
	const qEvent = `
		INSERT INTO signals
			(pool, signal_type, metric, zscore, window_buckets,
			 bucket_ts, payload, llm_note, attest_tx, event_ref, actor, size_usd, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,COALESCE(NULLIF($13,''),'new'))
		ON CONFLICT (pool, signal_type, event_ref) WHERE event_ref IS NOT NULL DO NOTHING
		RETURNING id`

	q := qBucket
	if s.EventRef != "" {
		q = qEvent
	}

	err = d.Pool.QueryRow(ctx, q,
		strings.ToLower(s.Pool),
		s.SignalType,
		s.Metric,
		s.Zscore,
		s.WindowBuckets,
		timeOrNil(s.BucketTS),
		[]byte(payload),
		nilIfEmpty(s.LLMNote),
		nilIfEmpty(s.AttestTx),
		nilIfEmpty(s.EventRef),
		lowerNilIfEmpty(s.Actor),
		decStr(s.SizeUSD), // display-only; nil -> NULL
		s.Status,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING returned no row: already recorded, not an error.
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: insert signal pool=%s: %w", strings.ToLower(s.Pool), err)
	}
	return id, true, nil
}

// UpdateSignalNote sets the LLM enrichment note on a signal.
func (d *DB) UpdateSignalNote(ctx context.Context, id int64, note string) error {
	const q = `UPDATE signals SET llm_note = $2 WHERE id = $1`
	ct, err := d.Pool.Exec(ctx, q, id, note)
	if err != nil {
		return fmt.Errorf("store: update signal %d note: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: update signal %d note: no such signal", id)
	}
	return nil
}

// UpdateSignalAttest records the attestation tx hash on a signal (lowercased).
func (d *DB) UpdateSignalAttest(ctx context.Context, id int64, tx string) error {
	const q = `UPDATE signals SET attest_tx = $2 WHERE id = $1`
	ct, err := d.Pool.Exec(ctx, q, id, strings.ToLower(tx))
	if err != nil {
		return fmt.Errorf("store: update signal %d attest: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: update signal %d attest: no such signal", id)
	}
	return nil
}

// UpdateSignalTGMessageID records the Telegram message_id of a signal's alert, stored
// verbatim. An empty msgID is rejected; a no-op write (no such signal) is an error so a
// lost row is never silent.
func (d *DB) UpdateSignalTGMessageID(ctx context.Context, id int64, msgID string) error {
	if msgID == "" {
		return fmt.Errorf("store: update signal %d tg_message_id: empty message id", id)
	}
	const q = `UPDATE signals SET tg_message_id = $2 WHERE id = $1`
	ct, err := d.Pool.Exec(ctx, q, id, msgID)
	if err != nil {
		return fmt.Errorf("store: update signal %d tg_message_id: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: update signal %d tg_message_id: no such signal", id)
	}
	return nil
}

// SetSignalStatus transitions a signal's lifecycle status.
func (d *DB) SetSignalStatus(ctx context.Context, id int64, status string) error {
	const q = `UPDATE signals SET status = $2 WHERE id = $1`
	ct, err := d.Pool.Exec(ctx, q, id, status)
	if err != nil {
		return fmt.Errorf("store: set signal %d status: %w", id, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: set signal %d status: no such signal", id)
	}
	return nil
}

// signalColumns is the canonical SELECT column list (and order) for a full Signal row,
// shared by every signal query so the projection and scanSignals stay in lock-step.
// COALESCE keeps nullable text columns out of non-NULL scan targets. Columns are
// `s`-qualified so the projection is unambiguous when joined against signal_outcomes;
// every signal query therefore aliases the signals table AS s.
const signalColumns = `s.id, s.created_at, s.bucket_ts, s.pool, s.signal_type, s.metric, s.zscore,
	       s.window_buckets, s.payload, COALESCE(s.llm_note,''),
	       COALESCE(s.attest_tx,''), COALESCE(s.event_ref,''), s.status,
	       COALESCE(s.tg_message_id,''), COALESCE(s.actor,''), s.size_usd::text`

// scanSignals scans a SELECT of signalColumns into a slice of Signal, owning the
// nullable bucket_ts/size_usd handling so callers share one scan path. The caller owns
// rows.Close.
func scanSignals(rows pgx.Rows) ([]Signal, error) {
	var out []Signal
	for rows.Next() {
		var (
			s         Signal
			bucketTS  *time.Time // nullable column
			sizeUSDTx *string    // nullable NUMERIC, cast ::text
		)
		if err := rows.Scan(
			&s.ID, &s.CreatedAt, &bucketTS, &s.Pool, &s.SignalType, &s.Metric,
			&s.Zscore, &s.WindowBuckets, &s.Payload, &s.LLMNote, &s.AttestTx, &s.EventRef, &s.Status,
			&s.TGMessageID, &s.Actor, &sizeUSDTx,
		); err != nil {
			return nil, fmt.Errorf("store: scan signal: %w", err)
		}
		if bucketTS != nil {
			s.BucketTS = *bucketTS
		}
		sz, perr := decimalPtr(sizeUSDTx)
		if perr != nil {
			return nil, fmt.Errorf("store: parse signal size_usd: %w", perr)
		}
		s.SizeUSD = sz
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate signals: %w", err)
	}
	return out, nil
}

// RecentSignals returns up to `limit` signals, newest first.
func (d *DB) RecentSignals(ctx context.Context, limit int) ([]Signal, error) {
	if limit <= 0 {
		return nil, nil
	}
	const q = `
		SELECT ` + signalColumns + `
		FROM signals s
		ORDER BY s.created_at DESC
		LIMIT $1`
	rows, err := d.Pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query recent signals: %w", err)
	}
	defer rows.Close()
	return scanSignals(rows)
}

// SignalsNeedingNote returns up to `limit` signals still lacking an analyst note
// (llm_note IS NULL or all-whitespace), newest first. It is the work queue for the
// async enrich stage; newest-first so the freshest, most demo-relevant signals enrich
// first, and the bound drains a backlog over several ticks rather than one sweep.
func (d *DB) SignalsNeedingNote(ctx context.Context, limit int) ([]Signal, error) {
	if limit <= 0 {
		return nil, nil
	}
	// NULL or all-whitespace = "needs a note". The [[:space:]] class matches Go's
	// strings.TrimSpace (space/tab/newline/CR/FF/VT); a bare btrim would strip only
	// spaces, so a tab/newline-only note would wrongly look non-empty.
	const q = `
		SELECT ` + signalColumns + `
		FROM signals s
		WHERE s.llm_note IS NULL OR s.llm_note ~ '^[[:space:]]*$'
		ORDER BY s.created_at DESC
		LIMIT $1`
	rows, err := d.Pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query signals needing note: %w", err)
	}
	defer rows.Close()
	return scanSignals(rows)
}

// SignalsAwaitingOutcome returns up to `limit` matured signals with no outcome row yet:
// non-NULL bucket_ts at or before olderThan (horizon elapsed) and no signal_outcomes
// row. The LEFT JOIN ... WHERE so.signal_id IS NULL anti-join excludes already-measured
// signals; the measure-once InsertOutcome makes the pass idempotent under overlap.
// Oldest-first so a bounded batch drains deterministically. NULL bucket_ts is excluded:
// no anchor time means no horizon to measure against.
func (d *DB) SignalsAwaitingOutcome(ctx context.Context, olderThan time.Time, limit int) ([]Signal, error) {
	if limit <= 0 {
		return nil, nil
	}
	const q = `
		SELECT ` + signalColumns + `
		FROM signals s
		LEFT JOIN signal_outcomes so ON so.signal_id = s.id
		WHERE so.signal_id IS NULL
		  AND s.bucket_ts IS NOT NULL
		  AND s.bucket_ts <= $1
		ORDER BY s.bucket_ts ASC
		LIMIT $2`
	rows, err := d.Pool.Query(ctx, q, olderThan.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: query signals awaiting outcome: %w", err)
	}
	defer rows.Close()
	return scanSignals(rows)
}

// LastSignalTime returns the newest created_at for a pool's signals. The bool is
// false (with a nil error) when the pool has no signals yet.
func (d *DB) LastSignalTime(ctx context.Context, pool string) (time.Time, bool, error) {
	const q = `SELECT created_at FROM signals WHERE pool = $1 ORDER BY created_at DESC LIMIT 1`
	var ts time.Time
	err := d.Pool.QueryRow(ctx, q, strings.ToLower(pool)).Scan(&ts)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: last signal time pool=%s: %w", strings.ToLower(pool), err)
	}
	return ts, true, nil
}

// LastSignalTimeByType returns the newest created_at for a pool's signals of a given
// signal_type (false/nil error when none). Scoping to one family keeps each detector's
// per-pool cooldown independent of the others.
func (d *DB) LastSignalTimeByType(ctx context.Context, pool string, signalType int16) (time.Time, bool, error) {
	const q = `SELECT created_at FROM signals WHERE pool = $1 AND signal_type = $2 ORDER BY created_at DESC LIMIT 1`
	var ts time.Time
	err := d.Pool.QueryRow(ctx, q, strings.ToLower(pool), signalType).Scan(&ts)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: last signal time pool=%s type=%d: %w", strings.ToLower(pool), signalType, err)
	}
	return ts, true, nil
}

// LastSignalTimeByActor returns the newest created_at for signals of a given
// signal_type fired by a given actor (false/nil error when none). It backs the
// smart-money detector's per-actor cooldown: keying on the actor (not the pool) stops
// one accumulating actor from firing once per pool. The partial index
// idx_signals_actor_type_created (actor IS NOT NULL) serves it.
func (d *DB) LastSignalTimeByActor(ctx context.Context, actor string, signalType int16) (time.Time, bool, error) {
	const q = `SELECT created_at FROM signals WHERE actor = $1 AND signal_type = $2 ORDER BY created_at DESC LIMIT 1`
	var ts time.Time
	err := d.Pool.QueryRow(ctx, q, strings.ToLower(actor), signalType).Scan(&ts)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: last signal time actor=%s type=%d: %w", strings.ToLower(actor), signalType, err)
	}
	return ts, true, nil
}

// InsertOutcome persists a matured signal's report card idempotently (measure once):
// inserted=true only when this call wrote the row, so concurrent or re-run passes never
// double-count or overwrite a verdict. The canonical card JSON and its keccak256
// report_hash are stored verbatim so the hash is reproducible from the stored card.
func (d *DB) InsertOutcome(ctx context.Context, o Outcome) (bool, error) {
	card := o.Card
	if len(card) == 0 {
		// A report card is never legitimately empty; refuse to store a row that
		// could not be re-hashed rather than silently writing "{}".
		return false, fmt.Errorf("store: insert outcome signal=%d: empty card", o.SignalID)
	}
	const q = `
		INSERT INTO signal_outcomes
			(signal_id, horizon_hours, grade, letter, status, report_hash, card)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (signal_id) DO NOTHING
		RETURNING signal_id`
	var id int64
	err := d.Pool.QueryRow(ctx, q,
		o.SignalID,
		o.HorizonHours,
		o.Grade,
		o.Letter,
		o.Status,
		o.ReportHash,
		string(card), // TEXT column: store exact canonical bytes (string, not []byte→bytea) so the stored card re-hashes to report_hash
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING returned no row: already measured.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: insert outcome signal=%d: %w", o.SignalID, err)
	}
	return true, nil
}

// OutcomeAttestRow is one matured outcome needing an on-chain attestation, joined to
// the signal it grades. It carries the full Signal (so the caller re-derives the same
// signalHash via attest.SignalHash / SignalHashEvent, never re-hashing in the store)
// plus the outcome's Grade and ReportHash. Returned by OutcomesAwaitingAttest.
type OutcomeAttestRow struct {
	Signal     Signal
	Grade      int    // composite outcome score 0..100 (fits uint8)
	ReportHash string // 0x-hex keccak256 of the canonical card
}

// OutcomesAwaitingAttest returns up to `limit` measured outcomes not yet attested
// on-chain (attest_tx IS NULL or empty), each joined to its signal so the caller can
// re-derive the signalHash and submit attestOutcome(). The attest_tx exclusion is the
// idempotency gate: a re-run never re-attests. Oldest-first (signal bucket_ts, then id)
// so a bounded batch drains deterministically. No bucket_ts filter: a NULL-bucket_ts
// per-event signal hashes on its event_ref instead, so it is still returned.
func (d *DB) OutcomesAwaitingAttest(ctx context.Context, limit int) ([]OutcomeAttestRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	const q = `
		SELECT ` + signalColumns + `, so.grade, so.report_hash
		FROM signal_outcomes so
		JOIN signals s ON s.id = so.signal_id
		WHERE so.attest_tx IS NULL OR so.attest_tx = ''
		ORDER BY s.bucket_ts ASC NULLS FIRST, s.id ASC
		LIMIT $1`
	rows, err := d.Pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query outcomes awaiting attest: %w", err)
	}
	defer rows.Close()

	var out []OutcomeAttestRow
	for rows.Next() {
		var (
			s         Signal
			bucketTS  *time.Time // nullable column
			sizeUSDTx *string    // nullable NUMERIC, cast ::text
			grade     int
			report    string
		)
		// signalColumns order plus the two appended outcome columns; kept in
		// lock-step manually since this is a joined row, not a bare signal.
		if err := rows.Scan(
			&s.ID, &s.CreatedAt, &bucketTS, &s.Pool, &s.SignalType, &s.Metric,
			&s.Zscore, &s.WindowBuckets, &s.Payload, &s.LLMNote, &s.AttestTx, &s.EventRef, &s.Status,
			&s.TGMessageID, &s.Actor, &sizeUSDTx,
			&grade, &report,
		); err != nil {
			return nil, fmt.Errorf("store: scan outcome awaiting attest: %w", err)
		}
		if bucketTS != nil {
			s.BucketTS = *bucketTS
		}
		sz, perr := decimalPtr(sizeUSDTx)
		if perr != nil {
			return nil, fmt.Errorf("store: parse outcome size_usd: %w", perr)
		}
		s.SizeUSD = sz
		out = append(out, OutcomeAttestRow{Signal: s, Grade: grade, ReportHash: report})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate outcomes awaiting attest: %w", err)
	}
	return out, nil
}

// UpdateOutcomeAttest records the on-chain outcome-attestation tx hash on a
// signal_outcomes row (lowercased), mirroring UpdateSignalAttest. A no-op write
// (no such outcome) is an error so a lost row is never silent.
func (d *DB) UpdateOutcomeAttest(ctx context.Context, signalID int64, tx string) error {
	const q = `UPDATE signal_outcomes SET attest_tx = $2 WHERE signal_id = $1`
	ct, err := d.Pool.Exec(ctx, q, signalID, strings.ToLower(tx))
	if err != nil {
		return fmt.Errorf("store: update outcome %d attest: %w", signalID, err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("store: update outcome %d attest: no such outcome", signalID)
	}
	return nil
}

// OutcomeStat is the aggregate report-card summary: a per-letter count, the
// total number of measured outcomes, and the mean grade across them. It feeds a
// future stats API; MeanGrade is 0 when Total is 0.
type OutcomeStat struct {
	ByLetter  map[string]int
	Total     int
	MeanGrade float64
}

// OutcomeStats returns the report-card distribution: per-letter counts, the total
// measured, and the mean grade. An empty table yields a zero-value stat (empty map,
// Total 0, MeanGrade 0).
func (d *DB) OutcomeStats(ctx context.Context) (OutcomeStat, error) {
	const q = `
		SELECT letter, COUNT(*), AVG(grade)::float8
		FROM signal_outcomes
		GROUP BY letter`
	rows, err := d.Pool.Query(ctx, q)
	if err != nil {
		return OutcomeStat{}, fmt.Errorf("store: query outcome stats: %w", err)
	}
	defer rows.Close()

	stat := OutcomeStat{ByLetter: make(map[string]int)}
	var weightedSum float64
	for rows.Next() {
		var (
			letter   string
			count    int
			avgGrade float64
		)
		if err := rows.Scan(&letter, &count, &avgGrade); err != nil {
			return OutcomeStat{}, fmt.Errorf("store: scan outcome stat: %w", err)
		}
		stat.ByLetter[letter] = count
		stat.Total += count
		weightedSum += avgGrade * float64(count)
	}
	if err := rows.Err(); err != nil {
		return OutcomeStat{}, fmt.Errorf("store: iterate outcome stats: %w", err)
	}
	if stat.Total > 0 {
		stat.MeanGrade = weightedSum / float64(stat.Total)
	}
	return stat, nil
}

// InsertLargeSwap records one resolved-actor large swap in the large_swaps ledger
// idempotently, reporting whether this call wrote the row. The business key
// (tx_hash, log_index) makes a re-scan a no-op, so the detector's overlapping candidate
// windows never double-count an actor's accumulation. Hex is lowercased on write; the
// NUMERIC(78) size + signed net0/net1 (for the net-vs-churn computation) pass as
// strings to preserve precision.
func (d *DB) InsertLargeSwap(ctx context.Context, ls LargeSwap) (bool, error) {
	const q = `
		INSERT INTO large_swaps
			(tx_hash, log_index, pool, actor, size_token0, net0, net1, block_time)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (tx_hash, log_index) DO NOTHING
		RETURNING tx_hash`
	var ref string
	err := d.Pool.QueryRow(ctx, q,
		strings.ToLower(ls.TxHash),
		ls.LogIndex,
		strings.ToLower(ls.Pool),
		strings.ToLower(ls.Actor),
		ls.SizeToken0.String(),
		ls.Net0.String(),
		ls.Net1.String(),
		ls.BlockTime.UTC(),
	).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING returned no row: already recorded, not an error.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: insert large_swap tx=%s idx=%d: %w",
			strings.ToLower(ls.TxHash), ls.LogIndex, err)
	}
	return true, nil
}

// ActorActivitySince returns an actor's accumulation footprint in the large_swaps
// ledger at or after `since`: the large-swap count and the distinct pools touched, in
// one round-trip. It is the windowed query behind the smart-money detector's
// accumulation factor (same EOA, repeated large swaps, possibly across pools).
func (d *DB) ActorActivitySince(ctx context.Context, actor string, since time.Time) (swaps int, distinctPools int, err error) {
	const q = `
		SELECT COUNT(*), COUNT(DISTINCT pool)
		FROM large_swaps
		WHERE actor = $1 AND block_time >= $2`
	if err := d.Pool.QueryRow(ctx, q, strings.ToLower(actor), since.UTC()).
		Scan(&swaps, &distinctPools); err != nil {
		return 0, 0, fmt.Errorf("store: actor activity since actor=%s: %w", strings.ToLower(actor), err)
	}
	return swaps, distinctPools, nil
}

// TokenFlow is one token's signed-net and gross flow for an actor over a window,
// keyed by the token's contract address (lowercase). The smart-money detector uses
// it to compute directionality = max over tokens of |Net|/Gross: a pure
// accumulator nets ~= gross on some asset (directionality ~= 1), while a churning
// arb/MM bot's buys and sells cancel (Net ~= 0, directionality ~= 0).
type TokenFlow struct {
	Token string          // lowercase token contract address
	Net   decimal.Decimal // signed net change for the actor (positive = received)
	Gross decimal.Decimal // gross (absolute) flow = sum(|net contribution|)
}

// ActorTokenFlowsSince returns an actor's per-token-address signed-net and gross flow
// across its large swaps at or after `since`, joining large_swaps to pools for the
// token addresses. Netting is by token address, not (pool, slot): the same asset may be
// token0 in one pool and token1 in another, so cross-pool arb cancels only when both
// sides sum under one address key. Gross is the sum of absolute per-swap contributions
// (not |sum|), so opposite legs do not cancel and a churner cannot dodge the
// divide-by-zero guard. net0/net1 COALESCE to 0 (pre-0008 rows contribute nothing). The
// idx_large_swaps_actor index serves the (actor, block_time >= since) scan.
func (d *DB) ActorTokenFlowsSince(ctx context.Context, actor string, since time.Time) ([]TokenFlow, error) {
	// Unpivot each swap into two (token_address, signed_net) rows, then aggregate by
	// address: SUM(signed_net) is the net, SUM(ABS) the gross (per-leg absolute).
	const q = `
		WITH legs AS (
			SELECT p.token0 AS token, COALESCE(ls.net0, 0) AS net
			FROM large_swaps ls
			JOIN pools p ON p.address = ls.pool
			WHERE ls.actor = $1 AND ls.block_time >= $2
			UNION ALL
			SELECT p.token1 AS token, COALESCE(ls.net1, 0) AS net
			FROM large_swaps ls
			JOIN pools p ON p.address = ls.pool
			WHERE ls.actor = $1 AND ls.block_time >= $2
		)
		SELECT token, SUM(net)::text AS net, SUM(ABS(net))::text AS gross
		FROM legs
		GROUP BY token`
	rows, err := d.Pool.Query(ctx, q, strings.ToLower(actor), since.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: actor token flows since actor=%s: %w", strings.ToLower(actor), err)
	}
	defer rows.Close()

	var out []TokenFlow
	for rows.Next() {
		var (
			token      string
			net, gross string
		)
		if err := rows.Scan(&token, &net, &gross); err != nil {
			return nil, fmt.Errorf("store: scan actor token flow actor=%s: %w", strings.ToLower(actor), err)
		}
		tf := TokenFlow{Token: token}
		if tf.Net, err = decimal.NewFromString(net); err != nil {
			return nil, fmt.Errorf("store: parse token flow net %q: %w", net, err)
		}
		if tf.Gross, err = decimal.NewFromString(gross); err != nil {
			return nil, fmt.Errorf("store: parse token flow gross %q: %w", gross, err)
		}
		out = append(out, tf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate actor token flows actor=%s: %w", strings.ToLower(actor), err)
	}
	return out, nil
}

// lowerOrNil lowercases a topic, returning nil for empty strings so the column
// stays NULL rather than an empty string (topic1-3 are nullable).
func lowerOrNil(s string) any {
	if s == "" {
		return nil
	}
	return strings.ToLower(s)
}

// nilIfEmpty returns nil for empty strings so nullable text columns stay NULL.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// lowerNilIfEmpty returns nil for empty strings (nullable column stays NULL), else the
// lowercased value. Used for signals.actor (lowercase hex; NULL for no actor).
func lowerNilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return strings.ToLower(s)
}

// timeOrNil returns nil for the zero time (nullable TIMESTAMPTZ stays NULL), else the
// UTC time. Used for signals.bucket_ts: a zero BucketTS stores NULL and is exempt from
// the (pool, signal_type, metric, bucket_ts) unique key.
func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// lowerAll returns a new slice with every element lowercased.
func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}
