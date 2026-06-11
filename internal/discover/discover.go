package discover

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog/log"

	"ocr/internal/config"
	"ocr/internal/store"
)

// anchorSymbols is the token universe a discovered pool may be built from: both on-chain
// tokens must be anchors or the pool is rejected, bounding the registry to a small
// hand-verified set so a bad token from the discovery API cannot reach detection.
// Addresses are lowercase; matching is by address, and the symbol is only a human label.
var anchorSymbols = map[string]string{
	"0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8": "WMNT",
	"0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34": "USDe",
	"0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9": "USDC",
	"0x201eba5cc46d216ce6dc03f6a759e8e766e956ae": "USDT",
	"0xcda86a272531e8640cd7f1a92c01839911b90bb0": "mETH",
	"0xe6829d9a7ee3040e1276fa75293bde931859e8fa": "cmETH", // liquid restaking token (verified on-chain: symbol/decimals)
	"0xdeaddeaddeaddeaddeaddeaddeaddeaddead1111": "WETH",  // bridged WETH (verified on-chain; Mantle sentinel address, decimals 18)
	"0xc96de26018a54d51c097160568752c4e3bd6c364": "FBTC",  // fBTC, 8 decimals (verified on-chain)
}

// isAnchor reports whether a token address (any case) is a known anchor.
func isAnchor(addr string) bool {
	_, ok := anchorSymbols[strings.ToLower(addr)]
	return ok
}

// anchorSymbol returns the canonical symbol for an anchor address (any case), or
// the lowercased address itself when unknown. Callers gate on isAnchor first, so
// the fallback is never hit for a proposed pool.
func anchorSymbol(addr string) string {
	if s, ok := anchorSymbols[strings.ToLower(addr)]; ok {
		return s
	}
	return strings.ToLower(addr)
}

// canonicalDex maps a raw discovery-API dex id to the canonical dex name, which also
// selects PoolTokens's accessor pair, so it must match chain.PoolTokens's switch. The
// match is case-insensitive and tolerant of versioned slugs ("agni" -> "agni";
// "merchant"+"moe" -> "merchant_moe"). Unsupported DEXes return ("", false).
//
// FusionX maps only for its V2 (Uniswap-V2-shape) pools, whose standard UniV2 Swap event
// aggregate.DecodeSwap already decodes. FusionX V3 emits a non-standard swap event OCR
// cannot decode; proposing one would feed the aggregator count-only, zero-volume buckets
// and corrupt its signals, so an explicit "v3" id is rejected.
func canonicalDex(dexID string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(dexID))
	switch {
	case strings.Contains(id, "agni"):
		return "agni", true
	case strings.Contains(id, "merchant") && strings.Contains(id, "moe"):
		return "merchant_moe", true
	case strings.Contains(id, "fusionx") && (strings.Contains(id, "v2") || !strings.Contains(id, "v3")):
		return "fusionx", true
	default:
		return "", false
	}
}

// ChainVerifier is the narrow on-chain dependency Discover needs: re-derive a pool's
// canonical token ordering and each token's decimals via eth_call, and read a Liquidity
// Book pool's bin step. The real *chain.Client satisfies it; tests inject a mock so the
// verify pipeline runs without an RPC node.
type ChainVerifier interface {
	PoolTokens(ctx context.Context, pool common.Address, dex string) (token0, token1 common.Address, err error)
	TokenDecimals(ctx context.Context, token common.Address) (uint8, error)
	// PoolBinStep reads a merchant_moe (Liquidity Book) pool's bin step (Agni pools
	// have none). Non-fatal: a failure still proposes with bin_step nil. Deep-link only.
	PoolBinStep(ctx context.Context, pool common.Address) (uint16, error)
}

