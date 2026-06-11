package detect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"

	"ocr/internal/config"
	"ocr/internal/store"
)

// These tests cover the smart-money detector's pure cores (scoreSmartMoney,
// pickSmartCandidates, actorDirectionality, the group-by-actor selection) and the
// gate logic of fireForActor and ledgerCandidates through mock stores and a mock
// resolver. They are fully hermetic: no DB, no RPC, no wall clock.

// smThreshold mirrors the config default (SMART_MONEY_THRESHOLD = 50): the 0..100
// score gate ScanOnce applies. The fire / no-fire assertions below compare the
// pure score against this value, so they track the shipped default.
const smThreshold = 50

// scoreSmartMoney

// An accumulating, net-directional actor (>= the default MinSwaps of 2 large
// swaps) with real size scores at or above the threshold and therefore fires.
func TestScoreSmartMoney_AccumulatingActorFires(t *testing.T) {
	// 20x median (full size credit), 3 large swaps, single pool, directionality 1.0:
	// size 1.0*0.30 + accum (3-1)/(6-1)=0.4*0.30=0.12 + cross 0 + dir 1.0*0.25
	// = 0.30+0.12+0.25 = 0.67 -> 67 >= 50.
	score := scoreSmartMoney(20, 3, 1, 1.0)
	if score < smThreshold {
		t.Fatalf("accumulating actor (20x, 3 swaps, 1 pool, dir 1.0): score=%d, want >= %d", score, smThreshold)
	}
	if score < 0 || score > 100 {
		t.Fatalf("score out of range: %d", score)
	}
}

// A churning actor (high size + repetition + breadth but no net direction,
// directionality 0) is held below the gate by the directionality factor (the
// misclassification fix at the score level; the hard floor in fireForActor is the
// belt-and-suspenders). Without the directionality weight this exact input would
// clear 50; with it, the churner is suppressed.
func TestScoreSmartMoney_ChurnerHeldDown(t *testing.T) {
	// 20x size, 2 swaps, 2 pools, directionality 0:
	// size 1.0*0.30 + accum (2-1)/(6-1)=0.2*0.30=0.06 + cross (2-1)/(3-1)=0.5*0.15=0.075
	// + dir 0 = 0.435 -> 44 < 50.
	score := scoreSmartMoney(20, 2, 2, 0.0)
	if score >= smThreshold {
		t.Fatalf("churner (dir 0) should be held below the gate, got score=%d (>= %d)", score, smThreshold)
	}
	// The same actor with full directionality clears the gate, proving direction is
	// the deciding factor here (0.435 + 1.0*0.25 = 0.685 -> 69).
	if s := scoreSmartMoney(20, 2, 2, 1.0); s < smThreshold {
		t.Fatalf("same actor with dir 1.0 should clear the gate, got %d", s)
	}
}

// A one-off large swap (a single swap in the window) earns zero accumulation
// credit; with no net direction either, the score is the size weight alone, well
// under the gate. (fireForActor also hard-gates accumSwaps < MinSwaps, so a one-off
// is rejected before scoring; this proves the score does not rescue a directionless
// one-off either.)
func TestScoreSmartMoney_OneOffDoesNotFire(t *testing.T) {
	// Max size, 1 swap, 1 pool, dir 0: size 1.0*0.30 + accum 0 + cross 0 + dir 0
	// = 0.30 -> 30.
	score := scoreSmartMoney(1_000_000, 1, 1, 0.0)
	if score >= smThreshold {
		t.Fatalf("one-off large swap (1 swap, dir 0): score=%d, want < %d", score, smThreshold)
	}
	// The size weight is the ceiling for a directionless one-off; confirm exactly.
	// (A runtime float var avoids the untyped-constant int-conversion error.)
	sw := sizeWeight
	if want := int(sw*100 + 0.5); score != want {
		t.Fatalf("one-off score = %d, want size-weight ceiling %d", score, want)
	}
}

// Cross-pool breadth raises the score: holding size, swap-count, and directionality
// fixed, more distinct pools yields a strictly higher score (until the cross-pool
// cap).
func TestScoreSmartMoney_CrossPoolRaisesScore(t *testing.T) {
	// Fix a mid directionality (0.5) so breadth is the only mover and the gate flip
	// is attributable to cross-pool, not direction.
	onePool := scoreSmartMoney(10, 3, 1, 0.5)
	twoPools := scoreSmartMoney(10, 3, 2, 0.5)
	threePools := scoreSmartMoney(10, 3, 3, 0.5)

	if !(onePool < twoPools && twoPools < threePools) {
		t.Fatalf("cross-pool should raise score: 1pool=%d 2pool=%d 3pool=%d (want strictly increasing)",
			onePool, twoPools, threePools)
	}

	// A purely cross-pool difference can flip a sub-threshold actor over the gate:
	// size 0.5*0.30=0.15 + accum 0.4*0.30=0.12 + dir 0.5*0.25=0.125 = 0.395 -> 40 on
	// one pool, but reaches the gate once breadth is added (3 pools adds 0.15 -> 55).
	if onePool >= smThreshold {
		t.Fatalf("precondition: single-pool case should be below threshold, got %d", onePool)
	}
	if threePools < smThreshold {
		t.Fatalf("three-pool case should clear threshold, got %d", threePools)
	}
}

