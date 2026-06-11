package aggregate

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// Hermetic tests (pure CPU, no network/RPC/DB/clock) for the swap decoders
// (IsSwapTopic, DecodeSwap, splitPackedAmounts, decodeInt256/decodeUint256), driven by
// hand-crafted ABI-encoded log data. The helpers here are named distinctly from
// aggregate_test.go's so the two files compose without collision.

// hexWord returns the big-endian 32-byte hex encoding of a (non-negative) big.Int.
// It zero-pads on the left, so it is the canonical uint256 word for n.
func hexWord(n *big.Int) string {
	if n.Sign() < 0 {
		// Unsigned-only path; a negative input is a test bug, so fail loud.
		panic("hexWord: negative input; use twosComplementWord for signed values")
	}
	b := make([]byte, evmWordBytes)
	n.FillBytes(b)
	return hex.EncodeToString(b)
}

// uintWord renders a uint64 as a uint256 word.
func uintWord(v uint64) string { return hexWord(new(big.Int).SetUint64(v)) }

// twosComplementWord renders an arbitrary (possibly negative) big.Int as a
// 256-bit two's-complement word. For negative n it computes 2^256 + n, so the
// top bit is set; this is exactly how int256 is laid out on-chain and lets us
// drive decodeInt256's top-bit branch directly.
func twosComplementWord(n *big.Int) string {
	v := new(big.Int).Set(n)
	if v.Sign() < 0 {
		twoTo256 := new(big.Int).Lsh(big.NewInt(1), uint(evmWordBytes*8))
		v.Add(twoTo256, v) // 2^256 + n  (n negative)
	}
	b := make([]byte, evmWordBytes)
	v.FillBytes(b)
	return hex.EncodeToString(b)
}

// packedWord builds a Liquidity Book packed-amounts word: token0 in the low 128
// bits, token1 in the high 128 bits. token0 and token1 must each fit in 128 bits.
func packedWord(token0, token1 *big.Int) string {
	const bits128 = 128
	if token0.Sign() < 0 || token1.Sign() < 0 {
		panic("packedWord: amounts must be non-negative")
	}
	if token0.BitLen() > bits128 || token1.BitLen() > bits128 {
		panic("packedWord: amount exceeds 128 bits")
	}
	hi := new(big.Int).Lsh(token1, bits128)
	combined := new(big.Int).Or(hi, token0)
	return hexWord(combined)
}

// bi is a tiny helper to build a big.Int from an int64.
func bi(v int64) *big.Int { return big.NewInt(v) }

