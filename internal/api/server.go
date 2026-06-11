package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"ocr/internal/store"
)

// Server is the read-only JSON API + SSE live feed. It owns the store handle, the SSE
// Hub, the explorer base (for tx/address links), and a periodically-refreshed pool-label
// snapshot. One handler per route; all data comes from the store (no inline SQL).
//
// Concurrency: the HTTP handlers and the live-tail goroutine all read the store
// concurrently (pgxpool is safe). The only mutable shared state is the pool-label
// snapshot, guarded by labelMu; the Hub has its own lock.
type Server struct {
	db           *store.DB
	hub          *Hub
	explorerBase string // e.g. "https://mantlescan.xyz"; "" disables links
	addr         string
	chainID      int64 // chain to scope the raw_logs lookup for on-demand swap decode

	tailInterval time.Duration // live-tail poll cadence
	replayN      int           // signals replayed to a new SSE client on connect

	rateLimit *rateLimiter // per-IP token bucket for /api/*; nil when disabled

	labelMu sync.RWMutex
	labels  map[string]string // pool address (lowercase) -> human label
}

// Config configures the API server. Addr defaults to 0.0.0.0:7070 when empty;
// ExplorerBase is the MantleScan root for links; ChainID scopes the raw_logs swap-decode
// lookup (defaults to 5000); RateLimitRPS is the per-IP /api/* budget (0 disables it).
type Config struct {
	Addr         string
	ExplorerBase string
	ChainID      int64
	RateLimitRPS int
}

const (
	// defaultAddr binds all interfaces so the API can serve as a public, no-wallet
	// read-only URL. Local dev can override with API_ADDR=127.0.0.1:7070.
	defaultAddr = "0.0.0.0:7070"

	// defaultChainID is Mantle mainnet, the chain the raw_logs point-lookup is
	// scoped to when Config.ChainID is unset.
	defaultChainID = 5000

	// tailInterval is how often the live-tail goroutine polls the DB for new signals.
	// Polling the DB (not coupling to processSignal) keeps the feed restart-safe; ~2s
	// latency is fine.
	tailInterval = 2 * time.Second

	// tailBatch bounds how many new signals one tail poll publishes, so a burst
	// drains over a few ticks instead of one unbounded query.
	tailBatch = 200

	// replayN is how many recent signals a newly-connected SSE client receives
	// before the live stream, so a freshly-loaded dashboard is not empty.
	replayN = 20

	// labelRefreshInterval is how often the pool-label snapshot is refreshed (so a
	// newly-discovered/enabled pool's label appears without a restart).
	labelRefreshInterval = 5 * time.Minute

	// readHeaderTimeout guards the listener against slow-loris header sends.
	readHeaderTimeout = 10 * time.Second

	// shutdownTimeout bounds the graceful-drain window on ctx cancellation.
	shutdownTimeout = 5 * time.Second

	// maxLimit caps any list endpoint's page size, regardless of the requested
	// limit, so a single request can never ask for an unbounded scan.
	maxLimit = 200

	// maxSignalsOffset caps how deep a public caller can page into /api/signals: a huge
	// offset would force Postgres to walk that many index rows before discarding them.
	// Clamping (rather than a 400) still returns a final page for honest deep-paging,
	// since the response carries total. 100k is 500 pages at the 200-row cap.
	maxSignalsOffset = 100_000

	// defaultSignalLimit / defaultActorLimit are the page sizes used when the
	// caller omits ?limit.
	defaultSignalLimit = 50
	defaultActorLimit  = 50

	// eventSignal is the SSE event name for a new signal.
	eventSignal = "signal"

	// defaultRateLimitRPS is the per-IP /api/* budget when Config.RateLimitRPS is unset
	// (negative is treated as unset). A cheap per-IP bucket caps one client's burst
	// without affecting honest dashboard traffic; 0 in config disables it.
	defaultRateLimitRPS = 20

	// rateLimitBurstFactor sizes the burst as this multiple of the per-second rate, so a
	// normal page-load fan-out is absorbed while a sustained flood is still throttled.
	rateLimitBurstFactor = 2
)

