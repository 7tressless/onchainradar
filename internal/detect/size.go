package detect

import (
	"strings"

	"github.com/shopspring/decimal"

	"ocr/internal/store"
)

// This file derives the display-only approximate USD size of a signal from our own
// decoded amounts. The value is written to size_usd for the read-only API and
// dashboard only; detection, scoring, and baselines never read it, so the caller-
// supplied market price never moves a detection decision.

// stableTokens is the set of known Mantle stablecoins whose on-chain amount is
// treated as one dollar per whole token for the display-only size_usd. Verified
// registry addresses from config/pools.yaml (lowercase hex), not invented here:
//
//	USDe 0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34
//	USDC 0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9
//	USDT 0x201eba5cc46d216ce6dc03f6a759e8e766e956ae
//
// Membership is by contract address (not symbol), so a relabelled pool cannot
// misclassify a token. Per-pool decimals (pools.dec0/dec1) stay authoritative for
// scaling.
var stableTokens = map[string]struct{}{
	"0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34": {}, // USDe
	"0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9": {}, // USDC
	"0x201eba5cc46d216ce6dc03f6a759e8e766e956ae": {}, // USDT
}

// isStableToken reports whether a token contract address is a known stablecoin in
// the verified set above (case-insensitive; trims surrounding space).
func isStableToken(addr string) bool {
	_, ok := stableTokens[strings.ToLower(strings.TrimSpace(addr))]
	return ok
}

// stableSizeUSD computes the display-only approximate USD notional of one swap from
// the pool's stable side (one stable token at one dollar per whole token), or nil
// when neither side is a known stablecoin. gross0/gross1 are the swap's absolute
// per-token raw amounts. Pure (no I/O).
//
//   - pool.Token0 stable -> |gross0| / 10^dec0;
//   - else pool.Token1 stable -> |gross1| / 10^dec1;
//   - else -> nil (do not guess a non-stable pool's USD value).
//
// token0 wins when both sides are stable (either is one dollar, same magnitude).
// Abs guards a signed net passed by mistake.
func stableSizeUSD(pool store.Pool, gross0, gross1 decimal.Decimal) *decimal.Decimal {
	switch {
	case isStableToken(pool.Token0):
		v := scaleByDecimals(gross0.Abs(), pool.Dec0)
		return &v
	case isStableToken(pool.Token1):
		v := scaleByDecimals(gross1.Abs(), pool.Dec1)
		return &v
	default:
		// Neither side is a dollar stable: no USD without an external price. Leave nil.
		return nil
	}
}

// scaleByDecimals divides a raw token amount by 10^decimals to get the whole-token
// value (e.g. 1_500_000 raw USDC at 6 decimals -> 1.5), exact via decimal.Shift.
func scaleByDecimals(raw decimal.Decimal, decimals int) decimal.Decimal {
	return raw.Shift(int32(-decimals))
}

// swapSizeUSD is stableSizeUSD with a market-price fallback for pools that have no
// stable side. The stable rule stays first because it needs no external price; only
// when neither side is a dollar stable is the token0 leg valued at price0 (token0's
// display market price). A nil or non-positive price0 leaves the size nil.
func swapSizeUSD(pool store.Pool, gross0, gross1 decimal.Decimal, price0 *decimal.Decimal) *decimal.Decimal {
	if v := stableSizeUSD(pool, gross0, gross1); v != nil {
		return v
	}
	return token0ValueUSD(gross0, pool.Dec0, price0)
}

// bucketSizeUSD is the display-only USD volume of one flow bucket: its gross token0
// volume valued at token0's market price. A flow signal aggregates many swaps, so it has
// no single swap size; this gives the feed a magnitude for the burst. nil when no price.
func bucketSizeUSD(pool store.Pool, vol0 decimal.Decimal, price0 *decimal.Decimal) *decimal.Decimal {
	return token0ValueUSD(vol0, pool.Dec0, price0)
}

// token0ValueUSD scales a raw token0 amount to whole tokens and values it at price0,
// returning nil when price0 is absent or non-positive. The shared core of the
// market-price size paths (a swap's token0 leg, a bucket's token0 volume).
func token0ValueUSD(rawAmount decimal.Decimal, dec0 int, price0 *decimal.Decimal) *decimal.Decimal {
	if price0 == nil || !price0.IsPositive() {
		return nil
	}
	v := scaleByDecimals(rawAmount.Abs(), dec0).Mul(*price0)
	return &v
}

// poolPrice looks up a pool's token0 market price from a PoolPricesUSD map, returning a
// fresh pointer (so callers can store it) or nil when the pool has no price yet. The
// returned price feeds only the display-only size_usd, never a detection decision.
func poolPrice(prices map[string]decimal.Decimal, pool string) *decimal.Decimal {
	if p, ok := prices[strings.ToLower(strings.TrimSpace(pool))]; ok {
		return &p
	}
	return nil
}
