package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"ocr/internal/aggregate"
	"ocr/internal/attest"
	"ocr/internal/config"
	"ocr/internal/store"
)

// The depeg detector (signal_type 7, metric "depeg") detects a stablecoin depeg on
// Mantle purely from our own decoded swap data (no external price feed): in a
// stable/stable pool, when the median implied price of recent swaps has drifted off
// the 1.0 peg by at least DepegThresholdBps, the cheaper side is the depegging token
// and the signal names the at-risk pools (every other enabled pool holding it).
//
// Unlike the per-event detectors, a depeg is a condition over a recent window (like
// the flow anomaly), so it uses the bucket path: idempotent on (pool, signal_type=7,
// metric, bucket_ts) via uq_signals_anomaly, with a per-token type-7 cooldown so a
// sustained depeg does not re-fire. The subject (and dedup/cooldown key) is the
// depegging token address (the signal's Pool field), so one token's depeg is one
// story across several pools. The implied price is derived from our decoded swap
// amounts alone (the derivation is on impliedPrice); the median over the window keeps
// a single large-slippage swap from moving the reading.
const (
	// signalTypeDepeg is the int16-typed local alias of store.SignalTypeDepeg
	// (1/2/3 flow/whale/smart_money; 4 LST flow; 5/6 lending; 7 depeg).
	signalTypeDepeg int16 = store.SignalTypeDepeg

	// metricDepeg is the metric string for the family, also part of the bucket-keyed
	// attestation hash.
	metricDepeg = "depeg"

	// peg is the on-peg implied price for a stable/stable pair (1.0); deviation is
	// |peg - impliedPrice|.
	peg = 1.0

	// bpsPerUnit converts a fractional deviation to basis points (1 = 100 bps = 1%).
	bpsPerUnit = 10000.0

	// depegScoreBase is the base of the depeg pseudo-score, with a log ramp on the
	// deviation in bps on top (a depeg has no z-score). See scoreDepeg.
	depegScoreBase = 3.0

	// depegScoreCapZ caps the pseudo-score at the attestor's uint16 ceiling
	// (attest.MaxScoreZ). A depeg deviation never approaches this; the cap is purely
	// defensive.
	depegScoreCapZ = attest.MaxScoreZ
)

// DepegDetector emits depeg (type 7) signals from our swap data on the registry's
// stable/stable pools. It holds the store and config and needs no chain client or
// external price source.
type DepegDetector struct {
	db  *store.DB
	cfg *config.Config
}

// NewDepegDetector constructs a DepegDetector over the given store and config. cfg
// supplies DepegThresholdBps (the depeg trigger, in bps off peg), DepegMinSwaps (the
// minimum number of recent swaps before the implied price is trusted), and
// DepegCooldownMin (the per-token type-7 cooldown).
func NewDepegDetector(db *store.DB, cfg *config.Config) *DepegDetector {
	return &DepegDetector{db: db, cfg: cfg}
}

// depegRecentWindowMin bounds the candidate swaps to the last N minutes: a depeg is
// a current market condition, and the per-token cooldown + bucket-ts idempotency make
// the overlap across ticks harmless.
const depegRecentWindowMin = 15

// depegStore is the minimal store surface ScanOnce needs: the enabled-pool registry
// (stable/stable pools + contagion map), the bounded per-pool recent-log read, the
// idempotent signal insert, and the per-token type-7 cooldown lookup. A mock lets the
// firing + math + contagion be tested with no Postgres.
type depegStore interface {
	ListEnabledPools(ctx context.Context) ([]store.Pool, error)
	RawLogsSince(ctx context.Context, since time.Time, pools []string) ([]store.RawLog, error)
	InsertSignal(ctx context.Context, s store.Signal) (int64, bool, error)
	LastSignalTimeByType(ctx context.Context, pool string, signalType int16) (time.Time, bool, error)
}

// affectedPool is one at-risk pool in a depeg's contagion map: a tracked pool holding
// the depegging token. Address + label so the payload links out without a second lookup.
type affectedPool struct {
	Address string `json:"address"`
	Label   string `json:"label"`
}

