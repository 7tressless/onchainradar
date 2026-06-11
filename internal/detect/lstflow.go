package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"ocr/internal/attest"
	"ocr/internal/config"
	"ocr/internal/lstflow"
	"ocr/internal/store"
)

// The LST-flow detector (signal_type 4, metric "lst_flow") follows large
// Liquid-Staking-Token (mETH / cmETH) ERC-20 Transfers on Mantle, classified by
// counterparty into mint (from 0x0), burn (to 0x0), or move (wallet-to-wallet). A
// transfer where either side is a tracked DEX pool is skipped: it is a swap leg
// already covered by our swap signals, the key noise filter.
//
// It mirrors the lending detector's event-keyed shape (lending.go), keyed and
// idempotent on (pool, signal_type, event_ref). Large LST transfers are sparse, so
// the gate is an absolute whole-token floor (LSTFlowMinTokens) with no baseline or
// z-score, and the on-chain score is a log-scaled size band (scoreLSTFlow).
//
// A wallet's burst of transfers is aggregated, not repeated: the first transfer opens
// (and is attested at) a signal; later transfers by the same actor within
// LSTFlowWindowMin fold into it, growing the running total, count, and span in the
// payload, so one relayer cannot spam the feed. Only the opening transfer is attested;
// the aggregate that follows is display state.
const (
	// lstFlowRecentWindowMin bounds the candidate set to the last N minutes
	// (mirroring lendingRecentWindowMin); idempotency on event_ref makes the overlap
	// across ticks harmless.
	lstFlowRecentWindowMin = 10

	// signalTypeLSTFlow is the int16-typed local alias of store.SignalTypeLSTFlow
	// (1/2/3 flow/whale/smart_money; 4 LST flow; 5/6 lending).
	signalTypeLSTFlow int16 = store.SignalTypeLSTFlow

	// metricLSTFlow is the metric string for the family, surfaced verbatim in the
	// API + enricher + alert.
	metricLSTFlow = "lst_flow"

	// lstFlowDecimals is the decimals of both LST tokens (18); the whole-token floor
	// scales to raw units by 10^lstFlowDecimals.
	lstFlowDecimals = lstflow.TokenDecimals
)

// LSTFlowDetector emits lst_flow (type 4) signals from large mETH/cmETH ERC-20
// Transfers in raw_logs. It holds the store, the config (the whole-token floor), and
// a best-effort SenderResolver for tx.origin. Shaped exactly like LendingDetector.
type LSTFlowDetector struct {
	db  *store.DB
	cfg *config.Config
	// res refines a transfer's actor to tx.origin. Best-effort + optional: a nil
	// resolver or resolve failure falls back to the non-zero, non-pool counterparty
	// and the signal still fires. Not part of the hash.
	res SenderResolver
}

// NewLSTFlowDetector constructs an LSTFlowDetector. cfg supplies LSTFlowMinTokens (the
// whole-token floor) and LSTFlowWindowMin (the per-actor aggregation window). res
// (best-effort, may be nil) refines the actor to tx.origin; *chain.Client satisfies it
// via TxSender.
func NewLSTFlowDetector(db *store.DB, cfg *config.Config, res SenderResolver) *LSTFlowDetector {
	return &LSTFlowDetector{db: db, cfg: cfg, res: res}
}

// lstFlowStore is the minimal store surface ScanOnce needs: the bounded LST-token
// raw-log read, the tracked-pool membership set (the noise-filter exclude set), the
// idempotent signal insert, and the open-aggregate read/update that lets a wallet's
// burst fold into one signal. A mock lets the whole path be tested with no Postgres.
type lstFlowStore interface {
	RawLogsSince(ctx context.Context, since time.Time, pools []string) ([]store.RawLog, error)
	ListPoolAddresses(ctx context.Context) (map[string]struct{}, error)
	InsertSignal(ctx context.Context, s store.Signal) (int64, bool, error)
	// OpenLSTSignalByActor returns the actor's most recent type-4 signal created since the
	// window start (its id + payload), the open aggregate to fold into; UpdateSignalAggregate
	// rewrites that row's payload + size_usd; TokenPricesUSD supplies the display-only size.
	OpenLSTSignalByActor(ctx context.Context, actor string, since time.Time) (int64, []byte, bool, error)
	UpdateSignalAggregate(ctx context.Context, id int64, payload []byte, sizeUSD *decimal.Decimal) error
	TokenPricesUSD(ctx context.Context) (map[string]decimal.Decimal, error)
}