// Net directionality raises the score: holding size, swap-count, and pools fixed, a
// stronger net direction yields a strictly higher score (until it saturates at 1).
func TestScoreSmartMoney_DirectionalityRaisesScore(t *testing.T) {
	low := scoreSmartMoney(10, 3, 2, 0.0)
	mid := scoreSmartMoney(10, 3, 2, 0.5)
	high := scoreSmartMoney(10, 3, 2, 1.0)
	if !(low < mid && mid < high) {
		t.Fatalf("directionality should raise score: dir0=%d dir.5=%d dir1=%d (want strictly increasing)",
			low, mid, high)
	}
}

// scoreSmartMoney is monotonic non-decreasing in each input and always clamped to
// [0,100], including degenerate / extreme inputs.
func TestScoreSmartMoney_MonotonicAndClamped(t *testing.T) {
	// Clamp: extreme inputs never escape [0,100].
	if s := scoreSmartMoney(1e9, 1000, 1000, 1e9); s != 100 {
		t.Fatalf("saturating inputs should score 100, got %d", s)
	}
	// Clamp low: a below-baseline swap with a single swap/pool/no direction floors.
	if s := scoreSmartMoney(0, 1, 1, 0); s < 0 || s > 100 {
		t.Fatalf("score out of range for minimal inputs: %d", s)
	}
	// Negative-ish inputs (defensive; ScanOnce never passes these) stay clamped.
	if s := scoreSmartMoney(-5, 0, 0, -1); s < 0 {
		t.Fatalf("negative inputs should clamp to >= 0, got %d", s)
	}

	// Monotonic in size multiple (accum/pools/dir fixed).
	prev := -1
	for _, m := range []float64{0, 1, 5, 10, 20, 40, 100} {
		s := scoreSmartMoney(m, 3, 2, 0.5)
		if s < prev {
			t.Fatalf("score not monotonic in size: multiple=%g gave %d < previous %d", m, s, prev)
		}
		prev = s
	}
	// Monotonic in accumulation depth (size/pools/dir fixed).
	prev = -1
	for _, n := range []int{1, 2, 3, 4, 6, 10, 50} {
		s := scoreSmartMoney(10, n, 2, 0.5)
		if s < prev {
			t.Fatalf("score not monotonic in accum swaps: n=%d gave %d < previous %d", n, s, prev)
		}
		prev = s
	}
	// Monotonic in distinct pools (size/accum/dir fixed).
	prev = -1
	for _, dp := range []int{1, 2, 3, 4, 10} {
		s := scoreSmartMoney(10, 3, dp, 0.5)
		if s < prev {
			t.Fatalf("score not monotonic in distinct pools: dp=%d gave %d < previous %d", dp, s, prev)
		}
		prev = s
	}
	// Monotonic in directionality (size/accum/pools fixed).
	prev = -1
	for _, dir := range []float64{0, 0.1, 0.25, 0.5, 0.75, 1.0, 5.0} {
		s := scoreSmartMoney(10, 3, 2, dir)
		if s < prev {
			t.Fatalf("score not monotonic in directionality: dir=%g gave %d < previous %d", dir, s, prev)
		}
		prev = s
	}
}

// pickSmartCandidates

// Candidates clearing the size bar are selected (with their multiple set); those
// below are dropped; input order is preserved.
func TestPickSmartCandidates_SelectsAboveBar(t *testing.T) {
	const median = 100.0
	const threshold = 5 * median // the default 5x bar -> 500

	in := []smCandidate{
		{size: 100, tx: "0xa", logIndex: 0},  // 1x, below the bar
		{size: 600, tx: "0xb", logIndex: 1},  // 6x, clears
		{size: 499, tx: "0xc", logIndex: 2},  // ~5x, just below
		{size: 5000, tx: "0xd", logIndex: 3}, // 50x, clears
	}
	got := pickSmartCandidates(median, in, threshold)

	if len(got) != 2 {
		t.Fatalf("expected 2 candidates above the bar, got %d", len(got))
	}
	if got[0].tx != "0xb" || got[1].tx != "0xd" {
		t.Fatalf("order/selection wrong: got %q then %q, want 0xb then 0xd", got[0].tx, got[1].tx)
	}
	// multiple must be computed for the survivors.
	if got[0].multiple != 6 || got[1].multiple != 50 {
		t.Fatalf("multiple not set correctly: got %g and %g, want 6 and 50", got[0].multiple, got[1].multiple)
	}
}

