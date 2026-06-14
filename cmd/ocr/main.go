// Command ocr is the single OnChain Radar binary. It dispatches on its first CLI argument
// into one pipeline stage or a supervisor that runs them together:
//
//	ocr collect     poll Mantle for pool logs into raw_logs
//	ocr aggregate   roll raw_logs into per-pool time buckets
//	ocr detect      score buckets, enrich + attest + alert each new signal
//	ocr serve       serve the read-only dashboard
//	ocr run         supervise collect + aggregate + detect (+ serve) together
//
// All stages share one config (config.Load) and one Postgres pool (store.New).
// SIGINT/SIGTERM cancel a root context that every long-running stage honors for graceful
// shutdown. The full command reference is in usage.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"ocr/internal/aggregate"
	"ocr/internal/api"
	"ocr/internal/attest"
	"ocr/internal/chain"
	"ocr/internal/config"
	"ocr/internal/deliver"
	"ocr/internal/detect"
	"ocr/internal/discover"
	"ocr/internal/enrich"
	"ocr/internal/marketdata"
	"ocr/internal/outcome"
	"ocr/internal/poolstats"
	"ocr/internal/store"
)

const (
	// mantleScanBase is the explorer root used to build address/tx links in
	// dashboard rows and Telegram alerts.
	mantleScanBase = "https://mantlescan.xyz"

	// dashboardURL is the public dashboard link surfaced as a button on every Telegram
	// alert, so a channel reader can jump from a signal to the live feed.
	dashboardURL = "https://onchainradar.tech"

	// poolsConfigPath is the curated pool registry seeded into Postgres before
	// the collector starts.
	poolsConfigPath = "config/pools.yaml"

	// detectInterval is how often the detect stage rescans every pool.
	detectInterval = 30 * time.Second

	// enrichInterval is how often the async enrich stage looks for note-less signals.
	// 20s keeps notes timely without hammering the LLM.
	enrichInterval = 20 * time.Second

	// enrichBatch bounds how many note-less signals one enrich pass processes, so a large
	// backlog drains over several ticks and one slow LLM call cannot stall the loop.
	enrichBatch = 50

	// aggregateInterval is how often the aggregate stage rolls new raw logs up.
	aggregateInterval = 60 * time.Second

	// rawLogsRetentionFactor multiplies the baseline window to set raw_logs retention.
	// Buckets are already materialized, so the factor of 2 keeps a safety margin (no live
	// aggregation or replay can need a pruned row) while still bounding the table.
	rawLogsRetentionFactor = 2

	// watchdogInterval is how often the silent-stall watchdog checks pipeline freshness:
	// one cheap query per tick. The stall threshold is STALL_ALERT_MIN.
	watchdogInterval = 5 * time.Minute

	// stallReAlertInterval throttles repeat stall alerts: once on the first crossing, then
	// at most hourly until freshness recovers, so a sustained outage does not spam.
	stallReAlertInterval = 1 * time.Hour

	// outcomeInterval is how often the outcome stage measures matured signals. Outcomes
	// mature over hours and the pass is idempotent, so a slow cadence only delays a
	// verdict, never loses one.
	outcomeInterval = 1 * time.Hour

	// attestPendingInterval is how often the recovery stage re-attests signals whose
	// attest_tx is still empty. See attestPendingOnce for the self-heal contract.
	attestPendingInterval = 2 * time.Minute

	// attestPendingScan bounds how many recent signals the recovery stage inspects per
	// tick (newest first). A scan window, not an attest cap: already-attested rows are
	// skipped cheaply, so it only needs to cover the freshly failed tail.
	attestPendingScan = 500

	// attestWriteAttempts and attestWriteBackoff bound the retry on the DB write that
	// records an already-broadcast attest tx hash (see persistAttestTx).
	attestWriteAttempts = 3
	attestWriteBackoff  = 200 * time.Millisecond

	// deliverTimeout bounds the synchronous Telegram send on the detect hot path. Dispatch
	// retries up to 5 times with a 20s HTTP client, so without this a black-holing endpoint
	// could block detection/attestation for ~2 min per signal. The Telegram path is fully
	// ctx-aware, so this deadline cuts both the in-flight round-trip and the remaining
	// retries, capping the worst case at ~8s.
	deliverTimeout = 8 * time.Second

	// apiAddr is the default bind for the serve stage and the supervisor's serve goroutine.
	// It binds all interfaces so the API/dashboard can be a public no-wallet read-only URL;
	// local dev can override with API_ADDR=127.0.0.1:7070. Legacy DASHBOARD_ADDR is a fallback.
	apiAddr = "0.0.0.0:7070"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	// Root context cancelled on the first SIGINT/SIGTERM so every stage drains.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd := os.Args[1]
	var err error
	switch cmd {
	case "collect":
		err = runCollect(ctx)
	case "backfill":
		err = runBackfill(ctx)
	case "aggregate":
		err = runAggregate(ctx)
	case "detect":
		err = runDetect(ctx)
	case "enrich":
		err = runEnrich(ctx)
	case "attest-pending":
		err = runAttestPending(ctx)
	case "smartmoney":
		err = runSmartMoney(ctx)
	case "lending":
		err = runLending(ctx)
	case "lstflow":
		err = runLSTFlow(ctx)
	case "depeg":
		err = runDepeg(ctx)
	case "outcomes":
		err = runOutcomes(ctx)
	case "discover":
		err = runDiscover(ctx)
	case "poolmeta":
		err = runPoolMeta(ctx)
	case "poolstats":
		err = runPoolStats(ctx)
	case "serve":
		err = runServe(ctx)
	case "run":
		err = runSupervisor(ctx)
	case "-h", "--help", "help":
		usage()
		return
	default:
		log.Error().Str("command", cmd).Msg("ocr: unknown command")
		usage()
		os.Exit(1)
	}

	if err != nil {
		// A clean shutdown surfaces as context.Canceled; that is not a failure.
		if errors.Is(err, context.Canceled) {
			log.Info().Str("command", cmd).Msg("ocr: shutdown complete")
			return
		}
		log.Error().Err(err).Str("command", cmd).Msg("ocr: fatal")
		os.Exit(1)
	}
}

// usage prints the subcommand reference to stderr.
func usage() {
	fmt.Fprintf(os.Stderr, `ocr: OnChain Radar (Mantle)

Usage:
  ocr <command>

Commands:
  collect     Poll Mantle for pool logs into raw_logs (seeds the pool registry).
  backfill    Backfill the last N hours (default 48) of pool logs, then exit.
  aggregate   Roll raw_logs into per-pool time buckets.
  detect      Score buckets; enrich, attest, and alert each new signal.
              Optional mode: "once" (newest bucket) or "replay" (full history).
  enrich      Write LLM analyst notes for signals that lack one, then exit.
  smartmoney  Scan once for smart-money accumulation signals (type 3), then exit.
  lending     Scan once for Aave V3 liquidation (type 5) + big-borrow (type 6)
              signals, then exit.
  lstflow     Scan once for large mETH/cmETH LST-flow (type 4) signals, then exit.
  depeg       Scan once for stablecoin depeg + contagion (type 7) signals, then exit.
  attest-pending  Attest every signal that lacks an on-chain attestation, then exit.
  outcomes    Grade the outcome report card for every matured signal, then exit.
  discover    Discover new Mantle anchor pools, verify each on-chain, and propose
              them as disabled registry rows, then exit. Add --auto to enable only
              fully-verified proposals automatically (default proposes disabled).
  poolmeta    Backfill each enabled Merchant Moe pool's on-chain bin step
              (getBinStep) into pools.bin_step for the API add-liquidity link, then exit.
  poolstats   Refresh the external pool market stats once (display only), then exit.
  serve       Serve the read-only JSON API + SSE live feed on %s (override API_ADDR).
  run         Supervise collect + aggregate + detect + outcome + poolstats + serve.
  help        Show this message.
`, apiAddr)
}

// runCollect seeds the curated pool registry, dials Mantle, and runs the
// log collector until the context is cancelled.
func runCollect(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("collect: load config: %w", err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("collect: open store: %w", err)
	}
	defer db.Close()

	pools, err := seedPools(ctx, db)
	if err != nil {
		return fmt.Errorf("collect: seed pools: %w", err)
	}

	client, err := chain.Dial(ctx, cfg.RPCURL, cfg.ChainID)
	if err != nil {
		return fmt.Errorf("collect: dial chain: %w", err)
	}
	defer client.Close()

	if err := chain.NewCollector(client, db, cfg, pools).Run(ctx); err != nil {
		return fmt.Errorf("collect: collector run: %w", err)
	}
	return nil
}

// Mantle produces ~1 block every 2 seconds (1800 blocks/hour, measured
// 2026-06-06), so an N-hour backfill rewinds N*mantleBlocksPerHour blocks.
const (
	mantleBlocksPerHour  uint64 = 1800
	defaultBackfillHours uint64 = 48
)

