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
	"ocr/internal/lending"
	"ocr/internal/store"
)

// The lending detector follows Aave V3 money-flow on Mantle and emits two per-event
// signal types from the Aave V3 Pool's events (collected into raw_logs by the
// protocol-contracts registry; see internal/chain/collector.go): type 5 liquidation
// (subject = the liquidated borrower, actor = the liquidator) and type 6 big_borrow
// (subject = the borrowed reserve, actor = onBehalfOf). The build* methods document
// each type's exact fields and floor.
//
// It mirrors the whale detector's event-keyed shape (whale.go), keyed and idempotent
// on (pool, signal_type, event_ref). Liquidations and large borrows are sparse, so
// (unlike whale / smart-money) there is no per-pool baseline or z-score: the gate is
// an absolute USD floor and the on-chain score is a log-scaled size band
// (scoreLendingUSD).
const (
	// lendingRecentWindowMin bounds the candidate set to the last N minutes
	// (mirroring whaleRecentWindowMin); idempotency on event_ref makes the overlap
	// across ticks harmless.
	lendingRecentWindowMin = 10

	// signalTypeLiquidation / signalTypeBigBorrow are int16-typed local aliases of
	// the canonical store.SignalType* (1/2/3 flow/whale/smart_money; 4 LST flow;
	// 5/6 lending).
	signalTypeLiquidation int16 = store.SignalTypeLiquidation
	signalTypeBigBorrow   int16 = store.SignalTypeBigBorrow

	// metricLiquidation / metricBigBorrow are the metric strings for the two
	// families, surfaced verbatim in the API + enricher.
	metricLiquidation = "liquidation"
	metricBigBorrow   = "big_borrow"
)

// SenderResolver is reused from the smart-money detector (defined in
// smartmoney.go). The lending detector uses it best-effort to refine a
// liquidation's actor to the controlling EOA; on failure it falls back to the
// event's `liquidator` field and the liquidation still fires.

// lendingStore is the minimal store surface ScanOnce needs: the bounded Aave-Pool
// raw-log read and the idempotent signal insert. A mock lets firing + idempotency
// be tested with no Postgres.
type lendingStore interface {
	RawLogsSince(ctx context.Context, since time.Time, pools []string) ([]store.RawLog, error)
	InsertSignal(ctx context.Context, s store.Signal) (int64, bool, error)
}

// LendingDetector emits liquidation (type 5) and big-borrow (type 6) signals from
// the Aave V3 Pool's events in raw_logs. It holds the store, the config (the two
// USD floors), and a best-effort SenderResolver for liquidator tx.origin.
type LendingDetector struct {
	db  *store.DB
	cfg *config.Config
	// res refines a liquidation's actor to tx.origin. Best-effort + optional: a nil
	// resolver or resolve failure falls back to the event's liquidator address and
	// the signal still fires. Not part of the hash.
	res SenderResolver
}

// NewLendingDetector constructs a LendingDetector. cfg supplies BigBorrowMinUSD and
// LiquidationMinUSD (0 = any). res (best-effort, may be nil) refines the liquidation
// actor to tx.origin; *chain.Client satisfies it via TxSender.
func NewLendingDetector(db *store.DB, cfg *config.Config, res SenderResolver) *LendingDetector {
	return &LendingDetector{db: db, cfg: cfg, res: res}
}

// liquidationPayload is the JSON written to a liquidation signal's Payload: the
// event's parties and magnitudes, so the finding is auditable and the event-keyed
// attestation hash is reproducible from the stored row.
type liquidationPayload struct {
	CollateralAsset            string `json:"collateral_asset"`
	DebtAsset                  string `json:"debt_asset"`
	User                       string `json:"user"` // the liquidated borrower (subject)
	Liquidator                 string `json:"liquidator"`
	DebtToCover                string `json:"debt_to_cover"`                // raw units of debtAsset
	LiquidatedCollateralAmount string `json:"liquidated_collateral_amount"` // raw units of collateralAsset
	ReceiveAToken              bool   `json:"receive_atoken"`
	Tx                         string `json:"tx"`
	LogIndex                   int    `json:"log_index"`
	BlockTime                  string `json:"block_time"`
}