// lstFlowPayload is the JSON written to an lst_flow signal's Payload. The first block
// describes the opening transfer (so the event-keyed hash is reproducible); the aggregate
// block grows as later transfers by the same actor fold in within the window.
type lstFlowPayload struct {
	Token     string `json:"token"`      // the LST token contract address (subject)
	Symbol    string `json:"symbol"`     // "mETH" / "cmETH" (display)
	Direction string `json:"direction"`  // opening transfer: "mint" | "burn" | "move"
	From      string `json:"from"`       // opening transfer sender (0x0 for a mint)
	To        string `json:"to"`         // opening transfer recipient (0x0 for a burn)
	Value     string `json:"value"`      // opening transfer raw units (18-dec)
	ValueLST  string `json:"value_lst"`  // opening transfer whole-token value
	Tx        string `json:"tx"`         // opening transfer tx hash (lowercase)
	LogIndex  int    `json:"log_index"`  // opening transfer log index
	BlockTime string `json:"block_time"` // opening transfer time (RFC3339, UTC)

	// Aggregate over the window (a wallet's burst rolled into this one signal).
	Transfers int      `json:"transfers"`  // count of transfers folded in (>= 1)
	TotalLST  string   `json:"total_lst"`  // sum of whole-token values across all of them
	FirstAt   string   `json:"first_at"`   // earliest transfer time (RFC3339, UTC)
	LastAt    string   `json:"last_at"`    // latest transfer time, the span end
	EventRefs []string `json:"event_refs"` // tx:logindex of every folded transfer (idempotency)
}

// lstTransfer is one decoded, qualifying LST transfer (it cleared the floor and is not a
// DEX swap leg), with its resolved actor. The caller decides whether to open a new signal
// or fold it into the actor's open aggregate.
type lstTransfer struct {
	token     string
	symbol    string
	direction string
	from      string
	to        string
	valueRaw  decimal.Decimal // raw 18-dec amount
	valueLST  decimal.Decimal // whole-token amount
	actor     string          // resolved tx.origin, or the counterparty fallback
	tx        string          // lowercase tx hash
	logIndex  int
	ref       string // "tx:logindex"
	blockTime time.Time
}

// ScanOnce reads the LST tokens' recent logs once and either opens a type-4 signal per
// qualifying (large, non-DEX) transfer or folds it into the same actor's open aggregate,
// returning only the newly opened signals (with ids) for the enrich/attest/deliver
// pipeline. Opening is idempotent on (pool, signal_type, event_ref); folding is idempotent
// on the aggregate's recorded refs. A decode failure on one log is logged + skipped, and
// only ctx cancellation or a real DB error returns an error (with the already-opened ones).
func (d *LSTFlowDetector) ScanOnce(ctx context.Context) ([]store.Signal, error) {
	return d.scanOnce(ctx, d.db)
}