// runBackfill rewinds the cursor by N hours of blocks and runs a single catch-up
// scan, then exits. It gives the detector a full baseline immediately instead of
// waiting for live blocks. N comes from the optional second CLI arg
// (`ocr backfill 72`); it defaults to defaultBackfillHours.
func runBackfill(ctx context.Context) error {
	hours := defaultBackfillHours
	if len(os.Args) > 2 {
		h, err := strconv.ParseUint(os.Args[2], 10, 64)
		if err != nil || h == 0 {
			return fmt.Errorf("backfill: invalid hours %q (want a positive integer)", os.Args[2])
		}
		hours = h
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("backfill: load config: %w", err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("backfill: open store: %w", err)
	}
	defer db.Close()

	pools, err := seedPools(ctx, db)
	if err != nil {
		return fmt.Errorf("backfill: seed pools: %w", err)
	}

	client, err := chain.Dial(ctx, cfg.RPCURL, cfg.ChainID)
	if err != nil {
		return fmt.Errorf("backfill: dial chain: %w", err)
	}
	defer client.Close()

	lookback := hours * mantleBlocksPerHour
	if err := chain.NewCollector(client, db, cfg, pools).Backfill(ctx, lookback); err != nil {
		return fmt.Errorf("backfill: run: %w", err)
	}
	log.Info().Uint64("hours", hours).Msg("ocr: backfill finished")
	return nil
}

// runAggregate runs the bucket aggregator until the context is cancelled.
func runAggregate(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("aggregate: load config: %w", err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("aggregate: open store: %w", err)
	}
	defer db.Close()

	agg := aggregate.New(db, cfg.BucketMin, rawLogsRetention(cfg))
	// `ocr aggregate once` runs a single sweep and exits (used after a backfill).
	if len(os.Args) > 2 && os.Args[2] == "once" {
		if err := agg.RunOnce(ctx); err != nil {
			return fmt.Errorf("aggregate: run once: %w", err)
		}
		log.Info().Msg("ocr: aggregate one-shot complete")
		return nil
	}
	if err := agg.Run(ctx, aggregateInterval); err != nil {
		return fmt.Errorf("aggregate: run: %w", err)
	}
	return nil
}

// runDetect dials Mantle (for attestation), constructs the detector, and runs
// the detect→enrich→attest→deliver loop until the context is cancelled.
func runDetect(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("detect: load config: %w", err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("detect: open store: %w", err)
	}
	defer db.Close()

	client, err := chain.Dial(ctx, cfg.RPCURL, cfg.ChainID)
	if err != nil {
		return fmt.Errorf("detect: dial chain: %w", err)
	}
	defer client.Close()

	mode := ""
	if len(os.Args) > 2 {
		mode = os.Args[2]
	}
	if mode != "" && mode != "once" && mode != "replay" {
		return fmt.Errorf("detect: unknown mode %q (want \"once\" or \"replay\")", mode)
	}

	// One NonceManager for this process's agent wallet (this standalone command
	// only signals from the signal attestor, so a single manager is sufficient).
	nonces := attest.NewNonceManager()
	attestor, err := newSignalAttestor(ctx, cfg, client, nonces)
	if err != nil {
		return fmt.Errorf("detect: %w", err)
	}

	if err := detectLoop(ctx, cfg, db, client, attestor, mode); err != nil {
		return fmt.Errorf("detect: loop: %w", err)
	}
	return nil
}

// apiBind returns the API/dashboard listen address: API_ADDR if set, else the
// legacy DASHBOARD_ADDR (for existing deploy configs), else the all-interfaces
// default. A public deploy keeps the default 0.0.0.0:7070; local dev can set
// API_ADDR=127.0.0.1:7070 and reach it via an SSH tunnel.
func apiBind() string {
	if v := os.Getenv("API_ADDR"); v != "" {
		return v
	}
	if v := os.Getenv("DASHBOARD_ADDR"); v != "" {
		return v
	}
	return apiAddr
}

// rawLogsRetention is how long the aggregate stage keeps raw_logs before pruning
// the aged tail: the baseline window (BaselineBuckets buckets of BucketMin minutes)
// times rawLogsRetentionFactor. Buckets are already materialized, so older raw rows
// are dead weight; the factor keeps a margin so no live aggregation can still need a
// pruned row. Validate already guarantees both inputs are positive, so the result is
// always > 0 (pruning enabled).
func rawLogsRetention(cfg *config.Config) time.Duration {
	windowMin := cfg.BaselineBuckets * cfg.BucketMin * rawLogsRetentionFactor
	return time.Duration(windowMin) * time.Minute
}

// newAPIServer builds the read-only JSON API + SSE server over the shared store,
// wiring the MantleScan explorer base for tx/address links and the chain id (so the
// detail endpoint can scope its raw_logs lookup for the on-demand swap decode).
func newAPIServer(db *store.DB, cfg *config.Config) *api.Server {
	return api.NewServer(db, api.Config{
		Addr:         apiBind(),
		ExplorerBase: mantleScanBase,
		ChainID:      cfg.ChainID,
		RateLimitRPS: cfg.APIRateLimitRPS,
	})
}

// runServe runs the read-only JSON API + SSE live feed until the context is
// cancelled. The rich dashboard is a separate React app that consumes this API; "/"
// serves a minimal HTML index so a direct hit on the root URL is useful.
func runServe(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("serve: load config: %w", err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("serve: open store: %w", err)
	}
	defer db.Close()

	if err := newAPIServer(db, cfg).Serve(ctx); err != nil {
		return fmt.Errorf("serve: api: %w", err)
	}
	return nil
}

// runPoolStats refreshes the on-chain market snapshot (TVL, 24h volume, spot price) for
// every enabled pool once, then exits. It is the one-shot twin of the supervisor's
// poolstats stage. The data is for display only and never feeds detection.
func runPoolStats(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("poolstats: load config: %w", err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("poolstats: open store: %w", err)
	}
	defer db.Close()

	client, err := chain.Dial(ctx, cfg.RPCURL, cfg.ChainID)
	if err != nil {
		return fmt.Errorf("poolstats: dial chain: %w", err)
	}
	defer client.Close()

	// RunOnce does a single pass regardless of the configured interval, so the command is
	// useful even when the supervisor stage is disabled. The reader reads reserves/price
	// on-chain and 24h volume from OCR's own buckets (db).
	stage := poolstats.New(db, marketdata.NewReader(client, db), time.Duration(cfg.PoolStatsIntervalMin)*time.Minute)
	if err := stage.RunOnce(ctx); err != nil {
		return fmt.Errorf("poolstats: run once: %w", err)
	}
	log.Info().Msg("ocr: poolstats one-shot complete")
	return nil
}

// runSupervisor starts every stage as a goroutine under a shared, cancellable context.
// The supervisor owns the single config, store, and chain client so the stages share
// connections rather than each opening their own. The first stage to fail fatally cancels
// the rest, and its error is returned; a clean ctx cancellation is not fatal.
//
// A sync.WaitGroup plus a buffered error channel stands in for errgroup, which is not in
// the module graph.
func runSupervisor(parent context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("run: load config: %w", err)
	}

	db, err := store.New(parent, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("run: open store: %w", err)
	}
	defer db.Close()

	pools, err := seedPools(parent, db)
	if err != nil {
		return fmt.Errorf("run: seed pools: %w", err)
	}

	client, err := chain.Dial(parent, cfg.RPCURL, cfg.ChainID)
	if err != nil {
		return fmt.Errorf("run: dial chain: %w", err)
	}
	defer client.Close()

	// Child context: the first stage to fail cancels every other stage.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// One NonceManager for the agent wallet, shared by every component that signs from
	// AGENT_PRIVATE_KEY (the signal attestor on the detect and recovery stages, and the
	// outcome attestor). These stages run as concurrent goroutines below; without a shared
	// counter two could read the same PendingNonceAt and broadcast on the same nonce, a
	// collision that drops one tx and leaves a permanent gap in the on-chain record. The
	// manager serialises every send from this wallet through one lock.
	nonces := attest.NewNonceManager()

	signalAttestor, err := newSignalAttestor(ctx, cfg, client, nonces)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	outcomeAttestor, err := newOutcomeAttestor(ctx, cfg, client, nonces)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	// stage pairs a name with its blocking run function for the supervisor loop.
	type stage struct {
		name string
		run  func(context.Context) error
	}
	stages := []stage{
		{"collect", func(c context.Context) error {
			return chain.NewCollector(client, db, cfg, pools).Run(c)
		}},
		{"aggregate", func(c context.Context) error {
			return aggregate.New(db, cfg.BucketMin, rawLogsRetention(cfg)).Run(c, aggregateInterval)
		}},
		{"detect", func(c context.Context) error {
			return detectLoop(c, cfg, db, client, signalAttestor, "")
		}},
		{"attest-pending", func(c context.Context) error {
			return attestPendingLoop(c, db, signalAttestor)
		}},
		{"enrich", func(c context.Context) error {
			return enrichLoop(c, db, cfg)
		}},
		{"outcome", func(c context.Context) error {
			return outcomeLoop(c, db, cfg, outcomeAttestor)
		}},
		{"poolstats", func(c context.Context) error {
			// Display-only market stats sourced on-chain (reserves/price) + OCR's own
			// buckets (24h volume); never feeds detection. Self-disables when
			// POOL_STATS_INTERVAL_MIN <= 0.
			return poolstats.New(db, marketdata.NewReader(client, db),
				time.Duration(cfg.PoolStatsIntervalMin)*time.Minute).Run(c)
		}},
		{"watchdog", func(c context.Context) error {
			// Silent-stall watchdog: warns (and optionally TG-alerts) when ingestion
			// lags wall-clock. Self-disables when STALL_ALERT_MIN <= 0.
			return watchdogLoop(c, db, cfg)
		}},
		{"serve", func(c context.Context) error {
			return newAPIServer(db, cfg).Serve(c)
		}},
	}

	// Buffered so every goroutine can report without blocking on shutdown.
	errCh := make(chan error, len(stages))
	var wg sync.WaitGroup
	for _, s := range stages {
		wg.Add(1)
		go func(s stage) {
			defer wg.Done()
			log.Info().Str("stage", s.name).Msg("run: stage started")
			if err := s.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				// Fatal: report it and cancel the rest of the supervisor.
				errCh <- fmt.Errorf("run: stage %s: %w", s.name, err)
				cancel()
				return
			}
			log.Info().Str("stage", s.name).Msg("run: stage stopped")
		}(s)
	}

	// Wait for all stages to unwind, then drain the first fatal error (if any).
	wg.Wait()
	close(errCh)

	var firstErr error
	for e := range errCh {
		if firstErr == nil {
			firstErr = e
		} else {
			// Secondary failures are usually cascade noise; log, don't lose them.
			log.Error().Err(e).Msg("run: additional stage error")
		}
	}
	if firstErr != nil {
		return firstErr
	}
	// No stage reported a fatal error: the parent context was cancelled
	// (SIGINT/SIGTERM). Surface that so main logs a clean shutdown.
	return parent.Err()
}

