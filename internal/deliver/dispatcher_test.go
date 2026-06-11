package deliver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"ocr/internal/store"
)

const testScanBase = "https://mantlescan.xyz"

// decFromStr builds a decimal from a string for the lending size_usd tests.
func decFromStr(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// A flow (type 1) signal renders the original z-score framing: a "z=" title, a
// "Z-score:" line, the pool link, and (when set) the note + attestation. This
// guards that the type-3 branch leaves the existing type-1/2 rendering unchanged.
func TestFormatSignal_FlowUnchanged(t *testing.T) {
	s := store.Signal{
		SignalType: 1,
		Pool:       "0xbcf99c834e65e8a58090e20edc058279317865bd",
		Metric:     "vol0",
		Zscore:     9.12,
		LLMNote:    "Volume spiked. medium",
	}
	out := FormatSignal(s, "USDC/USDe", "0xtxhash", testScanBase)

	for _, want := range []string{
		"<b>vol0</b> z=9.12",
		"Pool: USDC/USDe",
		"Z-score: 9.12",
		"Note: Volume spiked. medium",
		"/tx/0xtxhash",
		"Attestation:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("flow message missing %q:\n%s", want, out)
		}
	}
	// It must not use smart-money framing.
	for _, banned := range []string{"smart money", "score=", "Score:", "Wallet:"} {
		if strings.Contains(out, banned) {
			t.Errorf("flow message leaked smart-money wording %q:\n%s", banned, out)
		}
	}
}

// A whale (type 2) signal also keeps the z-score framing (it is per-event but
// still scored by a z-score), unchanged by the type-3 branch.
func TestFormatSignal_WhaleUnchanged(t *testing.T) {
	s := store.Signal{
		SignalType: 2,
		Pool:       "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5",
		Metric:     "whale_swap",
		Zscore:     8.5,
	}
	out := FormatSignal(s, "USDe/WMNT", "", testScanBase)
	if !strings.Contains(out, "<b>whale_swap</b> z=8.50") || !strings.Contains(out, "Z-score: 8.50") {
		t.Errorf("whale message lost z-score framing:\n%s", out)
	}
	// No attestation tx -> no Attestation line.
	if strings.Contains(out, "Attestation:") {
		t.Errorf("whale message with empty tx should omit the attestation line:\n%s", out)
	}
}

// A smart-money (type 3) signal is framed as a score finding (not a z-score) and
// names the accumulating wallet decoded from the payload, with the accumulation
// evidence.
func TestFormatSignal_SmartMoneyScoreFraming(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"actor":          "0x00000000000000000000000000000000000000aa",
		"accum_swaps":    4,
		"distinct_pools": 2,
		"size_multiple":  7.5,
		"score":          56,
		"window_hours":   24,
	})
	s := store.Signal{
		SignalType: 3,
		Pool:       "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5",
		Metric:     "smart_money",
		Zscore:     56, // the composite score, carried in Zscore
		Payload:    payload,
	}
	out := FormatSignal(s, "USDe/WMNT", "0xtx3", testScanBase)

	for _, want := range []string{
		"<b>smart money</b> score=56",
		"Score: 56",
		"Wallet:",
		"/address/0x00000000000000000000000000000000000000aa",
		"4 large swaps across 2 pool(s)",
		"7.5x pool median",
		"/tx/0xtx3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("smart-money message missing %q:\n%s", want, out)
		}
	}
	// It must not use the z-score framing.
	for _, banned := range []string{"z=", "Z-score:"} {
		if strings.Contains(out, banned) {
			t.Errorf("smart-money message leaked z-score wording %q:\n%s", banned, out)
		}
	}
}

// A smart-money signal whose payload fails to decode still renders a valid alert
// (score + pool, no wallet line) rather than dropping the alert or panicking.
func TestFormatSignal_SmartMoneyMalformedPayload(t *testing.T) {
	s := store.Signal{
		SignalType: 3,
		Pool:       "0xpool",
		Metric:     "smart_money",
		Zscore:     50,
		Payload:    []byte(`{not json`),
	}
	out := FormatSignal(s, "USDe/WMNT", "", testScanBase)
	if !strings.Contains(out, "score=50") || !strings.Contains(out, "Score: 50") {
		t.Errorf("malformed-payload smart-money message lost the score:\n%s", out)
	}
	if strings.Contains(out, "Wallet:") {
		t.Errorf("malformed payload should omit the wallet line:\n%s", out)
	}
}

