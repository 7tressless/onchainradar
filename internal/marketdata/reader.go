// Package marketdata builds each pool's display-only market snapshot (TVL, 24h volume,
// spot price) from first-party sources: token reserves and spot price read on-chain
// (eth_call), and 24h volume summed from OCR's own aggregated buckets. It replaces the
// third-party aggregator on the display path — no API key, no monthly cap, no indexer lag.
//
// This data is for human display only: detection runs purely on raw on-chain buckets and
// never reads it. Pricing anchors the verified dollar stables (internal/tokens) at $1 and
// derives every other token (WMNT) from a Uniswap-V3-shape pool's on-chain spot price, so
// no external price feed is involved.
package marketdata

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"ocr/internal/store"
	"ocr/internal/tokens"
)

// sourceOnchain is the provenance marker stamped on every snapshot, distinguishing the
// on-chain-derived figures from the former third-party source in pool_stats.source_url.
const sourceOnchain = "on-chain"

// volumeWindow is the look-back for the 24h USD volume summed from buckets.
const volumeWindow = 24 * time.Hour

// blocksPerDay is the ~24h look-back in blocks for the price change. Mantle produces ~1
// block / 2s (1800/hr, see cmd/ocr mantleBlocksPerHour), so 24h ~= 43200 blocks; the
// block time is stable enough that the small drift is immaterial for a display percent.
const blocksPerDay = 43200

// twoPow96 is the Q64.96 scale (2^96) a Uniswap-V3 sqrtPriceX96 is fixed against.
var twoPow96 = new(big.Int).Lsh(big.NewInt(1), 96)

// PoolMarketStats is one pool's display-only market snapshot derived first-party. A field
// is nil when its inputs were unavailable (an RPC read failed, or a token's USD price
// could not be derived), so a missing value stays NULL downstream rather than a bogus 0.
type PoolMarketStats struct {
	PoolAddr  string
	TVLUSD    *float64
	Vol24hUSD *float64
	PriceUSD  *float64 // token0's USD price (matches pool_stats.price_usd semantics)
	// PriceChange24h is token0's percent price change vs ~24h ago, from the same derivation
	// read at the block ~24h back (the archive RPC serves historical slot0). A stable's
	// flat $1 reads 0; nil when the historical price is unavailable (non-archive RPC, or a
	// pool younger than 24h).
	PriceChange24h *float64
	SourceURL      string
}

// ChainCaller is the on-chain read surface the reader needs: the chain head (to locate the
// ~24h-ago block), a pool's token reserves (balanceOf), and a pool's spot price — slot0
// sqrtPriceX96 for a V3 pool (Agni), or the active bin + bin step for a Liquidity Book pool
// (Merchant Moe) — at a given block. *chain.Client satisfies it; a mock satisfies it in
// tests with no RPC.
type ChainCaller interface {
	LatestBlock(ctx context.Context) (uint64, error)
	TokenBalance(ctx context.Context, token, holder common.Address) (*big.Int, error)
	PoolSqrtPriceX96At(ctx context.Context, pool common.Address, block *big.Int) (*big.Int, error)
	PoolActiveBinAt(ctx context.Context, pool common.Address, block *big.Int) (activeID uint32, binStep uint16, err error)
}

// VolumeSource yields a pool's gross swap volume per token (raw units) since a cutoff,
// from OCR's aggregated buckets. *store.DB satisfies it.
type VolumeSource interface {
	SumBucketVolumeSince(ctx context.Context, pool string, since time.Time) (vol0, vol1 decimal.Decimal, err error)
}

// Reader produces pool market snapshots from the chain and OCR's buckets. Construct with
// NewReader; it is stateless across passes and safe to reuse.
type Reader struct {
	chain  ChainCaller
	vol    VolumeSource
	window time.Duration
	source string
}

// NewReader builds a Reader over an on-chain caller and a bucket volume source.
func NewReader(chain ChainCaller, vol VolumeSource) *Reader {
	return &Reader{chain: chain, vol: vol, window: volumeWindow, source: sourceOnchain}
}

// Snapshot computes the display-only market snapshot for every pool in `pools`, keyed by
// lowercase pool address. It first derives a USD price per token (stables at $1, every
// other token from a V3 pool's on-chain spot price), then values each pool's reserves and
// 24h volume. Best-effort: a per-pool read failure logs and leaves that pool's affected
// fields nil; only ctx cancellation aborts the pass. A pool whose every field is unknown
// is still returned (so the caller can refresh fetched_at).
func (r *Reader) Snapshot(ctx context.Context, pools []store.Pool) (map[string]PoolMarketStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("marketdata: snapshot cancelled: %w", err)
	}

	pricesNow, err := r.derivePrices(ctx, pools, nil)
	if err != nil {
		return nil, err
	}
	// Prices ~24h ago for the change field, best-effort: an unavailable historical read
	// leaves the change nil without touching the live fields.
	pricesOld := r.pricesAt24hAgo(ctx, pools)

	since := time.Now().Add(-r.window)
	out := make(map[string]PoolMarketStats, len(pools))
	for i := range pools {
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("marketdata: snapshot cancelled: %w", err)
		}
		out[lower(pools[i].Address)] = r.poolStats(ctx, pools[i], pricesNow, pricesOld, since)
	}
	return out, nil
}