// detectLoop runs ScanOnce on a fixed interval and, for each freshly inserted
// signal, runs the per-signal hot path: best-effort attest, then deliver (when
// Telegram is configured), then record the sent Telegram message id. No LLM call
// happens here: enrichment is async and off the hot path, so a signal is attested,
// alerted, and on the dashboard immediately, never waiting on (or lost to) the LLM.
// The analyst note is added later by enrichLoop. A scan failure is logged and the
// loop continues; ctx cancellation returns ctx.Err().
//
// Unlike Detector.Run (which discards the signals it emits), this loop owns the
// downstream stages, so it drives ScanOnce directly and processes the returned
// signals.
func detectLoop(ctx context.Context, cfg *config.Config, db *store.DB, client *chain.Client, attestor *attest.Attestor, mode string) error {
	detector := detect.NewDetector(db, cfg)
	// Whale (type 2) and smart-money (type 3) both take the chain client to resolve an
	// actor to tx.origin best-effort; a resolve failure never blocks or drops a signal.
	whale := detect.NewWhaleDetector(db, cfg, client)
	smartMoney := detect.NewSmartMoneyDetector(db, cfg, client)
	// Aave V3 liquidation (type 5) + big-borrow (type 6) from the Aave Pool logs the
	// collector ingests; chain client refines the actor to tx.origin best-effort.
	lendingDet := detect.NewLendingDetector(db, cfg, client)
	// lst_flow (type 4) from large mETH/cmETH ERC-20 Transfers, classified by counterparty
	// (mint/burn/move) with DEX-pool legs filtered; chain client refines actor best-effort.
	lstFlowDet := detect.NewLSTFlowDetector(db, cfg, client)
	// depeg (type 7) from OCR's own decoded swaps on the stable/stable pools: fires when a
	// stablecoin's median implied price drifts off peg. No chain client, no external price.
	depegDet := detect.NewDepegDetector(db, cfg)

	// Telegram is optional: configured only when both token and chat id are set.
	tg := newTelegram(cfg, cfg.TGChannelID, "detect: telegram delivery disabled (TG_BOT_TOKEN or TG_CHANNEL_ID unset)")

	// One-shot modes used after a backfill (each signal still flows through the hot path):
	//   once   - score only the newest bucket per pool, plus one whale + one smart-money scan.
	//   replay - score the full bucket history for past anomalies. Whale and smart-money are
	//            inherently real-time (rolling recent window), so they do not run in replay.
	switch mode {
	case "once":
		signals, err := detector.ScanOnce(ctx)
		if err != nil {
			return fmt.Errorf("detect: one-shot scan: %w", err)
		}
		for i := range signals {
			processSignal(ctx, db, attestor, tg, signals[i])
		}
		// Smart-money runs before whale so its fired event_refs suppress a whale on the
		// same swap: a richer type-3 signal wins over a type-2 on the same tx:logindex.
		// The detectors stay decoupled, whale takes a bare set of refs.
		smart, smartRefs, err := smartMoney.ScanOnceWithRefs(ctx)
		if err != nil {
			return fmt.Errorf("detect: one-shot smart-money scan: %w", err)
		}
		for i := range smart {
			processSignal(ctx, db, attestor, tg, smart[i])
		}
		whales, err := whale.ScanOnceExcept(ctx, smartRefs)
		if err != nil {
			return fmt.Errorf("detect: one-shot whale scan: %w", err)
		}
		for i := range whales {
			processSignal(ctx, db, attestor, tg, whales[i])
		}
		lendings, err := lendingDet.ScanOnce(ctx)
		if err != nil {
			return fmt.Errorf("detect: one-shot lending scan: %w", err)
		}
		for i := range lendings {
			processSignal(ctx, db, attestor, tg, lendings[i])
		}
		lstFlows, err := lstFlowDet.ScanOnce(ctx)
		if err != nil {
			return fmt.Errorf("detect: one-shot lst-flow scan: %w", err)
		}
		for i := range lstFlows {
			processSignal(ctx, db, attestor, tg, lstFlows[i])
		}
		depegs, err := depegDet.ScanOnce(ctx)
		if err != nil {
			return fmt.Errorf("detect: one-shot depeg scan: %w", err)
		}
		for i := range depegs {
			processSignal(ctx, db, attestor, tg, depegs[i])
		}
		log.Info().Int("signals", len(signals)).Int("smart_money", len(smart)).Int("whales", len(whales)).Int("lending", len(lendings)).Int("lst_flow", len(lstFlows)).Int("depeg", len(depegs)).Msg("detect: one-shot scan complete")
		return nil
	case "replay":
		signals, err := detector.Replay(ctx)
		if err != nil {
			return fmt.Errorf("detect: replay: %w", err)
		}
		for i := range signals {
			processSignal(ctx, db, attestor, tg, signals[i])
		}
		log.Info().Int("signals", len(signals)).Msg("detect: replay complete")
		return nil
	}

	ticker := time.NewTicker(detectInterval)
	defer ticker.Stop()

	log.Info().Dur("interval", detectInterval).Msg("detect: loop started")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			signals, err := detector.ScanOnce(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// Do not abort the loop on a single failed scan; retry next tick.
				log.Error().Err(err).Msg("detect: scan failed, will retry next tick")
			} else {
				for i := range signals {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					processSignal(ctx, db, attestor, tg, signals[i])
				}
			}

			// Each detector's scan is isolated: a failure logs and retries next tick, never
			// skipping the others or aborting the loop. Smart-money runs first (as in once
			// mode); on its failure smartRefs is empty and whale still runs, suppressing
			// nothing.
			smart, smartRefs, serr := smartMoney.ScanOnceWithRefs(ctx)
			if serr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Error().Err(serr).Msg("detect: smart-money scan failed, will retry next tick")
			} else {
				for i := range smart {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					processSignal(ctx, db, attestor, tg, smart[i])
				}
			}

			whales, werr := whale.ScanOnceExcept(ctx, smartRefs)
			if werr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Error().Err(werr).Msg("detect: whale scan failed, will retry next tick")
			} else {
				for i := range whales {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					processSignal(ctx, db, attestor, tg, whales[i])
				}
			}

			lendings, lerr := lendingDet.ScanOnce(ctx)
			if lerr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Error().Err(lerr).Msg("detect: lending scan failed, will retry next tick")
			} else {
				for i := range lendings {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					processSignal(ctx, db, attestor, tg, lendings[i])
				}
			}

			lstFlows, lferr := lstFlowDet.ScanOnce(ctx)
			if lferr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Error().Err(lferr).Msg("detect: lst-flow scan failed, will retry next tick")
			} else {
				for i := range lstFlows {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					processSignal(ctx, db, attestor, tg, lstFlows[i])
				}
			}

			depegs, derr := depegDet.ScanOnce(ctx)
			if derr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Error().Err(derr).Msg("detect: depeg scan failed, will retry next tick")
				continue
			}
			for i := range depegs {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				processSignal(ctx, db, attestor, tg, depegs[i])
			}
		}
	}
}

// llmNote calls the LLM provider for an analyst note, retries once on a transient error,
// and returns (note, true) only on a non-empty result, else ("", false). It has no
// deterministic fallback: ok=false leaves the note empty for the next async pass to retry
// once the LLM recovers. Errors are logged but never returned (enrichment is best-effort,
// off the hot path).
//
// A Noop provider returns ok=false with no retry. The async stage self-disables on a Noop,
// so one only reaches here via the one-shot backfill, which applies its own fallback.
func llmNote(ctx context.Context, provider enrich.Provider, in enrich.SignalContext, signalID int64) (string, bool) {
	note, err := provider.Analyze(ctx, in)
	if err != nil {
		// Do not burn the retry during shutdown.
		if ctx.Err() != nil {
			return "", false
		}
		log.Warn().Err(err).Int64("signal_id", signalID).Msg("enrich: llm call failed; retrying once")
		note, err = provider.Analyze(ctx, in)
		if err != nil {
			log.Error().Err(err).Int64("signal_id", signalID).Msg("enrich: llm retry failed; leaving note empty for a later pass")
			return "", false
		}
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return "", false
	}
	return note, true
}

// persistAttestTx records an already-broadcast attest tx hash via write (the UPDATE
// closure), retrying a transient failure a few times with a short ctx-aware backoff.
// The tx is irreversibly on-chain by now, so the only thing at stake is leaving
// attest_tx empty, which makes the recovery loop re-broadcast a duplicate; retrying
// the cheap write avoids that. On final failure it returns the last error to log.
func persistAttestTx(ctx context.Context, write func(context.Context) error) error {
	var err error
	for attempt := 1; attempt <= attestWriteAttempts; attempt++ {
		if err = write(ctx); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err // shutting down: stop retrying, surface the error
		}
		if attempt < attestWriteAttempts {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(attestWriteBackoff):
			}
		}
	}
	return err
}

// deliverContext resolves a signal's readable pool metadata (pair label, dex) for the
// Telegram alert, so it shows "USDe/WMNT · Agni" instead of a raw address. A missing pool
// or a lookup error degrades to an empty context (the renderer falls back to the short
// address) and never blocks delivery.
func deliverContext(ctx context.Context, db *store.DB, sig store.Signal) deliver.Context {
	pool, ok, err := db.GetPool(ctx, sig.Pool)
	if err != nil {
		log.Warn().Err(err).Int64("signal_id", sig.ID).Msg("deliver: pool lookup failed; using address")
		return deliver.Context{}
	}
	if !ok {
		return deliver.Context{}
	}
	return deliver.Context{PoolLabel: pool.Label, Dex: prettyDex(pool.Dex)}
}

// prettyDex maps a registry dex id to its display name for alerts.
func prettyDex(dex string) string {
	switch dex {
	case "agni":
		return "Agni"
	case "merchant_moe":
		return "Merchant Moe"
	case "fusionx":
		return "FusionX"
	default:
		return dex
	}
}

// processSignal runs the per-signal hot path for one freshly detected signal:
// attest, deliver the Telegram alert, then record the sent message id (so the async
// enrich stage can later edit the note into that message). Enrichment is not here; it
// is off the hot path so the signal is recorded and alerted as fast as possible.
//
// Each stage is independent and never blocks the others: a failed attestation still
// lets a configured alert go out, and a failed message-id write only costs the future
// in-place note edit (the note still reaches the DB and dashboard). All errors logged.
func processSignal(
	ctx context.Context,
	db *store.DB,
	attestor *attest.Attestor,
	tg *deliver.Telegram,
	sig store.Signal,
) {
	// attest (best-effort)
	var attestTx string
	if attestor != nil {
		score := attest.ScoreFromZ(sig.Zscore)

		// Guard the int16->uint8 narrowing: a type outside [1,255] would wrap and
		// corrupt the type field in an immutable attestation record, so fail loud.
		if sig.SignalType < 1 || sig.SignalType > 255 {
			log.Error().
				Int64("signal_id", sig.ID).
				Int16("signal_type", sig.SignalType).
				Msg("detect: signal_type out of uint8 range [1,255]; skipping attest")
		} else {
			signalType := uint8(sig.SignalType)        //nolint:gosec // range checked above
			hash := attestHash(sig, signalType, score) // keyed on the logical time, see attestHash
			subject := common.HexToAddress(sig.Pool)

			// Bound the attest RPC round-trips so a hung node cannot stall the detect
			// loop. 30s is generous for three RPCs.
			actx, acancel := context.WithTimeout(ctx, 30*time.Second)
			tx, aerr := attestor.Attest(actx, hash, subject, signalType, score)
			acancel()
			if aerr != nil {
				log.Error().Err(aerr).Int64("signal_id", sig.ID).Msg("detect: attest failed; continuing")
			} else {
				attestTx = tx
				// Persist the tx hash before updating status so a crash between the two
				// writes leaves a recoverable breadcrumb, not a silent orphan.
				if uerr := persistAttestTx(ctx, func(c context.Context) error {
					return db.UpdateSignalAttest(c, sig.ID, tx)
				}); uerr != nil {
					log.Error().Err(uerr).Int64("signal_id", sig.ID).Msg("detect: persist attest tx failed after retries")
				} else {
					sig.AttestTx = tx
					if serr := db.SetSignalStatus(ctx, sig.ID, store.StatusSubmitted); serr != nil {
						log.Error().Err(serr).Int64("signal_id", sig.ID).Msg("detect: set status submitted failed")
					}
				}
			}
		}
	}

	// deliver (only when Telegram is configured)
	if tg == nil {
		return
	}
	// The alert goes out without a note; the async enrich stage edits the note into this
	// message later via the id recorded below. Bound the synchronous send so a black-holing
	// Telegram endpoint cannot stall detection/attestation; on timeout the alert is skipped
	// (logged) and the signal is already attested and on the dashboard.
	dc := deliverContext(ctx, db, sig)
	dctx, dcancel := context.WithTimeout(ctx, deliverTimeout)
	msgID, err := deliver.Dispatch(dctx, tg, sig, dc, attestTx, mantleScanBase, dashboardURL)
	dcancel()
	if err != nil {
		log.Error().Err(err).Int64("signal_id", sig.ID).Msg("detect: telegram dispatch failed")
		return
	}
	if serr := db.SetSignalStatus(ctx, sig.ID, store.StatusAlerted); serr != nil {
		log.Error().Err(serr).Int64("signal_id", sig.ID).Msg("detect: set status alerted failed")
	}
	// Record the sent message id so the async enrich stage can edit the note into
	// it. Best-effort: if this write fails the note still reaches the DB /
	// dashboard, only the in-place Telegram edit is lost. An empty id means the
	// Bot API returned no message_id (already logged in Send); skip the write.
	if msgID != "" {
		if uerr := db.UpdateSignalTGMessageID(ctx, sig.ID, msgID); uerr != nil {
			log.Error().Err(uerr).Int64("signal_id", sig.ID).Msg("detect: persist tg message id failed")
		}
	}
}

