package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"ocr/internal/aggregate"
	"ocr/internal/config"
	"ocr/internal/store"
)

// The whale detector (signal_type 2, metric "whale_swap") flags an individual swap
// whose gross token0 size is a large outlier versus that pool's own recent per-swap
// size distribution. It works at single-transaction granularity (unlike the
// bucket-based flow detector), so a fired signal is keyed and attested by its event
// reference ("txhash:logindex"), not a bucket timestamp.
//
// The firing gate is an absolute multiple of the pool median (WhaleMinMultiple), not
// a MAD z-threshold. A pure z-threshold was empirically too loose: against ~10k real
// Mantle swaps it fired on 10-35% of them, because on pools whose swaps cluster
// tightly even a modest swap scores a large z. The z-score is still recorded for
// context but no longer decides firing. At most one whale (the largest qualifying
// swap) fires per pool per scan; a per-pool cooldown (CooldownMin) throttles repeats.
const (
	// whaleWarmupSwaps is the minimum usable swaps in the baseline before the
	// median is trustworthy; below it a pool is skipped (unstable robust stats).
	whaleWarmupSwaps = 30

	// whaleRecentWindowMin bounds the firing candidate set to the last N minutes
	// (the baseline still spans the full BaselineBuckets window). Older swaps were
	// evaluated on prior ticks; idempotency on event_ref makes the overlap harmless.
	whaleRecentWindowMin = 10
)

// WhaleDetector flags individual large-outlier swaps per pool against that pool's
// own recent per-swap size distribution.
type WhaleDetector struct {
	db  *store.DB
	cfg *config.Config
	// res resolves a fired whale's tx to its signing EOA (tx.origin) for
	// signals.actor (type 2). Best-effort + optional: a nil resolver or any resolve
	// failure leaves actor empty and the whale still fires. Not part of the hash.
	res SenderResolver
}

// NewWhaleDetector constructs a WhaleDetector. cfg supplies BaselineBuckets +
// BucketMin (baseline window), WhaleMinMultiple (the firing gate), and CooldownMin
// (per-pool cooldown). res (best-effort, may be nil) refines the whale's actor to
// tx.origin; *chain.Client satisfies it via TxSender.
func NewWhaleDetector(db *store.DB, cfg *config.Config, res SenderResolver) *WhaleDetector {
	return &WhaleDetector{db: db, cfg: cfg, res: res}
}

// whaleCandidate is one decoded swap eligible to fire. The pure selector reads
// only size (the decision input); the remaining fields carry through to the
// signal row and payload for swaps that do fire.
type whaleCandidate struct {
	size      float64         // gross token0 size: the value scored against the baseline
	size1     float64         // gross token1 size: recorded in the payload only
	size0Dec  decimal.Decimal // exact gross token0 amount (raw units) for the display-only size_usd
	size1Dec  decimal.Decimal // exact gross token1 amount (raw units) for the display-only size_usd
	tx        string          // lowercase tx hash
	logIndex  int             // log index within the tx
	blockTime time.Time       // event time (UTC); the signal's BucketTS / hash timestamp
}

// whalePayload is the JSON written to a whale signal's Payload: the firing swap's
// magnitude plus the baseline it was scored against and the originating event, so
// the finding is auditable and the event-keyed attestation hash is reproducible.
type whalePayload struct {
	Size0     float64 `json:"size0"`
	Size1     float64 `json:"size1"`
	Median    float64 `json:"median"`
	MAD       float64 `json:"mad"`
	Multiple  float64 `json:"multiple"` // size0 / median (how far past the gate)
	Tx        string  `json:"tx"`
	LogIndex  int     `json:"log_index"`
	BlockTime string  `json:"block_time"`
}

// ScanOnce evaluates every enabled pool once and persists at most one whale
// signal per pool: the single largest recent swap that clears the
// multiple-of-median gate. It returns the signals it inserted (with their
// generated ids) so the caller can hand them straight to the enrich/attest/
// deliver pipeline.
//
// Per pool: pull the last BaselineBuckets*BucketMin minutes of swap logs, decode
// each swap's gross token0 size, build the size baseline, then pick the largest
// swap in the last whaleRecentWindowMin minutes whose size is >=
// WhaleMinMultiple x the pool's median swap size. Pools with fewer than
// whaleWarmupSwaps usable swaps are skipped (unstable baseline).
//
// Two gates suppress repeats: a per-pool type-2 cooldown (CooldownMin) and
// idempotent emission on (pool, signal_type, event_ref). A per-pool failure
// returns the already-collected signals plus the wrapped error.
func (w *WhaleDetector) ScanOnce(ctx context.Context) ([]store.Signal, error) {
	return w.ScanOnceExcept(ctx, nil)
}