// A non-positive median or threshold yields no candidates (a non-positive bar
// would otherwise admit everything), and a candidate with size<=0 is never picked.
func TestPickSmartCandidates_GuardsAndNonPositive(t *testing.T) {
	cands := []smCandidate{{size: 1_000_000, tx: "0xa"}}
	if got := pickSmartCandidates(0, cands, 0); got != nil {
		t.Fatalf("median<=0 should yield no candidates, got %d", len(got))
	}
	if got := pickSmartCandidates(100, cands, 0); got != nil {
		t.Fatalf("threshold<=0 should yield no candidates, got %d", len(got))
	}
	// size<=0 is excluded even when the bar is met by others.
	mixed := []smCandidate{{size: 0, tx: "0xzero"}, {size: 600, tx: "0xb"}}
	got := pickSmartCandidates(100, mixed, 500)
	if len(got) != 1 || got[0].tx != "0xb" {
		t.Fatalf("size<=0 should be excluded; got %d candidates", len(got))
	}
}

// Mock SenderResolver + resolveActorCached.

// mockResolver is a hermetic SenderResolver: it maps a tx hash to a fixed address
// or a fixed error, and counts calls so the per-tx cache can be verified. It also
// records whether the most recent call's context carried a deadline, so the
// per-call resolution timeout can be asserted without a real RPC.
type mockResolver struct {
	byTx        map[common.Hash]common.Address
	errTx       map[common.Hash]error
	calls       map[common.Hash]int
	lastHadDdl  bool // did the last TxSender ctx have a deadline?
	sawDeadline bool // did any TxSender ctx have a deadline?
}

func newMockResolver() *mockResolver {
	return &mockResolver{
		byTx:  map[common.Hash]common.Address{},
		errTx: map[common.Hash]error{},
		calls: map[common.Hash]int{},
	}
}

func (m *mockResolver) TxSender(ctx context.Context, txHash common.Hash) (common.Address, error) {
	m.calls[txHash]++
	_, m.lastHadDdl = ctx.Deadline()
	if m.lastHadDdl {
		m.sawDeadline = true
	}
	if err, ok := m.errTx[txHash]; ok {
		return common.Address{}, err
	}
	if a, ok := m.byTx[txHash]; ok {
		return a, nil
	}
	return common.Address{}, errors.New("mock: unknown tx")
}

// recordingInserter is a hermetic largeSwapInserter that appends every ledger
// write so a test can assert the complete-ledger guarantee: pass A must record
// every selected candidate, regardless of which one later fires.
type recordingInserter struct {
	inserted []store.LargeSwap
	err      error // when set, every InsertLargeSwap fails with it
}

func (r *recordingInserter) InsertLargeSwap(_ context.Context, ls store.LargeSwap) (bool, error) {
	if r.err != nil {
		return false, r.err
	}
	r.inserted = append(r.inserted, ls)
	return true, nil
}

// A successful resolution is returned and CACHED: a second lookup of the same tx
// does not call the resolver again (a tx with multiple swap logs resolves once).
func TestResolveActorCached_CachesPerTx(t *testing.T) {
	// HexToHash left-pads to 32 bytes, so a short hex literal is a valid tx hash.
	tx := "0xabc123"
	hash := common.HexToHash(tx)
	want := common.HexToAddress("0x000000000000000000000000000000000000beef")

	res := newMockResolver()
	res.byTx[hash] = want
	cache := map[string]common.Address{}

	got1, ok1 := resolveActorCached(context.Background(), res, cache, tx)
	got2, ok2 := resolveActorCached(context.Background(), res, cache, tx)

	if !ok1 || !ok2 {
		t.Fatalf("expected both resolutions to succeed, got ok1=%v ok2=%v", ok1, ok2)
	}
	if got1 != want || got2 != want {
		t.Fatalf("resolved address mismatch: got %s and %s, want %s", got1.Hex(), got2.Hex(), want.Hex())
	}
	if n := res.calls[hash]; n != 1 {
		t.Fatalf("expected the resolver to be called once (cache hit on the 2nd), got %d", n)
	}
}

