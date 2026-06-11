// Package aggregate rolls raw_logs up into fixed-width time buckets per pool: read each
// enabled pool's raw swap logs, group them into bucketMin-minute windows (start =
// block_time floored to the interval), and write one buckets row per (pool, window) via
// store.UpsertBucket.
//
// Per bucket: swap_count, gross volume per token (vol0/vol1), and signed net flow per
// token (netflow0/netflow1). Amounts stay in raw on-chain units (no decimals, no USD; a
// downstream stage handles that), via shopspring/decimal so the full NUMERIC(78)
// precision survives the round-trip to Postgres.
//
// Sign convention (uniform across variants, see DecodeSwap for the per-variant word
// layouts): vol contribution = in + out; netflow contribution = out - in, positive when
// the token leaves the pool toward the trader. A log whose topic0 is a known Swap topic
// but whose data cannot be decoded (short/odd payload) still counts toward swap_count
// with a zero volume/netflow contribution: a swap is never dropped for unparseable words.
package aggregate

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"ocr/internal/store"
)

// Known Swap event topic0 hashes (keccak256 of the event signature string, not a
// contract address). Exported so other stages (e.g. the whale detector) classify swap
// logs without re-deriving the signatures.
const (
	// TopicUniV2Swap is UniV2 Swap(address,uint256,uint256,uint256,uint256,address):
	// data = amount0In, amount1In, amount0Out, amount1Out (4 x uint256).
	TopicUniV2Swap = "0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822"
	// TopicUniV3Swap is UniV3 Swap(address,address,int256,int256,uint160,uint128,int24):
	// data = amount0, amount1, sqrtPriceX96, liquidity, tick. We read the first
	// two int256 words only.
	TopicUniV3Swap = "0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67"

	// TopicAgniSwap is Agni Finance (Uniswap-V3 fork on Mantle) Swap, verified via
	// openchain: Swap(address,address,int256,int256,uint160,uint128,int24,uint128,uint128).
	// Leading words are amount0, amount1 (int256) exactly as UniV3, so it shares the V3
	// decoder; trailing protocol-fee words are ignored.
	TopicAgniSwap = "0x19b47279256b2a23a1665c810c8d55a1758940ee09377d4f8d26497a3577dc83"

	// TopicMoeLBSwap is Merchant Moe / Trader Joe Liquidity Book v2.1 Swap.
	// Verified via openchain:
	// Swap(address,address,uint24,bytes32,bytes32,uint24,bytes32,bytes32).
	// data words: [id, amountsIn, amountsOut, volatilityAccumulator, totalFees,
	// protocolFees]. amountsIn/Out are packed uint128 pairs: token X (token0) in
	// the low 128 bits, token Y (token1) in the high 128 bits.
	TopicMoeLBSwap = "0xad7d6f97abf51ce18e17a38f4d70e975be9c0708474987bb3e26ad21bd93ca70"
)

// evmWordBytes is the size of one ABI-encoded 32-byte word.
const evmWordBytes = 32

// Aggregator turns raw_logs into per-pool time buckets.
type Aggregator struct {
	db        *store.DB
	bucketMin int
	// retention is how far back raw_logs are kept before the Run loop prunes them
	// (<= 0 disables pruning; RunOnce never prunes). Set comfortably behind the
	// baseline window so no live aggregation can still need a deleted row.
	retention time.Duration
}

// New builds an Aggregator writing bucketMin-minute buckets and retaining raw_logs
// for `retention` before the continuous Run loop prunes the aged tail. bucketMin
// must be a positive number of minutes (typically 5). A non-positive retention
// disables raw_logs pruning.
func New(db *store.DB, bucketMin int, retention time.Duration) *Aggregator {
	return &Aggregator{db: db, bucketMin: bucketMin, retention: retention}
}

