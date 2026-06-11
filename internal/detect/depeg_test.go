package detect

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"ocr/internal/aggregate"
	"ocr/internal/config"
	"ocr/internal/store"
)

// These tests cover the depeg detector (signal_type 7): the pure implied-price math
// (impliedPrice), the contagion mapping (contagionPools), and the firing path
// (scanOnce) through a mock store, fully hermetic (no DB, no RPC, no external price).
// Every input is our own decoded swap amounts plus the verified stable set.

// Verified stablecoin addresses (lowercase) reused by the firing tests.
// These must match the verified registry set in size.go / config/pools.yaml.
const (
	dpUSDe = "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34" // 18 dec
	dpUSDC = "0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9" // 6 dec
	dpUSDT = "0x201eba5cc46d216ce6dc03f6a759e8e766e956ae" // 6 dec
	dpWMNT = "0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8" // 18 dec, non-stable (gas token)
)

// Swap-log shaping (V3-style: two leading int256 words amount0, amount1).

// dpSignedWord renders a signed big.Int as a 256-bit two's-complement word (matches
// aggregate's decodeInt256 layout).
func dpSignedWord(n *big.Int) string {
	v := new(big.Int).Set(n)
	if v.Sign() < 0 {
		twoTo256 := new(big.Int).Lsh(big.NewInt(1), 256)
		v.Add(twoTo256, v)
	}
	b := make([]byte, 32)
	v.FillBytes(b)
	return strings.ToLower(hexEncode(b))
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0x0f]
	}
	return string(out)
}

// dpSwapLog builds a raw_logs row for a V3-style swap with the given signed raw
// amount0/amount1 (pool perspective; sign is irrelevant to the implied price, which
// uses the absolute amounts). pool is the contract address the swap hit.
func dpSwapLog(pool string, amount0, amount1 *big.Int, logIndex int) store.RawLog {
	data := "0x" + dpSignedWord(amount0) + dpSignedWord(amount1)
	return store.RawLog{
		ChainID:   5000,
		TxHash:    "0x" + strings.Repeat("0", 63) + itoaHex(logIndex+1),
		LogIndex:  logIndex,
		Address:   strings.ToLower(pool),
		Topic0:    aggregate.TopicUniV3Swap,
		Data:      data,
		BlockTime: time.Now().UTC().Add(-2 * time.Minute), // inside the recent window
	}
}

func itoaHex(n int) string {
	if n == 0 {
		return "0"
	}
	const hexdigits = "0123456789abcdef"
	var b []byte
	for n > 0 {
		b = append([]byte{hexdigits[n%16]}, b...)
		n /= 16
	}
	return string(b)
}

// dpRawUnits returns whole * 10^dec as a big.Int (e.g. 100 USDe at 18 dec).
func dpRawUnits(whole int64, dec int) *big.Int {
	n := big.NewInt(whole)
	return n.Mul(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil))
}

// Pool registry helpers.

// dpPool builds a stable/stable pool registry entry.
func dpPool(addr, token0 string, dec0 int, token1 string, dec1 int, label string) store.Pool {
	return store.Pool{
		Address: addr, Token0: token0, Dec0: dec0, Token1: token1, Dec1: dec1,
		Label: label, Enabled: true,
	}
}

// dpCfg is a config with the shipped depeg defaults for the firing tests.
func dpCfg() *config.Config {
	return &config.Config{
		BucketMin:         5,
		DepegThresholdBps: 50,
		DepegMinSwaps:     5,
		DepegCooldownMin:  60,
	}
}

// Mock store.

// mockDepegStore is a hermetic depegStore. pools is the enabled-pool set; logsByPool
// maps a lowercase pool address to its recent swap logs; lastSignal drives the
// per-token cooldown (token -> last fire time); inserted is what InsertSignal reports
// for a fresh insert; the *Err fields drive the abort paths; got captures inserts.
type mockDepegStore struct {
	pools      []store.Pool
	logsByPool map[string][]store.RawLog
	lastSignal map[string]time.Time
	inserted   bool
	poolsErr   error
	logsErr    error
	insertErr  error
	got        []store.Signal
}