// NewServer constructs an API server over the given store and config. It does not
// start listening; call Serve.
func NewServer(db *store.DB, cfg Config) *Server {
	addr := cfg.Addr
	if addr == "" {
		addr = defaultAddr
	}
	chainID := cfg.ChainID
	if chainID == 0 {
		chainID = defaultChainID
	}
	// Negative RPS = "unset" (falls back to the default); exactly 0 disables the limiter
	// (newRateLimiter returns nil), so it can be turned off explicitly.
	rps := cfg.RateLimitRPS
	if rps < 0 {
		rps = defaultRateLimitRPS
	}
	return &Server{
		db:           db,
		hub:          NewHub(),
		explorerBase: strings.TrimRight(cfg.ExplorerBase, "/"),
		addr:         addr,
		chainID:      chainID,
		tailInterval: tailInterval,
		replayN:      replayN,
		rateLimit:    newRateLimiter(rps, rps*rateLimitBurstFactor),
		labels:       map[string]string{},
	}
}

// label resolves a pool address to its human label via the snapshot (thread-safe);
// "" when unknown.
func (s *Server) label(addr string) string {
	s.labelMu.RLock()
	defer s.labelMu.RUnlock()
	return s.labels[strings.ToLower(addr)]
}

// refreshLabels reloads the pool-label snapshot from the registry. A failure is logged
// and the previous snapshot kept (a stale label beats a wiped map).
func (s *Server) refreshLabels(ctx context.Context) {
	pools, err := s.db.ListEnabledPools(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Error().Err(err).Msg("api: refresh pool labels failed; keeping previous")
		}
		return
	}
	m := make(map[string]string, len(pools))
	for _, p := range pools {
		m[strings.ToLower(p.Address)] = p.Label
	}
	s.labelMu.Lock()
	s.labels = m
	s.labelMu.Unlock()
}

// Serve starts the HTTP server, the live-tail producer, and the label-refresh loop,
// blocking until ctx is cancelled or the listener fails. On cancellation it drains
// in-flight requests (Shutdown), closes the Hub (so SSE goroutines exit), and returns nil.
func (s *Server) Serve(ctx context.Context) error {
	if s.db == nil {
		return errors.New("api: nil store.DB")
	}

	// Prime the label snapshot before serving so the first responses have labels.
	s.refreshLabels(ctx)
	s.rateLimit.logConfig()

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		// No WriteTimeout: SSE connections are long-lived. ReadHeaderTimeout mitigates
		// slow-loris; SSE writes are bounded by the Hub's per-client buffer drop.
	}

	// Background goroutines tied to the serve lifetime.
	var wg sync.WaitGroup
	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()

	// Evict idle per-IP rate-limit buckets in the background (no-op when the limiter
	// is disabled), tied to the serve lifetime so it stops on shutdown.
	if s.rateLimit != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.rateLimit.runJanitor(bgCtx)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.runLiveTail(bgCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error().Err(err).Msg("api: live tail stopped with error")
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runLabelRefresh(bgCtx)
	}()

	serveErr := make(chan error, 1)
	go func() {
		log.Info().Str("addr", s.addr).Msg("api: listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("api: listen and serve: %w", err)
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		// Listener failed on its own (bind error). Tear down the rest.
		bgCancel()
		s.hub.Close()
		wg.Wait()
		return err
	case <-ctx.Done():
		log.Info().Msg("api: shutdown requested")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		shutdownErr := srv.Shutdown(shutdownCtx)
		// Closing the Hub unblocks every SSE handler's range loop so Shutdown can
		// finish draining and the goroutines exit.
		s.hub.Close()
		bgCancel()
		wg.Wait()
		if shutdownErr != nil {
			return fmt.Errorf("api: shutdown: %w", shutdownErr)
		}
		if err := <-serveErr; err != nil {
			return err
		}
		return nil
	}
}