// depegPayload is the JSON written to a depeg signal's Payload: the depegging token,
// the reading pool, the median implied price, the deviation, and the contagion map, so
// the finding is auditable and the bucket-keyed hash is reproducible.
type depegPayload struct {
	Token         string         `json:"token"`          // the depegging token contract address (subject)
	TokenSymbol   string         `json:"token_symbol"`   // "USDe" | "USDC" | "USDT" (from the registry pool label)
	PoolLabel     string         `json:"pool_label"`     // label of the stable/stable pool the reading came from
	ImpliedPrice  string         `json:"implied_price"`  // median implied price token0-in-token1 (decimal string)
	DeviationBps  int            `json:"deviation_bps"`  // |1 - impliedPrice| * 10000, rounded
	UnderPeg      bool           `json:"under_peg"`      // true: the token traded below $1 (always true when we fire)
	SwapCount     int            `json:"swap_count"`     // swaps in the window that produced the median
	AffectedPools []affectedPool `json:"affected_pools"` // other tracked pools holding the token (contagion)
	BucketTS      string         `json:"bucket_ts"`      // window start (RFC3339, UTC)
}

// ScanOnce evaluates every enabled stable/stable pool once and persists a depeg
// (type 7) signal for each token whose recent median implied price has drifted off the
// 1.0 peg by at least DepegThresholdBps, that is not in per-token cooldown. It returns
// the inserted signals (with their generated ids) so the caller can hand them straight
// to the enrich/attest/deliver pipeline, exactly like the other detectors.
//
// Per stable/stable pool it computes the median implied price over the recent
// window and, when off peg by >= DepegThresholdBps, builds the contagion map and
// emits (see evaluatePool). Emission is idempotent on (pool=token, signal_type,
// metric, bucket_ts) with a per-token type-7 cooldown. A decode failure on one log
// is skipped; only ctx cancellation or a real DB error returns an error (with the
// already-collected signals).
func (d *DepegDetector) ScanOnce(ctx context.Context) ([]store.Signal, error) {
	return d.scanOnce(ctx, d.db)
}

// scanOnce is ScanOnce's testable core: it takes the store as a narrow interface
// (depegStore) so the firing + math + contagion + idempotency path can be exercised
// with a mock and no Postgres. The public ScanOnce passes the real *store.DB.
func (d *DepegDetector) scanOnce(ctx context.Context, st depegStore) ([]store.Signal, error) {
	pools, err := st.ListEnabledPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect: depeg list enabled pools: %w", err)
	}

	now := time.Now().UTC()
	recentCut := now.Add(-depegRecentWindowMin * time.Minute)
	// bucketTS anchors idempotency + the hash to the window start (floored to
	// BucketMin), not wall-clock now: re-scanning a sustained depeg within a window
	// dedups on the same bucket_ts instead of minting a row each tick. (The per-token
	// cooldown is the primary throttle; this is the durable backstop.)
	bucketTS := floorToBucketMin(now, d.cfg.BucketMin)
	cooldown := time.Duration(d.cfg.DepegCooldownMin) * time.Minute

	var out []store.Signal
	for i := range pools {
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("detect: depeg context cancelled: %w", err)
		}
		p := pools[i]

		// Only stable/stable pools (isStableToken, from size.go): a pool with a
		// non-stable side (e.g. WMNT) has no meaningful peg and is skipped.
		if !isStableToken(p.Token0) || !isStableToken(p.Token1) {
			continue
		}

		sig, ok, ferr := d.evaluatePool(ctx, st, p, pools, recentCut, bucketTS, cooldown)
		if ferr != nil {
			return out, ferr
		}
		if !ok {
			continue
		}

		id, inserted, ierr := st.InsertSignal(ctx, sig)
		if ierr != nil {
			return out, fmt.Errorf("detect: depeg insert signal token=%s: %w", sig.Pool, ierr)
		}
		if !inserted {
			// Already recorded for this token+window: the (pool=token, signal_type,
			// metric, bucket_ts) key makes re-scanning idempotent. Skip.
			log.Debug().
				Str("token", sig.Pool).
				Str("bucket_ts", bucketTS.Format(time.RFC3339)).
				Msg("detect: depeg signal already recorded, skipping")
			continue
		}
		sig.ID = id
		out = append(out, sig)

		log.Info().
			Str("token", sig.Pool).
			Str("pool", strings.ToLower(p.Address)).
			Float64("score_input", sig.Zscore).
			Str("bucket_ts", bucketTS.Format(time.RFC3339)).
			Int64("signal_id", id).
			Msg("detect: depeg signal emitted")
	}

	return out, nil
}