func (m *mockDepegStore) ListEnabledPools(_ context.Context) ([]store.Pool, error) {
	return m.pools, m.poolsErr
}

func (m *mockDepegStore) RawLogsSince(_ context.Context, _ time.Time, pools []string) ([]store.RawLog, error) {
	if m.logsErr != nil {
		return nil, m.logsErr
	}
	if len(pools) == 0 {
		return nil, nil
	}
	return m.logsByPool[strings.ToLower(pools[0])], nil
}

func (m *mockDepegStore) InsertSignal(_ context.Context, s store.Signal) (int64, bool, error) {
	if m.insertErr != nil {
		return 0, false, m.insertErr
	}
	m.got = append(m.got, s)
	if m.inserted {
		return int64(len(m.got)), true, nil
	}
	return 0, false, nil
}

func (m *mockDepegStore) LastSignalTimeByType(_ context.Context, pool string, _ int16) (time.Time, bool, error) {
	t, ok := m.lastSignal[strings.ToLower(pool)]
	return t, ok, nil
}

// impliedPrice math

func TestImpliedPrice_AtPeg(t *testing.T) {
	// 100 USDe (18 dec) traded for 100 USDC (6 dec): a0_whole = a1_whole = 100 -> 1.0.
	g0 := decBigInt(dpRawUnits(100, 18)) // token0 = USDe
	g1 := decBigInt(dpRawUnits(100, 6))  // token1 = USDC
	px, ok := impliedPrice(g0, g1, 18, 6)
	if !ok {
		t.Fatal("expected a usable implied price at peg")
	}
	if d := absf(px - 1.0); d > 1e-9 {
		t.Fatalf("at-peg implied price = %v, want ~1.0", px)
	}
}

func TestImpliedPrice_OnePercentDepeg(t *testing.T) {
	// token0 trades below $1: 100 token0 buys only 99 token1 -> a1/a0 = 0.99 (< 1 =>
	// token0 is the cheaper/under-peg side). Decimals-correct for 18-vs-6.
	g0 := decBigInt(dpRawUnits(100, 18))
	g1 := decBigInt(dpRawUnits(99, 6))
	px, ok := impliedPrice(g0, g1, 18, 6)
	if !ok {
		t.Fatal("expected a usable implied price")
	}
	if d := absf(px - 0.99); d > 1e-9 {
		t.Fatalf("1%% depeg implied price = %v, want ~0.99", px)
	}
}

func TestImpliedPrice_DecimalsMatterForAsymmetricPair(t *testing.T) {
	// Same whole amounts (100 each) but different decimals: the raw integers differ by
	// 10^12, yet after the 10^dec scaling the price is 1.0. This proves the decimals
	// handling: a naive raw-unit ratio would be 10^12 (or 10^-12), not 1.0.
	g0 := decBigInt(dpRawUnits(100, 18)) // 1e20 raw
	g1 := decBigInt(dpRawUnits(100, 6))  // 1e8  raw
	px, ok := impliedPrice(g0, g1, 18, 6)
	if !ok {
		t.Fatal("expected a usable implied price")
	}
	if d := absf(px - 1.0); d > 1e-9 {
		t.Fatalf("asymmetric-decimals price = %v, want ~1.0 (decimals not applied?)", px)
	}
	// Sanity: the same raw amounts without the decimals correction would be wildly off.
	rawRatio := g1.Div(g0).InexactFloat64() // 1e8 / 1e20 = 1e-12
	if absf(rawRatio-1.0) < 1e-3 {
		t.Fatalf("raw ratio unexpectedly ~1.0 (%v); the decimals test is not meaningful", rawRatio)
	}
}

func TestImpliedPrice_ZeroAmountSkips(t *testing.T) {
	// A one-sided / zero-amount swap has no usable ratio -> ok=false (no divide-by-zero).
	if _, ok := impliedPrice(decimal.Zero, decBigInt(dpRawUnits(100, 6)), 18, 6); ok {
		t.Error("zero token0 amount should yield ok=false")
	}
	if _, ok := impliedPrice(decBigInt(dpRawUnits(100, 18)), decimal.Zero, 18, 6); ok {
		t.Error("zero token1 amount should yield ok=false")
	}
	if _, ok := impliedPrice(decimal.Zero, decimal.Zero, 18, 6); ok {
		t.Error("both-zero swap should yield ok=false")
	}
}