// Discover fetches candidate pools from gt, runs the propose-then-verify pipeline per
// candidate, and returns only fully-verified proposals as store.Pool rows with Enabled =
// false. It never trusts the API for any property OCR relies on; the API only shortlists
// candidates and names the dex.
//
// Per candidate, in order (cheap filters first, on-chain calls last):
//
//  1. dex supported? canonicalDex(DexID) maps to a decodable DEX, else skip.
//  2. coarse discovery gate? Keep when ReserveUSD OR Vol24hUSD clears its floor (a
//     high-volume, lower-TVL pool stays eligible); never detection math.
//  3. on-chain token ordering: PoolTokens(pool, dex) → canonical token0/token1,
//     discarding the API's tokens. A call failure ⇒ skip.
//  4. anchor/anchor? Both on-chain tokens must be anchors, so an API that lies about
//     a pool being anchor/anchor cannot get a non-anchor pool proposed.
//  5. on-chain decimals: TokenDecimals(token0/token1). A failure ⇒ skip.
//
// A candidate failing any step is logged and dropped; one bad candidate never aborts the
// run, only ctx cancellation returns early. Proposals are always Enabled=false (--auto at
// the call site decides promotion). Duplicate addresses are de-duped (first verified wins).
func Discover(ctx context.Context, gt GeckoClient, ch ChainVerifier, cfg *config.Config) ([]store.Pool, error) {
	if gt == nil || ch == nil || cfg == nil {
		return nil, fmt.Errorf("discover: nil dependency (gt/ch/cfg)")
	}

	candidates, err := gt.TopPools(ctx, discoverPages)
	if err != nil {
		return nil, fmt.Errorf("discover: fetch candidates: %w", err)
	}

	var (
		out  []store.Pool
		seen = make(map[string]struct{}, len(candidates))

		// Counters for the run summary so it is clear why candidates dropped.
		nUnsupportedDex, nLowLiquidity, nVerifyFail, nNotAnchor, nDup int
	)

	for i := range candidates {
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("discover: context cancelled: %w", err)
		}
		c := candidates[i]

		// (1) decodable DEX?
		dex, ok := canonicalDex(c.DexID)
		if !ok {
			nUnsupportedDex++
			log.Debug().Str("pool", c.PoolAddr).Str("dex_id", c.DexID).
				Msg("discover: skip: unsupported dex (no decoder)")
			continue
		}

		// (2) Coarse discovery gate: skip only when below both the reserve and volume
		// floors. The on-chain checks below are the real safety gate.
		if c.ReserveUSD < cfg.DiscoverMinLiquidityUSD && c.Vol24hUSD < cfg.DiscoverMinVol24USD {
			nLowLiquidity++
			log.Debug().Str("pool", c.PoolAddr).
				Float64("reserve_usd", c.ReserveUSD).
				Float64("reserve_floor", cfg.DiscoverMinLiquidityUSD).
				Float64("vol24h_usd", c.Vol24hUSD).
				Float64("vol24h_floor", cfg.DiscoverMinVol24USD).
				Msg("discover: skip: below liquidity and volume floor")
			continue
		}

		// Cheap de-dup before any RPC: the same pool can appear once per run only.
		poolAddr := strings.ToLower(c.PoolAddr)
		if !common.IsHexAddress(poolAddr) {
			nVerifyFail++
			log.Warn().Str("pool", c.PoolAddr).Msg("discover: skip: pool address is not valid hex")
			continue
		}
		if _, dup := seen[poolAddr]; dup {
			nDup++
			continue
		}

		// (3) On-chain canonical token ordering, overriding whatever the API said.
		pool := common.HexToAddress(poolAddr)
		token0, token1, terr := ch.PoolTokens(ctx, pool, dex)
		if terr != nil {
			if ctx.Err() != nil {
				return out, fmt.Errorf("discover: context cancelled: %w", ctx.Err())
			}
			nVerifyFail++
			log.Warn().Err(terr).Str("pool", poolAddr).Str("dex", dex).
				Msg("discover: skip: cannot read on-chain tokens (unverifiable)")
			continue
		}
		t0 := strings.ToLower(token0.Hex())
		t1 := strings.ToLower(token1.Hex())

		// (4) anchor/anchor against the on-chain tokens, not the API's.
		if !isAnchor(t0) || !isAnchor(t1) {
			nNotAnchor++
			log.Debug().Str("pool", poolAddr).Str("token0", t0).Str("token1", t1).
				Msg("discover: skip: on-chain tokens are not both anchors")
			continue
		}

		// (5) On-chain decimals for each token.
		dec0, derr := ch.TokenDecimals(ctx, token0)
		if derr != nil {
			if ctx.Err() != nil {
				return out, fmt.Errorf("discover: context cancelled: %w", ctx.Err())
			}
			nVerifyFail++
			log.Warn().Err(derr).Str("pool", poolAddr).Str("token0", t0).
				Msg("discover: skip: cannot read token0 decimals (unverifiable)")
			continue
		}
		dec1, derr := ch.TokenDecimals(ctx, token1)
		if derr != nil {
			if ctx.Err() != nil {
				return out, fmt.Errorf("discover: context cancelled: %w", ctx.Err())
			}
			nVerifyFail++
			log.Warn().Err(derr).Str("pool", poolAddr).Str("token1", t1).
				Msg("discover: skip: cannot read token1 decimals (unverifiable)")
			continue
		}

		// (6) merchant_moe (Liquidity Book) only: read the bin step for the add-liquidity
		// deep-link. Best-effort, a read failure still proposes with bin_step nil.
		var binStep *int
		if dex == "merchant_moe" {
			bs, bserr := ch.PoolBinStep(ctx, pool)
			if bserr != nil {
				if ctx.Err() != nil {
					return out, fmt.Errorf("discover: context cancelled: %w", ctx.Err())
				}
				// `ocr poolmeta` can backfill the missing bin step later.
				log.Warn().Err(bserr).Str("pool", poolAddr).
					Msg("discover: bin step read failed; proposing with bin_step nil")
			} else {
				v := int(bs)
				binStep = &v
			}
		}

		// Fully verified: build the proposal from on-chain values only; the label is
		// anchor symbols in canonical order. enabled stays false (the call site promotes).
		label := anchorSymbol(t0) + "/" + anchorSymbol(t1)
		seen[poolAddr] = struct{}{}
		out = append(out, store.Pool{
			Address:   poolAddr,
			Dex:       dex,
			Token0:    t0,
			Token1:    t1,
			Dec0:      int(dec0),
			Dec1:      int(dec1),
			Label:     label,
			Enabled:   false, // propose only; never blind-enable
			SourceURL: geckoPoolURL(poolAddr),
			BinStep:   binStep, // nil for Agni / unread moe; set for a read moe bin step
		})

		ev := log.Info().
			Str("pool", poolAddr).
			Str("dex", dex).
			Str("label", label).
			Int("dec0", int(dec0)).
			Int("dec1", int(dec1)).
			Float64("reserve_usd", c.ReserveUSD)
		if binStep != nil {
			ev = ev.Int("bin_step", *binStep)
		}
		ev.Msg("discover: verified anchor pool proposed (enabled=false)")
	}

	log.Info().
		Int("candidates", len(candidates)).
		Int("proposed", len(out)).
		Int("skip_unsupported_dex", nUnsupportedDex).
		Int("skip_low_liquidity", nLowLiquidity).
		Int("skip_not_anchor", nNotAnchor).
		Int("skip_verify_fail", nVerifyFail).
		Int("skip_duplicate", nDup).
		Msg("discover: pipeline complete")

	return out, nil
}

// discoverPages is how many listing pages the pipeline pulls. The first few cover the
// high-liquidity pools and deeper pages are mostly dust, so three is a rate-limit-friendly
// default.
const discoverPages = 3

// geckoPoolURL returns the public GeckoTerminal page for a pool, stored as the proposal's
// source_url for a one-click cross-reference.
func geckoPoolURL(poolAddr string) string {
	return fmt.Sprintf("https://www.geckoterminal.com/%s/pools/%s", geckoNetwork, strings.ToLower(poolAddr))
}

// parseUSD parses a reserve string into a float64. A blank or unparseable value yields 0,
// the safe default (a 0 reserve fails the floor and is skipped). Coarse gate only.
func parseUSD(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseFloatPtr parses a decimal string into a *float64, returning nil for a blank or
// unparseable value. Unlike parseUSD it distinguishes absent (nil → NULL) from a genuine
// 0, which matters for display-only stats (a missing price must not render as $0).
func parseFloatPtr(s string) *float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

// parseUSDPtr is parseFloatPtr under a name that reads as a USD field at the call site
// (TVL, volume, price). Same lenient nil-on-missing semantics.
func parseUSDPtr(s string) *float64 {
	return parseFloatPtr(s)
}
