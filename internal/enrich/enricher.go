// Package enrich produces a short, human-readable analyst note for a detector signal by
// calling an OpenAI-compatible chat-completions endpoint.
//
// Enrichment sits between detection and on-chain attestation and is strictly best-effort:
// it must never block or fail that pipeline. Every path returns ("", err) on any problem so
// the caller can log it and attest the signal with an empty note. FallbackNote supplies a
// deterministic LLM-free note when the model is unavailable.
package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SignalContext is the detector finding flattened into the fields the analyst prompt
// needs. It carries no secrets and is safe to serialize into a prompt. Each family block
// below (smart-money, lending, LST flow, depeg) is populated only for that signal type
// and stays zero/empty otherwise; flow (type 1) and whale (type 2) use Zscore/Median/etc.
type SignalContext struct {
	Pool        string  // lowercase hex pool address (or the lending subject address)
	PoolLabel   string  // human-friendly pool label, e.g. "WETH/USDC 0.05%"
	Metric      string  // metric that fired, e.g. "swap_count" or "netflow0"
	Zscore      float64 // robust z-score of the latest bucket vs. baseline
	LatestValue float64 // latest bucket value for the metric
	Median      float64 // baseline median for the metric
	BucketTime  string  // bucket timestamp (RFC3339) the signal is for

	// smart-money (signal_type 3) only; zero/empty otherwise.
	IsSmartMoney  bool    // true when this is a smart-money accumulation finding
	Actor         string  // the accumulating wallet (tx.origin), lowercase hex
	AccumSwaps    int     // the actor's large swaps over the window
	DistinctPools int     // distinct pools those large swaps spanned
	SizeMultiple  float64 // latest firing swap size / pool median swap size
	WindowHours   int     // accumulation window length, in hours

	// lending (Aave V3: liquidation type 5 / big_borrow type 6) only. IsLiquidation
	// selects the prompt; SizeUSD is the display-only notional (empty for non-stable).
	IsLending     bool   // true when this is an Aave V3 lending finding
	IsLiquidation bool   // true for a liquidation (type 5); false for a big_borrow (type 6)
	SizeUSD       string // display-only approximate USD notional (decimal string; "" when unknown)
	// Liquidation-specific:
	Liquidator      string // the liquidator address (the smart-money actor)
	LiquidatedUser  string // the borrower whose collateral was seized
	CollateralAsset string // seized collateral asset address
	DebtAsset       string // repaid debt asset address
	// Big-borrow-specific:
	Reserve    string // the borrowed reserve asset address
	OnBehalfOf string // the borrower (debt owner)

	// LST flow (signal_type 4: mETH/cmETH ERC-20 Transfer) only. No USD size (mETH/cmETH
	// are ETH-priced); the whole-token amount carries the magnitude.
	IsLSTFlow    bool   // true when this is an LST-flow finding
	LSTSymbol    string // "mETH" / "cmETH"
	LSTDirection string // "mint" | "burn" | "move"
	LSTValue     string // whole-token transfer amount (decimal string)
	LSTFrom      string // transfer sender (0x0 for a mint)
	LSTTo        string // transfer recipient (0x0 for a burn)

	// depeg (signal_type 7: stablecoin depeg + contagion) only. No external price; the
	// implied price comes from our swaps and the deviation in bps carries the magnitude.
	IsDepeg          bool   // true when this is a depeg finding
	DepegSymbol      string // the depegging stablecoin symbol ("USDe" / "USDC" / "USDT")
	DepegImpliedPx   string // median implied price off the 1.0 peg (decimal string)
	DepegDeviationBp int    // |1 - impliedPrice| * 10000 (basis points off peg)
	DepegAffected    string // comma-joined labels of the at-risk pools holding the token ("" when none)
}

// Provider turns a SignalContext into a short analyst note. Implementations
// must be safe for concurrent use and must never panic on bad input.
type Provider interface {
	// Analyze returns a 2-3 sentence note for the signal. On any failure it
	// returns ("", err); callers treat enrichment as best-effort and must not
	// let a non-nil error block attestation.
	Analyze(ctx context.Context, in SignalContext) (note string, err error)
}

