// Package poolstats refreshes the display-only market snapshot for each enabled pool
// (TVL, 24h volume, spot price) into pool_stats, on an interval.
//
// This data is for human display only: never read by the detector, aggregator, or any
// scoring/baseline path (detection runs purely on raw on-chain buckets). It lives in its
// own table so the figures cannot be mistaken for a detection input.
//
// The snapshot is sourced first-party (on-chain reserves + spot price, plus 24h volume
// from OCR's own buckets; see internal/marketdata), so there is no external API key or
// rate limit. A field that could not be read stays NULL, leaving the previous value on the
// dashboard until the next pass refreshes it. The loop self-disables on a non-positive
// interval.
package poolstats

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"ocr/internal/marketdata"
	"ocr/internal/store"
)

// MarketSource produces the display-only market snapshot for a set of pools, keyed by
// lowercase pool address. *marketdata.Reader satisfies it; a mock satisfies it in tests
// with no chain or DB.
type MarketSource interface {
	Snapshot(ctx context.Context, pools []store.Pool) (map[string]marketdata.PoolMarketStats, error)
}

// poolStore is the slice of *store.DB the stage uses: list the enabled pools and upsert
// each pool's snapshot. Isolated for testability.
type poolStore interface {
	ListEnabledPools(ctx context.Context) ([]store.Pool, error)
	UpsertPoolStats(ctx context.Context, ps store.PoolStats) error
}

// Stage refreshes pool_stats on an interval. Construct with New; run with Run.
type Stage struct {
	db       poolStore
	market   MarketSource
	interval time.Duration
}

// New constructs the poolstats stage over a store and a market source, refreshing every
// `interval`. A non-positive interval makes Run a no-op (the stage is disabled); the
// supervisor gates on that.
func New(db poolStore, market MarketSource, interval time.Duration) *Stage {
	return &Stage{db: db, market: market, interval: interval}
}

// Run refreshes every enabled pool's snapshot once at startup, then every interval, until
// ctx is cancelled. A non-positive interval disables the stage (logs once, returns nil). A
// pass or per-pool failure is logged and the loop continues; only ctx cancellation ends it.
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

// RunOnce refreshes every enabled pool's snapshot exactly once and returns. Unlike Run it
// ignores the disabled gate (interval <= 0): the one-shot CLI (`ocr poolstats`) always
// refreshes once. Returns ctx.Err() only on cancellation.
func (s *Stage) RunOnce(ctx context.Context) error {
	s.runPass(ctx)
	return ctx.Err()
}

// runPass lists the enabled pools, computes the whole snapshot once (one batched on-chain
// read + per-pool bucket sums), and upserts each pool's row. Best-effort: the snapshot
// itself omits unreadable fields per pool (leaving them NULL); a per-pool upsert failure
// is logged and the pass continues. It stops promptly on ctx cancellation.
func (s *Stage) runPass(ctx context.Context) {
	pools, err := s.db.ListEnabledPools(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error().Err(err).Msg("poolstats: list enabled pools failed, will retry next tick")
		return
	}
	if len(pools) == 0 {
		return
	}

	snap, err := s.market.Snapshot(ctx, pools)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error().Err(err).Msg("poolstats: snapshot failed, will retry next tick")
		return
	}

	refreshed := 0
	for i := range pools {
		if ctx.Err() != nil {
			return
		}
		addr := strings.ToLower(pools[i].Address)
		ms, ok := snap[addr]
		if !ok {
			continue // pool absent from the snapshot (defensive; Snapshot returns every pool)
		}
		if uerr := s.db.UpsertPoolStats(ctx, toStorePoolStats(ms)); uerr != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error().Err(uerr).Str("pool", addr).Msg("poolstats: upsert failed, skipping pool")
			continue
		}
		refreshed++
		log.Debug().Str("pool", addr).Msg("poolstats: snapshot refreshed")
	}

	if refreshed > 0 {
		log.Info().Int("refreshed", refreshed).Int("pools", len(pools)).Msg("poolstats: pass complete")
	}
}

// toStorePoolStats converts a marketdata.PoolMarketStats (float pointers) into a
// store.PoolStats (decimal pointers), nil-preserving so a missing field stays NULL (a
// missing price must not become $0). Display-only.
func toStorePoolStats(ms marketdata.PoolMarketStats) store.PoolStats {
	return store.PoolStats{
		Pool:           ms.PoolAddr,
		TVLUSD:         floatToDecPtr(ms.TVLUSD),
		Vol24hUSD:      floatToDecPtr(ms.Vol24hUSD),
		PriceUSD:       floatToDecPtr(ms.PriceUSD),
		PriceChange24h: floatToDecPtr(ms.PriceChange24h),
		FetchedAt:      time.Now().UTC(),
		SourceURL:      ms.SourceURL,
	}
}

// floatToDecPtr converts an optional float64 into an optional decimal.Decimal, nil-
// preserving. NewFromFloat is exact enough for display values; these never feed detection
// math, so no precision guarantee is required.
func floatToDecPtr(f *float64) *decimal.Decimal {
	if f == nil {
		return nil
	}
	d := decimal.NewFromFloat(*f)
	return &d
}