// evaluatePool reads one stable/stable pool's recent swaps, computes the median
// implied price, and (when off peg by >= DepegThresholdBps and the token is not in
// cooldown) builds the type-7 signal (subject = the depegging token, bucket path).
// It returns ok=false + nil error for every non-fatal non-firing reason (thin data,
// on-peg, cooldown, marshal failure); a non-nil error only when the scan must abort
// (the raw-log read or the cooldown-lookup DB call failed).
func (d *DepegDetector) evaluatePool(
	ctx context.Context,
	st depegStore,
	p store.Pool,
	allPools []store.Pool,
	recentCut, bucketTS time.Time,
	cooldown time.Duration,
) (store.Signal, bool, error) {
	logs, err := st.RawLogsSince(ctx, recentCut, []string{strings.ToLower(p.Address)})
	if err != nil {
		return store.Signal{}, false, fmt.Errorf("detect: depeg raw logs pool=%s: %w", strings.ToLower(p.Address), err)
	}

	prices := impliedPrices(logs, p)
	// Thin-data guard: a price from too few swaps is noise. Require >= DepegMinSwaps.
	if len(prices) < d.cfg.DepegMinSwaps {
		log.Debug().
			Str("pool", strings.ToLower(p.Address)).
			Int("usable_swaps", len(prices)).
			Int("min_swaps", d.cfg.DepegMinSwaps).
			Msg("detect: depeg thin data, skipping pool")
		return store.Signal{}, false, nil
	}

	median := medianOf(prices)
	deviation := peg - median
	if deviation < 0 {
		deviation = -deviation
	}
	deviationBps := deviation * bpsPerUnit
	if deviationBps < float64(d.cfg.DepegThresholdBps) {
		// On peg (within threshold): no depeg.
		return store.Signal{}, false, nil
	}

	// The depegging token is the side trading below $1. impliedPrice is token0-in-token1
	// (a1_whole/a0_whole = P0/P1): < 1 means token0 is cheaper (under peg); > 1 means
	// token1 is cheaper. (We only fire when off peg, so median != 1 here.) Note the bps
	// is the deviation of the ratio from 1.0, so a token1 depeg of x% reads as ~(1/(1-x)
	// - 1) rather than x (a second-order difference: a 3% token1 depeg reads ~309 bps vs
	// 300); this is immaterial at the threshold scale and keeps the math single-ratio.
	var (
		token    string
		symbol   string
		underPeg = true // we only emit for a token trading below $1
	)
	if median < peg {
		token = strings.ToLower(p.Token0)
		symbol = poolLabelSideSymbol(p.Label, 0)
	} else {
		token = strings.ToLower(p.Token1)
		symbol = poolLabelSideSymbol(p.Label, 1)
	}

	// Per-token type-7 cooldown: suppress a repeat for the same token (keyed on the
	// token, our subject) within the window; non-positive disables it.
	if cd, derr := d.tokenInCooldown(ctx, st, token, cooldown); derr != nil {
		return store.Signal{}, false, derr
	} else if cd {
		log.Debug().
			Str("token", token).
			Dur("cooldown", cooldown).
			Msg("detect: depeg token in cooldown, skipping")
		return store.Signal{}, false, nil
	}

	// Contagion: every other enabled pool that holds the depegging token (by token0 or
	// token1 address) is at risk. Built from the same enabled-pool set we already have
	// (no extra query), excluding the pool the reading came from.
	affected := contagionPools(token, strings.ToLower(p.Address), allPools)

	medianDec := decimal.NewFromFloat(median)
	payload, perr := json.Marshal(depegPayload{
		Token:         token,
		TokenSymbol:   symbol,
		PoolLabel:     p.Label,
		ImpliedPrice:  medianDec.String(),
		DeviationBps:  int(deviationBps + 0.5),
		UnderPeg:      underPeg,
		SwapCount:     len(prices),
		AffectedPools: affected,
		BucketTS:      bucketTS.Format(time.RFC3339),
	})
	if perr != nil {
		// Non-fatal: log + skip this pool rather than abort the scan.
		log.Error().Err(perr).Str("token", token).Msg("detect: depeg marshal payload; skipping pool")
		return store.Signal{}, false, nil
	}

	return store.Signal{
		// Subject = the depegging token (attested as on-chain `subject`, and the dedup
		// + cooldown key); the reading pool + contagion map are in the payload.
		Pool:          token,
		SignalType:    signalTypeDepeg,
		Metric:        metricDepeg,
		Zscore:        scoreDepeg(deviationBps), // pseudo-score; ScoreFromZ maps it onto the uint16 on-chain score
		WindowBuckets: 0,                        // not a per-bucket-baseline signal
		BucketTS:      bucketTS,                 // bucket path: idempotent on (token, type, metric, bucket_ts)
		EventRef:      "",                       // bucket signal (not event-keyed) -> SignalHash, not SignalHashEvent
		Actor:         "",                       // market condition, not an EOA -> NULL actor
		SizeUSD:       nil,                      // a price condition has no notional size -> NULL
		Payload:       payload,
		Status:        store.StatusNew,
	}, true, nil
}