// Contagion mapping.

func TestContagionPools(t *testing.T) {
	pools := []store.Pool{
		dpPool("0xpool_usdc_usde", dpUSDC, 6, dpUSDe, 18, "USDC/USDe"),  // holds USDe
		dpPool("0xpool_usdt_usde", dpUSDT, 6, dpUSDe, 18, "USDT/USDe"),  // holds USDe
		dpPool("0xpool_usde_wmnt", dpUSDe, 18, dpWMNT, 18, "USDe/WMNT"), // holds USDe (the source)
		dpPool("0xpool_wmnt_usdt", dpWMNT, 18, dpUSDT, 6, "WMNT/USDT"),  // does not hold USDe
	}
	// USDe depegs, measured in the USDe/WMNT pool -> contagion = the OTHER pools holding
	// USDe (the two stable/stable ones), excluding the source pool and the WMNT/USDT one.
	got := contagionPools(dpUSDe, "0xpool_usde_wmnt", pools)
	if len(got) != 2 {
		t.Fatalf("want 2 at-risk pools, got %d: %+v", len(got), got)
	}
	gotAddrs := map[string]string{}
	for _, ap := range got {
		gotAddrs[ap.Address] = ap.Label
	}
	if gotAddrs["0xpool_usdc_usde"] != "USDC/USDe" || gotAddrs["0xpool_usdt_usde"] != "USDT/USDe" {
		t.Errorf("contagion set mismatch: %+v", gotAddrs)
	}
	// The source pool must be excluded.
	if _, ok := gotAddrs["0xpool_usde_wmnt"]; ok {
		t.Error("the source pool must not appear in its own contagion set")
	}
	// A pool not holding the token must be excluded.
	if _, ok := gotAddrs["0xpool_wmnt_usdt"]; ok {
		t.Error("a pool not holding the depegging token must not be at-risk")
	}
}

// Firing: depeg emits with correct subject + payload.

// A USDe/USDC pool where USDe (token0) trades at 0.97 fires a type-7 signal: subject =
// USDe (the cheaper side), metric "depeg", deviation ~300 bps, under_peg=true, and the
// contagion map names the OTHER pool holding USDe. Bucket path (no event_ref, null
// actor, null size).
func TestDepegScanOnce_FiresUnderPegToken0(t *testing.T) {
	source := "0xaaa0000000000000000000000000000000000001" // USDe/USDC (token0=USDe)
	other := "0xbbb0000000000000000000000000000000000002"  // USDT/USDe (holds USDe)
	pools := []store.Pool{
		dpPool(source, dpUSDe, 18, dpUSDC, 6, "USDe/USDC"),
		dpPool(other, dpUSDT, 6, dpUSDe, 18, "USDT/USDe"),
	}
	// 6 swaps at 100 USDe -> 97 USDC each (a1/a0 = 0.97 -> 300 bps off, token0 cheap).
	var logs []store.RawLog
	for i := 0; i < 6; i++ {
		logs = append(logs, dpSwapLog(source,
			dpRawUnits(100, 18), dpRawUnits(97, 6), i))
	}
	st := &mockDepegStore{
		pools:      pools,
		logsByPool: map[string][]store.RawLog{source: logs},
		inserted:   true,
	}

	out, err := NewDepegDetector(nil, dpCfg()).scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 depeg signal, got %d", len(out))
	}
	s := out[0]
	if s.SignalType != signalTypeDepeg {
		t.Errorf("signal_type = %d, want %d", s.SignalType, signalTypeDepeg)
	}
	if s.Metric != metricDepeg {
		t.Errorf("metric = %q, want %q", s.Metric, metricDepeg)
	}
	// Subject (Pool field) = the depegging token = USDe (token0, the cheaper side).
	if s.Pool != dpUSDe {
		t.Errorf("subject = %q, want the depegging token USDe %q", s.Pool, dpUSDe)
	}
	// Bucket path: no event_ref (-> SignalHash), null actor, null size.
	if s.EventRef != "" {
		t.Errorf("event_ref = %q, want empty (bucket path)", s.EventRef)
	}
	if s.Actor != "" {
		t.Errorf("actor = %q, want empty (market condition)", s.Actor)
	}
	if s.SizeUSD != nil {
		t.Errorf("size_usd = %v, want nil (price condition)", s.SizeUSD)
	}
	if s.BucketTS.IsZero() {
		t.Error("bucket_ts must be set (bucket-keyed idempotency)")
	}
	if s.Zscore <= 0 {
		t.Errorf("score input should be positive, got %g", s.Zscore)
	}

	// Payload: under_peg true, deviation ~300 bps, contagion names the other USDe pool.
	var p depegPayload
	if err := json.Unmarshal(s.Payload, &p); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if !p.UnderPeg {
		t.Error("under_peg should be true")
	}
	if p.DeviationBps < 290 || p.DeviationBps > 310 {
		t.Errorf("deviation_bps = %d, want ~300", p.DeviationBps)
	}
	if p.Token != dpUSDe {
		t.Errorf("payload token = %q, want USDe", p.Token)
	}
	if p.TokenSymbol != "USDe" {
		t.Errorf("payload token_symbol = %q, want USDe", p.TokenSymbol)
	}
	if len(p.AffectedPools) != 1 || p.AffectedPools[0].Address != other {
		t.Errorf("contagion = %+v, want the single other USDe pool %s", p.AffectedPools, other)
	}
}

