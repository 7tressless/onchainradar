package attest

// Additional hermetic unit tests for the attestor pure functions
// (SignalHash, ScoreFromZ). These complement attestor_test.go and cover the
// money-path invariants: the on-chain signal hash must be deterministic and
// distinct per logical signal, and stay valid even when the metric string
// contains JSON metacharacters; the z-score-to-uint16 conversion must never
// overflow, sign-flip, or panic.
//
// No network, no key material. All helpers/symbols here are uniquely named so
// this file co-compiles with attestor_test.go.

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// keccakOf hashes raw bytes with the same primitive SignalHash uses, so the
// oracle's expected hash is computed identically to the implementation.
func keccakOf(b []byte) [32]byte { return crypto.Keccak256Hash(b) }

// canonicalOracle independently reconstructs the exact byte string that
// SignalHash documents it hashes:
//
//	{"pool":<lowercase>,"type":<n>,"metric":<json-escaped>,"score":<n>,"ts":<n>}
//
// Built without fmt-on-the-impl so it is a genuine second opinion. The metric is
// escaped via encoding/json (the same primitive the impl uses) so we are
// asserting the *shape*, not re-implementing the escaper by hand.
func canonicalOracle(t *testing.T, pool string, signalType uint8, metric string, score uint16, ts int64) []byte {
	t.Helper()
	mj, err := json.Marshal(metric)
	if err != nil {
		t.Fatalf("oracle: marshal metric %q: %v", metric, err)
	}
	s := `{"pool":"` + strings.ToLower(pool) + `","type":` +
		itoaU8(signalType) + `,"metric":` + string(mj) + `,"score":` +
		itoaU16(score) + `,"ts":` + fmt.Sprintf("%d", ts) + `}`
	return []byte(s)
}

func itoaU8(v uint8) string   { return fmt.Sprintf("%d", v) }
func itoaU16(v uint16) string { return fmt.Sprintf("%d", v) }

// TestSignalHash_StableAcrossManyCalls hammers the same logical signal many
// times: the hash committed on-chain must be byte-identical every call, in this
// process and (by construction, since it has no hidden state) across processes.
func TestSignalHash_StableAcrossManyCalls(t *testing.T) {
	const (
		pool   = "0xAbCdEf0000000000000000000000000000000123"
		typ    = uint8(1)
		metric = "swap_count"
		score  = uint16(412)
		ts     = int64(1_750_000_000)
	)
	want := SignalHash(pool, typ, metric, score, ts)
	for i := 0; i < 1000; i++ {
		if got := SignalHash(pool, typ, metric, score, ts); got != want {
			t.Fatalf("SignalHash drifted on call %d: %x != %x", i, got, want)
		}
	}
}

// TestSignalHash_EachFieldChangesHash asserts every field is load-bearing and
// that all the perturbed hashes are pairwise distinct. Pairwise uniqueness is the
// anti-collision property: if two different logical signals shared a hash, the
// on-chain record would be ambiguous.
func TestSignalHash_EachFieldChangesHash(t *testing.T) {
	base := SignalHash("0xaaa", 1, "vol0", 10, 100)

	variants := map[string][32]byte{
		"base":   base,
		"pool":   SignalHash("0xbbb", 1, "vol0", 10, 100),
		"type":   SignalHash("0xaaa", 2, "vol0", 10, 100),
		"metric": SignalHash("0xaaa", 1, "vol1", 10, 100),
		"score":  SignalHash("0xaaa", 1, "vol0", 11, 100),
		"ts":     SignalHash("0xaaa", 1, "vol0", 10, 101),
	}

	seen := make(map[[32]byte]string, len(variants))
	for name, h := range variants {
		if prev, dup := seen[h]; dup {
			t.Errorf("hash collision: %q and %q produced identical hash %x", prev, name, h)
		}
		seen[h] = name
	}
}

