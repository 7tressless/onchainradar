package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
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

// The smart-money detector (signal_type 3, metric "smart_money") flags an
// accumulating actor: a signing EOA (tx.origin) doing repeated large swaps in a
// window, especially across pools (one-off size is the whale detector's job). The
// firing path scores candidates in two passes (ScanOnce), gated on accumulation,
// directionality, and a composite score; a fired signal is keyed and attested by
// its event reference, with the pool as Subject and the actor in the payload.
//
// Why tx.origin, not the log sender: a swap log's sender/recipient are routers and
// aggregators (validated on real Mantle data: ~18 distinct senders across ~1320
// swaps), so the log sender would lump every user behind a router into one "actor".
// eth_getTransactionByHash(tx).From recovers the signing wallet (validation: 30
// swaps -> 11 distinct EOAs with repeat actors visible). The origin is resolved only
// for the bounded set of already-large candidate swaps, cached per tx within a pass.
//
// Directionality (max over token address of |net|/gross, netted by address so
// cross-pool arb cancels) is the anti-churn measure: it is both a score factor and a
// hard floor (SmartMoneyMinDirectionality), so a churning arb/MM bot that buys one
// pool and sells another (net ~= 0) scores ~0 and is excluded outright. Dedup and
// cooldown are per actor, not per pool, so one accumulating actor cannot fire once
// per pool and re-fire as it grows.
const (
	// smartMoneyWarmupSwaps mirrors whaleWarmupSwaps: minimum usable swaps before
	// the pool median is trustworthy (below it the pool is skipped).
	smartMoneyWarmupSwaps = whaleWarmupSwaps

	// smartMoneyRecentWindowMin bounds the firing candidate set to the last N
	// minutes (the baseline still spans the full window), reusing the whale
	// constant so both per-event detectors evaluate the same fresh slice each tick.
	smartMoneyRecentWindowMin = whaleRecentWindowMin

	// smartMoneyResolveTimeout bounds a single tx.origin resolution so one hung RPC
	// node cannot stall the detect loop; a timeout skips that swap (resolveActorCached
	// logs + continues). 15s is generous for one getTransactionByHash.
	smartMoneyResolveTimeout = 15 * time.Second
)

// Score component weights and caps. The score is a weighted mean of independent
// 0..1 components scaled to 0..100 (a mean, not a sum, so a future reputation
// component can be appended without rescaling; see scoreSmartMoney).
const (
	// The four score-factor weights sum to 1.0. Size and accumulation are co-dominant
	// (need both magnitude and repetition); directionality is a strong third (net, not
	// churn); cross-pool breadth is a secondary booster.
	sizeWeight      = 0.30
	accumWeight     = 0.30
	crossPoolWeight = 0.15
	directionWeight = 0.25

	// sizeTargetMultiple is the multiple-of-median at which the size component
	// saturates to 1.0. 20x sits just under the whale gate (25x), so a near-whale
	// accumulator earns full size credit.
	sizeTargetMultiple = 20.0

	// accumCap saturates the accumulation component at 1.0. Credit ramps from the
	// 2nd swap: (accumSwaps-1)/(accumCap-1), so a lone swap earns 0 and 6+ earn full.
	accumCap = 6

	// crossPoolCap saturates the cross-pool component at 1.0. Credit ramps from the
	// 2nd pool: (distinctPools-1)/(crossPoolCap-1), so single-pool earns 0 and 3+ full.
	crossPoolCap = 3
)

// SenderResolver resolves a transaction hash to its signing EOA (tx.origin). The
// real *chain.Client satisfies it via TxSender; tests inject a mock to exercise
// actor attribution without an RPC node. Narrow (one method) on purpose.
type SenderResolver interface {
	TxSender(ctx context.Context, txHash common.Hash) (common.Address, error)
}

// largeSwapInserter is the single store method pass A (ledgerCandidates) needs:
// idempotently recording one candidate in large_swaps. *store.DB satisfies it; a
// mock lets the "ledger every candidate" guarantee be tested with no Postgres.
type largeSwapInserter interface {
	InsertLargeSwap(ctx context.Context, ls store.LargeSwap) (bool, error)
}