// decd builds a decimal from an int64 (distinct name from aggregate_test.go's dec).
func decd(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

// decBig builds a decimal from a big.Int.
func decBig(n *big.Int) decimal.Decimal { return decimal.NewFromBigInt(n, 0) }

func TestIsSwapTopic_Decode(t *testing.T) {
	cases := []struct {
		name  string
		topic string
		want  bool
	}{
		{"univ2", TopicUniV2Swap, true},
		{"univ3", TopicUniV3Swap, true},
		{"agni", TopicAgniSwap, true},
		{"moe-lb", TopicMoeLBSwap, true},
		// Sanity: the Agni / Moe constants are exactly the canonical Swap topic prefixes.
		{"agni-literal", "0x19b47279256b2a23a1665c810c8d55a1758940ee09377d4f8d26497a3577dc83", true},
		{"moe-literal", "0xad7d6f97abf51ce18e17a38f4d70e975be9c0708474987bb3e26ad21bd93ca70", true},
		// Case-insensitive: an uppercased known topic must still match.
		{"univ2-uppercase", strings.ToUpper(TopicUniV2Swap), true},
		// Negatives.
		{"unrelated", "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := IsSwapTopic(c.topic); got != c.want {
			t.Errorf("%s: IsSwapTopic(%q) = %v, want %v", c.name, c.topic, got, c.want)
		}
	}

	// Guard against the literal-prefix constants drifting.
	if !strings.HasPrefix(TopicAgniSwap, "0x19b47279") {
		t.Errorf("TopicAgniSwap drifted from expected 0x19b47279… prefix: %s", TopicAgniSwap)
	}
	if !strings.HasPrefix(TopicMoeLBSwap, "0xad7d6f97") {
		t.Errorf("TopicMoeLBSwap drifted from expected 0xad7d6f97… prefix: %s", TopicMoeLBSwap)
	}
}

// V2: data = (a0In, a1In, a0Out, a1Out), all uint256.
//
//	vol0 = a0In + a0Out, vol1 = a1In + a1Out
//	netflow0 = a0Out - a0In, netflow1 = a1Out - a1In
func TestDecodeSwap_V2_FourWords(t *testing.T) {
	const a0In, a1In, a0Out, a1Out = 100, 7, 30, 200
	data := "0x" + uintWord(a0In) + uintWord(a1In) + uintWord(a0Out) + uintWord(a1Out)

	got, err := DecodeSwap(TopicUniV2Swap, data)
	if err != nil {
		t.Fatalf("DecodeSwap(v2): unexpected error: %v", err)
	}

	if !got.Vol0.Equal(decd(a0In + a0Out)) { // 130
		t.Errorf("vol0 = %s, want %d", got.Vol0, a0In+a0Out)
	}
	if !got.Vol1.Equal(decd(a1In + a1Out)) { // 207
		t.Errorf("vol1 = %s, want %d", got.Vol1, a1In+a1Out)
	}
	if !got.Netflow0.Equal(decd(a0Out - a0In)) { // -70
		t.Errorf("netflow0 = %s, want %d", got.Netflow0, a0Out-a0In)
	}
	if !got.Netflow1.Equal(decd(a1Out - a1In)) { // 193
		t.Errorf("netflow1 = %s, want %d", got.Netflow1, a1Out-a1In)
	}
}

// V2 with trailing words beyond the 4 required must be tolerated (>=4 check),
// and the extra words must not change the result.
func TestDecodeSwap_V2_ExtraWordsIgnored(t *testing.T) {
	base := "0x" + uintWord(100) + uintWord(7) + uintWord(30) + uintWord(200)
	withExtra := base + uintWord(999) + uintWord(12345)

	got, err := DecodeSwap(TopicUniV2Swap, base)
	if err != nil {
		t.Fatalf("DecodeSwap(v2 base): %v", err)
	}
	got2, err := DecodeSwap(TopicUniV2Swap, withExtra)
	if err != nil {
		t.Fatalf("DecodeSwap(v2 +extra): %v", err)
	}
	if !got.Vol0.Equal(got2.Vol0) || !got.Vol1.Equal(got2.Vol1) ||
		!got.Netflow0.Equal(got2.Netflow0) || !got.Netflow1.Equal(got2.Netflow1) {
		t.Errorf("trailing V2 words changed result: base=%+v extra=%+v", got, got2)
	}
}

// V3 and Agni share the int256 decoder path. One negative amount drives the
// two's-complement top-bit branch in decodeInt256. Trailing words
// (sqrtPriceX96, liquidity, tick, …) must be ignored.
//
//	vol = |amount|, netflow = -amount  (positive = token left pool toward trader)
func TestDecodeSwap_V3AndAgni_SignedAmounts(t *testing.T) {
	for _, topic := range []struct {
		name string
		t    string
	}{
		{"univ3", TopicUniV3Swap},
		{"agni", TopicAgniSwap},
	} {
		// amount0 = -100 (pool paid token0 out), amount1 = +250 (pool received token1).
		amount0 := bi(-100)
		amount1 := bi(250)

		// Two leading int256 words + 3 trailing junk words to confirm they're ignored.
		data := "0x" +
			twosComplementWord(amount0) +
			twosComplementWord(amount1) +
			uintWord(0xdeadbeef) + // sqrtPriceX96 (junk)
			uintWord(42) + // liquidity (junk)
			twosComplementWord(bi(-9)) // tick, signed junk

		got, err := DecodeSwap(topic.t, data)
		if err != nil {
			t.Fatalf("%s: DecodeSwap: %v", topic.name, err)
		}

		// vol = abs(amount)
		if !got.Vol0.Equal(decd(100)) {
			t.Errorf("%s: vol0 = %s, want 100 (abs of -100)", topic.name, got.Vol0)
		}
		if !got.Vol1.Equal(decd(250)) {
			t.Errorf("%s: vol1 = %s, want 250", topic.name, got.Vol1)
		}
		// netflow = -amount  ->  -(-100)=+100, -(250)=-250
		if !got.Netflow0.Equal(decd(100)) {
			t.Errorf("%s: netflow0 = %s, want 100 (-amount0)", topic.name, got.Netflow0)
		}
		if !got.Netflow1.Equal(decd(-250)) {
			t.Errorf("%s: netflow1 = %s, want -250 (-amount1)", topic.name, got.Netflow1)
		}

		// Same two leading words with the trailing junk stripped must match exactly:
		// proves the trailing words contributed nothing.
		minimal := "0x" + twosComplementWord(amount0) + twosComplementWord(amount1)
		gotMin, err := DecodeSwap(topic.t, minimal)
		if err != nil {
			t.Fatalf("%s: DecodeSwap(minimal): %v", topic.name, err)
		}
		if !gotMin.Vol0.Equal(got.Vol0) || !gotMin.Vol1.Equal(got.Vol1) ||
			!gotMin.Netflow0.Equal(got.Netflow0) || !gotMin.Netflow1.Equal(got.Netflow1) {
			t.Errorf("%s: trailing words changed result: min=%+v full=%+v",
				topic.name, gotMin, got)
		}
	}
}

// decodeInt256 directly on a word with the top bit set: a large negative value
// near -2^255. This exercises the two's-complement subtraction past int64 range.
func TestDecodeInt256_TopBitSet(t *testing.T) {
	// -2^255 is the most negative int256: word = 0x8000...0000 (top bit set, rest 0).
	mostNegative := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 255))
	word, err := hex.DecodeString(twosComplementWord(mostNegative))
	if err != nil {
		t.Fatalf("decode crafted word: %v", err)
	}
	if word[0]&0x80 == 0 {
		t.Fatalf("crafted word top bit not set: %x", word[:1])
	}
	got := decodeInt256(word)
	if !got.Equal(decBig(mostNegative)) {
		t.Errorf("decodeInt256(0x8000…) = %s, want %s", got, decBig(mostNegative))
	}

	// A positive word with the top bit clear must stay positive (no subtraction).
	pos := new(big.Int).Lsh(big.NewInt(1), 200) // 2^200, top bit clear
	wPos, err := hex.DecodeString(hexWord(pos))
	if err != nil {
		t.Fatalf("decode positive word: %v", err)
	}
	if !decodeInt256(wPos).Equal(decBig(pos)) {
		t.Errorf("decodeInt256(2^200) misread as negative: %s", decodeInt256(wPos))
	}
}