// pricesAt24hAgo derives the token price map at the block ~24h back, for the 24h change.
// Best-effort: a missing chain head, a height below the window, or a historical read error
// yields an empty map (every change stays nil) without failing the snapshot. The archive
// RPC serves the historical slot0; a non-archive node simply degrades the change to nil.
func (r *Reader) pricesAt24hAgo(ctx context.Context, pools []store.Pool) map[string]float64 {
	latest, err := r.chain.LatestBlock(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("marketdata: latest block read failed; 24h change omitted")
		return nil
	}
	if latest <= blocksPerDay {
		return nil // chain younger than the window (only a fresh devnet); no 24h baseline
	}
	old := new(big.Int).SetUint64(latest - blocksPerDay)
	prices, err := r.derivePrices(ctx, pools, old)
	if err != nil {
		return nil // ctx cancelled; the Snapshot loop surfaces it
	}
	return prices
}

// derivePrices builds a USD price per token across the pool set at the given block (nil =
// latest): stables anchor at $1, then every other token is priced from a V3 pool that
// pairs it with an already-priced token, reading that pool's slot0 spot price. It iterates
// to a fixpoint (a token can be one hop from a stable, the next a hop from that token).
// Each pool's slot0 is read at most once (cached). A failed slot0 read is logged and
// skipped (the pool just does not help pricing); ctx cancellation aborts. A token that
// stays unpriced is simply absent, so the pools needing it emit nil for the affected
// fields. Called once at the head and once at the ~24h-ago block (for the change).
func (r *Reader) derivePrices(ctx context.Context, pools []store.Pool, block *big.Int) (map[string]float64, error) {
	prices := make(map[string]float64)
	for i := range pools {
		seedStable(prices, pools[i].Token0)
		seedStable(prices, pools[i].Token1)
	}

	cache := make(map[string]*float64) // pool addr -> spot price0in1 (nil = read failed)
	// Bounded fixpoint: each productive pass prices >= 1 new token, so len(pools) passes
	// cover the longest possible chain; the loop breaks early once a pass adds nothing.
	for pass := 0; pass <= len(pools); pass++ {
		changed := false
		for i := range pools {
			p := pools[i]
			if !isPriceable(p.Dex) {
				continue // an unsupported dex cannot supply a spot price
			}
			t0, t1 := lower(p.Token0), lower(p.Token1)
			_, have0 := prices[t0]
			_, have1 := prices[t1]
			if have0 == have1 {
				continue // both known (nothing to add) or both unknown (cannot bridge)
			}
			if err := ctx.Err(); err != nil {
				return prices, fmt.Errorf("marketdata: derive prices cancelled: %w", err)
			}
			price0in1, ok := r.poolSpot(ctx, p, cache, block)
			if !ok {
				continue
			}
			if have0 {
				prices[t1] = prices[t0] / price0in1
			} else {
				prices[t0] = prices[t1] * price0in1
			}
			changed = true
		}
		if !changed {
			break
		}
	}
	return prices, nil
}

// poolSpot returns a pool's human spot price of token0 in token1 (whole token1 per whole
// token0) at `block` (nil = latest), reading and caching it. ok=false when the read failed
// or the price is non-positive. The result is cached per pool (nil = a failed read, so it
// is not retried within the pass).
func (r *Reader) poolSpot(ctx context.Context, p store.Pool, cache map[string]*float64, block *big.Int) (float64, bool) {
	addr := lower(p.Address)
	if cached, ok := cache[addr]; ok {
		if cached == nil {
			return 0, false
		}
		return *cached, true
	}
	price, ok := r.readSpot(ctx, p, block)
	if !ok {
		cache[addr] = nil // negative-cache
		return 0, false
	}
	cache[addr] = &price
	return price, true
}