// smartFireStore is the set of store reads/writes pass B (fireForActor) needs: the
// per-actor cooldown, the actor's accumulation footprint, its per-token net/gross
// flows (directionality), and the idempotent signal insert. A mock lets the gates
// be tested with no Postgres.
type smartFireStore interface {
	LastSignalTimeByActor(ctx context.Context, actor string, signalType int16) (time.Time, bool, error)
	ActorActivitySince(ctx context.Context, actor string, since time.Time) (int, int, error)
	ActorTokenFlowsSince(ctx context.Context, actor string, since time.Time) ([]store.TokenFlow, error)
	InsertSignal(ctx context.Context, s store.Signal) (int64, bool, error)
}

// SmartMoneyDetector flags accumulating actors (repeated large swaps by one EOA)
// per pool. It holds the store (baseline + ledger + cooldown), the config
// (gates/window/threshold), and a SenderResolver (tx.origin attribution).
type SmartMoneyDetector struct {
	db  *store.DB
	cfg *config.Config
	res SenderResolver
}

// NewSmartMoneyDetector constructs a SmartMoneyDetector. cfg supplies the baseline
// window (BaselineBuckets/BucketMin), the candidate bar (SmartMoneySizeMultiple),
// the accumulation gate (SmartMoneyMinSwaps/WindowHours), the score + churn gates
// (SmartMoneyThreshold/MinDirectionality), and the per-actor cooldown
// (SmartMoneyCooldownMin). res resolves a swap's tx to its signing EOA.
func NewSmartMoneyDetector(db *store.DB, cfg *config.Config, res SenderResolver) *SmartMoneyDetector {
	return &SmartMoneyDetector{db: db, cfg: cfg, res: res}
}

// smCandidate is one decoded recent swap eligible to be a smart-money candidate
// (its size already cleared the per-pool candidate bar). The fields carry through
// to actor resolution, the ledger row, and the signal payload.
type smCandidate struct {
	size      float64         // gross token0 size as float: the median gate and score input
	sizeDec   decimal.Decimal // gross token0 size in exact raw units, stored in the ledger
	size1Dec  decimal.Decimal // gross token1 size in exact raw units, for the display-only size_usd
	net0      decimal.Decimal // actor's signed net token0 change (trader perspective), stored in the ledger
	net1      decimal.Decimal // actor's signed net token1 change (trader perspective), stored in the ledger
	multiple  float64         // size / pool median: the size factor input
	tx        string          // lowercase tx hash
	logIndex  int             // log index within the tx
	blockTime time.Time       // event time (UTC); the signal's BucketTS / hash timestamp
}

// ledgeredCandidate is a selected candidate whose actor (tx.origin) is resolved
// and whose large_swaps row is written (pass A). It carries the candidate + actor
// + pool + that pool's median into the global firing pass (pass B) so pass B
// scores across pools and groups by actor without re-resolving the tx. Candidates
// whose actor could not be resolved are dropped (the swap is skipped).
type ledgeredCandidate struct {
	c       smCandidate
	actor   string           // resolved signing EOA (tx.origin), lowercase hex
	pool    string           // lowercase pool address the swap hit (the signal's Subject)
	median  float64          // the pool's baseline median swap size (size factor denominator)
	sizeUSD *decimal.Decimal // display-only approximate USD size from the pool's stable side; nil when no stable side
}

// smartPayload is the JSON written to a smart-money signal's Payload: the firing
// swap's magnitude, the resolved actor, and the accumulation evidence, so the
// finding is auditable and the event-keyed attestation hash is reproducible.
type smartPayload struct {
	Actor          string  `json:"actor"`
	AccumSwaps     int     `json:"accum_swaps"`
	DistinctPools  int     `json:"distinct_pools"`
	SizeMultiple   float64 `json:"size_multiple"`
	Directionality float64 `json:"directionality"`
	Score          int     `json:"score"`
	WindowHours    int     `json:"window_hours"`
	Size0          float64 `json:"size0"`
	Tx             string  `json:"tx"`
	LogIndex       int     `json:"log_index"`
	BlockTime      string  `json:"block_time"`
}