// A liquidation (type 5) renders the lending framing: the "liquidation" headline
// with the USD size, the borrower (subject) + liquidator (actor) linked, and no
// z-score wording. This also guards the type-5 branch did not alter types 1/2/3.
func TestFormatSignal_Liquidation(t *testing.T) {
	usd := decFromStr("50000")
	s := store.Signal{
		SignalType: 5,
		Pool:       "0xborrower", // subject = the liquidated borrower
		Metric:     "liquidation",
		Zscore:     7.7,
		Actor:      "0xliquidatoreoa",
		SizeUSD:    &usd,
		Payload:    []byte(`{"liquidator":"0xliquidatoreoa","user":"0xborrower","debt_asset":"0xusdt"}`),
	}
	out := FormatSignal(s, "", "0xtx", testScanBase)
	for _, want := range []string{
		"<b>liquidation</b> ~$50000",
		"Borrower: ",
		"/address/0xborrower",
		"Liquidator: ",
		"/address/0xliquidatoreoa",
		"/tx/0xtx",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("liquidation message missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"z=", "Z-score:", "score=", "smart money"} {
		if strings.Contains(out, banned) {
			t.Errorf("liquidation message leaked %q:\n%s", banned, out)
		}
	}
}

// A big borrow (type 6) renders the borrow framing: the "big borrow" headline with
// the USD size, the reserve (subject) + borrower (actor) linked.
func TestFormatSignal_BigBorrow(t *testing.T) {
	usd := decFromStr("75000")
	s := store.Signal{
		SignalType: 6,
		Pool:       "0xreserve", // subject = the borrowed reserve
		Metric:     "big_borrow",
		Zscore:     7.87,
		Actor:      "0xborrowerwallet",
		SizeUSD:    &usd,
	}
	out := FormatSignal(s, "", "", testScanBase)
	for _, want := range []string{
		"<b>big borrow</b> ~$75000",
		"Reserve: ",
		"/address/0xreserve",
		"Borrower: ",
		"/address/0xborrowerwallet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("big-borrow message missing %q:\n%s", want, out)
		}
	}
	// No attestation tx -> no Attestation line; no z-score wording.
	for _, banned := range []string{"Attestation:", "z=", "Z-score:"} {
		if strings.Contains(out, banned) {
			t.Errorf("big-borrow message leaked %q:\n%s", banned, out)
		}
	}
}

// A lending signal with no USD size (non-stable asset) and no payload still renders
// a valid alert (kind + subject), proving the size and actor lines are optional.
func TestFormatSignal_LendingNoSizeNoPayload(t *testing.T) {
	s := store.Signal{
		SignalType: 5,
		Pool:       "0xborrower",
		Metric:     "liquidation",
		Zscore:     3,
		// no SizeUSD, no Actor, no Payload
	}
	out := FormatSignal(s, "", "", testScanBase)
	if !strings.Contains(out, "<b>liquidation</b>") || strings.Contains(out, "~$") {
		t.Errorf("no-size liquidation should show the kind but no USD size:\n%s", out)
	}
	if !strings.Contains(out, "Borrower: ") {
		t.Errorf("liquidation should still show the borrower subject:\n%s", out)
	}
}

// An LST-flow (type 4) mint renders the LST framing: the "LST minted in" headline with
// the whole-token amount + symbol, the token (subject) + actor linked, and no z-score
// or USD wording. Guards that the type-4 branch did not alter types 1/2/3/5/6.
func TestFormatSignal_LSTFlowMint(t *testing.T) {
	s := store.Signal{
		SignalType: 4,
		Pool:       "0xmeth", // subject = the LST token
		Metric:     "lst_flow",
		Zscore:     5.0,
		Actor:      "0xrecipienteoa",
		Payload:    []byte(`{"symbol":"mETH","direction":"mint","value_lst":"100"}`),
	}
	out := FormatSignal(s, "", "0xtx", testScanBase)
	for _, want := range []string{
		"LST minted in",
		"100 mETH",
		"Token: ",
		"/address/0xmeth",
		"Actor: ",
		"/address/0xrecipienteoa",
		"/tx/0xtx",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lst-flow mint message missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"z=", "Z-score:", "score=", "smart money", "~$"} {
		if strings.Contains(out, banned) {
			t.Errorf("lst-flow mint message leaked %q:\n%s", banned, out)
		}
	}
}