// A resolver error yields ok=false (the caller skips that swap) without panicking
// or aborting, and a subsequent distinct tx still resolves, proving one
// unresolvable tx does not break the rest of the pass. The failing tx is not
// cached, so it is not mistaken for a resolved actor.
func TestResolveActorCached_ErrorSkipsWithoutAborting(t *testing.T) {
	badTx := "0xbad"
	goodTx := "0xa1"
	badHash := common.HexToHash(badTx)
	goodHash := common.HexToHash(goodTx)
	wantGood := common.HexToAddress("0x00000000000000000000000000000000000000a1")

	res := newMockResolver()
	res.errTx[badHash] = errors.New("rpc down")
	res.byTx[goodHash] = wantGood
	cache := map[string]common.Address{}

	// The failing tx is skipped (ok=false) and never cached.
	if _, ok := resolveActorCached(context.Background(), res, cache, badTx); ok {
		t.Fatalf("expected resolver error to yield ok=false")
	}
	if _, cached := cache[badTx]; cached {
		t.Fatalf("a failed resolution must not be cached")
	}

	// A distinct, healthy tx still resolves in the same pass.
	got, ok := resolveActorCached(context.Background(), res, cache, goodTx)
	if !ok || got != wantGood {
		t.Fatalf("healthy tx after a failure should resolve: got %s ok=%v, want %s", got.Hex(), ok, wantGood.Hex())
	}
}

// resolveActorCached bounds the RPC with a per-call deadline: even when the parent
// context has none, the resolver is called with a context that does carry a
// deadline, so a hung node cannot block the call forever. The cache hit on a second
// lookup must not re-impose (or need) a deadline; it makes no RPC.
func TestResolveActorCached_BoundsCallWithDeadline(t *testing.T) {
	tx := "0xdead"
	hash := common.HexToHash(tx)
	res := newMockResolver()
	res.byTx[hash] = common.HexToAddress("0x000000000000000000000000000000000000beef")
	cache := map[string]common.Address{}

	// Parent has NO deadline; the per-call timeout must still apply one.
	if _, ok := resolveActorCached(context.Background(), res, cache, tx); !ok {
		t.Fatalf("expected resolution to succeed")
	}
	if !res.lastHadDdl {
		t.Fatalf("expected the resolver call to carry a deadline (per-call timeout), but it did not")
	}

	// Second lookup is a cache hit: no RPC, so the resolver is not called again.
	if _, ok := resolveActorCached(context.Background(), res, cache, tx); !ok {
		t.Fatalf("expected cached resolution to succeed")
	}
	if n := res.calls[hash]; n != 1 {
		t.Fatalf("expected exactly one resolver call (2nd is a cache hit), got %d", n)
	}
}

// ledgerCandidates (pass A: ledger every candidate)

// mkCand builds a selected smCandidate with a distinct tx/size for the ledger
// tests. size is the gross token0 size; sizeDec mirrors it as exact units.
func mkCand(tx string, logIndex int, size float64) smCandidate {
	return smCandidate{
		size:      size,
		sizeDec:   decimal.NewFromFloat(size),
		tx:        tx,
		logIndex:  logIndex,
		blockTime: time.Now().UTC(),
	}
}

// Pass A must ledger every selected candidate, not stop at the first: if it broke
// out of the loop on the first firing candidate, later large swaps would never be
// recorded and an actor's accumulation would undercount on the next tick. With three
// candidates across two actors, all three large_swaps rows must be written and all
// three returned for the firing pass, in input (oldest-first) order.
func TestLedgerCandidates_LedgersAllSelected(t *testing.T) {
	a := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	b := common.HexToAddress("0x00000000000000000000000000000000000000bb")

	res := newMockResolver()
	res.byTx[common.HexToHash("0x1")] = a
	res.byTx[common.HexToHash("0x2")] = a // same actor, second large swap
	res.byTx[common.HexToHash("0x3")] = b

	det := &SmartMoneyDetector{res: res} // db/cfg unused by ledgerCandidates
	ins := &recordingInserter{}
	cache := map[string]common.Address{}
	pool := store.Pool{Address: "0xpool"}

	selected := []smCandidate{
		mkCand("0x1", 0, 600),
		mkCand("0x2", 1, 700),
		mkCand("0x3", 2, 800),
	}

	ledgered, err := det.ledgerCandidates(context.Background(), ins, cache, pool, 100.0, nil, selected)
	if err != nil {
		t.Fatalf("ledgerCandidates returned error: %v", err)
	}
	// Every candidate ledgered (the whole point of pass A).
	if len(ins.inserted) != 3 {
		t.Fatalf("expected all 3 candidates ledgered, got %d", len(ins.inserted))
	}
	if len(ledgered) != 3 {
		t.Fatalf("expected 3 ledgered candidates returned, got %d", len(ledgered))
	}
	// Order preserved (oldest-first) and actor attached.
	wantActors := []string{"0x00000000000000000000000000000000000000aa", "0x00000000000000000000000000000000000000aa", "0x00000000000000000000000000000000000000bb"}
	for i, lc := range ledgered {
		if lc.actor != wantActors[i] {
			t.Fatalf("ledgered[%d] actor = %s, want %s", i, lc.actor, wantActors[i])
		}
		if lc.c.tx != selected[i].tx {
			t.Fatalf("ledgered[%d] tx = %s, want %s (order not preserved)", i, lc.c.tx, selected[i].tx)
		}
	}
	// The ledger rows carry the resolved actor + the pool, lowercased.
	if ins.inserted[0].Actor != "0x00000000000000000000000000000000000000aa" || ins.inserted[0].Pool != "0xpool" {
		t.Fatalf("ledger row 0 wrong: actor=%s pool=%s", ins.inserted[0].Actor, ins.inserted[0].Pool)
	}
}

