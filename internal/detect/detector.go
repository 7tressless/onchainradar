package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/rs/zerolog/log"

	"ocr/internal/config"
	"ocr/internal/store"
)

// metricNames is the fixed set of per-bucket flow metrics scored for relative
// anomalies. Order is stable so logs and payloads compare across runs; each name
// maps 1:1 to a field extracted by metricValue.
var metricNames = []string{"swap_count", "vol0", "vol1", "netflow0", "netflow1"}

// swapCountFloor is the minimum latest swap_count before a swap_count z-score can
// fire. On a quiet pool 1->3 swaps is a large modified z but economically
// meaningless, so gate on a real activity floor, not statistical noise.
const swapCountFloor = 10

// Detector scores recent per-pool flow buckets against their own MAD baseline and
// emits a store.Signal when the latest bucket is a robust outlier on any metric.
type Detector struct {
	db  *store.DB
	cfg *config.Config
}

// NewDetector constructs a Detector over the given store and config. The config
// supplies the tunables BaselineBuckets, ZThreshold, and CooldownMin.
func NewDetector(db *store.DB, cfg *config.Config) *Detector {
	return &Detector{db: db, cfg: cfg}
}

// metricValue extracts a single named metric from a bucket as a float64. The
// decimal volume/netflow columns go through lossy InexactFloat64: a z-score only
// needs magnitude, and the precise value still lives in the signal payload.
func metricValue(b store.Bucket, metric string) float64 {
	switch metric {
	case "swap_count":
		return float64(b.SwapCount)
	case "vol0":
		return b.Vol0.InexactFloat64()
	case "vol1":
		return b.Vol1.InexactFloat64()
	case "netflow0":
		return b.Netflow0.InexactFloat64()
	case "netflow1":
		return b.Netflow1.InexactFloat64()
	default:
		// Unreachable (callers iterate metricNames). NaN so any accidental use is
		// rejected by the finite-value guard, not silently scored as zero.
		return math.NaN()
	}
}

// absFloorOK applies the per-metric absolute floor a candidate outlier must clear
// before firing. The MAD z-score is purely relative, so without this a pool idling
// near zero would alarm on trivial absolute moves. swap_count gates on a hard
// minimum count; volume/netflow require both the baseline median and the latest
// value non-zero, rejecting "first activity after a dead window".
func absFloorOK(metric string, latest, median float64) bool {
	if metric == "swap_count" {
		return latest >= swapCountFloor
	}
	return median > 0 && latest > 0
}

// metricResult holds a single metric's evaluation for one pool's latest bucket.
type metricResult struct {
	metric string
	z      float64
	latest float64
	median float64
	mad    float64
}

// signalPayload is the JSON written to store.Signal.Payload, capturing the
// inputs that produced the z-score for later enrichment and audit.
type signalPayload struct {
	Latest   float64 `json:"latest"`
	Median   float64 `json:"median"`
	MAD      float64 `json:"mad"`
	BucketTS string  `json:"bucket_ts"`
}

