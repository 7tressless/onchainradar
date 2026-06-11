package chain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/rs/zerolog/log"

	"ocr/internal/config"
	"ocr/internal/lending"
	"ocr/internal/lstflow"
	"ocr/internal/store"
)

// logChunkBlocks caps the inclusive block span of a single eth_getLogs call.
// Most providers reject or silently truncate ranges wider than a few thousand
// blocks, so we page through large gaps in fixed-size chunks.
const logChunkBlocks uint64 = 2000

// coldStartLookback is how far behind the chain head RunOnce begins when no
// cursor row exists yet, so a fresh deployment captures recent history without
// replaying the entire chain.
const coldStartLookback uint64 = 100

// iterationBackoff is the pause after a failed Run iteration before retrying,
// so a transient RPC or DB outage does not spin the loop hot.
const iterationBackoff = 5 * time.Second

// rpcCallTimeout bounds each per-iteration RPC round-trip (LatestBlock,
// HeaderByNumber, the GetLogs head fetch) so an unresponsive node surfaces as a logged,
// retryable error instead of stalling the loop. It also caps GetLogs's own backoff.
const rpcCallTimeout = 15 * time.Second

// protocolContracts is the registry of non-pool contracts the collector fetches into
// raw_logs beyond the swap pools: the Aave V3 Pool proxy (Borrow / LiquidationCall feed
// the lending detector, types 5/6) and mETH + cmETH (ERC-20 Transfer feeds the LST-flow
// detector, type 4). None is a tradable pool, so their logs never enter swap buckets.
// Each address is pulled from its decoder package's registry so the collected and decoder
// sets cannot drift.
var protocolContracts = buildProtocolContracts()

// buildProtocolContracts assembles the protocol-contracts address list from each decoder
// package's registry, so a package exposing its registry as a slice (lstflow.LSTTokens)
// contributes without re-hard-coding the addresses here.
func buildProtocolContracts() []string {
	out := []string{
		lending.AavePoolAddress,
	}
	out = append(out, lstflow.LSTTokens()...)
	return out
}

// Collector polls the chain for logs emitted by a fixed set of contracts and
// persists them as raw_logs, advancing a per-chain cursor for restart safety. It is
// the ingest producer; decoding and normalization happen downstream. The collected
// set is the curated swap pools plus the protocol contracts (protocolContracts),
// filtered together in one eth_getLogs call.
type Collector struct {
	c   *Client
	db  *store.DB
	cfg *config.Config
	// addrs is every contract the collector filters logs by (enabled swap pools plus
	// protocol contracts). One flat list keeps cursor + chunking restart-safe for all.
	addrs []common.Address
}

// NewCollector builds a Collector. The pool registry plus the protocol-contracts
// registry are merged into one address list so eth_getLogs fetches both in a single
// filtered call. Addresses are lowercased to match the storage convention and
// de-duplicated so the filter never lists one twice.
func NewCollector(c *Client, db *store.DB, cfg *config.Config, pools []store.Pool) *Collector {
	seen := make(map[common.Address]struct{}, len(pools)+len(protocolContracts))
	addrs := make([]common.Address, 0, len(pools)+len(protocolContracts))
	add := func(hexAddr string) {
		a := common.HexToAddress(strings.ToLower(hexAddr))
		if _, dup := seen[a]; dup {
			return
		}
		seen[a] = struct{}{}
		addrs = append(addrs, a)
	}
	for _, p := range pools {
		add(p.Address)
	}
	// Protocol contracts are not in the pools table; fetched so their events decode
	// from raw_logs.
	for _, pc := range protocolContracts {
		add(pc)
	}
	return &Collector{c: c, db: db, cfg: cfg, addrs: addrs}
}

