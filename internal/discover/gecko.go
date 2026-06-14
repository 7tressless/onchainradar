// Package discover finds new Mantle pools and proposes them as disabled registry rows
// for a human to enable. GeckoTerminal only shortlists candidate addresses; every
// property detection relies on is re-derived on-chain before a pool is proposed, so a
// lying API cannot reach the registry (the propose-then-verify rationale is on Discover).
//
// This file holds the discovery source (a GeckoTerminal v2 REST client); the verify
// pipeline lives in discover.go.
package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog/log"
)

const (
	// geckoBaseURL is the keyless GeckoTerminal v2 REST root (free, no API key).
	geckoBaseURL = "https://api.geckoterminal.com/api/v2"

	// geckoKeyedBaseURL is the CoinGecko onchain REST root, used when an API key is set.
	// It serves the same GeckoTerminal data at the key's higher rate limit (the keyless
	// root 429s aggressively). Auth via x-cg-demo-api-key.
	geckoKeyedBaseURL = "https://api.coingecko.com/api/v3/onchain"

	// geckoNetwork is the GeckoTerminal network slug for Mantle mainnet.
	geckoNetwork = "mantle"

	// geckoHTTPTimeout bounds a single request so a hung endpoint cannot stall the run.
	geckoHTTPTimeout = 20 * time.Second

	// geckoPageSleep spaces page fetches for the rate limit (~30 calls/min keyless).
	geckoPageSleep = 2 * time.Second

	// geckoSortVolume orders the listing by 24h USD volume descending. It drives the
	// second TopPools pass so high-volume, lower-TVL anchor pools the liquidity pass
	// buries still surface for the volume-OR-reserve gate.
	geckoSortVolume = "h24_volume_usd_desc"
)

// Candidate is one pool as the discovery API reports it, an untrusted hint: Discover
// re-derives the token ordering, decimals, and anchor membership on-chain before
// proposing. ReserveUSD and Vol24hUSD are coarse discovery gates only.
type Candidate struct {
	PoolAddr   string  // pool contract address as reported (lowercased)
	DexID      string  // raw GeckoTerminal dex id, e.g. "agni" / "merchant-moe-v2-2"
	Token0Addr string  // base-token address as reported (lowercased); hint only
	Token1Addr string  // quote-token address as reported (lowercased); hint only
	ReserveUSD float64 // reserve_in_usd as reported; coarse liquidity gate only
	Vol24hUSD  float64 // volume_usd.h24 as reported; coarse volume gate only
}

// GeckoClient fetches candidate pools from a discovery source. It is the narrow
// interface Discover depends on, so the verify pipeline can be unit-tested with a mock
// that needs no network. The real implementation is *Gecko.
type GeckoClient interface {
	// TopPools returns candidate pools across the first `pages` pages, fetched in two
	// orderings (liquidity then 24h-volume) and de-duplicated by address. Transport,
	// non-200, and decode failures must surface as errors, never a silently dropped page.
	TopPools(ctx context.Context, pages int) ([]Candidate, error)
}

// Gecko is the live GeckoTerminal v2 client. It holds an *http.Client with a
// bounded timeout; the zero value is not usable, so construct it with NewGecko.
type Gecko struct {
	http    *http.Client
	baseURL string
	apiKey  string // CoinGecko demo API key; "" => keyless GeckoTerminal root
}

// NewGecko builds a GeckoTerminal/CoinGecko-onchain client with a bounded per-request
// timeout. A non-empty apiKey targets the CoinGecko onchain root (same data, higher rate
// limit) via x-cg-demo-api-key; an empty key falls back to the keyless GeckoTerminal root.
func NewGecko(apiKey string) *Gecko {
	base := geckoBaseURL
	if apiKey != "" {
		base = geckoKeyedBaseURL
	}
	return &Gecko{
		http:    &http.Client{Timeout: geckoHTTPTimeout},
		baseURL: base,
		apiKey:  apiKey,
	}
}

// setHeaders applies the JSON Accept header and, when a key is configured, the
// CoinGecko demo auth header.
func (g *Gecko) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	if g.apiKey != "" {
		req.Header.Set("x-cg-demo-api-key", g.apiKey)
	}
}

