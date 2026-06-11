package detect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"

	"ocr/internal/config"
	"ocr/internal/lstflow"
	"ocr/internal/store"
)

// These tests cover the LST-flow detector's pure helpers (lstFlowFloorRaw,
// scoreLSTFlow) and its firing path (scanOnce) through a mock store and mock resolver,
// fully hermetic (no DB, no RPC). They exercise mint / burn / move classification, the
// whole-token floor, the DEX-pool noise filter, market-price sizing, per-actor burst
// aggregation, and open/fold idempotency.

// Verified LST addresses + sample parties (lowercase).
const (
	lstMETH     = "0xcda86a272531e8640cd7f1a92c01839911b90bb0" // 18 dec
	lstCMETH    = "0xe6829d9a7ee3040e1276fa75293bde931859e8fa" // 18 dec
	lstZero     = "0x0000000000000000000000000000000000000000"
	lstFrom     = "0x1111111111111111111111111111111111111111"
	lstTo       = "0x2222222222222222222222222222222222222222"
	lstPool     = "0x9999999999999999999999999999999999999999" // a tracked DEX pool
	lstOriginTx = "0x4444444444444444444444444444444444444444"
)

// Log-shaping helpers (build store.RawLog for an ERC-20 Transfer).

func lstWord(hexNo0x string) string { return fmt.Sprintf("%064s", hexNo0x) }

func lstAddrTopic(addr string) string {
	return "0x" + lstWord(strings.ToLower(strings.TrimPrefix(addr, "0x")))
}

func lstUintWord(decStr string) string {
	n, ok := new(big.Int).SetString(decStr, 10)
	if !ok {
		panic("lstUintWord: bad decimal " + decStr)
	}
	return lstWord(fmt.Sprintf("%x", n))
}

// xferLog builds a raw_logs row for an ERC-20 Transfer of `token` from->to of `rawValue`.
func xferLog(token, tx string, logIndex int, from, to, rawValue string) store.RawLog {
	return store.RawLog{
		ChainID:   5000,
		TxHash:    tx,
		LogIndex:  logIndex,
		Address:   token,
		Topic0:    lstflow.TopicTransfer,
		Topic1:    lstAddrTopic(from),
		Topic2:    lstAddrTopic(to),
		Data:      "0x" + lstUintWord(rawValue),
		BlockTime: time.Now().UTC().Add(-2 * time.Minute), // inside the recent window
	}
}

// tenTokensRaw / etc.: handy raw 18-dec amounts.
const (
	rawTenMETH    = "10000000000000000000"  // 10 mETH (= floor)
	rawFiveMETH   = "5000000000000000000"   // 5 mETH (below floor)
	rawHundredETH = "100000000000000000000" // 100 mETH
)

// Mock store.

// mockLSTStore is a hermetic lstFlowStore: it returns canned logs + a canned pool set
// and records every inserted signal, with a configurable "already recorded"
// (inserted=false) reply to exercise idempotency. logsErr / poolsErr / insertErr drive
// the abort paths.
type mockLSTStore struct {
	logs        []store.RawLog
	logsErr     error
	pools       map[string]struct{}
	poolsErr    error
	inserted    bool
	insertErr   error
	got         []store.Signal             // every InsertSignal arg
	open        map[string]*mockOpenSig    // actor -> its open aggregate (set on insert)
	updates     []mockAggUpdate            // every UpdateSignalAggregate call
	tokenPrices map[string]decimal.Decimal // backs the display-only size
}

// mockOpenSig is one actor's open aggregate as the mock remembers it (id + latest payload).
type mockOpenSig struct {
	id      int64
	payload []byte
}

// mockAggUpdate records one UpdateSignalAggregate call so a test can inspect the fold.
type mockAggUpdate struct {
	id      int64
	payload []byte
	sizeUSD *decimal.Decimal
}

func (m *mockLSTStore) RawLogsSince(_ context.Context, _ time.Time, _ []string) ([]store.RawLog, error) {
	return m.logs, m.logsErr
}

func (m *mockLSTStore) ListPoolAddresses(_ context.Context) (map[string]struct{}, error) {
	return m.pools, m.poolsErr
}

func (m *mockLSTStore) OpenLSTSignalByActor(_ context.Context, actor string, _ time.Time) (int64, []byte, bool, error) {
	if o, ok := m.open[actor]; ok && o != nil {
		return o.id, o.payload, true, nil
	}
	return 0, nil, false, nil
}

