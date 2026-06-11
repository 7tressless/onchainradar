// Package lending decodes Aave V3 lending-protocol events on Mantle: a verified
// protocol-contracts registry (Pool proxy + the two event topics, each documented on
// its const), and pure, allocation-light decoders. It performs no I/O and never touches
// the DEX swap path; the lending detector (internal/detect) consumes these to emit types
// 5 (liquidation) and 6 (big_borrow).
package lending

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
)

// AavePoolAddress is the Aave V3 Pool proxy on Mantle (chainId 5000) that emits the
// Borrow and LiquidationCall events. Lowercase to match raw_logs' storage convention.
// Source: Aave address book + MantleScan. Do not edit without re-verifying.
const AavePoolAddress = "0x458f293454fe0d67ec0655f3672301301dd51422"

// Event topic0 hashes, keccak256 of each canonical signature, lowercased to match the
// lowercased topic0 stored in raw_logs. Do not edit without re-deriving from the
// signature.
const (
	// TopicLiquidationCall is
	// LiquidationCall(address collateralAsset, address debtAsset, address user,
	//                 uint256 debtToCover, uint256 liquidatedCollateralAmount,
	//                 address liquidator, bool receiveAToken).
	// indexed: collateralAsset(t1), debtAsset(t2), user(t3);
	// data:    debtToCover, liquidatedCollateralAmount, liquidator, receiveAToken.
	TopicLiquidationCall = "0xe413a321e8681d831f4dbccbca790d2952b56f977908e45be37335533e005286"

	// TopicBorrow is
	// Borrow(address reserve, address user, address onBehalfOf, uint256 amount,
	//        uint8 interestRateMode, uint256 borrowRate, uint16 referralCode).
	// indexed: reserve(t1), onBehalfOf(t2), referralCode(t3); `user` is not
	//          indexed, so it is the first data word.
	// data:    user, amount, interestRateMode, borrowRate.
	TopicBorrow = "0xb3d084820fb1a9decffb176436bd02558d15fac9b0ddfed8c465bc7359d7dce0"
)

// IsAaveTopic reports whether topic0 is one of the Aave V3 Pool events OCR decodes
// (case-insensitive), like aggregate.IsSwapTopic for swap logs.
func IsAaveTopic(topic0 string) bool {
	switch strings.ToLower(strings.TrimSpace(topic0)) {
	case TopicLiquidationCall, TopicBorrow:
		return true
	default:
		return false
	}
}

// reserveDecimals maps Aave V3 reserve/debt assets on Mantle (lowercase address ->
// decimals). It only scales a raw amount into a whole-token figure for the display-only
// size_usd, never a detection input. A token absent here yields a nil USD size (we never
// guess). Verified on-chain (same provenance as config/pools.yaml); do not add an address
// without a source.
var reserveDecimals = map[string]int{
	"0x779ded0c9e1022225f8e0630b35a9b54be713736": 6,  // USDT0
	"0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9": 6,  // USDC
	"0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34": 18, // USDe
	"0xfc421ad3c883bf9e7c4f42de845c4e4405799e73": 18, // GHO
	"0x211cc4dd073734da055fbf44a2b4667d5e5fe5d2": 18, // sUSDe
	"0xc96de26018a54d51c097160568752c4e3bd6c364": 8,  // FBTC
	"0xdeaddeaddeaddeaddeaddeaddeaddeaddead1111": 18, // WETH
	"0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8": 18, // WMNT
	"0x93e855643e940d025be2e529272e4dbd15a2cf74": 18, // wrsETH
	"0x051665f2455116e929b9972c36d23070f5054ce0": 6,  // syrupUSDT
}

