package api

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"ocr/internal/aggregate"
	"ocr/internal/store"
)

// These hermetic tests pin the token-metadata + swap-decode parts of the public
// shape: the triggering-tx-hash extraction from event_ref, the decoded swap action
// (in/out direction from signed net, raw-amount formatting, token_meta fallback),
// the type-1 -> null swap rule, the actor recent-swap mapping, and the pool
// token-meta join. No DB, no HTTP, no network.

// ABI word helpers (signed int256 for the V3/Agni decoder path).

// twosComplementWord renders a (possibly negative) big.Int as a 256-bit
// two's-complement word, exactly how int256 amount0/amount1 are laid out in a
// V3-style Swap log's data, so we can drive aggregate.DecodeSwap from the api test.
func twosComplementWord(n *big.Int) string {
	v := new(big.Int).Set(n)
	if v.Sign() < 0 {
		twoTo256 := new(big.Int).Lsh(big.NewInt(1), 256)
		v.Add(twoTo256, v)
	}
	b := make([]byte, 32)
	v.FillBytes(b)
	return hex.EncodeToString(b)
}

// v3SwapData builds a V3/Agni Swap log's data: two leading int256 words
// (amount0, amount1, signed from the pool's perspective: negative = pool paid the
// token out to the trader). DecodeSwap negates these into Netflow (positive = token
// left pool toward the trader = the actor received it).
func v3SwapData(amount0, amount1 *big.Int) string {
	return "0x" + twosComplementWord(amount0) + twosComplementWord(amount1)
}

func decddd(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

func TestTxHashFromEventRef(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"normal", "0xABCDEF:7", "0xabcdef"},     // lowercased, log index stripped
		{"log-index-zero", "0xdead:0", "0xdead"}, // a 0 log index is valid
		{"empty-flow", "", ""},                   // flow signal: no ref
		{"no-separator", "0xfeed", "0xfeed"},     // defensive: whole string is the hash
		{"whitespace", "  0xBEef:3  ", "0xbeef"}, // trimmed
		{"multi-colon", "0xaa:bb:cc", "0xaa"},    // split on first colon only
	}
	for _, c := range cases {
		if got := txHashFromEventRef(c.ref); got != c.want {
			t.Errorf("%s: txHashFromEventRef(%q) = %q, want %q", c.name, c.ref, got, c.want)
		}
	}
}

// parseEventRef (server.go) must split into (hash, index, ok) and reject anything
// without a valid integer log index.
func TestParseEventRef(t *testing.T) {
	cases := []struct {
		name    string
		ref     string
		wantTx  string
		wantIdx int
		wantOK  bool
	}{
		{"normal", "0xABC:12", "0xabc", 12, true},
		{"zero-index", "0xabc:0", "0xabc", 0, true},
		{"empty", "", "", 0, false},
		{"no-colon", "0xabc", "", 0, false},
		{"no-index", "0xabc:", "", 0, false},
		{"no-hash", ":5", "", 0, false},
		{"bad-index", "0xabc:xy", "", 0, false},
		{"negative-index", "0xabc:-1", "", 0, false},
	}
	for _, c := range cases {
		tx, idx, ok := parseEventRef(c.ref)
		if tx != c.wantTx || idx != c.wantIdx || ok != c.wantOK {
			t.Errorf("%s: parseEventRef(%q) = (%q,%d,%v), want (%q,%d,%v)",
				c.name, c.ref, tx, idx, ok, c.wantTx, c.wantIdx, c.wantOK)
		}
	}
}

// testPool is a small two-token pool: token0 = USDe (18 dec), token1 = WMNT (6 dec).
var testPool = store.Pool{
	Address: "0xpool", Dex: "agni", Label: "USDe/WMNT",
	Token0: "0xtoken0", Token1: "0xtoken1", Dec0: 18, Dec1: 6,
}