// attestHash returns the on-chain attestation hash for a signal, selecting the
// canonical-JSON variant by signal family so the hash is reproducible from the
// stored row:
//
//   - bucket signals (EventRef == "", e.g. flow anomalies): keyed on
//     (pool, signal_type, metric, bucket_ts) via attest.SignalHash, so an existing
//     type=1 hash stays byte-for-byte reproducible.
//   - per-event signals (EventRef != "", e.g. whale): keyed on
//     (pool, signal_type, event_ref, score, bucket_ts) via attest.SignalHashEvent.
//
// Both use the bucket timestamp (the signal's logical time), never created_at, so
// re-attesting the same signal never produces a different on-chain record.
func attestHash(sig store.Signal, signalType uint8, score uint16) [32]byte {
	if sig.EventRef != "" {
		return attest.SignalHashEvent(sig.Pool, signalType, sig.EventRef, score, sig.BucketTS.Unix())
	}
	return attest.SignalHash(sig.Pool, signalType, sig.Metric, score, sig.BucketTS.Unix())
}

// enrichLoop is the async enrich stage: the off-hot-path companion to detectLoop. A
// single goroutine (no per-signal fan-out) that each tick drains a bounded batch of
// note-less signals, writing the LLM note to the DB and, when the signal carried a
// Telegram message id, editing it into that already-sent alert.
//
// It self-disables with a Noop provider (logs once, returns nil, no busy loop). A
// failed or empty LLM response just leaves the note empty for a later pass, so the
// loop is the resilience; a per-signal failure is logged and the loop continues, and
// only ctx cancellation ends it (returning ctx.Err()).
func enrichLoop(ctx context.Context, db *store.DB, cfg *config.Config) error {
	provider := enrich.NewProvider(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
	if _, isNoop := provider.(enrich.Noop); isNoop {
		// No LLM configured: nothing to do. Returning nil (not an error) keeps the
		// supervisor running every other stage; a busy ticker here would only spin.
		log.Info().Msg("enrich: disabled (no LLM configured; set LLM_BASE_URL and LLM_API_KEY to enable)")
		return nil
	}

	// Telegram is optional: the in-alert note edit happens only when it is configured.
	// Without it, notes still reach the DB / dashboard.
	tg := newTelegram(cfg, cfg.TGChannelID, "enrich: telegram edit disabled (TG_BOT_TOKEN or TG_CHANNEL_ID unset); notes go to DB/dashboard only")

	// Resolve pool labels so the LLM prompt reads "USDC/USDe", not a raw address. The map
	// is display-only (signalContext falls back to the raw address), so a load failure must
	// not be fatal: returning it would reach the supervisor, whose cancel-on-error would
	// crash-loop the whole agent on a transient Postgres blip. Start with whatever loads and
	// refresh lazily in the loop.
	labels, err := loadPoolLabels(ctx, db)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn().Err(err).Msg("enrich: initial pool-label load failed; starting with no labels, will refresh")
		labels = map[string]string{}
	}

	ticker := time.NewTicker(enrichInterval)
	defer ticker.Stop()

	// Refresh the labels periodically so a newly-discovered pool's label appears without a
	// restart, and an initial empty map self-heals once Postgres recovers. A refresh failure
	// keeps the previous map (a stale label only affects display). Cadence is independent of
	// the enrich tick, since labels change rarely.
	labelTicker := time.NewTicker(enrichLabelRefreshInterval)
	defer labelTicker.Stop()

	log.Info().Dur("interval", enrichInterval).Int("batch", enrichBatch).Msg("enrich: async loop started")

	// Drain once at startup so a freshly started supervisor does not wait a full
	// interval before annotating an already-present backlog.
	enrichPass(ctx, db, provider, tg, labels)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-labelTicker.C:
			if refreshed, rerr := loadPoolLabels(ctx, db); rerr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Warn().Err(rerr).Msg("enrich: refresh pool labels failed; keeping previous")
			} else {
				labels = refreshed
			}
		case <-ticker.C:
			enrichPass(ctx, db, provider, tg, labels)
		}
	}
}

// enrichLabelRefreshInterval is how often the async enrich loop reloads the pool-label
// snapshot. Labels change rarely, so a slow cadence is ample.
const enrichLabelRefreshInterval = 5 * time.Minute

// enrichPass runs one bounded sweep of the async enrich loop: fetch up to
// enrichBatch note-less signals (newest first) and try to annotate each. Every
// failure is logged and the pass continues to the next signal; a fetch failure
// (or ctx cancellation) ends the pass early. It never returns an error (the
// caller's ticker retries next tick) but it stops promptly on ctx cancellation.
func enrichPass(ctx context.Context, db *store.DB, provider enrich.Provider, tg *deliver.Telegram, labels map[string]string) {
	sigs, err := db.SignalsNeedingNote(ctx, enrichBatch)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error().Err(err).Msg("enrich: list signals needing note failed, will retry next tick")
		return
	}
	for i := range sigs {
		if ctx.Err() != nil {
			return
		}
		enrichOne(ctx, db, provider, tg, sigs[i], labels)
	}
}

// enrichOne enriches a single signal: call the LLM (one retry, no fallback), and
// on a real note persist it (DB → dashboard) and edit it into the sent Telegram
// alert. A failed/empty LLM response leaves the note empty so the next pass
// retries once the LLM recovers. All failures are logged; none abort the loop.
func enrichOne(ctx context.Context, db *store.DB, provider enrich.Provider, tg *deliver.Telegram, sig store.Signal, labels map[string]string) {
	note, ok := llmNote(ctx, provider, signalContext(sig, labels), sig.ID)
	if !ok {
		// Already logged inside llmNote (transient error / empty). Leave the note
		// empty; SignalsNeedingNote will surface this signal again next pass.
		return
	}

	if uerr := db.UpdateSignalNote(ctx, sig.ID, note); uerr != nil {
		log.Error().Err(uerr).Int64("signal_id", sig.ID).Msg("enrich: persist note failed")
		return // can't trust sig state; retry next pass
	}
	sig.LLMNote = note

	// Never move a signal backward: the hot path may already have advanced it past
	// "enriched" (enrichment runs last), so only a still-"new" signal is moved here.
	// status is display-only, so keeping the furthest-along state is the right choice.
	if sig.Status == store.StatusNew {
		if serr := db.SetSignalStatus(ctx, sig.ID, store.StatusEnriched); serr != nil {
			log.Error().Err(serr).Int64("signal_id", sig.ID).Msg("enrich: set status enriched failed")
		}
	}

	// Edit the note into the already-sent alert (best-effort), reusing the same
	// FormatSignal Dispatch used so the caption changes only by the analyst note.
	// With no message id (Telegram off, or send/record failed) there is nothing to edit.
	if tg == nil || sig.TGMessageID == "" {
		return
	}
	view := deliver.FormatSignal(sig, deliverContext(ctx, db, sig), sig.AttestTx, mantleScanBase, dashboardURL)
	if eerr := deliver.EditSignal(ctx, tg, sig.TGMessageID, view); eerr != nil {
		log.Error().Err(eerr).Int64("signal_id", sig.ID).Str("tg_message_id", sig.TGMessageID).
			Msg("enrich: edit telegram note failed")
		return
	}
	log.Info().Int64("signal_id", sig.ID).Msg("enrich: note written and edited into alert")
}

// runEnrich writes an LLM analyst note for every signal that lacks one, then exits.
// It is the manual one-shot backfill for signals produced by `detect replay` (or any
// run before the LLM was configured); enrichLoop handles live enrichment continuously.
//
// Unlike the async loop, this one-shot applies enrich.FallbackNote (a deterministic
// note from the signal's own numbers) so a finite run leaves zero empty notes. A Noop
// provider is a no-op with a warning. It does not edit Telegram (the live loop owns
// in-alert edits); a backfilled note reaches the DB and dashboard.
func runEnrich(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("enrich: load config: %w", err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("enrich: open store: %w", err)
	}
	defer db.Close()

	provider := enrich.NewProvider(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
	if _, isNoop := provider.(enrich.Noop); isNoop {
		log.Warn().Msg("enrich: no LLM configured (LLM_BASE_URL/LLM_API_KEY unset); nothing to do")
		return nil
	}

	labels, err := loadPoolLabels(ctx, db)
	if err != nil {
		return err
	}

	// SignalsNeedingNote already filters to llm_note IS NULL/empty, so the manual
	// skip is unnecessary; reusing it keeps the "needs a note" definition in one
	// place (shared with the async loop).
	sigs, err := db.SignalsNeedingNote(ctx, 1000)
	if err != nil {
		return fmt.Errorf("enrich: list signals: %w", err)
	}

	enriched := 0
	for i := range sigs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// One-shot backfill: prefer the LLM (one retry), but guarantee a non-empty
		// note via the deterministic fallback so a finite run leaves 0 empty notes.
		in := signalContext(sigs[i], labels)
		note, ok := llmNote(ctx, provider, in, sigs[i].ID)
		if !ok {
			note = enrich.FallbackNote(in)
		}
		if uerr := db.UpdateSignalNote(ctx, sigs[i].ID, note); uerr != nil {
			log.Error().Err(uerr).Int64("signal_id", sigs[i].ID).Msg("enrich: persist note failed")
			continue
		}
		if serr := db.SetSignalStatus(ctx, sigs[i].ID, store.StatusEnriched); serr != nil {
			log.Error().Err(serr).Int64("signal_id", sigs[i].ID).Msg("enrich: set status failed")
		}
		enriched++
		log.Info().Int64("signal_id", sigs[i].ID).Msg("enrich: note written")
	}

	log.Info().Int("enriched", enriched).Int("scanned", len(sigs)).Msg("enrich: complete")
	return nil
}