func (m *mockLSTStore) UpdateSignalAggregate(_ context.Context, id int64, payload []byte, sizeUSD *decimal.Decimal) error {
	m.updates = append(m.updates, mockAggUpdate{id: id, payload: payload, sizeUSD: sizeUSD})
	for _, o := range m.open { // reflect the fold so a later transfer sees the latest refs
		if o != nil && o.id == id {
			o.payload = payload
		}
	}
	return nil
}

func (m *mockLSTStore) TokenPricesUSD(_ context.Context) (map[string]decimal.Decimal, error) {
	return m.tokenPrices, nil
}

func (m *mockLSTStore) InsertSignal(_ context.Context, s store.Signal) (int64, bool, error) {
	if m.insertErr != nil {
		return 0, false, m.insertErr
	}
	m.got = append(m.got, s)
	if !m.inserted {
		return 0, false, nil
	}
	id := int64(len(m.got))
	if m.open == nil {
		m.open = map[string]*mockOpenSig{}
	}
	m.open[s.Actor] = &mockOpenSig{id: id, payload: s.Payload} // now the actor's open aggregate
	return id, true, nil
}

// lstCfg is a config with the shipped LST-flow default floor (10 whole tokens).
func lstCfg() *config.Config {
	return &config.Config{LSTFlowMinTokens: 10}
}

// Pure helpers.

func TestLSTFlowFloorRaw(t *testing.T) {
	// 10 whole tokens at 18 decimals = 10 * 10^18.
	got := lstFlowFloorRaw(10)
	if !got.Equal(dec(rawTenMETH)) {
		t.Fatalf("floor raw = %s, want %s", got.String(), rawTenMETH)
	}
}

func TestScoreLSTFlow(t *testing.T) {
	// <= 1 whole token -> base.
	if s := scoreLSTFlow(dec("1000000000000000000")); s != lstFlowBaseScore { // exactly 1 token
		t.Errorf("1-token score = %g, want base %g", s, lstFlowBaseScore)
	}
	// 10 tokens -> base + log10(10) = base + 1.
	got := scoreLSTFlow(dec(rawTenMETH))
	if got <= lstFlowBaseScore {
		t.Errorf("10-token score = %g, want > base", got)
	}
	// Strictly increasing in size: 1000 tokens beats 10 tokens.
	bigger := scoreLSTFlow(dec("1000000000000000000000")) // 1000 tokens
	if bigger <= got {
		t.Errorf("score should rise with size: 1000 (%g) should beat 10 (%g)", bigger, got)
	}
	// Always clamps within [base, cap] (so ScoreFromZ never overflows uint16).
	huge := scoreLSTFlow(dec("1000000000000000000000000000000000000000000")) // absurd
	if huge < lstFlowBaseScore || huge > lstFlowScoreCapZ {
		t.Errorf("huge score = %g, want within [%g,%g]", huge, lstFlowBaseScore, lstFlowScoreCapZ)
	}
}

// Firing: mint.

// A mint (from == 0x0) at the floor emits a type-4 signal: subject = the LST token,
// actor = the resolved tx.origin, direction "mint", event_ref "tx:logindex", size_usd
// nil. The whole-token amount is in the payload (no USD).
func TestLSTFlowScanOnce_MintEmits(t *testing.T) {
	st := &mockLSTStore{
		logs:     []store.RawLog{xferLog(lstMETH, "0xaa", 7, lstZero, lstTo, rawHundredETH)},
		inserted: true,
	}
	res := newMockResolver()
	res.byTx[common.HexToHash("0xaa")] = common.HexToAddress(lstOriginTx)

	det := &LSTFlowDetector{cfg: lstCfg(), res: res}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 lst_flow signal, got %d", len(out))
	}
	s := out[0]
	if s.SignalType != signalTypeLSTFlow {
		t.Errorf("signal_type = %d, want %d", s.SignalType, signalTypeLSTFlow)
	}
	if s.Metric != metricLSTFlow {
		t.Errorf("metric = %q, want %q", s.Metric, metricLSTFlow)
	}
	// Subject (Pool field) = the LST token address.
	if s.Pool != lstMETH {
		t.Errorf("subject = %q, want the LST token %q", s.Pool, lstMETH)
	}
	// Actor = resolved tx.origin.
	if s.Actor != lstOriginTx {
		t.Errorf("actor = %q, want resolved origin %q", s.Actor, lstOriginTx)
	}
	if s.EventRef != "0xaa:7" {
		t.Errorf("event_ref = %q, want 0xaa:7", s.EventRef)
	}
	// size_usd is nil here because this mock supplies no token prices; the priced path
	// is covered by TestLSTFlowScanOnce_SizeFromTokenPrice.
	if s.SizeUSD != nil {
		t.Errorf("size_usd = %v, want nil with no token price", s.SizeUSD)
	}
	if s.Zscore <= 0 {
		t.Errorf("score input should be positive, got %g", s.Zscore)
	}
	// Payload carries the direction + symbol + raw value.
	if !strings.Contains(string(s.Payload), `"direction":"mint"`) {
		t.Errorf("payload missing mint direction: %s", s.Payload)
	}
	if !strings.Contains(string(s.Payload), `"symbol":"mETH"`) {
		t.Errorf("payload missing mETH symbol: %s", s.Payload)
	}
}