// ScanOnce evaluates every enabled pool once and persists at most one smart-money
// signal per actor (its highest-scoring recent large swap), provided the actor is
// genuinely accumulating and not in its per-actor cooldown. It returns the inserted
// signals (with ids) for the attest/deliver/enrich pipeline. firedRefs (returned
// alongside) is the set of event_refs this scan fired type-3 on, used by the whale
// detector for same-swap dedup.
//
// Two passes: pass A (ledgerCandidates, per pool) makes the large_swaps ledger
// complete across all pools before any firing decision, so accumulation +
// directionality never undercount; pass B (fireForActor, global) groups the ledger
// by actor and applies the per-actor cooldown + accumulation + directionality +
// score gates. The gory detail lives on those two methods.
//
// A failure returns the already-collected signals plus the wrapped error. ctx
// cancellation aborts promptly; a per-swap actor-resolution failure is non-fatal.
func (s *SmartMoneyDetector) ScanOnce(ctx context.Context) ([]store.Signal, error) {
	out, _, err := s.scanOnce(ctx)
	return out, err
}

// ScanOnceWithRefs is ScanOnce plus the set of event_refs ("txhash:logindex") this
// scan fired a type-3 signal on. The whale detector consumes that set to suppress a
// whale (type 2) on the exact same swap a richer smart-money signal already covers.
// The map is never nil (empty when nothing fired). Splitting this out keeps the
// common caller (ScanOnce) signature unchanged.
func (s *SmartMoneyDetector) ScanOnceWithRefs(ctx context.Context) ([]store.Signal, map[string]struct{}, error) {
	return s.scanOnce(ctx)
}