// A candidate whose actor cannot be resolved is skipped (its swap is lost) but
// must not abort the pass: the remaining candidates are still resolved and
// ledgered. This proves one unresolvable tx does not lose the rest of the pool.
func TestLedgerCandidates_SkipsUnresolvableButLedgersRest(t *testing.T) {
	good := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	res := newMockResolver()
	res.errTx[common.HexToHash("0x1")] = errors.New("rpc down") // unresolvable
	res.byTx[common.HexToHash("0x2")] = good                    // resolves fine

	det := &SmartMoneyDetector{res: res}
	ins := &recordingInserter{}
	cache := map[string]common.Address{}

	selected := []smCandidate{
		mkCand("0x1", 0, 600), // actor unresolvable -> skipped, not ledgered
		mkCand("0x2", 1, 700), // still ledgered
	}

	ledgered, err := det.ledgerCandidates(context.Background(), ins, cache, store.Pool{Address: "0xpool"}, 100.0, nil, selected)
	if err != nil {
		t.Fatalf("an unresolvable tx must not abort the pass, got error: %v", err)
	}
	if len(ins.inserted) != 1 || ins.inserted[0].TxHash != "0x2" {
		t.Fatalf("expected only the resolvable candidate ledgered, got %d rows", len(ins.inserted))
	}
	if len(ledgered) != 1 || ledgered[0].c.tx != "0x2" {
		t.Fatalf("expected only 0x2 returned for the firing pass, got %d", len(ledgered))
	}
}

// A real DB write failure does abort the pass (return a wrapped error), matching
// the other detectors' contract: a ledger write failure is not silently
// swallowed, since a missing ledger row would corrupt accumulation counts.
func TestLedgerCandidates_DBErrorAborts(t *testing.T) {
	res := newMockResolver()
	res.byTx[common.HexToHash("0x1")] = common.HexToAddress("0x00000000000000000000000000000000000000aa")

	det := &SmartMoneyDetector{res: res}
	ins := &recordingInserter{err: errors.New("db down")}
	cache := map[string]common.Address{}

	_, err := det.ledgerCandidates(context.Background(), ins, cache, store.Pool{Address: "0xpool"}, 100.0, nil, []smCandidate{mkCand("0x1", 0, 600)})
	if err == nil {
		t.Fatalf("expected a DB write failure to abort the pass with an error")
	}
}

// actorDirectionality (net-vs-churn)

// tf builds a TokenFlow from plain float64 net/gross for the directionality tests.
func tf(token string, net, gross float64) store.TokenFlow {
	return store.TokenFlow{
		Token: token,
		Net:   decimal.NewFromFloat(net),
		Gross: decimal.NewFromFloat(gross),
	}
}

// A pure accumulator (every leg in one direction) has |net| == gross on its asset,
// so directionality is ~1.0.
func TestActorDirectionality_AccumulatorIsOne(t *testing.T) {
	flows := []store.TokenFlow{
		tf("0xweth", 1000, 1000), // bought 1000, sold 0 -> |1000|/1000 = 1.0
		tf("0xusdc", -500, 500),  // spent 500 -> |−500|/500 = 1.0 (the other side)
	}
	d := actorDirectionality(flows)
	if d < 0.999 {
		t.Fatalf("pure accumulator should have directionality ~1.0, got %g", d)
	}
}