// An LST-flow burn renders the "redeemed out" headline; a move renders "moved". A
// payload that fails to decode still produces a valid alert (kind + token subject).
func TestFormatSignal_LSTFlowBurnAndMoveAndMalformed(t *testing.T) {
	burn := FormatSignal(store.Signal{
		SignalType: 4, Pool: "0xcmeth", Metric: "lst_flow",
		Payload: []byte(`{"symbol":"cmETH","direction":"burn","value_lst":"50"}`),
	}, "", "", testScanBase)
	if !strings.Contains(burn, "LST redeemed out") || !strings.Contains(burn, "50 cmETH") {
		t.Errorf("burn should render the redeemed-out headline:\n%s", burn)
	}

	move := FormatSignal(store.Signal{
		SignalType: 4, Pool: "0xmeth", Metric: "lst_flow",
		Payload: []byte(`{"symbol":"mETH","direction":"move","value_lst":"25"}`),
	}, "", "", testScanBase)
	if !strings.Contains(move, "LST moved") || !strings.Contains(move, "25 mETH") {
		t.Errorf("move should render the moved headline:\n%s", move)
	}

	// Malformed payload: still a valid alert with the token subject (no panic, no drop).
	malformed := FormatSignal(store.Signal{
		SignalType: 4, Pool: "0xmeth", Metric: "lst_flow",
		Payload: []byte(`not json`),
	}, "", "", testScanBase)
	if !strings.Contains(malformed, "<b>LST moved</b>") || !strings.Contains(malformed, "/address/0xmeth") {
		t.Errorf("malformed lst-flow payload should still render kind + token subject:\n%s", malformed)
	}
}

// A depeg (type 7) alert renders the DEPEG headline with the stablecoin + bps off peg,
// the implied price, the depegging token (subject) linked, the at-risk pools (the
// contagion set, each linked), and the attestation, never a z-score or USD size.
func TestFormatSignal_Depeg(t *testing.T) {
	s := store.Signal{
		SignalType: 7,
		Pool:       "0xusde", // subject = the depegging token
		Metric:     "depeg",
		Zscore:     4.7,
		Payload: []byte(`{"token_symbol":"USDe","implied_price":"0.971","deviation_bps":290,` +
			`"affected_pools":[{"address":"0xpoola","label":"USDC/USDe"},{"address":"0xpoolb","label":"USDT/USDe"}]}`),
	}
	out := FormatSignal(s, "", "0xtxdepeg", testScanBase)
	for _, want := range []string{
		"<b>DEPEG</b>",
		"USDe 290 bps off $1",
		"Implied price: 0.971",
		"Token: ",
		"/address/0xusde",
		"At-risk pools (2):",
		"USDC/USDe",
		"/address/0xpoola",
		"USDT/USDe",
		"/address/0xpoolb",
		"/tx/0xtxdepeg",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("depeg message missing %q:\n%s", want, out)
		}
	}
	// A depeg has no z-score, no smart-money score, and no USD size.
	for _, banned := range []string{"z=", "Z-score:", "score=", "smart money", "~$"} {
		if strings.Contains(out, banned) {
			t.Errorf("depeg message leaked %q:\n%s", banned, out)
		}
	}
}

// A depeg alert with no contagion renders "none tracked"; a malformed payload still
// renders a valid alert with the token subject (no panic, no drop).
func TestFormatSignal_DepegNoContagionAndMalformed(t *testing.T) {
	none := FormatSignal(store.Signal{
		SignalType: 7, Pool: "0xusdt", Metric: "depeg",
		Payload: []byte(`{"token_symbol":"USDT","implied_price":"0.99","deviation_bps":100}`),
	}, "", "", testScanBase)
	if !strings.Contains(none, "At-risk pools: none tracked") {
		t.Errorf("a depeg with no contagion should render 'none tracked':\n%s", none)
	}

	malformed := FormatSignal(store.Signal{
		SignalType: 7, Pool: "0xusde", Metric: "depeg",
		Payload: []byte(`not json`),
	}, "", "", testScanBase)
	if !strings.Contains(malformed, "<b>DEPEG</b>") || !strings.Contains(malformed, "/address/0xusde") {
		t.Errorf("malformed depeg payload should still render the headline + token subject:\n%s", malformed)
	}
}