func (s *SmartMoneyDetector) scanOnce(ctx context.Context) ([]store.Signal, map[string]struct{}, error) {
	firedRefs := make(map[string]struct{})

	pools, err := s.db.ListEnabledPools(ctx)
	if err != nil {
		return nil, firedRefs, fmt.Errorf("detect: smartmoney list enabled pools: %w", err)
	}

	window := time.Duration(s.cfg.BaselineBuckets*s.cfg.BucketMin) * time.Minute
	now := time.Now().UTC()
	since := now.Add(-window)
	recentCut := now.Add(-smartMoneyRecentWindowMin * time.Minute)

	// Per-pass tx.origin cache: a tx can emit several swap logs, and re-resolving
	// the same hash wastes an RPC and could disagree under a flaky node. Bounded by
	// the distinct candidate txs in one scan.
	originCache := make(map[string]common.Address)

	// Token0 market prices for the display-only size_usd on non-stable pools, loaded
	// once. Best-effort: an unavailable snapshot leaves non-stable sizes nil, never a
	// detection input.
	prices, perr := s.db.PoolPricesUSD(ctx)
	if perr != nil {
		log.Warn().Err(perr).Msg("detect: smartmoney pool prices unavailable, sizes fall back to stable-only")
		prices = nil
	}

	// Pass A: build the complete ledger across all pools (with each candidate's pool
	// + median) for the global pass B, where dedup + cooldown are per actor.
	var allLedgered []ledgeredCandidate
	for _, p := range pools {
		if err := ctx.Err(); err != nil {
			return nil, firedRefs, fmt.Errorf("detect: smartmoney context cancelled: %w", err)
		}

		logs, err := s.db.RawLogsSince(ctx, since, []string{p.Address})
		if err != nil {
			return nil, firedRefs, fmt.Errorf("detect: smartmoney raw logs pool=%s: %w", p.Address, err)
		}

		// One decode pass: every usable swap feeds the baseline; recent ones also
		// become candidates. A swap that fails to decode or has size<=0 is left out
		// (like the aggregator's count-only).
		var (
			sizes      []float64
			candidates []smCandidate
		)
		for _, l := range logs {
			if !aggregate.IsSwapTopic(l.Topic0) {
				continue
			}
			d, derr := aggregate.DecodeSwap(l.Topic0, l.Data)
			if derr != nil {
				continue
			}
			size := d.Vol0.InexactFloat64()
			if size <= 0 {
				continue
			}
			sizes = append(sizes, size)
			if !l.BlockTime.Before(recentCut) {
				candidates = append(candidates, smCandidate{
					size:      size,
					sizeDec:   d.Vol0,     // exact raw units for the ledger (float is lossy)
					size1Dec:  d.Vol1,     // exact gross token1 for the display-only size_usd
					net0:      d.Netflow0, // signed: positive = actor received token0
					net1:      d.Netflow1, // signed: positive = actor received token1
					tx:        strings.ToLower(l.TxHash),
					logIndex:  l.LogIndex,
					blockTime: l.BlockTime.UTC(),
				})
			}
		}

		// Unstable baseline: too few usable swaps for a trustworthy median.
		if len(sizes) < smartMoneyWarmupSwaps {
			log.Debug().
				Str("pool", p.Address).
				Int("usable_swaps", len(sizes)).
				Int("warmup", smartMoneyWarmupSwaps).
				Msg("detect: smartmoney pool below warmup, skipping")
			continue
		}

		median, _ := MedianAndMAD(sizes)
		// The candidate bar: a per-pool override may exist later; for now use the
		// global multiple (read pool fields if present in a future migration).
		threshold := effectiveSmartMoneySizeMultiple(s.cfg, p) * median

		// pickSmartCandidates is the pure size selection: which recent swaps clear
		// the candidate bar (the RPC + accumulation work runs only for these).
		selected := pickSmartCandidates(median, candidates, threshold)

		// Ledger every selected candidate (resolve actor, write large_swaps row) and
		// carry it, with its pool + median, into the global pass B. A candidate whose
		// actor cannot be resolved is skipped (its swap is lost, the pass continues);
		// only ctx cancellation or a real DB error aborts.
		ledgered, lerr := s.ledgerCandidates(ctx, s.db, originCache, p, median, poolPrice(prices, p.Address), selected)
		if lerr != nil {
			return nil, firedRefs, lerr
		}
		allLedgered = append(allLedgered, ledgered...)
	}

	// Pass B: global, per-actor. Group the complete ledger by actor and fire at
	// most one signal per actor (its highest-multiple candidate), applying the
	// per-actor cooldown + accumulation + directionality + score gates.
	accumSince := now.Add(-time.Duration(s.cfg.SmartMoneyWindowHours) * time.Hour)
	cooldown := time.Duration(s.cfg.SmartMoneyCooldownMin) * time.Minute

	var out []store.Signal
	for _, actor := range sortedActors(allLedgered) {
		if err := ctx.Err(); err != nil {
			return out, firedRefs, fmt.Errorf("detect: smartmoney context cancelled: %w", err)
		}
		best, ok := bestCandidateForActor(allLedgered, actor)
		if !ok {
			continue // unreachable: sortedActors only yields actors that have a candidate
		}

		sig, fired, ferr := s.fireForActor(ctx, s.db, actor, best, accumSince, cooldown)
		if ferr != nil {
			return out, firedRefs, ferr
		}
		if !fired {
			continue
		}
		out = append(out, sig)
		firedRefs[sig.EventRef] = struct{}{}
	}

	return out, firedRefs, nil
}