// RunOnce processes every enabled pool once: for each pool it reads the raw logs
// at or after the pool's last bucket, groups them into bucketMin-minute windows,
// and upserts one bucket per window. It is idempotent: re-running over the same
// logs reproduces identical buckets (UpsertBucket replaces by (pool, ts_bucket)).
//
// The boundary bucket (the one containing LastBucketTime) is deliberately
// recomputed each run so late-arriving logs for an in-progress window are folded
// in rather than lost.
func (a *Aggregator) RunOnce(ctx context.Context) error {
	if a.bucketMin <= 0 {
		return fmt.Errorf("aggregate: invalid bucketMin %d (must be > 0)", a.bucketMin)
	}

	pools, err := a.db.ListEnabledPools(ctx)
	if err != nil {
		return fmt.Errorf("aggregate: list enabled pools: %w", err)
	}

	var firstErr error
	for _, p := range pools {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("aggregate: context cancelled: %w", err)
		}
		if err := a.processPool(ctx, p.Address); err != nil {
			// Do not abort the whole sweep on a single pool failure; log, keep
			// the first error to return, and continue with the rest.
			log.Error().Err(err).Str("pool", p.Address).Msg("aggregate: pool failed")
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// processPool aggregates one pool from its last bucket forward.
func (a *Aggregator) processPool(ctx context.Context, pool string) error {
	pool = strings.ToLower(pool)

	since, ok, err := a.db.LastBucketTime(ctx, pool)
	if err != nil {
		return fmt.Errorf("aggregate: last bucket time pool=%s: %w", pool, err)
	}
	if !ok {
		// No buckets yet: start from the zero time so we pick up all history.
		since = time.Time{}
	}

	logs, err := a.db.RawLogsSince(ctx, since, []string{pool})
	if err != nil {
		return fmt.Errorf("aggregate: raw logs since pool=%s: %w", pool, err)
	}
	if len(logs) == 0 {
		return nil
	}

	buckets := a.bucketize(logs)

	// Deterministic write order (oldest first) for stable, replayable behavior.
	keys := make([]time.Time, 0, len(buckets))
	for ts := range buckets {
		keys = append(keys, ts)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })

	for _, ts := range keys {
		b := buckets[ts]
		b.Pool = pool
		b.TsBucket = ts
		if err := a.db.UpsertBucket(ctx, b); err != nil {
			return fmt.Errorf("aggregate: upsert bucket pool=%s ts=%s: %w",
				pool, ts.Format(time.RFC3339), err)
		}
	}

	log.Debug().
		Str("pool", pool).
		Int("logs", len(logs)).
		Int("buckets", len(keys)).
		Msg("aggregate: pool processed")
	return nil
}

// bucketize groups logs into bucketMin-minute windows keyed by floored UTC start
// time, accumulating swap_count, volume, and netflow per token.
func (a *Aggregator) bucketize(logs []store.RawLog) map[time.Time]store.Bucket {
	out := make(map[time.Time]store.Bucket)
	for _, l := range logs {
		if !IsSwapTopic(l.Topic0) {
			continue
		}

		key := a.floorToBucket(l.BlockTime)
		b, present := out[key]
		if !present {
			b = store.Bucket{
				Vol0:     decimal.Zero,
				Vol1:     decimal.Zero,
				Netflow0: decimal.Zero,
				Netflow1: decimal.Zero,
			}
		}

		// A swap with a known topic always counts, even if undecodable.
		b.SwapCount++

		d, err := DecodeSwap(l.Topic0, l.Data)
		if err != nil {
			// Count-only: keep the swap, contribute zero volume/netflow.
			log.Warn().
				Err(err).
				Str("tx", l.TxHash).
				Int("log_index", l.LogIndex).
				Str("topic0", l.Topic0).
				Msg("aggregate: undecodable swap data, counting without amounts")
		} else {
			b.Vol0 = b.Vol0.Add(d.Vol0)
			b.Vol1 = b.Vol1.Add(d.Vol1)
			b.Netflow0 = b.Netflow0.Add(d.Netflow0)
			b.Netflow1 = b.Netflow1.Add(d.Netflow1)
		}

		out[key] = b
	}
	return out
}

// floorToBucket truncates t (in UTC) down to the bucketMin-minute grid. The grid
// is anchored at the Unix epoch so buckets are stable and chain-independent.
func (a *Aggregator) floorToBucket(t time.Time) time.Time {
	width := time.Duration(a.bucketMin) * time.Minute
	return t.UTC().Truncate(width)
}

// SwapAmounts holds one swap's contribution to a bucket, sign-normalized so netflow
// is positive when the token flows out of the pool toward the trader. Exported so
// other stages (e.g. the whale detector) can read a single swap's decoded magnitude.
type SwapAmounts struct {
	Vol0     decimal.Decimal
	Vol1     decimal.Decimal
	Netflow0 decimal.Decimal
	Netflow1 decimal.Decimal
}

// IsSwapTopic reports whether topic0 is a Swap event we know how to count.
func IsSwapTopic(topic0 string) bool {
	switch strings.ToLower(topic0) {
	case TopicUniV2Swap, TopicUniV3Swap, TopicAgniSwap, TopicMoeLBSwap:
		return true
	default:
		return false
	}
}

// DecodeSwap parses a swap log's data into a sign-normalized contribution (per-variant
// layouts in the package doc). It errors (without panicking) when the payload is too
// short for the expected layout; callers treat that as count-only.
func DecodeSwap(topic0, data string) (SwapAmounts, error) {
	words, err := splitWords(data)
	if err != nil {
		return SwapAmounts{}, err
	}

	switch strings.ToLower(topic0) {
	case TopicUniV2Swap:
		if len(words) < 4 {
			return SwapAmounts{}, fmt.Errorf(
				"aggregate: v2 swap expects >=4 words, got %d", len(words))
		}
		a0In := decodeUint256(words[0])
		a1In := decodeUint256(words[1])
		a0Out := decodeUint256(words[2])
		a1Out := decodeUint256(words[3])
		return SwapAmounts{
			// Gross volume = in + out (both already non-negative).
			Vol0: a0In.Add(a0Out),
			Vol1: a1In.Add(a1Out),
			// Netflow = out - in: positive when the token leaves the pool.
			Netflow0: a0Out.Sub(a0In),
			Netflow1: a1Out.Sub(a1In),
		}, nil

	case TopicUniV3Swap, TopicAgniSwap:
		if len(words) < 2 {
			return SwapAmounts{}, fmt.Errorf(
				"aggregate: v3 swap expects >=2 words, got %d", len(words))
		}
		// Signed from the pool's perspective: negative = pool paid out.
		a0 := decodeInt256(words[0])
		a1 := decodeInt256(words[1])
		return SwapAmounts{
			Vol0: a0.Abs(),
			Vol1: a1.Abs(),
			// Negate so the convention matches V2 (positive = token out to trader).
			Netflow0: a0.Neg(),
			Netflow1: a1.Neg(),
		}, nil

	case TopicMoeLBSwap:
		// data = [id, amountsIn, amountsOut, volAccum, totalFees, protocolFees].
		// We need amountsIn (word 1) and amountsOut (word 2); each packs token0 in
		// the low 128 bits and token1 in the high 128 bits.
		if len(words) < 3 {
			return SwapAmounts{}, fmt.Errorf(
				"aggregate: lb swap expects >=3 words, got %d", len(words))
		}
		in0, in1 := splitPackedAmounts(words[1])
		out0, out1 := splitPackedAmounts(words[2])
		return SwapAmounts{
			// Gross volume = in + out per token (both non-negative).
			Vol0: in0.Add(out0),
			Vol1: in1.Add(out1),
			// Netflow = out - in: positive when the token leaves the pool.
			Netflow0: out0.Sub(in0),
			Netflow1: out1.Sub(in1),
		}, nil

	default:
		return SwapAmounts{}, fmt.Errorf("aggregate: unknown swap topic0 %q", topic0)
	}
}

// splitWords decodes hex log data into 32-byte words, erroring on non-hex input or a
// length that is not a multiple of 32 so a malformed payload is surfaced.
func splitWords(data string) ([][]byte, error) {
	h := strings.TrimSpace(data)
	h = strings.TrimPrefix(h, "0x")
	h = strings.TrimPrefix(h, "0X")
	if h == "" {
		return nil, errors.New("aggregate: empty log data")
	}

	raw, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("aggregate: decode hex data: %w", err)
	}
	if len(raw)%evmWordBytes != 0 {
		return nil, fmt.Errorf(
			"aggregate: log data length %d not a multiple of %d", len(raw), evmWordBytes)
	}

	words := make([][]byte, 0, len(raw)/evmWordBytes)
	for off := 0; off < len(raw); off += evmWordBytes {
		words = append(words, raw[off:off+evmWordBytes])
	}
	return words, nil
}

