package deliver

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"ocr/internal/deliver/card"
	"ocr/internal/store"
)

const (
	testScanBase = "https://mantlescan.xyz"
	testDash     = "https://onchainradar.tech"
)

// decFromStr builds a decimal from a string for the size_usd tests.
func decFromStr(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// buttonURL returns the URL of the first inline button whose label equals text, or ""
// when the view has no such button. It lets the tests assert the keyboard wiring without
// caring about row layout.
func buttonURL(v MessageView, text string) string {
	if v.Keyboard == nil {
		return ""
	}
	for _, row := range v.Keyboard.Rows {
		for _, btn := range row {
			if btn.Text == text {
				return btn.URL
			}
		}
	}
	return ""
}

// noZScoreJargon fails when an alert exposes the raw z-score wording the channel format
// replaced with plain-language multiples.
func noZScoreJargon(t *testing.T, text string) {
	t.Helper()
	for _, banned := range []string{"z-score", "z=", "Z-score", "zscore"} {
		if strings.Contains(text, banned) {
			t.Errorf("alert leaked z-score jargon %q:\n%s", banned, text)
		}
	}
}

// specValue returns the value of the card spec labelled label, or "".
func specValue(d card.Data, label string) string {
	for _, s := range d.Specs {
		if s.Label == label {
			return s.Value
		}
	}
	return ""
}

// A flow anomaly (type 1) reads as a plain-language burst over the pool's own baseline,
// with a pool copy block (a flow has no actor) and the matching card fields.
func TestFormatSignal_Flow(t *testing.T) {
	s := store.Signal{
		ID:         142,
		CreatedAt:  time.Date(2026, 6, 12, 14, 31, 0, 0, time.UTC),
		SignalType: 1,
		Pool:       "0xbcf99c834e65e8a58090e20edc058279317865bd",
		Metric:     "vol0",
		Zscore:     9.12,
		Payload:    []byte(`{"latest":9120,"median":1000}`),
		LLMNote:    "Volume spiked.",
	}
	v := FormatSignal(s, Context{PoolLabel: "USDC/USDe", Dex: "Agni"}, "0xtxhash", testScanBase, testDash)

	for _, want := range []string{
		"📊 <b>Volume spike</b> · <b>USDC / USDe</b> · <i>Agni</i>",
		"<b>9.1×</b> this pool's own 48-hour average",
		"<blockquote expandable><b>Analyst take</b>\nVolume spiked.</blockquote>",
		"Pool  <code>" + s.Pool + "</code>",
		"<tg-time unix=",
	} {
		if !strings.Contains(v.Caption, want) {
			t.Errorf("flow caption missing %q:\n%s", want, v.Caption)
		}
	}
	if got := buttonURL(v, "On-chain proof"); got != testScanBase+"/tx/0xtxhash" {
		t.Errorf("flow proof button = %q", got)
	}
	if got := buttonURL(v, "View pool"); got != testScanBase+"/address/"+s.Pool {
		t.Errorf("flow pool button = %q", got)
	}
	if got := buttonURL(v, "Live dashboard"); got != testDash {
		t.Errorf("flow dashboard button = %q", got)
	}
	if got := buttonURL(v, "Wallet"); got != "" {
		t.Errorf("flow has no actor, so no wallet button; got %q", got)
	}

	c := v.Card
	if c.Kind != "flow" || c.TypeLabel != "VOLUME SPIKE" {
		t.Errorf("flow card kind/label = %q/%q", c.Kind, c.TypeLabel)
	}
	if c.Headline != "USDC / USDe" {
		t.Errorf("flow card headline = %q, want spaced pair", c.Headline)
	}
	if c.SigID != "SIG #142" {
		t.Errorf("flow card sig id = %q", c.SigID)
	}
	if got := specValue(c, "VS 48H MEDIAN"); got != "9.1×" {
		t.Errorf("flow card multiple spec = %q", got)
	}
	if got := specValue(c, "VENUE"); got != "AGNI" {
		t.Errorf("flow card venue spec = %q", got)
	}
	if c.Timestamp != "12 JUN 2026 · 14:31 UTC" {
		t.Errorf("flow card timestamp = %q", c.Timestamp)
	}
	if c.AttestTxShort == "" {
		t.Error("flow card attest tx short is empty for an attested signal")
	}
	noZScoreJargon(t, v.Caption)
}

// A whale (type 2) shows the dollar size and the multiple of the pool's usual trade,
// with the wallet as the copy block. With an empty attest tx the proof button is
// omitted and the card footer falls back to the pending state.
func TestFormatSignal_Whale(t *testing.T) {
	usd := decFromStr("48200")
	s := store.Signal{
		SignalType: 2,
		Pool:       "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5",
		Metric:     "whale_swap",
		Zscore:     6.2,
		Actor:      "0x8d58a3f1c0b2e7a9d4f6c1b8e2a05d7f3c9b6e10",
		SizeUSD:    &usd,
		Payload:    []byte(`{"size0":18000,"median":1000}`),
	}
	v := FormatSignal(s, Context{PoolLabel: "USDe/WMNT", Dex: "Agni"}, "", testScanBase, testDash)

	for _, want := range []string{
		"🐋 <b>Whale swap</b> · <b>USDe / WMNT</b> · <i>Agni</i>",
		"<b>$48,200</b> swap, <b>18×</b> the pool's usual trade size.",
		"Wallet  <code>" + s.Actor + "</code>",
	} {
		if !strings.Contains(v.Caption, want) {
			t.Errorf("whale caption missing %q:\n%s", want, v.Caption)
		}
	}
	if got := buttonURL(v, "On-chain proof"); got != "" {
		t.Errorf("whale with empty tx must omit the proof button; got %q", got)
	}
	if got := buttonURL(v, "Wallet"); got != testScanBase+"/address/"+s.Actor {
		t.Errorf("whale wallet button = %q", got)
	}

	c := v.Card
	if c.Kind != "whale" || c.Headline != "USDe / WMNT" {
		t.Errorf("whale card kind/headline = %q/%q", c.Kind, c.Headline)
	}
	if got := specValue(c, "NOTIONAL"); got != "$48.2K" {
		t.Errorf("whale card notional spec = %q", got)
	}
	if got := specValue(c, "VS POOL MEDIAN"); got != "18×" {
		t.Errorf("whale card multiple spec = %q", got)
	}
	if c.AttestTxShort != "" {
		t.Errorf("unattested whale card must leave the footer tx empty, got %q", c.AttestTxShort)
	}
	noZScoreJargon(t, v.Caption)
}

// A smart-money (type 3) alert reads as accumulation with a 0..100 score and the
// evidence; a decode failure still renders the score.
func TestFormatSignal_SmartMoney(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"actor":          "0x00000000000000000000000000000000000000aa",
		"accum_swaps":    6,
		"distinct_pools": 3,
		"size_multiple":  12.0,
	})
	s := store.Signal{
		SignalType: 3,
		Pool:       "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5",
		Metric:     "smart_money",
		Zscore:     78, // the composite score, carried in Zscore
		Actor:      "0x00000000000000000000000000000000000000aa",
		Payload:    payload,
	}
	v := FormatSignal(s, Context{PoolLabel: "USDC/USDe"}, "0xtx3", testScanBase, testDash)

	for _, want := range []string{
		"🧠 <b>Smart money</b> · <b>USDC / USDe</b>",
		"<b>6 large buys</b> across <b>3 pool(s)</b>",
		"Score <b>78/100</b>",
		"Wallet  <code>" + s.Actor + "</code>",
	} {
		if !strings.Contains(v.Caption, want) {
			t.Errorf("smart-money caption missing %q:\n%s", want, v.Caption)
		}
	}
	if got := buttonURL(v, "Wallet"); got != testScanBase+"/address/"+s.Actor {
		t.Errorf("smart-money wallet button = %q", got)
	}

	c := v.Card
	if got := specValue(c, "SMART SCORE"); got != "78" {
		t.Errorf("smart card score spec = %q", got)
	}
	if got := specValue(c, "LARGE BUYS"); got != "6" {
		t.Errorf("smart card buys spec = %q", got)
	}
	if got := specValue(c, "POOLS"); got != "3" {
		t.Errorf("smart card pools spec = %q", got)
	}
	noZScoreJargon(t, v.Caption)
}