// Trader SENT token0 (net0 < 0) and RECEIVED token1 (net1 > 0): token_in = token0,
// token_out = token1; amounts are the absolute nets.
func TestSwapBodyFromNets_Direction(t *testing.T) {
	body, ok := swapBodyFromNets(decddd(-1000), decddd(995), testPool, nil)
	if !ok {
		t.Fatal("expected a determinate direction")
	}
	if body.TokenIn.Address != "0xtoken0" || body.TokenOut.Address != "0xtoken1" {
		t.Fatalf("direction wrong: in=%s out=%s", body.TokenIn.Address, body.TokenOut.Address)
	}
	if body.AmountInRaw != "1000" || body.AmountOut != "995" {
		t.Fatalf("amounts wrong: in=%s out=%s", body.AmountInRaw, body.AmountOut)
	}
	// Decimals come from the pool registry (authoritative), not token_meta.
	if body.TokenIn.Decimals != 18 || body.TokenOut.Decimals != 6 {
		t.Fatalf("decimals wrong: in=%d out=%d", body.TokenIn.Decimals, body.TokenOut.Decimals)
	}
	// No token_meta -> symbol falls back to the pool-label side; logo is null.
	if body.TokenIn.Symbol != "USDe" || body.TokenOut.Symbol != "WMNT" {
		t.Fatalf("fallback symbols wrong: in=%s out=%s", body.TokenIn.Symbol, body.TokenOut.Symbol)
	}
	if body.TokenIn.LogoURL != nil || body.TokenOut.LogoURL != nil {
		t.Fatalf("logos should be nil without token_meta: in=%v out=%v", body.TokenIn.LogoURL, body.TokenOut.LogoURL)
	}
}

// The other direction: trader sent token1, received token0.
func TestSwapBodyFromNets_DirectionReversed(t *testing.T) {
	body, ok := swapBodyFromNets(decddd(500), decddd(-2), testPool, nil)
	if !ok {
		t.Fatal("expected determinate")
	}
	if body.TokenIn.Address != "0xtoken1" || body.TokenOut.Address != "0xtoken0" {
		t.Fatalf("reversed direction wrong: in=%s out=%s", body.TokenIn.Address, body.TokenOut.Address)
	}
	if body.AmountInRaw != "2" || body.AmountOut != "500" {
		t.Fatalf("reversed amounts wrong: in=%s out=%s", body.AmountInRaw, body.AmountOut)
	}
}

// Equal nets (covers both-zero, e.g. a counted-but-undecodable swap) -> ok=false.
func TestSwapBodyFromNets_Indeterminate(t *testing.T) {
	if _, ok := swapBodyFromNets(decimal.Zero, decimal.Zero, testPool, nil); ok {
		t.Fatal("both-zero nets must be indeterminate (ok=false)")
	}
	if _, ok := swapBodyFromNets(decddd(7), decddd(7), testPool, nil); ok {
		t.Fatal("equal nets must be indeterminate (ok=false)")
	}
}

// token_meta PRESENT overrides the fallback symbol and supplies the logo.
func TestSwapBodyFromNets_TokenMetaOverride(t *testing.T) {
	logo := "https://logo/usde.png"
	meta := map[string]store.TokenMeta{
		"0xtoken0": {Address: "0xtoken0", Symbol: "USDe", LogoURL: logo},
		// token1 present but with an EMPTY symbol + empty logo: symbol must fall back
		// to the label side, logo must be null.
		"0xtoken1": {Address: "0xtoken1", Symbol: "", LogoURL: ""},
	}
	body, ok := swapBodyFromNets(decddd(-10), decddd(20), testPool, meta)
	if !ok {
		t.Fatal("determinate expected")
	}
	if body.TokenIn.Symbol != "USDe" || body.TokenIn.LogoURL == nil || *body.TokenIn.LogoURL != logo {
		t.Fatalf("token0 meta not applied: %+v", body.TokenIn)
	}
	if body.TokenOut.Symbol != "WMNT" { // empty meta symbol -> label fallback
		t.Fatalf("token1 empty-symbol should fall back to label, got %q", body.TokenOut.Symbol)
	}
	if body.TokenOut.LogoURL != nil {
		t.Fatalf("token1 empty logo should be nil, got %v", body.TokenOut.LogoURL)
	}
}

// A pool with no usable label and no token_meta -> empty symbols (never panics).
func TestSwapBodyFromNets_NoLabelNoMeta(t *testing.T) {
	p := store.Pool{Address: "0xp", Token0: "0xt0", Token1: "0xt1", Dec0: 18, Dec1: 18, Label: ""}
	body, ok := swapBodyFromNets(decddd(-1), decddd(1), p, nil)
	if !ok {
		t.Fatal("determinate expected")
	}
	if body.TokenIn.Symbol != "" || body.TokenOut.Symbol != "" {
		t.Fatalf("no label/meta should yield empty symbols: in=%q out=%q", body.TokenIn.Symbol, body.TokenOut.Symbol)
	}
}

