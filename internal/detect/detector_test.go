package detect

import (
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"ocr/internal/config"
	"ocr/internal/store"
)

// bkt is a small helper for building baseline/candidate buckets in tests.
func bkt(swaps int, vol0 int64, ts time.Time) store.Bucket {
	return store.Bucket{
		SwapCount: swaps,
		Vol0:      decimal.NewFromInt(vol0),
		Vol1:      decimal.Zero,
		Netflow0:  decimal.NewFromInt(vol0),
		Netflow1:  decimal.Zero,
		TsBucket:  ts,
	}
}

func steadyBaseline(n int, ts time.Time) []store.Bucket {
	out := make([]store.Bucket, 0, n)
	// Small jitter so MAD > 0 on every metric.
	swapPat := []int{18, 20, 22, 19, 21}
	for i := 0; i < n; i++ {
		out = append(out, bkt(swapPat[i%len(swapPat)], int64(1000+i%5), ts.Add(time.Duration(i)*time.Minute)))
	}
	return out
}

// A zero-spread baseline must produce no signal even for an extreme candidate,
// and must never surface a non-finite z-score.
func TestScoreBucket_ZeroSpreadNoFire(t *testing.T) {
	d := NewDetector(nil, &config.Config{ZThreshold: 4.0})
	ts := time.Now().UTC()
	baseline := make([]store.Bucket, 0, 30)
	for i := 0; i < 30; i++ {
		baseline = append(baseline, bkt(20, 1000, ts.Add(time.Duration(i)*time.Minute)))
	}
	latest := bkt(999_999, 1_000_000_000, ts.Add(time.Hour))

	res, fired := d.scoreBucket(latest, baseline, d.cfg.ZThreshold)
	if fired {
		t.Errorf("zero-spread baseline fired: %+v", res)
	}
}

// A real outlier with non-zero spread fires with a finite z-score.
func TestScoreBucket_GenuineOutlierFires(t *testing.T) {
	d := NewDetector(nil, &config.Config{ZThreshold: 4.0})
	ts := time.Now().UTC()
	baseline := steadyBaseline(20, ts)
	latest := bkt(50, 50_000, ts.Add(time.Hour))

	res, fired := d.scoreBucket(latest, baseline, d.cfg.ZThreshold)
	if !fired {
		t.Fatalf("genuine outlier did not fire")
	}
	if math.IsNaN(res.z) || math.IsInf(res.z, 0) {
		t.Errorf("non-finite z on outlier: %v", res.z)
	}
	if math.Abs(res.z) < 4.0 {
		t.Errorf("firing z %v below threshold", res.z)
	}
}

// swap_count below the absolute activity floor must not fire even with high z.
func TestScoreBucket_SwapCountFloor(t *testing.T) {
	d := NewDetector(nil, &config.Config{ZThreshold: 3.0})
	ts := time.Now().UTC()
	baseline := make([]store.Bucket, 0, 16)
	pat := []int{0, 1, 0, 1}
	for i := 0; i < 16; i++ {
		baseline = append(baseline, bkt(pat[i%len(pat)], int64(10+i%3), ts.Add(time.Duration(i)*time.Minute)))
	}
	// 5 swaps is a large jump over a 0/1 baseline but is below swapCountFloor (10).
	latest := bkt(5, 12, ts.Add(time.Hour))
	res, fired := d.scoreBucket(latest, baseline, d.cfg.ZThreshold)
	if fired && res.metric == "swap_count" {
		t.Errorf("swap_count fired below floor (latest=%v, floor=%d)", res.latest, swapCountFloor)
	}
}

// First activity after a dead window (baseline median 0) must not fire on a
// volume/netflow metric; those require median>0 and latest>0.
func TestScoreBucket_VolFloorDeadWindow(t *testing.T) {
	d := NewDetector(nil, &config.Config{ZThreshold: 3.0})
	ts := time.Now().UTC()
	baseline := make([]store.Bucket, 0, 20)
	for i := 0; i < 20; i++ {
		baseline = append(baseline, bkt(0, 0, ts.Add(time.Duration(i)*time.Minute)))
	}
	latest := bkt(0, 5_000_000, ts.Add(time.Hour))
	if res, fired := d.scoreBucket(latest, baseline, d.cfg.ZThreshold); fired {
		t.Errorf("vol metric fired against zero-median baseline: %+v", res)
	}
}