// A churning arb/MM bot's legs cancel: net ~= 0 while gross stays large, so
// directionality is ~0 on every asset -> the max is ~0.
func TestActorDirectionality_ChurnerIsZero(t *testing.T) {
	flows := []store.TokenFlow{
		tf("0xwmnt", 0, 2000), // bought 1000 then sold 1000 across pools -> net 0, gross 2000
		tf("0xusde", 5, 1995), // tiny residual on the quote leg -> ~0.0025
	}
	d := actorDirectionality(flows)
	if d > 0.05 {
		t.Fatalf("churner should have directionality ~0, got %g", d)
	}
}

// directionality is the MAX over tokens: one strongly-directional asset lifts the
// score even if another asset churns.
func TestActorDirectionality_TakesMaxOverTokens(t *testing.T) {
	flows := []store.TokenFlow{
		tf("0xchurn", 0, 1000), // 0.0
		tf("0xdir", 800, 1000), // 0.8  <- the max
		tf("0xmid", 300, 1000), // 0.3
	}
	d := actorDirectionality(flows)
	if d < 0.79 || d > 0.81 {
		t.Fatalf("directionality should be the max (0.8), got %g", d)
	}
}

// Guards: empty flows (e.g. all rows predate migration 0008 and netted to nothing)
// yield 0; a token with gross 0 is skipped (no divide-by-zero, no spurious credit);
// a negative net uses its magnitude.
func TestActorDirectionality_GuardsAndEmpty(t *testing.T) {
	if d := actorDirectionality(nil); d != 0 {
		t.Fatalf("nil flows should yield 0, got %g", d)
	}
	if d := actorDirectionality([]store.TokenFlow{}); d != 0 {
		t.Fatalf("empty flows should yield 0, got %g", d)
	}
	// gross 0 is skipped (would be 0/0); the only real signal is the second token.
	flows := []store.TokenFlow{
		tf("0xzero", 0, 0),      // skipped (gross 0); must not panic or credit
		tf("0xreal", -900, 900), // |−900|/900 = 1.0
	}
	if d := actorDirectionality(flows); d < 0.999 {
		t.Fatalf("a gross-0 token must be skipped and the real token used, got %g", d)
	}
	// A lone gross-0 token yields 0 (nothing measurable), never a divide-by-zero.
	if d := actorDirectionality([]store.TokenFlow{tf("0xzero", 100, 0)}); d != 0 {
		t.Fatalf("a lone gross-0 token should yield 0, got %g", d)
	}
}

// sortedActors / bestCandidateForActor (group-by-actor, fire one)

// lcand builds a ledgeredCandidate for the grouping tests: actor + pool + median +
// a candidate of the given size (so candidateMultiple = size/median).
func lcand(actor, pool string, median, size float64, tx string) ledgeredCandidate {
	return ledgeredCandidate{
		c:      smCandidate{size: size, tx: tx, blockTime: time.Unix(0, 0).UTC()},
		actor:  actor,
		pool:   pool,
		median: median,
	}
}

// sortedActors returns the distinct actors in deterministic (lexicographic) order,
// collapsing an actor that appears across multiple pools/candidates to one entry.
func TestSortedActors_DistinctAndDeterministic(t *testing.T) {
	ledgered := []ledgeredCandidate{
		lcand("0xbb", "0xp1", 100, 600, "0x1"),
		lcand("0xaa", "0xp2", 100, 700, "0x2"),
		lcand("0xbb", "0xp3", 100, 800, "0x3"), // same actor 0xbb, different pool
		lcand("0xaa", "0xp1", 100, 900, "0x4"), // same actor 0xaa, different pool
	}
	got := sortedActors(ledgered)
	want := []string{"0xaa", "0xbb"}
	if len(got) != len(want) {
		t.Fatalf("expected %d distinct actors, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedActors[%d] = %q, want %q (order: %v)", i, got[i], want[i], got)
		}
	}
}

// bestCandidateForActor picks the actor's HIGHEST-multiple candidate (the single
// fire candidate), across pools, ignoring other actors' candidates.
func TestBestCandidateForActor_PicksHighestMultiple(t *testing.T) {
	ledgered := []ledgeredCandidate{
		lcand("0xaa", "0xp1", 100, 600, "0x1"),  // 0xaa: 6x
		lcand("0xbb", "0xp2", 100, 9000, "0x2"), // 0xbb: 90x (must be ignored for 0xaa)
		lcand("0xaa", "0xp3", 100, 2500, "0x3"), // 0xaa: 25x  <- best for 0xaa
		lcand("0xaa", "0xp1", 50, 600, "0x4"),   // 0xaa: 12x (median 50)
	}
	best, ok := bestCandidateForActor(ledgered, "0xaa")
	if !ok {
		t.Fatalf("expected a best candidate for 0xaa")
	}
	if best.c.tx != "0x3" {
		t.Fatalf("best candidate tx = %q, want 0x3 (the 25x one)", best.c.tx)
	}
	// A non-existent actor yields ok=false.
	if _, ok := bestCandidateForActor(ledgered, "0xcc"); ok {
		t.Fatalf("expected ok=false for an actor with no candidates")
	}
}