// runAttestPending attests every signal that still lacks an on-chain attestation,
// then exits. It is the backfill bridge: signals produced before the contract was
// deployed (e.g. `detect replay`) get their anomaly recorded on-chain here. The
// idempotency/best-effort contract lives in attestPendingOnce (the shared core).
func runAttestPending(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("attest-pending: load config: %w", err)
	}
	if cfg.AgentPrivateKey == "" || cfg.AttestorAddress == "" {
		return fmt.Errorf("attest-pending: AGENT_PRIVATE_KEY and ATTESTOR_ADDRESS must be set")
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("attest-pending: open store: %w", err)
	}
	defer db.Close()

	client, err := chain.Dial(ctx, cfg.RPCURL, cfg.ChainID)
	if err != nil {
		return fmt.Errorf("attest-pending: dial chain: %w", err)
	}
	defer client.Close()

	// One NonceManager for this standalone process's agent wallet.
	attestor, err := attest.New(ctx, client.Eth(), cfg.AgentPrivateKey, cfg.AttestorAddress, cfg.ChainID, attest.NewNonceManager())
	if err != nil {
		return fmt.Errorf("attest-pending: init attestor: %w", err)
	}

	// The CLI scans a large window (the whole backfill backlog); the supervisor's
	// recovery loop reuses the same attestPendingOnce with a smaller window.
	attested, skipped, failed, total, err := attestPendingOnce(ctx, db, attestor, 10000)
	if err != nil {
		return fmt.Errorf("attest-pending: %w", err)
	}

	log.Info().Int("attested", attested).Int("skipped", skipped).Int("failed", failed).
		Int("total", total).Msg("attest-pending: complete")
	return nil
}

// attestPendingOnce attests every signal in the most-recent `scanLimit` rows whose
// attest_tx is still empty, returning (attested, skipped, failed, total) counts. It
// is the shared core of both the `attest-pending` CLI and the supervisor's recovery
// stage, so the self-heal logic lives in one place.
//
// Idempotent and best-effort, mirroring outcome.AttestPending:
//   - A signal that already has an attest_tx is skipped (no re-attest); the
//     (pool,signal_type,metric/event_ref,bucket_ts) hash means re-attesting the
//     same anomaly would in any case never produce a different on-chain record.
//   - The tx hash is persisted right after a successful send (before the status
//     update) so a crash leaves a recoverable breadcrumb, not a silent orphan.
//   - A per-row failure (out-of-range signal_type, attest send error, persist
//     error) is logged and counted, never fatal; only ctx cancellation or the
//     candidate-fetch error returns a non-nil error.
//   - attestor must be non-nil; callers gate on a configured attestor, like the
//     optional outcome attestor.
//
// All sends go through attestor's shared NonceManager, so running this concurrently
// with the live detect-stage attests (which it does, in the supervisor) can never
// collide on a nonce.
func attestPendingOnce(ctx context.Context, db *store.DB, attestor *attest.Attestor, scanLimit int) (attested, skipped, failed, total int, err error) {
	sigs, err := db.RecentSignals(ctx, scanLimit)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("list signals: %w", err)
	}
	total = len(sigs)

	for i := range sigs {
		if ctx.Err() != nil {
			return attested, skipped, failed, total, ctx.Err()
		}
		s := sigs[i]
		if s.AttestTx != "" {
			skipped++
			continue
		}
		if s.SignalType < 1 || s.SignalType > 255 {
			log.Error().Int64("signal_id", s.ID).Int16("signal_type", s.SignalType).
				Msg("attest-pending: signal_type out of uint8 range; skipping")
			failed++
			continue
		}

		signalType := uint8(s.SignalType)
		score := attest.ScoreFromZ(s.Zscore)
		hash := attestHash(s, signalType, score)
		subject := common.HexToAddress(s.Pool)

		tx, aerr := attestor.Attest(ctx, hash, subject, signalType, score)
		if aerr != nil {
			log.Error().Err(aerr).Int64("signal_id", s.ID).Msg("attest-pending: attest failed; continuing")
			failed++
			continue
		}
		if uerr := persistAttestTx(ctx, func(c context.Context) error {
			return db.UpdateSignalAttest(c, s.ID, tx)
		}); uerr != nil {
			log.Error().Err(uerr).Int64("signal_id", s.ID).Str("tx", tx).Msg("attest-pending: persist tx failed after retries")
		}
		if serr := db.SetSignalStatus(ctx, s.ID, store.StatusSubmitted); serr != nil {
			log.Error().Err(serr).Int64("signal_id", s.ID).Msg("attest-pending: set status failed")
		}
		attested++
		log.Info().Int64("signal_id", s.ID).Str("pool", s.Pool).Str("metric", s.Metric).
			Str("tx", tx).Msg("attest-pending: attested")

		// Be polite to the RPC and let nonce tracking settle between sends.
		select {
		case <-ctx.Done():
			return attested, skipped, failed, total, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}

	return attested, skipped, failed, total, nil
}

// attestPendingLoop is the supervisor's signal-side self-heal stage: it periodically
// re-attests signals whose attest_tx is still empty, closing the gap that the hot
// path leaves when an attest loses a nonce race or hits a transient RPC error (the
// hot path logs "attest failed; continuing" and moves on). It is the signal
// counterpart of the outcome stage's AttestPending self-heal.
//
// It self-disables when attestation is off (attestor == nil). It reuses the detect
// stage's Attestor (the same shared NonceManager), so its sends serialise with the
// live attests and can never collide on a nonce. A pass failure is logged and the
// loop continues (a transient DB/RPC hiccup must not kill the stage); only ctx
// cancellation ends it (returning ctx.Err()).
func attestPendingLoop(ctx context.Context, db *store.DB, attestor *attest.Attestor) error {
	if attestor == nil {
		// Nothing to recover; nil (not an error) keeps every other supervisor stage running.
		log.Info().Msg("attest-pending: recovery loop disabled (attestation off)")
		return nil
	}

	ticker := time.NewTicker(attestPendingInterval)
	defer ticker.Stop()

	log.Info().Dur("interval", attestPendingInterval).Int("scan", attestPendingScan).
		Msg("attest-pending: recovery loop started")

	// Drain once at startup so a freshly started supervisor heals any pre-existing
	// gap without waiting a full interval.
	attestPendingPass(ctx, db, attestor)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			attestPendingPass(ctx, db, attestor)
		}
	}
}

// attestPendingPass runs one bounded recovery sweep and logs its outcome. It never
// returns an error (the caller's ticker retries next tick) and stops promptly on
// ctx cancellation (surfaced as a context error from attestPendingOnce, which it
// only logs at debug since the caller's select handles the real shutdown).
func attestPendingPass(ctx context.Context, db *store.DB, attestor *attest.Attestor) {
	attested, _, failed, _, err := attestPendingOnce(ctx, db, attestor, attestPendingScan)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error().Err(err).Msg("attest-pending: recovery pass failed, will retry next tick")
		return
	}
	if attested > 0 || failed > 0 {
		log.Info().Int("attested", attested).Int("failed", failed).Msg("attest-pending: recovery pass complete")
	}
}

// signalScanner is the one method the one-shot detector commands need: a single
// scan that returns the freshly inserted signals. Every detector family
// (smart-money, lending, LST-flow, depeg) satisfies it via ScanOnce, so the shared
// one-shot runner depends only on this, not on any concrete detector type.
type signalScanner interface {
	ScanOnce(ctx context.Context) ([]store.Signal, error)
}

// runOneShotDetector is the shared body of the four one-shot detector commands
// (smartmoney, lending, lstflow, depeg). Each opens the shared store + chain
// client, builds the optional attestor + Telegram client, runs one scan of its
// detector, and pushes every fresh signal through the same hot path as the live loop
// (processSignal: attest, deliver, record message id; the async enrich stage adds
// the analyst note separately), then exits. The four commands differ only in:
//
//   - label:  the per-command error/log prefix (e.g. "smartmoney");
//   - build:  the detector constructor (it captures db/cfg/client; depeg ignores
//     the client, which is dialed only so the optional attestor can sign);
//   - logKey / logMsg: the completion log's count field key and message.
//
// The chain client is always dialed (every command needs it for the attestor; the
// per-event detectors also resolve tx.origin through it). A nil attestor or nil
// Telegram just means no on-chain record or no alert; detection still records the
// signal, as in detectLoop.
func runOneShotDetector(
	ctx context.Context,
	label, logKey, logMsg string,
	build func(db *store.DB, cfg *config.Config, client *chain.Client) signalScanner,
) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("%s: load config: %w", label, err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("%s: open store: %w", label, err)
	}
	defer db.Close()

	client, err := chain.Dial(ctx, cfg.RPCURL, cfg.ChainID)
	if err != nil {
		return fmt.Errorf("%s: dial chain: %w", label, err)
	}
	defer client.Close()

	// One NonceManager for this standalone process's agent wallet.
	attestor, err := newSignalAttestor(ctx, cfg, client, attest.NewNonceManager())
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	tg := newTelegram(cfg, cfg.TGChannelID, label+": telegram delivery disabled (TG_BOT_TOKEN or TG_CHANNEL_ID unset)")

	signals, err := build(db, cfg, client).ScanOnce(ctx)
	if err != nil {
		return fmt.Errorf("%s: scan: %w", label, err)
	}
	for i := range signals {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		processSignal(ctx, db, attestor, tg, signals[i])
	}
	log.Info().Int(logKey, len(signals)).Msg(logMsg)
	return nil
}

// runSmartMoney is the one-shot twin of the live smart-money (accumulation, type 3)
// scan: one scan via runOneShotDetector, then exit.
func runSmartMoney(ctx context.Context) error {
	return runOneShotDetector(ctx, "smartmoney", "smart_money", "ocr: smart-money one-shot complete",
		func(db *store.DB, cfg *config.Config, client *chain.Client) signalScanner {
			return detect.NewSmartMoneyDetector(db, cfg, client)
		})
}

// runLending is the one-shot twin of the live Aave V3 scan (liquidation type 5 +
// big-borrow type 6) via runOneShotDetector. Relies on the collector having already
// ingested the Aave Pool's recent logs.
func runLending(ctx context.Context) error {
	return runOneShotDetector(ctx, "lending", "lending", "ocr: lending one-shot complete",
		func(db *store.DB, cfg *config.Config, client *chain.Client) signalScanner {
			return detect.NewLendingDetector(db, cfg, client)
		})
}