// ScanOnce evaluates every enabled pool once and persists a signal for each pool
// that produces a fresh, non-cooldown outlier, returning the inserted signals
// (with ids) for the enrichment/delivery stages. Per pool it pulls the last
// BaselineBuckets+1 buckets: the newest is the candidate, the rest the baseline.
//
// The aggregator only writes a bucket row for a window with at least one swap, so
// the baseline is the last BaselineBuckets active buckets, not a fixed
// BaselineBuckets*BUCKET_MIN wall-clock window. Intentional for now; revisit
// (gap-fill empty windows, or time-bound the query) if per-pool activity gets
// bursty enough to matter.
func (d *Detector) ScanOnce(ctx context.Context) ([]store.Signal, error) {
	pools, err := d.db.ListEnabledPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect: list enabled pools: %w", err)
	}

	// At least 12 samples regardless of config; a quarter of the window gives a
	// more stable median once the pool is mature. Too few yields an unstable MAD.
	warmup := d.cfg.BaselineBuckets / 4
	if warmup < 12 {
		warmup = 12
	}
	cooldown := time.Duration(d.cfg.CooldownMin) * time.Minute

	// Token0 market prices for the display-only flow burst size, loaded once.
	// Best-effort: an unavailable snapshot leaves size_usd nil, never a detection input.
	prices, perr := d.db.PoolPricesUSD(ctx)
	if perr != nil {
		log.Warn().Err(perr).Msg("detect: flow pool prices unavailable, flow size_usd stays nil")
		prices = nil
	}

	var out []store.Signal
	for _, p := range pools {
		buckets, err := d.db.GetRecentBuckets(ctx, p.Address, d.cfg.BaselineBuckets+1)
		if err != nil {
			return out, fmt.Errorf("detect: recent buckets pool=%s: %w", p.Address, err)
		}

		// Skip immature pools: an unstable baseline produces noise, not signal.
		if len(buckets) < warmup {
			log.Debug().
				Str("pool", p.Address).
				Int("buckets", len(buckets)).
				Int("warmup", warmup).
				Msg("detect: pool below warmup, skipping")
			continue
		}

		// Cooldown gate: suppress repeat alerts for an anomaly that spans
		// multiple buckets. Done before the (heavier) metric loop.
		inCooldown, err := d.poolInCooldown(ctx, p.Address, cooldown)
		if err != nil {
			return out, err
		}
		if inCooldown {
			log.Debug().
				Str("pool", p.Address).
				Dur("cooldown", cooldown).
				Msg("detect: pool in cooldown, skipping")
			continue
		}

		// GetRecentBuckets returns ascending (oldest first), so the latest
		// candidate is the final element and the baseline is everything before.
		latest := buckets[len(buckets)-1]
		baseline := buckets[:len(buckets)-1]

		// Per-pool z-threshold: an override lets heavy-tailed pools use a higher gate.
		// It only changes which signals fire, never a signal's hash (keyed on
		// pool/type/metric/bucket_ts, not the gate).
		zt := effectiveZThreshold(d.cfg, p)

		best, ok := d.scoreBucket(latest, baseline, zt)
		if !ok {
			continue
		}

		sig := store.Signal{
			Pool:          p.Address,
			SignalType:    store.SignalTypeFlow,
			Metric:        best.metric,
			Zscore:        best.z,
			WindowBuckets: len(baseline),
			BucketTS:      latest.TsBucket,
			SizeUSD:       bucketSizeUSD(p, latest.Vol0, poolPrice(prices, p.Address)),
			Status:        store.StatusNew,
		}

		payload, err := json.Marshal(signalPayload{
			Latest:   best.latest,
			Median:   best.median,
			MAD:      best.mad,
			BucketTS: latest.TsBucket.UTC().Format(time.RFC3339),
		})
		if err != nil {
			return out, fmt.Errorf("detect: marshal payload pool=%s: %w", p.Address, err)
		}
		sig.Payload = payload

		id, inserted, err := d.db.InsertSignal(ctx, sig)
		if err != nil {
			return out, fmt.Errorf("detect: insert signal pool=%s: %w", p.Address, err)
		}
		if !inserted {
			// Already recorded on (pool, type, metric, bucket_ts). The cooldown gate
			// usually prevents reaching here, but the unique key is the durable
			// guarantee against re-attesting a duplicate. Skip.
			log.Debug().
				Str("pool", p.Address).
				Str("metric", best.metric).
				Str("bucket_ts", latest.TsBucket.UTC().Format(time.RFC3339)).
				Msg("detect: anomaly already recorded, skipping")
			continue
		}
		sig.ID = id
		out = append(out, sig)

		log.Info().
			Str("pool", p.Address).
			Str("metric", best.metric).
			Float64("zscore", best.z).
			Int64("signal_id", id).
			Msg("detect: flow anomaly signal emitted")
	}

	return out, nil
}