// geckoPage mirrors the subset of the GeckoTerminal JSON:API pools response consumed
// here. The `included` section is unused: each relationship id embeds its token address
// ("mantle_0xabc..."), and the authoritative ordering is re-derived on-chain anyway.
type geckoPage struct {
	Data []struct {
		Attributes struct {
			Address      string `json:"address"`
			ReserveInUSD string `json:"reserve_in_usd"` // decimal string, e.g. "3210000.42"
			// volume_usd.h24: rolling 24h USD volume (decimal string). Coarse
			// discovery gate only; missing or garbage parses to 0.
			VolumeUSD struct {
				H24 string `json:"h24"`
			} `json:"volume_usd"`
		} `json:"attributes"`
		Relationships struct {
			Dex        relRef `json:"dex"`
			BaseToken  relRef `json:"base_token"`
			QuoteToken relRef `json:"quote_token"`
		} `json:"relationships"`
	} `json:"data"`
}

// relRef is a JSON:API single-resource relationship: { "data": { "id": "…" } }.
type relRef struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

// TopPools fetches the first `pages` pages of Mantle pools in two orderings and
// flattens them into a de-duplicated candidate list:
//
//	pass 1: the API's default ordering (highest liquidity first);
//	pass 2: 24h USD volume descending (geckoSortVolume).
//
// The passes share one `seen` set (first occurrence wins), so pass 2 only adds pools the
// liquidity pass missed, chiefly the high-volume, lower-TVL anchors the volume-OR-reserve
// gate exists to admit. A non-positive `pages` is treated as 1; a page with no data ends
// its pass early. Every transport, non-200, or decode failure fails the run rather than
// yielding a partial result.
func (g *Gecko) TopPools(ctx context.Context, pages int) ([]Candidate, error) {
	if pages < 1 {
		pages = 1
	}

	var out []Candidate
	seen := make(map[string]bool)

	// fetched gates the courtesy sleep so it runs before every fetch except the first
	// (including across the pass-1/pass-2 seam), never after the final page.
	fetched := false

	// runPass walks one ordering, appending only pools not already in `seen`.
	runPass := func(sort string) error {
		for page := 1; page <= pages; page++ {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("discover: gecko context cancelled: %w", err)
			}

			if fetched {
				select {
				case <-ctx.Done():
					return fmt.Errorf("discover: gecko context cancelled: %w", ctx.Err())
				case <-time.After(geckoPageSleep):
				}
			}

			cands, err := g.fetchPage(ctx, page, sort)
			fetched = true
			if err != nil {
				return fmt.Errorf("discover: gecko page %d: %w", page, err)
			}
			if len(cands) == 0 {
				return nil // end of this listing
			}
			for _, c := range cands {
				if seen[c.PoolAddr] {
					continue // already seen in an earlier page or pass
				}
				seen[c.PoolAddr] = true
				out = append(out, c)
			}
		}
		return nil
	}

	if err := runPass(""); err != nil {
		return out, err
	}
	if err := runPass(geckoSortVolume); err != nil {
		return out, err
	}
	return out, nil
}

// PoolMarketStats is one pool's external market snapshot as GeckoTerminal reports it
// (TVL, 24h volume, spot price, 24h price-change, token symbols). Display-only, never
// used by detection. An omitted or unparseable field stays nil so a missing value is
// NULL downstream, not a bogus 0.
type PoolMarketStats struct {
	PoolAddr       string // lowercased pool address the stats describe
	TVLUSD         *float64
	Vol24hUSD      *float64
	PriceUSD       *float64
	PriceChange24h *float64 // percent
	BaseToken      string   // base-token symbol as reported ("" when absent)
	QuoteToken     string   // quote-token symbol as reported ("" when absent)
	SourceURL      string   // upstream URL this snapshot came from (provenance)

	// Tokens is the per-token metadata (symbol, logo, decimals) for the pool's base and
	// quote tokens, from the same `included` resources. The poolstats stage upserts these
	// into token_meta keyed by address.
	Tokens []TokenMeta
}

// TokenMeta is one token's display-only metadata from a pool detail's `included`
// resources. Address is the join key (tokens are shared across pools); the authoritative
// decimals are pools.dec0/1, not this hint.
type TokenMeta struct {
	Address  string // token contract address (lowercased); "" when the resource carried none
	Symbol   string // token symbol as reported ("" when absent)
	LogoURL  string // token logo image URL ("" when absent or a placeholder like "missing.png")
	Decimals *int   // token decimals as reported (nil when absent/unparseable)
}

