package aggregate

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"ocr/internal/store"
)

// uint256Word renders v as a 32-byte big-endian hex word.
func uint256Word(v uint64) string {
	b := make([]byte, evmWordBytes)
	new(big.Int).SetUint64(v).FillBytes(b)
	return hex.EncodeToString(b)
}

// int256Word renders a (possibly negative) int64 as a two's-complement word.
func int256Word(v int64) string {
	n := big.NewInt(v)
	if v < 0 {
		n.Add(new(big.Int).Lsh(big.NewInt(1), 256), n)
	}
	b := make([]byte, evmWordBytes)
	n.FillBytes(b)
	return hex.EncodeToString(b)
}

func dec(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

// Malformed / short / unknown payloads must return an error (so the caller
// count-only's the swap) and must never panic.
func TestDecodeSwap_Errors(t *testing.T) {
	cases := []struct {
		name  string
		topic string
		data  string
	}{
		{"empty-data", TopicUniV2Swap, "0x"},
		{"empty-noprefix", TopicUniV2Swap, ""},
		{"non-hex", TopicUniV2Swap, "0xZZZZ"},
		{"31-bytes-odd", TopicUniV2Swap, "0x" + strings.Repeat("aa", 31)},
		{"33-bytes-odd", TopicUniV2Swap, "0x" + strings.Repeat("aa", 33)},
		{"v2-three-words", TopicUniV2Swap, "0x" + uint256Word(1) + uint256Word(2) + uint256Word(3)},
		{"v3-one-word", TopicUniV3Swap, "0x" + uint256Word(1)},
		{"unknown-topic", "0xdeadbeefdeadbeef", "0x" + strings.Repeat(uint256Word(1), 4)},
	}
	for _, c := range cases {
		if _, err := DecodeSwap(c.topic, c.data); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}

// V2 sign convention: vol = in+out, netflow = out-in (positive = token leaves
// the pool toward the trader).
func TestDecodeSwap_V2(t *testing.T) {
	// amount0In=10, amount1In=0, amount0Out=0, amount1Out=5
	got, err := DecodeSwap(TopicUniV2Swap, "0x"+uint256Word(10)+uint256Word(0)+uint256Word(0)+uint256Word(5))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Vol0.Equal(dec(10)) || !got.Vol1.Equal(dec(5)) {
		t.Errorf("vol = (%s,%s), want (10,5)", got.Vol0, got.Vol1)
	}
	if !got.Netflow0.Equal(dec(-10)) || !got.Netflow1.Equal(dec(5)) {
		t.Errorf("netflow = (%s,%s), want (-10,5)", got.Netflow0, got.Netflow1)
	}
}

// V3 sign convention: amounts are signed from the pool's perspective; netflow is
// negated so positive = token out to trader, matching V2.
func TestDecodeSwap_V3(t *testing.T) {
	// amount0 = -100 (pool paid token0 out), amount1 = +50 (pool received token1)
	got, err := DecodeSwap(TopicUniV3Swap, "0x"+int256Word(-100)+int256Word(50))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Vol0.Equal(dec(100)) || !got.Vol1.Equal(dec(50)) {
		t.Errorf("vol = (%s,%s), want (100,50)", got.Vol0, got.Vol1)
	}
	if !got.Netflow0.Equal(dec(100)) || !got.Netflow1.Equal(dec(-50)) {
		t.Errorf("netflow = (%s,%s), want (100,-50)", got.Netflow0, got.Netflow1)
	}
	// Trailing words (sqrtPriceX96, liquidity, tick) must be ignored, not error.
	got2, err := DecodeSwap(TopicUniV3Swap, "0x"+int256Word(-100)+int256Word(50)+uint256Word(1)+uint256Word(2)+uint256Word(3))
	if err != nil {
		t.Fatalf("trailing words rejected: %v", err)
	}
	if !got2.Netflow0.Equal(got.Netflow0) {
		t.Errorf("trailing words changed result")
	}
}

// bucketize: known-but-undecodable swaps still count (zero amounts); unknown
// topics are skipped entirely; topic case is normalized.
func TestBucketize_CountsAndSkips(t *testing.T) {
	a := New(nil, 5, 0)                                   // retention 0: pruning disabled (this test exercises bucketize only)
	base := time.Date(2026, 1, 2, 10, 3, 30, 0, time.UTC) // floors to 10:00
	logs := []store.RawLog{
		{Topic0: TopicUniV2Swap, BlockTime: base, Data: "0x" + uint256Word(10) + uint256Word(0) + uint256Word(0) + uint256Word(5)},
		{Topic0: TopicUniV2Swap, BlockTime: base.Add(time.Minute), Data: "0x" + uint256Word(1)}, // undecodable -> count only
		{Topic0: "0xdeadbeef", BlockTime: base, Data: "0x" + uint256Word(99)},                   // unknown -> skipped
		{Topic0: strings.ToUpper(TopicUniV2Swap), BlockTime: base, Data: "0x" + uint256Word(2) + uint256Word(0) + uint256Word(0) + uint256Word(0)},
	}
	buckets := a.bucketize(logs)
	key := base.Truncate(5 * time.Minute)
	b, ok := buckets[key]
	if !ok {
		t.Fatalf("no bucket at %s", key)
	}
	if b.SwapCount != 3 {
		t.Errorf("swap_count = %d, want 3 (unknown skipped, undecodable counted)", b.SwapCount)
	}
	if !b.Vol0.Equal(dec(12)) { // 10 + 0(undecodable) + 2(uppercase)
		t.Errorf("vol0 = %s, want 12", b.Vol0)
	}
}

// floorToBucket anchors on the UTC epoch grid regardless of the input's location.
func TestFloorToBucket(t *testing.T) {
	a := New(nil, 5, 0) // retention 0: pruning disabled (this test exercises floorToBucket only)
	want := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	cases := []time.Time{
		time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 2, 10, 3, 30, 0, time.UTC),
		time.Date(2026, 1, 2, 10, 4, 59, 999999999, time.UTC),
		time.Date(2026, 1, 2, 12, 3, 0, 0, time.FixedZone("UTC+2", 2*3600)), // == 10:03 UTC
	}
	for _, in := range cases {
		if got := a.floorToBucket(in); !got.Equal(want) {
			t.Errorf("floor(%s) = %s, want %s", in, got.UTC(), want)
		}
	}
	// Exact boundary stays put.
	b := time.Date(2026, 1, 2, 10, 5, 0, 0, time.UTC)
	if got := a.floorToBucket(b); !got.Equal(b) {
		t.Errorf("floor(boundary %s) = %s, want unchanged", b, got)
	}
}