// decodeUint256 must read the full unsigned range, including a top-bit-set word
// that decodeInt256 would treat as negative.
func TestDecodeUint256_FullRange(t *testing.T) {
	big200 := new(big.Int).Lsh(big.NewInt(1), 200) // 2^200
	word, err := hex.DecodeString(hexWord(big200))
	if err != nil {
		t.Fatalf("decode word: %v", err)
	}
	if !decodeUint256(word).Equal(decBig(big200)) {
		t.Errorf("decodeUint256(2^200) = %s, want %s", decodeUint256(word), decBig(big200))
	}

	// Top-bit-set word is a huge positive uint, not negative.
	topSet := new(big.Int).Lsh(big.NewInt(1), 255) // 2^255
	wTop, err := hex.DecodeString(hexWord(topSet))
	if err != nil {
		t.Fatalf("decode top word: %v", err)
	}
	if got := decodeUint256(wTop); !got.Equal(decBig(topSet)) || got.Sign() <= 0 {
		t.Errorf("decodeUint256(2^255) = %s, want positive %s", got, decBig(topSet))
	}
}

// splitPackedAmounts: token0 in the low 128 bits, token1 in the high 128 bits.
// Using distinct values catches a low/high swap bug.
func TestSplitPackedAmounts_LowHigh(t *testing.T) {
	token0 := bi(0xABCD) // low 128
	token1 := bi(0x1234) // high 128
	word, err := hex.DecodeString(packedWord(token0, token1))
	if err != nil {
		t.Fatalf("decode packed word: %v", err)
	}

	got0, got1 := splitPackedAmounts(word)
	if !got0.Equal(decBig(token0)) {
		t.Errorf("token0 (low) = %s, want %s", got0, decBig(token0))
	}
	if !got1.Equal(decBig(token1)) {
		t.Errorf("token1 (high) = %s, want %s", got1, decBig(token1))
	}

	// Large 128-bit values to confirm the boundary split is exact (no bleed
	// between halves). token0 = 2^128-1 (all low bits), token1 = 1.
	maxLow := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	wBig, err := hex.DecodeString(packedWord(maxLow, big.NewInt(1)))
	if err != nil {
		t.Fatalf("decode big packed word: %v", err)
	}
	g0, g1 := splitPackedAmounts(wBig)
	if !g0.Equal(decBig(maxLow)) {
		t.Errorf("token0 (low, max) = %s, want %s", g0, decBig(maxLow))
	}
	if !g1.Equal(decd(1)) {
		t.Errorf("token1 (high) = %s, want 1 (no bleed from low half)", g1)
	}

	// A non-32-byte word yields (0, 0) by contract rather than panicking.
	short0, short1 := splitPackedAmounts([]byte{0x01, 0x02})
	if !short0.IsZero() || !short1.IsZero() {
		t.Errorf("splitPackedAmounts(short) = (%s,%s), want (0,0)", short0, short1)
	}
}