// A V3 swap where the pool paid token1 out (amount1 < 0) and received token0
// (amount0 > 0): from the trader's view that is token1 received, token0 sent. So
// token_in = token0, token_out = token1.
func TestSwapActionFromLog_V3(t *testing.T) {
	// pool perspective: amount0 = +1000 (pool got token0 from trader),
	//                   amount1 = -995  (pool sent token1 to trader).
	// DecodeSwap negates -> Netflow0 = -1000 (trader sent token0 = token_in),
	//                       Netflow1 = +995  (trader received token1 = token_out).
	l := store.RawLog{
		TxHash: "0xSwapTx", LogIndex: 4,
		Topic0: aggregate.TopicUniV3Swap,
		Data:   v3SwapData(big.NewInt(1000), big.NewInt(-995)),
	}
	logo := "https://logo/wmnt.png"
	meta := map[string]store.TokenMeta{
		"0xtoken1": {Address: "0xtoken1", Symbol: "WMNT", LogoURL: logo},
	}
	act := swapActionFromLog(l, testPool, meta, testExplorer)
	if act == nil {
		t.Fatal("expected a decoded swap action")
	}
	if act.Dex != "agni" {
		t.Fatalf("dex = %q, want agni", act.Dex)
	}
	if act.TokenIn.Address != "0xtoken0" || act.TokenOut.Address != "0xtoken1" {
		t.Fatalf("direction wrong: in=%s out=%s", act.TokenIn.Address, act.TokenOut.Address)
	}
	if act.AmountInRaw != "1000" || act.AmountOut != "995" {
		t.Fatalf("amounts wrong: in=%s out=%s", act.AmountInRaw, act.AmountOut)
	}
	// token_out logo from meta; token_in (no meta) logo null + label-fallback symbol.
	if act.TokenOut.LogoURL == nil || *act.TokenOut.LogoURL != logo {
		t.Fatalf("token_out logo = %v, want %s", act.TokenOut.LogoURL, logo)
	}
	if act.TokenIn.Symbol != "USDe" || act.TokenIn.LogoURL != nil {
		t.Fatalf("token_in fallback wrong: sym=%q logo=%v", act.TokenIn.Symbol, act.TokenIn.LogoURL)
	}
	if act.TxURL == nil || *act.TxURL != "https://mantlescan.xyz/tx/0xswaptx" {
		t.Fatalf("tx_url = %v", act.TxURL)
	}
}

// An undecodable log (short data) -> nil action (the detail endpoint shows swap:null).
func TestSwapActionFromLog_Undecodable(t *testing.T) {
	l := store.RawLog{
		TxHash: "0xtx", LogIndex: 0,
		Topic0: aggregate.TopicUniV3Swap,
		Data:   "0x" + "00", // far too short for two int256 words
	}
	if act := swapActionFromLog(l, testPool, nil, testExplorer); act != nil {
		t.Fatalf("undecodable log should yield nil action, got %+v", act)
	}
}

// A counted-but-balanced swap (both netflows zero, e.g. a zero-amount swap) ->
// indeterminate -> nil action.
func TestSwapActionFromLog_ZeroNets(t *testing.T) {
	l := store.RawLog{
		TxHash: "0xtx", LogIndex: 1,
		Topic0: aggregate.TopicUniV3Swap,
		Data:   v3SwapData(big.NewInt(0), big.NewInt(0)),
	}
	if act := swapActionFromLog(l, testPool, nil, testExplorer); act != nil {
		t.Fatalf("zero-net swap should yield nil action, got %+v", act)
	}
}

