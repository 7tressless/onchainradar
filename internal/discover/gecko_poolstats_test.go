package discover

import (
	"encoding/json"
	"testing"
)

// Hermetic tests for the single-pool market-stats parse (parsePoolDetail): the
// JSON:API detail payload -> PoolMarketStats mapping, lenient numeric parsing
// (missing -> nil, not 0), and best-effort token-symbol lookup from `included`.
// The live HTTP path (PoolStats) is exercised end-to-end in the integration smoke;
// this file pins the pure mapping.

const sampleDetailJSON = `{
  "data": {
    "id": "mantle_0xeafc",
    "type": "pool",
    "attributes": {
      "address": "0xEAFC",
      "reserve_in_usd": "3300225.0629",
      "base_token_price_usd": "1.00120613",
      "volume_usd": { "h24": "234704.0987" },
      "price_change_percentage": { "h24": "0.111" }
    },
    "relationships": {
      "base_token": { "data": { "id": "mantle_0xbase" } },
      "quote_token": { "data": { "id": "mantle_0xquote" } }
    }
  },
  "included": [
    { "id": "mantle_0xbase", "type": "token", "attributes": { "symbol": "USDe" } },
    { "id": "mantle_0xquote", "type": "token", "attributes": { "symbol": "WMNT" } }
  ]
}`