// Replay walks every enabled pool's full bucket history and scores each bucket
// against its trailing baseline window, persisting a signal for each outlier
// that also clears cooldown. Unlike ScanOnce (which scores only the newest
// bucket), Replay surfaces the anomalies that already happened across the
// collected history, used after a backfill to seed a track record.
//
// Cooldown here is measured by bucket time (not wall clock): once a pool fires,
// later buckets within CooldownMin of that bucket are suppressed, exactly as the
// live detector would have throttled them in real time.
func (d *Detector) Replay(ctx context.Context) ([]store.Signal, error) {
	pools, err := d.db.ListEnabledPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect: replay list pools: %w", err)
	}

	warmup := d.cfg.BaselineBuckets / 4
	if warmup < 12 {
		warmup = 12
	}
	cooldown := time.Duration(d.cfg.CooldownMin) * time.Minute

	// Token0 market prices for the display-only flow burst size. Replay seeds
	// historical signals at the current price (an approximation, fine for a display
	// field). Best-effort, never a detection input.
	prices, perr := d.db.PoolPricesUSD(ctx)
	if perr != nil {
		log.Warn().Err(perr).Msg("detect: replay pool prices unavailable, flow size_usd stays nil")
		prices = nil
	}

	var out []store.Signal
	for _, p := range pools {
		// Full history ascending; pool bucket counts are bounded (one row per
		// active 5-minute window), so a large limit simply returns everything.
		buckets, err := d.db.GetRecentBuckets(ctx, p.Address, 1_000_000)
		if err != nil {
			return out, fmt.Errorf("detect: replay buckets pool=%s: %w", p.Address, err)
		}

		// Live and replay must use the same effective gate so replayed history
		// matches what the live loop would have fired. Bound into the scorer closure
		// and reused for the re-score below.
		zt := effectiveZThreshold(d.cfg, p)
		score := func(latest store.Bucket, baseline []store.Bucket) (metricResult, bool) {
			return d.scoreBucket(latest, baseline, zt)
		}

		// Pure pass: decide which bucket indices fire (windowing + cooldown).
		fired := selectReplay(buckets, d.cfg.BaselineBuckets, warmup, cooldown, score)

		for _, i := range fired {
			latest := buckets[i]

			// Reconstruct the same baseline window selectReplay used and re-score it
			// for the signal row. scoreBucket is pure over (latest, baseline, zt), so
			// this reproduces the exact metricResult that admitted the index.
			lo := 0
			if i-d.cfg.BaselineBuckets > 0 {
				lo = i - d.cfg.BaselineBuckets
			}
			baseline := buckets[lo:i]

			best, ok := d.scoreBucket(latest, baseline, zt)
			if !ok {
				// selectReplay only returns indices that scored ok, and scoreBucket
				// is deterministic, so a miss here means a logic bug. Fail loud
				// rather than emit a zero-value row.
				return out, fmt.Errorf("detect: replay rescore disagreed pool=%s bucket_ts=%s",
					p.Address, latest.TsBucket.UTC().Format(time.RFC3339))
			}

			sig := store.Signal{
				Pool:          p.Address,
				SignalType:    store.SignalTypeFlow,
				Metric:        best.metric,
				Zscore:        best.z,
				WindowBuckets: len(baseline),
				BucketTS:      latest.TsBucket,
				SizeUSD:       bucketSizeUSD(p, latest.Vol0, poolPrice(prices, p.Address)),
				Status:        store.StatusNew,
			}
			payload, err := json.Marshal(signalPayload{
				Latest:   best.latest,
				Median:   best.median,
				MAD:      best.mad,
				BucketTS: latest.TsBucket.UTC().Format(time.RFC3339),
			})
			if err != nil {
				return out, fmt.Errorf("detect: replay marshal payload pool=%s: %w", p.Address, err)
			}
			sig.Payload = payload

			id, inserted, err := d.db.InsertSignal(ctx, sig)
			if err != nil {
				return out, fmt.Errorf("detect: replay insert signal pool=%s: %w", p.Address, err)
			}
			if !inserted {
				// Already recorded by an earlier run (or earlier in this replay):
				// the (pool, type, metric, bucket_ts) unique key makes re-running
				// replay idempotent. Skip so the duplicate is not re-attested.
				log.Debug().
					Str("pool", p.Address).
					Str("metric", best.metric).
					Str("bucket_ts", latest.TsBucket.UTC().Format(time.RFC3339)).
					Msg("detect: replay anomaly already recorded, skipping")
				continue
			}
			sig.ID = id
			out = append(out, sig)

			log.Info().
				Str("pool", p.Address).
				Str("metric", best.metric).
				Float64("zscore", best.z).
				Str("bucket_ts", latest.TsBucket.UTC().Format(time.RFC3339)).
				Int64("signal_id", id).
				Msg("detect: replay anomaly emitted")
		}
	}

	return out, nil
}