// A flow signal carries no event_ref, so its detail DTO serializes swap as JSON
// null (the handler passes nil; here we pin the mapping end of that contract).
func TestSignalDetail_FlowSwapNull(t *testing.T) {
	row := store.SignalRow{Signal: store.Signal{
		ID: 1, CreatedAt: time.Now().UTC(), Pool: "0xpool",
		SignalType: 1, Metric: "vol0", WindowBuckets: 144,
		EventRef: "", // flow: no triggering swap
	}}
	d := signalToDetailDTO(row, mustLabel, testExplorer, nil)
	if d.Swap != nil {
		t.Fatalf("flow detail swap should be nil, got %+v", d.Swap)
	}
	// And tx_hash / tx_url on the embedded signal are null too.
	if d.TxHash != nil || d.TxURL != nil {
		t.Fatalf("flow tx_hash/tx_url should be nil: %v / %v", d.TxHash, d.TxURL)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, present := m["swap"]; !present || v != nil {
		t.Fatalf("swap must be JSON null, got present=%v value=%v", present, v)
	}
}

// A per-event (whale/smart_money) signal's lite DTO exposes tx_hash + tx_url from
// event_ref, plus pool_url; a smart_money signal also gets actor_url.
func TestSignalDTO_ClarityLinks(t *testing.T) {
	// whale (type 2): tx links present, actor_url null.
	whale := store.SignalRow{Signal: store.Signal{
		ID: 2, CreatedAt: time.Now().UTC(), Pool: "0xpool",
		SignalType: 2, Metric: "whale_swap", Zscore: 30,
		EventRef: "0xWhaleTx:9",
	}}
	wd := signalToDTO(whale, mustLabel, testExplorer)
	if wd.PoolURL != "https://mantlescan.xyz/address/0xpool" {
		t.Fatalf("pool_url = %q", wd.PoolURL)
	}
	if wd.TxHash == nil || *wd.TxHash != "0xwhaletx" {
		t.Fatalf("tx_hash = %v", wd.TxHash)
	}
	if wd.TxURL == nil || *wd.TxURL != "https://mantlescan.xyz/tx/0xwhaletx" {
		t.Fatalf("tx_url = %v", wd.TxURL)
	}
	if wd.ActorURL != nil {
		t.Fatalf("whale actor_url should be nil, got %v", wd.ActorURL)
	}

	// smart_money (type 3): actor_url present.
	sm := store.SignalRow{Signal: store.Signal{
		ID: 3, CreatedAt: time.Now().UTC(), Pool: "0xpool",
		SignalType: 3, Metric: "smart_money", Zscore: 77,
		Actor: "0xActor", EventRef: "0xSmTx:2",
	}}
	sd := signalToDTO(sm, mustLabel, testExplorer)
	if sd.ActorURL == nil || *sd.ActorURL != "https://mantlescan.xyz/address/0xActor" {
		t.Fatalf("actor_url = %v", sd.ActorURL)
	}
	if sd.TxHash == nil || *sd.TxHash != "0xsmtx" {
		t.Fatalf("smart-money tx_hash = %v", sd.TxHash)
	}
}

// Links disabled (empty explorer base) -> pool_url "" and all *url fields nil.
func TestSignalDTO_NoExplorerNoLinks(t *testing.T) {
	row := store.SignalRow{Signal: store.Signal{
		ID: 4, CreatedAt: time.Now().UTC(), Pool: "0xpool",
		SignalType: 2, EventRef: "0xtx:1",
	}}
	dto := signalToDTO(row, mustLabel, "")
	if dto.PoolURL != "" {
		t.Fatalf("pool_url should be empty without explorer, got %q", dto.PoolURL)
	}
	// tx_hash is still exposed (it is data, not a link), but tx_url must be nil.
	if dto.TxHash == nil || *dto.TxHash != "0xtx" {
		t.Fatalf("tx_hash should still be set, got %v", dto.TxHash)
	}
	if dto.TxURL != nil || dto.ActorURL != nil {
		t.Fatalf("no-explorer links should be nil: tx=%v actor=%v", dto.TxURL, dto.ActorURL)
	}
}

// actorSwapToDTO decodes a ledger swap (signed net0/net1) into a directional action
// without a raw-log re-read: positive net = received (out), negative = sent (in).
func TestActorSwapToDTO(t *testing.T) {
	ls := store.LargeSwap{
		TxHash: "0xLedgerTx", LogIndex: 3, Pool: "0xpool",
		Actor:     "0xactor",
		Net0:      decddd(-1000), // sent token0
		Net1:      decddd(995),   // received token1
		BlockTime: time.Date(2026, 6, 8, 9, 30, 0, 0, time.UTC),
	}
	logo := "https://logo/usde.png"
	meta := map[string]store.TokenMeta{"0xtoken0": {Address: "0xtoken0", Symbol: "USDe", LogoURL: logo}}

	dto, ok := actorSwapToDTO(ls, testPool, mustLabel, meta, testExplorer)
	if !ok {
		t.Fatal("expected a determinate actor swap")
	}
	if dto.Pool != "0xpool" || dto.Dex != "agni" {
		t.Fatalf("pool/dex wrong: %+v", dto)
	}
	if dto.PoolLabel != "USDC/USDe" { // resolved via the poolLabeler (mustLabel)
		t.Fatalf("pool_label = %q, want USDC/USDe", dto.PoolLabel)
	}
	if dto.Swap.TokenIn.Address != "0xtoken0" || dto.Swap.TokenOut.Address != "0xtoken1" {
		t.Fatalf("direction wrong: in=%s out=%s", dto.Swap.TokenIn.Address, dto.Swap.TokenOut.Address)
	}
	if dto.Swap.AmountInRaw != "1000" || dto.Swap.AmountOut != "995" {
		t.Fatalf("amounts wrong: %+v", dto.Swap)
	}
	if dto.Swap.TokenIn.LogoURL == nil || *dto.Swap.TokenIn.LogoURL != logo {
		t.Fatalf("token_in logo from meta = %v", dto.Swap.TokenIn.LogoURL)
	}
	if dto.BlockTime != "2026-06-08T09:30:00Z" {
		t.Fatalf("block_time = %q", dto.BlockTime)
	}
	if dto.TxURL == nil || *dto.TxURL != "https://mantlescan.xyz/tx/0xledgertx" {
		t.Fatalf("tx_url = %v", dto.TxURL)
	}
}

// An indeterminate ledger swap (equal nets, e.g. a pre-0008 null-net row read as
// 0/0) is reported ok=false so the caller skips it from the activity path.
func TestActorSwapToDTO_IndeterminateSkipped(t *testing.T) {
	ls := store.LargeSwap{
		TxHash: "0xtx", LogIndex: 0, Pool: "0xpool", Actor: "0xactor",
		Net0: decimal.Zero, Net1: decimal.Zero, BlockTime: time.Now().UTC(),
	}
	if _, ok := actorSwapToDTO(ls, testPool, mustLabel, nil, testExplorer); ok {
		t.Fatal("zero-net ledger swap must be skipped (ok=false)")
	}
}

// poolToDTO joins token_meta by token0/token1 address: symbols + logos appear when
// present, and decimals stay the registry's dec0/dec1 (authoritative).
func TestPoolToDTO_TokenMetaJoin(t *testing.T) {
	logo0 := "https://logo/usde.png"
	row := store.PoolRow{
		Pool:     store.Pool{Address: "0xpool", Dex: "agni", Label: "USDe/WMNT", Token0: "0xt0", Token1: "0xt1", Dec0: 18, Dec1: 6, Enabled: true},
		Vol0_24h: decimal.Zero, Vol1_24h: decimal.Zero,
	}
	meta := map[string]store.TokenMeta{
		"0xt0": {Address: "0xt0", Symbol: "USDe", LogoURL: logo0},
		// token1 present with empty logo -> symbol set, logo null.
		"0xt1": {Address: "0xt1", Symbol: "WMNT", LogoURL: ""},
	}
	dto := poolToDTO(row, testExplorer, meta)
	if dto.Token0Symbol != "USDe" || dto.Token1Symbol != "WMNT" {
		t.Fatalf("symbols wrong: t0=%q t1=%q", dto.Token0Symbol, dto.Token1Symbol)
	}
	if dto.Token0LogoURL == nil || *dto.Token0LogoURL != logo0 {
		t.Fatalf("token0 logo = %v", dto.Token0LogoURL)
	}
	if dto.Token1LogoURL != nil {
		t.Fatalf("token1 empty logo should be null, got %v", dto.Token1LogoURL)
	}
	// Authoritative decimals unchanged by token_meta.
	if dto.Dec0 != 18 || dto.Dec1 != 6 {
		t.Fatalf("decimals must stay registry values: %d/%d", dto.Dec0, dto.Dec1)
	}

	// No token_meta at all -> empty symbols + null logos (no panic).
	dto2 := poolToDTO(row, testExplorer, nil)
	if dto2.Token0Symbol != "" || dto2.Token0LogoURL != nil || dto2.Token1LogoURL != nil {
		t.Fatalf("nil token_meta should yield empty/null token display: %+v", dto2)
	}
}
