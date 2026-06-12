// Package config loads runtime config from the environment, plus a couple of
// design-time YAML files (chains, pools). Secrets live only in the environment,
// never in YAML.
package config

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"ocr/internal/store"
)

// Config is the runtime configuration assembled by Load. Defaults are in the const
// block below, documented in .env.example, and range-checked by Validate.
type Config struct {
	// Chain
	ChainID         int64
	RPCURL          string
	PollIntervalSec int
	Confirmations   int // blocks trimmed off the head before a block is treated as final

	// Storage
	DatabaseURL string

	// Telegram
	TGBotToken  string
	TGChannelID string // public signal channel
	// TGAlertChatID is a private operator chat for operational alerts (watchdog stalls),
	// kept separate from the public signal channel. Empty disables those alerts (logged
	// only); they are never posted to TGChannelID.
	TGAlertChatID string

	// Attestation. The agent wallet holds gas only.
	AgentPrivateKey string
	AttestorAddress string // empty disables on-chain signal attestation

	// OutcomeAttestorAddress is the sidecar contract for matured outcome cards. Empty
	// disables on-chain outcome attestation (outcomes are still graded and stored).
	// Reuses AgentPrivateKey.
	OutcomeAttestorAddress string

	// LLM enricher
	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string

	// GeckoAPIKey, when set, points discover + poolstats at the CoinGecko onchain
	// endpoint instead of the keyless GeckoTerminal root, which rate-limits hard.
	GeckoAPIKey string

	// Detector tunables
	ZThreshold      float64
	BucketMin       int
	BaselineBuckets int
	CooldownMin     int

	// WhaleMinMultiple: a swap fires the whale detector only at this multiple of the
	// pool's median swap size. A plain MAD z-threshold fired on 10-35% of swaps on real
	// Mantle pools (their swaps cluster tightly), so we gate on the median instead.
	WhaleMinMultiple float64

	// OutcomeHorizonHours: how long the scorer waits before grading a signal, comparing
	// post-signal activity (T, T+H] against the pre-signal baseline (T-H, T].
	OutcomeHorizonHours int

	// smart-money detector (type 3).

	// SmartMoneySizeMultiple: the "large swap" bar for the candidate ledger. Lower than
	// WhaleMinMultiple so mid-size accumulation isn't missed; the score decides firing.
	SmartMoneySizeMultiple float64

	// SmartMoneyMinSwaps: large swaps needed to count as accumulation. One large swap is
	// a whale, not smart money.
	SmartMoneyMinSwaps int

	SmartMoneyWindowHours int     // accumulation window
	SmartMoneyThreshold   float64 // 0..100 composite-score gate

	// SmartMoneyCooldownMin: suppression keyed on the actor, not the pool, so one
	// accumulator can't fire once per pool (and re-fire as its swap count grows).
	SmartMoneyCooldownMin int

	// SmartMoneyMinDirectionality in [0,1): max over tokens of |net|/gross, netted per
	// token address so cross-pool arb cancels. A pure accumulator scores ~1.0; a churning
	// arb/market-maker bot nets ~0 and is excluded.
	SmartMoneyMinDirectionality float64

	// DiscoverMinLiquidityUSD / DiscoverMinVol24USD: the `ocr discover` shortlist gate is
	// an OR of these two coarse hints (reserve OR 24h volume), so a busy low-TVL pool
	// isn't dropped. 0 on the volume side disables it.
	DiscoverMinLiquidityUSD float64
	DiscoverMinVol24USD     float64

	// PoolStatsIntervalMin: how often poolstats refreshes external market stats (TVL/
	// volume/price) for display. <= 0 disables the stage.
	PoolStatsIntervalMin int

	// lending detector (Aave V3; type 5 liquidation, 6 big_borrow). USD floors are
	// our own decoded amount under stable approx $1, not a price feed.

	// BigBorrowMinUSD: floor for a big-borrow signal. A non-stable reserve has no USD
	// size and never fires (we don't guess a price).
	BigBorrowMinUSD float64

	// LiquidationMinUSD: floor for a liquidation signal. 0 (default) fires on any
	// liquidation; a positive floor needs a USD-sizeable (stable-debt) one.
	LiquidationMinUSD float64

	// LST-flow detector (type 4). The gate is a whole-token floor; the display-only
	// size_usd is filled from the token's market price when one is known.

	// LSTFlowMinTokens: minimum transfer size in whole tokens (both are 18-decimal).
	LSTFlowMinTokens float64

	// LSTFlowWindowMin: the per-actor aggregation window. A wallet's first large LST
	// transfer opens a signal; later transfers by the same actor within this window fold
	// into it (total + count + time span grow) instead of each opening a row. 0 disables
	// aggregation, leaving one signal per transfer.
	LSTFlowWindowMin int

	// depeg detector (type 7).

	// DepegThresholdBps: drift off the 1.0 peg, in bps, that triggers a depeg.
	DepegThresholdBps int

	// DepegMinSwaps: usable swaps required before the median implied price is trusted;
	// one swap's slippage isn't a depeg.
	DepegMinSwaps int

	// DepegCooldownMin: per-token suppression so a sustained depeg doesn't re-fire each
	// scan. 0 leaves only the bucket_ts idempotency.
	DepegCooldownMin int

	// APIRateLimitRPS: per-IP /api/* budget (burst is a small multiple), a coarse cap
	// to keep one client from overloading the public API. 0 disables it.
	APIRateLimitRPS int

	// StallAlertMin: if the freshest ingested activity lags wall-clock by more than this,
	// the watchdog logs and (if Telegram is set) alerts once, throttled, so a wedged
	// collector surfaces instead of stalling silently. 0 disables it.
	StallAlertMin int
}