// readSpot reads a pool's spot price0in1 from the chain, by DEX family: a V3 pool (Agni)
// via slot0's sqrtPriceX96, a Liquidity Book pool (Merchant Moe) via its active bin + bin
// step. Decimal-adjusted by the pool's authoritative dec0/dec1. ok=false on a read error or
// a non-positive price.
func (r *Reader) readSpot(ctx context.Context, p store.Pool, block *big.Int) (float64, bool) {
	pool := common.HexToAddress(p.Address)
	switch {
	case isV3(p.Dex):
		sqrtP, err := r.chain.PoolSqrtPriceX96At(ctx, pool, block)
		if err != nil {
			log.Warn().Err(err).Str("pool", lower(p.Address)).Msg("marketdata: slot0 read failed; skipping for pricing")
			return 0, false
		}
		return sqrtPriceToPrice0in1(sqrtP, p.Dec0, p.Dec1)
	case isLB(p.Dex):
		activeID, binStep, err := r.chain.PoolActiveBinAt(ctx, pool, block)
		if err != nil {
			log.Warn().Err(err).Str("pool", lower(p.Address)).Msg("marketdata: active bin read failed; skipping for pricing")
			return 0, false
		}
		return binPriceToPrice0in1(activeID, binStep, p.Dec0, p.Dec1)
	default:
		return 0, false
	}
}

// poolStats builds one pool's snapshot: token0's USD price (now), its 24h change (now vs
// the ~24h-ago map), TVL from on-chain reserves, and 24h USD volume from OCR's buckets.
// Each field is independent and best-effort: a failed reserves read leaves TVL nil while
// price/volume still populate; a missing token price leaves TVL (and a non-stable token0's
// price) nil; a missing historical price leaves only the change nil.
func (r *Reader) poolStats(ctx context.Context, p store.Pool, pricesNow, pricesOld map[string]float64, since time.Time) PoolMarketStats {
	ms := PoolMarketStats{PoolAddr: lower(p.Address), SourceURL: r.source}
	t0 := lower(p.Token0)
	if px, ok := pricesNow[t0]; ok {
		ms.PriceUSD = floatPtr(px)
	}
	ms.PriceChange24h = priceChange24h(pricesNow, pricesOld, t0)
	ms.TVLUSD = r.tvlUSD(ctx, p, pricesNow)
	ms.Vol24hUSD = r.volumeUSD(ctx, p, pricesNow, since)
	return ms
}

// priceChange24h computes token0's percent price change from the now/old price maps, nil
// when either price is missing or the old price is zero (so a stable's flat $1 reads 0,
// and an unpriced or just-born token reads nil).
func priceChange24h(now, old map[string]float64, token string) *float64 {
	pn, okN := now[token]
	po, okO := old[token]
	if !okN || !okO || po == 0 {
		return nil
	}
	c := (pn - po) / po * 100
	return &c
}

// tvlUSD values a pool's on-chain reserves (token balances) in USD: reserve0*price0 +
// reserve1*price1. nil when either token has no derived price or either balanceOf read
// fails, so a partial TVL is never shown as a whole figure.
func (r *Reader) tvlUSD(ctx context.Context, p store.Pool, prices map[string]float64) *float64 {
	p0, ok0 := prices[lower(p.Token0)]
	p1, ok1 := prices[lower(p.Token1)]
	if !ok0 || !ok1 {
		return nil
	}
	pool := common.HexToAddress(p.Address)
	bal0, err := r.chain.TokenBalance(ctx, common.HexToAddress(p.Token0), pool)
	if err != nil {
		log.Warn().Err(err).Str("pool", lower(p.Address)).Msg("marketdata: token0 reserve read failed; TVL omitted")
		return nil
	}
	bal1, err := r.chain.TokenBalance(ctx, common.HexToAddress(p.Token1), pool)
	if err != nil {
		log.Warn().Err(err).Str("pool", lower(p.Address)).Msg("marketdata: token1 reserve read failed; TVL omitted")
		return nil
	}
	tvl := rawToWhole(bal0, p.Dec0)*p0 + rawToWhole(bal1, p.Dec1)*p1
	return floatPtr(tvl)
}

// volumeUSD values a pool's 24h swap volume (from OCR's buckets) in USD, preferring the
// stable side (exact $1) and otherwise token0's derived price. 0 for an empty window (a
// real zero); nil only when the bucket read fails, or neither side can be valued (no
// stable side and no token0 price).
func (r *Reader) volumeUSD(ctx context.Context, p store.Pool, prices map[string]float64, since time.Time) *float64 {
	vol0, vol1, err := r.vol.SumBucketVolumeSince(ctx, p.Address, since)
	if err != nil {
		log.Warn().Err(err).Str("pool", lower(p.Address)).Msg("marketdata: 24h volume read failed; omitted")
		return nil
	}
	switch {
	case tokens.IsStable(p.Token0):
		return floatPtr(decToWhole(vol0, p.Dec0))
	case tokens.IsStable(p.Token1):
		return floatPtr(decToWhole(vol1, p.Dec1))
	default:
		if px, ok := prices[lower(p.Token0)]; ok {
			return floatPtr(decToWhole(vol0, p.Dec0) * px)
		}
		return nil
	}
}