func TestFormatSignal_SmartMoneyMalformed(t *testing.T) {
	s := store.Signal{
		SignalType: 3, Pool: "0xpool", Metric: "smart_money", Zscore: 50,
		Payload: []byte(`{not json`),
	}
	v := FormatSignal(s, Context{PoolLabel: "USDC/USDe"}, "", testScanBase, testDash)
	if !strings.Contains(v.Caption, "Score <b>50/100</b>") {
		t.Errorf("malformed smart-money lost the score:\n%s", v.Caption)
	}
	if strings.Contains(v.Caption, "large buys") {
		t.Errorf("malformed payload should omit the evidence line:\n%s", v.Caption)
	}
}

// A liquidation (type 5) shows the dollar size with a plain verb, a borrower-explorer
// button, and the liquidator wallet as the copy block.
func TestFormatSignal_Liquidation(t *testing.T) {
	usd := decFromStr("50000")
	s := store.Signal{
		SignalType: 5,
		Pool:       "0xbcf99c834e65e8a58090e20edc058279317865bd",
		Metric:     "liquidation",
		Zscore:     7.7,
		Actor:      "0x8d58a3f1c0b2e7a9d4f6c1b8e2a05d7f3c9b6e10",
		SizeUSD:    &usd,
	}
	v := FormatSignal(s, Context{}, "0xtx", testScanBase, testDash)

	for _, want := range []string{
		"⚡ <b>Liquidation</b> · <b>Aave V3</b>",
		"<b>$50,000</b> position liquidated",
		"Liquidator  <code>" + s.Actor + "</code>",
	} {
		if !strings.Contains(v.Caption, want) {
			t.Errorf("liquidation caption missing %q:\n%s", want, v.Caption)
		}
	}
	if got := buttonURL(v, "View borrower"); got != testScanBase+"/address/"+s.Pool {
		t.Errorf("liquidation borrower button = %q", got)
	}
	if got := buttonURL(v, "Wallet"); got != testScanBase+"/address/"+s.Actor {
		t.Errorf("liquidation wallet button = %q", got)
	}

	c := v.Card
	if c.Kind != "liq" || c.Headline != "AAVE V3" {
		t.Errorf("liquidation card kind/headline = %q/%q", c.Kind, c.Headline)
	}
	if got := specValue(c, "EVENT"); got != "LIQUIDATION" {
		t.Errorf("liquidation card event spec = %q", got)
	}
	if got := specValue(c, "SIZE"); got != "$50.0K" {
		t.Errorf("liquidation card size spec = %q", got)
	}
	noZScoreJargon(t, v.Caption)
}