// decodeUint256 interprets a 32-byte word as a big-endian unsigned integer.
func decodeUint256(word []byte) decimal.Decimal {
	n := new(big.Int).SetBytes(word)
	return decimal.NewFromBigInt(n, 0)
}

// splitPackedAmounts decodes a Liquidity Book packed amounts word into its two
// token components. LB packs two uint128 values into one bytes32: token0 (X) in
// the low 128 bits, token1 (Y) in the high 128 bits. Returns (token0, token1) as
// non-negative decimals; a malformed (non-32-byte) word yields (0, 0).
func splitPackedAmounts(word []byte) (amount0, amount1 decimal.Decimal) {
	if len(word) != evmWordBytes {
		return decimal.Zero, decimal.Zero
	}
	amount1 = decimal.NewFromBigInt(new(big.Int).SetBytes(word[0:16]), 0)  // high 128 bits
	amount0 = decimal.NewFromBigInt(new(big.Int).SetBytes(word[16:32]), 0) // low 128 bits
	return amount0, amount1
}

// decodeInt256 interprets a 32-byte word as a two's-complement signed integer.
func decodeInt256(word []byte) decimal.Decimal {
	n := new(big.Int).SetBytes(word)
	// If the top bit is set the value is negative: subtract 2^256.
	if len(word) == evmWordBytes && word[0]&0x80 != 0 {
		twoTo256 := new(big.Int).Lsh(big.NewInt(1), uint(evmWordBytes*8))
		n.Sub(n, twoTo256)
	}
	return decimal.NewFromBigInt(n, 0)
}

