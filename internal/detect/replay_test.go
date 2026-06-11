package detect

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"ocr/internal/config"
	"ocr/internal/store"
)

// These tests cover the pure selection core of Replay (selectReplay): the
// trailing-baseline windowing, the warmup gate, and the bucket-time cooldown.
// They are fully hermetic (no DB, no RPC, no wall-clock dependence; all bucket
// timestamps are constructed from a fixed epoch). The scorer is the detector's
// real d.scoreBucket, so the math path under test is identical to production.

// epoch is a fixed, deterministic base time for bucket series. Using a constant
// (not time.Now) keeps the tests reproducible and free of live-time coupling.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// bucketStep is the spacing between consecutive buckets (the 5-minute window).
const bucketStep = 5 * time.Minute

// swapBucket builds a swap_count bucket at the i-th step from epoch. Volume
// columns are kept steady and non-zero so they never independently trip a
// signal; swap_count is the only metric the series is engineered to move.
func swapBucket(i, swaps int) store.Bucket {
	return store.Bucket{
		SwapCount: swaps,
		Vol0:      decimal.NewFromInt(1000),
		Vol1:      decimal.NewFromInt(1000),
		Netflow0:  decimal.NewFromInt(1000),
		Netflow1:  decimal.NewFromInt(1000),
		TsBucket:  epoch.Add(time.Duration(i) * bucketStep),
	}
}

// steadySwaps appends n steady-but-jittered swap_count buckets starting at
// index start. The small jitter keeps MAD > 0 so a genuine spike yields a
// finite, large modified z-score (a perfectly flat baseline has MAD=0 and can
// never fire; that case is exercised separately).
func steadySwaps(start, n int) []store.Bucket {
	pat := []int{18, 20, 22, 19, 21}
	out := make([]store.Bucket, 0, n)
	for k := 0; k < n; k++ {
		out = append(out, swapBucket(start+k, pat[k%len(pat)]))
	}
	return out
}

// testDetector builds a Detector with no DB (selectReplay and scoreBucket never
// touch it) and the given tunables. warmup is derived exactly as Replay does:
// max(12, BaselineBuckets/4).
func testDetector(baselineBuckets int, z float64, cooldownMin int) (*Detector, int, time.Duration) {
	d := NewDetector(nil, &config.Config{
		BaselineBuckets: baselineBuckets,
		ZThreshold:      z,
		CooldownMin:     cooldownMin,
	})
	warmup := baselineBuckets / 4
	if warmup < 12 {
		warmup = 12
	}
	cooldown := time.Duration(cooldownMin) * time.Minute
	return d, warmup, cooldown
}

// scorerFor adapts d.scoreBucket (which takes a per-pool z-threshold) to the
// generic score closure selectReplay expects, binding the detector's configured
// global ZThreshold, mirroring how Replay binds the effective per-pool value.
func scorerFor(d *Detector) func(store.Bucket, []store.Bucket) (metricResult, bool) {
	return func(latest store.Bucket, baseline []store.Bucket) (metricResult, bool) {
		return d.scoreBucket(latest, baseline, d.cfg.ZThreshold)
	}
}

// A single clear spike over a steady baseline fires exactly once.
func TestSelectReplay_SingleSpikeFiresOnce(t *testing.T) {
	d, warmup, cooldown := testDetector(20, 4.0, 60)

	// 20 steady baseline buckets, then one spike, then steady again.
	buckets := steadySwaps(0, 20)
	spikeIdx := len(buckets)
	buckets = append(buckets, swapBucket(spikeIdx, 80)) // the anomaly
	buckets = append(buckets, steadySwaps(spikeIdx+1, 10)...)

	got := selectReplay(buckets, d.cfg.BaselineBuckets, warmup, cooldown, scorerFor(d))

	if len(got) != 1 {
		t.Fatalf("expected exactly one fire, got %d at %v", len(got), got)
	}
	if got[0] != spikeIdx {
		t.Errorf("fired at index %d, want spike index %d", got[0], spikeIdx)
	}
}