// RunOnce advances the collector by a single scan: it reads the cursor, fetches
// logs for the registered pools across the newly-finalized block range in
// bounded chunks, persists them, and stores the new cursor. It is a no-op when
// the confirmed head has not advanced past the cursor.
func (col *Collector) RunOnce(ctx context.Context) error {
	cursor, hasCursor, err := col.db.GetCursor(ctx, col.cfg.ChainID)
	if err != nil {
		return fmt.Errorf("chain: collector get cursor: %w", err)
	}

	lctx, lcancel := context.WithTimeout(ctx, rpcCallTimeout)
	latest, err := col.c.LatestBlock(lctx)
	lcancel()
	if err != nil {
		return fmt.Errorf("chain: collector latest block: %w", err)
	}

	confirmations := uint64(0)
	if col.cfg.Confirmations > 0 {
		confirmations = uint64(col.cfg.Confirmations)
	}
	if latest < confirmations {
		// Chain head is shallower than the confirmation depth (only plausible
		// on a brand-new devnet); nothing is final yet.
		return nil
	}
	toBlock := latest - confirmations

	// Determine the first block to scan (inclusive).
	var fromBlock uint64
	if hasCursor {
		// cursor is the last fully-processed block; resume after it.
		if cursor < 0 {
			return fmt.Errorf("chain: collector negative cursor %d", cursor)
		}
		fromBlock = uint64(cursor) + 1
	} else {
		// Cold start: begin a fixed lookback behind the confirmed head.
		if toBlock > coldStartLookback {
			fromBlock = toBlock - coldStartLookback
		}
	}

	// No-op when the confirmed head has not advanced past what we've processed.
	// fromBlock > toBlock covers both "cursor already at/after toBlock" and a
	// cold start where toBlock <= lookback.
	if fromBlock > toBlock {
		return nil
	}

	var (
		blocksScanned uint64
		inserted      int64
	)
	for chunkStart := fromBlock; chunkStart <= toBlock; chunkStart += logChunkBlocks {
		chunkEnd := chunkStart + logChunkBlocks - 1
		if chunkEnd > toBlock {
			chunkEnd = toBlock
		}

		gctx, gcancel := context.WithTimeout(ctx, rpcCallTimeout)
		logs, err := col.c.GetLogs(gctx, chunkStart, chunkEnd, col.addrs)
		gcancel()
		if err != nil {
			return fmt.Errorf("chain: collector get logs [%d,%d]: %w", chunkStart, chunkEnd, err)
		}

		rows := make([]store.RawLog, 0, len(logs))
		if len(logs) > 0 {
			// Interpolate block times between the chunk's endpoint headers: two header
			// calls per chunk, not one per block (what makes backfill fast).
			timeAt, err := col.blockTimeFunc(ctx, chunkStart, chunkEnd)
			if err != nil {
				return fmt.Errorf("chain: collector block times [%d,%d]: %w", chunkStart, chunkEnd, err)
			}
			for i := range logs {
				rows = append(rows, col.toRawLog(logs[i], timeAt(logs[i].BlockNumber)))
			}
		}

		ins, err := col.db.InsertRawLogs(ctx, rows)
		if err != nil {
			return fmt.Errorf("chain: collector insert raw logs: %w", err)
		}

		blocksScanned += chunkEnd - chunkStart + 1
		inserted += ins

		// Advance the cursor per chunk so a long scan is durable: a mid-scan failure
		// resumes from the last completed chunk, not the whole range.
		if err := col.db.SetCursor(ctx, col.cfg.ChainID, int64(chunkEnd)); err != nil {
			return fmt.Errorf("chain: collector set cursor: %w", err)
		}
	}

	log.Info().
		Int64("chain_id", col.cfg.ChainID).
		Uint64("from_block", fromBlock).
		Uint64("to_block", toBlock).
		Uint64("blocks_scanned", blocksScanned).
		Int64("logs_inserted", inserted).
		Msg("chain: collector scan complete")

	return nil
}

// Backfill rewinds the cursor lookbackBlocks behind the confirmed head and then
// runs a single catch-up scan, so a fresh deployment can build baselines from
// recent history instead of waiting for new blocks to accrue. Inserts are
// idempotent and the cursor advances per chunk, so a failed backfill is safe to
// re-run (it simply re-seeds and re-scans, deduping on the raw_logs PK).
func (col *Collector) Backfill(ctx context.Context, lookbackBlocks uint64) error {
	latest, err := col.c.LatestBlock(ctx)
	if err != nil {
		return fmt.Errorf("chain: backfill latest block: %w", err)
	}

	var from uint64
	if latest > lookbackBlocks {
		from = latest - lookbackBlocks
	}

	// Seed the cursor just before the backfill start; RunOnce then resumes at
	// `from` and scans forward to the confirmed head in chunks.
	seed := int64(0)
	if from > 0 {
		seed = int64(from) - 1
	}
	if err := col.db.SetCursor(ctx, col.cfg.ChainID, seed); err != nil {
		return fmt.Errorf("chain: backfill seed cursor: %w", err)
	}

	log.Info().
		Int64("chain_id", col.cfg.ChainID).
		Uint64("from_block", from).
		Uint64("head", latest).
		Uint64("lookback_blocks", lookbackBlocks).
		Msg("chain: backfill starting (single catch-up scan)")

	if err := col.RunOnce(ctx); err != nil {
		return fmt.Errorf("chain: backfill scan: %w", err)
	}

	log.Info().Int64("chain_id", col.cfg.ChainID).Msg("chain: backfill complete")
	return nil
}