// Defaults for unset env vars. Secrets and the database DSN have none.
const (
	defaultChainID           int64   = 5000
	defaultRPCURL            string  = "https://rpc.mantle.xyz"
	defaultPollIntervalSec   int     = 3
	defaultConfirmations     int     = 2
	defaultZThreshold        float64 = 4.0
	defaultBucketMin         int     = 5
	defaultBaselineBuckets   int     = 576
	defaultCooldownMin       int     = 60
	defaultWhaleMinMultiple  float64 = 25.0
	defaultOutcomeHorizonHrs int     = 6

	defaultSmartMoneySizeMultiple      float64 = 5.0
	defaultSmartMoneyMinSwaps          int     = 2
	defaultSmartMoneyWindowHours       int     = 24
	defaultSmartMoneyThreshold         float64 = 50.0
	defaultSmartMoneyCooldownMin       int     = 240
	defaultSmartMoneyMinDirectionality float64 = 0.5

	defaultDiscoverMinLiquidityUSD float64 = 100_000.0
	defaultDiscoverMinVol24USD     float64 = 250_000.0

	defaultPoolStatsIntervalMin int = 10

	defaultBigBorrowMinUSD   float64 = 50_000.0
	defaultLiquidationMinUSD float64 = 0.0

	defaultLSTFlowMinTokens float64 = 10.0
	defaultLSTFlowWindowMin int     = 30

	defaultDepegThresholdBps int = 50
	defaultDepegMinSwaps     int = 5
	defaultDepegCooldownMin  int = 60

	defaultStallAlertMin int = 15

	defaultAPIRateLimitRPS int = 20
)