// ScanOnceExcept is ScanOnce with a same-swap dedup set: a candidate whose
// event_ref is in suppress is skipped, so a swap a richer smart-money (type 3)
// signal already covers does not also fire a whale (type 2). It drops only that
// exact event_ref (pickWhale falls through to the next-largest qualifying swap);
// a nil/empty set is identical to ScanOnce. suppress is a bare string set, so the
// two detectors stay decoupled.
func (w *WhaleDetector) ScanOnceExcept(ctx context.Context, suppress map[string]struct{}) ([]store.Signal, error) {
	pools, err := w.db.ListEnabledPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect: whale list enabled pools: %w", err)
	}

	window := time.Duration(w.cfg.BaselineBuckets*w.cfg.BucketMin) * time.Minute
	now := time.Now().UTC()
	since := now.Add(-window)
	recentCut := now.Add(-whaleRecentWindowMin * time.Minute)

	// Per-scan tx.origin cache, shared across pools so a whale on the same tx in
	// two pools resolves once. Bounded by the fired whales in one scan.
	originCache := make(map[string]common.Address)

	// Token0 market prices for the display-only size_usd on non-stable pools, loaded
	// once. Best-effort: an unavailable snapshot falls back to the stable-only rule
	// (nil for non-stable). Never a detection input.
	prices, perr := w.db.PoolPricesUSD(ctx)
	if perr != nil {
		log.Warn().Err(perr).Msg("detect: whale pool prices unavailable, sizes fall back to stable-only")
		prices = nil
	}

	var out []store.Signal
	for _, p := range pools {
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("detect: whale context cancelled: %w", err)
		}

		logs, err := w.db.RawLogsSince(ctx, since, []string{p.Address})
		if err != nil {
			return out, fmt.Errorf("detect: whale raw logs pool=%s: %w", p.Address, err)
		}

		// Decode usable swaps into the baseline sample and the recent candidate set in
		// one pass. A swap that fails to decode or has size<=0 is left out of the sample
		// (not an error; the aggregator treats those as count-only too).
		var (
			sizes      []float64
			candidates []whaleCandidate
			skipped    int
		)
		for _, l := range logs {
			if !aggregate.IsSwapTopic(l.Topic0) {
				continue
			}
			d, derr := aggregate.DecodeSwap(l.Topic0, l.Data)
			if derr != nil {
				skipped++
				continue
			}
			size := d.Vol0.InexactFloat64()
			if size <= 0 {
				skipped++
				continue
			}
			sizes = append(sizes, size)
			if !l.BlockTime.Before(recentCut) {
				tx := strings.ToLower(l.TxHash)
				// Same-swap dedup: a swap a type-3 signal already covers this scan
				// still counts toward the baseline but is dropped from the firing set
				// (pickWhale falls through to the next-largest swap).
				if _, dup := suppress[tx+":"+strconv.Itoa(l.LogIndex)]; dup {
					continue
				}
				candidates = append(candidates, whaleCandidate{
					size:      size,
					size1:     d.Vol1.InexactFloat64(),
					size0Dec:  d.Vol0, // exact raw units for the display-only size_usd (float is lossy)
					size1Dec:  d.Vol1,
					tx:        tx,
					logIndex:  l.LogIndex,
					blockTime: l.BlockTime.UTC(),
				})
			}
		}

		// Unstable baseline: too few usable swaps for a trustworthy median.
		if len(sizes) < whaleWarmupSwaps {
			log.Debug().
				Str("pool", p.Address).
				Int("usable_swaps", len(sizes)).
				Int("warmup", whaleWarmupSwaps).
				Int("skipped", skipped).
				Msg("detect: whale pool below warmup, skipping")
			continue
		}

		// Per-pool type-2 cooldown: suppress a pool that fired recently. Done before
		// picking so a cooling pool costs one cheap lookup; non-positive disables it.
		cooldown := time.Duration(w.cfg.CooldownMin) * time.Minute
		if cooldown > 0 {
			last, ok, lerr := w.db.LastSignalTimeByType(ctx, p.Address, store.SignalTypeWhale)
			if lerr != nil {
				return out, fmt.Errorf("detect: whale last signal time pool=%s: %w", p.Address, lerr)
			}
			if ok && time.Since(last) < cooldown {
				log.Debug().
					Str("pool", p.Address).
					Dur("cooldown", cooldown).
					Msg("detect: whale pool in cooldown, skipping")
				continue
			}
		}

		// Median/MAD for the row + payload (pickWhale computes its own median for
		// the gate). MAD is recorded for the z-score even though it no longer gates.
		median, mad := MedianAndMAD(sizes)

		// Per-pool whale gate: an override changes which swaps fire, never a
		// signal's hash (keyed on pool/type/event_ref, not the gate).
		wm := effectiveWhaleMinMultiple(w.cfg, p)

		// Pure decision: the single largest recent swap clearing the
		// multiple-of-median gate, or nothing.
		idx, ok := pickWhale(sizes, candidates, wm)
		if !ok {
			continue
		}

		c := candidates[idx]
		z := ModifiedZ(c.size, median, mad)
		multiple := c.size / median

		// Resolve tx.origin for signals.actor (best-effort; see resolveWhaleActor):
		// a nil resolver / RPC error / timeout leaves actor empty and the whale fires.
		actor := resolveWhaleActor(ctx, w.res, originCache, c.tx)

		// Display-only USD size from the exact decoded amounts (not the lossy floats),
		// so the float rounding never reaches the size field.
		sizeUSD := swapSizeUSD(p, c.size0Dec, c.size1Dec, poolPrice(prices, p.Address))

		eventRef := c.tx + ":" + strconv.Itoa(c.logIndex)
		sig := store.Signal{
			Pool:          p.Address,
			SignalType:    store.SignalTypeWhale,
			Metric:        "whale_swap",
			Zscore:        z,
			WindowBuckets: len(sizes), // reuse the column to record the sample size
			BucketTS:      c.blockTime,
			EventRef:      eventRef,
			Actor:         actor,   // best-effort tx.origin; "" -> NULL (display-only attribution)
			SizeUSD:       sizeUSD, // display-only; nil when no stable side
			Status:        store.StatusNew,
		}

		payload, perr := json.Marshal(whalePayload{
			Size0:     c.size,
			Size1:     c.size1,
			Median:    median,
			MAD:       mad,
			Multiple:  multiple,
			Tx:        c.tx,
			LogIndex:  c.logIndex,
			BlockTime: c.blockTime.Format(time.RFC3339),
		})
		if perr != nil {
			return out, fmt.Errorf("detect: whale marshal payload pool=%s ref=%s: %w", p.Address, eventRef, perr)
		}
		sig.Payload = payload

		id, inserted, ierr := w.db.InsertSignal(ctx, sig)
		if ierr != nil {
			return out, fmt.Errorf("detect: whale insert signal pool=%s ref=%s: %w", p.Address, eventRef, ierr)
		}
		if !inserted {
			// Recorded on an earlier overlapping tick: the (pool, signal_type,
			// event_ref) key makes re-scanning idempotent. Skip so it is not re-attested.
			log.Debug().
				Str("pool", p.Address).
				Str("event_ref", eventRef).
				Msg("detect: whale signal already recorded, skipping")
			continue
		}
		sig.ID = id
		out = append(out, sig)

		log.Info().
			Str("pool", p.Address).
			Float64("size", c.size).
			Float64("zscore", z).
			Float64("multiple", multiple).
			Str("event_ref", eventRef).
			Int64("signal_id", id).
			Msg("detect: whale signal emitted")
	}

	return out, nil
}

