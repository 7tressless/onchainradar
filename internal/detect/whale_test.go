package detect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"ocr/internal/config"
	"ocr/internal/store"
)

// These tests cover the pure selection core of the whale detector (pickWhale): the
// median baseline, the multiple-of-median firing gate, and the emit-single-largest
// rule. They are fully hermetic (no DB, no RPC, no wall-clock dependence). The
// cooldown and idempotency gates live in ScanOnce (I/O) and are out of scope here.

// whaleMultiple is the multiple-of-median gate the tests exercise; it mirrors the
// config default (WHALE_MIN_MULTIPLE / defaultWhaleMinMultiple = 25.0).
const whaleMultiple = 25.0

// whaleBaseline builds n usable baseline sizes clustered around `base` with a
// small jitter so MAD > 0 (a perfectly flat baseline has MAD = 0; that
// degenerate case is exercised separately to prove the gate is MAD-independent).
func whaleBaseline(n int, base float64) []float64 {
	jitter := []float64{-2, -1, 0, 1, 2}
	out := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, base+jitter[i%len(jitter)])
	}
	return out
}

// cand builds a candidate with the given token0 size; the other fields are
// irrelevant to pickWhale (it reads only size).
func cand(size float64) whaleCandidate {
	return whaleCandidate{size: size, blockTime: time.Unix(0, 0).UTC()}
}

// effectiveWhaleMinMultiple prefers a positive per-pool override and otherwise
// falls back to the global config default (nil/zero/negative override -> global).
// It also drives a behavioral check that a per-pool override flips pickWhale's
// verdict on the same candidate.
func TestEffectiveWhaleMinMultiple(t *testing.T) {
	cfg := &config.Config{WhaleMinMultiple: whaleMultiple} // global 25
	f := func(v float64) *float64 { return &v }

	cases := []struct {
		name     string
		override *float64
		want     float64
	}{
		{"nil-override-uses-global", nil, whaleMultiple},
		{"positive-override-wins", f(50), 50},
		{"zero-override-falls-back", f(0), whaleMultiple},
		{"negative-override-falls-back", f(-5), whaleMultiple},
	}
	for _, c := range cases {
		got := effectiveWhaleMinMultiple(cfg, store.Pool{WhaleMinMultiple: c.override})
		if got != c.want {
			t.Errorf("%s: effectiveWhaleMinMultiple = %g, want %g", c.name, got, c.want)
		}
	}

	// Behavioral: a 30x swap fires under the global 25x gate but not under a
	// per-pool 50x override; the override is the only thing that changed.
	sizes := whaleBaseline(whaleWarmupSwaps+10, 100) // median ~100
	thirtyX := []whaleCandidate{cand(3000)}          // 30x median

	if _, ok := pickWhale(sizes, thirtyX, effectiveWhaleMinMultiple(cfg, store.Pool{})); !ok {
		t.Errorf("30x swap should fire under the global %gx gate", whaleMultiple)
	}
	if idx, ok := pickWhale(sizes, thirtyX, effectiveWhaleMinMultiple(cfg, store.Pool{WhaleMinMultiple: f(50)})); ok {
		t.Errorf("30x swap should not fire under a 50x per-pool override, got idx=%d", idx)
	}
}

// A single swap well above the multiple-of-median gate is picked.
func TestPickWhale_ClearWhaleFires(t *testing.T) {
	sizes := whaleBaseline(whaleWarmupSwaps+10, 100) // median ~100, tight spread
	candidates := []whaleCandidate{cand(5000)}       // 50x median, past the 25x gate

	idx, ok := pickWhale(sizes, candidates, whaleMultiple)

	if !ok || idx != 0 {
		t.Fatalf("clear whale: expected pick at index 0, got idx=%d ok=%v", idx, ok)
	}
}

// When several candidates clear the gate, the largest one is returned (emit a
// single whale per pool per scan).
func TestPickWhale_LargestOfQualifyingWins(t *testing.T) {
	sizes := whaleBaseline(whaleWarmupSwaps+10, 100) // median ~100

	// Index 1 is the largest; all three clear the 25x gate (>= 2500).
	candidates := []whaleCandidate{
		cand(3000), // 30x
		cand(9000), // 90x, the largest, expected winner
		cand(4000), // 40x
	}

	idx, ok := pickWhale(sizes, candidates, whaleMultiple)
	if !ok {
		t.Fatalf("expected a whale to be picked, got ok=false")
	}
	if idx != 1 {
		t.Fatalf("expected the largest candidate (index 1) to win, got index %d", idx)
	}
}

// A candidate at only ~2x the median must not fire: it is far below the 25x gate.
func TestPickWhale_BelowMultipleNoFire(t *testing.T) {
	sizes := whaleBaseline(whaleWarmupSwaps+10, 100) // median ~100
	twoX := cand(2 * 100)                            // 2x median, well under the 25x gate

	idx, ok := pickWhale(sizes, []whaleCandidate{twoX}, whaleMultiple)
	if ok {
		t.Errorf("2x-median swap below the %gx gate fired: idx=%d", whaleMultiple, idx)
	}

	// And a swap just over the gate does fire, proving the gate (not warmup or a
	// missing candidate) is what suppressed the 2x case.
	overGate := cand(25.1 * 100)
	if idx, ok := pickWhale(sizes, []whaleCandidate{overGate}, whaleMultiple); !ok || idx != 0 {
		t.Errorf("swap above the %gx gate should fire, got idx=%d ok=%v", whaleMultiple, idx, ok)
	}
}