// impliedPrices decodes a stable/stable pool's recent swap logs into per-swap implied
// prices (token0 in token1 = a1_whole / a0_whole) using the pool's decimals. Pure
// (no I/O). A log that is not a swap, fails to decode, or has a zero amount on either
// side is skipped (never a divide-by-zero). One entry per usable swap; the caller
// gates on DepegMinSwaps before trusting the median.
func impliedPrices(logs []store.RawLog, p store.Pool) []float64 {
	prices := make([]float64, 0, len(logs))
	for i := range logs {
		l := logs[i]
		if !aggregate.IsSwapTopic(l.Topic0) {
			continue
		}
		amt, err := aggregate.DecodeSwap(l.Topic0, l.Data)
		if err != nil {
			// Undecodable swap: no usable amounts, so not a price sample. Skip.
			continue
		}
		price, ok := impliedPrice(amt.Vol0, amt.Vol1, p.Dec0, p.Dec1)
		if !ok {
			continue
		}
		prices = append(prices, price)
	}
	return prices
}

// impliedPrice computes one swap's implied price of token0 denominated in token1 from
// the swap's absolute per-token raw amounts (gross0/gross1, e.g.
// aggregate.SwapAmounts.Vol0/Vol1, already non-negative) and the pool's authoritative
// decimals. It returns ok=false when either whole-token amount is non-positive (a
// one-sided or zero-amount swap has no usable ratio), so the caller never divides by
// zero and never records a degenerate price.
//
// Formula (price of token0 in token1):
//
//	a0_whole = |gross0| / 10^dec0
//	a1_whole = |gross1| / 10^dec1
//	impliedPrice = a1_whole / a0_whole
//
// A swap conserves USD value (a0_whole*P0 = a1_whole*P1), so a1_whole/a0_whole =
// P0/P1, which is near 1.0 at peg for a stable/stable pair; below 1.0 means token0
// trades under a dollar. The 10^dec scaling makes the ratio decimals-correct for
// asymmetric pairs (e.g. 6-dec USDC vs 18-dec USDe); without it the raw-unit ratio
// would be off by 10^(dec1-dec0).
func impliedPrice(gross0, gross1 decimal.Decimal, dec0, dec1 int) (float64, bool) {
	a0 := gross0.Abs().Shift(int32(-dec0))
	a1 := gross1.Abs().Shift(int32(-dec1))
	if !a0.IsPositive() || !a1.IsPositive() {
		// One-sided or zero-amount swap: no usable price (and no divide-by-zero).
		return 0, false
	}
	price := a1.Div(a0).InexactFloat64()
	if !isFinite(price) || price <= 0 {
		return 0, false
	}
	return price, true
}

