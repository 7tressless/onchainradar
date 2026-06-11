// Package detect holds the flow detectors and the pure statistics they share.
//
// Detectors compare each pool only to its own recent baseline, never to other
// pools or an external price, so a quiet pool and a busy pool are judged on the
// same relative scale. The baseline uses robust statistics (MAD, not mean/stddev)
// because on-chain data is heavy-tailed: one whale swap or bridge replay skews a
// mean for a long time but barely moves the median or MAD.
//
// This file is the pure MAD (Median Absolute Deviation) z-score math over an
// in-memory sample slice, no SQL or storage. The maturity gate and absolute floor
// that turn a z-score into a fired signal live in detector.go.
package detect

import (
	"math"
	"sort"
)

// ModifiedZBias is the consistency constant 0.6745 (inverse of the standard-
// normal 75th percentile): it scales MAD so that for normal data the modified
// z-score matches an ordinary z-score, keeping thresholds comparable.
const ModifiedZBias = 0.6745

// MedianAndMAD returns the median and the Median Absolute Deviation of xs.
//
// MAD = median(|x - median(xs)|). It returns (NaN, NaN) for an empty slice
// so callers can detect "no samples" without a separate length check. The
// input slice is not mutated: a sorted copy is taken internally.
func MedianAndMAD(xs []float64) (median, mad float64) {
	if len(xs) == 0 {
		return math.NaN(), math.NaN()
	}

	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sort.Float64s(sorted)
	median = Median(sorted)

	deviations := make([]float64, len(sorted))
	for i, v := range sorted {
		deviations[i] = math.Abs(v - median)
	}
	sort.Float64s(deviations)
	mad = Median(deviations)
	return median, mad
}

// Median returns the median of an already-sorted slice (an unsorted slice
// yields a meaningless result). Returns NaN for an empty slice.
func Median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return math.NaN()
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// ModifiedZ returns the modified z-score 0.6745*(value-median)/mad.
//
// It returns 0 when the score is undefined: a non-positive or NaN mad
// (no spread, so nothing can be an outlier), or a NaN median. Callers
// treat |z| >= k together with their own sample-maturity gate as the
// outlier predicate.
func ModifiedZ(value, median, mad float64) float64 {
	if mad <= 0 || math.IsNaN(mad) || math.IsNaN(median) {
		return 0
	}
	return ModifiedZBias * (value - median) / mad
}