// fireForActor (cooldown + floor + gates)

// mockFireStore is a hermetic smartFireStore: it returns canned accumulation,
// token-flows, and cooldown values, and records the inserted signal. Any field's
// err* makes that method fail, so the abort paths are exercised without Postgres.
type mockFireStore struct {
	lastTime   time.Time
	lastOK     bool
	lastErr    error
	accumSwaps int
	pools      int
	accumErr   error
	flows      []store.TokenFlow
	flowsErr   error
	inserted   bool // value InsertSignal reports for "freshly inserted"
	insertErr  error
	gotSignal  *store.Signal // captured insert (nil until InsertSignal is called)
}

func (m *mockFireStore) LastSignalTimeByActor(_ context.Context, _ string, _ int16) (time.Time, bool, error) {
	return m.lastTime, m.lastOK, m.lastErr
}
func (m *mockFireStore) ActorActivitySince(_ context.Context, _ string, _ time.Time) (int, int, error) {
	return m.accumSwaps, m.pools, m.accumErr
}
func (m *mockFireStore) ActorTokenFlowsSince(_ context.Context, _ string, _ time.Time) ([]store.TokenFlow, error) {
	return m.flows, m.flowsErr
}
func (m *mockFireStore) InsertSignal(_ context.Context, s store.Signal) (int64, bool, error) {
	if m.insertErr != nil {
		return 0, false, m.insertErr
	}
	cp := s
	m.gotSignal = &cp
	if m.inserted {
		return 42, true, nil
	}
	return 0, false, nil
}

// fireCfg is a config with the shipped smart-money defaults, for the gate tests.
func fireCfg() *config.Config {
	return &config.Config{
		SmartMoneyMinSwaps:          defaultMinSwaps,
		SmartMoneyThreshold:         smThreshold,
		SmartMoneyWindowHours:       24,
		SmartMoneyMinDirectionality: 0.5,
	}
}

// defaultMinSwaps mirrors the config default (SMART_MONEY_MIN_SWAPS = 2).
const defaultMinSwaps = 2

