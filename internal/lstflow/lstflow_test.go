package lstflow

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// These tests cover the pure ERC-20 Transfer decoder and the counterparty
// classification. They are fully hermetic (no DB, no RPC). The decode tests assert
// the indexed-vs-data split (from/to are indexed topics; value is the first data
// word), the 32-byte word arithmetic (a large value that exercises the high bit),
// and address extraction from both indexed topics. The classification tests assert
// mint (from==0x0), burn (to==0x0), move (wallet-to-wallet), and the key noise filter
// skip (either side is a tracked DEX pool).

// Log-shaping helpers.

// word32 left-pads a hex string (no 0x) to a 32-byte ABI word.
func word32(hexNo0x string) string {
	return fmt.Sprintf("%064s", hexNo0x)
}

// addrTopic builds a 32-byte indexed-address topic (the 20-byte address right-aligned
// with a 0x prefix), exactly as go-ethereum emits an indexed address.
func addrTopic(addr string) string {
	a := strings.ToLower(strings.TrimPrefix(addr, "0x"))
	return "0x" + word32(a)
}

// uintWord encodes a non-negative integer (decimal string) as a 32-byte ABI word.
func uintWord(decStr string) string {
	n, ok := new(big.Int).SetString(decStr, 10)
	if !ok {
		panic("uintWord: bad decimal " + decStr)
	}
	return word32(fmt.Sprintf("%x", n))
}

// dec builds a decimal from a string (test helper).
func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// Arbitrary distinct EOAs / a sample pool.
const (
	sampleFrom = "0x1111111111111111111111111111111111111111"
	sampleTo   = "0x2222222222222222222222222222222222222222"
	samplePool = "0x3333333333333333333333333333333333333333"
)

// registry / topic

func TestLSTTokensAndSymbols(t *testing.T) {
	toks := LSTTokens()
	if len(toks) != 2 {
		t.Fatalf("LSTTokens len = %d, want 2", len(toks))
	}
	// Both must be the verified lowercase addresses.
	if toks[0] != METHAddress || toks[1] != CMETHAddress {
		t.Errorf("LSTTokens = %v, want [%s %s]", toks, METHAddress, CMETHAddress)
	}
	// The returned slice must be a copy: mutating it must not affect the next call.
	toks[0] = "0xdead"
	if LSTTokens()[0] != METHAddress {
		t.Error("LSTTokens must return a fresh slice each call (state leaked)")
	}

	if TokenSymbol(METHAddress) != "mETH" {
		t.Errorf("TokenSymbol(mETH) = %q, want mETH", TokenSymbol(METHAddress))
	}
	if TokenSymbol(CMETHAddress) != "cmETH" {
		t.Errorf("TokenSymbol(cmETH) = %q, want cmETH", TokenSymbol(CMETHAddress))
	}
	// Case-insensitive.
	if TokenSymbol(strings.ToUpper(METHAddress)) != "mETH" {
		t.Error("TokenSymbol should match case-insensitively")
	}
	if TokenSymbol("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef") != "" {
		t.Error("an unknown token must yield an empty symbol")
	}
	if !IsLSTToken(CMETHAddress) || IsLSTToken("") {
		t.Error("IsLSTToken classification wrong")
	}
}

func TestIsTransferTopic(t *testing.T) {
	if !IsTransferTopic(TopicTransfer) {
		t.Error("Transfer topic should be recognized")
	}
	// Case-insensitive (raw_logs stores lowercase, but defend an upper variant).
	if !IsTransferTopic(strings.ToUpper(TopicTransfer)) {
		t.Error("Transfer topic should match case-insensitively")
	}
	// A swap topic (UniV2) must not be classified as a Transfer.
	if IsTransferTopic("0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822") {
		t.Error("a non-Transfer topic must not be recognized")
	}
	if IsTransferTopic("") {
		t.Error("empty topic must not be recognized")
	}
}

// DecodeTransfer

// A real-shaped Transfer decodes from/to from the indexed topics and value from the
// first data word, with the token address from raw_logs.address.
func TestDecodeTransfer_RealShape(t *testing.T) {
	// 12.5 mETH at 18 decimals.
	valueRaw := "12500000000000000000"
	tr, err := DecodeTransfer(METHAddress, addrTopic(sampleFrom), addrTopic(sampleTo), "0x"+uintWord(valueRaw))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if tr.Token != METHAddress {
		t.Errorf("token = %s, want %s", tr.Token, METHAddress)
	}
	if tr.From != sampleFrom {
		t.Errorf("from = %s, want %s", tr.From, sampleFrom)
	}
	if tr.To != sampleTo {
		t.Errorf("to = %s, want %s", tr.To, sampleTo)
	}
	if !tr.Value.Equal(dec(valueRaw)) {
		t.Errorf("value = %s, want %s", tr.Value, valueRaw)
	}
}

// A very large value that sets the high bit of the 256-bit word must decode as a
// positive unsigned integer (ERC-20 amounts are unsigned; no two's-complement negate).
func TestDecodeTransfer_LargeUnsignedValue(t *testing.T) {
	big255 := new(big.Int).Lsh(big.NewInt(1), 255)
	big255.Add(big255, big.NewInt(7))
	valueRaw := big255.String()

	tr, err := DecodeTransfer(CMETHAddress, addrTopic(sampleFrom), addrTopic(sampleTo), "0x"+uintWord(valueRaw))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if tr.Value.Sign() <= 0 {
		t.Fatalf("high-bit value decoded non-positive (%s); must be unsigned", tr.Value)
	}
	if !tr.Value.Equal(dec(valueRaw)) {
		t.Fatalf("value = %s, want %s (unsigned)", tr.Value, valueRaw)
	}
}