// runLabelRefresh periodically refreshes the pool-label snapshot until ctx is
// cancelled.
func (s *Server) runLabelRefresh(ctx context.Context) {
	ticker := time.NewTicker(labelRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshLabels(ctx)
		}
	}
}

// runLiveTail is the Hub's producer: every tailInterval it polls the store for signals
// with id past the last seen and broadcasts each as an SSE "signal" event. Reading from
// the DB keeps the feed restart-safe and decoupled from the hot path. It seeds its
// cursor from MaxSignalID so a fresh tail does not replay history (per-client
// connect-time replay is handled in the SSE handler). A poll error is logged and the
// loop continues; only ctx cancellation ends it. Each poll is bounded by tailBatch.
func (s *Server) runLiveTail(ctx context.Context) error {
	lastID := int64(0)
	if maxID, ok, err := s.db.MaxSignalID(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Error().Err(err).Msg("api: live tail seed max id failed; starting from 0")
	} else if ok {
		lastID = maxID
	}
	log.Info().Int64("from_id", lastID).Dur("interval", s.tailInterval).Msg("api: live tail started")

	ticker := time.NewTicker(s.tailInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			rows, err := s.db.SignalsAfterID(ctx, lastID, tailBatch)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.Error().Err(err).Int64("after_id", lastID).Msg("api: live tail poll failed, will retry next tick")
				continue
			}
			for i := range rows {
				data, merr := marshalJSON(signalToDTO(rows[i], s.label, s.explorerBase))
				if merr != nil {
					// A single un-marshalable row must not wedge the cursor; log,
					// advance past it, continue.
					log.Error().Err(merr).Int64("signal_id", rows[i].Signal.ID).Msg("api: live tail marshal signal failed; skipping")
					lastID = rows[i].Signal.ID
					continue
				}
				s.hub.Broadcast(sseEvent{id: rows[i].Signal.ID, name: eventSignal, data: data})
				lastID = rows[i].Signal.ID
			}
		}
	}
}

// routes builds the mux. Every route is GET-only and wrapped with CORS (GET/OPTIONS from
// any origin, since this is a public read-only API).
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.handleIndex) // minimal HTML landing page

	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/signals", s.handleSignals)
	mux.HandleFunc("/api/signals/", s.handleSignalByID) // /api/signals/{id}
	mux.HandleFunc("/api/actors", s.handleActors)
	mux.HandleFunc("/api/actors/", s.handleActorByAddr) // /api/actors/{addr}
	mux.HandleFunc("/api/pools", s.handlePools)
	mux.HandleFunc("/api/pools/", s.handlePoolByAddr) // /api/pools/{addr}
	mux.HandleFunc("/api/series", s.handleSeries)
	mux.HandleFunc("/api/live", s.handleLive) // SSE

	// CORS wraps everything; gzip wraps the JSON routes (it skips text/event-stream, so
	// SSE streams unbuffered). The rate limiter sits outside gzip but inside CORS, so a
	// 429 still carries CORS headers; it limits only /api/* and a nil limiter is a
	// transparent pass-through.
	return withCORS(s.rateLimit.wrapAPI(withGzip(mux)))
}

// handleStats serves GET /api/stats.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !okGet(w, r) {
		return
	}
	summary, err := s.db.StatsSummary(r.Context())
	if err != nil {
		serverError(w, r, "load stats", err)
		return
	}
	writeJSON(w, http.StatusOK, statsToDTO(summary, time.Now()))
}

