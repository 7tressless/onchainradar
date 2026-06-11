package api

import "testing"

// actionURL must produce the verified protocol-app CTAs (and nothing for DEX/depeg).
func TestActionURL(t *testing.T) {
	if u, l := actionURL(6, "0xReSerVe"); u != "https://app.aave.com/reserve-overview/?underlyingAsset=0xreserve&marketName=proto_mantle_v3" || l != "Borrow on Aave" {
		t.Fatalf("big_borrow: %q / %q", u, l)
	}
	if u, l := actionURL(6, ""); u != "https://app.aave.com/markets/?marketName=proto_mantle_v3" || l != "Aave market" {
		t.Fatalf("big_borrow no-subject fallback: %q / %q", u, l)
	}
	if u, l := actionURL(5, "0xuser"); u != "https://app.aave.com/markets/?marketName=proto_mantle_v3" || l != "View on Aave" {
		t.Fatalf("liquidation: %q / %q", u, l)
	}
	if u, l := actionURL(4, "0xtoken"); u != "https://www.methprotocol.xyz/" || l != "Stake on mETH Protocol" {
		t.Fatalf("lst_flow: %q / %q", u, l)
	}
	for _, ty := range []int16{1, 2, 3, 7} {
		if u, l := actionURL(ty, "0xpool"); u != "" || l != "" {
			t.Fatalf("type %d must have no action CTA: %q / %q", ty, u, l)
		}
	}
}
