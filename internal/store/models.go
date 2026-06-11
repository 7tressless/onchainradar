// Package store is the OCR storage layer: pgx/v5 wrappers over the Postgres
// schema in migrations/0001_init.sql, with the row models and the read/write
// queries the ingest, detector, and delivery stages use. Addresses are stored
// and queried as lowercase hex throughout.
package store

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"
)

// SignalType is the signal_type discriminator (uint8 on the contract, SMALLINT
// in the DB, int16 in Go). Values go into the immutable on-chain attestation, so
// they must never change. The constants below are untyped so they slot into the
// int16 fields without conversion.
type SignalType int16

const (
	SignalTypeFlow        = 1 // flow_anomaly  (bucket-keyed relative anomaly)
	SignalTypeWhale       = 2 // whale         (single large outlier swap)
	SignalTypeSmartMoney  = 3 // smart_money   (accumulating actor)
	SignalTypeLSTFlow     = 4 // lst_flow      (large mETH/cmETH transfer)
	SignalTypeLiquidation = 5 // liquidation  (Aave V3 LiquidationCall)
	SignalTypeBigBorrow   = 6 // big_borrow    (Aave V3 large Borrow)
	SignalTypeDepeg       = 7 // depeg         (stablecoin depeg + contagion)
)

// SignalStatus is the signals.status lifecycle value. The strings are stored
// verbatim, so they must stay byte-identical.
type SignalStatus string

// Status lifecycle: new -> enriched -> submitted -> alerted. "submitted" means the
// attest tx reached the mempool (not yet confirmed on-chain). Enrichment may run
// last, so the hot path can advance a signal past "enriched".
const (
	StatusNew       = "new"
	StatusEnriched  = "enriched"
	StatusSubmitted = "submitted"
	StatusAlerted   = "alerted"
)

// RawLog is one on-chain log row for a registered pool. The business key is
// (chain_id, tx_hash, log_index); ingest is idempotent against it.
type RawLog struct {
	ChainID     int64
	BlockNumber int64
	BlockTime   time.Time
	TxHash      string
	LogIndex    int
	Address     string // lowercase hex
	Topic0      string
	Topic1      string
	Topic2      string
	Topic3      string
	Data        string
}

// Pool is a curated pool-registry entry. YAML tags allow seeding the registry
// from config; every address/ABI must trace back to SourceURL.
type Pool struct {
	Address   string `yaml:"address"` // lowercase hex
	Dex       string `yaml:"dex"`
	Token0    string `yaml:"token0"`
	Token1    string `yaml:"token1"`
	Dec0      int    `yaml:"dec0"`
	Dec1      int    `yaml:"dec1"`
	Label     string `yaml:"label"`
	Enabled   bool   `yaml:"enabled"`
	SourceURL string `yaml:"source_url"`

	// ZThreshold optionally overrides the global cfg.ZThreshold for the flow-anomaly
	// detector (type 1) on this pool; nil means use the global default. Per-pool z-score
	// distributions differ widely, so heavy-tailed pools need a higher gate to avoid
	// over-firing.
	ZThreshold *float64 `yaml:"z_threshold"`

	// WhaleMinMultiple optionally overrides the global cfg.WhaleMinMultiple for the
	// whale detector (type 2) on this pool; nil means use the global default.
	WhaleMinMultiple *float64 `yaml:"whale_min_multiple"`

	// BinStep is a Merchant Moe (Liquidity Book) pool's on-chain bin step
	// (getBinStep() -> uint16; see migrations/0012_pool_bin_step.sql), used so the API can
	// deep-link the add-liquidity page (.../pool/v22/{X}/{Y}/{binStep}). Display-only. nil
	// means unknown or not a Liquidity Book pool (every Agni pool stays nil).
	BinStep *int `yaml:"bin_step"`
}

// Bucket is a time-bucketed aggregate of swap activity for a pool. Volume and
// netflow are in raw token units (no USD), hence decimal.Decimal over NUMERIC(78).
type Bucket struct {
	Pool      string
	TsBucket  time.Time
	SwapCount int
	Vol0      decimal.Decimal
	Vol1      decimal.Decimal
	Netflow0  decimal.Decimal
	Netflow1  decimal.Decimal
}

