// Package lstflow decodes large Liquid-Staking-Token (mETH, cmETH) ERC-20 Transfers on
// Mantle: a verified token + Transfer-topic registry, a pure decoder, and the
// mint/burn/move counterparty classification the internal/detect LST-flow detector
// (type 4) consumes. It does no I/O and never touches the swap or lending paths; the LST
// tokens are not tradable pools, so their Transfer logs never pollute the swap buckets.
//
// The registry is verified on-chain, each address and the keccak-derived Transfer topic
// documented on its const below. Do not edit it without re-verifying, and never invent an
// on-chain address or event signature. Everything is lowercased to match raw_logs.
package lstflow

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"ocr/internal/lending"
)

// METHAddress / CMETHAddress are the two Mantle (chainId 5000) Liquid-Staking-Token
// ERC-20 contracts this detector follows. Stored lowercase to match raw_logs'
// lowercase-hex storage convention.
const (
	// METHAddress is mETH (mETHL2), 18 decimals.
	METHAddress = "0xcda86a272531e8640cd7f1a92c01839911b90bb0"
	// CMETHAddress is cmETH, 18 decimals.
	CMETHAddress = "0xe6829d9a7ee3040e1276fa75293bde931859e8fa"
)

// TokenDecimals is the verified decimals of both LST tokens. mETH and cmETH are both
// 18-decimal ERC-20s, so the value word is scaled by 10^18 to whole tokens.
const TokenDecimals = 18

// TopicTransfer is the ERC-20 Transfer event topic0 (keccak256 of
// "Transfer(address,address,uint256)"), the standard topic every ERC-20 emits.
// Lowercase to match the lowercased topic0 in raw_logs.
const TopicTransfer = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// ZeroAddress is the ERC-20 mint/burn sentinel: a Transfer from it is a mint, a
// Transfer to it is a burn. Lowercase to compare against the decoded addresses.
const ZeroAddress = "0x0000000000000000000000000000000000000000"

// LSTTokens returns the lowercase addresses of the tracked LST tokens, as a fresh
// slice each call so a caller cannot mutate package state. The collector adds these to
// its filter; the detector passes them to RawLogsSince.
func LSTTokens() []string {
	return []string{METHAddress, CMETHAddress}
}

// TokenSymbol returns the human symbol ("mETH" / "cmETH") for a known LST token
// address (case-insensitive), or "" when not tracked. Display-only; membership is by
// contract address.
func TokenSymbol(addr string) string {
	switch strings.ToLower(strings.TrimSpace(addr)) {
	case METHAddress:
		return "mETH"
	case CMETHAddress:
		return "cmETH"
	default:
		return ""
	}
}

// IsLSTToken reports whether addr is one of the tracked LST token contracts
// (case-insensitive).
func IsLSTToken(addr string) bool {
	return TokenSymbol(addr) != ""
}

// IsTransferTopic reports whether topic0 is the ERC-20 Transfer topic (case-
// insensitive), used to keep only Transfer logs (these contracts also emit Approval etc.).
func IsTransferTopic(topic0 string) bool {
	return strings.EqualFold(strings.TrimSpace(topic0), TopicTransfer)
}

// Transfer is a decoded ERC-20 Transfer of a tracked LST token: Value (raw 18-dec)
// moved from From to To. Addresses are lowercase hex; Value is exact (decimal.Decimal).
type Transfer struct {
	Token string          // the LST token contract that emitted the log (lowercase hex)
	From  string          // indexed topic1 (lowercase hex); ZeroAddress for a mint
	To    string          // indexed topic2 (lowercase hex); ZeroAddress for a burn
	Value decimal.Decimal // data word 0 (raw units of the token, 18-dec)
}