// TestSignalHash_MatchesCanonicalEncoding pins the exact canonical bytes via the
// independent oracle and confirms the resulting JSON parses, so the documented
// wire format cannot silently change and stays valid JSON for the dashboard to
// re-derive.
func TestSignalHash_MatchesCanonicalEncoding(t *testing.T) {
	pool := "0xDeadBeef00000000000000000000000000000001"
	canon := canonicalOracle(t, pool, 1, "swap_count", 412, 1_750_000_000)

	// The canonical string must itself be valid, re-parseable JSON.
	var probe map[string]any
	if err := json.Unmarshal(canon, &probe); err != nil {
		t.Fatalf("canonical encoding is not valid JSON: %v\n%s", err, canon)
	}
	if got, ok := probe["pool"].(string); !ok || got != strings.ToLower(pool) {
		t.Fatalf("canonical pool not lowercased: got %v", probe["pool"])
	}

	// Hashing the oracle bytes must equal SignalHash for the same inputs.
	wantFromOracle := keccakOf(canon)
	got := SignalHash(pool, 1, "swap_count", 412, 1_750_000_000)
	if got != wantFromOracle {
		t.Fatalf("SignalHash diverged from documented canonical encoding:\n got=%x\n want=%x\n canon=%s", got, wantFromOracle, canon)
	}
}

// TestSignalHash_MetricJSONInjectionIsContained checks that a metric string full
// of JSON metacharacters (whether crafted or accidental) still:
//  1. never panics,
//  2. keeps the canonical encoding valid JSON,
//  3. stays deterministic, and
//  4. does not collide with the hash of a different logical signal whose
//     "type"/"score" fields happen to match text embedded in the metric value.
func TestSignalHash_MetricJSONInjectionIsContained(t *testing.T) {
	trickyMetrics := []string{
		`met"ric`,
		`met\ric`,
		`met"ric\`,
		"metric\n\t\r",
		"metric\x00\x01\x1f",                   // control characters
		`","type":99,"score":65535,"metric":"`, // text that looks like extra fields
		`{"nested":"object"}`,
		`}]}`,
		strings.Repeat(`"\`, 64),
	}

	for _, m := range trickyMetrics {
		m := m
		t.Run(fmt.Sprintf("%q", m), func(t *testing.T) {
			// (1) no panic + (3) deterministic
			h1 := SignalHash("0xabc", 1, m, 1, 1)
			h2 := SignalHash("0xabc", 1, m, 1, 1)
			if h1 != h2 {
				t.Fatalf("metric with metacharacters produced non-deterministic hash: %x != %x", h1, h2)
			}

			// (2) canonical bytes remain valid JSON and round-trip the metric
			// back to the exact original value (proves the metacharacters were
			// escaped, not interpreted as structure).
			canon := canonicalOracle(t, "0xabc", 1, m, 1, 1)
			var decoded map[string]any
			if err := json.Unmarshal(canon, &decoded); err != nil {
				t.Fatalf("metric with metacharacters broke canonical JSON: %v\n%s", err, canon)
			}
			if got, _ := decoded["metric"].(string); got != m {
				t.Fatalf("metric did not round-trip: got %q want %q", got, m)
			}
			// The impl's own hash must match the oracle for this input.
			if want := keccakOf(canon); h1 != want {
				t.Fatalf("metric with metacharacters: impl hash != canonical hash\n got=%x\n want=%x", h1, want)
			}
		})
	}

	// (4) A metric built to look like it carries type=99/score=65535 must not
	// collide with the signal that actually has type=99/score=65535. Because the
	// metric is a JSON string token, that text stays inside quotes and the two
	// encodings differ.
	lookalikeMetric := `","type":99,"score":65535,"metric":"`
	forgedHash := SignalHash("0xpool", 1, lookalikeMetric, 1, 100)
	genuine := SignalHash("0xpool", 99, "metric", 65535, 100)
	if forgedHash == genuine {
		t.Fatalf("metric value changed the hash of a different signal: %x", forgedHash)
	}
}