// sqrtPriceToPrice0in1 converts a Q64.96 sqrtPriceX96 into the human spot price of token0
// in token1: (sqrtP / 2^96)^2 * 10^(dec0-dec1). Uses big.Float for the wide square.
// ok=false on a non-positive or non-finite result.
func sqrtPriceToPrice0in1(sqrtP *big.Int, dec0, dec1 int) (float64, bool) {
	if sqrtP == nil || sqrtP.Sign() <= 0 {
		return 0, false
	}
	q := new(big.Float).SetPrec(256).Quo(
		new(big.Float).SetPrec(256).SetInt(sqrtP),
		new(big.Float).SetPrec(256).SetInt(twoPow96),
	)
	q.Mul(q, q)                // (sqrtP/2^96)^2 = raw token1 per raw token0
	q.Mul(q, pow10(dec0-dec1)) // adjust raw->whole: *10^(dec0-dec1)
	f, _ := q.Float64()
	if f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// Liquidity Book price constants: a bin's price is (1 + binStep/lbBasisPoints)^(activeId -
// lbCenterBin), where lbCenterBin (2^23) is the bin whose price is 1.
const (
	lbCenterBin   = 8388608 // 2^23
	lbBasisPoints = 10000.0
)

// binPriceToPrice0in1 converts a Liquidity Book pool's active bin + bin step into the human
// spot price of token0 in token1: (1 + binStep/1e4)^(activeId - 2^23) * 10^(dec0-dec1)
// (raw token1 per raw token0, decimal-adjusted). The formula and decimal convention are
// verified on-chain against known pools (WMNT/USDT, FBTC/cmETH). ok=false on a zero bin
// step (not an LB pool) or a non-finite result. float64 math.Pow is exact enough for a
// display price across the realistic bin range.
func binPriceToPrice0in1(activeID uint32, binStep uint16, dec0, dec1 int) (float64, bool) {
	if binStep == 0 {
		return 0, false
	}
	raw := math.Pow(1+float64(binStep)/lbBasisPoints, float64(int64(activeID)-lbCenterBin))
	price := raw * math.Pow(10, float64(dec0-dec1))
	if price <= 0 || math.IsInf(price, 0) || math.IsNaN(price) {
		return 0, false
	}
	return price, true
}

// pow10 returns 10^n as a big.Float for any integer n (negative when dec1 > dec0).
func pow10(n int) *big.Float {
	res := big.NewFloat(1).SetPrec(256)
	ten := big.NewFloat(10).SetPrec(256)
	k := n
	if k < 0 {
		k = -k
	}
	for i := 0; i < k; i++ {
		res.Mul(res, ten)
	}
	if n < 0 {
		return new(big.Float).SetPrec(256).Quo(big.NewFloat(1).SetPrec(256), res)
	}
	return res
}

// rawToWhole converts a raw token amount (smallest units) to whole tokens via 10^dec.
func rawToWhole(raw *big.Int, dec int) float64 {
	if raw == nil {
		return 0
	}
	f := new(big.Float).SetPrec(256).SetInt(raw)
	f.Quo(f, pow10(dec))
	v, _ := f.Float64()
	return v
}

// decToWhole converts a raw decimal amount to whole tokens via 10^dec.
func decToWhole(d decimal.Decimal, dec int) float64 {
	return d.Shift(int32(-dec)).InexactFloat64()
}

// seedStable anchors a verified dollar stable at $1 in the price map (no-op otherwise).
func seedStable(prices map[string]float64, token string) {
	if tokens.IsStable(token) {
		prices[lower(token)] = 1.0
	}
}

// isV3 reports whether a dex uses the Uniswap-V3 shape (slot0 spot price): Agni and
// FusionX.
func isV3(dex string) bool {
	switch strings.ToLower(strings.TrimSpace(dex)) {
	case "agni", "fusionx":
		return true
	default:
		return false
	}
}

// isLB reports whether a dex uses the Liquidity Book shape (active-bin spot price):
// Merchant Moe.
func isLB(dex string) bool {
	return strings.ToLower(strings.TrimSpace(dex)) == "merchant_moe"
}

// isPriceable reports whether a pool's dex can supply an on-chain spot price (V3 or LB), so
// it can bridge an unpriced token in the derivation. A pool on any other dex is still
// valued from prices derived elsewhere; it just cannot itself price a new token.
func isPriceable(dex string) bool {
	return isV3(dex) || isLB(dex)
}

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func floatPtr(f float64) *float64 { return &f }