// Load assembles the config from the environment, applying defaults. DATABASE_URL is
// required. A set-but-unparseable numeric var is a hard error, not a silent default.
func Load() (*Config, error) {
	// Load a .env in the working directory if present; real env vars always win.
	// Production injects env directly, so a missing file is fine.
	loadDotEnv(".env")

	cfg := &Config{
		RPCURL:                 envOr("MANTLE_RPC_URL", defaultRPCURL),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		TGBotToken:             os.Getenv("TG_BOT_TOKEN"),
		TGChannelID:            os.Getenv("TG_CHANNEL_ID"),
		TGAlertChatID:          os.Getenv("TG_ALERT_CHAT_ID"),
		AgentPrivateKey:        os.Getenv("AGENT_PRIVATE_KEY"),
		AttestorAddress:        os.Getenv("ATTESTOR_ADDRESS"),
		OutcomeAttestorAddress: os.Getenv("OUTCOME_ATTESTOR_ADDRESS"),
		LLMBaseURL:             os.Getenv("LLM_BASE_URL"),
		LLMAPIKey:              os.Getenv("LLM_API_KEY"),
		LLMModel:               os.Getenv("LLM_MODEL"),
		GeckoAPIKey:            os.Getenv("GECKO_API_KEY"),
	}

	var err error
	if cfg.ChainID, err = envInt64("CHAIN_ID", defaultChainID); err != nil {
		return nil, err
	}
	if cfg.PollIntervalSec, err = envInt("POLL_INTERVAL_SEC", defaultPollIntervalSec); err != nil {
		return nil, err
	}
	// CONFIRMATIONS_DEPTH is an accepted alias for older deploy configs; CONFIRMATIONS
	// wins when both are set.
	confEnv := "CONFIRMATIONS"
	if _, ok := os.LookupEnv("CONFIRMATIONS"); !ok {
		if _, ok := os.LookupEnv("CONFIRMATIONS_DEPTH"); ok {
			confEnv = "CONFIRMATIONS_DEPTH"
		}
	}
	if cfg.Confirmations, err = envInt(confEnv, defaultConfirmations); err != nil {
		return nil, err
	}
	if cfg.ZThreshold, err = envFloat("Z_THRESHOLD", defaultZThreshold); err != nil {
		return nil, err
	}
	if cfg.BucketMin, err = envInt("BUCKET_MIN", defaultBucketMin); err != nil {
		return nil, err
	}
	if cfg.BaselineBuckets, err = envInt("BASELINE_BUCKETS", defaultBaselineBuckets); err != nil {
		return nil, err
	}
	if cfg.CooldownMin, err = envInt("COOLDOWN_MIN", defaultCooldownMin); err != nil {
		return nil, err
	}
	if cfg.WhaleMinMultiple, err = envFloat("WHALE_MIN_MULTIPLE", defaultWhaleMinMultiple); err != nil {
		return nil, err
	}
	if cfg.OutcomeHorizonHours, err = envInt("OUTCOME_HORIZON_HOURS", defaultOutcomeHorizonHrs); err != nil {
		return nil, err
	}
	if cfg.SmartMoneySizeMultiple, err = envFloat("SMART_MONEY_SIZE_MULTIPLE", defaultSmartMoneySizeMultiple); err != nil {
		return nil, err
	}
	if cfg.SmartMoneyMinSwaps, err = envInt("SMART_MONEY_MIN_SWAPS", defaultSmartMoneyMinSwaps); err != nil {
		return nil, err
	}
	if cfg.SmartMoneyWindowHours, err = envInt("SMART_MONEY_WINDOW_HOURS", defaultSmartMoneyWindowHours); err != nil {
		return nil, err
	}
	if cfg.SmartMoneyThreshold, err = envFloat("SMART_MONEY_THRESHOLD", defaultSmartMoneyThreshold); err != nil {
		return nil, err
	}
	if cfg.SmartMoneyCooldownMin, err = envInt("SMART_MONEY_COOLDOWN_MIN", defaultSmartMoneyCooldownMin); err != nil {
		return nil, err
	}
	if cfg.SmartMoneyMinDirectionality, err = envFloat("SMART_MONEY_MIN_DIRECTIONALITY", defaultSmartMoneyMinDirectionality); err != nil {
		return nil, err
	}
	if cfg.DiscoverMinLiquidityUSD, err = envFloat("DISCOVER_MIN_LIQUIDITY_USD", defaultDiscoverMinLiquidityUSD); err != nil {
		return nil, err
	}
	if cfg.DiscoverMinVol24USD, err = envFloat("DISCOVER_MIN_VOL24_USD", defaultDiscoverMinVol24USD); err != nil {
		return nil, err
	}
	if cfg.PoolStatsIntervalMin, err = envInt("POOL_STATS_INTERVAL_MIN", defaultPoolStatsIntervalMin); err != nil {
		return nil, err
	}
	if cfg.BigBorrowMinUSD, err = envFloat("BIG_BORROW_MIN_USD", defaultBigBorrowMinUSD); err != nil {
		return nil, err
	}
	if cfg.LiquidationMinUSD, err = envFloat("LIQUIDATION_MIN_USD", defaultLiquidationMinUSD); err != nil {
		return nil, err
	}
	if cfg.LSTFlowMinTokens, err = envFloat("LST_FLOW_MIN_TOKENS", defaultLSTFlowMinTokens); err != nil {
		return nil, err
	}
	if cfg.LSTFlowWindowMin, err = envInt("LST_FLOW_WINDOW_MIN", defaultLSTFlowWindowMin); err != nil {
		return nil, err
	}
	if cfg.DepegThresholdBps, err = envInt("DEPEG_THRESHOLD_BPS", defaultDepegThresholdBps); err != nil {
		return nil, err
	}
	if cfg.DepegMinSwaps, err = envInt("DEPEG_MIN_SWAPS", defaultDepegMinSwaps); err != nil {
		return nil, err
	}
	if cfg.DepegCooldownMin, err = envInt("DEPEG_COOLDOWN_MIN", defaultDepegCooldownMin); err != nil {
		return nil, err
	}
	if cfg.StallAlertMin, err = envInt("STALL_ALERT_MIN", defaultStallAlertMin); err != nil {
		return nil, err
	}
	if cfg.APIRateLimitRPS, err = envInt("API_RATE_LIMIT_RPS", defaultAPIRateLimitRPS); err != nil {
		return nil, err
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required")
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate range-checks the numeric tunables. An out-of-range value is a
// misconfiguration worth failing loud at startup, rather than letting the detector
// silently spam attestations (ZThreshold <= 0) or never fire (BaselineBuckets < 1).
func (c *Config) Validate() error {
	if c.ChainID <= 0 {
		// TxSender recovers tx.origin via LatestSignerForChainID; a bad chain id
		// mis-recovers the actor address and signs against the wrong chain.
		return fmt.Errorf("config: CHAIN_ID must be > 0 (got %d)", c.ChainID)
	}
	if c.ZThreshold <= 0 {
		return fmt.Errorf("config: Z_THRESHOLD must be > 0 (got %g)", c.ZThreshold)
	}
	if c.WhaleMinMultiple <= 0 {
		return fmt.Errorf("config: WHALE_MIN_MULTIPLE must be > 0 (got %g)", c.WhaleMinMultiple)
	}
	if c.BucketMin <= 0 {
		return fmt.Errorf("config: BUCKET_MIN must be > 0 (got %d)", c.BucketMin)
	}
	if c.BaselineBuckets < 1 {
		return fmt.Errorf("config: BASELINE_BUCKETS must be >= 1 (got %d)", c.BaselineBuckets)
	}
	if c.CooldownMin < 0 {
		return fmt.Errorf("config: COOLDOWN_MIN must be >= 0 (got %d)", c.CooldownMin)
	}
	if c.PollIntervalSec <= 0 {
		return fmt.Errorf("config: POLL_INTERVAL_SEC must be > 0 (got %d)", c.PollIntervalSec)
	}
	if c.Confirmations < 0 {
		return fmt.Errorf("config: CONFIRMATIONS must be >= 0 (got %d)", c.Confirmations)
	}
	if c.OutcomeHorizonHours <= 0 {
		return fmt.Errorf("config: OUTCOME_HORIZON_HOURS must be > 0 (got %d)", c.OutcomeHorizonHours)
	}
	if c.SmartMoneySizeMultiple <= 0 {
		return fmt.Errorf("config: SMART_MONEY_SIZE_MULTIPLE must be > 0 (got %g)", c.SmartMoneySizeMultiple)
	}
	if c.SmartMoneyMinSwaps <= 0 {
		return fmt.Errorf("config: SMART_MONEY_MIN_SWAPS must be > 0 (got %d)", c.SmartMoneyMinSwaps)
	}
	if c.SmartMoneyWindowHours <= 0 {
		return fmt.Errorf("config: SMART_MONEY_WINDOW_HOURS must be > 0 (got %d)", c.SmartMoneyWindowHours)
	}
	if c.SmartMoneyThreshold <= 0 {
		return fmt.Errorf("config: SMART_MONEY_THRESHOLD must be > 0 (got %g)", c.SmartMoneyThreshold)
	}
	if c.SmartMoneyCooldownMin < 0 {
		return fmt.Errorf("config: SMART_MONEY_COOLDOWN_MIN must be >= 0 (got %d)", c.SmartMoneyCooldownMin)
	}
	if c.SmartMoneyMinDirectionality < 0 || c.SmartMoneyMinDirectionality >= 1 {
		// Must be in [0,1): 1.0 would demand perfectly one-sided flow no real
		// accumulator reaches, so it would silently never fire. 0 disables the gate.
		return fmt.Errorf("config: SMART_MONEY_MIN_DIRECTIONALITY must be in [0,1) (got %g)", c.SmartMoneyMinDirectionality)
	}
	if c.DiscoverMinLiquidityUSD < 0 {
		return fmt.Errorf("config: DISCOVER_MIN_LIQUIDITY_USD must be >= 0 (got %g)", c.DiscoverMinLiquidityUSD)
	}
	if c.DiscoverMinVol24USD < 0 {
		return fmt.Errorf("config: DISCOVER_MIN_VOL24_USD must be >= 0 (got %g)", c.DiscoverMinVol24USD)
	}
	if c.BigBorrowMinUSD <= 0 {
		return fmt.Errorf("config: BIG_BORROW_MIN_USD must be > 0 (got %g)", c.BigBorrowMinUSD)
	}
	if c.LiquidationMinUSD < 0 {
		return fmt.Errorf("config: LIQUIDATION_MIN_USD must be >= 0 (got %g)", c.LiquidationMinUSD)
	}
	if c.LSTFlowMinTokens <= 0 {
		return fmt.Errorf("config: LST_FLOW_MIN_TOKENS must be > 0 (got %g)", c.LSTFlowMinTokens)
	}
	if c.DepegThresholdBps <= 0 {
		return fmt.Errorf("config: DEPEG_THRESHOLD_BPS must be > 0 (got %d)", c.DepegThresholdBps)
	}
	if c.DepegMinSwaps <= 0 {
		return fmt.Errorf("config: DEPEG_MIN_SWAPS must be > 0 (got %d)", c.DepegMinSwaps)
	}
	if c.DepegCooldownMin < 0 {
		return fmt.Errorf("config: DEPEG_COOLDOWN_MIN must be >= 0 (got %d)", c.DepegCooldownMin)
	}
	if c.StallAlertMin < 0 {
		return fmt.Errorf("config: STALL_ALERT_MIN must be >= 0 (got %d)", c.StallAlertMin)
	}
	if c.APIRateLimitRPS < 0 {
		return fmt.Errorf("config: API_RATE_LIMIT_RPS must be >= 0 (got %d)", c.APIRateLimitRPS)
	}
	return nil
}

// poolsFile mirrors the top-level shape of config/pools.yaml.
type poolsFile struct {
	Pools []store.Pool `yaml:"pools"`
}

// LoadPools parses pools.yaml into the curated pool registry. An empty or `pools: []`
// file yields an empty slice, the intended state before the registry is filled.
func LoadPools(path string) ([]store.Pool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read pools file %q: %w", path, err)
	}

	var f poolsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("config: parse pools file %q: %w", path, err)
	}
	return f.Pools, nil
}

