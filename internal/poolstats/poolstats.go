// Package poolstats refreshes the display-only external market snapshot for each enabled
// pool (TVL, 24h volume, spot price, 24h price change) from GeckoTerminal into pool_stats,
// on a slow cadence.
//
// This data is for human display only: never read by the detector, aggregator, or any
// scoring/baseline path (detection runs purely on raw on-chain buckets). It lives in its
// own table so the external numbers cannot be mistaken for a detection input.
//
// Best-effort per pool: a fetch failure logs and continues, retrying next tick. Calls are
// spaced for GeckoTerminal's keyless rate limit (~30/min). The loop self-disables on a
// non-positive interval.
package poolstats

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"ocr/internal/discover"
	"ocr/internal/store"
)

// StatsFetcher is the narrow dependency the stage needs: fetch one pool's external
// market snapshot, and the per-token spot price used to override each pool's display
// price. *discover.Gecko satisfies it; isolating it lets the stage be unit-tested with a
// mock (no network).
type StatsFetcher interface {
	PoolStats(ctx context.Context, poolAddr string) (discover.PoolMarketStats, error)
	TokenSpotPrices(ctx context.Context, addrs []string) (map[string]discover.TokenSpot, error)
}

// poolStore is the slice of *store.DB the stage uses: list the enabled pools, upsert each
// pool snapshot, and upsert each token's display-only metadata. Isolated for testability.
type poolStore interface {
	ListEnabledPools(ctx context.Context) ([]store.Pool, error)
	UpsertPoolStats(ctx context.Context, ps store.PoolStats) error
	UpsertTokenMeta(ctx context.Context, tm store.TokenMeta) error
}

// callSpacing is the delay between per-pool GeckoTerminal calls within one pass. At
// ~2s/call the handful of registered pools stay well under the keyless ~30/min limit,
// negligible against the 10-min interval. Mirrors the discovery client's geckoPageSleep.
const callSpacing = 2 * time.Second

// Stage refreshes pool_stats on an interval. Construct with New; run with Run.
type Stage struct {
	db       poolStore
	fetcher  StatsFetcher
	interval time.Duration
}

// New constructs the poolstats stage over a store and a stats fetcher, refreshing
// every `interval`. A non-positive interval makes Run a no-op (the stage is
// disabled); the supervisor gates on that.
func New(db poolStore, fetcher StatsFetcher, interval time.Duration) *Stage {
	return &Stage{db: db, fetcher: fetcher, interval: interval}
}

// Run refreshes every enabled pool's snapshot once at startup, then every interval,
// until ctx is cancelled. A non-positive interval disables the stage (logs once, returns
// nil). A pass or per-pool failure is logged and the loop continues; only ctx
// cancellation ends it.
func (s *Stage) Run(ctx context.Context) error {
	if s.interval <= 0 {
		log.Info().Msg("poolstats: disabled (POOL_STATS_INTERVAL_MIN <= 0)")
		return nil
	}

	log.Info().Dur("interval", s.interval).Msg("poolstats: loop started")

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	// Refresh once at startup so the dashboard has context without waiting an interval.
	s.runPass(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.runPass(ctx)
		}
	}
}

// RunOnce refreshes every enabled pool's snapshot exactly once and returns. Unlike Run
// it ignores the disabled gate (interval <= 0): the one-shot CLI (`ocr poolstats`) always
// refreshes once. Errors are best-effort per pool as in a normal pass; returns ctx.Err()
// only on cancellation.
func (s *Stage) RunOnce(ctx context.Context) error {
	s.runPass(ctx)
	return ctx.Err()
}

// runPass refreshes every enabled pool's snapshot once, spacing the calls. It never
// returns an error (a list or per-pool failure is logged and the pass continues) but
// stops promptly on ctx cancellation. Fetch and upsert are best-effort: one bad pool
// must not block the others.
func (s *Stage) runPass(ctx context.Context) {
	pools, err := s.db.ListEnabledPools(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error().Err(err).Msg("poolstats: list enabled pools failed, will retry next tick")
		return
	}

	// One batched spot-price lookup keyed by token0 address: a token reads one price
	// everywhere, instead of CoinGecko's per-pool reserve-derived price (which makes the
	// same token show different prices across pools). Display-only; on failure each pool
	// keeps its per-pool price. Done once before the loop so it costs a single request.
	spot := s.fetchTokenSpot(ctx, pools)

	refreshed := 0
	for i, p := range pools {
		if ctx.Err() != nil {
			return
		}

		ms, ferr := s.fetcher.PoolStats(ctx, p.Address)
		if ferr != nil {
			if ctx.Err() != nil {
				return
			}
			// Best-effort: one pool's failure must not abort the pass; retry next tick.
			log.Warn().Err(ferr).Str("pool", p.Address).Msg("poolstats: fetch failed, skipping pool")
			s.space(ctx, i, len(pools))
			continue
		}

		// Override the display price with token0's single spot value; tvl/vol stay per-pool.
		applySpotPrice(&ms, p.Token0, spot)

		if uerr := s.db.UpsertPoolStats(ctx, toStorePoolStats(ms)); uerr != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error().Err(uerr).Str("pool", p.Address).Msg("poolstats: upsert failed, skipping pool")
			s.space(ctx, i, len(pools))
			continue
		}
		refreshed++
		log.Debug().Str("pool", p.Address).Msg("poolstats: snapshot refreshed")

		// Best-effort display-only token metadata (symbols/logos/decimals), keyed by
		// token address. A failure never aborts the pass or undoes the upsert above:
		// token_meta is purely cosmetic, so log and continue. Never read by detection.
		s.upsertTokenMeta(ctx, p.Address, ms)

		s.space(ctx, i, len(pools))
	}

	if refreshed > 0 {
		log.Info().Int("refreshed", refreshed).Int("pools", len(pools)).Msg("poolstats: pass complete")
	}
}