// blockTimeFunc maps a block number in [from,to] to its timestamp by linear
// interpolation between the two endpoint headers, bounding header RPC calls to two per
// chunk. Mantle's ~2s cadence keeps the interpolation error far below the 5-min bucket.
func (col *Collector) blockTimeFunc(ctx context.Context, from, to uint64) (func(uint64) time.Time, error) {
	fctx, fcancel := context.WithTimeout(ctx, rpcCallTimeout)
	hFrom, err := col.c.HeaderByNumber(fctx, from)
	fcancel()
	if err != nil {
		return nil, err
	}
	tFrom := time.Unix(int64(hFrom.Time), 0).UTC()
	if to <= from {
		return func(uint64) time.Time { return tFrom }, nil
	}

	tctx, tcancel := context.WithTimeout(ctx, rpcCallTimeout)
	hTo, err := col.c.HeaderByNumber(tctx, to)
	tcancel()
	if err != nil {
		return nil, err
	}
	tTo := time.Unix(int64(hTo.Time), 0).UTC()

	return func(blk uint64) time.Time {
		return interpolateBlockTime(tFrom, tTo, from, to, blk)
	}, nil
}

// interpolateBlockTime maps blk to a timestamp by linear interpolation between block
// `from` at tFrom and block `to` at tTo, clamping outside [from,to] and collapsing a
// degenerate range to tFrom. Pure and deterministic (no RPC, no clock), so unit-testable.
func interpolateBlockTime(tFrom, tTo time.Time, from, to, blk uint64) time.Time {
	switch {
	case to <= from || blk <= from:
		return tFrom
	case blk >= to:
		return tTo
	default:
		span := float64(to - from)
		secs := tTo.Sub(tFrom).Seconds()
		off := time.Duration(secs * float64(blk-from) / span * float64(time.Second))
		return tFrom.Add(off)
	}
}

// toRawLog converts a go-ethereum log into a store.RawLog with a pre-resolved
// block time (see blockTimeFunc).
func (col *Collector) toRawLog(l types.Log, bt time.Time) store.RawLog {
	return store.RawLog{
		ChainID:     col.cfg.ChainID,
		BlockNumber: int64(l.BlockNumber),
		BlockTime:   bt,
		TxHash:      l.TxHash.Hex(),
		LogIndex:    int(l.Index),
		Address:     l.Address.Hex(),
		Topic0:      topicAt(l.Topics, 0),
		Topic1:      topicAt(l.Topics, 1),
		Topic2:      topicAt(l.Topics, 2),
		Topic3:      topicAt(l.Topics, 3),
		Data:        hexutil.Encode(l.Data),
	}
}

// Run loops RunOnce on the configured poll interval until the context is
// cancelled. Per-iteration errors are logged and retried after a short backoff
// rather than crashing the process, so a transient RPC/DB blip is survivable.
func (col *Collector) Run(ctx context.Context) error {
	interval := time.Duration(col.cfg.PollIntervalSec) * time.Second
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Info().
		Int64("chain_id", col.cfg.ChainID).
		Int("contracts", len(col.addrs)).
		Dur("interval", interval).
		Msg("chain: collector started")

	for {
		if err := col.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				// Cancellation surfaced through a downstream call; exit cleanly.
				return ctx.Err()
			}
			log.Error().
				Err(err).
				Int64("chain_id", col.cfg.ChainID).
				Msg("chain: collector iteration failed; backing off")

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(iterationBackoff):
			}
			continue
		}

		select {
		case <-ctx.Done():
			log.Info().Int64("chain_id", col.cfg.ChainID).Msg("chain: collector stopped")
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// topicAt returns the hex of the topic at index i, or "" when absent. The store
// layer maps the empty string to a NULL topic column.
func topicAt(topics []common.Hash, i int) string {
	if i < len(topics) {
		return topics[i].Hex()
	}
	return ""
}
