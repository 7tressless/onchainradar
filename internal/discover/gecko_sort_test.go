package discover

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These tests cover the two-pass discovery in TopPools: fetchPage's optional &sort query
// param (default pass sends none, volume pass sends &sort=h24_volume_usd_desc) and
// TopPools' cross-pass de-duplication by pool address. fetchPage and TopPools hit the
// network, so they run against a local httptest server.
//
// The per-fetch courtesy sleep (geckoPageSleep) is real, so these tests keep `pages` at 1:
// exactly two fetches (one per pass) with a single sleep between them, exercising both
// orderings and the seam while keeping the suite fast.

// onePoolPageJSON builds a minimal but valid JSON:API pools page containing exactly
// one pool with the given pool address and dex id. Both token relationship ids carry
// the "<net>_0x…" form parsePage extracts, so the row maps to a Candidate. The
// numeric attributes are present and parseable (their exact values are irrelevant to
// the dedupe assertions).
func onePoolPageJSON(poolAddr, dexID, baseTok, quoteTok string) string {
	return fmt.Sprintf(`{
      "data": [
        {
          "attributes": {
            "address": %q,
            "reserve_in_usd": "1000000",
            "volume_usd": { "h24": "500000" }
          },
          "relationships": {
            "dex":         { "data": { "id": %q } },
            "base_token":  { "data": { "id": "mantle_%s" } },
            "quote_token": { "data": { "id": "mantle_%s" } }
          }
        }
      ]
    }`, poolAddr, dexID, baseTok, quoteTok)
}

// emptyPageJSON is a JSON:API page with no pools; signals end-of-listing.
const emptyPageJSON = `{ "data": [] }`

// newGeckoForServer builds a *Gecko whose baseURL points at a test server, so the
// live fetchPage/TopPools paths run without real network. apiKey "" keeps the keyless
// header behavior (no auth header), irrelevant to ordering/dedupe.
func newGeckoForServer(serverURL string) *Gecko {
	g := NewGecko("")
	g.baseURL = serverURL
	return g
}

// TestFetchPage_SortParam pins the URL contract: the default pass (sort == "") sends
// no sort query param, while a non-empty sort appends exactly &sort=<value>. The
// handler records the raw RequestURI so the query string is asserted directly.
func TestFetchPage_SortParam(t *testing.T) {
	var gotURIs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURIs = append(gotURIs, r.RequestURI)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, emptyPageJSON)
	}))
	defer srv.Close()

	g := newGeckoForServer(srv.URL)
	ctx := context.Background()

	// Default ordering: no sort param at all.
	if _, err := g.fetchPage(ctx, 1, ""); err != nil {
		t.Fatalf("fetchPage(default) error: %v", err)
	}
	// Volume ordering: &sort=h24_volume_usd_desc appended.
	if _, err := g.fetchPage(ctx, 2, geckoSortVolume); err != nil {
		t.Fatalf("fetchPage(volume) error: %v", err)
	}

	if len(gotURIs) != 2 {
		t.Fatalf("expected 2 requests, got %d: %v", len(gotURIs), gotURIs)
	}

	// Pass 1: default, page param present, no sort param.
	def := gotURIs[0]
	if !strings.Contains(def, "page=1") {
		t.Fatalf("default request missing page=1: %q", def)
	}
	if strings.Contains(def, "sort") {
		t.Fatalf("default request must NOT carry a sort param: %q", def)
	}

	// Pass 2: volume, exact &sort=h24_volume_usd_desc appended.
	vol := gotURIs[1]
	if !strings.Contains(vol, "page=2") {
		t.Fatalf("volume request missing page=2: %q", vol)
	}
	if !strings.Contains(vol, "&sort="+geckoSortVolume) {
		t.Fatalf("volume request missing &sort=%s: %q", geckoSortVolume, vol)
	}
}