// fireForActor decides and (on success) persists the single smart-money signal for
// one actor's best candidate. It returns (signal, true, nil) on a fresh insert,
// (zero, false, nil) when the actor was gated out or already recorded, and
// (zero, false, err) on a real DB failure (which aborts the scan). The gates, in
// order: per-actor cooldown, accumulation floor, directionality floor, score gate.
// Each DB read is bounded and ctx-aware.
func (s *SmartMoneyDetector) fireForActor(
	ctx context.Context,
	st smartFireStore,
	actor string,
	best ledgeredCandidate,
	accumSince time.Time,
	cooldown time.Duration,
) (store.Signal, bool, error) {
	// Per-actor cooldown: suppress an actor that fired a type-3 recently (any pool).
	// DB-backed (signals.actor) so it survives restarts; non-positive disables it.
	// Done first so a cooling-down actor costs one cheap lookup.
	if cooldown > 0 {
		last, ok, lerr := st.LastSignalTimeByActor(ctx, actor, store.SignalTypeSmartMoney)
		if lerr != nil {
			return store.Signal{}, false, fmt.Errorf("detect: smartmoney last signal time actor=%s: %w", actor, lerr)
		}
		if ok && time.Since(last) < cooldown {
			log.Debug().
				Str("actor", actor).
				Dur("cooldown", cooldown).
				Msg("detect: smartmoney actor in cooldown, skipping")
			return store.Signal{}, false, nil
		}
	}

	// Accumulation: this actor's large swaps over the window (reads the complete
	// ledger pass A built).
	accumSwaps, distinctPools, aerr := st.ActorActivitySince(ctx, actor, accumSince)
	if aerr != nil {
		return store.Signal{}, false, fmt.Errorf("detect: smartmoney actor activity actor=%s: %w", actor, aerr)
	}
	if accumSwaps < s.cfg.SmartMoneyMinSwaps {
		// Not accumulation (that is the whale signal's job). The ledger rows stay so
		// a later swap can cross the floor.
		log.Debug().
			Str("actor", actor).
			Int("accum_swaps", accumSwaps).
			Int("min_swaps", s.cfg.SmartMoneyMinSwaps).
			Msg("detect: smartmoney actor below accumulation floor, skipping")
		return store.Signal{}, false, nil
	}

	// Directionality (the misclassification fix): genuine net accumulation vs churn.
	flows, ferr := st.ActorTokenFlowsSince(ctx, actor, accumSince)
	if ferr != nil {
		return store.Signal{}, false, fmt.Errorf("detect: smartmoney actor token flows actor=%s: %w", actor, ferr)
	}
	directionality := actorDirectionality(flows)
	if directionality < s.cfg.SmartMoneyMinDirectionality {
		// A churning arb/MM bot (buys and sells cancel, net ~= 0 on every asset) is
		// not accumulating. Exclude outright, else the LLM confabulates an "informed
		// accumulation" narrative about it.
		log.Debug().
			Str("actor", actor).
			Float64("directionality", directionality).
			Float64("min_directionality", s.cfg.SmartMoneyMinDirectionality).
			Msg("detect: smartmoney actor below directionality floor (churn), skipping")
		return store.Signal{}, false, nil
	}

	// Composite score gate (now includes directionality as a factor).
	c := best.c
	multiple := candidateMultiple(best)
	score := scoreSmartMoney(multiple, accumSwaps, distinctPools, directionality)
	if float64(score) < s.cfg.SmartMoneyThreshold {
		log.Debug().
			Str("actor", actor).
			Int("score", score).
			Float64("threshold", s.cfg.SmartMoneyThreshold).
			Msg("detect: smartmoney score below threshold, skipping")
		return store.Signal{}, false, nil
	}

	eventRef := c.tx + ":" + strconv.Itoa(c.logIndex)
	sig := store.Signal{
		Pool:       best.pool, // subject stays the pool (actor is in the payload + actor column)
		SignalType: store.SignalTypeSmartMoney,
		Metric:     "smart_money",
		// Zscore carries the 0..100 score so the on-chain score (ScoreFromZ) is
		// score*100, consistent with how types 1/2 map their z.
		Zscore:        float64(score),
		WindowBuckets: accumSwaps, // record the actor's accumulation depth this window
		BucketTS:      c.blockTime,
		EventRef:      eventRef,
		Actor:         actor,        // powers the per-actor cooldown on the next scan
		SizeUSD:       best.sizeUSD, // display-only; nil when no stable side
		Status:        store.StatusNew,
	}

	payload, perr := json.Marshal(smartPayload{
		Actor:          actor,
		AccumSwaps:     accumSwaps,
		DistinctPools:  distinctPools,
		SizeMultiple:   multiple,
		Directionality: directionality,
		Score:          score,
		WindowHours:    s.cfg.SmartMoneyWindowHours,
		Size0:          c.size,
		Tx:             c.tx,
		LogIndex:       c.logIndex,
		BlockTime:      c.blockTime.Format(time.RFC3339),
	})
	if perr != nil {
		return store.Signal{}, false, fmt.Errorf("detect: smartmoney marshal payload actor=%s ref=%s: %w", actor, eventRef, perr)
	}
	sig.Payload = payload

	id, inserted, serr := st.InsertSignal(ctx, sig)
	if serr != nil {
		return store.Signal{}, false, fmt.Errorf("detect: smartmoney insert signal actor=%s ref=%s: %w", actor, eventRef, serr)
	}
	if !inserted {
		// Recorded on an earlier overlapping tick: the (pool, signal_type,
		// event_ref) key makes re-scanning idempotent. Report not-fired.
		log.Debug().
			Str("pool", best.pool).
			Str("event_ref", eventRef).
			Msg("detect: smartmoney signal already recorded, skipping")
		return store.Signal{}, false, nil
	}
	sig.ID = id

	log.Info().
		Str("pool", best.pool).
		Str("actor", actor).
		Float64("size", c.size).
		Float64("multiple", multiple).
		Int("accum_swaps", accumSwaps).
		Int("distinct_pools", distinctPools).
		Float64("directionality", directionality).
		Int("score", score).
		Str("event_ref", eventRef).
		Int64("signal_id", id).
		Msg("detect: smartmoney signal emitted")

	return sig, true, nil
}