// borrowPayload is the JSON written to a big-borrow signal's Payload.
type borrowPayload struct {
	Reserve          string `json:"reserve"` // the borrowed asset (subject)
	OnBehalfOf       string `json:"on_behalf_of"`
	User             string `json:"user"`   // the initiator (may differ from onBehalfOf)
	Amount           string `json:"amount"` // raw units of reserve
	InterestRateMode uint8  `json:"interest_rate_mode"`
	Tx               string `json:"tx"`
	LogIndex         int    `json:"log_index"`
	BlockTime        string `json:"block_time"`
}

// ScanOnce reads the Aave Pool's recent logs once and persists a signal per
// qualifying liquidation (type 5) and big borrow (type 6). It returns the inserted
// signals (with their generated ids) so the caller can hand them straight to the
// enrich/attest/deliver pipeline, exactly like the whale / smart-money detectors.
//
// For each Aave event in the last lendingRecentWindowMin minutes it emits a
// liquidation (clearing LiquidationMinUSD; see passesLiquidationFloor) or a
// big-borrow (clearing BigBorrowMinUSD; see passesBorrowFloor). Emission is
// idempotent on (pool, signal_type, event_ref). A decode failure on one log is
// logged + skipped; only ctx cancellation or a real DB error returns an error
// (with the already-collected signals).
func (d *LendingDetector) ScanOnce(ctx context.Context) ([]store.Signal, error) {
	return d.scanOnce(ctx, d.db)
}

// scanOnce is ScanOnce's testable core: it takes the store as a narrow interface
// (lendingStore) so the firing + idempotency path can be exercised with a mock and
// no Postgres. The public ScanOnce passes the real *store.DB.
func (d *LendingDetector) scanOnce(ctx context.Context, st lendingStore) ([]store.Signal, error) {
	now := time.Now().UTC()
	recentCut := now.Add(-lendingRecentWindowMin * time.Minute)

	// Recent window only (lending events are sparse, no baseline). RawLogsSince
	// filters to the Aave Pool address (lowercase-stored, matches AavePoolAddress).
	logs, err := st.RawLogsSince(ctx, recentCut, []string{lending.AavePoolAddress})
	if err != nil {
		return nil, fmt.Errorf("detect: lending raw logs: %w", err)
	}

	// Per-scan tx.origin cache so two liquidations in one tx resolve once. Bounded
	// by the fired liquidations in one scan.
	originCache := make(map[string]common.Address)

	var out []store.Signal
	for i := range logs {
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("detect: lending context cancelled: %w", err)
		}
		l := logs[i]
		// Defensive: RawLogsSince already filtered to the Aave Pool address, but a
		// non-Aave topic from that address (should not happen) is simply skipped.
		if !lending.IsAaveTopic(l.Topic0) {
			continue
		}

		var (
			sig store.Signal
			ok  bool
		)
		switch strings.ToLower(l.Topic0) {
		case lending.TopicLiquidationCall:
			sig, ok = d.buildLiquidation(ctx, originCache, l)
		case lending.TopicBorrow:
			sig, ok = d.buildBorrow(l)
		default:
			continue
		}
		if !ok {
			continue // decode failed (logged in build*) or gated out by the floor
		}

		id, inserted, ierr := st.InsertSignal(ctx, sig)
		if ierr != nil {
			return out, fmt.Errorf("detect: lending insert signal type=%d ref=%s: %w", sig.SignalType, sig.EventRef, ierr)
		}
		if !inserted {
			// Recorded on an earlier overlapping tick: the (pool, signal_type,
			// event_ref) key makes re-scanning idempotent. Skip so it is not re-attested.
			log.Debug().
				Int16("signal_type", sig.SignalType).
				Str("event_ref", sig.EventRef).
				Msg("detect: lending signal already recorded, skipping")
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
			Msg("detect: lending signal emitted")
	}

	return out, nil
}

