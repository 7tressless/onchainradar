// Package api serves the read-only public JSON API and the SSE live signal feed for a
// separate React frontend. The JSON shape is frozen: the DTO structs here, with their
// snake_case json tags, are the public schema, so add fields but never rename or remove
// them. DTOs are built explicitly from store rows (see mapping.go), never by marshaling
// internal structs, so nothing internal-only is serialized.
package api

// StatsDTO is GET /api/stats: the headline rollup for the dashboard hero.
type StatsDTO struct {
	TotalSignals int64     `json:"total_signals"`
	ByType       ByTypeDTO `json:"by_type"`
	// SignalAttestations / OutcomeAttestations: how many records are on-chain.
	SignalAttestations  int64                `json:"signal_attestations"`
	OutcomeAttestations int64                `json:"outcome_attestations"`
	OutcomesTotal       int64                `json:"outcomes_total"`
	GradeDistribution   GradeDistributionDTO `json:"grade_distribution"`
	AvgGrade            float64              `json:"avg_grade"`
	Last24h             Last24hDTO           `json:"last_24h"`
	PoolsTracked        int64                `json:"pools_tracked"`
	// FirstSignalAt is RFC3339 (UTC), or null when there are no signals.
	FirstSignalAt *string `json:"first_signal_at"`
	// UpdatedAt is when this response was generated (RFC3339, UTC).
	UpdatedAt string `json:"updated_at"`
}

// ByTypeDTO splits lifetime signal counts by detector family (signal_type per field).
type ByTypeDTO struct {
	Flow        int64 `json:"flow"`        // signal_type 1
	Whale       int64 `json:"whale"`       // signal_type 2
	SmartMoney  int64 `json:"smart_money"` // signal_type 3
	LSTFlow     int64 `json:"lst_flow"`    // signal_type 4
	Liquidation int64 `json:"liquidation"` // signal_type 5
	BigBorrow   int64 `json:"big_borrow"`  // signal_type 6
	Depeg       int64 `json:"depeg"`       // signal_type 7
}

// GradeDistributionDTO is the outcome report-card letter histogram.
type GradeDistributionDTO struct {
	A int64 `json:"A"`
	B int64 `json:"B"`
	C int64 `json:"C"`
	D int64 `json:"D"`
	F int64 `json:"F"`
}

// Last24hDTO is the trailing-24h activity window, broken down by detector family.
type Last24hDTO struct {
	Signals     int64 `json:"signals"`
	Flow        int64 `json:"flow"`
	Whale       int64 `json:"whale"`
	SmartMoney  int64 `json:"smart_money"`
	LSTFlow     int64 `json:"lst_flow"`
	Liquidation int64 `json:"liquidation"`
	BigBorrow   int64 `json:"big_borrow"`
	Depeg       int64 `json:"depeg"`
	// TopActor is the most active resolved actor (by signal count) in the window,
	// or null when no actor-attributed signal fired.
	TopActor *string `json:"top_actor"`

	// Outcomes graded within the window: a momentum success rate beside the all-time
	// stats.grade_distribution. All zero when none graded.
	OutcomesTotal     int64                `json:"outcomes_total"`
	GradeDistribution GradeDistributionDTO `json:"grade_distribution"`
	AvgGrade          float64              `json:"avg_grade"`
}