// Signal is a detector finding.
//
// BucketTS is the source bucket's window start (the anomaly's logical time) and the
// idempotency anchor: (Pool, SignalType, Metric, BucketTS) uniquely identifies a flow
// anomaly, so the same anomaly inserts at most once and hashes identically on-chain
// across detector runs. Zero time (stored NULL) for sources with no originating bucket.
type Signal struct {
	ID            int64
	CreatedAt     time.Time
	BucketTS      time.Time
	Pool          string
	SignalType    int16 // one of the SignalType* consts
	Metric        string
	Zscore        float64
	WindowBuckets int
	Payload       json.RawMessage
	LLMNote       string
	AttestTx      string
	Status        string
	// EventRef is "txhash:logindex" for per-event signals (e.g. whale, type 2); empty for
	// bucket signals (flow anomalies, type 1). It is the idempotency anchor for per-event
	// signals (pool, signal_type, event_ref) and feeds the event-keyed attestation hash.
	EventRef string
	// Actor is the resolved signing EOA (tx.origin), lowercase hex, for per-event signals
	// (whale type 2, smart-money type 3); empty/NULL otherwise. It powers the per-actor
	// cooldown (LastSignalTimeByActor) that dedups one accumulating actor across pools
	// (type 3 only). Not part of any signal's attestation hash.
	Actor string
	// SizeUSD is a display-only approximate USD notional for a per-event signal's
	// triggering swap (whale type 2, smart_money type 3): the stable side's amount under
	// the stable-approx-$1 assumption (see migrations/0011_signal_size.sql). nil when no
	// side is a known stablecoin, or for flow (type 1). Derived from our own decoded swap
	// amounts (no external price); never read by detection.
	SizeUSD *decimal.Decimal
	// TGMessageID is the Telegram message_id of this signal's alert, stored verbatim, so
	// the async enrich stage can edit the analyst note into the already-sent message.
	// Empty when no alert was sent (Telegram off, or send failed).
	TGMessageID string
}

// LargeSwap is one resolved-actor large swap in the large_swaps ledger (see
// migrations/0006_large_swaps.sql). The smart-money detector writes a row per candidate
// swap whose signing EOA (Actor = tx.origin) it resolves, so accumulation is a windowed
// COUNT / COUNT(DISTINCT pool) over this table. The business key (TxHash, LogIndex)
// makes ingest idempotent.
type LargeSwap struct {
	TxHash     string // lowercase tx hash of the swap
	LogIndex   int    // log index of the swap within the tx
	Pool       string // lowercase pool address the swap hit
	Actor      string // resolved signing EOA (tx.origin), lowercase hex
	SizeToken0 decimal.Decimal
	// Net0 / Net1 are the actor's signed net change of token0 / token1 (positive =
	// received, negative = sent), from aggregate.DecodeSwap. The detector nets them per
	// token address across an actor's window to measure net-directionality and exclude
	// churning arb/MM bots. Nullable (rows before migration 0008 are NULL); new rows write
	// them, reads COALESCE NULL to 0.
	Net0      decimal.Decimal
	Net1      decimal.Decimal
	BlockTime time.Time
}

// Outcome is a matured signal's graded report card (see migrations/0003_outcomes.sql),
// written once per signal after its horizon elapses and post-window activity is measured
// against the pre-signal baseline. Card is the canonical JSON whose keccak256 is
// ReportHash, so the hash is reproducible from the stored card (mirroring the on-chain
// SignalHash pattern).
type Outcome struct {
	SignalID     int64
	HorizonHours int
	Grade        int    // composite score 0..100
	Letter       string // A/B/C/D/F bucket of Grade
	Status       string // coarse rollup: confirmed | transient
	ReportHash   string // 0x-hex keccak256 of Card
	Card         json.RawMessage
	MeasuredAt   time.Time
	// AttestTx is the tx hash of the on-chain outcome attestation (OutcomeRecorded on the
	// OutcomeAttestor sidecar), lowercase. Empty when not yet attested (disabled, or the
	// attest has not run / failed). See migrations/0007_outcome_attest.sql.
	AttestTx string
}

// PoolStats is a pool's latest external market snapshot (see
// migrations/0009_pool_stats.sql): TVL, 24h volume, spot price, and 24h price change
// from an off-chain source (GeckoTerminal). The poolstats stage writes it; only the
// read-only API reads it. Display-only, never read by detection (each pool is compared
// only to its own raw-unit on-chain baseline).
//
// USD/percent fields are *decimal.Decimal over nullable NUMERIC columns, so a
// never-fetched pool is distinguishable from one whose value is genuinely 0.
type PoolStats struct {
	Pool           string
	TVLUSD         *decimal.Decimal
	Vol24hUSD      *decimal.Decimal
	PriceUSD       *decimal.Decimal
	PriceChange24h *decimal.Decimal
	BaseToken      string
	QuoteToken     string
	FetchedAt      time.Time
	SourceURL      string
}

// TokenMeta is one token's display-only metadata (see migrations/0010_token_meta.sql):
// symbol, logo URL, and decimals from an external source (GeckoTerminal). Keyed by token
// contract address (lowercase), so one row serves every pool referencing it. Never read
// by detection. Decimals here is a display hint, not authoritative (pools.dec0/dec1,
// re-derived on-chain, is); nil when the source omitted it, Symbol/LogoURL "" when absent.
type TokenMeta struct {
	Address   string
	Symbol    string
	LogoURL   string
	Decimals  *int
	FetchedAt time.Time
	SourceURL string
}
