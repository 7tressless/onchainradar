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

	// geckoMarketBaseURL is the CoinGecko coin-level REST root. Token spot prices come
	// from here when a key is set, because the onchain root prices a token only from one
	// network's own DEX pools, which drift when the token's depth lives elsewhere.
	// Requires the same x-cg-demo-api-key auth; no keyless equivalent.
	geckoMarketBaseURL = "https://api.coingecko.com/api/v3"

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

	// geckoSpotBatch caps the token addresses in one simple-token-price request, keeping
	// each comma-list URL within length limits and at the documented per-call ceiling.
	geckoSpotBatch = 30
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

// TokenSpot is one token's display price and 24h change, one value per token rather than
// per pool. Display-only, never detection. A missing or unparseable field stays nil so
// the caller keeps its existing value rather than nulling it.
type TokenSpot struct {
	PriceUSD  *float64
	Change24h *float64 // percent
}

// geckoSimpleTokenPrice mirrors the consumed subset of
// GET /simple/networks/{net}/token_price/{addrs}. Both maps are keyed by the lowercased
// token address; values are decimal strings parsed leniently (junk -> nil).
type geckoSimpleTokenPrice struct {
	Data struct {
		Attributes struct {
			TokenPrices              map[string]string `json:"token_prices"`
			H24PriceChangePercentage map[string]string `json:"h24_price_change_percentage"`
		} `json:"attributes"`
	} `json:"data"`
}

// geckoMarketQuote is one token's quote in a coin-level simple/token_price response:
// {"0xaddr": {"usd": 1766.26, "usd_24h_change": -1.9}}. Pointers so an absent field
// stays nil rather than a bogus 0.
type geckoMarketQuote struct {
	USD       *float64 `json:"usd"`
	Change24h *float64 `json:"usd_24h_change"`
}

// TokenSpotPrices fetches one price and 24h change per token address, so a token reads
// the same price everywhere instead of a per-pool reserve-derived figure. With an API key
// it returns CoinGecko's coin-level market price, aggregated across venues; a price from
// one network's own pools drifts when the token's depth is elsewhere (mETH on thin Mantle
// pools read 14% above market). Keyless falls back to the GeckoTerminal network-derived
// price. Addresses are lowercased, de-duplicated, fetched in batches of geckoSpotBatch,
// and merged into one map keyed by lowercased address; a token absent from the response
// is simply absent from the map. Transport, non-200, and decode failures are wrapped and
// returned so the caller can fall back to per-pool prices. Display-only.
func (g *Gecko) TokenSpotPrices(ctx context.Context, addrs []string) (map[string]TokenSpot, error) {
	// Lowercase + dedupe, preserving first-seen order so batches are stable across runs.
	seen := make(map[string]bool, len(addrs))
	uniq := make([]string, 0, len(addrs))
	for _, a := range addrs {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		uniq = append(uniq, a)
	}

	// The coin-level endpoint needs auth, so it is only selected when a key exists.
	fetch := g.fetchSpotOnchain
	if g.apiKey != "" {
		fetch = g.fetchSpotMarket
	}

	out := make(map[string]TokenSpot, len(uniq))
	for start := 0; start < len(uniq); start += geckoSpotBatch {
		end := start + geckoSpotBatch
		if end > len(uniq) {
			end = len(uniq)
		}
		if err := ctx.Err(); err != nil {
			return out, fmt.Errorf("discover: gecko token spot context cancelled: %w", err)
		}
		if err := fetch(ctx, uniq[start:end], out); err != nil {
			return out, err
		}
	}
	return out, nil
}

// fetchSpotMarket GETs one batch of contract addresses from the coin-level CoinGecko
// endpoint and merges the market quotes into dst. CoinGecko's asset-platform id for
// Mantle equals the GeckoTerminal network slug, so geckoNetwork serves both.
func (g *Gecko) fetchSpotMarket(ctx context.Context, batch []string, dst map[string]TokenSpot) error {
	list := strings.Join(batch, ",")
	url := fmt.Sprintf("%s/simple/token_price/%s?contract_addresses=%s&vs_currencies=usd&include_24hr_change=true",
		geckoMarketBaseURL, geckoNetwork, list)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("discover: gecko market price build request: %w", err)
	}
	g.setHeaders(req)

	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("discover: gecko market price http get %s: %w", url, err)
	}
	defer func() {
		if _, derr := io.Copy(io.Discard, resp.Body); derr != nil {
			log.Debug().Err(derr).Msg("discover: drain gecko market-price body")
		}
		if cerr := resp.Body.Close(); cerr != nil {
			log.Debug().Err(cerr).Msg("discover: close gecko market-price body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("discover: gecko market price non-200 status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var quotes map[string]geckoMarketQuote
	if err := json.NewDecoder(resp.Body).Decode(&quotes); err != nil {
		return fmt.Errorf("discover: gecko market price decode json: %w", err)
	}

	mergeMarketPrices(quotes, dst)
	return nil
}

// mergeMarketPrices folds a coin-level quote map into dst, keyed by lowercased address.
// Pure (no I/O) for unit tests. A quote carrying neither field is omitted so the caller's
// fallback (keep the per-pool price) applies.
func mergeMarketPrices(quotes map[string]geckoMarketQuote, dst map[string]TokenSpot) {
	for addr, q := range quotes {
		key := strings.ToLower(strings.TrimSpace(addr))
		if key == "" || (q.USD == nil && q.Change24h == nil) {
			continue
		}
		dst[key] = TokenSpot{PriceUSD: q.USD, Change24h: q.Change24h}
	}
}

// fetchSpotOnchain GETs one comma-joined batch of token addresses from the network-level
// simple-token-price endpoint and merges the parsed prices into dst. The keyless path: the
// figure is derived from this network's pools, so it is pool-consistent but can sit off the
// market level for thinly pooled tokens.
func (g *Gecko) fetchSpotOnchain(ctx context.Context, batch []string, dst map[string]TokenSpot) error {
	list := strings.Join(batch, ",")
	url := fmt.Sprintf("%s/simple/networks/%s/token_price/%s?include_24hr_price_change=true", g.baseURL, geckoNetwork, list)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("discover: gecko token spot build request: %w", err)
	}
	g.setHeaders(req)

	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("discover: gecko token spot http get %s: %w", url, err)
	}
	defer func() {
		if _, derr := io.Copy(io.Discard, resp.Body); derr != nil {
			log.Debug().Err(derr).Msg("discover: drain gecko token-spot body")
		}
		if cerr := resp.Body.Close(); cerr != nil {
			log.Debug().Err(cerr).Msg("discover: close gecko token-spot body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("discover: gecko token spot non-200 status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var body geckoSimpleTokenPrice
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("discover: gecko token spot decode json: %w", err)
	}

	mergeSpotPrices(body, dst)
	return nil
}

// mergeSpotPrices folds one decoded simple-token-price response into dst, keyed by
// lowercased address. Pure (no I/O). A price and its change are parsed leniently (junk or
// empty -> nil); an address carrying neither a usable price nor change is omitted.
func mergeSpotPrices(body geckoSimpleTokenPrice, dst map[string]TokenSpot) {
	a := body.Data.Attributes
	for addr, raw := range a.TokenPrices {
		key := strings.ToLower(strings.TrimSpace(addr))
		if key == "" {
			continue
		}
		spot := TokenSpot{
			PriceUSD:  parseFloatPtr(raw),
			Change24h: parseFloatPtr(a.H24PriceChangePercentage[addr]),
		}
		if spot.PriceUSD == nil && spot.Change24h == nil {
			continue // nothing usable for this token
		}
		dst[key] = spot
	}
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