// With a nil resolver, the mint still fires and the actor falls back to the non-zero
// counterparty (the recipient, since From is the zero address).
func TestLSTFlowScanOnce_NilResolverFallsBackToCounterparty(t *testing.T) {
	st := &mockLSTStore{
		logs:     []store.RawLog{xferLog(lstMETH, "0xbb", 0, lstZero, lstTo, rawHundredETH)},
		inserted: true,
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil} // no resolver
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 mint, got %d", len(out))
	}
	if out[0].Actor != lstTo {
		t.Errorf("actor = %q, want the recipient %q (nil-resolver fallback)", out[0].Actor, lstTo)
	}
}

// Firing: burn + move.

// A burn (to == 0x0) emits with direction "burn"; the actor fallback is the sender.
func TestLSTFlowScanOnce_BurnEmits(t *testing.T) {
	st := &mockLSTStore{
		logs:     []store.RawLog{xferLog(lstCMETH, "0xcc", 1, lstFrom, lstZero, rawHundredETH)},
		inserted: true,
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 burn, got %d", len(out))
	}
	if out[0].Actor != lstFrom {
		t.Errorf("burn actor = %q, want the sender %q", out[0].Actor, lstFrom)
	}
	if !strings.Contains(string(out[0].Payload), `"direction":"burn"`) {
		t.Errorf("payload missing burn direction: %s", out[0].Payload)
	}
	if !strings.Contains(string(out[0].Payload), `"symbol":"cmETH"`) {
		t.Errorf("payload missing cmETH symbol: %s", out[0].Payload)
	}
}

// A wallet-to-wallet transfer emits with direction "move".
func TestLSTFlowScanOnce_MoveEmits(t *testing.T) {
	st := &mockLSTStore{
		logs:     []store.RawLog{xferLog(lstMETH, "0xdd", 2, lstFrom, lstTo, rawHundredETH)},
		inserted: true,
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 move, got %d", len(out))
	}
	if !strings.Contains(string(out[0].Payload), `"direction":"move"`) {
		t.Errorf("payload missing move direction: %s", out[0].Payload)
	}
}

// Floor + noise filter.

// A transfer below the whole-token floor does not fire.
func TestLSTFlowScanOnce_BelowFloorSkips(t *testing.T) {
	st := &mockLSTStore{
		logs:     []store.RawLog{xferLog(lstMETH, "0xee", 0, lstFrom, lstTo, rawFiveMETH)}, // 5 < 10
		inserted: true,
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a sub-floor transfer should not fire, got %d", len(out))
	}
	// It must not even attempt an insert (gated before InsertSignal).
	if len(st.got) != 0 {
		t.Fatalf("a sub-floor transfer should not reach InsertSignal, got %d", len(st.got))
	}
}

// A transfer exactly at the floor fires (>= is the gate).
func TestLSTFlowScanOnce_AtFloorFires(t *testing.T) {
	st := &mockLSTStore{
		logs:     []store.RawLog{xferLog(lstMETH, "0xef", 0, lstFrom, lstTo, rawTenMETH)}, // exactly 10
		inserted: true,
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("a floor-sized transfer should fire, got %d", len(out))
	}
}