// Merchant Moe LB full decode: data = [id, amountsIn, amountsOut, volAccum,
// totalFees, protocolFees]. amountsIn/Out pack token0 (low) + token1 (high).
// token0 and token1 amounts differ so a low/high mix-up is caught.
//
//	vol0 = in0+out0, vol1 = in1+out1
//	netflow0 = out0-in0, netflow1 = out1-in1
func TestDecodeSwap_MerchantMoeLB(t *testing.T) {
	// A trader sells token0 into the pool and receives token1:
	//   amountsIn:  token0=1000, token1=0
	//   amountsOut: token0=0,    token1=995
	in0, in1 := bi(1000), bi(0)
	out0, out1 := bi(0), bi(995)

	id := uintWord(8388608) // active bin id (arbitrary; word 0, ignored by decoder)
	data := "0x" +
		id +
		packedWord(in0, in1) + // word 1: amountsIn
		packedWord(out0, out1) + // word 2: amountsOut
		uintWord(123) + // word 3: volatilityAccumulator (ignored)
		packedWord(bi(7), bi(3)) + // word 4: totalFees (ignored)
		packedWord(bi(1), bi(0)) // word 5: protocolFees (ignored)

	got, err := DecodeSwap(TopicMoeLBSwap, data)
	if err != nil {
		t.Fatalf("DecodeSwap(moe-lb): %v", err)
	}

	// vol0 = 1000 + 0 = 1000 ; vol1 = 0 + 995 = 995
	if !got.Vol0.Equal(decd(1000)) {
		t.Errorf("vol0 = %s, want 1000", got.Vol0)
	}
	if !got.Vol1.Equal(decd(995)) {
		t.Errorf("vol1 = %s, want 995", got.Vol1)
	}
	// netflow0 = out0 - in0 = 0 - 1000 = -1000 (token0 entered the pool)
	if !got.Netflow0.Equal(decd(-1000)) {
		t.Errorf("netflow0 = %s, want -1000", got.Netflow0)
	}
	// netflow1 = out1 - in1 = 995 - 0 = +995 (token1 left the pool toward trader)
	if !got.Netflow1.Equal(decd(995)) {
		t.Errorf("netflow1 = %s, want 995", got.Netflow1)
	}

	// Defensive: a swept-direction case where both halves are non-zero on both
	// sides, with token0 != token1, so a low/high swap would change netflow signs.
	in0b, in1b := bi(40), bi(900)
	out0b, out1b := bi(11), bi(500)
	data2 := "0x" + uintWord(1) +
		packedWord(in0b, in1b) +
		packedWord(out0b, out1b) +
		uintWord(0) + packedWord(bi(0), bi(0)) + packedWord(bi(0), bi(0))
	got2, err := DecodeSwap(TopicMoeLBSwap, data2)
	if err != nil {
		t.Fatalf("DecodeSwap(moe-lb 2): %v", err)
	}
	if !got2.Vol0.Equal(decd(40 + 11)) { // 51
		t.Errorf("vol0(2) = %s, want 51", got2.Vol0)
	}
	if !got2.Vol1.Equal(decd(900 + 500)) { // 1400
		t.Errorf("vol1(2) = %s, want 1400", got2.Vol1)
	}
	if !got2.Netflow0.Equal(decd(11 - 40)) { // -29
		t.Errorf("netflow0(2) = %s, want -29", got2.Netflow0)
	}
	if !got2.Netflow1.Equal(decd(500 - 900)) { // -400
		t.Errorf("netflow1(2) = %s, want -400", got2.Netflow1)
	}
}

