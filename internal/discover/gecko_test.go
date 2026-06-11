package discover

import (
	"testing"
)

// These tests cover the pure GeckoTerminal parsing helpers: the JSON:API page →
// Candidate mapping, the "<network>_<address>" token-id extraction, and the
// reserve string parse. They are hermetic (no network): the live TopPools/HTTP
// path is exercised by the verification pipeline tests through a mock GeckoClient.

func TestTokenAddrFromID(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want string
	}{
		{"mantle_prefixed", "mantle_0x5d3a1Ff2b6BAb83b63cd9AD0787074081a52ef34", "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34"},
		{"already_lower", "mantle_0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8", "0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8"},
		{"bare_address", "0xabc", "0xabc"},
		{"empty", "", ""},
		{"no_address", "mantle_token", ""},
		{"whitespace", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenAddrFromID(tc.id); got != tc.want {
				t.Fatalf("tokenAddrFromID(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func TestParseUSD(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"3210000.42", 3210000.42},
		{"  1000  ", 1000},
		{"0", 0},
		{"", 0},             // blank -> 0 (safe default: fails the floor)
		{"not-a-number", 0}, // junk -> 0 (safe default), never an error
	}
	for _, tc := range cases {
		if got := parseUSD(tc.in); got != tc.want {
			t.Fatalf("parseUSD(%q) = %g, want %g", tc.in, got, tc.want)
		}
	}
}

// parsePage maps the JSON:API shape to candidates, lowercasing the pool address,
// extracting both token addresses from their relationship ids, and skipping rows
// missing the pool address or either token id.
func TestParsePage(t *testing.T) {
	var p geckoPage
	// Row 0: complete, should map. The inline struct mirrors geckoPage.Data's
	// element shape (including the volume_usd.h24 field the candidate parse reads).
	row0 := struct {
		Attributes struct {
			Address      string `json:"address"`
			ReserveInUSD string `json:"reserve_in_usd"`
			VolumeUSD    struct {
				H24 string `json:"h24"`
			} `json:"volume_usd"`
		} `json:"attributes"`
		Relationships struct {
			Dex        relRef `json:"dex"`
			BaseToken  relRef `json:"base_token"`
			QuoteToken relRef `json:"quote_token"`
		} `json:"relationships"`
	}{}
	row0.Attributes.Address = "0xEAFC4d6D4c3391CD4FC10C85d2f5f972D58c0Dd5"
	row0.Attributes.ReserveInUSD = "3210000.42"
	row0.Attributes.VolumeUSD.H24 = "987654.321"
	row0.Relationships.Dex.Data.ID = "agni"
	row0.Relationships.BaseToken.Data.ID = "mantle_0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34"
	row0.Relationships.QuoteToken.Data.ID = "mantle_0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8"

	// Row 1: missing quote-token id, should be skipped.
	row1 := row0
	row1.Attributes.Address = "0xdeadbeef"
	row1.Relationships.QuoteToken.Data.ID = ""

	// Row 2: missing pool address, should be skipped.
	row2 := row0
	row2.Attributes.Address = ""

	p.Data = append(p.Data, row0, row1, row2)

	got := parsePage(p)
	if len(got) != 1 {
		t.Fatalf("expected 1 parsed candidate (rows 1,2 skipped), got %d", len(got))
	}
	c := got[0]
	if c.PoolAddr != "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5" {
		t.Fatalf("pool address not lowercased: %q", c.PoolAddr)
	}
	if c.DexID != "agni" {
		t.Fatalf("dex id = %q, want agni", c.DexID)
	}
	if c.Token0Addr != "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34" {
		t.Fatalf("token0 = %q", c.Token0Addr)
	}
	if c.Token1Addr != "0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8" {
		t.Fatalf("token1 = %q", c.Token1Addr)
	}
	if c.ReserveUSD != 3210000.42 {
		t.Fatalf("reserve = %g", c.ReserveUSD)
	}
	if c.Vol24hUSD != 987654.321 {
		t.Fatalf("vol24h = %g, want 987654.321 (parsed from volume_usd.h24)", c.Vol24hUSD)
	}
}