// The noise filter: a transfer whose counterparty is a tracked DEX pool is skipped
// (it is a swap leg already covered by our swap signals), even when it is large. This
// is the key cleanliness guarantee.
func TestLSTFlowScanOnce_DexPoolLegSkipped(t *testing.T) {
	st := &mockLSTStore{
		logs: []store.RawLog{
			xferLog(lstMETH, "0x10", 0, lstFrom, lstPool, rawHundredETH), // -> pool (sell leg)
			xferLog(lstMETH, "0x11", 0, lstPool, lstTo, rawHundredETH),   // pool -> (buy leg)
		},
		pools:    map[string]struct{}{lstPool: {}},
		inserted: true,
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("DEX-pool legs must be skipped (covered by swap signals), got %d", len(out))
	}
	if len(st.got) != 0 {
		t.Fatalf("a skipped swap leg should not reach InsertSignal, got %d", len(st.got))
	}
}

// A large transfer not touching a pool still fires even when a pool set is present.
func TestLSTFlowScanOnce_NonPoolMoveFiresWithPoolSet(t *testing.T) {
	st := &mockLSTStore{
		logs:     []store.RawLog{xferLog(lstMETH, "0x12", 0, lstFrom, lstTo, rawHundredETH)},
		pools:    map[string]struct{}{lstPool: {}},
		inserted: true,
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("a non-pool transfer should fire even with a pool set, got %d", len(out))
	}
}

// Idempotency + error paths.

