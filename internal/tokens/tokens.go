// Package tokens is the single source of truth for OCR's verified Mantle anchor token
// addresses: the dollar-stable set treated as $1, and WMNT (the gas token, priced
// on-chain). Membership is by contract address (lowercase hex), never symbol, so a
// relabelled pool cannot misclassify a token. Addresses are verified against
// config/pools.yaml + MantleScan, never invented.
package tokens

import "strings"

// Verified Mantle anchor contract addresses (lowercase hex), cross-referenced with
// config/pools.yaml and internal/api token links.
const (
	USDe = "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34"
	USDC = "0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9"
	USDT = "0x201eba5cc46d216ce6dc03f6a759e8e766e956ae"
	WMNT = "0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8"
)

// stableSet is the dollar-stable anchor set, valued at $1 per whole token for every
// display-only figure (size_usd, pool TVL, USD volume). Detection never reads it.
var stableSet = map[string]struct{}{
	USDe: {},
	USDC: {},
	USDT: {},
}

// IsStable reports whether addr is a verified dollar-stable anchor (case-insensitive,
// space-trimmed).
func IsStable(addr string) bool {
	_, ok := stableSet[normalize(addr)]
	return ok
}

// IsWMNT reports whether addr is WMNT (case-insensitive, space-trimmed).
func IsWMNT(addr string) bool {
	return normalize(addr) == WMNT
}

func normalize(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}