// scanOnce is ScanOnce's testable core: it takes the store as a narrow interface
// (lstFlowStore) so the open/fold/classification path can be exercised with a mock and no
// Postgres. The public ScanOnce passes the real *store.DB.
func (d *LSTFlowDetector) scanOnce(ctx context.Context, st lstFlowStore) ([]store.Signal, error) {
	now := time.Now().UTC()
	recentCut := now.Add(-lstFlowRecentWindowMin * time.Minute)

	// The noise-filter exclude set, fetched once per scan (not per-log: that is an
	// N+1): a transfer touching any tracked pool is a swap leg already covered.
	poolSet, err := st.ListPoolAddresses(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect: lstflow pool set: %w", err)
	}

	// Recent window only (large LST transfers are sparse, no baseline). RawLogsSince
	// filters to the two LST token addresses (lowercase-stored, match LSTTokens()).
	logs, err := st.RawLogsSince(ctx, recentCut, lstflow.LSTTokens())
	if err != nil {
		return nil, fmt.Errorf("detect: lstflow raw logs: %w", err)
	}

	// Per-scan tx.origin cache so two transfers in one tx resolve once. Bounded by
	// the fired transfers in one scan.
	originCache := make(map[string]common.Address)

	// The whole-token floor in raw units: LSTFlowMinTokens * 10^18. Computed once.
	floorRaw := lstFlowFloorRaw(d.cfg.LSTFlowMinTokens)

	// Token market prices for the display-only size_usd, loaded once. Best-effort: an
	// unavailable snapshot leaves the size nil, never a detection input.
	tokenPrices, perr := st.TokenPricesUSD(ctx)
	if perr != nil {
		log.Warn().Err(perr).Msg("detect: lstflow token prices unavailable, sizes stay nil")
		tokenPrices = nil
	}

	// Aggregation window (see the package doc). 0 disables aggregation, so every
	// transfer opens its own signal.
	window := time.Duration(d.cfg.LSTFlowWindowMin) * time.Minute

	var out []store.Signal
	for i := range logs {
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("detect: lstflow context cancelled: %w", err)
		}
		l := logs[i]
		// Keep only Transfer topics from an LST address (these contracts also emit
		// Approval etc.); the address filter is defensive (RawLogsSince already scoped it).
		if !lstflow.IsLSTToken(l.Address) || !lstflow.IsTransferTopic(l.Topic0) {
			continue
		}

		tr, ok := d.decodeTransfer(ctx, originCache, poolSet, floorRaw, l)
		if !ok {
			continue // decode failed (logged), below the floor, or a DEX swap leg (skip)
		}

		// Fold into the actor's open aggregate when one is in the window; an empty actor
		// (unresolvable) cannot be aggregated, so it always opens its own signal.
		if window > 0 && tr.actor != "" {
			id, payloadRaw, found, ferr := st.OpenLSTSignalByActor(ctx, tr.actor, now.Add(-window))
			if ferr != nil {
				return out, fmt.Errorf("detect: lstflow open aggregate actor=%s: %w", tr.actor, ferr)
			}
			if found {
				if _, uerr := d.foldIntoAggregate(ctx, st, id, payloadRaw, tr, tokenPrices); uerr != nil {
					return out, uerr
				}
				continue // folded (or already counted): not a fresh signal
			}
		}

		sig, ok := buildLSTSignal(tr, tokenPrices)
		if !ok {
			continue // marshal failure (logged)
		}

		id, inserted, ierr := st.InsertSignal(ctx, sig)
		if ierr != nil {
			return out, fmt.Errorf("detect: lstflow insert signal ref=%s: %w", sig.EventRef, ierr)
		}
		if !inserted {
			// Recorded on an earlier overlapping tick: the (pool, signal_type,
			// event_ref) key makes re-scanning idempotent. Skip so it is not re-attested.
			log.Debug().
				Int16("signal_type", sig.SignalType).
				Str("event_ref", sig.EventRef).
				Msg("detect: lstflow signal already recorded, skipping")
			continue
		}
		sig.ID = id
		out = append(out, sig)

		log.Info().
			Int16("signal_type", sig.SignalType).
			Str("metric", sig.Metric).
			Str("subject", sig.Pool).
			Str("actor", sig.Actor).
			Float64("score_input", sig.Zscore).
			Str("event_ref", sig.EventRef).
			Int64("signal_id", id).
			Msg("detect: lstflow signal emitted")
	}

	return out, nil
}