// DecodeTransfer decodes an ERC-20 Transfer log strictly per the verified layout:
//
//	indexed: from(topic1), to(topic2)
//	data:    value (uint256)
//
// token is the emitting LST contract (raw_logs.address). It errors (never panics) when
// an indexed topic is missing or the data has no value word, so a malformed log is
// skipped rather than mis-read. Addresses are lowercased; the value is unsigned. Word
// helpers are reused from internal/lending.
func DecodeTransfer(token, topic1, topic2, data string) (Transfer, error) {
	from, err := lending.AddrFromTopic(topic1)
	if err != nil {
		return Transfer{}, fmt.Errorf("lstflow: transfer from topic: %w", err)
	}
	to, err := lending.AddrFromTopic(topic2)
	if err != nil {
		return Transfer{}, fmt.Errorf("lstflow: transfer to topic: %w", err)
	}

	words, err := lending.SplitWords(data)
	if err != nil {
		return Transfer{}, fmt.Errorf("lstflow: transfer data: %w", err)
	}
	if len(words) < 1 {
		return Transfer{}, fmt.Errorf("lstflow: transfer expects >=1 data word, got %d", len(words))
	}

	return Transfer{
		Token: strings.ToLower(strings.TrimSpace(token)),
		From:  from,
		To:    to,
		Value: lending.DecodeUint256(words[0]),
	}, nil
}

// Direction is the classified counterparty kind of an LST Transfer.
type Direction string

const (
	// DirectionMint is a Transfer from the zero address: LST minted (staked / bridged
	// in), fresh ETH exposure entering Mantle. Bullish for Mantle ETH liquidity.
	DirectionMint Direction = "mint"
	// DirectionBurn is a Transfer to the zero address: LST burned (redeemed / bridged
	// out), ETH exposure leaving Mantle.
	DirectionBurn Direction = "burn"
	// DirectionMove is a wallet-to-wallet Transfer (neither side is the zero address
	// and neither side is a tracked DEX pool): OTC accumulation / treasury movement.
	DirectionMove Direction = "move"
	// DirectionSkip means the Transfer is a DEX-swap leg (one side is a tracked pool)
	// and must be dropped (already covered by the swap signals). The key noise filter.
	DirectionSkip Direction = "skip"
)

// Classify determines an LST Transfer's Direction from its counterparties and the set
// of tracked DEX pool addresses. The rules, in priority order:
//
//  1. either side is a tracked DEX pool  -> DirectionSkip (a swap leg; already covered
//     by our swap signals; the noise filter, applied first so a mint/burn that also
//     routes through a pool is still excluded).
//  2. from == zero address               -> DirectionMint (minted / bridged in).
//  3. to   == zero address               -> DirectionBurn (redeemed / bridged out).
//  4. otherwise                          -> DirectionMove (wallet-to-wallet / OTC).
//
// poolSet is the lowercase pool-address set (a nil/empty set excludes nothing). The
// pool check comes first so a pool that is somehow also the zero address (defended) is
// treated as a swap leg, not a mint/burn.
func Classify(from, to string, poolSet map[string]struct{}) Direction {
	f := strings.ToLower(strings.TrimSpace(from))
	t := strings.ToLower(strings.TrimSpace(to))

	if isPool(f, poolSet) || isPool(t, poolSet) {
		return DirectionSkip
	}
	if f == ZeroAddress {
		return DirectionMint
	}
	if t == ZeroAddress {
		return DirectionBurn
	}
	return DirectionMove
}

// isPool reports whether the lowercase addr is in the tracked-pool set (a nil set
// excludes nothing). The zero address reports false so it falls through to the
// mint/burn checks in Classify.
func isPool(addr string, poolSet map[string]struct{}) bool {
	if len(poolSet) == 0 {
		return false
	}
	if addr == ZeroAddress {
		return false
	}
	_, ok := poolSet[addr]
	return ok
}

// CounterpartyEOA returns the non-zero, non-pool counterparty: the best fallback
// "actor" when tx.origin is unavailable. It prefers the sender (From), falling back to
// the recipient (To, e.g. a mint). It returns "" only when both sides are degenerate
// (which Classify already routes away from a fired signal). Lowercase.
func CounterpartyEOA(from, to string, poolSet map[string]struct{}) string {
	f := strings.ToLower(strings.TrimSpace(from))
	t := strings.ToLower(strings.TrimSpace(to))

	// Prefer the sender when it is a real EOA (not zero, not a pool).
	if f != ZeroAddress && !isPool(f, poolSet) {
		return f
	}
	// Otherwise the recipient (e.g. a mint, where From is the zero address).
	if t != ZeroAddress && !isPool(t, poolSet) {
		return t
	}
	return ""
}
