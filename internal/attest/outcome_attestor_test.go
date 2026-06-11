package attest

import (
	"context"
	"strings"
	"testing"
)

// NewOutcome mirrors New's construction-time validation. These tests exercise the
// key/address validation and the no-key-leak guarantee on the sidecar attestor, without
// any network (the stub ethclient dials lazily over HTTP).
func TestNewOutcome_Validation(t *testing.T) {
	const goodKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	const goodAddr = "0x2222222222222222222222222222222222222222"

	ctx := context.Background()

	// nil ethclient is rejected before anything else, with no panic.
	if _, err := NewOutcome(ctx, nil, goodKey, goodAddr, 5000, NewNonceManager()); err == nil {
		t.Errorf("NewOutcome(nil eth) returned nil error")
	}

	eth := stubClient(t)
	defer eth.Close()

	// nil NonceManager is rejected, with no panic.
	if _, err := NewOutcome(ctx, eth, goodKey, goodAddr, 5000, nil); err == nil {
		t.Errorf("NewOutcome(nil nonce manager) returned nil error")
	} else if !strings.Contains(err.Error(), "nil nonce manager") {
		t.Errorf("NewOutcome(nil nonce manager): error %q missing %q", err.Error(), "nil nonce manager")
	}

	cases := []struct {
		name    string
		key     string
		addr    string
		wantSub string // "" => expect success
	}{
		{"empty-key", "", goodAddr, "empty private key"},
		{"whitespace-key", "   ", goodAddr, "empty private key"},
		{"empty-contract", goodKey, "", "empty outcome contract address"},
		{"invalid-contract", goodKey, "0xnothex", "invalid outcome contract address"},
		{"garbage-key", "zzzz", goodAddr, "parse private key"},
		{"short-key", "abcd", goodAddr, "parse private key"},
		{"valid", goodKey, goodAddr, ""},
		{"valid-0x", "0x" + goodKey, goodAddr, ""},
	}
	for _, c := range cases {
		a, err := NewOutcome(ctx, eth, c.key, c.addr, 5000, NewNonceManager())
		if c.wantSub == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			} else if a == nil {
				t.Errorf("%s: nil outcome attestor on success", c.name)
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