// LB with trailing words beyond the 3 required (id, in, out) must be tolerated
// and ignored; the 3-word minimum must decode identically to the 6-word form.
func TestDecodeSwap_MerchantMoeLB_MinimalAndExtra(t *testing.T) {
	in := packedWord(bi(1000), bi(2))
	out := packedWord(bi(3), bi(995))

	minimal := "0x" + uintWord(5) + in + out // exactly 3 words
	full := minimal + uintWord(0) + uintWord(0) + uintWord(0)

	gotMin, err := DecodeSwap(TopicMoeLBSwap, minimal)
	if err != nil {
		t.Fatalf("DecodeSwap(lb minimal 3 words): %v", err)
	}
	gotFull, err := DecodeSwap(TopicMoeLBSwap, full)
	if err != nil {
		t.Fatalf("DecodeSwap(lb 6 words): %v", err)
	}
	if !gotMin.Vol0.Equal(gotFull.Vol0) || !gotMin.Vol1.Equal(gotFull.Vol1) ||
		!gotMin.Netflow0.Equal(gotFull.Netflow0) || !gotMin.Netflow1.Equal(gotFull.Netflow1) {
		t.Errorf("LB trailing words changed result: min=%+v full=%+v", gotMin, gotFull)
	}
}

// Undecodable / short / malformed payloads return an error so the caller can
// take the count-only path. These complement (do not duplicate) aggregate_test.go
// by focusing on the per-variant word-count floors and never panicking.
func TestDecodeSwap_ShortDataErrors(t *testing.T) {
	cases := []struct {
		name  string
		topic string
		data  string
	}{
		{"v2-one-word", TopicUniV2Swap, "0x" + uintWord(1)},
		{"v2-two-words", TopicUniV2Swap, "0x" + uintWord(1) + uintWord(2)},
		{"v3-one-word", TopicUniV3Swap, "0x" + uintWord(1)},
		{"agni-one-word", TopicAgniSwap, "0x" + uintWord(1)},
		{"lb-two-words", TopicMoeLBSwap, "0x" + uintWord(1) + uintWord(2)},
		{"empty", TopicUniV2Swap, ""},
		{"empty-prefix", TopicUniV2Swap, "0x"},
		{"odd-31", TopicUniV2Swap, "0x" + strings.Repeat("ab", 31)},
		{"odd-33", TopicUniV3Swap, "0x" + strings.Repeat("ab", 33)},
		{"non-hex", TopicUniV2Swap, "0xzz"},
		{"unknown-topic", "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			"0x" + strings.Repeat(uintWord(1), 4)},
	}
	for _, c := range cases {
		// Must not panic, and must return a non-nil error.
		got, err := DecodeSwap(c.topic, c.data)
		if err == nil {
			t.Errorf("%s: expected error, got nil (result %+v)", c.name, got)
		}
		// On error the contribution must be the zero value (count-only).
		if err != nil && (!got.Vol0.IsZero() || !got.Vol1.IsZero() ||
			!got.Netflow0.IsZero() || !got.Netflow1.IsZero()) {
			t.Errorf("%s: error path returned non-zero amounts: %+v", c.name, got)
		}
	}
}