// geckoPoolDetail mirrors the subset of GET /networks/{net}/pools/{addr} consumed here.
// Numeric attributes are decimal strings, parsed leniently (junk or empty -> nil). Token
// symbols come best-effort from the included resources.
type geckoPoolDetail struct {
	Data struct {
		Attributes struct {
			Address           string `json:"address"`
			ReserveInUSD      string `json:"reserve_in_usd"`
			BaseTokenPriceUSD string `json:"base_token_price_usd"`
			VolumeUSD         struct {
				H24 string `json:"h24"`
			} `json:"volume_usd"`
			PriceChangePercentage struct {
				H24 string `json:"h24"`
			} `json:"price_change_percentage"`
		} `json:"attributes"`
		Relationships struct {
			BaseToken  relRef `json:"base_token"`
			QuoteToken relRef `json:"quote_token"`
		} `json:"relationships"`
	} `json:"data"`
	Included []struct {
		ID         string `json:"id"`
		Type       string `json:"type"` // "token" for base/quote tokens; dex resources also appear
		Attributes struct {
			// address: the token contract address (lowercase 0x-hex). Also recoverable
			// from the "<network>_<address>" id, the fallback when this field is absent.
			Address string `json:"address"`
			Symbol  string `json:"symbol"`
			// image_url: the token logo. GeckoTerminal sends "missing.png" (not a URL)
			// when it has none; treated as absent downstream.
			ImageURL string `json:"image_url"`
			// decimals: a JSON number; a pointer so an absent field stays nil, not a 0.
			Decimals *int `json:"decimals"`
		} `json:"attributes"`
	} `json:"included"`
}

// PoolStats fetches one pool's external market snapshot from GeckoTerminal. Display-only,
// never detection input. Transport, non-200, and decode failures are wrapped and returned
// (the caller logs and skips per pool, never aborting a batch). A missing or unparseable
// numeric attribute is left nil rather than guessed.
func (g *Gecko) PoolStats(ctx context.Context, poolAddr string) (PoolMarketStats, error) {
	addr := strings.ToLower(strings.TrimSpace(poolAddr))
	if addr == "" {
		return PoolMarketStats{}, fmt.Errorf("discover: gecko pool stats: empty pool address")
	}
	// The address is interpolated into the request path, so validate its shape: a
	// non-hex value could otherwise alter the fetched URL. Reject rather than send.
	if !common.IsHexAddress(addr) {
		return PoolMarketStats{}, fmt.Errorf("discover: gecko pool stats: invalid pool address %q", poolAddr)
	}
	// include=base_token,quote_token carries the token symbols in `included` (optional).
	url := fmt.Sprintf("%s/networks/%s/pools/%s?include=base_token,quote_token", g.baseURL, geckoNetwork, addr)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PoolMarketStats{}, fmt.Errorf("discover: gecko pool stats build request: %w", err)
	}
	g.setHeaders(req)

	resp, err := g.http.Do(req)
	if err != nil {
		return PoolMarketStats{}, fmt.Errorf("discover: gecko pool stats http get %s: %w", url, err)
	}
	defer func() {
		if _, derr := io.Copy(io.Discard, resp.Body); derr != nil {
			log.Debug().Err(derr).Msg("discover: drain gecko pool-stats body")
		}
		if cerr := resp.Body.Close(); cerr != nil {
			log.Debug().Err(cerr).Msg("discover: close gecko pool-stats body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return PoolMarketStats{}, fmt.Errorf("discover: gecko pool stats non-200 status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var detail geckoPoolDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return PoolMarketStats{}, fmt.Errorf("discover: gecko pool stats decode json: %w", err)
	}

	return parsePoolDetail(detail, addr, url), nil
}

// parsePoolDetail flattens a decoded single-pool response into a PoolMarketStats. Pure
// (no I/O): lenient numeric parsing (junk -> nil) and best-effort symbol lookup by token
// relationship id.
func parsePoolDetail(d geckoPoolDetail, addr, sourceURL string) PoolMarketStats {
	a := d.Data.Attributes
	out := PoolMarketStats{
		PoolAddr:       addr,
		TVLUSD:         parseUSDPtr(a.ReserveInUSD),
		Vol24hUSD:      parseUSDPtr(a.VolumeUSD.H24),
		PriceUSD:       parseUSDPtr(a.BaseTokenPriceUSD),
		PriceChange24h: parseFloatPtr(a.PriceChangePercentage.H24),
		SourceURL:      sourceURL,
	}

	// Best-effort token symbols from the included resources, matched by relationship id
	// (absent ones stay ""). The full per-token metadata also goes into out.Tokens.
	symbols := make(map[string]string, len(d.Included))
	for _, inc := range d.Included {
		// Only token resources carry metadata; a dex resource (returned with include=dex)
		// has no address, so the non-empty-address guard below skips it.
		addr := strings.ToLower(strings.TrimSpace(inc.Attributes.Address))
		if addr == "" {
			addr = tokenAddrFromID(inc.ID)
		}
		sym := strings.TrimSpace(inc.Attributes.Symbol)
		if sym != "" {
			symbols[strings.TrimSpace(inc.ID)] = sym
		}
		if addr == "" {
			continue // not a token resource (or no address); skip token_meta
		}
		out.Tokens = append(out.Tokens, TokenMeta{
			Address:  addr,
			Symbol:   sym,
			LogoURL:  sanitizeLogoURL(inc.Attributes.ImageURL),
			Decimals: inc.Attributes.Decimals,
		})
	}
	out.BaseToken = symbols[strings.TrimSpace(d.Data.Relationships.BaseToken.Data.ID)]
	out.QuoteToken = symbols[strings.TrimSpace(d.Data.Relationships.QuoteToken.Data.ID)]
	return out
}

// sanitizeLogoURL normalizes a GeckoTerminal token image_url: it treats the
// "missing.png" placeholder and any non-URL junk as absent (returns "" so the API
// serializes null), and returns a genuine http(s) URL unchanged.
func sanitizeLogoURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// GeckoTerminal sends the literal "missing.png" when it has no logo.
	if strings.EqualFold(s, "missing.png") {
		return ""
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return ""
	}
	return s
}

