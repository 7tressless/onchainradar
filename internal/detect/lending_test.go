package detect

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"

	"ocr/internal/config"
	"ocr/internal/lending"
	"ocr/internal/store"
)

// These tests cover the lending detector's pure gates (passesLiquidationFloor,
// passesBorrowFloor, scoreLendingUSD) and its firing path (scanOnce) through a mock
// store and mock resolver, fully hermetic (no DB, no RPC).

// Verified addresses (lowercase) reused by the firing tests.
const (
	lAaveUSDT0  = "0x779ded0c9e1022225f8e0630b35a9b54be713736" // 6 dec, stable
	lAaveUSDe   = "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34" // 18 dec, stable
	lAaveFBTC   = "0xc96de26018a54d51c097160568752c4e3bd6c364" // 8 dec, non-stable
	lUser       = "0x1111111111111111111111111111111111111111"
	lLiquidator = "0x2222222222222222222222222222222222222222"
	lBorrower   = "0x3333333333333333333333333333333333333333"
	lOrigin     = "0x4444444444444444444444444444444444444444"
)

// Log-shaping helpers (build store.RawLog).

func lWord(hexNo0x string) string { return fmt.Sprintf("%064s", hexNo0x) }

func lAddrTopic(addr string) string {
	return "0x" + lWord(strings.ToLower(strings.TrimPrefix(addr, "0x")))
}

func lUintWord(decStr string) string {
	n, ok := new(big.Int).SetString(decStr, 10)
	if !ok {
		panic("lUintWord: bad decimal " + decStr)
	}
	return lWord(fmt.Sprintf("%x", n))
}

func lAddrWord(addr string) string {
	return lWord(strings.ToLower(strings.TrimPrefix(addr, "0x")))
}

func lBoolWord(b bool) string {
	if b {
		return lWord("1")
	}
	return lWord("0")
}

// liqLog builds a raw_logs row for an Aave LiquidationCall: collateral=FBTC,
// debt=debtAsset, user=lUser; data = debtToCover, collateralAmount, liquidator, bool.
func liqLog(tx string, logIndex int, debtAsset, debtRaw string) store.RawLog {
	data := "0x" + strings.Join([]string{
		lUintWord(debtRaw),     // debtToCover
		lUintWord("1"),         // liquidatedCollateralAmount (irrelevant here)
		lAddrWord(lLiquidator), // liquidator
		lBoolWord(false),       // receiveAToken
	}, "")
	return store.RawLog{
		ChainID:   5000,
		TxHash:    tx,
		LogIndex:  logIndex,
		Address:   lending.AavePoolAddress,
		Topic0:    lending.TopicLiquidationCall,
		Topic1:    lAddrTopic(lAaveFBTC), // collateralAsset
		Topic2:    lAddrTopic(debtAsset), // debtAsset
		Topic3:    lAddrTopic(lUser),     // user
		Data:      data,
		BlockTime: time.Now().UTC().Add(-2 * time.Minute), // inside the recent window
	}
}

// borrowLog builds a raw_logs row for an Aave Borrow: reserve=reserve,
// onBehalfOf=lBorrower, referral=0; data = user, amount, rateMode, borrowRate.
func borrowLog(tx string, logIndex int, reserve, amountRaw string) store.RawLog {
	data := "0x" + strings.Join([]string{
		lAddrWord(lBorrower), // user (not indexed; first data word)
		lUintWord(amountRaw), // amount
		lUintWord("2"),       // interestRateMode = variable
		lUintWord("0"),       // borrowRate
	}, "")
	return store.RawLog{
		ChainID:   5000,
		TxHash:    tx,
		LogIndex:  logIndex,
		Address:   lending.AavePoolAddress,
		Topic0:    lending.TopicBorrow,
		Topic1:    lAddrTopic(reserve),   // reserve
		Topic2:    lAddrTopic(lBorrower), // onBehalfOf
		Topic3:    "0x" + lWord("0"),     // referralCode = 0
		Data:      data,
		BlockTime: time.Now().UTC().Add(-2 * time.Minute),
	}
}

// Mock store + resolver.

// mockLendingStore is a hermetic lendingStore: it returns canned logs and records
// every inserted signal, with a configurable "already recorded" (inserted=false)
// reply to exercise idempotency. logsErr / insertErr drive the abort paths.
type mockLendingStore struct {
	logs      []store.RawLog
	logsErr   error
	inserted  bool // value InsertSignal reports for "freshly inserted"
	insertErr error
	got       []store.Signal // captured inserts
}