// TestTopPools_TwoPassDedupe proves the merge semantics over both orderings:
//   - a pool returned by both passes appears exactly once (first-seen wins);
//   - a pool returned only by the volume pass is still included (the whole point of
//     the second pass);
//   - the default pass still drives ordering for the shared pool.
//
// The handler returns different single-pool pages depending on whether the request
// carries the volume sort param, modeling two genuinely different orderings.
func TestTopPools_TwoPassDedupe(t *testing.T) {
	const (
		shared    = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // in both passes
		volOnly   = "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" // in the volume pass only
		baseToken = "0x1111111111111111111111111111111111111111"
		quoteTok  = "0x2222222222222222222222222222222222222222"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		isVolume := strings.Contains(r.URL.RawQuery, "sort="+geckoSortVolume)
		switch {
		case !isVolume && r.URL.Query().Get("page") == "1":
			// Default pass, page 1: the shared pool.
			fmt.Fprint(w, onePoolPageJSON(shared, "agni", baseToken, quoteTok))
		case isVolume && r.URL.Query().Get("page") == "1":
			// Volume pass, page 1: the shared pool again (must dedupe) plus a
			// volume-only pool (must be added). Two-pool page.
			fmt.Fprint(w, twoPoolPageJSON(shared, volOnly, baseToken, quoteTok))
		default:
			// Any page>1 in either pass: end of that listing.
			fmt.Fprint(w, emptyPageJSON)
		}
	}))
	defer srv.Close()

	g := newGeckoForServer(srv.URL)

	out, err := g.TopPools(context.Background(), 1)
	if err != nil {
		t.Fatalf("TopPools error: %v", err)
	}

	// Exactly two distinct pools: shared (once) + volOnly.
	if len(out) != 2 {
		t.Fatalf("expected 2 deduped candidates, got %d: %+v", len(out), out)
	}

	seen := map[string]int{}
	for _, c := range out {
		seen[c.PoolAddr]++
	}
	if seen[shared] != 1 {
		t.Fatalf("shared pool should appear exactly once, got %d: %+v", seen[shared], out)
	}
	if seen[volOnly] != 1 {
		t.Fatalf("volume-only pool should be included exactly once, got %d: %+v", seen[volOnly], out)
	}

	// First-seen wins: the shared pool comes from the default pass and is emitted
	// before the volume-only pool discovered later.
	if out[0].PoolAddr != shared {
		t.Fatalf("expected shared pool first (default pass), got %q first: %+v", out[0].PoolAddr, out)
	}
}

// twoPoolPageJSON builds a JSON:API page with two pools sharing the same token pair,
// used for the volume pass, which returns a previously-seen pool plus a new one.
func twoPoolPageJSON(poolA, poolB, baseTok, quoteTok string) string {
	pool := func(addr string) string {
		return fmt.Sprintf(`{
          "attributes": {
            "address": %q,
            "reserve_in_usd": "750000",
            "volume_usd": { "h24": "900000" }
          },
          "relationships": {
            "dex":         { "data": { "id": "agni" } },
            "base_token":  { "data": { "id": "mantle_%s" } },
            "quote_token": { "data": { "id": "mantle_%s" } }
          }
        }`, addr, baseTok, quoteTok)
	}
	return fmt.Sprintf(`{ "data": [ %s, %s ] }`, pool(poolA), pool(poolB))
}

// TestTopPools_PageErrorAborts confirms the preserved error contract carries over to
// the two-pass walk: a non-200 on a fetch wraps + aborts the run (no silent partial).
func TestTopPools_PageErrorAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	g := newGeckoForServer(srv.URL)
	// Short deadline guard so a logic regression that loops/sleeps can't hang CI.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := g.TopPools(ctx, 1)
	if err == nil {
		t.Fatal("expected an error from a non-200 page, got nil")
	}
	if !strings.Contains(err.Error(), "non-200") {
		t.Fatalf("error should wrap the non-200 status, got: %v", err)
	}
}
