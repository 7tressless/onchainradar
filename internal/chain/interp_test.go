package chain

import (
	"testing"
	"time"
)

// base is an arbitrary fixed instant used as the "from" endpoint timestamp.
// Tests build the "to" endpoint relative to this so expectations stay obvious.
var base = time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

func TestInterpolateBlockTime(t *testing.T) {
	// A regular 2s/block range: 100 blocks span 200s.
	const (
		from = uint64(1000)
		to   = uint64(1100)
	)
	tFrom := base
	tTo := base.Add(200 * time.Second) // 2s per block over 100 blocks

	tests := []struct {
		name string
		blk  uint64
		want time.Time
	}{
		{"endpoint_from", from, tFrom},
		{"endpoint_to", to, tTo},
		{"clamp_below_from", from - 50, tFrom},
		{"clamp_above_to", to + 50, tTo},
		{"midpoint", (from + to) / 2, tFrom.Add(100 * time.Second)}, // block 1050 -> +100s
		{"quarter", from + 25, tFrom.Add(50 * time.Second)},         // block 1025 -> +50s
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := interpolateBlockTime(tFrom, tTo, from, to, tc.blk)
			if !got.Equal(tc.want) {
				t.Fatalf("interpolateBlockTime(blk=%d) = %s, want %s", tc.blk, got, tc.want)
			}
		})
	}
}

// TestInterpolateBlockTimeDegenerate covers to == from: a single-block (or
// inverted) range must collapse to tFrom for any block, matching blockTimeFunc's
// early-return branch which never fetches the second header.
func TestInterpolateBlockTimeDegenerate(t *testing.T) {
	const blk = uint64(2000)
	tFrom := base
	// tTo is deliberately different from tFrom to prove it is ignored when
	// to <= from; the result must still be tFrom.
	tTo := base.Add(time.Hour)

	cases := []struct {
		name     string
		from, to uint64
		probe    uint64
	}{
		{"equal_at_block", blk, blk, blk},
		{"equal_below_block", blk, blk, blk - 1},
		{"equal_above_block", blk, blk, blk + 1},
		{"inverted_to_lt_from", blk, blk - 10, blk - 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := interpolateBlockTime(tFrom, tTo, tc.from, tc.to, tc.probe)
			if !got.Equal(tFrom) {
				t.Fatalf("degenerate range [%d,%d] probe %d = %s, want tFrom %s",
					tc.from, tc.to, tc.probe, got, tFrom)
			}
		})
	}
}