// When token1 is the cheaper side (implied price > 1), the depegging token is token1.
func TestDepegScanOnce_FiresUnderPegToken1(t *testing.T) {
	source := "0xccc0000000000000000000000000000000000003" // USDC/USDe (token1=USDe)
	pools := []store.Pool{dpPool(source, dpUSDC, 6, dpUSDe, 18, "USDC/USDe")}
	// token1 (USDe) trades below $1: 100 USDC buys 103 USDe -> a1/a0 = 1.03 (> 1 =>
	// token1 cheaper). deviation = 300 bps.
	var logs []store.RawLog
	for i := 0; i < 5; i++ {
		logs = append(logs, dpSwapLog(source,
			dpRawUnits(100, 6), dpRawUnits(103, 18), i))
	}
	st := &mockDepegStore{
		pools:      pools,
		logsByPool: map[string][]store.RawLog{source: logs},
		inserted:   true,
	}
	out, err := NewDepegDetector(nil, dpCfg()).scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 depeg signal, got %d", len(out))
	}
	if out[0].Pool != dpUSDe {
		t.Errorf("subject = %q, want token1 USDe %q (the cheaper side)", out[0].Pool, dpUSDe)
	}
}

// At peg (within threshold) no depeg fires.
func TestDepegScanOnce_OnPegNoFire(t *testing.T) {
	source := "0xddd0000000000000000000000000000000000004"
	pools := []store.Pool{dpPool(source, dpUSDe, 18, dpUSDC, 6, "USDe/USDC")}
	var logs []store.RawLog
	for i := 0; i < 6; i++ {
		logs = append(logs, dpSwapLog(source, dpRawUnits(100, 18), dpRawUnits(100, 6), i)) // 1.0
	}
	st := &mockDepegStore{pools: pools, logsByPool: map[string][]store.RawLog{source: logs}, inserted: true}
	out, err := NewDepegDetector(nil, dpCfg()).scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("an on-peg pool must not fire, got %d", len(out))
	}
}