// Noop is the disabled enricher used when no LLM endpoint is configured.
type Noop struct{}

// Analyze always returns an empty note and no error, signaling that enrichment
// is intentionally disabled.
func (Noop) Analyze(context.Context, SignalContext) (string, error) {
	return "", nil
}

// OpenAIProvider calls an OpenAI-compatible /chat/completions endpoint to
// generate the analyst note. BaseURL is the API root (without the
// /chat/completions suffix), e.g. "https://api.openai.com/v1".
type OpenAIProvider struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// NewProvider returns an OpenAIProvider when both baseURL and apiKey are set,
// otherwise a Noop. The OpenAIProvider is given an HTTP client with a 20s
// timeout so a slow or hung LLM endpoint can never stall the pipeline.
func NewProvider(baseURL, apiKey, model string) Provider {
	if baseURL == "" || apiKey == "" {
		return Noop{}
	}
	return &OpenAIProvider{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 20 * time.Second},
	}
}

// chatMessage is one OpenAI chat message (system/user/assistant).
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the OpenAI-compatible /chat/completions request body.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
}

// chatResponse is the subset of the /chat/completions response we read.
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

const (
	// systemPrompt frames the model as a terse on-chain analyst. Keeping the
	// note to 2-3 sentences bounds token cost and keeps alerts skimmable.
	systemPrompt = "You are a concise on-chain liquidity analyst. You are given one anomaly in a " +
		"single Mantle DEX pool's flow over a 5-minute window. Metric values are in raw token units, " +
		"not USD: describe the relative change (how the latest value compares to the baseline median, " +
		"and the z-score); do NOT invent dollar amounts or other time spans. Write 2-3 sentences: first " +
		"what changed, then 2-3 plausible interpretations, then end with a single confidence word " +
		"(low, medium, or high). No preamble, no markdown, no bullet points."

	// smartMoneySystemPrompt frames the model for a signal_type 3 finding: an
	// accumulating wallet scored by a 0..100 composite, not a z-score, so the prompt
	// must not ask it to reason about a z-score or a 5-minute window.
	smartMoneySystemPrompt = "You are a concise on-chain smart-money analyst. You are given one wallet on " +
		"Mantle that is ACCUMULATING: it has made several large swaps (each well above the pool's median " +
		"swap size) across one or more DEX pools within a multi-hour window. Sizes are in raw token units, " +
		"not USD: reason about the multiple of the pool median and the repetition/breadth; do NOT invent " +
		"dollar amounts, a z-score, or a 5-minute window (this is accumulation over hours, not a flow " +
		"anomaly). Write 2-3 sentences: first what the wallet is doing, then 2-3 plausible interpretations " +
		"(e.g. informed accumulation, market-making, bridging/treasury flow), then end with a single " +
		"confidence word (low, medium, or high). No preamble, no markdown, no bullet points."

	// liquidationSystemPrompt frames the model for a signal_type 5 finding: an Aave V3
	// liquidation, a discrete lending event (not a flow anomaly or accumulation), so the
	// prompt must not ask for a z-score, a window, or a pool median.
	liquidationSystemPrompt = "You are a concise on-chain lending analyst. You are given one Aave V3 " +
		"LIQUIDATION on Mantle: a liquidator repaid part of a borrower's debt and seized the borrower's " +
		"collateral because the position became under-collateralized. A USD size may be given (the repaid " +
		"debt); if it is, you may reference it, but do NOT invent figures that are not given, a z-score, or a " +
		"time window. Treat the liquidator as a sophisticated/automated actor. Write 2-3 sentences: first " +
		"what happened (who liquidated whom, which assets), then 2-3 plausible interpretations (e.g. a " +
		"liquidation bot, market-stress cascade, a specific borrower being unwound), then end with a single " +
		"confidence word (low, medium, or high). No preamble, no markdown, no bullet points."

	// bigBorrowSystemPrompt frames the model for a signal_type 6 finding: a large
	// Aave V3 borrow on Mantle. A discrete lending event, not a flow anomaly: no
	// z-score, no window, no pool median.
	bigBorrowSystemPrompt = "You are a concise on-chain lending analyst. You are given one large Aave V3 " +
		"BORROW on Mantle: a wallet borrowed a sizeable amount of a reserve asset against its collateral. A " +
		"USD size is given (the borrowed amount); you may reference it, but do NOT invent other figures, a " +
		"z-score, or a time window. Write 2-3 sentences: first what was borrowed and by whom, then 2-3 " +
		"plausible interpretations (e.g. leveraged position, looping/yield strategy, treasury draw, " +
		"shorting), then end with a single confidence word (low, medium, or high). No preamble, no markdown, " +
		"no bullet points."

	// lstFlowSystemPrompt frames the model for a signal_type 4 finding: a large mETH /
	// cmETH transfer classified by counterparty as mint / burn / move. A discrete
	// token-flow event (no z-score, window, or pool median), amount in whole ETH-priced
	// tokens, so the model must not invent a dollar value.
	lstFlowSystemPrompt = "You are a concise on-chain analyst tracking liquid-staking-token (LST) flow on " +
		"Mantle. You are given ONE large transfer of an LST (mETH or cmETH, both ETH-denominated), classified " +
		"by counterparty as: mint (newly minted / bridged INTO Mantle, fresh ETH exposure entering), burn " +
		"(redeemed / bridged OUT of Mantle), or move (wallet-to-wallet / OTC). The size is in WHOLE LST tokens, " +
		"NOT USD: do NOT invent a dollar amount, a z-score, or a time window. Write 2-3 sentences: first what " +
		"moved (how many tokens, which direction, who), then 2-3 plausible interpretations and what it implies " +
		"for ETH exposure on Mantle (e.g. inflow/outflow of staked ETH, bridging, treasury/OTC accumulation), " +
		"then end with a single confidence word (low, medium, or high). No preamble, no markdown, no bullet points."

	// depegSystemPrompt frames the model for a signal_type 7 finding: a stablecoin depeg
	// detected from our swap data (median implied price off the 1.0 peg) plus a contagion
	// map. A systemic-risk condition, not a flow z-score or discrete event, so the prompt
	// must not ask for a window, a pool median, or an accumulation count; the implied
	// price is from our swaps (no oracle), deviation in bps off $1.
	depegSystemPrompt = "You are a concise on-chain systemic-risk analyst tracking stablecoin pegs on Mantle. " +
		"You are given ONE stablecoin that is trading OFF its $1 peg, measured as the median implied price of " +
		"recent swaps in a stable/stable pool (derived from on-chain swap amounts, NOT an oracle), expressed as a " +
		"price near 1.0 and a deviation in basis points (100 bps = 1%). You are also given the OTHER tracked pools " +
		"that hold the depegging token (the contagion / at-risk set). Do NOT invent a dollar volume, a z-score, or a " +
		"time window. Write 2-3 sentences: first which stablecoin is off peg and by how much (price + bps, and which " +
		"direction), then 2-3 plausible interpretations and what the contagion implies for the named at-risk pools " +
		"(e.g. redemption pressure, a temporary liquidity imbalance, real solvency concern), then end with a single " +
		"confidence word (low, medium, or high). No preamble, no markdown, no bullet points."

	// analystMaxTokens caps the completion size; a few sentences fit easily.
	analystMaxTokens = 220
)