// runLSTFlow is the one-shot twin of the live LST-flow scan (large mETH/cmETH ERC-20
// Transfers, type 4) via runOneShotDetector. Relies on the collector having already
// ingested the LST tokens' recent Transfer logs.
func runLSTFlow(ctx context.Context) error {
	return runOneShotDetector(ctx, "lstflow", "lst_flow", "ocr: lst-flow one-shot complete",
		func(db *store.DB, cfg *config.Config, client *chain.Client) signalScanner {
			return detect.NewLSTFlowDetector(db, cfg, client)
		})
}

// runDepeg is the one-shot twin of the live depeg scan (stablecoin depeg + contagion,
// type 7) via runOneShotDetector. The detector reads only OCR's own swap data (no RPC),
// so the client is dialed solely for the optional attestor; relies on the collector +
// aggregator having already ingested the stable/stable pools' recent swaps.
func runDepeg(ctx context.Context) error {
	return runOneShotDetector(ctx, "depeg", "depeg", "ocr: depeg one-shot complete",
		func(db *store.DB, cfg *config.Config, _ *chain.Client) signalScanner {
			return detect.NewDepegDetector(db, cfg)
		})
}

// runOutcomes grades the outcome report card for every matured signal (one whose
// horizon has elapsed and has no outcome row yet), then, when an OutcomeAttestor is
// configured, commits any matured-but-unattested cards on-chain, then exits. It is
// the one-shot twin of the supervisor's outcome stage, used to drain the backlog or
// measure/attest on demand. The chain client is dialed only when on-chain outcome
// attestation is enabled (OUTCOME_ATTESTOR_ADDRESS + AGENT_PRIVATE_KEY set);
// otherwise outcomes are measured off-chain with no RPC dependency.
func runOutcomes(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("outcomes: load config: %w", err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("outcomes: open store: %w", err)
	}
	defer db.Close()

	n, err := outcome.Measure(ctx, db, cfg)
	if err != nil {
		return fmt.Errorf("outcomes: measure: %w", err)
	}

	// On-chain attestation is optional (gated on address + key). Idempotent:
	// AttestPending skips already-attested outcomes, so re-running never double-attests.
	attestedTotal := 0
	if cfg.OutcomeAttestorAddress != "" && cfg.AgentPrivateKey != "" {
		client, derr := chain.Dial(ctx, cfg.RPCURL, cfg.ChainID)
		if derr != nil {
			return fmt.Errorf("outcomes: dial chain: %w", derr)
		}
		defer client.Close()

		// One NonceManager for this standalone process's agent wallet.
		attestor, aerr := newOutcomeAttestor(ctx, cfg, client, attest.NewNonceManager())
		if aerr != nil {
			return fmt.Errorf("outcomes: %w", aerr)
		}
		attestedTotal, aerr = outcome.AttestPending(ctx, db, attestor)
		if aerr != nil {
			return fmt.Errorf("outcomes: attest pending: %w", aerr)
		}
	} else {
		log.Warn().Msg("outcomes: on-chain outcome attestation disabled (OUTCOME_ATTESTOR_ADDRESS or AGENT_PRIVATE_KEY unset)")
	}

	log.Info().Int("measured", n).Int("attested", attestedTotal).Msg("ocr: outcomes one-shot complete")
	return nil
}

// runDiscover discovers new Mantle anchor pools, verifies each on-chain, and
// proposes the verified ones into the pool registry, then exits. It is a manual /
// periodic command (not a supervisor stage): coverage growth is a deliberate,
// human-reviewed act, not a hot loop.
//
// Safety model (propose-then-verify; see internal/discover):
//   - Every proposal returned by discover.Discover is already fully on-chain
//     verified (canonical token ordering, decimals, anchor/anchor) and carries
//     Enabled=false. The discovery API is not trusted for any of those.
//   - A proposal whose address is already in the registry (enabled or disabled) is
//     skipped, so discovery never duplicates a row and never downgrades an existing
//     (possibly enabled) pool back to disabled by re-proposing it.
//   - Default: new proposals are written enabled=false for a human to review and
//     flip. With --auto, a new proposal is written enabled=true, but only because
//     Discover already guarantees it is fully verified and clears the liquidity
//     floor; --auto never relaxes a check, it only skips the manual flip.
func runDiscover(ctx context.Context) error {
	// Parse the optional --auto flag from the remaining args. Anything else is a
	// usage error rather than a silent no-op.
	auto := false
	for _, a := range os.Args[2:] {
		switch a {
		case "--auto":
			auto = true
		default:
			return fmt.Errorf("discover: unknown argument %q (only --auto is supported)", a)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("discover: load config: %w", err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("discover: open store: %w", err)
	}
	defer db.Close()

	client, err := chain.Dial(ctx, cfg.RPCURL, cfg.ChainID)
	if err != nil {
		return fmt.Errorf("discover: dial chain: %w", err)
	}
	defer client.Close()

	// The discovery source (untrusted) + the on-chain verifier (*chain.Client
	// satisfies discover.ChainVerifier via PoolTokens / TokenDecimals).
	proposals, err := discover.Discover(ctx, discover.NewGecko(cfg.GeckoAPIKey), client, cfg)
	if err != nil {
		return fmt.Errorf("discover: run: %w", err)
	}

	// Membership set to skip already-known proposals (see the no-downgrade invariant above).
	existing, err := db.ListPoolAddresses(ctx)
	if err != nil {
		return fmt.Errorf("discover: list existing pools: %w", err)
	}

	var proposed, enabled, skippedExisting int
	for i := range proposals {
		if err := ctx.Err(); err != nil {
			return ctx.Err()
		}
		p := proposals[i] // already Enabled=false, fully on-chain verified
		if _, known := existing[strings.ToLower(p.Address)]; known {
			skippedExisting++
			log.Debug().Str("pool", p.Address).Msg("discover: already in registry, leaving untouched")
			continue
		}

		// The only place enabled may be set true, and only for a brand-new, fully-verified
		// pool (existing rows were skipped above). See the --auto rule in the doc comment.
		if auto {
			p.Enabled = true
		}

		if err := db.UpsertPool(ctx, p); err != nil {
			return fmt.Errorf("discover: upsert proposed pool %s: %w", p.Address, err)
		}
		proposed++
		if p.Enabled {
			enabled++
		}
	}

	if auto {
		log.Info().
			Int("verified", len(proposals)).
			Int("new", proposed).
			Int("enabled", enabled).
			Int("skipped_existing", skippedExisting).
			Msg("discover: --auto enabled fully-verified new pools")
	} else {
		log.Info().
			Int("verified", len(proposals)).
			Int("new", proposed).
			Int("skipped_existing", skippedExisting).
			Msg("discover: proposed new pools (enabled=false); review then enable")
	}
	return nil
}

// runPoolMeta backfills each enabled pool's static display metadata, then exits: the
// Merchant Moe (Liquidity Book) bin step (getBinStep(), selector 0x17f11ecc) into
// pools.bin_step, and each pool's token symbols/logos/decimals from GeckoTerminal into
// token_meta. Both are static (they change only when the registry changes), plumbing for
// the API's add-liquidity deep-link and token icons, never read by detection — so they
// live in this operator-run backfill rather than the frequent on-chain poolstats loop.
//
// A pool whose getBinStep() reverts (a non-LB moe pool or a transient RPC error) is
// logged and skipped (stays NULL) so one bad pool never aborts the run. The reads are
// always re-derived (not gated on a NULL column), so the backfill is idempotent.
func runPoolMeta(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("poolmeta: load config: %w", err)
	}

	db, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("poolmeta: open store: %w", err)
	}
	defer db.Close()

	client, err := chain.Dial(ctx, cfg.RPCURL, cfg.ChainID)
	if err != nil {
		return fmt.Errorf("poolmeta: dial chain: %w", err)
	}
	defer client.Close()

	pools, err := db.ListEnabledPools(ctx)
	if err != nil {
		return fmt.Errorf("poolmeta: list enabled pools: %w", err)
	}

	var updated, skippedNonMoe, failed int
	for i := range pools {
		if err := ctx.Err(); err != nil {
			return ctx.Err()
		}
		p := pools[i]

		// Only Merchant Moe (Liquidity Book) pools have a bin step; Agni pools never
		// answer getBinStep(), so skip them without an RPC call.
		if p.Dex != "merchant_moe" {
			skippedNonMoe++
			continue
		}

		bs, bserr := client.PoolBinStep(ctx, common.HexToAddress(p.Address))
		if bserr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Non-fatal (see doc): leave bin_step NULL and continue, logging the cause.
			log.Warn().Err(bserr).Str("pool", p.Address).
				Msg("poolmeta: bin step read failed; leaving bin_step unset")
			failed++
			continue
		}

		v := int(bs)
		p.BinStep = &v
		if uerr := db.UpsertPool(ctx, p); uerr != nil {
			return fmt.Errorf("poolmeta: persist bin_step for pool %s: %w", p.Address, uerr)
		}
		updated++
		log.Info().Str("pool", p.Address).Str("label", p.Label).Int("bin_step", v).
			Msg("poolmeta: bin step backfilled")
	}

	// Static token metadata (symbols/logos/decimals) from GeckoTerminal, keyed by address.
	// Kept off the frequent poolstats loop (which is now fully on-chain); refreshed here so
	// a newly-added pool's token icons land when the operator runs poolmeta.
	backfillTokenMeta(ctx, db, discover.NewGecko(cfg.GeckoAPIKey), pools)

	log.Info().
		Int("enabled", len(pools)).
		Int("updated", updated).
		Int("skipped_non_moe", skippedNonMoe).
		Int("failed", failed).
		Msg("ocr: poolmeta one-shot complete")
	return nil
}

// tokenMetaSpacing spaces the per-pool GeckoTerminal calls in the token-meta backfill for
// the keyless rate limit (~30/min), matching the discovery client's page spacing.
const tokenMetaSpacing = 2 * time.Second