// SignalDTO is the canonical signal shape for /api/signals (list + detail), the actor
// views, and the SSE feed. Nullable fields are pointers so the frontend gets a real JSON
// null (not a zero value) when data is absent:
//   - Actor:    null for flow (type 1) and whale (type 2); set for smart_money.
//   - Note:     null until the async enricher writes the LLM analyst note.
//   - AttestTx: null until the signal is attested on-chain.
//   - Outcome:  null until the signal's horizon elapses and it is graded.
type SignalDTO struct {
	ID        int64   `json:"id"`
	CreatedAt string  `json:"created_at"` // RFC3339 UTC
	Type      int16   `json:"type"`       // numeric signal_type (1/2/3)
	TypeName  string  `json:"type_name"`  // flow_anomaly | whale | smart_money
	Pool      string  `json:"pool"`       // pool address (lowercase hex)
	PoolLabel string  `json:"pool_label"` // human label, e.g. "USDC/USDe" ("" if unknown)
	Metric    string  `json:"metric"`     // swap_count | vol0 | whale_swap | smart_money | ...
	Score     int     `json:"score"`      // on-chain score (z*100, capped); the attested integer
	Zscore    float64 `json:"zscore"`     // raw z-score / composite score (type 3)
	Actor     *string `json:"actor"`      // resolved EOA for smart_money; null otherwise
	Note      *string `json:"note"`       // LLM analyst note; null until enriched
	AttestTx  *string `json:"attest_tx"`  // on-chain attestation tx hash; null until attested
	AttestURL *string `json:"attest_url"` // explorer link for AttestTx; null when not attested
	Status    string  `json:"status"`     // new | enriched | submitted | alerted
	// Outcome is the graded report card, or null until the signal matures.
	Outcome *OutcomeDTO `json:"outcome"`

	// BucketTS is the anomaly's logical time (source bucket start), RFC3339 UTC, or
	// null for signals with no originating bucket. For replayed signals this is the
	// actual anomaly time (created_at is the wall-clock insert time).
	BucketTS *string `json:"bucket_ts"`

	// PoolURL is the MantleScan address link for the pool (present when the explorer
	// base is configured; "" when links are disabled).
	PoolURL string `json:"pool_url"`
	// ActorURL is the MantleScan address link for Actor; null when Actor is null
	// (flow type 1 and whale type 2 carry no actor).
	ActorURL *string `json:"actor_url"`
	// TxHash / TxURL are the triggering swap's tx hash + MantleScan link, from the
	// internal event_ref. Set for per-event signals (whale type 2, smart_money type 3);
	// null for flow (type 1). Only the hash is exposed; the log index stays internal.
	TxHash *string `json:"tx_hash"`
	TxURL  *string `json:"tx_url"`

	// SizeUSD is a display-only approximate USD notional for the triggering swap (whale
	// type 2, smart_money type 3): the stable side's amount under the stable-approx-$1
	// assumption, as a decimal string. null for flow (type 1) and when neither side is a
	// known stablecoin. Derived from our own decoded swap amounts, never a detection input.
	SizeUSD *string `json:"size_usd"`

	// ActionURL / ActionLabel: a display-only "go interact" CTA for protocol signals
	// (verified URLs; see actionURL in mapping.go). null for DEX types (1/2/3, which use
	// the pool's trade_url) and type 7 depeg.
	ActionURL   *string `json:"action_url"`
	ActionLabel *string `json:"action_label"`
}

// OutcomeDTO is a signal's graded outcome embedded in a SignalDTO.
type OutcomeDTO struct {
	Grade  int    `json:"grade"`  // composite score 0..100
	Letter string `json:"letter"` // A/B/C/D/F
	Status string `json:"status"` // confirmed | transient
	// AttestTx / AttestURL: the on-chain OUTCOME attestation, null until attested.
	AttestTx  *string `json:"attest_tx"`
	AttestURL *string `json:"attest_url"`
}

// SignalDetailDTO is GET /api/signals/{id}: the full Signal plus the stored
// detector payload (raw JSONB) and the baseline window size.
type SignalDetailDTO struct {
	SignalDTO
	// Payload is the detector's stored evidence JSON (shape varies by type), passed
	// through verbatim as a nested JSON object.
	Payload       jsonRaw `json:"payload"`
	WindowBuckets int     `json:"window_buckets"`

	// Swap is the decoded triggering swap action, computed on-demand from the event_ref
	// + raw log. null for flow (type 1) and when the log cannot be loaded/decoded.
	// Detail-only (kept off the list endpoint to keep the list light).
	Swap *SwapActionDTO `json:"swap"`
}

// SwapActionDTO is one decoded swap for display: the trader sent token_in and received
// token_out, in raw token-unit decimal strings (the frontend formats with decimals).
// dex is the pool's DEX name; tx_url links the swap on MantleScan.
type SwapActionDTO struct {
	Dex         string       `json:"dex"`
	TokenIn     SwapTokenDTO `json:"token_in"`
	TokenOut    SwapTokenDTO `json:"token_out"`
	AmountInRaw string       `json:"amount_in_raw"`  // raw token-unit decimal string (token_in sent)
	AmountOut   string       `json:"amount_out_raw"` // raw token-unit decimal string (token_out received)
	TxURL       *string      `json:"tx_url"`         // MantleScan tx link; null when links disabled
}

// SwapTokenDTO identifies one side of a swap for display: contract address, symbol,
// decimals, and logo. symbol/decimals fall back to the pool's registry data when
// token_meta has not been fetched; logo_url is null when unknown.
type SwapTokenDTO struct {
	Address  string  `json:"address"`  // token contract address (lowercase hex)
	Symbol   string  `json:"symbol"`   // symbol ("" when unknown)
	Decimals int     `json:"decimals"` // token decimals (from pool registry; authoritative)
	LogoURL  *string `json:"logo_url"` // token logo URL; null when unknown
}