// A big borrow (type 6) shows the dollar size with "borrowed" and a reserve button.
func TestFormatSignal_BigBorrow(t *testing.T) {
	usd := decFromStr("75000")
	s := store.Signal{
		SignalType: 6,
		Pool:       "0xa125af1a4704044501fe12ca9567ef1550e430e8",
		Metric:     "big_borrow",
		Zscore:     7.87,
		Actor:      "0x2c7b9e41a0d3f85c6b1e2a9d7f04c3b8e6a1d052",
		SizeUSD:    &usd,
	}
	v := FormatSignal(s, Context{}, "", testScanBase, testDash)

	for _, want := range []string{"💵 <b>Big borrow</b> · <b>Aave V3</b>", "<b>$75,000</b> borrowed"} {
		if !strings.Contains(v.Caption, want) {
			t.Errorf("big-borrow caption missing %q:\n%s", want, v.Caption)
		}
	}
	if got := buttonURL(v, "View reserve"); got != testScanBase+"/address/"+s.Pool {
		t.Errorf("big-borrow reserve button = %q", got)
	}
	if got := buttonURL(v, "On-chain proof"); got != "" {
		t.Errorf("empty tx must omit the proof button; got %q", got)
	}
	if got := specValue(v.Card, "EVENT"); got != "BORROW" {
		t.Errorf("big-borrow card event spec = %q", got)
	}
}