// A per-pool z-threshold override is honored by scoreBucket: a candidate whose
// z clears the global default (4) but not the pool's override (12) must not fire
// under the override, yet does fire under the global default, proving the
// override (not warmup, a floor, or a missing outlier) is what suppressed it.
func TestScoreBucket_PerPoolThresholdOverride(t *testing.T) {
	d := NewDetector(nil, &config.Config{ZThreshold: 4.0})
	ts := time.Now().UTC()

	// Steady swap_count baseline (~20, small jitter so MAD>0). Find a candidate
	// swap_count whose modified z lands strictly between the global 4 and the
	// per-pool 12, so the two gates disagree. swap_count is also gated by an
	// absolute floor (10); the candidate is well above it.
	baseline := steadyBaseline(40, ts)

	const (
		globalZ = 4.0
		poolZ   = 12.0
	)
	var (
		latest store.Bucket
		midZ   float64
		found  bool
	)
	for swaps := swapCountFloor; swaps <= 200; swaps++ {
		cand := bkt(swaps, 1000, ts.Add(time.Hour))
		res, fired := d.scoreBucket(cand, baseline, globalZ)
		if fired && res.metric == "swap_count" && res.z > globalZ && res.z < poolZ {
			latest, midZ, found = cand, res.z, true
			break
		}
	}
	if !found {
		t.Fatalf("could not construct a swap_count candidate with z in (%g, %g)", globalZ, poolZ)
	}

	// Under the pool override (12) the in-between candidate must not fire.
	if res, fired := d.scoreBucket(latest, baseline, poolZ); fired {
		t.Errorf("per-pool override (z=%g) fired on a candidate with z=%.2f (< override): %+v", poolZ, midZ, res)
	}

	// Under the global default (4) the same candidate must fire, proving the
	// override is the only reason it was suppressed above.
	if _, fired := d.scoreBucket(latest, baseline, globalZ); !fired {
		t.Errorf("global default (z=%g) failed to fire on a candidate with z=%.2f (>= default)", globalZ, midZ)
	}
}

// effectiveZThreshold prefers a positive per-pool override and otherwise falls
// back to the global config default (a nil or non-positive override falls back).
func TestEffectiveZThreshold(t *testing.T) {
	cfg := &config.Config{ZThreshold: 4.0}
	f := func(v float64) *float64 { return &v }

	cases := []struct {
		name     string
		override *float64
		want     float64
	}{
		{"nil-override-uses-global", nil, 4.0},
		{"positive-override-wins", f(12.0), 12.0},
		{"zero-override-falls-back", f(0), 4.0},
		{"negative-override-falls-back", f(-3), 4.0},
	}
	for _, c := range cases {
		got := effectiveZThreshold(cfg, store.Pool{ZThreshold: c.override})
		if got != c.want {
			t.Errorf("%s: effectiveZThreshold = %g, want %g", c.name, got, c.want)
		}
	}
}

func TestMetricValue(t *testing.T) {
	b := bkt(7, 123, time.Now())
	if got := metricValue(b, "swap_count"); got != 7 {
		t.Errorf("swap_count = %v, want 7", got)
	}
	if got := metricValue(b, "vol0"); got != 123 {
		t.Errorf("vol0 = %v, want 123", got)
	}
	if got := metricValue(b, "netflow0"); got != 123 {
		t.Errorf("netflow0 = %v, want 123", got)
	}
	if got := metricValue(b, "does-not-exist"); !math.IsNaN(got) {
		t.Errorf("unknown metric = %v, want NaN", got)
	}
}

func TestAbsFloorOK(t *testing.T) {
	cases := []struct {
		metric         string
		latest, median float64
		want           bool
	}{
		{"swap_count", float64(swapCountFloor), 0, true},
		{"swap_count", float64(swapCountFloor) - 0.001, 0, false},
		{"swap_count", 0, 0, false},
		{"vol0", 5, 1, true},
		{"vol0", 0, 1, false},
		{"vol0", 5, 0, false},
		{"netflow0", 5, 1, true},
		{"netflow0", -5, 1, false},
		{"netflow1", 5, -1, false}, // median must be > 0
	}
	for _, c := range cases {
		if got := absFloorOK(c.metric, c.latest, c.median); got != c.want {
			t.Errorf("absFloorOK(%s, lat=%v, med=%v) = %v, want %v", c.metric, c.latest, c.median, got, c.want)
		}
	}
}