// A drift just UNDER the threshold (40 bps < 50) does not fire; one just OVER does.
func TestDepegScanOnce_ThresholdBoundary(t *testing.T) {
	source := "0xeee0000000000000000000000000000000000005"
	pools := []store.Pool{dpPool(source, dpUSDe, 18, dpUSDC, 6, "USDe/USDC")}

	// 40 bps: 1000 USDe -> 996 USDC (a1/a0 = 0.996 -> 40 bps). Below 50 -> no fire.
	var under []store.RawLog
	for i := 0; i < 6; i++ {
		under = append(under, dpSwapLog(source, dpRawUnits(1000, 18), dpRawUnits(996, 6), i))
	}
	stUnder := &mockDepegStore{pools: pools, logsByPool: map[string][]store.RawLog{source: under}, inserted: true}
	if out, err := NewDepegDetector(nil, dpCfg()).scanOnce(context.Background(), stUnder); err != nil || len(out) != 0 {
		t.Fatalf("40bps drift: want 0 signals (err=%v), got %d", err, len(out))
	}

	// 60 bps: 1000 USDe -> 994 USDC (a1/a0 = 0.994 -> 60 bps). Above 50 -> fires.
	var over []store.RawLog
	for i := 0; i < 6; i++ {
		over = append(over, dpSwapLog(source, dpRawUnits(1000, 18), dpRawUnits(994, 6), i))
	}
	stOver := &mockDepegStore{pools: pools, logsByPool: map[string][]store.RawLog{source: over}, inserted: true}
	if out, err := NewDepegDetector(nil, dpCfg()).scanOnce(context.Background(), stOver); err != nil || len(out) != 1 {
		t.Fatalf("60bps drift: want 1 signal (err=%v), got %d", err, len(out))
	}
}

// Thin data (fewer than DepegMinSwaps usable swaps) skips the pool even when the few
// swaps are wildly off peg.
func TestDepegScanOnce_ThinDataSkips(t *testing.T) {
	source := "0xfff0000000000000000000000000000000000006"
	pools := []store.Pool{dpPool(source, dpUSDe, 18, dpUSDC, 6, "USDe/USDC")}
	// Only 4 swaps (< default 5), each a huge depeg.
	var logs []store.RawLog
	for i := 0; i < 4; i++ {
		logs = append(logs, dpSwapLog(source, dpRawUnits(100, 18), dpRawUnits(80, 6), i)) // 0.80
	}
	st := &mockDepegStore{pools: pools, logsByPool: map[string][]store.RawLog{source: logs}, inserted: true}
	out, err := NewDepegDetector(nil, dpCfg()).scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("thin data should skip, got %d signals", len(out))
	}
	// InsertSignal must not have been attempted (we skipped before insert).
	if len(st.got) != 0 {
		t.Fatalf("thin-data pool should not attempt an insert, got %d", len(st.got))
	}
}

// A pool with a non-stable side (USDe/WMNT) is never evaluated for a depeg, even if its
// implied price is far from 1.0 (WMNT is not a dollar stable, so there is no peg).
func TestDepegScanOnce_NonStablePoolSkipped(t *testing.T) {
	source := "0x1110000000000000000000000000000000000007"
	pools := []store.Pool{dpPool(source, dpUSDe, 18, dpWMNT, 18, "USDe/WMNT")}
	var logs []store.RawLog
	for i := 0; i < 10; i++ {
		// USDe<->WMNT is not ~1.0 anyway, but the point is it's never even read.
		logs = append(logs, dpSwapLog(source, dpRawUnits(100, 18), dpRawUnits(50, 18), i))
	}
	st := &mockDepegStore{pools: pools, logsByPool: map[string][]store.RawLog{source: logs}, inserted: true}
	out, err := NewDepegDetector(nil, dpCfg()).scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a non-stable pool must not produce a depeg, got %d", len(out))
	}
	if len(st.got) != 0 {
		t.Fatalf("a non-stable pool must not attempt an insert, got %d", len(st.got))
	}
}