// handleSignals serves GET /api/signals?limit&offset&type&pool&actor&graded.
func (s *Server) handleSignals(w http.ResponseWriter, r *http.Request) {
	if !okGet(w, r) {
		return
	}
	q := r.URL.Query()
	limit := clampLimit(intParam(q.Get("limit"), defaultSignalLimit))
	offset := clampOffset(intParam(q.Get("offset"), 0))

	f := store.SignalFilter{Limit: limit, Offset: offset}
	if t := q.Get("type"); t != "" {
		tv, perr := strconv.ParseInt(t, 10, 16)
		if perr != nil {
			badRequest(w, "invalid type (want an integer)")
			return
		}
		t16 := int16(tv)
		f.Type = &t16
	}
	f.Pool = strings.ToLower(strings.TrimSpace(q.Get("pool")))
	f.Actor = strings.ToLower(strings.TrimSpace(q.Get("actor")))
	// graded=true|1 restricts to signals that already carry a graded outcome (the
	// track-record list), so a caller need not page the live feed to find matured calls.
	if g := q.Get("graded"); g == "true" || g == "1" {
		f.Graded = true
	}

	rows, total, err := s.db.ListSignalsFiltered(r.Context(), f)
	if err != nil {
		serverError(w, r, "list signals", err)
		return
	}
	writeJSON(w, http.StatusOK, SignalListDTO{
		Items:  signalsToDTOs(rows, s.label, s.explorerBase),
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// handleSignalByID serves GET /api/signals/{id}.
func (s *Server) handleSignalByID(w http.ResponseWriter, r *http.Request) {
	if !okGet(w, r) {
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/signals/")
	if idStr == "" || strings.Contains(idStr, "/") {
		notFound(w)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		badRequest(w, "invalid signal id")
		return
	}
	row, found, err := s.db.SignalByID(r.Context(), id)
	if err != nil {
		serverError(w, r, "load signal", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	// Decode the triggering swap on demand (detail-only); nil for flow or an undecodable
	// log.
	swap := s.swapActionForSignal(r.Context(), row.Signal)
	writeJSON(w, http.StatusOK, signalToDetailDTO(row, s.label, s.explorerBase, swap))
}

// handleActors serves GET /api/actors?limit.
func (s *Server) handleActors(w http.ResponseWriter, r *http.Request) {
	if !okGet(w, r) {
		return
	}
	limit := clampLimit(intParam(r.URL.Query().Get("limit"), defaultActorLimit))
	rows, err := s.db.ActorLeaderboard(r.Context(), limit)
	if err != nil {
		serverError(w, r, "list actors", err)
		return
	}
	items := make([]ActorDTO, 0, len(rows))
	for i := range rows {
		items = append(items, actorToDTO(rows[i], s.explorerBase))
	}
	writeJSON(w, http.StatusOK, ActorListDTO{Items: items})
}

// handleActorByAddr serves GET /api/actors/{addr}.
func (s *Server) handleActorByAddr(w http.ResponseWriter, r *http.Request) {
	if !okGet(w, r) {
		return
	}
	addr := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/api/actors/"))
	if addr == "" || strings.Contains(addr, "/") {
		notFound(w)
		return
	}
	row, found, err := s.db.ActorByAddress(r.Context(), addr)
	if err != nil {
		serverError(w, r, "load actor", err)
		return
	}
	if !found {
		notFound(w)
		return
	}

	// Recent signals + windowed accumulation footprint for the detail view.
	const actorSignalLimit = 50
	sigRows, err := s.db.RecentSignalsByActor(r.Context(), addr, actorSignalLimit)
	if err != nil {
		serverError(w, r, "load actor signals", err)
		return
	}
	windowHours := defaultActorWindowHours
	since := time.Now().Add(-time.Duration(windowHours) * time.Hour)
	swaps, pools, err := s.db.ActorAccumulationSince(r.Context(), addr, since)
	if err != nil {
		serverError(w, r, "load actor accumulation", err)
		return
	}

	recentSwaps, err := s.actorRecentSwaps(r.Context(), addr, actorRecentSwapLimit)
	if err != nil {
		serverError(w, r, "load actor recent swaps", err)
		return
	}

	writeJSON(w, http.StatusOK, ActorDetailDTO{
		ActorDTO: actorToDTO(row, s.explorerBase),
		Signals:  signalsToDTOs(sigRows, s.label, s.explorerBase),
		Accumulation: AccumulationDTO{
			Swaps:       swaps,
			Pools:       pools,
			WindowHours: windowHours,
		},
		RecentSwaps: recentSwaps,
	})
}

// defaultActorWindowHours is the accumulation window shown in the actor detail
// view. 24h matches the smart-money detector's default window (display only).
const defaultActorWindowHours = 24

// actorRecentSwapLimit caps the decoded recent-swap activity path on the actor
// detail endpoint (~25, newest first) so the path stays bounded and light.
const actorRecentSwapLimit = 25

// tokenMetaForPools loads the display-only token metadata for every token0/token1 in the
// given pool rows, in one bounded lookup keyed by address. Best-effort: a failure logs
// and returns an empty map (cosmetic data must never fail a pool response). Never nil.
func (s *Server) tokenMetaForPools(ctx context.Context, rows []store.PoolRow) map[string]store.TokenMeta {
	addrs := make([]string, 0, len(rows)*2)
	for i := range rows {
		addrs = append(addrs, rows[i].Pool.Token0, rows[i].Pool.Token1)
	}
	return s.tokenMetaFor(ctx, addrs)
}

// tokenMetaFor loads token_meta for an arbitrary address set (best-effort; empty map
// on failure or empty input). Shared by the pool and actor-swap paths.
func (s *Server) tokenMetaFor(ctx context.Context, addrs []string) map[string]store.TokenMeta {
	if len(addrs) == 0 {
		return map[string]store.TokenMeta{}
	}
	tm, err := s.db.TokenMetaByAddresses(ctx, addrs)
	if err != nil {
		if ctx.Err() == nil {
			log.Error().Err(err).Msg("api: load token_meta failed; rendering without symbols/logos")
		}
		return map[string]store.TokenMeta{}
	}
	return tm
}

// swapActionForSignal decodes a signal's triggering swap for the detail endpoint, or nil
// when there is nothing to show (a flow signal, a missing pool/log, or an undecodable
// swap). It loads the raw log by event_ref and the pool row, then defers to
// swapActionFromLog. A DB error degrades to nil (swap:null) rather than failing the
// request: the swap is enrichment, not the core payload.
func (s *Server) swapActionForSignal(ctx context.Context, sig store.Signal) *SwapActionDTO {
	tx, logIndex, ok := parseEventRef(sig.EventRef)
	if !ok {
		return nil // flow (type 1) or no parseable event ref: no single triggering swap
	}

	l, found, err := s.db.RawLogByRef(ctx, s.chainID, tx, logIndex)
	if err != nil {
		if ctx.Err() == nil {
			log.Error().Err(err).Int64("signal_id", sig.ID).Str("tx", tx).Msg("api: load triggering log failed; swap omitted")
		}
		return nil
	}
	if !found {
		return nil // the log is not (yet) in raw_logs; nothing to decode
	}

	pr, found, err := s.db.PoolByAddress(ctx, sig.Pool)
	if err != nil {
		if ctx.Err() == nil {
			log.Error().Err(err).Int64("signal_id", sig.ID).Str("pool", sig.Pool).Msg("api: load pool for swap failed; swap omitted")
		}
		return nil
	}
	if !found {
		return nil
	}

	tokenMeta := s.tokenMetaFor(ctx, []string{pr.Pool.Token0, pr.Pool.Token1})
	return swapActionFromLog(l, pr.Pool, tokenMeta, s.explorerBase)
}

// actorRecentSwaps builds the actor's decoded recent-swap activity path: up to `limit`
// large swaps (newest first), the registry rows for the pools they touched, and their
// token_meta, mapped to directional actions (indeterminate-direction swaps skipped).
// Returns a non-nil (possibly empty) slice so the JSON is [] not null.
func (s *Server) actorRecentSwaps(ctx context.Context, actor string, limit int) ([]ActorSwapDTO, error) {
	ledger, err := s.db.RecentLargeSwapsByActor(ctx, actor, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ActorSwapDTO, 0, len(ledger))
	if len(ledger) == 0 {
		return out, nil
	}

	// Resolve each distinct pool's registry row once for the decode. A pool absent from
	// the registry simply drops its swaps from the path.
	pools := make(map[string]store.Pool)
	tokenAddrs := make([]string, 0, len(ledger)*2)
	for i := range ledger {
		key := strings.ToLower(ledger[i].Pool)
		if _, seen := pools[key]; seen {
			continue
		}
		pr, found, perr := s.db.PoolByAddress(ctx, key)
		if perr != nil {
			return nil, perr
		}
		if !found {
			continue
		}
		pools[key] = pr.Pool
		tokenAddrs = append(tokenAddrs, pr.Pool.Token0, pr.Pool.Token1)
	}

	tokenMeta := s.tokenMetaFor(ctx, tokenAddrs)

	for i := range ledger {
		pool, ok := pools[strings.ToLower(ledger[i].Pool)]
		if !ok {
			continue // pool not in registry: cannot decode tokens, skip this swap
		}
		dto, ok := actorSwapToDTO(ledger[i], pool, s.label, tokenMeta, s.explorerBase)
		if !ok {
			continue // indeterminate direction: skip
		}
		out = append(out, dto)
	}
	return out, nil
}

// parseEventRef parses a signal's internal event_ref "txhash:logindex" into its parts.
// ok=false for an empty ref, no ':' separator, or an unparseable/negative log index, so
// the caller treats the signal as having no single triggering swap. The hash is lowercased.
func parseEventRef(eventRef string) (txHash string, logIndex int, ok bool) {
	ref := strings.TrimSpace(eventRef)
	if ref == "" {
		return "", 0, false
	}
	i := strings.IndexByte(ref, ':')
	if i <= 0 || i == len(ref)-1 {
		return "", 0, false
	}
	tx := strings.ToLower(strings.TrimSpace(ref[:i]))
	idx, err := strconv.Atoi(strings.TrimSpace(ref[i+1:]))
	if err != nil || idx < 0 || tx == "" {
		return "", 0, false
	}
	return tx, idx, true
}

// handlePools serves GET /api/pools.
func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	if !okGet(w, r) {
		return
	}
	rows, err := s.db.PoolsWithActivityAndStats(r.Context())
	if err != nil {
		serverError(w, r, "list pools", err)
		return
	}
	tokenMeta := s.tokenMetaForPools(r.Context(), rows)
	items := make([]PoolDTO, 0, len(rows))
	for i := range rows {
		items = append(items, poolToDTO(rows[i], s.explorerBase, tokenMeta))
	}
	writeJSON(w, http.StatusOK, PoolListDTO{Items: items})
}

// handlePoolByAddr serves GET /api/pools/{addr}.
func (s *Server) handlePoolByAddr(w http.ResponseWriter, r *http.Request) {
	if !okGet(w, r) {
		return
	}
	addr := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/api/pools/"))
	if addr == "" || strings.Contains(addr, "/") {
		notFound(w)
		return
	}
	row, found, err := s.db.PoolByAddress(r.Context(), addr)
	if err != nil {
		serverError(w, r, "load pool", err)
		return
	}
	if !found {
		notFound(w)
		return
	}

	const poolSeriesLimit = 288 // 24h of 5-min buckets
	const poolSignalLimit = 50
	buckets, err := s.db.RecentBucketsForPool(r.Context(), addr, poolSeriesLimit)
	if err != nil {
		serverError(w, r, "load pool series", err)
		return
	}
	sigRows, err := s.db.RecentSignalsByPool(r.Context(), addr, poolSignalLimit)
	if err != nil {
		serverError(w, r, "load pool signals", err)
		return
	}

	series := make([]SeriesBucketDTO, 0, len(buckets))
	for i := range buckets {
		series = append(series, bucketToSeriesDTO(buckets[i]))
	}

	tokenMeta := s.tokenMetaForPools(r.Context(), []store.PoolRow{row})

	writeJSON(w, http.StatusOK, PoolDetailDTO{
		PoolDTO:       poolToDTO(row, s.explorerBase, tokenMeta),
		Series:        series,
		RecentSignals: signalsToDTOs(sigRows, s.label, s.explorerBase),
	})
}

// handleSeries serves GET /api/series?pool&metric&hours.
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	if !okGet(w, r) {
		return
	}
	q := r.URL.Query()
	pool := strings.ToLower(strings.TrimSpace(q.Get("pool")))
	if pool == "" {
		badRequest(w, "pool is required")
		return
	}
	metric := strings.TrimSpace(q.Get("metric"))
	if metric == "" {
		metric = "swap_count"
	}
	if _, ok := allowedSeriesMetrics[metric]; !ok {
		badRequest(w, "invalid metric (want swap_count|vol0|vol1|netflow0|netflow1)")
		return
	}
	hours := intParam(q.Get("hours"), 24)
	if hours <= 0 {
		hours = 24
	}
	if hours > maxSeriesHours {
		hours = maxSeriesHours
	}

	_, pts, err := s.db.SeriesForPool(r.Context(), pool, metric, hours)
	if err != nil {
		serverError(w, r, "load series", err)
		return
	}
	points := make([]SeriesPointDTO, 0, len(pts))
	for i := range pts {
		points = append(points, SeriesPointDTO{
			TS:    rfc3339(pts[i].TS),
			Value: pts[i].Value.String(),
		})
	}
	writeJSON(w, http.StatusOK, SeriesDTO{Pool: pool, Metric: metric, Points: points})
}

// allowedSeriesMetrics mirrors store.SeriesForPool's allow-list so the handler rejects
// an unknown metric with a 400 before touching the DB.
var allowedSeriesMetrics = map[string]struct{}{
	"swap_count": {}, "vol0": {}, "vol1": {}, "netflow0": {}, "netflow1": {},
}

// maxSeriesHours caps the series window so one request cannot scan an unbounded
// time range. 14 days of 5-min buckets is plenty for a chart.
const maxSeriesHours = 24 * 14

// okGet enforces GET (OPTIONS is short-circuited upstream by the CORS wrapper). A
// non-GET method gets a 405 with Allow; returns false when already handled.
func okGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	w.Header().Set("Allow", "GET, OPTIONS")
	writeJSON(w, http.StatusMethodNotAllowed, ErrorDTO{Error: "method not allowed"})
	return false
}

// writeJSON marshals v and writes it with the given status and JSON content type. A
// marshal failure is logged and downgraded to a 500 (nothing has been written yet).
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := marshalJSON(v)
	if err != nil {
		log.Error().Err(err).Msg("api: marshal response failed")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		// Best-effort static error body.
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, werr := w.Write(body); werr != nil {
		// Client likely went away mid-write; log at debug, nothing else to do.
		log.Debug().Err(werr).Msg("api: write response body failed")
	}
}

// serverError logs the underlying error (with context) and returns a generic 500 to
// the client. Internal error detail stays in the logs, not the response body.
func serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		// The client disconnected / request was cancelled; not a server fault.
		log.Debug().Err(err).Str("what", what).Msg("api: request cancelled")
		return
	}
	log.Error().Err(err).Str("what", what).Str("path", r.URL.Path).Msg("api: handler error")
	writeJSON(w, http.StatusInternalServerError, ErrorDTO{Error: "internal error"})
}

// badRequest returns a 400 with a client-safe message.
func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, ErrorDTO{Error: msg})
}

// notFound returns a 404 JSON error.
func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, ErrorDTO{Error: "not found"})
}

// intParam parses a query int, returning fallback on empty/invalid (lenient: a
// junk limit/offset/hours falls back to the default rather than erroring).
func intParam(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

// clampLimit bounds a requested limit to [1, maxLimit].
func clampLimit(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}

// clampOffset bounds a requested list offset to [0, maxSignalsOffset], so a
// negative offset floors at 0 and a very large one cannot force a deep index scan.
func clampOffset(n int) int {
	if n < 0 {
		return 0
	}
	if n > maxSignalsOffset {
		return maxSignalsOffset
	}
	return n
}

// handleIndex serves the static landing page (indexHTML) at "/", 404 for any other path.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		notFound(w)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, ErrorDTO{Error: "method not allowed"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(indexHTML))
}