// selectReplay is the pure selection core of Replay: given a pool's full
// ascending bucket history, the detector tunables, and a scorer, it returns the
// indices into buckets that should emit a signal, applying the trailing-baseline
// windowing, the warmup gate, and the bucket-time cooldown exactly as Replay
// does. It performs no I/O and does not mutate buckets, so it is unit-testable
// in isolation.
//
//   - baselineN: rolling window size; candidate i is scored against
//     buckets[max(0, i-baselineN):i].
//   - warmup: minimum baseline buckets before a candidate may fire (shorter -> skip).
//   - cooldown: bucket-time throttle; after a fire, candidates within cooldown of
//     the last fire are suppressed (non-positive disables it).
//   - score: the per-bucket scorer; its bool decides outlier-on-any-metric.
func selectReplay(
	buckets []store.Bucket,
	baselineN, warmup int,
	cooldown time.Duration,
	score func(store.Bucket, []store.Bucket) (metricResult, bool),
) []int {
	if len(buckets) <= warmup {
		return nil
	}

	var (
		fired    []int
		lastFire time.Time
		haveFire bool
	)
	// Start once at least `warmup` baseline buckets precede the candidate.
	for i := warmup; i < len(buckets); i++ {
		latest := buckets[i]

		lo := 0
		if i-baselineN > 0 {
			lo = i - baselineN
		}
		baseline := buckets[lo:i]
		if len(baseline) < warmup {
			continue
		}

		_, ok := score(latest, baseline)
		if !ok {
			continue
		}

		// Bucket-time cooldown: skip repeats within cooldown of the last fire.
		if haveFire && cooldown > 0 && latest.TsBucket.Sub(lastFire) < cooldown {
			continue
		}

		fired = append(fired, i)
		lastFire = latest.TsBucket
		haveFire = true
	}

	return fired
}

// effectiveZThreshold returns the z-threshold a pool is scored against: its
// positive per-pool override, else the global default. A nil/non-positive
// override falls back to the default (a non-positive gate fires on every bucket).
func effectiveZThreshold(cfg *config.Config, p store.Pool) float64 {
	if p.ZThreshold != nil && *p.ZThreshold > 0 {
		return *p.ZThreshold
	}
	return cfg.ZThreshold
}

// scoreBucket evaluates all metrics for one candidate bucket against its
// baseline and returns the metric with the largest absolute z-score that also
// clears zThreshold and its absolute floor (false when none triggers).
// zThreshold is passed in (not read from cfg) so live + replay share one value.
func (d *Detector) scoreBucket(latest store.Bucket, baseline []store.Bucket, zThreshold float64) (metricResult, bool) {
	var (
		best    metricResult
		bestAbs float64
		found   bool
	)

	for _, metric := range metricNames {
		latestVal := metricValue(latest, metric)
		if !isFinite(latestVal) {
			continue
		}

		sample := make([]float64, 0, len(baseline))
		for _, b := range baseline {
			v := metricValue(b, metric)
			if !isFinite(v) {
				continue
			}
			sample = append(sample, v)
		}
		if len(sample) == 0 {
			continue
		}

		median, mad := MedianAndMAD(sample)
		z := ModifiedZ(latestVal, median, mad)
		// Guard against NaN/Inf creeping in from degenerate inputs before any
		// comparison; ModifiedZ already returns 0 for non-positive/NaN mad,
		// but defend the boundary regardless.
		if !isFinite(z) {
			continue
		}

		az := math.Abs(z)
		if az < zThreshold {
			continue
		}
		if !absFloorOK(metric, latestVal, median) {
			continue
		}

		if !found || az > bestAbs {
			best = metricResult{
				metric: metric,
				z:      z,
				latest: latestVal,
				median: median,
				mad:    mad,
			}
			bestAbs = az
			found = true
		}
	}

	return best, found
}

// poolInCooldown reports whether the pool emitted a signal within the cooldown
// window. A non-positive cooldown disables the gate. Pools with no prior signal
// are never in cooldown.
func (d *Detector) poolInCooldown(ctx context.Context, pool string, cooldown time.Duration) (bool, error) {
	if cooldown <= 0 {
		return false, nil
	}
	last, ok, err := d.db.LastSignalTime(ctx, pool)
	if err != nil {
		return false, fmt.Errorf("detect: last signal time pool=%s: %w", pool, err)
	}
	if !ok {
		return false, nil
	}
	return time.Since(last) < cooldown, nil
}

// isFinite reports whether f is a usable real number (not NaN, not ±Inf).
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