// sortedActors returns the distinct actors present in the ledgered candidates, in
// deterministic (lexicographic) order, so the per-actor firing pass is reproducible
// (map iteration order is not). Bounded by the number of distinct actors in one
// scan.
func sortedActors(ledgered []ledgeredCandidate) []string {
	seen := make(map[string]struct{}, len(ledgered))
	for i := range ledgered {
		seen[ledgered[i].actor] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// bestCandidateForActor returns one actor's highest-multiple ledgered candidate,
// which maximizes its composite score (accumulation, directionality, and cross-pool
// breadth are actor-level constants, so size is the only intra-actor differentiator).
// Ties keep the earliest. The bool is false when the actor has no candidate. Pure.
func bestCandidateForActor(ledgered []ledgeredCandidate, actor string) (ledgeredCandidate, bool) {
	var (
		best  ledgeredCandidate
		found bool
	)
	for i := range ledgered {
		lc := ledgered[i]
		if lc.actor != actor {
			continue
		}
		if !found || candidateMultiple(lc) > candidateMultiple(best) {
			best = lc
			found = true
		}
	}
	return best, found
}

// candidateMultiple is a ledgered candidate's size-multiple (size / pool median),
// the intra-actor ranking key. A non-positive median (impossible here, the
// baseline is all positive sizes) yields 0 so it never divides by zero.
func candidateMultiple(lc ledgeredCandidate) float64 {
	if lc.median <= 0 {
		return 0
	}
	return lc.c.size / lc.median
}

// ledgerCandidates is ScanOnce's pass A for one pool: resolve every selected
// candidate's actor (tx.origin, cached per tx) and write its large_swaps row,
// returning the resolved + ledgered candidates (oldest-first) for the firing pass.
// Each carries its pool + median so pass B computes the size factor without
// re-reading the pool.
//
// The ledger row records the actor's signed net token0/token1 change for the
// directionality computation; rows from before migration 0008 stay NULL and the
// store reads them COALESCEd to 0. A candidate whose actor cannot be resolved is
// dropped (one unresolvable tx must not lose the rest of the pool). It errors only
// on ctx cancellation or a real DB write failure (both abort the scan).
// InsertLargeSwap is idempotent on (tx_hash, log_index).
func (s *SmartMoneyDetector) ledgerCandidates(
	ctx context.Context,
	ins largeSwapInserter,
	originCache map[string]common.Address,
	p store.Pool,
	median float64,
	price0 *decimal.Decimal,
	selected []smCandidate,
) ([]ledgeredCandidate, error) {
	ledgered := make([]ledgeredCandidate, 0, len(selected))
	for i := range selected {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("detect: smartmoney context cancelled: %w", err)
		}
		c := selected[i]

		// Resolve actor = tx.origin (cached per tx). A resolve failure skips this
		// swap; abort only on parent ctx cancellation (a node hang is bounded by the
		// per-call timeout and does not cancel parent).
		actor, ok := resolveActorCached(ctx, s.res, originCache, c.tx)
		if !ok {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("detect: smartmoney context cancelled: %w", ctx.Err())
			}
			continue
		}
		actorHex := strings.ToLower(actor.Hex())

		// Record the candidate in large_swaps (idempotent) so the actor's history
		// accrues across ticks. A duplicate is fine; only a real DB error aborts.
		if _, ierr := ins.InsertLargeSwap(ctx, store.LargeSwap{
			TxHash:     c.tx,
			LogIndex:   c.logIndex,
			Pool:       p.Address,
			Actor:      actorHex,
			SizeToken0: c.sizeDec,
			Net0:       c.net0,
			Net1:       c.net1,
			BlockTime:  c.blockTime,
		}); ierr != nil {
			return nil, fmt.Errorf("detect: smartmoney insert large_swap pool=%s tx=%s: %w", p.Address, c.tx, ierr)
		}

		ledgered = append(ledgered, ledgeredCandidate{
			c:      c,
			actor:  actorHex,
			pool:   p.Address,
			median: median,
			// Display-only USD size, computed here because pass A holds the full pool
			// (tokens + decimals): the stable side when there is one, else the token0 leg
			// at its market price; nil when neither is available.
			sizeUSD: swapSizeUSD(p, c.sizeDec, c.size1Dec, price0),
		})
	}
	return ledgered, nil
}