// buildLiquidation decodes a LiquidationCall log and (when it clears the USD floor)
// builds the liquidation signal: actor = liquidator (refined to tx.origin),
// subject = the liquidated user, size_usd from the stable debt asset, metric
// "liquidation". It returns ok=false when the log fails to decode (logged) or the
// liquidation is below LiquidationMinUSD.
func (d *LendingDetector) buildLiquidation(ctx context.Context, originCache map[string]common.Address, l store.RawLog) (store.Signal, bool) {
	lc, err := lending.DecodeLiquidationCall(l.Topic1, l.Topic2, l.Topic3, l.Data)
	if err != nil {
		log.Warn().Err(err).Str("tx", l.TxHash).Int("log_index", l.LogIndex).
			Msg("detect: lending undecodable LiquidationCall, skipping")
		return store.Signal{}, false
	}

	// Approximate USD notional from the stable debt asset, or nil (non-stable) for
	// unknown size. Own-data sizing (our decoded stablecoin amount at one dollar), not
	// an external price, so it is not the market data the detection rule targets.
	sizeUSD := lending.StableSizeUSD(lc.DebtAsset, lc.DebtToCover)
	if !passesLiquidationFloor(sizeUSD, d.cfg.LiquidationMinUSD) {
		return store.Signal{}, false
	}

	// Actor = the liquidator (always present), refined to tx.origin best-effort: a
	// nil resolver / RPC error leaves it as the liquidator and the signal still fires.
	actor := lc.Liquidator
	if d.res != nil {
		if addr, okr := resolveActorCached(ctx, d.res, originCache, strings.ToLower(l.TxHash)); okr {
			actor = strings.ToLower(addr.Hex())
		}
	}

	scoreInput := scoreLendingUSD(sizeUSD)
	blockTime := l.BlockTime.UTC()
	eventRef := strings.ToLower(l.TxHash) + ":" + strconv.Itoa(l.LogIndex)

	payload, perr := json.Marshal(liquidationPayload{
		CollateralAsset:            lc.CollateralAsset,
		DebtAsset:                  lc.DebtAsset,
		User:                       lc.User,
		Liquidator:                 lc.Liquidator,
		DebtToCover:                lc.DebtToCover.String(),
		LiquidatedCollateralAmount: lc.LiquidatedCollateralAmount.String(),
		ReceiveAToken:              lc.ReceiveAToken,
		Tx:                         strings.ToLower(l.TxHash),
		LogIndex:                   l.LogIndex,
		BlockTime:                  blockTime.Format(time.RFC3339),
	})
	if perr != nil {
		log.Error().Err(perr).Str("event_ref", eventRef).Msg("detect: lending marshal liquidation payload; skipping")
		return store.Signal{}, false
	}

	return store.Signal{
		// Subject = the liquidated borrower (attested as on-chain `subject`); the
		// liquidator lives in the actor column + payload.
		Pool:          lc.User,
		SignalType:    signalTypeLiquidation,
		Metric:        metricLiquidation,
		Zscore:        scoreInput, // a pseudo-score; ScoreFromZ maps it onto the uint16 on-chain score
		WindowBuckets: 0,          // not applicable to a sparse per-event lending signal
		BucketTS:      blockTime,
		EventRef:      eventRef,
		Actor:         actor,
		SizeUSD:       sizeUSD, // display-only; nil when the debt asset is non-stable
		Payload:       payload,
		Status:        store.StatusNew,
	}, true
}

// buildBorrow decodes a Borrow log and (when it clears the USD floor) builds the
// big-borrow signal: actor = onBehalfOf (the debt owner), subject = the borrowed
// reserve, size_usd from the stable reserve, metric "big_borrow". It returns
// ok=false when the log fails to decode (logged) or the borrow is below
// BigBorrowMinUSD, including the case where the reserve is non-stable (no USD size),
// because we cannot confirm a non-stable borrow crossed the dollar floor.
func (d *LendingDetector) buildBorrow(l store.RawLog) (store.Signal, bool) {
	b, err := lending.DecodeBorrow(l.Topic1, l.Topic2, l.Topic3, l.Data)
	if err != nil {
		log.Warn().Err(err).Str("tx", l.TxHash).Int("log_index", l.LogIndex).
			Msg("detect: lending undecodable Borrow, skipping")
		return store.Signal{}, false
	}

	// Approximate USD notional from the stable reserve, or nil (non-stable). A
	// non-stable borrow we cannot size in USD does not fire (passesBorrowFloor).
	// Own-data sizing (stable at one dollar), not an external price.
	sizeUSD := lending.StableSizeUSD(b.Reserve, b.Amount)
	if !passesBorrowFloor(sizeUSD, d.cfg.BigBorrowMinUSD) {
		return store.Signal{}, false
	}

	scoreInput := scoreLendingUSD(sizeUSD)
	blockTime := l.BlockTime.UTC()
	eventRef := strings.ToLower(l.TxHash) + ":" + strconv.Itoa(l.LogIndex)

	payload, perr := json.Marshal(borrowPayload{
		Reserve:          b.Reserve,
		OnBehalfOf:       b.OnBehalfOf,
		User:             b.User,
		Amount:           b.Amount.String(),
		InterestRateMode: b.InterestRateMode,
		Tx:               strings.ToLower(l.TxHash),
		LogIndex:         l.LogIndex,
		BlockTime:        blockTime.Format(time.RFC3339),
	})
	if perr != nil {
		log.Error().Err(perr).Str("event_ref", eventRef).Msg("detect: lending marshal borrow payload; skipping")
		return store.Signal{}, false
	}

	return store.Signal{
		// Subject = the borrowed reserve (the asset under pressure). The borrower
		// (onBehalfOf) is the actor.
		Pool:          b.Reserve,
		SignalType:    signalTypeBigBorrow,
		Metric:        metricBigBorrow,
		Zscore:        scoreInput,
		WindowBuckets: 0,
		BucketTS:      blockTime,
		EventRef:      eventRef,
		Actor:         b.OnBehalfOf,
		SizeUSD:       sizeUSD, // present (the floor only passes a stable, sized borrow)
		Payload:       payload,
		Status:        store.StatusNew,
	}, true
}