// contagionPools builds a depeg's contagion map: every enabled pool other than the
// reading pool (excludeAddr) that holds the depegging token (token0 or token1,
// case-insensitive). Pure (operates on the already-fetched pool set). Returned in
// registry order for deterministic payloads.
func contagionPools(token, excludeAddr string, pools []store.Pool) []affectedPool {
	token = strings.ToLower(strings.TrimSpace(token))
	var out []affectedPool
	for i := range pools {
		p := pools[i]
		addr := strings.ToLower(p.Address)
		if addr == strings.ToLower(excludeAddr) {
			continue // the pool the depeg was measured in is not "contagion"
		}
		if strings.ToLower(p.Token0) == token || strings.ToLower(p.Token1) == token {
			out = append(out, affectedPool{Address: addr, Label: p.Label})
		}
	}
	return out
}

// tokenInCooldown reports whether a depeg (type 7) for this token fired within the
// cooldown window. Like poolInCooldown but scoped to the depeg family and keyed on the
// token, so one token's sustained depeg does not block another's. A non-positive
// cooldown disables the gate; a token with no prior depeg is never in cooldown.
func (d *DepegDetector) tokenInCooldown(ctx context.Context, st depegStore, token string, cooldown time.Duration) (bool, error) {
	if cooldown <= 0 {
		return false, nil
	}
	last, ok, err := st.LastSignalTimeByType(ctx, token, signalTypeDepeg)
	if err != nil {
		return false, fmt.Errorf("detect: depeg last signal time token=%s: %w", token, err)
	}
	if !ok {
		return false, nil
	}
	return time.Since(last) < cooldown, nil
}

// scoreDepeg maps a depeg's deviation (in bps off peg) to a pseudo-score that the
// attestor's ScoreFromZ turns into the uint16 on-chain score. A depeg has no z-score,
// so like scoreLendingUSD / scoreLSTFlow the score communicates magnitude (how far off
// peg): a base plus a log10 ramp on the bps, so a 50-bps wobble and a 1000-bps break
// are distinguishable on-chain:
//
//	deviationBps <= 1 (degenerate; gated out by the threshold anyway) -> depegScoreBase
//	otherwise                                                         -> depegScoreBase + log10(deviationBps)
//
// e.g. 50 bps -> ~3 + 1.70 = 4.70 (on-chain score 470); 1000 bps (10%) -> ~3 + 3.0 =
// 6.0 (600). Clamped to [depegScoreBase, depegScoreCapZ] so it always yields a valid,
// non-saturating uint16 after the *100 scale.
func scoreDepeg(deviationBps float64) float64 {
	if deviationBps <= 1 || !isFinite(deviationBps) {
		return depegScoreBase
	}
	score := depegScoreBase + math.Log10(deviationBps)
	if score < depegScoreBase {
		return depegScoreBase
	}
	if score > depegScoreCapZ {
		return depegScoreCapZ
	}
	return score
}

// medianOf returns the median of an unordered price slice, sorting a copy and reusing
// the package's Median (zscore.go) so depeg shares the MAD baseline's median. An empty
// slice yields NaN (the caller gates on DepegMinSwaps first).
func medianOf(xs []float64) float64 {
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	return Median(cp)
}

// floorToBucketMin truncates t (UTC) to the bucketMin-minute grid, the same grid as
// aggregate.floorToBucket, so a depeg's bucket_ts aligns with the swap buckets.
// bucketMin is config-validated > 0 (Truncate(0) would be a harmless no-op).
func floorToBucketMin(t time.Time, bucketMin int) time.Time {
	return t.UTC().Truncate(time.Duration(bucketMin) * time.Minute)
}

// poolLabelSideSymbol returns side i (0 or 1) of a "TOKEN0/TOKEN1" pool label as a
// display symbol, or "" when the label is missing or not two-part (splits on the first
// "/" only). Display-only; the token address is the authoritative identity.
func poolLabelSideSymbol(label string, i int) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}
	slash := strings.IndexByte(label, '/')
	if slash < 0 {
		return ""
	}
	if i == 0 {
		return strings.TrimSpace(label[:slash])
	}
	return strings.TrimSpace(label[slash+1:])
}