// When InsertSignal reports the row already exists (inserted=false), the signal is not
// returned (so it is not re-attested): the (pool, signal_type, event_ref) idempotency
// that overlapping recent windows across ticks rely on.
func TestLSTFlowScanOnce_IdempotentSkip(t *testing.T) {
	st := &mockLSTStore{
		logs:     []store.RawLog{xferLog(lstMETH, "0x13", 4, lstFrom, lstTo, rawHundredETH)},
		inserted: false, // already recorded
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
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

// Multiple qualifying transfers in one scan all emit, proving the loop processes every
// LST log (not just the first), and that a non-Transfer / non-LST log is ignored.
func TestLSTFlowScanOnce_MultipleEventsAndIgnoresNonTransfer(t *testing.T) {
	// A non-Transfer log from the LST token (e.g. Approval) and a non-LST log must be
	// ignored; only the two real transfers fire.
	approval := store.RawLog{
		ChainID: 5000, TxHash: "0x14", LogIndex: 9, Address: lstMETH,
		Topic0:    "0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925", // Approval topic
		Topic1:    lstAddrTopic(lstFrom),
		Topic2:    lstAddrTopic(lstTo),
		Data:      "0x" + lstUintWord(rawHundredETH),
		BlockTime: time.Now().UTC().Add(-1 * time.Minute),
	}
	st := &mockLSTStore{
		logs: []store.RawLog{
			xferLog(lstMETH, "0x15", 0, lstZero, lstTo, rawHundredETH), // mint
			approval, // ignored (not a Transfer)
			xferLog(lstCMETH, "0x16", 1, lstFrom, lstZero, rawHundredETH), // burn
		},
		inserted: true,
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 signals (mint + burn), got %d", len(out))
	}
}

// A wallet's burst aggregates: the first large transfer opens a signal, a later transfer
// by the same actor folds into it (count + total + span grow) instead of opening a second
// row; a distinct actor still opens its own.
func TestLSTFlowScanOnce_AggregatesActorBurst(t *testing.T) {
	other := "0x3333333333333333333333333333333333333333"
	st := &mockLSTStore{
		logs: []store.RawLog{
			xferLog(lstMETH, "0x20", 0, lstFrom, lstTo, rawHundredETH),  // actor A, opens
			xferLog(lstMETH, "0x21", 1, lstFrom, lstTo, rawHundredETH),  // actor A, folds in
			xferLog(lstCMETH, "0x22", 0, lstZero, other, rawHundredETH), // distinct actor, opens its own
		},
		inserted: true,
	}
	cfg := lstCfg()
	cfg.LSTFlowWindowMin = 30
	det := &LSTFlowDetector{cfg: cfg, res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 opened signals (one per actor), got %d", len(out))
	}
	if len(st.updates) != 1 {
		t.Fatalf("want exactly 1 fold (actor A's second transfer), got %d", len(st.updates))
	}
	// The fold grew the aggregate: 2 transfers, 200 total (100 + 100), 2 refs.
	var p lstFlowPayload
	if err := json.Unmarshal(st.updates[0].payload, &p); err != nil {
		t.Fatalf("aggregate payload not JSON: %v", err)
	}
	if p.Transfers != 2 || p.TotalLST != "200" || len(p.EventRefs) != 2 {
		t.Fatalf("aggregate wrong: transfers=%d total=%q refs=%v, want 2/200/2", p.Transfers, p.TotalLST, p.EventRefs)
	}
}

// Folding is idempotent across overlapping scans: re-running the same logs opens nothing
// new and folds nothing again (the refs are already recorded).
func TestLSTFlowScanOnce_AggregateIdempotentAcrossScans(t *testing.T) {
	st := &mockLSTStore{
		logs: []store.RawLog{
			xferLog(lstMETH, "0x25", 0, lstFrom, lstTo, rawHundredETH),
			xferLog(lstMETH, "0x26", 1, lstFrom, lstTo, rawHundredETH),
		},
		inserted: true,
	}
	cfg := lstCfg()
	cfg.LSTFlowWindowMin = 30
	det := &LSTFlowDetector{cfg: cfg, res: nil}
	if _, err := det.scanOnce(context.Background(), st); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	opens1, folds1 := len(st.got), len(st.updates)
	out2, err := det.scanOnce(context.Background(), st) // identical logs again
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if len(out2) != 0 || len(st.got) != opens1 || len(st.updates) != folds1 {
		t.Fatalf("a re-scan must be a no-op: out=%d, inserts %d->%d, folds %d->%d",
			len(out2), opens1, len(st.got), folds1, len(st.updates))
	}
}

// With the window disabled (0), every qualifying transfer opens its own signal (no fold).
func TestLSTFlowScanOnce_WindowDisabledOpensEach(t *testing.T) {
	st := &mockLSTStore{
		logs: []store.RawLog{
			xferLog(lstMETH, "0x27", 0, lstFrom, lstTo, rawHundredETH),
			xferLog(lstMETH, "0x28", 1, lstFrom, lstTo, rawHundredETH),
		},
		inserted: true,
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil} // LSTFlowWindowMin defaults to 0 here
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 || len(st.updates) != 0 {
		t.Fatalf("window 0 should open each (no fold): out=%d updates=%d, want 2/0", len(out), len(st.updates))
	}
}

// size_usd is filled from the token's market price: 100 mETH at $1800 -> 180000.
func TestLSTFlowScanOnce_SizeFromTokenPrice(t *testing.T) {
	st := &mockLSTStore{
		logs:        []store.RawLog{xferLog(lstMETH, "0x30", 0, lstZero, lstTo, rawHundredETH)},
		inserted:    true,
		tokenPrices: map[string]decimal.Decimal{lstMETH: dec("1800")},
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	out, err := det.scanOnce(context.Background(), st)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 signal, got %d", len(out))
	}
	if out[0].SizeUSD == nil || !out[0].SizeUSD.Equal(dec("180000")) {
		t.Fatalf("size_usd = %v, want 180000 (100 mETH * $1800)", out[0].SizeUSD)
	}
}

// A ListPoolAddresses error aborts the scan (the noise-filter set is required).
func TestLSTFlowScanOnce_PoolSetErrorAborts(t *testing.T) {
	st := &mockLSTStore{poolsErr: errors.New("pools query down")}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	if _, err := det.scanOnce(context.Background(), st); err == nil {
		t.Fatal("a ListPoolAddresses failure should abort the scan")
	}
}

// A RawLogsSince error aborts the scan with a wrapped error (not a partial result).
func TestLSTFlowScanOnce_RawLogsErrorAborts(t *testing.T) {
	st := &mockLSTStore{logsErr: errors.New("db down")}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	if _, err := det.scanOnce(context.Background(), st); err == nil {
		t.Fatal("a RawLogsSince failure should abort the scan")
	}
}

// An InsertSignal error aborts the scan (a real DB write failure is fatal, matching
// the other detectors).
func TestLSTFlowScanOnce_InsertErrorAborts(t *testing.T) {
	st := &mockLSTStore{
		logs:      []store.RawLog{xferLog(lstMETH, "0x17", 0, lstFrom, lstTo, rawHundredETH)},
		insertErr: errors.New("write failed"),
	}
	det := &LSTFlowDetector{cfg: lstCfg(), res: nil}
	if _, err := det.scanOnce(context.Background(), st); err == nil {
		t.Fatal("an InsertSignal failure should abort the scan")
	}
}
