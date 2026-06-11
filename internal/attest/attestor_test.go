package attest

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/ethclient"
)

// SignalHash is committed on-chain, so it must be byte-for-byte deterministic
// and insensitive to the pool address's case.
func TestSignalHash_DeterministicAndCaseInsensitive(t *testing.T) {
	a := SignalHash("0xAbCdef0000000000000000000000000000000001", 1, "swap_count", 500, 1_700_000_000)
	b := SignalHash("0xabcdef0000000000000000000000000000000001", 1, "swap_count", 500, 1_700_000_000)
	c := SignalHash("0xABCDEF0000000000000000000000000000000001", 1, "swap_count", 500, 1_700_000_000)
	if a != b || a != c {
		t.Errorf("SignalHash not case-insensitive on pool: %x / %x / %x", a, b, c)
	}
	// Repeating the exact same inputs reproduces the same hash.
	if a != SignalHash("0xabcdef0000000000000000000000000000000001", 1, "swap_count", 500, 1_700_000_000) {
		t.Errorf("SignalHash non-deterministic across calls")
	}
}

func TestSignalHash_FieldSensitivity(t *testing.T) {
	base := SignalHash("0xabc", 1, "vol0", 10, 100)
	diffs := map[string][32]byte{
		"type":   SignalHash("0xabc", 2, "vol0", 10, 100),
		"metric": SignalHash("0xabc", 1, "vol1", 10, 100),
		"score":  SignalHash("0xabc", 1, "vol0", 11, 100),
		"ts":     SignalHash("0xabc", 1, "vol0", 10, 101),
		"pool":   SignalHash("0xdef", 1, "vol0", 10, 100),
	}
	for field, h := range diffs {
		if h == base {
			t.Errorf("SignalHash collided when %s changed", field)
		}
	}
}

// A metric containing JSON metacharacters must not corrupt the canonical
// encoding or panic; it just yields a stable, distinct hash.
func TestSignalHash_MetricEscaping(t *testing.T) {
	h1 := SignalHash("0xabc", 1, `met"ric`, 1, 1)
	h2 := SignalHash("0xabc", 1, "met\\\"ric", 1, 1)
	h3 := SignalHash("0xabc", 1, "metric\n\t", 1, 1)
	// Each must be deterministic.
	if h1 != SignalHash("0xabc", 1, `met"ric`, 1, 1) {
		t.Errorf("escaped-metric hash non-deterministic")
	}
	// And the escaped forms must not all collapse to the same bytes.
	if h1 == h3 {
		t.Errorf("distinct metrics collided")
	}
	_ = h2
}

func TestScoreFromZ(t *testing.T) {
	cases := []struct {
		z    float64
		want uint16
	}{
		{math.NaN(), 0},
		{0, 0},
		{-5, 500}, // abs value used
		{4.23, 423},
		{655.35, math.MaxUint16}, // exactly at ceiling -> saturates
		{700, math.MaxUint16},
		{math.Inf(1), math.MaxUint16},
		{math.Inf(-1), math.MaxUint16},
	}
	for _, c := range cases {
		if got := ScoreFromZ(c.z); got != c.want {
			t.Errorf("ScoreFromZ(%v) = %d, want %d", c.z, got, c.want)
		}
	}
}

// stubClient returns a real *ethclient.Client over an HTTP URL. HTTP dials are
// lazy, so no network connection is made; enough to satisfy New's nil check and
// exercise the key/address validation that follows.
func stubClient(t *testing.T) *ethclient.Client {
	t.Helper()
	c, err := ethclient.DialContext(context.Background(), "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("dial stub: %v", err)
	}
	return c
}

func TestNew_Validation(t *testing.T) {
	const goodKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	const goodAddr = "0x1111111111111111111111111111111111111111"

	ctx := context.Background()

	// nil ethclient is rejected before anything else, with no panic.
	if _, err := New(ctx, nil, goodKey, goodAddr, 5000, NewNonceManager()); err == nil {
		t.Errorf("New(nil eth) returned nil error")
	}

	eth := stubClient(t)
	defer eth.Close()

	// nil NonceManager is rejected (a per-attestor counter is the collision bug the
	// shared manager removes), with no panic.
	if _, err := New(ctx, eth, goodKey, goodAddr, 5000, nil); err == nil {
		t.Errorf("New(nil nonce manager) returned nil error")
	} else if !strings.Contains(err.Error(), "nil nonce manager") {
		t.Errorf("New(nil nonce manager): error %q missing %q", err.Error(), "nil nonce manager")
	}

	cases := []struct {
		name    string
		key     string
		addr    string
		wantSub string // "" => expect success
	}{
		{"empty-key", "", goodAddr, "empty private key"},
		{"whitespace-key", "   ", goodAddr, "empty private key"},
		{"empty-contract", goodKey, "", "empty contract address"},
		{"invalid-contract", goodKey, "0xnothex", "invalid contract address"},
		{"garbage-key", "zzzz", goodAddr, "parse private key"},
		{"short-key", "abcd", goodAddr, "parse private key"},
		{"valid", goodKey, goodAddr, ""},
		{"valid-0x", "0x" + goodKey, goodAddr, ""},
	}
	for _, c := range cases {
		a, err := New(ctx, eth, c.key, c.addr, 5000, NewNonceManager())
		if c.wantSub == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			} else if a == nil {
				t.Errorf("%s: nil attestor on success", c.name)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: expected error containing %q, got nil", c.name, c.wantSub)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%s: error %q missing %q", c.name, err.Error(), c.wantSub)
		}
		// The OCR wrapper must not echo the raw key back in its own error text.
		if c.key != "" && strings.Contains(err.Error(), c.key) {
			t.Errorf("%s: error leaks full key material: %q", c.name, err.Error())
		}
	}
}