// effectiveWhaleMinMultiple returns the multiple-of-median gate a pool's swaps
// are scored against: its positive per-pool override, else the global default
// (a nil/non-positive override falls back to the default).
func effectiveWhaleMinMultiple(cfg *config.Config, p store.Pool) float64 {
	if p.WhaleMinMultiple != nil && *p.WhaleMinMultiple > 0 {
		return *p.WhaleMinMultiple
	}
	return cfg.WhaleMinMultiple
}

// pickWhale is the pure selection core of ScanOnce: it returns the index of the
// single largest candidate that qualifies (and true), or (-1, false) when none do.
// No I/O, no mutation, so it is unit-testable in isolation.
//
// A candidate qualifies iff:
//   - the baseline has at least whaleWarmupSwaps samples (else nothing fires), and
//   - the candidate size is positive, and
//   - size >= minMultiple * median (the absolute multiple-of-median gate).
//
// The gate is independent of MAD: a zero-spread baseline (MAD == 0, all swaps
// equal) still has a well-defined positive median (every sample is a positive
// size), so a large swap fires and there is no divide-by-zero.
func pickWhale(sizes []float64, candidates []whaleCandidate, minMultiple float64) (int, bool) {
	if len(sizes) < whaleWarmupSwaps {
		return -1, false
	}

	median, _ := MedianAndMAD(sizes)
	threshold := minMultiple * median

	bestIdx := -1
	var bestSize float64
	for i, c := range candidates {
		if c.size <= 0 || c.size < threshold {
			continue
		}
		if bestIdx == -1 || c.size > bestSize {
			bestIdx = i
			bestSize = c.size
		}
	}
	if bestIdx == -1 {
		return -1, false
	}
	return bestIdx, true
}

// resolveWhaleActor resolves a fired whale's tx to its signing EOA (tx.origin),
// returning the lowercase hex address or "" when attribution is unavailable
// (nil resolver, or a resolve failure logged inside resolveActorCached). It is
// best-effort: it never blocks or drops a whale. "" maps to a NULL signals.actor.
// Factored out so the nil-resolver / skip-on-error paths are unit-testable.
func resolveWhaleActor(ctx context.Context, res SenderResolver, cache map[string]common.Address, tx string) string {
	if res == nil {
		return ""
	}
	addr, ok := resolveActorCached(ctx, res, cache, tx)
	if !ok {
		return ""
	}
	return strings.ToLower(addr.Hex())
}