func TestParsePoolDetail_Full(t *testing.T) {
	var d geckoPoolDetail
	if err := json.Unmarshal([]byte(sampleDetailJSON), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := parsePoolDetail(d, "0xeafc", "https://src")

	if got.PoolAddr != "0xeafc" {
		t.Fatalf("pool addr = %q", got.PoolAddr)
	}
	if got.TVLUSD == nil || *got.TVLUSD != 3300225.0629 {
		t.Fatalf("tvl = %v", got.TVLUSD)
	}
	if got.Vol24hUSD == nil || *got.Vol24hUSD != 234704.0987 {
		t.Fatalf("vol24h = %v", got.Vol24hUSD)
	}
	if got.PriceUSD == nil || *got.PriceUSD != 1.00120613 {
		t.Fatalf("price = %v", got.PriceUSD)
	}
	if got.PriceChange24h == nil || *got.PriceChange24h != 0.111 {
		t.Fatalf("change = %v", got.PriceChange24h)
	}
	if got.BaseToken != "USDe" || got.QuoteToken != "WMNT" {
		t.Fatalf("symbols = %q/%q, want USDe/WMNT", got.BaseToken, got.QuoteToken)
	}
	if got.SourceURL != "https://src" {
		t.Fatalf("source = %q", got.SourceURL)
	}
}

func TestParsePoolDetail_MissingFieldsAreNil(t *testing.T) {
	// A detail payload with empty/absent numeric attributes must yield nil pointers
	// (so the columns stay NULL), not 0, and absent symbols stay "".
	const sparse = `{
	  "data": {
	    "attributes": {
	      "address": "0xabc",
	      "reserve_in_usd": "",
	      "base_token_price_usd": "not-a-number",
	      "volume_usd": {},
	      "price_change_percentage": {}
	    },
	    "relationships": {
	      "base_token": { "data": { "id": "mantle_0xb" } },
	      "quote_token": { "data": { "id": "mantle_0xq" } }
	    }
	  },
	  "included": []
	}`
	var d geckoPoolDetail
	if err := json.Unmarshal([]byte(sparse), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := parsePoolDetail(d, "0xabc", "u")
	if got.TVLUSD != nil || got.Vol24hUSD != nil || got.PriceUSD != nil || got.PriceChange24h != nil {
		t.Fatalf("all numeric fields should be nil, got %+v", got)
	}
	if got.BaseToken != "" || got.QuoteToken != "" {
		t.Fatalf("absent symbols should be empty, got %q/%q", got.BaseToken, got.QuoteToken)
	}
}

// TestParsePoolDetail_TokenMeta pins the token-metadata extraction (out.Tokens):
// the real GeckoTerminal token-resource shape supplies address + symbol + decimals
// (a JSON number) + image_url, and the "missing.png" placeholder (and non-token
// resources like a dex) are handled. Decimals stay a pointer (nil when absent).
// This is display-only data.
func TestParsePoolDetail_TokenMeta(t *testing.T) {
	// Mirrors the live GET .../pools/{addr}?include=base_token,quote_token,dex shape:
	// token resources carry attributes.{address,symbol,decimals,image_url}; the base
	// token has a real logo, the quote token's logo is the "missing.png" placeholder,
	// and a dex resource (no address) must be ignored for token_meta.
	const withTokens = `{
	  "data": {
	    "attributes": { "address": "0xpool", "reserve_in_usd": "1000" },
	    "relationships": {
	      "base_token": { "data": { "id": "mantle_0xbase" } },
	      "quote_token": { "data": { "id": "mantle_0xquote" } }
	    }
	  },
	  "included": [
	    { "id": "mantle_0xbase", "type": "token",
	      "attributes": { "address": "0xBASE", "symbol": "USDe", "decimals": 18,
	                      "image_url": "https://coin-images/usde.png" } },
	    { "id": "mantle_0xquote", "type": "token",
	      "attributes": { "address": "0xQUOTE", "symbol": "WMNT", "decimals": 6,
	                      "image_url": "missing.png" } },
	    { "id": "fluxion", "type": "dex", "attributes": { "name": "Fluxion" } }
	  ]
	}`
	var d geckoPoolDetail
	if err := json.Unmarshal([]byte(withTokens), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := parsePoolDetail(d, "0xpool", "https://src")

	// Symbols still resolve for the stats block.
	if got.BaseToken != "USDe" || got.QuoteToken != "WMNT" {
		t.Fatalf("stats symbols = %q/%q", got.BaseToken, got.QuoteToken)
	}

	// Exactly two token-meta entries (the dex resource is excluded).
	if len(got.Tokens) != 2 {
		t.Fatalf("Tokens = %d, want 2 (dex excluded): %+v", len(got.Tokens), got.Tokens)
	}
	byAddr := map[string]TokenMeta{}
	for _, tm := range got.Tokens {
		byAddr[tm.Address] = tm
	}
	// base: lowercased address from attributes.address, real logo, decimals 18.
	b, ok := byAddr["0xbase"]
	if !ok || b.Symbol != "USDe" || b.LogoURL != "https://coin-images/usde.png" || b.Decimals == nil || *b.Decimals != 18 {
		t.Fatalf("base token meta wrong: %+v (ok=%v)", b, ok)
	}
	// quote: "missing.png" sanitized to "" (absent), decimals 6.
	q, ok := byAddr["0xquote"]
	if !ok || q.Symbol != "WMNT" || q.LogoURL != "" || q.Decimals == nil || *q.Decimals != 6 {
		t.Fatalf("quote token meta wrong: %+v (ok=%v)", q, ok)
	}
}

// TestParsePoolDetail_TokenMeta_AddressFromID: when a token resource omits
// attributes.address, the address is recovered from the "<net>_0x…" relationship id;
// a token with no usable address and no symbol is skipped.
func TestParsePoolDetail_TokenMeta_AddressFromID(t *testing.T) {
	const j = `{
	  "data": { "attributes": { "address": "0xpool" },
	            "relationships": { "base_token": { "data": { "id": "mantle_0xfromid" } },
	                               "quote_token": { "data": { "id": "mantle_0xq" } } } },
	  "included": [
	    { "id": "mantle_0xFROMID", "type": "token", "attributes": { "symbol": "AAA", "decimals": 8 } },
	    { "id": "no-address-here", "type": "token", "attributes": { "symbol": "" } }
	  ]
	}`
	var d geckoPoolDetail
	if err := json.Unmarshal([]byte(j), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := parsePoolDetail(d, "0xpool", "u")
	if len(got.Tokens) != 1 {
		t.Fatalf("Tokens = %d, want 1 (the address-less resource is skipped): %+v", len(got.Tokens), got.Tokens)
	}
	tm := got.Tokens[0]
	if tm.Address != "0xfromid" || tm.Symbol != "AAA" || tm.Decimals == nil || *tm.Decimals != 8 {
		t.Fatalf("token from id wrong: %+v", tm)
	}
}

// TestSanitizeLogoURL pins the logo normalization: trims, drops "missing.png" and
// non-http junk, keeps real http(s) URLs.
func TestSanitizeLogoURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://x/a.png", "https://x/a.png"},
		{"http://x/a.png", "http://x/a.png"},
		{"  https://x/b.png  ", "https://x/b.png"},
		{"missing.png", ""},
		{"MISSING.PNG", ""},
		{"", ""},
		{"ipfs://whatever", ""}, // not http(s)
		{"data:image/png;base64,xxx", ""},
	}
	for _, c := range cases {
		if got := sanitizeLogoURL(c.in); got != c.want {
			t.Errorf("sanitizeLogoURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMergeSpotPrices pins the simple-token-price parse (mergeSpotPrices): the
// token_prices / h24_price_change_percentage maps -> TokenSpot, keyed by lowercased
// address, with lenient numeric parsing (junk/empty -> nil) and omission of an address
// that carries neither a usable price nor change. This is display-only data.
func TestMergeSpotPrices(t *testing.T) {
	const sample = `{
	  "data": {
	    "attributes": {
	      "token_prices": {
	        "0xAAA": "2015.3479",
	        "0xbbb": "0.99981",
	        "0xccc": "junk",
	        "0xddd": ""
	      },
	      "h24_price_change_percentage": {
	        "0xAAA": "-1.1586",
	        "0xbbb": "garbage"
	      }
	    }
	  }
	}`
	var body geckoSimpleTokenPrice
	if err := json.Unmarshal([]byte(sample), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dst := map[string]TokenSpot{}
	mergeSpotPrices(body, dst)

	// 0xAAA: price + change, address lowercased to the map key.
	a, ok := dst["0xaaa"]
	if !ok || a.PriceUSD == nil || *a.PriceUSD != 2015.3479 || a.Change24h == nil || *a.Change24h != -1.1586 {
		t.Fatalf("0xaaa spot wrong: %+v (ok=%v)", a, ok)
	}
	// 0xbbb: a valid price but an unparseable change -> price set, change nil.
	b, ok := dst["0xbbb"]
	if !ok || b.PriceUSD == nil || *b.PriceUSD != 0.99981 || b.Change24h != nil {
		t.Fatalf("0xbbb spot wrong: %+v (ok=%v)", b, ok)
	}
	// 0xccc (junk price) and 0xddd (empty price) carry nothing usable -> omitted.
	if _, ok := dst["0xccc"]; ok {
		t.Fatalf("0xccc should be omitted (junk price), got %+v", dst["0xccc"])
	}
	if _, ok := dst["0xddd"]; ok {
		t.Fatalf("0xddd should be omitted (empty price), got %+v", dst["0xddd"])
	}
	if len(dst) != 2 {
		t.Fatalf("dst should hold exactly the two usable tokens, got %d: %+v", len(dst), dst)
	}
}

// TestMergeMarketPrices pins the coin-level quote parse (mergeMarketPrices): the
// {"0xaddr": {"usd", "usd_24h_change"}} map -> TokenSpot, keyed by lowercased address,
// nil-preserving per field, and omission of a quote with neither field. Display-only.
func TestMergeMarketPrices(t *testing.T) {
	const sample = `{
	  "0xAAA": { "usd": 1766.26, "usd_24h_change": -1.9053 },
	  "0xbbb": { "usd": 0.999022 },
	  "0xccc": {}
	}`
	var quotes map[string]geckoMarketQuote
	if err := json.Unmarshal([]byte(sample), &quotes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dst := map[string]TokenSpot{}
	mergeMarketPrices(quotes, dst)

	// 0xAAA: price + change, address lowercased to the map key.
	a, ok := dst["0xaaa"]
	if !ok || a.PriceUSD == nil || *a.PriceUSD != 1766.26 || a.Change24h == nil || *a.Change24h != -1.9053 {
		t.Fatalf("0xaaa quote wrong: %+v (ok=%v)", a, ok)
	}
	// 0xbbb: price only -> change stays nil (the caller keeps its existing change).
	b, ok := dst["0xbbb"]
	if !ok || b.PriceUSD == nil || *b.PriceUSD != 0.999022 || b.Change24h != nil {
		t.Fatalf("0xbbb quote wrong: %+v (ok=%v)", b, ok)
	}
	// 0xccc: an empty quote carries nothing usable -> omitted.
	if _, ok := dst["0xccc"]; ok {
		t.Fatalf("0xccc should be omitted (empty quote), got %+v", dst["0xccc"])
	}
	if len(dst) != 2 {
		t.Fatalf("dst should hold exactly the two usable tokens, got %d: %+v", len(dst), dst)
	}
}

func TestParseFloatPtr(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
		want   float64
	}{
		{"3300225.0629", true, 3300225.0629},
		{"  1.5  ", true, 1.5},
		{"-0.5", true, -0.5},
		{"0", true, 0},
		{"", false, 0},
		{"junk", false, 0},
	}
	for _, c := range cases {
		got := parseFloatPtr(c.in)
		if c.wantOK {
			if got == nil || *got != c.want {
				t.Fatalf("parseFloatPtr(%q) = %v, want %g", c.in, got, c.want)
			}
		} else if got != nil {
			t.Fatalf("parseFloatPtr(%q) = %v, want nil", c.in, got)
		}
	}
}