// A second spike within the cooldown window is suppressed; the same spike placed
// after the cooldown fires again. Both scenarios share one baseline so the only
// variable is the gap between spikes.
func TestSelectReplay_CooldownSuppressesThenAllows(t *testing.T) {
	d, warmup, cooldown := testDetector(20, 4.0, 60) // cooldown = 60min = 12 buckets

	// Case A: two spikes 4 buckets (20min) apart, inside the 60min cooldown.
	withinBuckets := steadySwaps(0, 20)
	firstIdx := len(withinBuckets)
	withinBuckets = append(withinBuckets, swapBucket(firstIdx, 80))
	withinBuckets = append(withinBuckets, steadySwaps(firstIdx+1, 3)...)
	secondWithinIdx := len(withinBuckets)
	withinBuckets = append(withinBuckets, swapBucket(secondWithinIdx, 80)) // 4 buckets after first
	withinBuckets = append(withinBuckets, steadySwaps(secondWithinIdx+1, 10)...)

	gotWithin := selectReplay(withinBuckets, d.cfg.BaselineBuckets, warmup, cooldown, scorerFor(d))
	if len(gotWithin) != 1 {
		t.Fatalf("within-cooldown: expected 1 fire, got %d at %v", len(gotWithin), gotWithin)
	}
	if gotWithin[0] != firstIdx {
		t.Errorf("within-cooldown: fired at %d, want first spike %d", gotWithin[0], firstIdx)
	}

	// Case B: two spikes 13 buckets (65min) apart, outside the 60min cooldown.
	afterBuckets := steadySwaps(0, 20)
	firstIdxB := len(afterBuckets)
	afterBuckets = append(afterBuckets, swapBucket(firstIdxB, 80))
	afterBuckets = append(afterBuckets, steadySwaps(firstIdxB+1, 12)...)
	secondAfterIdx := len(afterBuckets)
	afterBuckets = append(afterBuckets, swapBucket(secondAfterIdx, 80)) // 13 buckets after first
	afterBuckets = append(afterBuckets, steadySwaps(secondAfterIdx+1, 10)...)

	gotAfter := selectReplay(afterBuckets, d.cfg.BaselineBuckets, warmup, cooldown, scorerFor(d))
	if len(gotAfter) != 2 {
		t.Fatalf("after-cooldown: expected 2 fires, got %d at %v", len(gotAfter), gotAfter)
	}
	if gotAfter[0] != firstIdxB || gotAfter[1] != secondAfterIdx {
		t.Errorf("after-cooldown: fired at %v, want [%d %d]", gotAfter, firstIdxB, secondAfterIdx)
	}
}

// A cooldown of zero (CooldownMin=0) disables throttling: adjacent spikes both
// fire. Guards the `cooldown > 0` branch in selectReplay.
func TestSelectReplay_ZeroCooldownNoThrottle(t *testing.T) {
	d, warmup, cooldown := testDetector(20, 4.0, 0)
	if cooldown != 0 {
		t.Fatalf("precondition: cooldown should be 0, got %v", cooldown)
	}

	buckets := steadySwaps(0, 20)
	firstIdx := len(buckets)
	buckets = append(buckets, swapBucket(firstIdx, 80))
	secondIdx := len(buckets)
	buckets = append(buckets, swapBucket(secondIdx, 80)) // immediately adjacent
	buckets = append(buckets, steadySwaps(secondIdx+1, 10)...)

	got := selectReplay(buckets, d.cfg.BaselineBuckets, warmup, cooldown, scorerFor(d))
	if len(got) != 2 {
		t.Fatalf("zero-cooldown: expected 2 fires, got %d at %v", len(got), got)
	}
	if got[0] != firstIdx || got[1] != secondIdx {
		t.Errorf("zero-cooldown: fired at %v, want [%d %d]", got, firstIdx, secondIdx)
	}
}

// Fewer than (warmup+1) buckets yields nothing: there is no candidate with a
// warmup-length baseline preceding it. selectReplay returns early on
// len(buckets) <= warmup.
func TestSelectReplay_BelowWarmupYieldsNothing(t *testing.T) {
	d, warmup, cooldown := testDetector(20, 4.0, 60) // warmup = 12

	// Exactly `warmup` buckets, the last of which is a huge spike. With no room
	// for a warmup-length baseline ahead of any candidate, nothing should fire.
	buckets := steadySwaps(0, warmup-1)
	buckets = append(buckets, swapBucket(warmup-1, 999)) // total length == warmup

	if len(buckets) != warmup {
		t.Fatalf("precondition: expected %d buckets, got %d", warmup, len(buckets))
	}

	got := selectReplay(buckets, d.cfg.BaselineBuckets, warmup, cooldown, scorerFor(d))
	if len(got) != 0 {
		t.Errorf("below-warmup: expected no fires, got %d at %v", len(got), got)
	}
}

// An all-equal series has MAD=0 on every metric. ModifiedZ returns 0 for a
// non-positive MAD, so even an extreme-looking candidate must not fire, and no
// NaN/Inf may leak through. This exercises the degenerate-baseline path end to
// end through the real scorer.
func TestSelectReplay_AllEqualSeriesNoFire(t *testing.T) {
	d, warmup, cooldown := testDetector(20, 4.0, 60)

	// 30 identical buckets, then a candidate that is wildly higher. Because the
	// baseline spread is exactly zero, the modified z-score is defined as 0 and
	// nothing fires regardless of how large the candidate is.
	buckets := make([]store.Bucket, 0, 31)
	for i := 0; i < 30; i++ {
		buckets = append(buckets, swapBucket(i, 20))
	}
	buckets = append(buckets, swapBucket(30, 1_000_000))

	got := selectReplay(buckets, d.cfg.BaselineBuckets, warmup, cooldown, scorerFor(d))
	if len(got) != 0 {
		t.Errorf("all-equal series: expected no fires, got %d at %v", len(got), got)
	}
}