// A net-directional, accumulating, large actor not in cooldown FIRES: the signal
// carries the actor (for the next cooldown), the pool subject, and a high score.
func TestFireForActor_FiresOnGenuineAccumulation(t *testing.T) {
	st := &mockFireStore{
		lastOK:     false, // never fired before -> not in cooldown
		accumSwaps: 3,
		pools:      1,
		flows:      []store.TokenFlow{tf("0xweth", 1000, 1000)}, // directionality 1.0
		inserted:   true,
	}
	det := &SmartMoneyDetector{cfg: fireCfg()}
	best := lcand("0xactor", "0xpool", 100, 2000, "0xtx") // 20x size

	sig, fired, err := det.fireForActor(context.Background(), st, "0xactor", best, time.Now().Add(-24*time.Hour), 240*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fired {
		t.Fatalf("a genuine net-directional accumulator should fire")
	}
	if sig.Actor != "0xactor" {
		t.Fatalf("fired signal must carry the actor for the per-actor cooldown, got %q", sig.Actor)
	}
	if sig.Pool != "0xpool" || sig.SignalType != 3 || sig.EventRef != "0xtx:0" {
		t.Fatalf("fired signal fields wrong: pool=%q type=%d ref=%q", sig.Pool, sig.SignalType, sig.EventRef)
	}
	if st.gotSignal == nil || st.gotSignal.Actor != "0xactor" {
		t.Fatalf("InsertSignal must receive the actor on the row")
	}
}

// The spam fix: an actor that fired a type-3 within the cooldown is suppressed,
// regardless of how strong its current candidate is. No InsertSignal happens.
func TestFireForActor_PerActorCooldownSuppresses(t *testing.T) {
	st := &mockFireStore{
		lastTime:   time.Now().Add(-1 * time.Hour), // fired 1h ago
		lastOK:     true,
		accumSwaps: 9, // would easily clear every other gate
		pools:      3,
		flows:      []store.TokenFlow{tf("0xweth", 1000, 1000)},
		inserted:   true,
	}
	det := &SmartMoneyDetector{cfg: fireCfg()}
	best := lcand("0xactor", "0xpool", 100, 5000, "0xtx")

	_, fired, err := det.fireForActor(context.Background(), st, "0xactor", best, time.Now().Add(-24*time.Hour), 240*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fired {
		t.Fatalf("an actor in cooldown must be suppressed")
	}
	if st.gotSignal != nil {
		t.Fatalf("a cooling-down actor must not reach InsertSignal")
	}
}

// A cooldown of 0 disables the per-actor gate (parity with COOLDOWN_MIN=0), so even
// a recently-fired actor is eligible again.
func TestFireForActor_ZeroCooldownDisablesGate(t *testing.T) {
	st := &mockFireStore{
		lastTime:   time.Now().Add(-1 * time.Minute), // fired a minute ago
		lastOK:     true,
		accumSwaps: 3,
		pools:      1,
		flows:      []store.TokenFlow{tf("0xweth", 1000, 1000)},
		inserted:   true,
	}
	det := &SmartMoneyDetector{cfg: fireCfg()}
	best := lcand("0xactor", "0xpool", 100, 2000, "0xtx")

	_, fired, err := det.fireForActor(context.Background(), st, "0xactor", best, time.Now().Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fired {
		t.Fatalf("cooldown 0 should disable the gate and let the actor fire")
	}
}

// The misclassification fix: a churning actor (directionality below the floor) is
// excluded outright even with high size + accumulation + breadth. No InsertSignal.
func TestFireForActor_DirectionalityFloorExcludesChurner(t *testing.T) {
	st := &mockFireStore{
		accumSwaps: 8,
		pools:      4,
		// Churn: net ~= 0 on both legs, gross large -> directionality ~0.0025 < 0.5.
		flows:    []store.TokenFlow{tf("0xwmnt", 0, 4000), tf("0xusde", 10, 3990)},
		inserted: true,
	}
	det := &SmartMoneyDetector{cfg: fireCfg()}
	best := lcand("0xchurner", "0xpool", 100, 5000, "0xtx") // 50x size

	_, fired, err := det.fireForActor(context.Background(), st, "0xchurner", best, time.Now().Add(-24*time.Hour), 240*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fired {
		t.Fatalf("a churner below the directionality floor must be excluded")
	}
	if st.gotSignal != nil {
		t.Fatalf("a churner must not reach InsertSignal")
	}
}

// The accumulation floor still applies: a one-off large swap (accumSwaps below
// MinSwaps) does not fire, even when perfectly net-directional.
func TestFireForActor_AccumulationFloorExcludesOneOff(t *testing.T) {
	st := &mockFireStore{
		accumSwaps: 1, // below MinSwaps (2)
		pools:      1,
		flows:      []store.TokenFlow{tf("0xweth", 1000, 1000)}, // direction 1.0
		inserted:   true,
	}
	det := &SmartMoneyDetector{cfg: fireCfg()}
	best := lcand("0xactor", "0xpool", 100, 5000, "0xtx")

	_, fired, err := det.fireForActor(context.Background(), st, "0xactor", best, time.Now().Add(-24*time.Hour), 240*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fired {
		t.Fatalf("a one-off (accumSwaps < MinSwaps) must not fire")
	}
}

// An idempotent re-scan (InsertSignal reports already-recorded) returns fired=false
// without error, so the duplicate is neither returned nor re-attested.
func TestFireForActor_IdempotentSkip(t *testing.T) {
	st := &mockFireStore{
		accumSwaps: 3,
		pools:      1,
		flows:      []store.TokenFlow{tf("0xweth", 1000, 1000)},
		inserted:   false, // already recorded
	}
	det := &SmartMoneyDetector{cfg: fireCfg()}
	best := lcand("0xactor", "0xpool", 100, 2000, "0xtx")

	_, fired, err := det.fireForActor(context.Background(), st, "0xactor", best, time.Now().Add(-24*time.Hour), 240*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fired {
		t.Fatalf("an already-recorded signal must report fired=false")
	}
}

// A real DB read failure (here the cooldown lookup) aborts with a wrapped error,
// matching the other detectors' contract; never swallowed.
func TestFireForActor_DBErrorAborts(t *testing.T) {
	st := &mockFireStore{lastErr: errors.New("db down")}
	det := &SmartMoneyDetector{cfg: fireCfg()}
	best := lcand("0xactor", "0xpool", 100, 2000, "0xtx")

	_, fired, err := det.fireForActor(context.Background(), st, "0xactor", best, time.Now().Add(-24*time.Hour), 240*time.Minute)
	if err == nil {
		t.Fatalf("a DB read failure must abort with an error")
	}
	if fired {
		t.Fatalf("no signal should fire on a DB error")
	}
}