// stableReserves is the subset of the reserve table treated as ≈ $1 per whole token
// for display-only USD sizing: a debt/reserve is sized only when in this set (we never
// approximate a volatile asset without a price). Membership is by contract address.
var stableReserves = map[string]struct{}{
	"0x779ded0c9e1022225f8e0630b35a9b54be713736": {}, // USDT0
	"0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9": {}, // USDC
	"0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34": {}, // USDe
	"0xfc421ad3c883bf9e7c4f42de845c4e4405799e73": {}, // GHO
	"0x211cc4dd073734da055fbf44a2b4667d5e5fe5d2": {}, // sUSDe
	"0x051665f2455116e929b9972c36d23070f5054ce0": {}, // syrupUSDT
}

// ReserveDecimals returns the verified decimals for a reserve/debt asset and true,
// or (0, false) when absent from the table. Case-insensitive.
func ReserveDecimals(addr string) (int, bool) {
	d, ok := reserveDecimals[strings.ToLower(strings.TrimSpace(addr))]
	return d, ok
}

// IsStableReserve reports whether a reserve/debt asset is a known dollar stable in
// the verified set above (case-insensitive). Only a stable asset gets a USD size.
func IsStableReserve(addr string) bool {
	_, ok := stableReserves[strings.ToLower(strings.TrimSpace(addr))]
	return ok
}

// StableSizeUSD computes a display-only approximate USD notional for a raw amount of a
// reserve/debt asset (|amount| / 10^decimals when the asset is a known dollar stable in
// the decimals table, else nil). Pure own-data sizing (stable ≈ $1), no external price,
// matching detect.stableSizeUSD. Abs defends a negative amount, never expected from an
// unsigned on-chain field.
func StableSizeUSD(asset string, amount decimal.Decimal) *decimal.Decimal {
	if !IsStableReserve(asset) {
		return nil
	}
	dec, ok := ReserveDecimals(asset)
	if !ok {
		// In the stable set but missing decimals would be a registry inconsistency;
		// do not fabricate a scale. Leave it nil rather than mis-size.
		return nil
	}
	v := amount.Abs().Shift(int32(-dec))
	return &v
}

// LiquidationCall is a decoded Aave V3 LiquidationCall event: a liquidator repaid
// part of `User`'s debt (`DebtToCover` of `DebtAsset`) and seized
// `LiquidatedCollateralAmount` of `CollateralAsset`. All addresses are lowercase
// hex; the two amounts are exact raw on-chain units (decimal.Decimal so full
// numeric precision survives).
type LiquidationCall struct {
	CollateralAsset            string          // indexed t1 (lowercase hex)
	DebtAsset                  string          // indexed t2 (lowercase hex)
	User                       string          // indexed t3: the liquidated borrower (lowercase hex)
	DebtToCover                decimal.Decimal // data word 0 (raw units of DebtAsset)
	LiquidatedCollateralAmount decimal.Decimal // data word 1 (raw units of CollateralAsset)
	Liquidator                 string          // data word 2 (lowercase hex): the smart-money bot
	ReceiveAToken              bool            // data word 3
}

// Borrow is a decoded Aave V3 Borrow event: `User` borrowed `Amount` of `Reserve`
// on behalf of `OnBehalfOf`. Addresses are lowercase hex; Amount/BorrowRate are
// exact raw units. InterestRateMode is 1=stable, 2=variable (Aave's enum).
type Borrow struct {
	Reserve          string          // indexed t1 (lowercase hex): the borrowed asset
	OnBehalfOf       string          // indexed t2 (lowercase hex): the debt owner
	ReferralCode     uint16          // indexed t3
	User             string          // data word 0 (lowercase hex): the initiator (not indexed)
	Amount           decimal.Decimal // data word 1 (raw units of Reserve)
	InterestRateMode uint8           // data word 2 (1=stable, 2=variable)
	BorrowRate       decimal.Decimal // data word 3 (ray)
}

// evmWordBytes is the size of one ABI-encoded 32-byte word (mirrors aggregate).
const evmWordBytes = 32