// TestSignalHash_PoolCaseInsensitivity confirms the documented behavior: the
// pool string is lowercased before hashing, so a checksummed (EIP-55 mixed-case)
// address and its lowercase form attest to the same hash, while two genuinely
// different pools never collide.
func TestSignalHash_PoolCaseInsensitivity(t *testing.T) {
	checksummed := "0xAbCdEf0000000000000000000000000000000001"
	lower := "0xabcdef0000000000000000000000000000000001"
	upper := "0XABCDEF0000000000000000000000000000000001"

	a := SignalHash(checksummed, 1, "m", 5, 7)
	b := SignalHash(lower, 1, "m", 5, 7)
	c := SignalHash(upper, 1, "m", 5, 7)
	if a != b || a != c {
		t.Fatalf("pool case-insensitivity broken: %x / %x / %x", a, b, c)
	}

	// Different pools must produce different hashes (here the addresses differ
	// only in their last nibble: 0x...01 vs 0x...02).
	other := SignalHash("0xabcdef0000000000000000000000000000000002", 1, "m", 5, 7)
	if other == a {
		t.Fatalf("distinct pools collided to %x", a)
	}
}

// TestScoreFromZ_Table is the extended z-score conversion table. It pins the
// canonical z->score values plus every boundary and special-float case.
// Sign must be dropped (|z|), NaN -> 0, +/-Inf and oversized z saturate at
// uint16 max, and rounding follows math.Round (round-half-away-from-zero).
func TestScoreFromZ_Table(t *testing.T) {
	cases := []struct {
		name string
		z    float64
		want uint16
	}{
		{"nan", math.NaN(), 0},
		{"zero", 0, 0},
		{"typical_4.0", 4.0, 400},   // z=4.0 -> 400
		{"typical_250", 250, 25000}, // z=250 -> 25000
		{"fractional", 4.23, 423},   // 4.23 * 100 = 423
		// Exact-half cases: 4.005*100 and 0.005*100 are exactly representable as
		// 400.5 / 0.5, so they genuinely exercise round-half-away-from-zero.
		{"exact_half_up", 4.005, 401}, // 400.5 -> 401
		{"exact_half_tiny", 0.005, 1}, // 0.5 -> 1
		// Float reality: 1.005 is not exactly 1.005; 1.005*100 == 100.4999...,
		// so it rounds down to 100. This pins that ScoreFromZ uses binary float
		// rounding (math.Round), not decimal rounding: the score is committed
		// on-chain and any re-derivation must match bit-for-bit.
		{"binary_float_not_decimal", 1.005, 100},
		{"round_down", 1.004, 100},  // 100.4 -> 100
		{"negative_abs", -4.0, 400}, // abs used
		{"negative_large", -250, 25000},
		{"just_below_ceiling", 655.34, 65534},
		{"at_ceiling", 655.35, math.MaxUint16}, // 655.35 -> saturates
		{"above_ceiling", 700, math.MaxUint16},
		{"huge", 1e18, math.MaxUint16},
		{"pos_inf", math.Inf(1), math.MaxUint16},
		{"neg_inf", math.Inf(-1), math.MaxUint16},
		{"tiny_floor", 0.004, 0}, // 0.4 -> rounds to 0
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ScoreFromZ(c.z); got != c.want {
				t.Errorf("ScoreFromZ(%v) = %d, want %d", c.z, got, c.want)
			}
		})
	}
}

// TestScoreFromZ_NeverPanicsAndNeverWraps fuzzes a spread of finite and
// non-finite inputs and asserts the result is always a valid uint16 in range
// (the type guarantees the upper bound; this guards against a future change that
// might compute a negative or NaN-derived value and wrap on conversion).
func TestScoreFromZ_NeverPanicsAndNeverWraps(t *testing.T) {
	inputs := []float64{
		math.NaN(), math.Inf(1), math.Inf(-1),
		-1e308, -1e6, -655.35, -1, -0.0, 0,
		0.5, 4, 655.35, 1e6, 1e308,
		math.SmallestNonzeroFloat64, math.MaxFloat64,
	}
	for _, z := range inputs {
		got := ScoreFromZ(z) // would panic on a bad implementation
		// uint16 cannot exceed MaxUint16 by type; assert the saturating contract
		// explicitly for the out-of-range magnitudes.
		if math.Abs(z) >= MaxScoreZ && !math.IsNaN(z) && got != math.MaxUint16 {
			t.Errorf("ScoreFromZ(%v) = %d, expected saturation at %d", z, got, uint16(math.MaxUint16))
		}
		if math.IsNaN(z) && got != 0 {
			t.Errorf("ScoreFromZ(NaN) = %d, want 0", got)
		}
	}
}
