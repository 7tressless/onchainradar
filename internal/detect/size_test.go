package detect

import (
	"testing"

	"github.com/shopspring/decimal"

	"ocr/internal/store"
)

// These tests cover the pure display-only USD-size derivation (stableSizeUSD,
// swapSizeUSD, bucketSizeUSD, poolPrice). They are fully hermetic (no DB, no RPC) and
// assert the value the API surfaces, nothing about firing.

// Verified Mantle stablecoin addresses from config/pools.yaml (lowercase hex). They
// must match the registry set in size.go; a drift here would prove the production
// set diverged from the verified registry.
const (
	addrUSDe = "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34"
	addrUSDC = "0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9"
	addrUSDT = "0x201eba5cc46d216ce6dc03f6a759e8e766e956ae"
	addrWMNT = "0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8" // a non-stable anchor (gas token)
)

// dec is a small helper to build a decimal from an integer-ish string in tests.
func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// stable=token0: size_usd = |gross0| / 10^dec0. A 6-decimal USDC swap of 1_500_000
// raw units is $1.5 (the magnitude is the stable side; the WMNT side is ignored).
func TestStableSizeUSD_Token0Stable(t *testing.T) {
	pool := store.Pool{
		Token0: addrUSDC, Dec0: 6, // stable side
		Token1: addrWMNT, Dec1: 18,
	}
	// gross0 = 1_500_000 raw USDC (6 decimals) -> 1.5 ; gross1 is a large WMNT amount
	// that must not be used.
	got := stableSizeUSD(pool, dec("1500000"), dec("999000000000000000000"))
	if got == nil {
		t.Fatal("expected a size_usd for a stable token0, got nil")
	}
	if !got.Equal(dec("1.5")) {
		t.Fatalf("size_usd = %s, want 1.5", got.String())
	}
}

// stable=token1: size_usd = |gross1| / 10^dec1. USDe is 18 decimals; 2 whole USDe is
// 2_000000000000000000 raw -> 2. The non-stable token0 side is ignored.
func TestStableSizeUSD_Token1Stable(t *testing.T) {
	pool := store.Pool{
		Token0: addrWMNT, Dec0: 18,
		Token1: addrUSDe, Dec1: 18, // stable side
	}
	got := stableSizeUSD(pool, dec("123456789000000000000"), dec("2000000000000000000"))
	if got == nil {
		t.Fatal("expected a size_usd for a stable token1, got nil")
	}
	if !got.Equal(dec("2")) {
		t.Fatalf("size_usd = %s, want 2", got.String())
	}
}

// Neither side is a known stablecoin -> nil (do not guess; the frontend renders
// null). A WMNT/WMNT-like pool with no dollar leg cannot be approximated without an
// external price, which we do not fetch.
func TestStableSizeUSD_NoStableSideIsNil(t *testing.T) {
	pool := store.Pool{
		Token0: addrWMNT, Dec0: 18,
		Token1: "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", Dec1: 8, // some non-stable
	}
	if got := stableSizeUSD(pool, dec("5000000000000000000"), dec("100000000")); got != nil {
		t.Fatalf("non-stable pool should yield nil size_usd, got %s", got.String())
	}
}

// Both sides stable (a stable/stable pool, e.g. USDC/USDe): token0 is preferred. The
// value uses token0's amount + decimals; either side is about a dollar so the
// magnitude is the same order regardless, but the selection must be deterministic.
func TestStableSizeUSD_BothStablePrefersToken0(t *testing.T) {
	pool := store.Pool{
		Token0: addrUSDC, Dec0: 6, // 6-decimal stable
		Token1: addrUSDe, Dec1: 18, // 18-decimal stable
	}
	// token0 amount = 1_000_000 raw USDC (6 dec) -> 1 ; token1 amount differs (would be
	// ~7 if it were used) to prove token0 wins.
	got := stableSizeUSD(pool, dec("1000000"), dec("7000000000000000000"))
	if got == nil {
		t.Fatal("both-stable pool should yield a size_usd, got nil")
	}
	if !got.Equal(dec("1")) {
		t.Fatalf("both-stable size_usd = %s, want 1 (token0 preferred)", got.String())
	}
}

// A signed (negative) amount on the stable side is still rendered as a NON-negative
// size (Abs), so a caller that passes a signed net by mistake never produces a
// negative USD figure.
func TestStableSizeUSD_NegativeAmountAbs(t *testing.T) {
	pool := store.Pool{
		Token0: addrUSDT, Dec0: 6, // stable
		Token1: addrWMNT, Dec1: 18,
	}
	got := stableSizeUSD(pool, dec("-2500000"), dec("0"))
	if got == nil {
		t.Fatal("expected a size_usd, got nil")
	}
	if !got.Equal(dec("2.5")) {
		t.Fatalf("size_usd = %s, want 2.5 (abs of a signed amount)", got.String())
	}
}