func (m *mockLendingStore) RawLogsSince(_ context.Context, _ time.Time, _ []string) ([]store.RawLog, error) {
	return m.logs, m.logsErr
}

func (m *mockLendingStore) InsertSignal(_ context.Context, s store.Signal) (int64, bool, error) {
	if m.insertErr != nil {
		return 0, false, m.insertErr
	}
	m.got = append(m.got, s)
	if m.inserted {
		return int64(len(m.got)), true, nil
	}
	return 0, false, nil
}

// lendCfg is a config with the shipped lending defaults for the firing tests.
func lendCfg() *config.Config {
	return &config.Config{
		BigBorrowMinUSD:   50_000,
		LiquidationMinUSD: 0,
	}
}

// Floor predicates.

func TestPassesLiquidationFloor(t *testing.T) {
	usd := func(s string) *decimal.Decimal { d := dec(s); return &d }
	cases := []struct {
		name  string
		size  *decimal.Decimal
		floor float64
		want  bool
	}{
		{"zero-floor-any-fires", usd("5"), 0, true},
		{"zero-floor-nil-size-fires", nil, 0, true}, // non-stable debt still fires under 0 floor
		{"positive-floor-above", usd("60000"), 50_000, true},
		{"positive-floor-below", usd("40000"), 50_000, false},
		{"positive-floor-nil-size-gated", nil, 50_000, false}, // can't confirm -> excluded
		{"positive-floor-exact", usd("50000"), 50_000, true},
	}
	for _, c := range cases {
		if got := passesLiquidationFloor(c.size, c.floor); got != c.want {
			t.Errorf("%s: passesLiquidationFloor = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPassesBorrowFloor(t *testing.T) {
	usd := func(s string) *decimal.Decimal { d := dec(s); return &d }
	cases := []struct {
		name  string
		size  *decimal.Decimal
		floor float64
		want  bool
	}{
		{"nil-size-never-fires", nil, 50_000, false}, // non-stable reserve: can't size -> never a "big borrow"
		{"nil-size-zero-floor-still-false", nil, 0, false},
		{"above-floor", usd("75000"), 50_000, true},
		{"below-floor", usd("10000"), 50_000, false},
		{"exact-floor", usd("50000"), 50_000, true},
	}
	for _, c := range cases {
		if got := passesBorrowFloor(c.size, c.floor); got != c.want {
			t.Errorf("%s: passesBorrowFloor = %v, want %v", c.name, got, c.want)
		}
	}
}

// Score ramp.

func TestScoreLendingUSD(t *testing.T) {
	// nil / degenerate -> base.
	if s := scoreLendingUSD(nil); s != lendingBaseScore {
		t.Errorf("nil size score = %g, want base %g", s, lendingBaseScore)
	}
	tiny := dec("1")
	if s := scoreLendingUSD(&tiny); s != lendingBaseScore {
		t.Errorf("$1 score = %g, want base %g", s, lendingBaseScore)
	}
	// $50k -> base + log10(50000) ~= 3 + 4.699 = 7.699.
	big := dec("50000")
	got := scoreLendingUSD(&big)
	if got <= lendingBaseScore {
		t.Errorf("$50k score = %g, want > base", got)
	}
	// Strictly increasing in size: a bigger borrow scores higher.
	bigger := dec("5000000")
	if scoreLendingUSD(&bigger) <= got {
		t.Errorf("score should rise with size: $5M (%g) should beat $50k (%g)", scoreLendingUSD(&bigger), got)
	}
	// Always clamps within [base, cap] (so ScoreFromZ never overflows uint16).
	huge := dec("1000000000000000000000000")
	if s := scoreLendingUSD(&huge); s < lendingBaseScore || s > lendingScoreCapZ {
		t.Errorf("huge size score = %g, want within [%g,%g]", s, lendingBaseScore, lendingScoreCapZ)
	}
}

// Firing: liquidation.

// A stable-debt liquidation emits a type-5 signal whose subject is the liquidated
// user, actor is the liquidator's tx.origin (resolved), metric is "liquidation",
// event_ref is "tx:logindex", and size_usd is the debt sized from the stable side.
func TestLendingScanOnce_LiquidationEmits(t *testing.T) {
	st := &mockLendingStore{
		logs:     []store.RawLog{liqLog("0xaa", 7, lAaveUSDT0, "50000000000")}, // 50,000 USDT0 (6 dec)
		inserted: true,
	}
	// Resolver maps the tx to a controlling EOA (refines the actor beyond the bare
	// liquidator field).
	res := newMockResolver()
	res.byTx[common.HexToHash("0xaa")] = common.HexToAddress(lOrigin)

	det := &LendingDetector{cfg: lendCfg(), res: res}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 liquidation signal, got %d", len(out))
	}
	s := out[0]
	if s.SignalType != signalTypeLiquidation {
		t.Errorf("signal_type = %d, want %d", s.SignalType, signalTypeLiquidation)
	}
	if s.Metric != metricLiquidation {
		t.Errorf("metric = %q, want %q", s.Metric, metricLiquidation)
	}
	// Subject (Pool field) = the liquidated borrower.
	if s.Pool != lUser {
		t.Errorf("subject = %q, want the liquidated user %q", s.Pool, lUser)
	}
	// Actor = the resolved tx.origin (refined from the liquidator).
	if s.Actor != lOrigin {
		t.Errorf("actor = %q, want resolved origin %q", s.Actor, lOrigin)
	}
	if s.EventRef != "0xaa:7" {
		t.Errorf("event_ref = %q, want 0xaa:7", s.EventRef)
	}
	// size_usd = 50,000 USDT0 / 10^6 = 50000.
	if s.SizeUSD == nil || !s.SizeUSD.Equal(dec("50000")) {
		t.Errorf("size_usd = %v, want 50000", s.SizeUSD)
	}
	if s.Zscore <= 0 {
		t.Errorf("score input should be positive, got %g", s.Zscore)
	}
}

// With a nil resolver, the liquidation still fires and the actor falls back to the
// event's liquidator address (attribution is best-effort, never blocking).
func TestLendingScanOnce_LiquidationNilResolverFallsBack(t *testing.T) {
	st := &mockLendingStore{
		logs:     []store.RawLog{liqLog("0xbb", 0, lAaveUSDe, "1500000000000000000000")}, // 1500 USDe (18 dec)
		inserted: true,
	}
	det := &LendingDetector{cfg: lendCfg(), res: nil} // no resolver
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 liquidation, got %d", len(out))
	}
	if out[0].Actor != lLiquidator {
		t.Errorf("actor = %q, want the event liquidator %q (nil-resolver fallback)", out[0].Actor, lLiquidator)
	}
}

// A liquidation with a non-stable debt asset (FBTC) still fires under the default
// 0 floor (every liquidation is a recordable smart-money event), but its size_usd
// is nil (we cannot size a non-stable asset in USD).
func TestLendingScanOnce_LiquidationNonStableNilSize(t *testing.T) {
	st := &mockLendingStore{
		logs:     []store.RawLog{liqLog("0xcc", 1, lAaveFBTC, "150000000")}, // 1.5 FBTC debt
		inserted: true,
	}
	det := &LendingDetector{cfg: lendCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("non-stable liquidation should still fire under 0 floor, got %d", len(out))
	}
	if out[0].SizeUSD != nil {
		t.Errorf("non-stable debt should yield nil size_usd, got %s", out[0].SizeUSD)
	}
}

// A positive liquidation floor excludes a non-stable-debt liquidation (size unknown).
func TestLendingScanOnce_LiquidationPositiveFloorGatesNonStable(t *testing.T) {
	st := &mockLendingStore{
		logs:     []store.RawLog{liqLog("0xdd", 2, lAaveFBTC, "150000000")},
		inserted: true,
	}
	cfg := lendCfg()
	cfg.LiquidationMinUSD = 10_000 // require a known USD size >= 10k
	det := &LendingDetector{cfg: cfg, res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("non-stable liquidation should be gated out under a positive floor, got %d", len(out))
	}
}

// Firing: big borrow.

// A stable borrow at/above the floor emits a type-6 signal: subject = the reserve,
// actor = the borrower (onBehalfOf), size_usd from the stable reserve.
func TestLendingScanOnce_BigBorrowEmits(t *testing.T) {
	st := &mockLendingStore{
		logs:     []store.RawLog{borrowLog("0xee", 3, lAaveUSDT0, "75000000000")}, // 75,000 USDT0
		inserted: true,
	}
	det := &LendingDetector{cfg: lendCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 big-borrow signal, got %d", len(out))
	}
	s := out[0]
	if s.SignalType != signalTypeBigBorrow {
		t.Errorf("signal_type = %d, want %d", s.SignalType, signalTypeBigBorrow)
	}
	if s.Metric != metricBigBorrow {
		t.Errorf("metric = %q, want %q", s.Metric, metricBigBorrow)
	}
	if s.Pool != lAaveUSDT0 {
		t.Errorf("subject = %q, want the reserve %q", s.Pool, lAaveUSDT0)
	}
	if s.Actor != lBorrower {
		t.Errorf("actor = %q, want the borrower %q", s.Actor, lBorrower)
	}
	if s.EventRef != "0xee:3" {
		t.Errorf("event_ref = %q, want 0xee:3", s.EventRef)
	}
	if s.SizeUSD == nil || !s.SizeUSD.Equal(dec("75000")) {
		t.Errorf("size_usd = %v, want 75000", s.SizeUSD)
	}
}

// A borrow below the floor does not fire.
func TestLendingScanOnce_BigBorrowBelowFloorSkips(t *testing.T) {
	st := &mockLendingStore{
		logs:     []store.RawLog{borrowLog("0xff", 0, lAaveUSDT0, "10000000000")}, // 10,000 USDT0 < 50k
		inserted: true,
	}
	det := &LendingDetector{cfg: lendCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a sub-floor borrow should not fire, got %d", len(out))
	}
}

// A borrow of a non-stable reserve never fires (cannot confirm it crossed the
// dollar floor), even if the raw amount is huge.
func TestLendingScanOnce_BigBorrowNonStableNeverFires(t *testing.T) {
	st := &mockLendingStore{
		logs:     []store.RawLog{borrowLog("0x01", 0, lAaveFBTC, "100000000000")}, // huge FBTC raw
		inserted: true,
	}
	det := &LendingDetector{cfg: lendCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a non-stable-reserve borrow must not fire, got %d", len(out))
	}
}

// Idempotency + error paths.

// When InsertSignal reports the row already exists (inserted=false), the signal is
// not returned (so it is not re-attested): the (pool, signal_type, event_ref)
// idempotency that overlapping recent windows across ticks rely on.
func TestLendingScanOnce_IdempotentSkip(t *testing.T) {
	st := &mockLendingStore{
		logs:     []store.RawLog{borrowLog("0x02", 4, lAaveUSDe, "100000000000000000000000")}, // 100k USDe
		inserted: false,                                                                       // already recorded
	}
	det := &LendingDetector{cfg: lendCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("an already-recorded event must not be re-emitted, got %d", len(out))
	}
	// InsertSignal was still attempted exactly once (the dedup happens at the DB).
	if len(st.got) != 1 {
		t.Fatalf("InsertSignal should have been attempted once, got %d", len(st.got))
	}
}

// Two events in one scan both emit (a liquidation + a big borrow), proving the loop
// processes every Aave log, not just the first.
func TestLendingScanOnce_MultipleEvents(t *testing.T) {
	st := &mockLendingStore{
		logs: []store.RawLog{
			liqLog("0x03", 0, lAaveUSDT0, "50000000000"),               // liquidation, 50k
			borrowLog("0x03", 1, lAaveUSDe, "60000000000000000000000"), // borrow, 60k USDe
		},
		inserted: true,
	}
	det := &LendingDetector{cfg: lendCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 signals (liquidation + borrow), got %d", len(out))
	}
	// One of each type.
	types := map[int16]bool{}
	for _, s := range out {
		types[s.SignalType] = true
	}
	if !types[signalTypeLiquidation] || !types[signalTypeBigBorrow] {
		t.Errorf("want both type 5 and 6, got %v", types)
	}
}

// A RawLogsSince error aborts the scan with a wrapped error (not a partial result).
func TestLendingScanOnce_RawLogsErrorAborts(t *testing.T) {
	st := &mockLendingStore{logsErr: errors.New("db down")}
	det := &LendingDetector{cfg: lendCfg(), res: nil}
	if _, err := det.scanOnce(context.Background(), st); err == nil {
		t.Fatal("a RawLogsSince failure should abort the scan")
	}
}

// An InsertSignal error aborts the scan (a real DB write failure is fatal, matching
// the other detectors), returning the already-collected signals plus the error.
func TestLendingScanOnce_InsertErrorAborts(t *testing.T) {
	st := &mockLendingStore{
		logs:      []store.RawLog{borrowLog("0x04", 0, lAaveUSDT0, "75000000000")},
		insertErr: errors.New("write failed"),
	}
	det := &LendingDetector{cfg: lendCfg(), res: nil}
	if _, err := det.scanOnce(context.Background(), st); err == nil {
		t.Fatal("an InsertSignal failure should abort the scan")
	}
}