// backfillTokenMeta refreshes each pool's display-only token metadata (symbol, logo,
// decimals) from GeckoTerminal into token_meta, keyed by token address. Best-effort: a
// per-pool fetch or per-token upsert failure is logged and skipped, never aborting the run
// — token_meta is purely cosmetic (the API falls back to the pool label for symbols and to
// no icon for logos). Calls are spaced for the keyless rate limit; ctx cancellation stops
// it promptly.
func backfillTokenMeta(ctx context.Context, db *store.DB, gecko *discover.Gecko, pools []store.Pool) {
	upserted := 0
	for i := range pools {
		if ctx.Err() != nil {
			return
		}
		ms, err := gecko.PoolStats(ctx, pools[i].Address)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(err).Str("pool", pools[i].Address).Msg("poolmeta: token metadata fetch failed; skipping pool")
		} else {
			for _, tok := range ms.Tokens {
				if tok.Address == "" {
					continue
				}
				if uerr := db.UpsertTokenMeta(ctx, store.TokenMeta{
					Address:   tok.Address,
					Symbol:    tok.Symbol,
					LogoURL:   tok.LogoURL,
					Decimals:  tok.Decimals,
					FetchedAt: time.Now().UTC(),
					SourceURL: ms.SourceURL,
				}); uerr != nil {
					if ctx.Err() != nil {
						return
					}
					log.Warn().Err(uerr).Str("token", tok.Address).Msg("poolmeta: token_meta upsert failed; skipping token")
					continue
				}
				upserted++
			}
		}
		// Space calls for the keyless rate limit, skipping the sleep after the final pool.
		if i < len(pools)-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(tokenMetaSpacing):
			}
		}
	}
	log.Info().Int("tokens", upserted).Int("pools", len(pools)).Msg("poolmeta: token metadata backfilled")
}

// outcomeLoop runs one outcomePass on a fixed interval until the context is cancelled,
// mirroring Detector.Run's resilient ticker shape. attestor is nil when outcome
// attestation is disabled, making the pass's attest step a no-op. A pass failure is
// logged and the loop continues; ctx cancellation returns ctx.Err().
func outcomeLoop(ctx context.Context, db *store.DB, cfg *config.Config, attestor *attest.OutcomeAttestor) error {
	ticker := time.NewTicker(outcomeInterval)
	defer ticker.Stop()

	log.Info().Dur("interval", outcomeInterval).Bool("attest", attestor != nil).Msg("outcome: loop started")

	// Run once at startup so a freshly started supervisor does not wait a full
	// interval before grading (and attesting) an already-matured backlog.
	outcomePass(ctx, db, cfg, attestor)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			outcomePass(ctx, db, cfg, attestor)
		}
	}
}

// outcomePass runs one measure-then-attest cycle. Measure grades newly-matured
// signals into report cards; AttestPending (when an attestor is configured) commits
// any still-unattested cards on-chain. The two steps are independent: a measure
// failure must not skip attesting already-graded outcomes, and an attest failure
// must not abort the loop. Both log and continue; ctx cancellation is surfaced by
// the caller's select. It never returns an error (the ticker retries next tick).
func outcomePass(ctx context.Context, db *store.DB, cfg *config.Config, attestor *attest.OutcomeAttestor) {
	if _, err := outcome.Measure(ctx, db, cfg); err != nil {
		if ctx.Err() != nil {
			return
		}
		// Do not abort the loop on a single failed pass; retry next tick.
		log.Error().Err(err).Msg("outcome: measure failed, will retry next tick")
	}

	// Best-effort on-chain attestation of matured outcomes (no-op when attestor is
	// nil). Idempotent: AttestPending only submits outcomes with no attest_tx yet.
	if attestor != nil {
		if _, err := outcome.AttestPending(ctx, db, attestor); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error().Err(err).Msg("outcome: attest pending failed, will retry next tick")
		}
	}
}

// watchdogState carries the silent-stall watchdog's progress across ticks: the last
// collector cursor it observed, when that cursor last advanced, and when it last alerted.
type watchdogState struct {
	lastBlock  int64
	advancedAt time.Time
	lastAlert  time.Time
}

// watchdogLoop is the supervisor's silent-stall watchdog stage. Every watchdogInterval it
// reads the collector cursor (the last block the collector processed); when that cursor
// stops advancing for more than StallAlertMin minutes it emits a throttled WARN and, when
// an alert chat is configured, a throttled Telegram alert. The cursor advances on every
// scan that covers new blocks regardless of whether the tracked pools traded, so a frozen
// cursor means a real stall — a wedged collector or a down RPC — not a quiet market. The
// hot loops already log "scan failed, will retry"; this catches the subtler case where
// every stage looks "fine" yet ingestion has silently stopped.
//
// It self-disables when STALL_ALERT_MIN <= 0. It is best-effort and never fatal: a cursor
// read failure is logged and the loop continues; only ctx cancellation ends it. The pure
// liveness + throttle logic lives in watchdogStep.
func watchdogLoop(ctx context.Context, db *store.DB, cfg *config.Config) error {
	if cfg.StallAlertMin <= 0 {
		log.Info().Msg("watchdog: disabled (STALL_ALERT_MIN <= 0)")
		return nil
	}
	threshold := time.Duration(cfg.StallAlertMin) * time.Minute

	// Operational alerts go to a private operator chat, never the public signal channel;
	// unset leaves stalls in the WARN log only.
	tg := newTelegram(cfg, cfg.TGAlertChatID, "watchdog: telegram alerting disabled (TG_ALERT_CHAT_ID unset); stalls log only")

	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()

	log.Info().Dur("interval", watchdogInterval).Dur("threshold", threshold).
		Msg("watchdog: silent-stall loop started")

	var st watchdogState
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			st = watchdogCheck(ctx, db, cfg.ChainID, tg, threshold, st)
		}
	}
}

// watchdogCheck runs one liveness probe and returns the next watchdog state. It reads the
// collector cursor and defers the liveness + throttle decision to watchdogStep; on a stall
// it WARNs and (when tg != nil) TG-alerts. It never returns an error: a cursor read failure
// or a not-yet-seeded cursor is treated as "cannot assess" and leaves the state intact.
func watchdogCheck(ctx context.Context, db *store.DB, chainID int64, tg *deliver.Telegram, threshold time.Duration, st watchdogState) watchdogState {
	cursor, ok, err := db.GetCursor(ctx, chainID)
	if err != nil {
		if ctx.Err() != nil {
			return st
		}
		// Cannot read the cursor: leave state untouched so a transient DB blip neither
		// fires a false stall alert nor resets a real one.
		log.Error().Err(err).Msg("watchdog: cursor read failed, will retry next tick")
		return st
	}

	now := time.Now()
	next, alert := watchdogStep(st, cursor, ok, now, threshold)
	if !alert {
		return next
	}

	frozen := now.Sub(st.advancedAt)
	log.Warn().
		Int64("cursor_block", cursor).
		Dur("frozen_for", frozen.Round(time.Second)).
		Dur("threshold", threshold).
		Msg("watchdog: collector cursor not advancing (ingestion stalled)")

	if tg != nil {
		msg := fmt.Sprintf(
			"OCR watchdog: ingestion stalled. Collector cursor stuck at block %d for %s (threshold %s). Check the collector / RPC.",
			cursor, frozen.Round(time.Second), threshold)
		// Bound the send so a black-holing Telegram endpoint cannot stall the watchdog
		// tick; a failure is logged, never fatal.
		sctx, scancel := context.WithTimeout(ctx, deliverTimeout)
		if _, serr := tg.Send(sctx, msg, nil); serr != nil {
			log.Error().Err(serr).Msg("watchdog: stall telegram alert failed")
		}
		scancel()
	}
	return next
}

// watchdogStep is the pure state transition for one watchdog tick, split out so the
// liveness + throttle logic is unit-testable without a DB, clock, or Telegram. Given the
// prior state, the freshly-read cursor (and whether one exists yet), the current time, and
// the stall threshold, it returns the next state and whether to alert now:
//
//   - no cursor yet (cold start): cannot assess; carry state forward, no alert.
//   - cursor advanced (or first observation): the collector is progressing; record the new
//     block and advance time and clear the throttle.
//   - cursor unchanged: a candidate stall; defer to stallAlertDecision for the threshold
//     crossing and re-alert throttle.
func watchdogStep(st watchdogState, cursor int64, hasCursor bool, now time.Time, threshold time.Duration) (next watchdogState, alert bool) {
	if !hasCursor {
		return st, false
	}
	if st.advancedAt.IsZero() || cursor > st.lastBlock {
		return watchdogState{lastBlock: cursor, advancedAt: now}, false
	}
	doAlert, newLast := stallAlertDecision(now.Sub(st.advancedAt), threshold, now, st.lastAlert)
	return watchdogState{lastBlock: st.lastBlock, advancedAt: st.advancedAt, lastAlert: newLast}, doAlert
}

// stallAlertDecision is the watchdog's throttle policy: given how long the collector
// cursor has been frozen, the stall threshold, the current time, and when an alert last
// fired, it returns whether to alert now and the last-alert timestamp to carry forward.
//
//   - frozen <= threshold (healthy): no alert; clear the throttle so the next distinct
//     stall alerts immediately.
//   - frozen > threshold (stalled): alert on the first crossing, then at most once per
//     stallReAlertInterval; while suppressed, carry the existing lastAlert forward.
func stallAlertDecision(frozen, threshold time.Duration, now, lastAlert time.Time) (alert bool, newLast time.Time) {
	if frozen <= threshold {
		return false, time.Time{}
	}
	if !lastAlert.IsZero() && now.Sub(lastAlert) < stallReAlertInterval {
		return false, lastAlert
	}
	return true, now
}

// newTelegram builds the optional Telegram client for an explicit destination chat.
// Configured only when both the bot token and chatID are set; otherwise it logs
// disabledMsg once and returns nil, which callers pass straight through (Dispatch and
// the edit/send paths all no-op on a nil client). Passing the chat per call keeps each
// destination obvious: signals go to the public channel, ops alerts to a private chat.
func newTelegram(cfg *config.Config, chatID, disabledMsg string) *deliver.Telegram {
	if cfg.TGBotToken == "" || chatID == "" {
		log.Warn().Msg(disabledMsg)
		return nil
	}
	return deliver.NewTelegram(cfg.TGBotToken, chatID)
}

// newSignalAttestor builds the optional on-chain signal attestor. It returns
// (nil, nil) with a warning when AGENT_PRIVATE_KEY or ATTESTOR_ADDRESS is unset, so
// detection (and any alerting) still runs without an on-chain record. nonces is the
// shared per-wallet NonceManager the caller threads through every signer on the agent
// key. It reuses the passed ethclient and validates the key/address in attest.New.
func newSignalAttestor(ctx context.Context, cfg *config.Config, client *chain.Client, nonces *attest.NonceManager) (*attest.Attestor, error) {
	if cfg.AgentPrivateKey == "" || cfg.AttestorAddress == "" {
		log.Warn().Msg("detect: attestation disabled (AGENT_PRIVATE_KEY or ATTESTOR_ADDRESS unset)")
		return nil, nil
	}
	a, err := attest.New(ctx, client.Eth(), cfg.AgentPrivateKey, cfg.AttestorAddress, cfg.ChainID, nonces)
	if err != nil {
		return nil, fmt.Errorf("init attestor: %w", err)
	}
	return a, nil
}