// space sleeps callSpacing between per-pool calls (skipping after the last pool),
// returning early on ctx cancellation, to stay under the keyless rate limit.
func (s *Stage) space(ctx context.Context, idx, total int) {
	if idx >= total-1 {
		return // no sleep after the final pool
	}
	select {
	case <-ctx.Done():
	case <-time.After(callSpacing):
	}
}

// fetchTokenSpot gathers the distinct token0 addresses of the pools being refreshed and
// fetches their single spot price in one batched call, best-effort: a failure (or no
// token0s) is logged and yields an empty map, so each pool falls back to its per-pool
// price. The map is keyed by lowercased token address.
func (s *Stage) fetchTokenSpot(ctx context.Context, pools []store.Pool) map[string]discover.TokenSpot {
	addrs := make([]string, 0, len(pools))
	for _, p := range pools {
		if p.Token0 != "" {
			addrs = append(addrs, p.Token0)
		}
	}
	if len(addrs) == 0 {
		return nil
	}
	spot, err := s.fetcher.TokenSpotPrices(ctx, addrs)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		log.Warn().Err(err).Msg("poolstats: token spot price fetch failed, falling back to per-pool prices")
		return nil
	}
	return spot
}

// applySpotPrice overrides a pool's display price with token0's single spot value when one
// is present, leaving tvl/vol (correct per-pool) untouched. A missing token0 spot, or a
// spot field that is nil, leaves the existing per-pool value as the fallback rather than
// nulling a price we already have. Pure, so it is unit-testable without a fetcher.
func applySpotPrice(ms *discover.PoolMarketStats, token0 string, spot map[string]discover.TokenSpot) {
	ts, ok := spot[strings.ToLower(strings.TrimSpace(token0))]
	if !ok {
		return
	}
	if ts.PriceUSD != nil {
		ms.PriceUSD = ts.PriceUSD
	}
	if ts.Change24h != nil {
		ms.PriceChange24h = ts.Change24h
	}
}

// upsertTokenMeta upserts the display-only metadata for every token the pool response
// carried (keyed by token address), best-effort: one failure is logged and skipped so
// the rest land. A token with no address is skipped. Returns early on ctx cancellation.
func (s *Stage) upsertTokenMeta(ctx context.Context, pool string, ms discover.PoolMarketStats) {
	for _, t := range ms.Tokens {
		if ctx.Err() != nil {
			return
		}
		if t.Address == "" {
			continue
		}
		if terr := s.db.UpsertTokenMeta(ctx, toStoreTokenMeta(t, ms.SourceURL)); terr != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(terr).Str("pool", pool).Str("token", t.Address).
				Msg("poolstats: token_meta upsert failed, skipping token")
		}
	}
}

// toStoreTokenMeta converts a discover.TokenMeta into a store.TokenMeta for upsert,
// stamping the provenance source URL and fetch time. Display-only.
func toStoreTokenMeta(t discover.TokenMeta, sourceURL string) store.TokenMeta {
	return store.TokenMeta{
		Address:   t.Address,
		Symbol:    t.Symbol,
		LogoURL:   t.LogoURL,
		Decimals:  t.Decimals,
		FetchedAt: time.Now().UTC(),
		SourceURL: sourceURL,
	}
}

// toStorePoolStats converts a discover.PoolMarketStats (float pointers) into a
// store.PoolStats (decimal pointers), preserving nil so a column stays NULL (a missing
// price must not become $0). Display-only.
func toStorePoolStats(ms discover.PoolMarketStats) store.PoolStats {
	return store.PoolStats{
		Pool:           ms.PoolAddr,
		TVLUSD:         floatToDecPtr(ms.TVLUSD),
		Vol24hUSD:      floatToDecPtr(ms.Vol24hUSD),
		PriceUSD:       floatToDecPtr(ms.PriceUSD),
		PriceChange24h: floatToDecPtr(ms.PriceChange24h),
		BaseToken:      ms.BaseToken,
		QuoteToken:     ms.QuoteToken,
		FetchedAt:      time.Now().UTC(),
		SourceURL:      ms.SourceURL,
	}
}

// floatToDecPtr converts an optional float64 into an optional decimal.Decimal,
// nil-preserving. NewFromFloat is exact enough for display values; these never feed
// detection math, so no precision guarantee is required.
func floatToDecPtr(f *float64) *decimal.Decimal {
	if f == nil {
		return nil
	}
	d := decimal.NewFromFloat(*f)
	return &d
}

// TODO(defillama): a second source (DefiLlama yields/protocol TVL) could add APY or
// protocol-level TVL. Not wired: GeckoTerminal already supplies every pool_stats field,
// and a second keyless source spends rate-limit budget for no contract change. If added,
// keep it display-only on the same UpsertPoolStats path, never feeding detection.