// Analyze posts the signal to the configured chat-completions endpoint and
// returns the model's note. Any transport, status, decode, or empty-content
// problem is wrapped and returned with an empty note; the caller logs it and
// continues. Analyze never retries; enrichment is best-effort by design.
func (p *OpenAIProvider) Analyze(ctx context.Context, in SignalContext) (string, error) {
	if p.HTTP == nil {
		return "", fmt.Errorf("enrich: openai provider has no HTTP client")
	}

	// Each family gets its own system framing + user prompt; flow/whale (types 1/2)
	// keep the default. At most one Is* flag is set per signal, so one branch applies.
	sys := systemPrompt
	user := buildUserPrompt(in)
	switch {
	case in.IsLending:
		if in.IsLiquidation {
			sys = liquidationSystemPrompt
		} else {
			sys = bigBorrowSystemPrompt
		}
		user = buildLendingPrompt(in)
	case in.IsLSTFlow:
		sys = lstFlowSystemPrompt
		user = buildLSTFlowPrompt(in)
	case in.IsDepeg:
		sys = depegSystemPrompt
		user = buildDepegPrompt(in)
	case in.IsSmartMoney:
		sys = smartMoneySystemPrompt
		user = buildSmartMoneyPrompt(in)
	}

	reqBody := chatRequest{
		Model: p.Model,
		Messages: []chatMessage{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		},
		MaxTokens:   analystMaxTokens,
		Temperature: 0.2,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("enrich: marshal chat request: %w", err)
	}

	endpoint := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("enrich: build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("enrich: chat request failed: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("enrich: read chat response (status %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("enrich: chat endpoint returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var parsed chatResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", fmt.Errorf("enrich: decode chat response: %w", err)
	}

	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("enrich: chat response contained no choices")
	}

	note := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if note == "" {
		return "", fmt.Errorf("enrich: chat response note was empty")
	}
	return note, nil
}