// DecodeLiquidationCall decodes an Aave V3 LiquidationCall log strictly per the
// verified layout:
//
//	indexed: collateralAsset(topic1), debtAsset(topic2), user(topic3)
//	data:    debtToCover (uint256), liquidatedCollateralAmount (uint256),
//	         liquidator (address), receiveAToken (bool)
//
// It errors (never panics) when a topic is missing or the data is not the expected 4
// words, so a malformed/foreign log is skipped rather than mis-read. Addresses are
// lowercased; the two amounts are read as unsigned 256-bit integers.
func DecodeLiquidationCall(topic1, topic2, topic3, data string) (LiquidationCall, error) {
	collateral, err := addrFromTopic(topic1)
	if err != nil {
		return LiquidationCall{}, fmt.Errorf("lending: liquidation collateralAsset topic: %w", err)
	}
	debt, err := addrFromTopic(topic2)
	if err != nil {
		return LiquidationCall{}, fmt.Errorf("lending: liquidation debtAsset topic: %w", err)
	}
	user, err := addrFromTopic(topic3)
	if err != nil {
		return LiquidationCall{}, fmt.Errorf("lending: liquidation user topic: %w", err)
	}

	words, err := splitWords(data)
	if err != nil {
		return LiquidationCall{}, fmt.Errorf("lending: liquidation data: %w", err)
	}
	if len(words) < 4 {
		return LiquidationCall{}, fmt.Errorf("lending: liquidation expects >=4 data words, got %d", len(words))
	}

	return LiquidationCall{
		CollateralAsset:            collateral,
		DebtAsset:                  debt,
		User:                       user,
		DebtToCover:                decodeUint256(words[0]),
		LiquidatedCollateralAmount: decodeUint256(words[1]),
		Liquidator:                 addrFromWord(words[2]),
		ReceiveAToken:              decodeBool(words[3]),
	}, nil
}

// DecodeBorrow decodes an Aave V3 Borrow log strictly per the verified layout:
//
//	indexed: reserve(topic1), onBehalfOf(topic2), referralCode(topic3, uint16)
//	data:    user (address), amount (uint256), interestRateMode (uint8),
//	         borrowRate (uint256)
//
// `user` is not indexed, so it is the first data word (the classic Aave indexed-
// ordering gotcha). It returns an error (never panics) when a topic is missing or
// the data is not the expected 4 words. Addresses are normalized to lowercase.
func DecodeBorrow(topic1, topic2, topic3, data string) (Borrow, error) {
	reserve, err := addrFromTopic(topic1)
	if err != nil {
		return Borrow{}, fmt.Errorf("lending: borrow reserve topic: %w", err)
	}
	onBehalfOf, err := addrFromTopic(topic2)
	if err != nil {
		return Borrow{}, fmt.Errorf("lending: borrow onBehalfOf topic: %w", err)
	}
	referral, err := uint16FromTopic(topic3)
	if err != nil {
		return Borrow{}, fmt.Errorf("lending: borrow referralCode topic: %w", err)
	}

	words, err := splitWords(data)
	if err != nil {
		return Borrow{}, fmt.Errorf("lending: borrow data: %w", err)
	}
	if len(words) < 4 {
		return Borrow{}, fmt.Errorf("lending: borrow expects >=4 data words, got %d", len(words))
	}

	return Borrow{
		Reserve:          reserve,
		OnBehalfOf:       onBehalfOf,
		ReferralCode:     referral,
		User:             addrFromWord(words[0]),
		Amount:           decodeUint256(words[1]),
		InterestRateMode: decodeUint8(words[2]),
		BorrowRate:       decodeUint256(words[3]),
	}, nil
}

// SplitWords is the exported form of splitWords, reused by the lstflow Transfer decoder
// so the 32-byte word math lives in one place.
func SplitWords(data string) ([][]byte, error) {
	return splitWords(data)
}

// AddrFromTopic is the exported form of addrFromTopic, reused by the lstflow Transfer
// decoder (from/to are indexed topics there too).
func AddrFromTopic(topic string) (string, error) {
	return addrFromTopic(topic)
}

