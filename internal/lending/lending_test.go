package lending

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// Hermetic tests (no DB, no RPC) for the pure Aave V3 decoders and the display-only USD
// sizing. The decode tests assert the indexed-vs-data split (the Aave gotcha: `user` is
// not indexed in Borrow), the 32-byte word arithmetic on high-bit amounts, and address
// extraction from both topics and data words.

// word32 left-pads a hex string (no 0x) to a 32-byte ABI word.
func word32(hexNo0x string) string {
	return fmt.Sprintf("%064s", hexNo0x)
}

// addrTopic builds a 32-byte indexed-address topic: the 20-byte address
// right-aligned (ABI padding) with a 0x prefix, exactly as go-ethereum emits an
// indexed address.
func addrTopic(addr string) string {
	a := strings.ToLower(strings.TrimPrefix(addr, "0x"))
	return "0x" + word32(a)
}

// uintWord encodes a non-negative integer (given as a decimal string) as a 32-byte
// ABI word of hex.
func uintWord(decStr string) string {
	n, ok := new(big.Int).SetString(decStr, 10)
	if !ok {
		panic("uintWord: bad decimal " + decStr)
	}
	return word32(fmt.Sprintf("%x", n))
}

// addrWord encodes an address as a 32-byte ABI word (right-aligned), i.e. how an
// address appears inside log data (not as an indexed topic).
func addrWord(addr string) string {
	a := strings.ToLower(strings.TrimPrefix(addr, "0x"))
	return word32(a)
}

// boolWord encodes an ABI bool word (0 or 1).
func boolWord(b bool) string {
	if b {
		return word32("1")
	}
	return word32("0")
}

// concat joins ABI words into a single 0x-prefixed data string.
func concat(words ...string) string {
	return "0x" + strings.Join(words, "")
}

// Sample verified reserve addresses (lowercase) reused across the tests. These must
// match the registry table in lending.go; a drift here proves divergence.
const (
	addrUSDT0 = "0x779ded0c9e1022225f8e0630b35a9b54be713736" // 6 decimals, stable
	addrUSDC  = "0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9" // 6 decimals, stable
	addrUSDe  = "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34" // 18 decimals, stable
	addrFBTC  = "0xc96de26018a54d51c097160568752c4e3bd6c364" // 8 decimals, non-stable
	addrWMNT  = "0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8" // 18 decimals, non-stable
)

// sampleUser / sampleLiquidator / sampleOnBehalf are arbitrary distinct EOAs.
const (
	sampleUser       = "0x1111111111111111111111111111111111111111"
	sampleLiquidator = "0x2222222222222222222222222222222222222222"
	sampleOnBehalf   = "0x3333333333333333333333333333333333333333"
)

func TestIsAaveTopic(t *testing.T) {
	if !IsAaveTopic(TopicLiquidationCall) {
		t.Error("LiquidationCall topic should be recognized")
	}
	if !IsAaveTopic(TopicBorrow) {
		t.Error("Borrow topic should be recognized")
	}
	// Case-insensitive (raw_logs stores lowercase, but defend an upper variant).
	if !IsAaveTopic(strings.ToUpper(TopicBorrow)) {
		t.Error("Borrow topic should match case-insensitively")
	}
	// A swap topic (UniV2) must not be classified as Aave.
	if IsAaveTopic("0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822") {
		t.Error("a non-Aave topic must not be recognized")
	}
	if IsAaveTopic("") {
		t.Error("empty topic must not be recognized")
	}
}

// A real-shaped LiquidationCall decodes every field from the correct slot: the
// three indexed assets/user from topics, and debtToCover / liquidatedCollateral /
// liquidator / receiveAToken from data words 0..3.
func TestDecodeLiquidationCall_RealShape(t *testing.T) {
	// 50,000 USDT0 debt covered (6 decimals -> 50_000_000000 raw) and 1.5 FBTC
	// collateral seized (8 decimals -> 150_000000 raw).
	debtRaw := "50000000000" // 50,000 * 10^6
	collRaw := "150000000"   // 1.5 * 10^8
	data := concat(
		uintWord(debtRaw),          // word0: debtToCover
		uintWord(collRaw),          // word1: liquidatedCollateralAmount
		addrWord(sampleLiquidator), // word2: liquidator
		boolWord(true),             // word3: receiveAToken
	)

	lc, err := DecodeLiquidationCall(
		addrTopic(addrFBTC),   // collateralAsset (t1)
		addrTopic(addrUSDT0),  // debtAsset (t2)
		addrTopic(sampleUser), // user (t3)
		data,
	)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if lc.CollateralAsset != addrFBTC {
		t.Errorf("collateralAsset = %s, want %s", lc.CollateralAsset, addrFBTC)
	}
	if lc.DebtAsset != addrUSDT0 {
		t.Errorf("debtAsset = %s, want %s", lc.DebtAsset, addrUSDT0)
	}
	if lc.User != sampleUser {
		t.Errorf("user = %s, want %s", lc.User, sampleUser)
	}
	if !lc.DebtToCover.Equal(dec(debtRaw)) {
		t.Errorf("debtToCover = %s, want %s", lc.DebtToCover, debtRaw)
	}
	if !lc.LiquidatedCollateralAmount.Equal(dec(collRaw)) {
		t.Errorf("liquidatedCollateralAmount = %s, want %s", lc.LiquidatedCollateralAmount, collRaw)
	}
	if lc.Liquidator != sampleLiquidator {
		t.Errorf("liquidator = %s, want %s", lc.Liquidator, sampleLiquidator)
	}
	if !lc.ReceiveAToken {
		t.Error("receiveAToken = false, want true")
	}
}

