package api

import "testing"

// binStep returns a *int for the table-driven lpURL cases (nil = bin step unknown).
func binStep(v int) *int { return &v }

// lpURL must build the verified per-pool add-liquidity deep-links and yield "" when
// it cannot (missing token, unknown bin step for moe, unknown dex) so the frontend
// falls back to dex_url. WMNT must render as the literal "MNT" for merchant_moe only,
// on EITHER token side; Agni uses the address as-is.
func TestLPURL(t *testing.T) {
	const (
		wmnt  = "0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8" // WMNT -> "MNT" on moe
		usde  = "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34"
		cmeth = "0xe6829d9a7ee3040e1276fa75293bde931859e8fa"
	)
	cases := []struct {
		name    string
		dex     string
		token0  string
		token1  string
		binStep *int
		want    string
	}{
		{
			name: "agni two addresses (lowercased, no bin step)",
			dex:  "agni", token0: "0xAAa", token1: "0xBbB", binStep: nil,
			want: "https://agni.finance/add/0xaaa/0xbbb",
		},
		{
			name: "agni ignores bin step entirely",
			dex:  "agni", token0: usde, token1: cmeth, binStep: binStep(25),
			want: "https://agni.finance/add/" + usde + "/" + cmeth,
		},
		{
			name: "moe WMNT on token0 side -> MNT",
			dex:  "merchant_moe", token0: wmnt, token1: usde, binStep: binStep(25),
			want: "https://merchantmoe.com/pool/v22/MNT/" + usde + "/25",
		},
		{
			name: "moe WMNT on token1 side -> MNT (case-insensitive)",
			dex:  "merchant_moe", token0: cmeth, token1: "0x78C1B0C915C4FAA5FFFA6CABF0219DA63D7F4CB8", binStep: binStep(5),
			want: "https://merchantmoe.com/pool/v22/" + cmeth + "/MNT/5",
		},
		{
			name: "moe normal pair (no WMNT)",
			dex:  "merchant_moe", token0: usde, token1: cmeth, binStep: binStep(25),
			want: "https://merchantmoe.com/pool/v22/" + usde + "/" + cmeth + "/25",
		},
		{
			name: "moe nil bin step -> empty (frontend falls back to dex_url)",
			dex:  "merchant_moe", token0: usde, token1: cmeth, binStep: nil,
			want: "",
		},
		{
			name: "moe alias merchantmoe with bin step",
			dex:  "merchantmoe", token0: usde, token1: cmeth, binStep: binStep(10),
			want: "https://merchantmoe.com/pool/v22/" + usde + "/" + cmeth + "/10",
		},
		{
			// FusionX is a decodable dex but its add-liquidity URL is unverified, so
			// lpURL must yield "" (never a guessed URL) even with a bin step.
			name: "fusionx (decodable but unverified LP url) -> empty even with bin step",
			dex:  "fusionx", token0: usde, token1: cmeth, binStep: binStep(25),
			want: "",
		},
		{
			name: "fusionx nil bin step -> empty",
			dex:  "fusionx", token0: usde, token1: cmeth, binStep: nil,
			want: "",
		},
		{
			name: "agni empty token -> empty",
			dex:  "agni", token0: "", token1: cmeth, binStep: nil,
			want: "",
		},
		{
			name: "moe empty token -> empty (even with a bin step)",
			dex:  "merchant_moe", token0: usde, token1: "", binStep: binStep(25),
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lpURL(c.dex, c.token0, c.token1, c.binStep); got != c.want {
				t.Errorf("lpURL(%q,%q,%q,%v) = %q, want %q", c.dex, c.token0, c.token1, c.binStep, got, c.want)
			}
		})
	}
}

// matchaTradeURL must build the verified Matcha deep-link (lowercased tokens) and
// yield "" when either token is missing (so the frontend renders no broken link).
func TestMatchaTradeURL(t *testing.T) {
	got := matchaTradeURL("0xAAa", "0xBbB")
	want := "https://matcha.xyz/tokens/mantle/0xaaa?buyChain=5000&buyAddress=0xbbb"
	if got != want {
		t.Fatalf("matchaTradeURL: got %q want %q", got, want)
	}
	if matchaTradeURL("", "0xb") != "" || matchaTradeURL("0xa", "") != "" || matchaTradeURL("  ", "0xb") != "" {
		t.Fatalf("matchaTradeURL: a missing/blank token must yield \"\"")
	}
}

// dexAppURL maps only the DEXs OCR decodes; anything else is "" (never a guessed URL).
func TestDexAppURL(t *testing.T) {
	cases := map[string]string{
		"merchant_moe": "https://merchantmoe.com/",
		"MerchantMoe":  "https://merchantmoe.com/",
		"agni":         "https://agni.finance/swap",
		"fusionx":      "",
		"":             "",
	}
	for in, want := range cases {
		if got := dexAppURL(in); got != want {
			t.Errorf("dexAppURL(%q): got %q want %q", in, got, want)
		}
	}
}