// decodeTransfer decodes one LST Transfer log and, when it clears the whole-token floor
// and is not a DEX swap leg, returns it with its resolved actor (tx.origin when the
// resolver succeeds, else the non-zero, non-pool counterparty). It returns ok=false when
// the log fails to decode (logged), is below the floor, or is a DEX swap leg (the noise
// filter). No signal is built here: the caller opens or folds.
func (d *LSTFlowDetector) decodeTransfer(
	ctx context.Context,
	originCache map[string]common.Address,
	poolSet map[string]struct{},
	floorRaw decimal.Decimal,
	l store.RawLog,
) (lstTransfer, bool) {
	tr, err := lstflow.DecodeTransfer(l.Address, l.Topic1, l.Topic2, l.Data)
	if err != nil {
		log.Warn().Err(err).Str("tx", l.TxHash).Int("log_index", l.LogIndex).
			Msg("detect: lstflow undecodable Transfer, skipping")
		return lstTransfer{}, false
	}

	// Absolute whole-token floor: drop dust before the cheaper-than-RPC classification.
	if tr.Value.LessThan(floorRaw) {
		return lstTransfer{}, false
	}

	// Classify by counterparty. A DEX-pool leg is dropped (already a swap signal): the
	// key noise filter.
	dir := lstflow.Classify(tr.From, tr.To, poolSet)
	if dir == lstflow.DirectionSkip {
		log.Debug().
			Str("tx", strings.ToLower(l.TxHash)).
			Int("log_index", l.LogIndex).
			Str("from", tr.From).
			Str("to", tr.To).
			Msg("detect: lstflow transfer is a DEX swap leg, skipping (covered by swap signals)")
		return lstTransfer{}, false
	}

	// Actor = tx.origin best-effort; a nil resolver / RPC error falls back to the
	// non-zero, non-pool counterparty (recipient for a mint, sender for a burn/move).
	actor := lstflow.CounterpartyEOA(tr.From, tr.To, poolSet)
	if d.res != nil {
		if addr, okr := resolveActorCached(ctx, d.res, originCache, strings.ToLower(l.TxHash)); okr {
			actor = strings.ToLower(addr.Hex())
		}
	}

	return lstTransfer{
		token:     tr.Token,
		symbol:    lstflow.TokenSymbol(tr.Token),
		direction: string(dir),
		from:      tr.From,
		to:        tr.To,
		valueRaw:  tr.Value,
		valueLST:  tr.Value.Shift(-int32(lstFlowDecimals)),
		actor:     actor,
		tx:        strings.ToLower(l.TxHash),
		logIndex:  l.LogIndex,
		ref:       strings.ToLower(l.TxHash) + ":" + strconv.Itoa(l.LogIndex),
		blockTime: l.BlockTime.UTC(),
	}, true
}

// buildLSTSignal builds the opening type-4 signal for a transfer: subject = the LST token
// address (attested as the on-chain subject), actor in the actor column, the aggregate
// seeded at this single transfer (count 1, total = its value, span = its time). size_usd
// is the value at the token's market price (nil when unpriced). ok=false on a marshal
// failure (logged).
func buildLSTSignal(tr lstTransfer, tokenPrices map[string]decimal.Decimal) (store.Signal, bool) {
	at := tr.blockTime.Format(time.RFC3339)
	payload, err := json.Marshal(lstFlowPayload{
		Token:     tr.token,
		Symbol:    tr.symbol,
		Direction: tr.direction,
		From:      tr.from,
		To:        tr.to,
		Value:     tr.valueRaw.String(),
		ValueLST:  tr.valueLST.String(),
		Tx:        tr.tx,
		LogIndex:  tr.logIndex,
		BlockTime: at,
		Transfers: 1,
		TotalLST:  tr.valueLST.String(),
		FirstAt:   at,
		LastAt:    at,
		EventRefs: []string{tr.ref},
	})
	if err != nil {
		log.Error().Err(err).Str("event_ref", tr.ref).Msg("detect: lstflow marshal payload; skipping")
		return store.Signal{}, false
	}

	return store.Signal{
		Pool:          tr.token,
		SignalType:    signalTypeLSTFlow,
		Metric:        metricLSTFlow,
		Zscore:        scoreLSTFlow(tr.valueRaw), // pseudo-score; ScoreFromZ maps it onto the uint16 on-chain score
		WindowBuckets: 0,                         // not applicable to a sparse per-event transfer signal
		BucketTS:      tr.blockTime,
		EventRef:      tr.ref,
		Actor:         tr.actor,
		SizeUSD:       lstSizeUSD(tr.valueLST, tr.token, tokenPrices), // display-only; nil when unpriced
		Payload:       payload,
		Status:        store.StatusNew,
	}, true
}

