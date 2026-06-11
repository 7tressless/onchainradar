package detect

import (
	"math"
	"testing"
)

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// A zero-spread baseline (MAD == 0) must yield z == 0 (never NaN or ±Inf)
// regardless of how far the value is from the median. This is the core robust-
// stats invariant the whole detector relies on to avoid alerting on flat pools.
func TestModifiedZ_ZeroSpread(t *testing.T) {
	xs := []float64{7, 7, 7, 7, 7, 7}
	median, mad := MedianAndMAD(xs)
	if mad != 0 {
		t.Fatalf("expected MAD 0 for all-equal input, got %v", mad)
	}
	z := ModifiedZ(1e9, median, mad)
	if z != 0 {
		t.Errorf("zero-spread z = %v, want 0", z)
	}
	if !finite(z) {
		t.Errorf("zero-spread z is non-finite: %v", z)
	}
}

func TestModifiedZ_DegenerateInputs(t *testing.T) {
	cases := []struct {
		name               string
		value, median, mad float64
		want               float64
	}{
		{"nan-mad", 5, 10, math.NaN(), 0},
		{"negative-mad", 5, 10, -1, 0},
		{"zero-mad", 5, 10, 0, 0},
		{"nan-median", 5, math.NaN(), 2, 0},
		{"value-equals-median", 10, 10, 2, 0},
		{"positive-outlier", 16, 10, 2, ModifiedZBias * (16 - 10) / 2},
		{"negative-outlier", 4, 10, 2, ModifiedZBias * (4 - 10) / 2},
	}
	for _, c := range cases {
		got := ModifiedZ(c.value, c.median, c.mad)
		if got != c.want {
			t.Errorf("%s: ModifiedZ(%v,%v,%v) = %v, want %v", c.name, c.value, c.median, c.mad, got, c.want)
		}
		if !finite(got) {
			t.Errorf("%s: non-finite z %v", c.name, got)
		}
	}
}

func TestMedianAndMAD(t *testing.T) {
	cases := []struct {
		name           string
		in             []float64
		wantMed, wantM float64
		emptyNaN       bool
	}{
		{name: "empty", in: nil, emptyNaN: true},
		{name: "single", in: []float64{42}, wantMed: 42, wantM: 0},
		{name: "odd", in: []float64{3, 1, 2}, wantMed: 2, wantM: 1},       // sorted 1,2,3; devs 1,0,1 -> 1
		{name: "even", in: []float64{1, 2, 3, 4}, wantMed: 2.5, wantM: 1}, // devs 1.5,0.5,0.5,1.5 -> 1
		{name: "all-equal", in: []float64{5, 5, 5}, wantMed: 5, wantM: 0},
	}
	for _, c := range cases {
		med, mad := MedianAndMAD(c.in)
		if c.emptyNaN {
			if !math.IsNaN(med) || !math.IsNaN(mad) {
				t.Errorf("%s: want (NaN,NaN), got (%v,%v)", c.name, med, mad)
			}
			continue
		}
		if med != c.wantMed || mad != c.wantM {
			t.Errorf("%s: got (med=%v,mad=%v), want (med=%v,mad=%v)", c.name, med, mad, c.wantMed, c.wantM)
		}
	}
}

// MedianAndMAD must not mutate its input slice.
func TestMedianAndMAD_NoMutation(t *testing.T) {
	in := []float64{9, 1, 5, 3, 7}
	cp := append([]float64(nil), in...)
	_, _ = MedianAndMAD(in)
	for i := range in {
		if in[i] != cp[i] {
			t.Fatalf("input mutated at %d: %v != %v", i, in[i], cp[i])
		}
	}
}