// newOutcomeAttestor builds the optional on-chain outcome attestor, mirroring the
// signal-attestor pattern: (nil, nil) when OUTCOME_ATTESTOR_ADDRESS or AGENT_PRIVATE_KEY
// is unset, else reuse the passed ethclient and validate in attest.NewOutcome. nonces
// must be the same NonceManager passed to the signal attestor whenever both sign from
// the same agent key, so concurrent signal+outcome attests can never collide on a nonce.
func newOutcomeAttestor(ctx context.Context, cfg *config.Config, client *chain.Client, nonces *attest.NonceManager) (*attest.OutcomeAttestor, error) {
	if cfg.OutcomeAttestorAddress == "" || cfg.AgentPrivateKey == "" {
		log.Warn().Msg("outcome: on-chain outcome attestation disabled (OUTCOME_ATTESTOR_ADDRESS or AGENT_PRIVATE_KEY unset)")
		return nil, nil
	}
	a, err := attest.NewOutcome(ctx, client.Eth(), cfg.AgentPrivateKey, cfg.OutcomeAttestorAddress, cfg.ChainID, nonces)
	if err != nil {
		return nil, fmt.Errorf("init outcome attestor: %w", err)
	}
	return a, nil
}

// loadPoolLabels returns a lowercase-address -> human label map for the enabled
// pool registry, so signal notes and alerts can show "USDC/USDe" instead of a
// bare contract address.
func loadPoolLabels(ctx context.Context, db *store.DB) (map[string]string, error) {
	pools, err := db.ListEnabledPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("load pool labels: %w", err)
	}
	m := make(map[string]string, len(pools))
	for _, p := range pools {
		m[strings.ToLower(p.Address)] = p.Label
	}
	return m, nil
}

// signalContext flattens a store.Signal into the enricher's input, pulling the
// latest/median/bucket-time fields out of the detector payload. A payload that
// cannot be decoded is logged and the context falls back to the always-present
// top-level fields (pool, metric, z-score) so enrichment can still proceed.
func signalContext(sig store.Signal, labels map[string]string) enrich.SignalContext {
	label := labels[strings.ToLower(sig.Pool)]
	if label == "" {
		label = sig.Pool // fall back to the address when no label is known
	}
	in := enrich.SignalContext{
		Pool:      sig.Pool,
		PoolLabel: label,
		Metric:    sig.Metric,
		Zscore:    sig.Zscore,
	}

	// size_usd is the dollar notional stored on the signal row for display (whale,
	// smart-money, and lending). Surface it to the lending prompt as a string so the
	// model can reference a magnitude; empty when no stable side was present.
	if sig.SizeUSD != nil {
		in.SizeUSD = sig.SizeUSD.String()
	}

	if len(sig.Payload) > 0 {
		// Per-family decode keyed on signal_type, not a shared superset: a union would
		// let a future family reusing "from"/"to"/"address" silently cross-populate.
		applySignalPayload(&in, sig)
	}
	return in
}

// applySignalPayload decodes a signal's family-specific payload into the enricher
// input, branching on sig.SignalType into each family's own struct (the shared
// latest/median/bucket-time fields plus that family's extras). A decode failure logs
// a warning and leaves the header fields untouched, so enrich proceeds with
// pool/metric/z-score only.
func applySignalPayload(in *enrich.SignalContext, sig store.Signal) {
	switch sig.SignalType {
	case store.SignalTypeFlow:
		// Flow anomaly (bucket signal): latest/median/bucket_ts (detect.signalPayload).
		var p struct {
			Latest   float64 `json:"latest"`
			Median   float64 `json:"median"`
			BucketTS string  `json:"bucket_ts"`
		}
		if !decodePayload(sig, &p) {
			return
		}
		in.Median = p.Median
		in.LatestValue = p.Latest
		in.BucketTime = p.BucketTS

	case store.SignalTypeWhale:
		// Whale (per-event): size0/median/block_time (detect.whalePayload). The firing
		// swap's size0 is the "latest" value; the event time is the bucket time.
		var p struct {
			Size0     float64 `json:"size0"`
			Median    float64 `json:"median"`
			BlockTime string  `json:"block_time"`
		}
		if !decodePayload(sig, &p) {
			return
		}
		in.Median = p.Median
		in.LatestValue = p.Size0
		in.BucketTime = p.BlockTime

	case store.SignalTypeSmartMoney:
		// Smart-money (type 3): the accumulation fields so the enricher describes an
		// accumulating wallet, not a 5-minute z-score (detect.smartPayload).
		var p struct {
			Size0         float64 `json:"size0"`
			BlockTime     string  `json:"block_time"`
			Actor         string  `json:"actor"`
			AccumSwaps    int     `json:"accum_swaps"`
			DistinctPools int     `json:"distinct_pools"`
			SizeMultiple  float64 `json:"size_multiple"`
			WindowHours   int     `json:"window_hours"`
		}
		if !decodePayload(sig, &p) {
			return
		}
		in.LatestValue = p.Size0
		in.BucketTime = p.BlockTime
		in.IsSmartMoney = true
		in.Actor = p.Actor
		in.AccumSwaps = p.AccumSwaps
		in.DistinctPools = p.DistinctPools
		in.SizeMultiple = p.SizeMultiple
		in.WindowHours = p.WindowHours

	case store.SignalTypeLiquidation:
		// Liquidation (type 5): the lending parties so the enricher describes a
		// liquidation, not a flow or accumulation (detect.liquidationPayload).
		var p struct {
			BlockTime       string `json:"block_time"`
			Liquidator      string `json:"liquidator"`
			LiquidatedUser  string `json:"user"`
			CollateralAsset string `json:"collateral_asset"`
			DebtAsset       string `json:"debt_asset"`
		}
		if !decodePayload(sig, &p) {
			return
		}
		in.BucketTime = p.BlockTime
		in.IsLending = true
		in.IsLiquidation = true
		in.Liquidator = p.Liquidator
		in.LiquidatedUser = p.LiquidatedUser
		in.CollateralAsset = p.CollateralAsset
		in.DebtAsset = p.DebtAsset

	case store.SignalTypeBigBorrow:
		// Big borrow (type 6): the borrowed reserve + borrower (detect.borrowPayload).
		var p struct {
			BlockTime  string `json:"block_time"`
			Reserve    string `json:"reserve"`
			OnBehalfOf string `json:"on_behalf_of"`
		}
		if !decodePayload(sig, &p) {
			return
		}
		in.BucketTime = p.BlockTime
		in.IsLending = true
		in.IsLiquidation = false
		in.Reserve = p.Reserve
		in.OnBehalfOf = p.OnBehalfOf

	case store.SignalTypeLSTFlow:
		// LST flow (type 4): the token/direction/amount + counterparties so the enricher
		// describes a mint/burn/move (detect.lstFlowPayload). The whole-token amount
		// conveys magnitude (no USD).
		var p struct {
			BlockTime string `json:"block_time"`
			Symbol    string `json:"symbol"`
			Direction string `json:"direction"`
			ValueLST  string `json:"value_lst"`
			From      string `json:"from"`
			To        string `json:"to"`
		}
		if !decodePayload(sig, &p) {
			return
		}
		in.BucketTime = p.BlockTime
		in.IsLSTFlow = true
		in.LSTSymbol = p.Symbol
		in.LSTDirection = p.Direction
		in.LSTValue = p.ValueLST
		in.LSTFrom = p.From
		in.LSTTo = p.To

	case store.SignalTypeDepeg:
		// Depeg (type 7): the depegging stablecoin, its implied price + bps off peg, and
		// the contagion set (at-risk pool labels joined) so the enricher describes a
		// stablecoin depeg + contagion (detect.depegPayload). The deviation conveys
		// magnitude (no external price).
		var p struct {
			BucketTS          string `json:"bucket_ts"`
			DepegTokenSymbol  string `json:"token_symbol"`
			DepegImpliedPrice string `json:"implied_price"`
			DepegDeviationBps int    `json:"deviation_bps"`
			DepegAffected     []struct {
				Address string `json:"address"`
				Label   string `json:"label"`
			} `json:"affected_pools"`
		}
		if !decodePayload(sig, &p) {
			return
		}
		in.BucketTime = p.BucketTS
		in.IsDepeg = true
		in.DepegSymbol = p.DepegTokenSymbol
		in.DepegImpliedPx = p.DepegImpliedPrice
		in.DepegDeviationBp = p.DepegDeviationBps
		labels := make([]string, 0, len(p.DepegAffected))
		for _, ap := range p.DepegAffected {
			if ap.Label != "" {
				labels = append(labels, ap.Label)
			}
		}
		in.DepegAffected = strings.Join(labels, ", ")

	default:
		// Unknown / future family: decode only the shared header fields any payload may
		// carry, with the same latest/median/bucket-time fallbacks the union path used,
		// so enrichment still surfaces a magnitude + time without a family-specific shape.
		var p struct {
			Latest    float64 `json:"latest"`
			Median    float64 `json:"median"`
			BucketTS  string  `json:"bucket_ts"`
			Size0     float64 `json:"size0"`
			BlockTime string  `json:"block_time"`
		}
		if !decodePayload(sig, &p) {
			return
		}
		in.Median = p.Median
		in.LatestValue = p.Latest
		if in.LatestValue == 0 {
			in.LatestValue = p.Size0
		}
		in.BucketTime = p.BucketTS
		if in.BucketTime == "" {
			in.BucketTime = p.BlockTime
		}
	}
}

// decodePayload unmarshals a signal's payload into dst, returning false (and
// logging the same warning signalContext always logged) when the payload cannot be
// decoded, so the caller leaves the header-only context untouched.
func decodePayload(sig store.Signal, dst any) bool {
	if err := json.Unmarshal(sig.Payload, dst); err != nil {
		log.Warn().Err(err).Int64("signal_id", sig.ID).Msg("detect: decode signal payload; enriching with header fields only")
		return false
	}
	return true
}

// seedPools upserts the curated YAML registry, then returns the full enabled pool
// set from the DB (not just the YAML seed) for the collector/backfill to poll.
// This matters: pools added later by `ocr discover --auto` live only in the DB
// (enabled=true) and are absent from poolsConfigPath, so returning the YAML slice
// would silently exclude them from collection (detectors, which read
// ListEnabledPools, would have no data for them). The YAML is the curated seed;
// ListEnabledPools is the authoritative active set. An empty registry is allowed.
func seedPools(ctx context.Context, db *store.DB) ([]store.Pool, error) {
	seed, err := config.LoadPools(poolsConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load pools %q: %w", poolsConfigPath, err)
	}

	for i := range seed {
		if err := db.UpsertPool(ctx, seed[i]); err != nil {
			return nil, fmt.Errorf("upsert pool %s: %w", seed[i].Address, err)
		}
	}

	enabled, err := db.ListEnabledPools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list enabled pools: %w", err)
	}

	log.Info().Int("seeded", len(seed)).Int("enabled", len(enabled)).
		Msg("ocr: pool registry seeded (collector polls the enabled set)")
	return enabled, nil
}