// foldIntoAggregate adds one transfer to an actor's open aggregate signal: it skips a ref
// already recorded (so overlapping scans stay idempotent), then bumps the count, adds to
// the running total, advances last_at, appends the ref, and recomputes the display size
// from the new total. It reports whether the row changed. Pure aggregation: nothing
// re-attests, and the opening transfer's hash/score are untouched.
func (d *LSTFlowDetector) foldIntoAggregate(
	ctx context.Context,
	st lstFlowStore,
	id int64,
	payloadRaw []byte,
	tr lstTransfer,
	tokenPrices map[string]decimal.Decimal,
) (bool, error) {
	var p lstFlowPayload
	if err := json.Unmarshal(payloadRaw, &p); err != nil {
		return false, fmt.Errorf("detect: lstflow decode open aggregate id=%d: %w", id, err)
	}
	for _, r := range p.EventRefs {
		if r == tr.ref {
			return false, nil // already folded: a re-scan of the same transfer is a no-op
		}
	}

	prev, perr := decimal.NewFromString(p.TotalLST)
	if perr != nil {
		prev = decimal.Zero // a malformed prior total should not abort; carry this transfer alone
	}
	total := prev.Add(tr.valueLST)

	p.Transfers++
	p.TotalLST = total.String()
	p.LastAt = tr.blockTime.Format(time.RFC3339)
	p.EventRefs = append(p.EventRefs, tr.ref)

	newPayload, err := json.Marshal(p)
	if err != nil {
		return false, fmt.Errorf("detect: lstflow marshal aggregate id=%d: %w", id, err)
	}
	if err := st.UpdateSignalAggregate(ctx, id, newPayload, lstSizeUSD(total, p.Token, tokenPrices)); err != nil {
		return false, fmt.Errorf("detect: lstflow update aggregate id=%d: %w", id, err)
	}
	return true, nil
}

// lstSizeUSD values a whole-token LST amount at the token's market price, or nil when the
// token has no known price. Display-only, mirroring swapSizeUSD; firing stays on the
// whole-token floor, so the price never affects a detection decision.
func lstSizeUSD(valueLST decimal.Decimal, token string, prices map[string]decimal.Decimal) *decimal.Decimal {
	price, ok := prices[strings.ToLower(strings.TrimSpace(token))]
	if !ok || !price.IsPositive() {
		return nil
	}
	v := valueLST.Mul(price)
	return &v
}

// lstFlowFloorRaw converts the whole-token floor (LSTFlowMinTokens) to raw 18-dec
// units: floor * 10^18, as an exact decimal. Config.Validate guarantees floor > 0, so
// the raw floor is positive. Kept pure so the floor comparison is unit-testable.
func lstFlowFloorRaw(minTokens float64) decimal.Decimal {
	return decimal.NewFromFloat(minTokens).Shift(int32(lstFlowDecimals))
}

// scoreLSTFlow maps an LST transfer's raw value (18-dec) to a pseudo-score that the
// attestor's ScoreFromZ turns into the uint16 on-chain score. Like scoreLendingUSD, a
// log10 ramp over the whole-token amount so a 10-token and a 10,000-token transfer are
// distinguishable on-chain (LST flow has no baseline/z-score):
//
//	value <= 1 whole token (degenerate; gated out by the floor anyway) -> lstFlowBaseScore
//	otherwise                                                           -> lstFlowBaseScore + log10(wholeTokens)
//
// e.g. 10 mETH -> ~3 + 1.0 = 4.0 (on-chain score 400); 1,000 mETH -> ~3 + 3.0 = 6.0
// (600). Clamped to [lstFlowBaseScore, lstFlowScoreCapZ]. The score communicates
// magnitude, not statistical surprise.
func scoreLSTFlow(rawValue decimal.Decimal) float64 {
	whole := rawValue.Shift(-int32(lstFlowDecimals)).InexactFloat64()
	if whole <= 1 || math.IsNaN(whole) || math.IsInf(whole, 0) {
		return lstFlowBaseScore
	}
	score := lstFlowBaseScore + math.Log10(whole)
	if score < lstFlowBaseScore {
		return lstFlowBaseScore
	}
	if score > lstFlowScoreCapZ {
		return lstFlowScoreCapZ
	}
	return score
}

const (
	// lstFlowBaseScore is the floor of the LST-flow pseudo-score band: a fixed non-zero
	// base (3.0 -> on-chain score 300) with log10(wholeTokens) on top. Matches
	// lendingBaseScore so the two per-event families read on a comparable scale.
	lstFlowBaseScore = 3.0

	// lstFlowScoreCapZ caps the pseudo-score at the attestor's uint16 ceiling
	// (attest.MaxScoreZ). log10(wholeTokens) never approaches this for a real LST
	// amount; the cap is purely defensive.
	lstFlowScoreCapZ = attest.MaxScoreZ
)