// whaleMetric is the metric name the whale detector (signal_type 2) stamps on
// its signals; FallbackNote uses it to phrase a per-transaction note instead of
// a per-bucket one.
const whaleMetric = "whale_swap"

// FallbackNote builds a deterministic, LLM-free analyst note from a signal's own numbers,
// the never-empty backstop when the LLM is unavailable. It is marked automated so it is
// never mistaken for a model analysis, and never returns "" (a zero median still reports
// metric/pool/z-score). Each family is phrased to its own shape (accumulation, whale
// per-transaction, lending/LST/depeg events, else per-bucket).
func FallbackNote(in SignalContext) string {
	label := in.PoolLabel
	if label == "" {
		label = in.Pool
	}
	if label == "" {
		label = "(unknown pool)"
	}

	// The discrete-event families (lending, LST flow, depeg, smart-money) each return their
	// own phrasing before the median/z-score wording below, which does not apply to them.

	// Lending (type 5/6): a liquidation or big borrow with its USD size.
	if in.IsLending {
		size := ""
		if in.SizeUSD != "" {
			size = fmt.Sprintf(" (~$%s)", in.SizeUSD)
		}
		if in.IsLiquidation {
			liq := in.Liquidator
			if liq == "" {
				liq = "(unknown liquidator)"
			}
			user := in.LiquidatedUser
			if user == "" {
				user = "(unknown borrower)"
			}
			return fmt.Sprintf(
				"Automated note (LLM unavailable): an Aave V3 liquidation%s on Mantle; liquidator %s seized collateral from borrower %s. The on-chain record stands; an analyst note will follow if enrichment recovers.",
				size, liq, user,
			)
		}
		borrower := in.OnBehalfOf
		if borrower == "" {
			borrower = "(unknown wallet)"
		}
		return fmt.Sprintf(
			"Automated note (LLM unavailable): a large Aave V3 borrow%s on Mantle by %s against reserve %s. The on-chain record stands; an analyst note will follow if enrichment recovers.",
			size, borrower, label,
		)
	}

	// LST flow (type 4): N whole tokens minted / burned / moved.
	if in.IsLSTFlow {
		sym := in.LSTSymbol
		if sym == "" {
			sym = "LST"
		}
		amt := in.LSTValue
		if amt == "" {
			amt = "a large amount of"
		} else {
			amt += " " + sym
		}
		var phrase string
		switch in.LSTDirection {
		case "mint":
			phrase = fmt.Sprintf("%s was minted/bridged INTO Mantle (ETH exposure entering)", amt)
		case "burn":
			phrase = fmt.Sprintf("%s was redeemed/bridged OUT of Mantle (ETH exposure leaving)", amt)
		default: // "move" or unknown
			phrase = fmt.Sprintf("%s moved wallet-to-wallet on Mantle (possible OTC/treasury flow)", amt)
		}
		return fmt.Sprintf(
			"Automated note (LLM unavailable): an LST flow on Mantle: %s. The on-chain record stands; an analyst note will follow if enrichment recovers.",
			phrase,
		)
	}

	// Depeg (type 7): a stable off its $1 peg by N bps, with the at-risk pools.
	if in.IsDepeg {
		sym := in.DepegSymbol
		if sym == "" {
			sym = "a stablecoin"
		}
		px := in.DepegImpliedPx
		if px == "" {
			px = "below $1"
		}
		affected := in.DepegAffected
		if affected == "" {
			affected = "no other tracked pools"
		}
		return fmt.Sprintf(
			"Automated note (LLM unavailable): %s is trading off its $1 peg on Mantle (implied price %s, ~%d bps off), measured from on-chain swaps; at-risk pools holding it: %s. The on-chain record stands; an analyst note will follow if enrichment recovers.",
			sym, px, in.DepegDeviationBp, affected,
		)
	}

	// Smart-money (type 3): a wallet doing N large swaps across M pools, with the latest-swap
	// size multiple.
	if in.IsSmartMoney {
		actor := in.Actor
		if actor == "" {
			actor = "(unknown wallet)"
		}
		size := ""
		if in.SizeMultiple > 0 {
			size = fmt.Sprintf("; latest swap %.1fx the pool median", in.SizeMultiple)
		}
		return fmt.Sprintf(
			"Automated note (LLM unavailable): wallet %s is accumulating on %s with %d large swap(s) across %d pool(s) in %dh%s. The on-chain record stands; an analyst note will follow if enrichment recovers.",
			actor, label, in.AccumSwaps, in.DistinctPools, in.WindowHours, size,
		)
	}

	// ratio is the headline magnitude (latest vs median); guard the zero-median case.
	ratio := ""
	if in.Median != 0 {
		ratio = fmt.Sprintf("%.1fx its baseline median", in.LatestValue/in.Median)
	} else {
		ratio = "well above its baseline median"
	}

	if in.Metric == whaleMetric {
		return fmt.Sprintf(
			"Automated note (LLM unavailable): a single whale swap on %s was %s swap size (z=%.2f). The on-chain record stands; an analyst note will follow if enrichment recovers.",
			label, ratio, in.Zscore,
		)
	}
	return fmt.Sprintf(
		"Automated note (LLM unavailable): %s on %s hit %s (z=%.2f) in the last 5m. The on-chain record stands; an analyst note will follow if enrichment recovers.",
		in.Metric, label, ratio, in.Zscore,
	)
}