// Too few usable swaps (below whaleWarmupSwaps) yields (-1,false), regardless of
// how extreme the candidate is: the baseline is not yet trustworthy.
func TestPickWhale_WarmupYieldsNothing(t *testing.T) {
	sizes := whaleBaseline(whaleWarmupSwaps-1, 100) // one short of warmup
	candidates := []whaleCandidate{cand(1_000_000)} // absurdly large

	idx, ok := pickWhale(sizes, candidates, whaleMultiple)
	if ok || idx != -1 {
		t.Errorf("below warmup (%d samples): expected (-1,false), got idx=%d ok=%v", len(sizes), idx, ok)
	}
}

// An all-equal-size baseline has MAD = 0 but a well-defined median > 0. The
// multiple-of-median gate is independent of MAD, so a candidate >= 25x median
// STILL fires (no divide-by-zero anywhere); a candidate below the gate does not.
func TestPickWhale_AllEqualBaselineStillGatesOnMultiple(t *testing.T) {
	sizes := make([]float64, whaleWarmupSwaps+10)
	for i := range sizes {
		sizes[i] = 100 // perfectly flat -> MAD == 0, median == 100
	}

	// Guard the precondition: the baseline is genuinely flat (MAD == 0) yet the
	// median is positive, so the gate threshold (25*100) is meaningful.
	med, mad := MedianAndMAD(sizes)
	if mad != 0 {
		t.Fatalf("precondition: flat baseline MAD should be 0, got %v", mad)
	}
	if med != 100 {
		t.Fatalf("precondition: flat baseline median should be 100, got %v", med)
	}

	// A 30x swap clears the gate and fires even though MAD is 0.
	if idx, ok := pickWhale(sizes, []whaleCandidate{cand(3000)}, whaleMultiple); !ok || idx != 0 {
		t.Errorf("flat baseline (MAD=0): 30x swap should still fire, got idx=%d ok=%v", idx, ok)
	}

	// A 10x swap is below the 25x gate and must not fire on the same flat baseline.
	if idx, ok := pickWhale(sizes, []whaleCandidate{cand(1000)}, whaleMultiple); ok {
		t.Errorf("flat baseline (MAD=0): 10x swap below the %gx gate fired: idx=%d", whaleMultiple, idx)
	}
}

// Whale actor best-effort (resolveWhaleActor).
//
// These hermetic tests assert the best-effort contract of the whale's tx.origin
// attribution: a resolved actor is set (lowercase hex), and a nil resolver or resolve
// error yields "" so the whale still fires with a null actor. The firing path
// (ScanOnceExcept) sets sig.Actor to exactly this helper's result and never branches
// on it, so an empty actor cannot block a whale. They use the shared mockResolver
// (defined in smartmoney_test.go).

// A successful resolution returns the lowercase tx.origin hex.
func TestResolveWhaleActor_ResolvedSetsActor(t *testing.T) {
	tx := "0xabc123"
	hash := common.HexToHash(tx)
	want := common.HexToAddress("0x000000000000000000000000000000000000BEEF")

	res := newMockResolver()
	res.byTx[hash] = want
	cache := map[string]common.Address{}

	got := resolveWhaleActor(context.Background(), res, cache, tx)
	if got != "0x000000000000000000000000000000000000beef" {
		t.Fatalf("resolveWhaleActor = %q, want the lowercase tx.origin hex", got)
	}
	// The resolver was actually consulted (not short-circuited).
	if res.calls[hash] != 1 {
		t.Fatalf("expected the resolver to be called once, got %d", res.calls[hash])
	}
}

// A resolver ERROR yields "" (best-effort): the whale must still fire with a null
// actor. The empty string maps to a NULL signals.actor in InsertSignal.
func TestResolveWhaleActor_ErrorYieldsEmpty(t *testing.T) {
	tx := "0xbad"
	hash := common.HexToHash(tx)

	res := newMockResolver()
	res.errTx[hash] = errors.New("rpc down")
	cache := map[string]common.Address{}

	got := resolveWhaleActor(context.Background(), res, cache, tx)
	if got != "" {
		t.Fatalf("resolveWhaleActor on a resolver error = %q, want \"\" (whale still fires, actor null)", got)
	}
}

// A nil resolver (attribution disabled) yields "" without any RPC: the whale fires
// with a null actor.
func TestResolveWhaleActor_NilResolverYieldsEmpty(t *testing.T) {
	if got := resolveWhaleActor(context.Background(), nil, map[string]common.Address{}, "0xabc"); got != "" {
		t.Fatalf("resolveWhaleActor with a nil resolver = %q, want \"\"", got)
	}
}

// The per-scan cache is honoured: a tx resolved once is not re-resolved (a whale on
// the same tx across pools resolves it a single time).
func TestResolveWhaleActor_CachesPerTx(t *testing.T) {
	tx := "0xdeadbeef"
	hash := common.HexToHash(tx)
	want := common.HexToAddress("0x00000000000000000000000000000000000000a1")

	res := newMockResolver()
	res.byTx[hash] = want
	cache := map[string]common.Address{}

	_ = resolveWhaleActor(context.Background(), res, cache, tx)
	_ = resolveWhaleActor(context.Background(), res, cache, tx)
	if res.calls[hash] != 1 {
		t.Fatalf("expected one resolver call across two lookups (cache hit), got %d", res.calls[hash])
	}
}

// NewWhaleDetector accepts a nil resolver (attribution simply disabled) and stores
// the resolver it is given.
func TestNewWhaleDetector_AcceptsResolver(t *testing.T) {
	cfg := &config.Config{WhaleMinMultiple: whaleMultiple}

	if d := NewWhaleDetector(nil, cfg, nil); d == nil || d.res != nil {
		t.Fatal("nil resolver: expected a detector with a nil res field")
	}
	res := newMockResolver()
	if d := NewWhaleDetector(nil, cfg, res); d == nil || d.res == nil {
		t.Fatal("non-nil resolver: expected it to be stored on the detector")
	}
}