// DecodeUint256 is the exported form of decodeUint256, reused by the lstflow Transfer
// decoder for the same unsigned value-word semantics.
func DecodeUint256(word []byte) decimal.Decimal {
	return decodeUint256(word)
}

// splitWords decodes hex log data into 32-byte words, erroring on non-hex input or a
// length that is not a multiple of 32. Kept local (mirrors aggregate.splitWords) so
// the lending package has no dependency on the aggregate/swap path.
func splitWords(data string) ([][]byte, error) {
	h := strings.TrimSpace(data)
	h = strings.TrimPrefix(h, "0x")
	h = strings.TrimPrefix(h, "0X")
	if h == "" {
		return nil, errors.New("empty log data")
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("decode hex data: %w", err)
	}
	if len(raw)%evmWordBytes != 0 {
		return nil, fmt.Errorf("log data length %d not a multiple of %d", len(raw), evmWordBytes)
	}
	words := make([][]byte, 0, len(raw)/evmWordBytes)
	for off := 0; off < len(raw); off += evmWordBytes {
		words = append(words, raw[off:off+evmWordBytes])
	}
	return words, nil
}

// decodeUint256 interprets a 32-byte word as a big-endian unsigned integer
// (Aave amounts are non-negative, so no two's-complement handling is needed).
func decodeUint256(word []byte) decimal.Decimal {
	return decimal.NewFromBigInt(new(big.Int).SetBytes(word), 0)
}

// decodeUint8 reads the low (last) byte of a 32-byte ABI word as a uint8, where an
// ABI-encoded uint8 sits. A short word yields 0.
func decodeUint8(word []byte) uint8 {
	if len(word) == 0 {
		return 0
	}
	return word[len(word)-1]
}

// decodeBool reads an ABI bool word: any non-zero byte is true, robust to a
// non-canonical encoding.
func decodeBool(word []byte) bool {
	for _, b := range word {
		if b != 0 {
			return true
		}
	}
	return false
}

// addrFromWord extracts an EVM address from a 32-byte ABI word: the low 20 bytes
// (the leading 12 bytes are zero padding). Returned lowercase hex.
func addrFromWord(word []byte) string {
	return strings.ToLower(common.BytesToAddress(word).Hex())
}

// addrFromTopic parses an indexed-address topic (32-byte, ABI-padded to the low 20
// bytes) into a lowercase hex address. An empty topic errors, so a truncated/foreign
// log is skipped rather than decoded as the zero address.
func addrFromTopic(topic string) (string, error) {
	t := strings.TrimSpace(topic)
	if t == "" {
		return "", errors.New("empty indexed topic")
	}
	h := strings.TrimPrefix(strings.TrimPrefix(t, "0x"), "0X")
	raw, err := hex.DecodeString(h)
	if err != nil {
		return "", fmt.Errorf("decode topic hex %q: %w", topic, err)
	}
	// BytesToAddress takes the low 20 bytes, so a 32-byte word maps to its address.
	return strings.ToLower(common.BytesToAddress(raw).Hex()), nil
}

// uint16FromTopic parses an indexed uint16 topic (right-aligned in the 32-byte word).
// An empty topic errors; a value above uint16 range is rejected rather than truncated
// (a wider value would mean we mis-identified the field).
func uint16FromTopic(topic string) (uint16, error) {
	t := strings.TrimSpace(topic)
	if t == "" {
		return 0, errors.New("empty indexed topic")
	}
	h := strings.TrimPrefix(strings.TrimPrefix(t, "0x"), "0X")
	raw, err := hex.DecodeString(h)
	if err != nil {
		return 0, fmt.Errorf("decode topic hex %q: %w", topic, err)
	}
	n := new(big.Int).SetBytes(raw)
	if !n.IsUint64() || n.Uint64() > 0xFFFF {
		return 0, fmt.Errorf("referralCode topic %q out of uint16 range: %s", topic, n.String())
	}
	return uint16(n.Uint64()), nil
}