// utf8BOM is the byte-order mark some editors (notably on Windows) prepend. TrimSpace
// doesn't strip U+FEFF, so a BOM left on line 1 glues onto the first key and it never
// matches os.Getenv, silently falling back to defaults. Strip it up front.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// loadDotEnv reads KEY=VALUE lines from a .env into the environment without overriding
// vars already set (real env wins). Missing file is fine. Blank/comment/keyless lines
// are skipped and surrounding quotes trimmed. Intentionally minimal: no interpolation,
// no inline-comment stripping (a value may contain '#', e.g. a secret).
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	data = bytes.TrimPrefix(data, utf8BOM)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue // no key, or empty key
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			continue
		}
	}
}

// envOr returns key's value, or fallback when unset/empty.
func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envInt parses key as an int: fallback when unset/empty, error when unparseable.
func envInt(key string, fallback int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not a valid integer: %w", key, v, err)
	}
	return n, nil
}

// envInt64 parses key as an int64: fallback when unset/empty, error when unparseable.
func envInt64(key string, fallback int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not a valid integer: %w", key, v, err)
	}
	return n, nil
}

// envFloat parses key as a float64: fallback when unset/empty, error when unparseable.
func envFloat(key string, fallback float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not a valid float: %w", key, v, err)
	}
	return f, nil
}