// An LST-flow (type 4) mint reads as "staked in" with the grouped amount and a tagline,
// and links the token and wallet.
func TestFormatSignal_LSTFlowMint(t *testing.T) {
	s := store.Signal{
		SignalType: 4,
		Pool:       "0xcda86a272531e8640cd7f1a92c01839911b90bb0",
		Metric:     "lst_flow",
		Actor:      "0x91d4a2ff60bb18c3e7a0d9f5c2b8e41a6d037c2e",
		Payload:    []byte(`{"symbol":"mETH","direction":"mint","value_lst":"1240"}`),
	}
	v := FormatSignal(s, Context{}, "0xtx", testScanBase, testDash)

	for _, want := range []string{
		"🟢 <b>mETH staked in</b> · <b>Mantle</b>",
		"<b>1,240 mETH</b>, fresh ETH into Mantle.",
		"Wallet  <code>" + s.Actor + "</code>",
	} {
		if !strings.Contains(v.Caption, want) {
			t.Errorf("lst-flow mint caption missing %q:\n%s", want, v.Caption)
		}
	}
	if got := buttonURL(v, "View token"); got != testScanBase+"/address/"+s.Pool {
		t.Errorf("lst-flow token button = %q", got)
	}
	if got := buttonURL(v, "Wallet"); got != testScanBase+"/address/"+s.Actor {
		t.Errorf("lst-flow wallet button = %q", got)
	}

	c := v.Card
	if c.Kind != "lst" || c.Headline != "mETH" {
		t.Errorf("lst card kind/headline = %q/%q", c.Kind, c.Headline)
	}
	if got := specValue(c, "DIRECTION"); got != "STAKED IN" {
		t.Errorf("lst card direction spec = %q", got)
	}
	if got := specValue(c, "AMOUNT"); got != "1,240" {
		t.Errorf("lst card amount spec = %q", got)
	}
}

// An LST-flow burn reads as "redeemed"; a move reads as "moved"; a malformed payload
// still renders the default verb and the token button (token becomes the copy block).
func TestFormatSignal_LSTFlowBurnMoveMalformed(t *testing.T) {
	burn := FormatSignal(store.Signal{
		SignalType: 4, Pool: "0xcmeth", Metric: "lst_flow",
		Payload: []byte(`{"symbol":"cmETH","direction":"burn","value_lst":"50"}`),
	}, Context{}, "", testScanBase, testDash)
	if !strings.Contains(burn.Caption, "<b>cmETH redeemed</b> · <b>Mantle</b>") || !strings.Contains(burn.Caption, "<b>50 cmETH</b>") {
		t.Errorf("burn should render the redeemed headline:\n%s", burn.Caption)
	}

	move := FormatSignal(store.Signal{
		SignalType: 4, Pool: "0xmeth", Metric: "lst_flow",
		Payload: []byte(`{"symbol":"mETH","direction":"move","value_lst":"25"}`),
	}, Context{}, "", testScanBase, testDash)
	if !strings.Contains(move.Caption, "<b>mETH moved</b> · <b>Mantle</b>") || !strings.Contains(move.Caption, "<b>25 mETH</b>") {
		t.Errorf("move should render the moved headline:\n%s", move.Caption)
	}

	malformed := FormatSignal(store.Signal{
		SignalType: 4, Pool: "0xcda86a272531e8640cd7f1a92c01839911b90bb0", Metric: "lst_flow",
		Payload: []byte(`not json`),
	}, Context{}, "", testScanBase, testDash)
	if !strings.Contains(malformed.Caption, "<b>LST moved</b> · <b>Mantle</b>") {
		t.Errorf("malformed lst-flow should still render the default verb:\n%s", malformed.Caption)
	}
	if buttonURL(malformed, "View token") == "" {
		t.Errorf("malformed lst-flow should still expose the token button:\n%s", malformed.Caption)
	}
	// No actor: the token address becomes the tap-to-copy artifact.
	if !strings.Contains(malformed.Caption, "Token  <code>0xcda86a272531e8640cd7f1a92c01839911b90bb0</code>") {
		t.Errorf("actor-less lst-flow should copy the token address:\n%s", malformed.Caption)
	}
}