// SignalListDTO is GET /api/signals: a page of signals plus pagination metadata.
type SignalListDTO struct {
	Items  []SignalDTO `json:"items"`
	Total  int64       `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

// ActorDTO is one actor's aggregate footprint for /api/actors (list + detail).
type ActorDTO struct {
	Actor           string    `json:"actor"`             // EOA address (lowercase hex)
	ActorURL        string    `json:"actor_url"`         // explorer address link
	TotalSignals    int64     `json:"total_signals"`     // lifetime smart-money signals fired
	LargeSwaps24h   int64     `json:"large_swaps_24h"`   // ledger swaps in the last 24h
	PoolsTouched24h int64     `json:"pools_touched_24h"` // distinct pools in the last 24h
	LatestScore     float64   `json:"latest_score"`      // score of the actor's most recent signal
	BestScore       float64   `json:"best_score"`        // max score across the actor's signals
	Grades          GradesDTO `json:"grades"`            // graded-outcome rollup for this actor
	FirstSeen       string    `json:"first_seen"`        // RFC3339 UTC of earliest signal
	LastSeen        string    `json:"last_seen"`         // RFC3339 UTC of latest signal
}

// GradesDTO is an actor's graded-outcome rollup.
type GradesDTO struct {
	Count int64   `json:"count"` // number of graded outcomes among the actor's signals
	Avg   float64 `json:"avg"`   // mean grade across them; 0 when count is 0
}

// ActorListDTO is GET /api/actors.
type ActorListDTO struct {
	Items []ActorDTO `json:"items"`
}

// ActorDetailDTO is GET /api/actors/{addr}: the Actor plus recent signals, the
// windowed accumulation footprint, and the decoded recent-swap activity path.
type ActorDetailDTO struct {
	ActorDTO
	Signals      []SignalDTO     `json:"signals"`
	Accumulation AccumulationDTO `json:"accumulation"`

	// RecentSwaps is the actor's recent large swaps (newest first, cap ~25), decoded into
	// directional actions for the activity path. Never null (empty [] when none).
	RecentSwaps []ActorSwapDTO `json:"recent_swaps"`
}

// ActorSwapDTO is one of an actor's recent large swaps as a decoded action for the
// activity path: the pool/DEX, the directional swap, the tx link, and the swap time.
// Direction + amounts come from the ledger's signed net0/net1, with no raw-log re-read.
type ActorSwapDTO struct {
	TxURL     *string     `json:"tx_url"`     // MantleScan tx link; null when links disabled
	Pool      string      `json:"pool"`       // pool address (lowercase hex)
	PoolLabel string      `json:"pool_label"` // human pool label ("" when unknown)
	Dex       string      `json:"dex"`        // pool DEX name
	Swap      SwapBodyDTO `json:"swap"`       // the decoded in/out + amounts
	BlockTime string      `json:"block_time"` // RFC3339 UTC swap time
}

// SwapBodyDTO is the directional core of a decoded swap (token_in/token_out + raw
// amounts) without the dex/tx_url envelope, reused inside ActorSwapDTO where those live
// on the outer object. Amounts are raw token-unit decimal strings.
type SwapBodyDTO struct {
	TokenIn     SwapTokenDTO `json:"token_in"`
	TokenOut    SwapTokenDTO `json:"token_out"`
	AmountInRaw string       `json:"amount_in_raw"`
	AmountOut   string       `json:"amount_out_raw"`
}

// AccumulationDTO is an actor's recent accumulation footprint (from the
// large_swaps ledger over a window).
type AccumulationDTO struct {
	Swaps       int64 `json:"swaps"`
	Pools       int64 `json:"pools"`
	WindowHours int   `json:"window_hours"`
}

// PoolDTO is one pool for /api/pools (list + detail). It carries the registry
// facts, our 24h on-chain activity, and the external display-only market stats as
// separate blocks (stats never feed detection).
type PoolDTO struct {
	Address string `json:"address"`
	URL     string `json:"url"` // explorer address link
	Dex     string `json:"dex"`
	Label   string `json:"label"`
	Token0  string `json:"token0"` // token0 contract (lowercase hex)
	Token1  string `json:"token1"` // token1 contract (lowercase hex)
	Dec0    int    `json:"dec0"`
	Dec1    int    `json:"dec1"`
	Enabled bool   `json:"enabled"`

	// Action links (URL above stays the MantleScan verify link). trade_url is Matcha (the
	// gasless Mantle swap aggregator) preset to this pair; dex_url is the native DEX app.
	// "" when not derivable.
	TradeURL string `json:"trade_url"`
	DexURL   string `json:"dex_url"`

	// LPURL deep-links this pool's add-liquidity page on its native DEX (a third action
	// link beside trade_url/dex_url; see lpURL in mapping.go for the verified patterns).
	// "" for an unknown DEX, a missing input, or merchant_moe until bin_step is known
	// (the frontend then falls back to dex_url). Display-only.
	LPURL string `json:"lp_url"`

	// token display metadata (token_meta, joined by address; null/"" when not yet
	// fetched). Display-only; decimals stay dec0/dec1 above.
	Token0Symbol  string  `json:"token0_symbol"`   // "" when unknown
	Token1Symbol  string  `json:"token1_symbol"`   // "" when unknown
	Token0LogoURL *string `json:"token0_logo_url"` // null when unknown
	Token1LogoURL *string `json:"token1_logo_url"` // null when unknown

	// Activity is our on-chain activity over the last 24h, derived from buckets
	// (raw token units). This is the data detection runs on.
	Activity PoolActivityDTO `json:"activity"`

	// Stats is external market context (TVL/volume/price) from GeckoTerminal,
	// display-only. null when never fetched. It is never an input to detection.
	Stats *PoolStatsDTO `json:"stats"`

	RecentSignals24h int64 `json:"recent_signals_24h"`
}

// PoolActivityDTO is a pool's trailing-24h on-chain activity from our buckets.
// Volumes are raw token-unit decimal strings (no USD; detection is unit-relative).
type PoolActivityDTO struct {
	Swaps24h int64 `json:"swaps_24h"`
	// Vol0_24h / Vol1_24h are raw token-unit sums as decimal strings (full
	// precision; raw units, not USD).
	Vol0_24h string `json:"vol0_24h"`
	Vol1_24h string `json:"vol1_24h"`
	// LastBucketTS is the newest bucket time in the window (RFC3339 UTC), or null
	// when the pool had no activity in the last 24h.
	LastBucketTS *string `json:"last_bucket_ts"`
}

// PoolStatsDTO is a pool's external market snapshot (display-only). All USD/price
// fields are decimal strings (full precision) or null when the upstream omitted
// them. price_change_24h is a percent.
type PoolStatsDTO struct {
	TVLUSD         *string `json:"tvl_usd"`
	Vol24hUSD      *string `json:"vol24h_usd"`
	PriceUSD       *string `json:"price_usd"`
	PriceChange24h *string `json:"price_change_24h"`
	FetchedAt      string  `json:"fetched_at"` // RFC3339 UTC
	Source         string  `json:"source"`     // upstream provenance URL
}

// PoolListDTO is GET /api/pools.
type PoolListDTO struct {
	Items []PoolDTO `json:"items"`
}

// PoolDetailDTO is GET /api/pools/{addr}: the Pool plus a recent bucket series and
// recent signals.
type PoolDetailDTO struct {
	PoolDTO
	Series        []SeriesBucketDTO `json:"series"`
	RecentSignals []SignalDTO       `json:"recent_signals"`
}

// SeriesBucketDTO is one bucket in a pool's detail mini-series (raw token units).
type SeriesBucketDTO struct {
	TS        string `json:"ts"` // RFC3339 UTC bucket start
	SwapCount int    `json:"swap_count"`
	Vol0      string `json:"vol0"` // raw token-unit decimal string
	Vol1      string `json:"vol1"` // raw token-unit decimal string
}

// SeriesDTO is GET /api/series: one metric for one pool as a (ts, value) series.
type SeriesDTO struct {
	Pool   string           `json:"pool"`
	Metric string           `json:"metric"` // swap_count | vol0 | vol1 | netflow0 | netflow1
	Points []SeriesPointDTO `json:"points"`
}

// SeriesPointDTO is one (time, value) sample. value is a decimal string (raw
// units; swap_count is an integer rendered as a string for a uniform shape).
type SeriesPointDTO struct {
	TS    string `json:"ts"`    // RFC3339 UTC
	Value string `json:"value"` // raw token-unit / count decimal string
}

// ErrorDTO is the JSON body of every error response.
type ErrorDTO struct {
	Error string `json:"error"`
}