// A mint (from == zero) and a burn (to == zero) decode their zero side correctly.
func TestDecodeTransfer_MintBurnSides(t *testing.T) {
	mint, err := DecodeTransfer(METHAddress, addrTopic(ZeroAddress), addrTopic(sampleTo), "0x"+uintWord("1"))
	if err != nil {
		t.Fatalf("mint decode failed: %v", err)
	}
	if mint.From != ZeroAddress {
		t.Errorf("mint from = %s, want zero address", mint.From)
	}
	burn, err := DecodeTransfer(METHAddress, addrTopic(sampleFrom), addrTopic(ZeroAddress), "0x"+uintWord("1"))
	if err != nil {
		t.Fatalf("burn decode failed: %v", err)
	}
	if burn.To != ZeroAddress {
		t.Errorf("burn to = %s, want zero address", burn.To)
	}
}

// A missing indexed topic (truncated/foreign log) is an error, not a zero-address
// decode.
func TestDecodeTransfer_MissingTopicErrors(t *testing.T) {
	if _, err := DecodeTransfer(METHAddress, "", addrTopic(sampleTo), "0x"+uintWord("1")); err == nil {
		t.Error("a missing from topic should be an error")
	}
	if _, err := DecodeTransfer(METHAddress, addrTopic(sampleFrom), "", "0x"+uintWord("1")); err == nil {
		t.Error("a missing to topic should be an error")
	}
}

// Empty data (no value word) is an error rather than a panic or a zero value.
func TestDecodeTransfer_EmptyDataErrors(t *testing.T) {
	if _, err := DecodeTransfer(METHAddress, addrTopic(sampleFrom), addrTopic(sampleTo), ""); err == nil {
		t.Error("empty transfer data should be an error")
	}
	if _, err := DecodeTransfer(METHAddress, addrTopic(sampleFrom), addrTopic(sampleTo), "0x"); err == nil {
		t.Error("0x-only transfer data should be an error")
	}
}

// classification

// Classify covers mint / burn / move and the DEX-pool skip (the noise filter).
func TestClassify(t *testing.T) {
	pools := map[string]struct{}{samplePool: {}}

	cases := []struct {
		name string
		from string
		to   string
		want Direction
	}{
		{"mint-from-zero", ZeroAddress, sampleTo, DirectionMint},
		{"burn-to-zero", sampleFrom, ZeroAddress, DirectionBurn},
		{"move-wallet-to-wallet", sampleFrom, sampleTo, DirectionMove},
		{"skip-pool-on-from", samplePool, sampleTo, DirectionSkip},
		{"skip-pool-on-to", sampleFrom, samplePool, DirectionSkip},
		// The pool check has priority over mint/burn: a mint whose recipient is a pool
		// is still a swap leg and must be skipped.
		{"pool-priority-over-mint", ZeroAddress, samplePool, DirectionSkip},
		{"pool-priority-over-burn", samplePool, ZeroAddress, DirectionSkip},
	}
	for _, c := range cases {
		if got := Classify(c.from, c.to, pools); got != c.want {
			t.Errorf("%s: Classify(%s,%s) = %q, want %q", c.name, c.from, c.to, got, c.want)
		}
	}

	// Case-insensitive pool membership: an upper-cased pool address still matches.
	if Classify(strings.ToUpper(samplePool), sampleTo, pools) != DirectionSkip {
		t.Error("pool membership should be case-insensitive")
	}
	// A nil pool set excludes nothing: a normal transfer is a move.
	if Classify(sampleFrom, sampleTo, nil) != DirectionMove {
		t.Error("nil pool set should classify a wallet transfer as move")
	}
}

// CounterpartyEOA returns the right fallback actor per direction.
func TestCounterpartyEOA(t *testing.T) {
	pools := map[string]struct{}{samplePool: {}}

	// Mint: From is zero, so the recipient is the actor.
	if got := CounterpartyEOA(ZeroAddress, sampleTo, pools); got != sampleTo {
		t.Errorf("mint counterparty = %q, want recipient %q", got, sampleTo)
	}
	// Burn: To is zero, so the sender is the actor.
	if got := CounterpartyEOA(sampleFrom, ZeroAddress, pools); got != sampleFrom {
		t.Errorf("burn counterparty = %q, want sender %q", got, sampleFrom)
	}
	// Move: prefer the sender.
	if got := CounterpartyEOA(sampleFrom, sampleTo, pools); got != sampleFrom {
		t.Errorf("move counterparty = %q, want sender %q", got, sampleFrom)
	}
	// A pool side is skipped in favour of the real EOA on the other side.
	if got := CounterpartyEOA(samplePool, sampleTo, pools); got != sampleTo {
		t.Errorf("counterparty with a pool sender = %q, want the EOA recipient %q", got, sampleTo)
	}
	// Both degenerate (both zero) -> "".
	if got := CounterpartyEOA(ZeroAddress, ZeroAddress, pools); got != "" {
		t.Errorf("both-zero counterparty = %q, want empty", got)
	}
}