// A very large debtToCover that sets the high bit of the 256-bit word must decode
// as a positive unsigned integer (Aave amounts are unsigned; the decoder must not
// two's-complement-negate it).
func TestDecodeLiquidationCall_LargeUnsignedAmount(t *testing.T) {
	// 2^255 + 7: the top bit is set. As an unsigned uint256 this is a huge positive.
	big255 := new(big.Int).Lsh(big.NewInt(1), 255)
	big255.Add(big255, big.NewInt(7))
	debtRaw := big255.String()

	data := concat(
		uintWord(debtRaw),
		uintWord("1"),
		addrWord(sampleLiquidator),
		boolWord(false),
	)
	lc, err := DecodeLiquidationCall(addrTopic(addrFBTC), addrTopic(addrUSDe), addrTopic(sampleUser), data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if lc.DebtToCover.Sign() <= 0 {
		t.Fatalf("high-bit amount decoded non-positive (%s); must be unsigned", lc.DebtToCover)
	}
	if !lc.DebtToCover.Equal(dec(debtRaw)) {
		t.Fatalf("debtToCover = %s, want %s (unsigned)", lc.DebtToCover, debtRaw)
	}
	if lc.ReceiveAToken {
		t.Error("receiveAToken = true, want false")
	}
}

// A missing indexed topic (truncated/foreign log) is an error, not a zero-address
// decode.
func TestDecodeLiquidationCall_MissingTopicErrors(t *testing.T) {
	data := concat(uintWord("1"), uintWord("1"), addrWord(sampleLiquidator), boolWord(false))
	if _, err := DecodeLiquidationCall("", addrTopic(addrUSDe), addrTopic(sampleUser), data); err == nil {
		t.Error("a missing collateralAsset topic should be an error")
	}
}

// Short data (fewer than 4 words) is an error rather than a panic or partial decode.
func TestDecodeLiquidationCall_ShortDataErrors(t *testing.T) {
	short := concat(uintWord("1"), uintWord("1")) // only 2 words
	if _, err := DecodeLiquidationCall(addrTopic(addrFBTC), addrTopic(addrUSDe), addrTopic(sampleUser), short); err == nil {
		t.Error("short liquidation data should be an error")
	}
}

// A real-shaped Borrow decodes reserve/onBehalfOf/referralCode from the indexed
// topics and, critically, `user` from the first data word (it is not indexed),
// then amount / interestRateMode / borrowRate from data words 1..3.
func TestDecodeBorrow_RealShape_UserIsInData(t *testing.T) {
	amountRaw := "75000000000" // 75,000 USDT0 (6 decimals)
	data := concat(
		addrWord(sampleUser), // word0: user (not indexed; the gotcha)
		uintWord(amountRaw),  // word1: amount
		uintWord("2"),        // word2: interestRateMode = variable
		uintWord("12345"),    // word3: borrowRate (ray; value irrelevant to the test)
	)

	b, err := DecodeBorrow(
		addrTopic(addrUSDT0),      // reserve (t1)
		addrTopic(sampleOnBehalf), // onBehalfOf (t2)
		"0x"+word32("0"),          // referralCode (t3) = 0
		data,
	)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if b.Reserve != addrUSDT0 {
		t.Errorf("reserve = %s, want %s", b.Reserve, addrUSDT0)
	}
	if b.OnBehalfOf != sampleOnBehalf {
		t.Errorf("onBehalfOf = %s, want %s", b.OnBehalfOf, sampleOnBehalf)
	}
	if b.ReferralCode != 0 {
		t.Errorf("referralCode = %d, want 0", b.ReferralCode)
	}
	// The gotcha: user must come from data word 0, not a topic.
	if b.User != sampleUser {
		t.Errorf("user = %s, want %s (user is the first DATA word, not indexed)", b.User, sampleUser)
	}
	if !b.Amount.Equal(dec(amountRaw)) {
		t.Errorf("amount = %s, want %s", b.Amount, amountRaw)
	}
	if b.InterestRateMode != 2 {
		t.Errorf("interestRateMode = %d, want 2 (variable)", b.InterestRateMode)
	}
}

// A non-zero referralCode round-trips through the uint16 topic decode.
func TestDecodeBorrow_ReferralCode(t *testing.T) {
	data := concat(addrWord(sampleUser), uintWord("1"), uintWord("1"), uintWord("0"))
	b, err := DecodeBorrow(addrTopic(addrUSDe), addrTopic(sampleOnBehalf), "0x"+word32("ffff"), data)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if b.ReferralCode != 0xFFFF {
		t.Errorf("referralCode = %d, want 65535", b.ReferralCode)
	}
	if b.InterestRateMode != 1 {
		t.Errorf("interestRateMode = %d, want 1 (stable)", b.InterestRateMode)
	}
}

// Short data is an error, not a panic.
func TestDecodeBorrow_ShortDataErrors(t *testing.T) {
	short := concat(addrWord(sampleUser), uintWord("1")) // only 2 words
	if _, err := DecodeBorrow(addrTopic(addrUSDe), addrTopic(sampleOnBehalf), "0x"+word32("0"), short); err == nil {
		t.Error("short borrow data should be an error")
	}
}

// A missing reserve topic is an error.
func TestDecodeBorrow_MissingTopicErrors(t *testing.T) {
	data := concat(addrWord(sampleUser), uintWord("1"), uintWord("1"), uintWord("0"))
	if _, err := DecodeBorrow("", addrTopic(sampleOnBehalf), "0x"+word32("0"), data); err == nil {
		t.Error("a missing reserve topic should be an error")
	}
}

func TestReserveDecimals(t *testing.T) {
	cases := []struct {
		addr string
		want int
		ok   bool
	}{
		{addrUSDT0, 6, true},
		{addrUSDC, 6, true},
		{addrUSDe, 18, true},
		{addrFBTC, 8, true},
		{strings.ToUpper(addrUSDC), 6, true}, // case-insensitive
		{"0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := ReserveDecimals(c.addr)
		if ok != c.ok || got != c.want {
			t.Errorf("ReserveDecimals(%s) = (%d,%v), want (%d,%v)", c.addr, got, ok, c.want, c.ok)
		}
	}
}

func TestIsStableReserve(t *testing.T) {
	for _, a := range []string{addrUSDT0, addrUSDC, addrUSDe} {
		if !IsStableReserve(a) {
			t.Errorf("%s should be a known stable reserve", a)
		}
	}
	// FBTC and WMNT are in the decimals table but are not dollar stables.
	if IsStableReserve(addrFBTC) {
		t.Error("FBTC must not be a stable reserve")
	}
	if IsStableReserve(addrWMNT) {
		t.Error("WMNT must not be a stable reserve")
	}
	if IsStableReserve("") {
		t.Error("empty must not be a stable reserve")
	}
}

// A stable reserve sizes to the decimals-adjusted amount; 50,000 USDT0 at 6
// decimals (50_000_000000 raw) -> 50000.
func TestStableSizeUSD_StableScales(t *testing.T) {
	got := StableSizeUSD(addrUSDT0, dec("50000000000"))
	if got == nil {
		t.Fatal("expected a size_usd for a stable reserve, got nil")
	}
	if !got.Equal(dec("50000")) {
		t.Fatalf("size_usd = %s, want 50000", got.String())
	}
	// USDe is 18 decimals: 1.5 USDe -> 1500000000000000000 raw -> 1.5.
	usde := StableSizeUSD(addrUSDe, dec("1500000000000000000"))
	if usde == nil || !usde.Equal(dec("1.5")) {
		t.Fatalf("USDe size_usd = %v, want 1.5", usde)
	}
}

// A non-stable reserve (FBTC) yields nil; we never guess a volatile asset's USD
// value without a price.
func TestStableSizeUSD_NonStableIsNil(t *testing.T) {
	if got := StableSizeUSD(addrFBTC, dec("150000000")); got != nil {
		t.Fatalf("non-stable reserve should yield nil size_usd, got %s", got.String())
	}
	if got := StableSizeUSD("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", dec("1")); got != nil {
		t.Fatalf("unknown reserve should yield nil size_usd, got %s", got.String())
	}
}

// A negative amount on a stable side is rendered non-negative (Abs), defending a
// caller that passes a signed value by mistake.
func TestStableSizeUSD_NegativeAbs(t *testing.T) {
	got := StableSizeUSD(addrUSDC, dec("-2500000")) // -2.5 USDC at 6 decimals
	if got == nil {
		t.Fatal("expected a size_usd, got nil")
	}
	if !got.Equal(dec("2.5")) {
		t.Fatalf("size_usd = %s, want 2.5 (abs)", got.String())
	}
}

// dec builds a decimal from a string (test helper).
func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}