// isStableToken matches the verified set case-insensitively and rejects unknowns.
func TestIsStableToken(t *testing.T) {
	for _, a := range []string{addrUSDe, addrUSDC, addrUSDT} {
		if !isStableToken(a) {
			t.Errorf("%s should be a known stable", a)
		}
		// Case-insensitive: an upper-cased variant must also match.
		if !isStableToken("0x" + upper(a[2:])) {
			t.Errorf("%s (upper) should be a known stable", a)
		}
	}
	if isStableToken(addrWMNT) {
		t.Error("WMNT must not be a stable")
	}
	if isStableToken("") {
		t.Error("empty address must not be a stable")
	}
}

// upper upper-cases an ASCII hex string (test-only; avoids importing strings just
// for one call in the case-insensitivity assertion).
func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - ('a' - 'A')
		}
	}
	return string(b)
}

// swapSizeUSD adds a market-price fallback for pools with no stable side: the token0 leg
// valued at price0. 2 WMNT (18 dec) at $0.52 -> 1.04; the token1 side is not used.
func TestSwapSizeUSD_MarketFallback(t *testing.T) {
	pool := store.Pool{
		Token0: addrWMNT, Dec0: 18, // non-stable
		Token1: "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", Dec1: 8,
	}
	price0 := dec("0.52")
	got := swapSizeUSD(pool, dec("2000000000000000000"), dec("12345"), &price0)
	if got == nil || !got.Equal(dec("1.04")) {
		t.Fatalf("size_usd = %v, want 1.04 (2 WMNT * $0.52)", got)
	}
}

// The stable side stays preferred even when a market price is present: a dollar stable
// needs no external price, so the own-data $1 rule wins and price0 is ignored.
func TestSwapSizeUSD_StablePreferredOverMarket(t *testing.T) {
	pool := store.Pool{
		Token0: addrUSDC, Dec0: 6, // stable
		Token1: addrWMNT, Dec1: 18,
	}
	price0 := dec("999999") // absurd, must be ignored because token0 is stable
	got := swapSizeUSD(pool, dec("1500000"), dec("0"), &price0)
	if got == nil || !got.Equal(dec("1.5")) {
		t.Fatalf("size_usd = %v, want 1.5 (stable side, price0 ignored)", got)
	}
}

// No stable side and no usable price -> nil: we never guess. A non-positive price is
// treated as absent (no zero/negative size).
func TestSwapSizeUSD_NoStableNoPriceIsNil(t *testing.T) {
	pool := store.Pool{
		Token0: addrWMNT, Dec0: 18,
		Token1: "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", Dec1: 8,
	}
	if got := swapSizeUSD(pool, dec("5000000000000000000"), dec("100000000"), nil); got != nil {
		t.Fatalf("no stable + no price should be nil, got %s", got.String())
	}
	zero := dec("0")
	if got := swapSizeUSD(pool, dec("5000000000000000000"), dec("100000000"), &zero); got != nil {
		t.Fatalf("non-positive price should be nil, got %s", got.String())
	}
}

// bucketSizeUSD values a bucket's gross token0 volume at token0's market price: 100 WMNT
// (18 dec) at $0.52 -> 52. nil when the price is absent.
func TestBucketSizeUSD(t *testing.T) {
	pool := store.Pool{Token0: addrWMNT, Dec0: 18}
	price0 := dec("0.52")
	got := bucketSizeUSD(pool, dec("100000000000000000000"), &price0)
	if got == nil || !got.Equal(dec("52")) {
		t.Fatalf("bucket size = %v, want 52 (100 * 0.52)", got)
	}
	if got := bucketSizeUSD(pool, dec("100000000000000000000"), nil); got != nil {
		t.Fatalf("no price should yield nil bucket size, got %s", got.String())
	}
}

// poolPrice looks a pool's price out of the map case-insensitively, or nil when absent.
func TestPoolPrice(t *testing.T) {
	prices := map[string]decimal.Decimal{"0xabc": dec("12.5")}
	if got := poolPrice(prices, "0xABC"); got == nil || !got.Equal(dec("12.5")) {
		t.Fatalf("poolPrice(0xABC) = %v, want 12.5", got)
	}
	if got := poolPrice(prices, "0xnope"); got != nil {
		t.Fatalf("absent pool should be nil, got %s", got.String())
	}
}