// buildUserPrompt renders the signal into a compact, deterministic prompt body.
func buildUserPrompt(in SignalContext) string {
	label := in.PoolLabel
	if label == "" {
		label = "(unlabeled)"
	}
	ratio := ""
	if in.Median != 0 {
		ratio = fmt.Sprintf("\nLatest / median ratio: %.1fx", in.LatestValue/in.Median)
	}
	return fmt.Sprintf(
		"Pool: %s %s\nWindow: 5 minutes\nMetric: %s\nLatest value (raw units): %g\nBaseline median (raw units): %g%s\nRobust z-score: %.2f\nWindow start: %s",
		label, in.Pool, in.Metric, in.LatestValue, in.Median, ratio, in.Zscore, in.BucketTime,
	)
}

// buildSmartMoneyPrompt renders a signal_type 3 (accumulating wallet) finding into a
// compact, deterministic prompt body (actor, repetition + cross-pool breadth, latest
// swap size vs pool median). Pairs with smartMoneySystemPrompt.
func buildSmartMoneyPrompt(in SignalContext) string {
	label := in.PoolLabel
	if label == "" {
		label = "(unlabeled)"
	}
	actor := in.Actor
	if actor == "" {
		actor = "(unknown wallet)"
	}
	size := ""
	if in.SizeMultiple > 0 {
		size = fmt.Sprintf("\nLatest swap size vs pool median: %.1fx", in.SizeMultiple)
	}
	return fmt.Sprintf(
		"Latest pool: %s %s\nAccumulating wallet: %s\nLarge swaps in window: %d\nDistinct pools touched: %d\nWindow length: %d hours\nComposite smart-money score (0-100, not a z-score): %.0f%s\nLatest swap time: %s",
		label, in.Pool, actor, in.AccumSwaps, in.DistinctPools, in.WindowHours, in.Zscore, size, in.BucketTime,
	)
}