// A depeg (type 7) shows the implied price, the bps deviation, the exposed pools, and a
// token copy block, with no z-score wording.
func TestFormatSignal_Depeg(t *testing.T) {
	s := store.Signal{
		SignalType: 7,
		Pool:       "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34",
		Metric:     "depeg",
		Zscore:     4.7,
		Payload: []byte(`{"token_symbol":"USDe","implied_price":"0.971","deviation_bps":290,` +
			`"affected_pools":[{"address":"0xpoola","label":"USDC/USDe"},{"address":"0xpoolb","label":"USDT/USDe"}]}`),
	}
	v := FormatSignal(s, Context{}, "0xtxdepeg", testScanBase, testDash)

	for _, want := range []string{
		"⚠️ <b>Depeg alert</b> · <b>USDe</b>",
		"USDe trading at <b>$0.971</b>, <b>290 bps below</b> the $1 peg.",
		"Exposed: USDC/USDe · USDT/USDe.",
		"Token  <code>" + s.Pool + "</code>",
	} {
		if !strings.Contains(v.Caption, want) {
			t.Errorf("depeg caption missing %q:\n%s", want, v.Caption)
		}
	}
	if got := buttonURL(v, "View token"); got != testScanBase+"/address/"+s.Pool {
		t.Errorf("depeg token button = %q", got)
	}
	if got := buttonURL(v, "On-chain proof"); got != testScanBase+"/tx/0xtxdepeg" {
		t.Errorf("depeg proof button = %q", got)
	}
	if got := buttonURL(v, "Wallet"); got != "" {
		t.Errorf("depeg has no actor, so no wallet button; got %q", got)
	}

	c := v.Card
	if c.Kind != "depeg" || c.Headline != "USDe" {
		t.Errorf("depeg card kind/headline = %q/%q", c.Kind, c.Headline)
	}
	if got := specValue(c, "DEVIATION"); got != "2.9%" {
		t.Errorf("depeg card deviation spec = %q", got)
	}
	if got := specValue(c, "EXPOSED POOLS"); got != "2" {
		t.Errorf("depeg card exposed spec = %q", got)
	}
	noZScoreJargon(t, v.Caption)
}

// The caption must stay inside the Bot API's 1024 visible-character budget even with an
// oversized analyst note: the note is clamped, the structure is preserved.
func TestCaption_BudgetWithLongNote(t *testing.T) {
	usd := decFromStr("48200")
	s := store.Signal{
		SignalType: 2,
		CreatedAt:  time.Date(2026, 6, 12, 14, 31, 0, 0, time.UTC), // exercises the tg-time footer
		Pool:       "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5",
		Metric:     "whale_swap",
		Actor:      "0x8d58a3f1c0b2e7a9d4f6c1b8e2a05d7f3c9b6e10",
		SizeUSD:    &usd,
		Payload:    []byte(`{"size0":18000,"median":1000}`),
		LLMNote:    strings.Repeat("Very long analyst note. ", 100), // ~2400 runes
	}
	v := FormatSignal(s, Context{PoolLabel: "USDe/WMNT", Dex: "Agni"}, "0xtx", testScanBase, testDash)

	visible := visibleLen(v.Caption)
	if visible > 1024 {
		t.Errorf("caption visible length %d exceeds the 1024 budget", visible)
	}
	if !strings.Contains(v.Caption, "…") {
		t.Error("an over-budget note must be clamped with an ellipsis")
	}
	if !strings.Contains(v.Caption, "<code>"+s.Actor+"</code>") {
		t.Error("the copy block must survive note clamping")
	}
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// visibleLen approximates Telegram's caption accounting: entities do not count, so the
// HTML tags are stripped and entity-escapes collapse back to one character.
func visibleLen(html string) int {
	stripped := tagRe.ReplaceAllString(html, "")
	for entity, ch := range map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">"} {
		stripped = strings.ReplaceAll(stripped, entity, ch)
	}
	return len([]rune(stripped))
}