// pruneEveryIterations is how many Run iterations pass between raw_logs prune sweeps.
// Pruning is bounded housekeeping, not per-minute hot-loop work; spacing it out keeps
// the steady-state write load light. At the 60s cadence this is ~one prune per hour.
const pruneEveryIterations = 60

// Run executes RunOnce immediately and then once per `every` interval until the
// context is cancelled. A failed RunOnce is logged and the loop continues (a transient
// DB hiccup must not kill the aggregator); ctx cancellation returns its error. Every
// pruneEveryIterations it also prunes raw_logs older than retention.
func (a *Aggregator) Run(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		return fmt.Errorf("aggregate: invalid interval %s (must be > 0)", every)
	}

	if err := a.RunOnce(ctx); err != nil {
		log.Error().Err(err).Msg("aggregate: initial run failed")
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	iterations := 0
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("aggregate: run loop stopped: %w", ctx.Err())
		case <-ticker.C:
			if err := a.RunOnce(ctx); err != nil {
				log.Error().Err(err).Msg("aggregate: run failed")
			}
			iterations++
			if iterations%pruneEveryIterations == 0 {
				a.pruneRawLogs(ctx)
			}
		}
	}
}

// pruneRawLogs deletes raw_logs older than the retention window (buckets are already
// materialized, so the aged rows are dead weight). Best-effort: a failure is logged
// and the loop continues; a non-positive retention disables it.
func (a *Aggregator) pruneRawLogs(ctx context.Context) {
	if a.retention <= 0 {
		return
	}
	if ctx.Err() != nil {
		return
	}
	cutoff := time.Now().Add(-a.retention)
	deleted, err := a.db.PruneRawLogsBefore(ctx, cutoff)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error().Err(err).Time("cutoff", cutoff).Msg("aggregate: prune raw_logs failed, will retry next cycle")
		return
	}
	if deleted > 0 {
		log.Info().Int64("deleted", deleted).Time("cutoff", cutoff).Msg("aggregate: pruned aged raw_logs")
	}
}