// buildLendingPrompt renders an Aave V3 lending finding (liquidation type 5 / big borrow
// type 6) into a compact, deterministic prompt body (parties + assets + display-only USD
// size). Pairs with liquidationSystemPrompt / bigBorrowSystemPrompt.
func buildLendingPrompt(in SignalContext) string {
	size := "unknown (non-stable asset)"
	if in.SizeUSD != "" {
		size = "$" + in.SizeUSD
	}
	if in.IsLiquidation {
		return fmt.Sprintf(
			"Event: Aave V3 liquidation on Mantle\nLiquidator (actor): %s\nLiquidated borrower: %s\nCollateral seized (asset): %s\nDebt repaid (asset): %s\nApprox USD debt repaid: %s\nTime: %s",
			in.Liquidator, in.LiquidatedUser, in.CollateralAsset, in.DebtAsset, size, in.BucketTime,
		)
	}
	return fmt.Sprintf(
		"Event: large Aave V3 borrow on Mantle\nBorrower (on behalf of): %s\nReserve borrowed (asset): %s\nApprox USD borrowed: %s\nTime: %s",
		in.OnBehalfOf, in.Reserve, size, in.BucketTime,
	)
}

// buildLSTFlowPrompt renders an LST-flow finding (type 4: a large mETH/cmETH transfer)
// into a compact, deterministic prompt body (token, direction, whole-token amount,
// counterparties). Pairs with lstFlowSystemPrompt.
func buildLSTFlowPrompt(in SignalContext) string {
	sym := in.LSTSymbol
	if sym == "" {
		sym = "LST"
	}
	amt := in.LSTValue
	if amt == "" {
		amt = "(unknown)"
	}
	dir := in.LSTDirection
	if dir == "" {
		dir = "move"
	}
	return fmt.Sprintf(
		"Event: large LST transfer on Mantle\nToken: %s\nDirection: %s (mint = bridged/staked IN, burn = redeemed/bridged OUT, move = wallet-to-wallet)\nAmount (whole tokens): %s\nFrom: %s\nTo: %s\nTime: %s",
		sym, dir, amt, in.LSTFrom, in.LSTTo, in.BucketTime,
	)
}

// buildDepegPrompt renders a depeg finding (type 7: a stablecoin off its $1 peg with a
// contagion map) into a compact, deterministic prompt body (which stable, implied price +
// bps, source pool, at-risk pools). Pairs with depegSystemPrompt.
func buildDepegPrompt(in SignalContext) string {
	sym := in.DepegSymbol
	if sym == "" {
		sym = "(unknown stablecoin)"
	}
	px := in.DepegImpliedPx
	if px == "" {
		px = "(unknown)"
	}
	affected := in.DepegAffected
	if affected == "" {
		affected = "(none tracked)"
	}
	label := in.PoolLabel
	if label == "" {
		label = "(unlabeled)"
	}
	return fmt.Sprintf(
		"Event: stablecoin DEPEG on Mantle (from on-chain swap data, no oracle)\nDepegging stablecoin: %s\nMedian implied price (≈1.0 at peg): %s\nDeviation from peg: %d bps (below $1)\nMeasured in pool: %s\nAt-risk pools holding the token (contagion): %s\nTime: %s",
		sym, px, in.DepegDeviationBp, label, affected, in.BucketTime,
	)
}