// Pool labels containing HTML metacharacters must arrive escaped in the caption.
func TestCaption_EscapesPoolLabel(t *testing.T) {
	s := store.Signal{SignalType: 1, Pool: "0xpool", Metric: "vol0"}
	v := FormatSignal(s, Context{PoolLabel: "<evil>&pair"}, "", testScanBase, testDash)
	if strings.Contains(v.Caption, "<evil>") {
		t.Errorf("caption leaked an unescaped pool label:\n%s", v.Caption)
	}
	if !strings.Contains(v.Caption, "&lt;evil&gt;&amp;pair") {
		t.Errorf("caption must carry the escaped label:\n%s", v.Caption)
	}
}

// humanMultiple rounds to one decimal under 10x and to an integer above, and reports
// false when there is nothing meaningful to compare against.
func TestHumanMultiple(t *testing.T) {
	cases := []struct {
		latest, median float64
		want           string
		ok             bool
	}{
		{9120, 1000, "9.1×", true},
		{18000, 1000, "18×", true},
		{297000, 1000, "297×", true},
		{-4000, 1000, "4.0×", true}, // signed metric reads as a positive multiple
		{500, 1000, "", false},      // below 1x: nothing notable
		{100, 0, "", false},         // no baseline
	}
	for _, c := range cases {
		got, ok := humanMultiple(c.latest, c.median)
		if got != c.want || ok != c.ok {
			t.Errorf("humanMultiple(%g,%g) = (%q,%v), want (%q,%v)", c.latest, c.median, got, ok, c.want, c.ok)
		}
	}
}

// groupThousands inserts comma separators only past three digits, both signs.
func TestGroupThousands(t *testing.T) {
	cases := map[int64]string{
		0: "0", 7: "7", 100: "100", 1000: "1,000", 48200: "48,200",
		1000000: "1,000,000", -2500: "-2,500",
	}
	for in, want := range cases {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", in, got, want)
		}
	}
}

// shortAddr abbreviates only addresses longer than the head+tail it keeps.
func TestShortAddr(t *testing.T) {
	if got := shortAddr("0x8d58a3f1c0b2e7a9d4f6c1b8e2a05d7f3c9b6e10"); got != "0x8d58…6e10" {
		t.Errorf("shortAddr long = %q", got)
	}
	if got := shortAddr("0xabcd"); got != "0xabcd" {
		t.Errorf("shortAddr short should pass through, got %q", got)
	}
}

// clampRunes keeps short strings intact and cuts long ones at the rune budget with an
// ellipsis (never mid-rune).
func TestClampRunes(t *testing.T) {
	if got := clampRunes("short", 10); got != "short" {
		t.Errorf("clampRunes short = %q", got)
	}
	long := strings.Repeat("×", 20) // multi-byte runes
	got := clampRunes(long, 10)
	if r := []rune(got); len(r) != 10 || r[9] != '…' {
		t.Errorf("clampRunes long = %q (%d runes)", got, len(r))
	}
}

// timeFooter emits the three tg-time entities (relative, long date, short clock) sharing
// the signal's Unix time, with a static fallback for clients that predate the entity.
func TestTimeFooter(t *testing.T) {
	ts := time.Date(2026, 6, 12, 14, 31, 0, 0, time.UTC).Unix()
	got := timeFooter(ts)
	for _, want := range []string{
		fmt.Sprintf(`unix=%d format="r"`, ts),
		fmt.Sprintf(`unix=%d format="D"`, ts),
		fmt.Sprintf(`unix=%d format="t"`, ts),
		"June 12, 2026", // long-date fallback
		"14:31",         // clock fallback
	} {
		if !strings.Contains(got, want) {
			t.Errorf("timeFooter missing %q:\n%s", want, got)
		}
	}
}