// effectiveSmartMoneySizeMultiple returns the candidate bar (multiple-of-median)
// a pool's swaps are admitted against: today always the global config value, kept
// as a seam for a future per-pool override (mirroring effectiveWhaleMinMultiple).
func effectiveSmartMoneySizeMultiple(cfg *config.Config, _ store.Pool) float64 {
	return cfg.SmartMoneySizeMultiple
}

// resolveActorCached resolves a swap's tx to its signing EOA (tx.origin),
// memoizing in cache so a tx with several swap logs resolves once. Shared by the
// smart-money (skip the swap) and whale (empty actor, still fire) detectors; a
// false bool means "no attribution, continue". The error is logged, not returned
// (a per-swap resolve failure is non-fatal).
//
// The RPC is bounded by a per-call timeout (smartMoneyResolveTimeout) derived from
// parent: a hung node fails that one resolution without cancelling parent, so the
// caller's parent.Err() still distinguishes a node hang (continue) from a real
// shutdown (abort). The child context is always cancelled before returning.
func resolveActorCached(
	ctx context.Context,
	res SenderResolver,
	cache map[string]common.Address,
	tx string,
) (common.Address, bool) {
	if a, ok := cache[tx]; ok {
		return a, true
	}
	rctx, cancel := context.WithTimeout(ctx, smartMoneyResolveTimeout)
	addr, err := res.TxSender(rctx, common.HexToHash(tx))
	cancel()
	if err != nil {
		// Message stays detector-agnostic so it is accurate for both callers.
		log.Warn().Err(err).Str("tx", tx).Msg("detect: resolve actor (tx.origin) failed; skipping attribution")
		return common.Address{}, false
	}
	cache[tx] = addr
	return addr, true
}

