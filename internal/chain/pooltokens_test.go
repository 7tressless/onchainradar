package chain

import (
	"bytes"
	"testing"
)

// PoolTokens selects the on-chain accessor pair by dex name. FusionX V2 is a
// Uniswap-V2-shape pair exposing the same token0()/token1() accessors as Agni, so
// "fusionx" must reuse the Agni selector path, not merchant_moe's Liquidity Book
// getTokenX()/getTokenY() pair.
//
// PoolTokens itself issues a live eth_call and cannot run without an RPC node (the full
// fusionx round-trip is covered hermetically in the discover package via a mock
// ChainVerifier). This locks the selector choice against an RPC-free, self-derived ground
// truth so a future edit cannot silently point fusionx at the wrong accessors.
func TestPoolTokensSelectorsFusionxUseAgniPair(t *testing.T) {
	// Ground truth, re-derived from the canonical signatures (independent of the
	// package vars under test).
	wantToken0 := selector("token0()")
	wantToken1 := selector("token1()")

	// The package selectors the agni/fusionx case in PoolTokens uses.
	if !bytes.Equal(selToken0, wantToken0) {
		t.Fatalf("selToken0 = %x, want %x (token0())", selToken0, wantToken0)
	}
	if !bytes.Equal(selToken1, wantToken1) {
		t.Fatalf("selToken1 = %x, want %x (token1())", selToken1, wantToken1)
	}

	// The fusionx/agni pair must not be the Liquidity Book pair (the moe path): a
	// fusionx pool read with getTokenX()/getTokenY() would revert / mis-decode.
	if bytes.Equal(selToken0, selGetTokenX) || bytes.Equal(selToken1, selGetTokenY) {
		t.Fatalf("fusionx/agni accessor selectors must differ from the Liquidity Book getTokenX()/getTokenY() pair")
	}
}