// A token in cooldown (a recent type-7 fire) is suppressed even when off peg.
func TestDepegScanOnce_CooldownSuppresses(t *testing.T) {
	source := "0x2220000000000000000000000000000000000008"
	pools := []store.Pool{dpPool(source, dpUSDe, 18, dpUSDC, 6, "USDe/USDC")}
	var logs []store.RawLog
	for i := 0; i < 6; i++ {
		logs = append(logs, dpSwapLog(source, dpRawUnits(100, 18), dpRawUnits(97, 6), i)) // 300 bps off
	}
	st := &mockDepegStore{
		pools:      pools,
		logsByPool: map[string][]store.RawLog{source: logs},
		inserted:   true,
		// USDe fired 10 min ago; default cooldown is 60 min -> suppressed.
		lastSignal: map[string]time.Time{dpUSDe: time.Now().UTC().Add(-10 * time.Minute)},
	}
	out, err := NewDepegDetector(nil, dpCfg()).scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a token in cooldown must be suppressed, got %d", len(out))
	}
	if len(st.got) != 0 {
		t.Fatalf("cooldown should suppress before the insert, got %d", len(st.got))
	}

	// With cooldown disabled (0), the same depeg fires.
	cfg := dpCfg()
	cfg.DepegCooldownMin = 0
	st2 := &mockDepegStore{
		pools:      pools,
		logsByPool: map[string][]store.RawLog{source: logs},
		inserted:   true,
		lastSignal: map[string]time.Time{dpUSDe: time.Now().UTC().Add(-10 * time.Minute)},
	}
	out2, err := NewDepegDetector(nil, cfg).scanOnce(context.Background(), st2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out2) != 1 {
		t.Fatalf("with cooldown disabled the depeg should fire, got %d", len(out2))
	}
}

// When InsertSignal reports the row already exists (inserted=false), the signal is not
// returned (so it is not re-attested): the (pool=token, signal_type, metric, bucket_ts)
// idempotency that overlapping windows across ticks rely on.
func TestDepegScanOnce_IdempotentSkip(t *testing.T) {
	source := "0x3330000000000000000000000000000000000009"
	pools := []store.Pool{dpPool(source, dpUSDe, 18, dpUSDC, 6, "USDe/USDC")}
	var logs []store.RawLog
	for i := 0; i < 6; i++ {
		logs = append(logs, dpSwapLog(source, dpRawUnits(100, 18), dpRawUnits(97, 6), i))
	}
	st := &mockDepegStore{
		pools:      pools,
		logsByPool: map[string][]store.RawLog{source: logs},
		inserted:   false, // already recorded
	}
	out, err := NewDepegDetector(nil, dpCfg()).scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("an already-recorded depeg must not be re-emitted, got %d", len(out))
	}
	// The insert was still attempted exactly once (the dedup happens at the DB).
	if len(st.got) != 1 {
		t.Fatalf("InsertSignal should have been attempted once, got %d", len(st.got))
	}
}

// A RawLogsSince error aborts the scan with a wrapped error (not a partial result).
func TestDepegScanOnce_RawLogsErrorAborts(t *testing.T) {
	source := "0x4440000000000000000000000000000000000010"
	st := &mockDepegStore{
		pools:   []store.Pool{dpPool(source, dpUSDe, 18, dpUSDC, 6, "USDe/USDC")},
		logsErr: errors.New("db down"),
	}
	if _, err := NewDepegDetector(nil, dpCfg()).scanOnce(context.Background(), st); err == nil {
		t.Fatal("a RawLogsSince failure should abort the scan")
	}
}

// scoreDepeg rises with the deviation and stays within the on-chain band.
func TestScoreDepeg(t *testing.T) {
	if s := scoreDepeg(0); s != depegScoreBase {
		t.Errorf("0 bps score = %g, want base %g", s, depegScoreBase)
	}
	s50 := scoreDepeg(50)
	s1000 := scoreDepeg(1000)
	if s50 <= depegScoreBase {
		t.Errorf("50 bps score = %g, want > base", s50)
	}
	if s1000 <= s50 {
		t.Errorf("score should rise with deviation: 1000bps (%g) should beat 50bps (%g)", s1000, s50)
	}
	// Always within [base, cap] so ScoreFromZ never overflows uint16.
	if s := scoreDepeg(1e9); s < depegScoreBase || s > depegScoreCapZ {
		t.Errorf("huge deviation score = %g, want within [%g,%g]", s, depegScoreBase, depegScoreCapZ)
	}
}

// Small local helpers.

// decBigInt builds a decimal from a big.Int (the raw on-chain amount).
func decBigInt(n *big.Int) decimal.Decimal { return decimal.NewFromBigInt(n, 0) }

// absf is float64 abs (test-only; avoids importing math for one call).
func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