// passesLiquidationFloor reports whether a liquidation clears the USD floor.
//
//   - floor <= 0: any liquidation fires (the default: every liquidation is a
//     smart-money signal worth recording), including a non-stable-debt one with no
//     USD size.
//   - floor > 0: the liquidation fires only when its USD-sized debt is known and
//     >= floor. A non-stable debt (nil size) is gated out under a positive floor:
//     we cannot confirm it cleared a dollar threshold.
func passesLiquidationFloor(sizeUSD *decimal.Decimal, floor float64) bool {
	if floor <= 0 {
		return true
	}
	if sizeUSD == nil {
		return false
	}
	return sizeUSD.GreaterThanOrEqual(decimal.NewFromFloat(floor))
}

// passesBorrowFloor reports whether a borrow clears the USD floor. Unlike the
// liquidation floor, a big borrow is inherently a dollar threshold, so a borrow we
// cannot size in USD (non-stable reserve, nil size) never passes; even a zero floor
// requires a known USD size to call something a "big borrow". A borrow with a known
// USD size passes when it is >= floor.
func passesBorrowFloor(sizeUSD *decimal.Decimal, floor float64) bool {
	if sizeUSD == nil {
		return false
	}
	if floor <= 0 {
		return true
	}
	return sizeUSD.GreaterThanOrEqual(decimal.NewFromFloat(floor))
}

// scoreLendingUSD maps a lending event's display-only USD size to a pseudo-score in
// a bounded, documented band, which the attestor's ScoreFromZ then turns into the
// uint16 on-chain score (ScoreFromZ = round(min(|x|,655.35)*100)). The mapping is a
// simple log10 ramp so a $50k event and a $50M event are distinguishable on-chain
// without a baseline/z-score (which lending's sparse events lack):
//
//	nil USD (non-stable, unknown size) -> a fixed mid band (lendingBaseScore)
//	USD <= $1 (degenerate)             -> lendingBaseScore
//	otherwise                          -> lendingBaseScore + log10(usd)
//
// e.g. $50k -> ~3 + 4.70 = 7.70 (on-chain score 770); $5M -> ~3 + 6.70 = 9.70 (970).
// The value is clamped to [lendingBaseScore, lendingScoreCapZ] so it always yields a
// valid, non-saturating uint16 after the *100 scale. The score communicates magnitude,
// not a statistical surprise: lending events are notable by their nature, not by
// deviating from a pool's baseline.
func scoreLendingUSD(sizeUSD *decimal.Decimal) float64 {
	if sizeUSD == nil {
		return lendingBaseScore
	}
	usd := sizeUSD.InexactFloat64()
	if usd <= 1 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return lendingBaseScore
	}
	score := lendingBaseScore + math.Log10(usd)
	if score < lendingBaseScore {
		return lendingBaseScore
	}
	if score > lendingScoreCapZ {
		return lendingScoreCapZ
	}
	return score
}

const (
	// lendingBaseScore is the floor of the lending pseudo-score band: a fixed,
	// non-zero base so even a small or unsized lending event carries a meaningful
	// on-chain score (3.0 -> on-chain score 300), with log10(usd) added on top for
	// larger events.
	lendingBaseScore = 3.0

	// lendingScoreCapZ caps the pseudo-score at the attestor's uint16 ceiling
	// (attest.MaxScoreZ). log10(usd) never approaches this for a real dollar amount,
	// so the cap is purely defensive against a degenerate value.
	lendingScoreCapZ = attest.MaxScoreZ
)