// pickSmartCandidates returns the recent candidates that clear the absolute size
// bar (multiple*median): the bounded set for which ScanOnce does the RPC-bearing
// actor resolution. Pure (no I/O, no mutation). A non-positive median or threshold
// yields nothing (a non-positive bar would admit everything); order is preserved.
func pickSmartCandidates(median float64, candidates []smCandidate, threshold float64) []smCandidate {
	if median <= 0 || threshold <= 0 {
		return nil
	}
	var out []smCandidate
	for _, c := range candidates {
		if c.size <= 0 || c.size < threshold {
			continue
		}
		c.multiple = c.size / median
		out = append(out, c)
	}
	return out
}

// scoreSmartMoney is the pure multi-factor smart-money score in [0,100]: a
// weighted mean of four independent 0..1 components, scaled to 0..100 and rounded.
//
//   - size:        clamp(sizeMultiple / sizeTargetMultiple, 0, 1)       weight sizeWeight
//   - accumulate:  clamp((accumSwaps-1) / (accumCap-1), 0, 1)           weight accumWeight
//   - crossPool:   clamp((distinctPools-1) / (crossPoolCap-1), 0, 1)    weight crossPoolWeight
//   - direction:   clamp(directionality, 0, 1)                          weight directionWeight
//
// directionality (see actorDirectionality) enters as a factor so a churner scores
// low even before ScanOnce applies SmartMoneyMinDirectionality as a hard gate. The
// score is monotonic non-decreasing in each input and clamped to [0,100].
func scoreSmartMoney(sizeMultiple float64, accumSwaps, distinctPools int, directionality float64) int {
	sizeComp := clamp01(sizeMultiple / sizeTargetMultiple)

	// Ramp accumulation/cross-pool from their 2nd unit so a single swap / single
	// pool earns 0 credit (denominators are compile-time >= 1; see the consts).
	accumComp := clamp01(float64(accumSwaps-1) / float64(accumCap-1))
	crossComp := clamp01(float64(distinctPools-1) / float64(crossPoolCap-1))
	// directionality is already in [0,1] (a ratio of magnitudes); clamp defends a
	// NaN/Inf or out-of-range value from a degenerate caller.
	dirComp := clamp01(directionality)

	mean := weightedMean(
		[]float64{sizeWeight, accumWeight, crossPoolWeight, directionWeight},
		[]float64{sizeComp, accumComp, crossComp, dirComp},
	)

	score := int(mean*100 + 0.5) // round half up; mean in [0,1] so score in [0,100]
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

// actorDirectionality is the pure net-vs-churn measure of an actor's window of
// large swaps: the maximum over token addresses of |net_T| / gross_T, where net_T
// is the actor's signed net change of token T (positive = received) and gross_T is
// the sum of the absolute per-leg contributions. It is in [0,1]:
//
//   - a pure accumulator (only buys some asset T) has |net_T| == gross_T -> 1.0;
//   - a churning arb/MM bot (buys then sells T, possibly across pools) has its legs
//     cancel in net_T while gross_T stays large -> ~0.
//
// A token with gross_T <= 0 or a non-finite ratio is skipped; empty input yields 0
// (which the hard floor excludes). Netting by token address (upstream in the store
// query) is what makes cross-pool arbitrage in one asset cancel.
func actorDirectionality(flows []store.TokenFlow) float64 {
	best := 0.0
	for _, f := range flows {
		gross := f.Gross.InexactFloat64()
		if !isFinite(gross) || gross <= 0 {
			// No gross flow (or degenerate): skip to avoid a divide-by-zero.
			continue
		}
		net := f.Net.InexactFloat64()
		if !isFinite(net) {
			continue
		}
		ratio := math.Abs(net) / gross
		if !isFinite(ratio) {
			continue
		}
		if ratio > best {
			best = ratio
		}
	}
	return clamp01(best)
}

// weightedMean returns sum(weights[i]*values[i]) / sum(weights), or 0 when the
// weights sum to <= 0 (defensive; the smart-money weights are positive consts).
func weightedMean(weights, values []float64) float64 {
	var num, den float64
	for i := range weights {
		num += weights[i] * values[i]
		den += weights[i]
	}
	if den <= 0 {
		return 0
	}
	return num / den
}

// clamp01 clamps x to the closed interval [0,1].
func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