// fetchPage GETs one page and parses it into candidates, split out so the rate-limit and
// loop logic stays in TopPools. sort selects the ordering: "" sends no sort param (the
// endpoint's default ranking), a non-empty value appends &sort=<sort>.
func (g *Gecko) fetchPage(ctx context.Context, page int, sort string) ([]Candidate, error) {
	url := fmt.Sprintf("%s/networks/%s/pools?page=%d", g.baseURL, geckoNetwork, page)
	if sort != "" {
		url += "&sort=" + sort
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	g.setHeaders(req)

	resp, err := g.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get %s: %w", url, err)
	}
	defer func() {
		// Drain and close so the connection is reused and no fd leaks; a close error is
		// not actionable but is logged rather than swallowed.
		if _, derr := io.Copy(io.Discard, resp.Body); derr != nil {
			log.Debug().Err(derr).Msg("discover: drain gecko response body")
		}
		if cerr := resp.Body.Close(); cerr != nil {
			log.Debug().Err(cerr).Msg("discover: close gecko response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		// Bound the body slice read for context without trusting its size.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("non-200 status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var page0 geckoPage
	if err := json.NewDecoder(resp.Body).Decode(&page0); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}

	return parsePage(page0), nil
}

// parsePage flattens a decoded GeckoTerminal page into candidates, skipping any row
// missing a needed field (pool address or either token relationship id). Pure (no I/O).
// An unparseable row is dropped with a debug log, never guessed at.
func parsePage(p geckoPage) []Candidate {
	out := make([]Candidate, 0, len(p.Data))
	for _, d := range p.Data {
		addr := strings.ToLower(strings.TrimSpace(d.Attributes.Address))
		if addr == "" {
			log.Debug().Msg("discover: gecko row missing pool address, skipping")
			continue
		}

		token0 := tokenAddrFromID(d.Relationships.BaseToken.Data.ID)
		token1 := tokenAddrFromID(d.Relationships.QuoteToken.Data.ID)
		if token0 == "" || token1 == "" {
			log.Debug().Str("pool", addr).Msg("discover: gecko row missing token id(s), skipping")
			continue
		}

		out = append(out, Candidate{
			PoolAddr:   addr,
			DexID:      strings.TrimSpace(d.Relationships.Dex.Data.ID),
			Token0Addr: token0,
			Token1Addr: token1,
			// A malformed or empty reserve parses to 0, failing the floor (safe default).
			ReserveUSD: parseUSD(d.Attributes.ReserveInUSD),
			// Same lenient parse for the 24h volume gate; 0 means the volume side of the
			// OR-gate cannot rescue a sub-floor-reserve pool.
			Vol24hUSD: parseUSD(d.Attributes.VolumeUSD.H24),
		})
	}
	return out
}

// tokenAddrFromID extracts the 0x token address from a GeckoTerminal token relationship
// id formatted "<network>_<address>" (e.g. "mantle_0x5d3a..."), lowercased, or "" when
// none is present. Only a hint to shortlist anchor pools; canonical tokens are on-chain.
func tokenAddrFromID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	// Locate the first "0x" rather than split on "_", which holds if a network slug has one.
	if i := strings.Index(strings.ToLower(id), "0x"); i >= 0 {
		return strings.ToLower(id[i:])
	}
	return ""
}
