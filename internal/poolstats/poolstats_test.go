package poolstats

import (
	"context"
	"errors"
	"testing"

	"ocr/internal/discover"
	"ocr/internal/store"
)

// Hermetic tests for the poolstats stage: a mock fetcher + mock store exercise the
// refresh path, the per-pool failure-continues guarantee, and the nil-field ->
// NULL conversion. No network, no DB.

type mockFetcher struct {
	byPool   map[string]discover.PoolMarketStats
	errFor   map[string]error
	calls    []string
	spot     map[string]discover.TokenSpot // token0 address -> spot, served by TokenSpotPrices
	spotErr  error                         // when set, TokenSpotPrices fails (caller falls back)
	spotArgs []string                      // the addresses TokenSpotPrices was last asked for
}

func (m *mockFetcher) PoolStats(_ context.Context, addr string) (discover.PoolMarketStats, error) {
	m.calls = append(m.calls, addr)
	if err, ok := m.errFor[addr]; ok {
		return discover.PoolMarketStats{}, err
	}
	return m.byPool[addr], nil
}

func (m *mockFetcher) TokenSpotPrices(_ context.Context, addrs []string) (map[string]discover.TokenSpot, error) {
	m.spotArgs = addrs
	if m.spotErr != nil {
		return nil, m.spotErr
	}
	return m.spot, nil
}

type mockStore struct {
	pools     []store.Pool
	listErr   error
	upserts   []store.PoolStats
	upsertErr map[string]error
	tokenMeta []store.TokenMeta
	tokenErr  map[string]error // keyed by token address
}

func (m *mockStore) ListEnabledPools(_ context.Context) ([]store.Pool, error) {
	return m.pools, m.listErr
}

func (m *mockStore) UpsertPoolStats(_ context.Context, ps store.PoolStats) error {
	if err, ok := m.upsertErr[ps.Pool]; ok {
		return err
	}
	m.upserts = append(m.upserts, ps)
	return nil
}

func (m *mockStore) UpsertTokenMeta(_ context.Context, tm store.TokenMeta) error {
	if err, ok := m.tokenErr[tm.Address]; ok {
		return err
	}
	m.tokenMeta = append(m.tokenMeta, tm)
	return nil
}

func f64(v float64) *float64 { return &v }

// TestRunOnceSinglePoolNoSleep: a single pool means no inter-call spacing sleep,
// so the test is instant. Verifies the upsert carries the fetched values.
func TestRunOnceSinglePoolNoSleep(t *testing.T) {
	st := &mockStore{pools: []store.Pool{{Address: "0xa", Enabled: true}}}
	fe := &mockFetcher{byPool: map[string]discover.PoolMarketStats{
		"0xa": {PoolAddr: "0xa", TVLUSD: f64(1234.5), Vol24hUSD: f64(67.8), PriceUSD: f64(1.001), PriceChange24h: f64(2.0), BaseToken: "USDe", QuoteToken: "WMNT", SourceURL: "u/a"},
	}}
	s := New(st, fe, 0) // interval irrelevant for RunOnce

	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(fe.calls) != 1 || fe.calls[0] != "0xa" {
		t.Fatalf("fetch calls = %v, want [0xa]", fe.calls)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(st.upserts))
	}
	got := st.upserts[0]
	if got.Pool != "0xa" || got.TVLUSD == nil || got.TVLUSD.String() != "1234.5" {
		t.Fatalf("upsert tvl wrong: %+v", got)
	}
	if got.BaseToken != "USDe" || got.SourceURL != "u/a" {
		t.Fatalf("upsert token/source wrong: %+v", got)
	}
	if got.FetchedAt.IsZero() {
		t.Fatal("fetched_at should be stamped")
	}
}

// TestRunOnceNilFieldsBecomeNull: an upstream with missing numeric fields (nil)
// produces nil decimals on the store row (so the columns stay NULL, not 0).
func TestRunOnceNilFieldsBecomeNull(t *testing.T) {
	st := &mockStore{pools: []store.Pool{{Address: "0xa", Enabled: true}}}
	fe := &mockFetcher{byPool: map[string]discover.PoolMarketStats{
		"0xa": {PoolAddr: "0xa", SourceURL: "u/a"}, // all numeric fields nil
	}}
	s := New(st, fe, 0)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got := st.upserts[0]
	if got.TVLUSD != nil || got.Vol24hUSD != nil || got.PriceUSD != nil || got.PriceChange24h != nil {
		t.Fatalf("nil upstream fields must map to nil decimals, got %+v", got)
	}
}

// TestRunOncePerPoolFailureContinues: a fetch error on one pool must not abort the
// pass; the other pools are still upserted. (Single-failure-among-two; we tolerate
// one 2s spacing sleep here, which is acceptable for correctness coverage.)
func TestRunOncePerPoolFailureContinues(t *testing.T) {
	if testing.Short() {
		t.Skip("skips the spacing sleep in -short")
	}
	st := &mockStore{pools: []store.Pool{
		{Address: "0xbad", Enabled: true},
		{Address: "0xgood", Enabled: true},
	}}
	fe := &mockFetcher{
		byPool: map[string]discover.PoolMarketStats{
			"0xgood": {PoolAddr: "0xgood", TVLUSD: f64(99), SourceURL: "u/good"},
		},
		errFor: map[string]error{"0xbad": errors.New("rate limited")},
	}
	s := New(st, fe, 0)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// Both pools attempted; only the good one upserted.
	if len(fe.calls) != 2 {
		t.Fatalf("expected both pools fetched, got %v", fe.calls)
	}
	if len(st.upserts) != 1 || st.upserts[0].Pool != "0xgood" {
		t.Fatalf("only the good pool should upsert, got %+v", st.upserts)
	}
}

// TestRunDisabledIsNoOp: Run with a non-positive interval returns nil immediately
// (the supervisor gate), touching neither fetcher nor store.
func TestRunDisabledIsNoOp(t *testing.T) {
	st := &mockStore{pools: []store.Pool{{Address: "0xa", Enabled: true}}}
	fe := &mockFetcher{}
	s := New(st, fe, 0)
	if err := s.Run(context.Background()); err != nil {
		t.Fatalf("disabled Run should return nil, got %v", err)
	}
	if len(fe.calls) != 0 || len(st.upserts) != 0 {
		t.Fatal("disabled stage must not fetch or upsert")
	}
}

// TestRunOnceUpsertsTokenMeta: the stage upserts each token's display-only metadata
// (from PoolMarketStats.Tokens) keyed by address, in the same pass as pool_stats.
func TestRunOnceUpsertsTokenMeta(t *testing.T) {
	dec18, dec6 := 18, 6
	st := &mockStore{pools: []store.Pool{{Address: "0xa", Enabled: true}}}
	fe := &mockFetcher{byPool: map[string]discover.PoolMarketStats{
		"0xa": {
			PoolAddr: "0xa", TVLUSD: f64(1234.5), SourceURL: "u/a",
			Tokens: []discover.TokenMeta{
				{Address: "0xtok0", Symbol: "USDe", LogoURL: "https://logo/usde.png", Decimals: &dec18},
				{Address: "0xtok1", Symbol: "WMNT", LogoURL: "", Decimals: &dec6},
			},
		},
	}}
	s := New(st, fe, 0)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// pool_stats still upserted.
	if len(st.upserts) != 1 {
		t.Fatalf("pool_stats upserts = %d, want 1", len(st.upserts))
	}
	// Both tokens upserted, carrying symbol/logo/decimals + the provenance source.
	if len(st.tokenMeta) != 2 {
		t.Fatalf("token_meta upserts = %d, want 2: %+v", len(st.tokenMeta), st.tokenMeta)
	}
	byAddr := map[string]store.TokenMeta{}
	for _, tm := range st.tokenMeta {
		byAddr[tm.Address] = tm
	}
	if tm := byAddr["0xtok0"]; tm.Symbol != "USDe" || tm.LogoURL != "https://logo/usde.png" || tm.Decimals == nil || *tm.Decimals != 18 || tm.SourceURL != "u/a" {
		t.Fatalf("tok0 meta wrong: %+v", tm)
	}
	if tm := byAddr["0xtok1"]; tm.Symbol != "WMNT" || tm.LogoURL != "" || tm.Decimals == nil || *tm.Decimals != 6 {
		t.Fatalf("tok1 meta wrong: %+v", tm)
	}
}

// TestRunOnceTokenMetaFailureDoesNotAbort: a token_meta upsert failure is logged and
// skipped; it must not abort the pass or undo the pool_stats upsert, and the other
// token still lands.
func TestRunOnceTokenMetaFailureDoesNotAbort(t *testing.T) {
	st := &mockStore{
		pools:    []store.Pool{{Address: "0xa", Enabled: true}},
		tokenErr: map[string]error{"0xbad": errors.New("boom")},
	}
	fe := &mockFetcher{byPool: map[string]discover.PoolMarketStats{
		"0xa": {
			PoolAddr: "0xa", TVLUSD: f64(1), SourceURL: "u/a",
			Tokens: []discover.TokenMeta{
				{Address: "0xbad", Symbol: "BAD"},
				{Address: "0xok", Symbol: "OK"},
			},
		},
	}}
	s := New(st, fe, 0)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(st.upserts) != 1 {
		t.Fatalf("pool_stats should still upsert despite token failure, got %d", len(st.upserts))
	}
	// Only the good token landed (the failing one was skipped, not fatal).
	if len(st.tokenMeta) != 1 || st.tokenMeta[0].Address != "0xok" {
		t.Fatalf("only the good token should upsert, got %+v", st.tokenMeta)
	}
}

// TestApplySpotPriceOverride pins the price override: a pool whose token0 has a spot
// value takes the spot price + change; a pool whose token0 is absent from the map keeps
// its original per-pool price; and a spot entry with a nil field leaves that field's
// existing value untouched (never nulls a price we already have).
func TestApplySpotPriceOverride(t *testing.T) {
	change := func(v float64) *float64 { return &v }
	spot := map[string]discover.TokenSpot{
		// token0 of the "present" pool: full spot price + change.
		"0xtok0": {PriceUSD: f64(2015.34), Change24h: change(-1.15)},
		// token0 of the "price-only" pool: a price but no change.
		"0xtok2": {PriceUSD: f64(0.9998), Change24h: nil},
	}

	// Present: both fields overridden with the single spot value.
	present := discover.PoolMarketStats{PoolAddr: "0xpA", PriceUSD: f64(1760.0), PriceChange24h: change(5.0)}
	applySpotPrice(&present, "0xTOK0", spot) // mixed case token0 still matches (lowercased)
	if present.PriceUSD == nil || *present.PriceUSD != 2015.34 {
		t.Fatalf("present price = %v, want spot 2015.34", present.PriceUSD)
	}
	if present.PriceChange24h == nil || *present.PriceChange24h != -1.15 {
		t.Fatalf("present change = %v, want spot -1.15", present.PriceChange24h)
	}

	// Missing: token0 not in the map, so the original per-pool price is kept.
	missing := discover.PoolMarketStats{PoolAddr: "0xpB", PriceUSD: f64(1760.0), PriceChange24h: change(5.0)}
	applySpotPrice(&missing, "0xtok1", spot)
	if missing.PriceUSD == nil || *missing.PriceUSD != 1760.0 {
		t.Fatalf("missing-token0 price = %v, want original 1760", missing.PriceUSD)
	}
	if missing.PriceChange24h == nil || *missing.PriceChange24h != 5.0 {
		t.Fatalf("missing-token0 change = %v, want original 5", missing.PriceChange24h)
	}

	// Price-only spot: the nil change must not overwrite the existing change.
	priceOnly := discover.PoolMarketStats{PoolAddr: "0xpC", PriceUSD: f64(1.0), PriceChange24h: change(3.3)}
	applySpotPrice(&priceOnly, "0xtok2", spot)
	if priceOnly.PriceUSD == nil || *priceOnly.PriceUSD != 0.9998 {
		t.Fatalf("price-only price = %v, want spot 0.9998", priceOnly.PriceUSD)
	}
	if priceOnly.PriceChange24h == nil || *priceOnly.PriceChange24h != 3.3 {
		t.Fatalf("price-only change = %v, want original 3.3 (nil spot change must not null it)", priceOnly.PriceChange24h)
	}
}

// TestRunOnceOverridesPriceFromSpot is the end-to-end stage check: two pools sharing the
// same token0 both upsert that token0's single spot price (not their differing per-pool
// prices), while a pool whose token0 has no spot keeps its per-pool price. tvl/vol always
// stay per-pool. One spacing sleep across three pools is tolerated for coverage.
func TestRunOnceOverridesPriceFromSpot(t *testing.T) {
	if testing.Short() {
		t.Skip("skips the spacing sleeps in -short")
	}
	pools := []store.Pool{
		{Address: "0xpoolA", Token0: "0xshared", Enabled: true},
		{Address: "0xpoolB", Token0: "0xshared", Enabled: true},
		{Address: "0xpoolC", Token0: "0xlonely", Enabled: true},
	}
	st := &mockStore{pools: pools}
	fe := &mockFetcher{
		byPool: map[string]discover.PoolMarketStats{
			// Same token, two different reserve-derived prices upstream.
			"0xpoolA": {PoolAddr: "0xpoolA", TVLUSD: f64(100), PriceUSD: f64(2013.0), PriceChange24h: f64(1.0)},
			"0xpoolB": {PoolAddr: "0xpoolB", TVLUSD: f64(200), PriceUSD: f64(1760.0), PriceChange24h: f64(9.0)},
			// token0 has no spot entry: per-pool price must survive.
			"0xpoolC": {PoolAddr: "0xpoolC", TVLUSD: f64(300), PriceUSD: f64(42.0), PriceChange24h: f64(2.0)},
		},
		spot: map[string]discover.TokenSpot{
			"0xshared": {PriceUSD: f64(2000.0), Change24h: f64(-0.5)},
		},
	}
	s := New(st, fe, 0)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Distinct token0s were requested once (de-dup is the client's job, but the stage
	// passes both pool rows' token0 here; the lonely one is included so the gather is
	// correct).
	if len(fe.spotArgs) == 0 {
		t.Fatalf("expected TokenSpotPrices to be called with token0 addresses, got %v", fe.spotArgs)
	}

	byPool := map[string]store.PoolStats{}
	for _, u := range st.upserts {
		byPool[u.Pool] = u
	}
	// Both shared-token pools read the single spot price, despite different upstream prices.
	for _, addr := range []string{"0xpoolA", "0xpoolB"} {
		u := byPool[addr]
		if u.PriceUSD == nil || u.PriceUSD.String() != "2000" {
			t.Fatalf("%s price = %v, want spot 2000", addr, u.PriceUSD)
		}
		if u.PriceChange24h == nil || u.PriceChange24h.String() != "-0.5" {
			t.Fatalf("%s change = %v, want spot -0.5", addr, u.PriceChange24h)
		}
	}
	// Per-pool tvl is preserved (not overridden).
	if u := byPool["0xpoolA"]; u.TVLUSD == nil || u.TVLUSD.String() != "100" {
		t.Fatalf("poolA tvl = %v, want per-pool 100", u.TVLUSD)
	}
	if u := byPool["0xpoolB"]; u.TVLUSD == nil || u.TVLUSD.String() != "200" {
		t.Fatalf("poolB tvl = %v, want per-pool 200", u.TVLUSD)
	}
	// The lonely pool (no spot for its token0) keeps its per-pool price.
	if u := byPool["0xpoolC"]; u.PriceUSD == nil || u.PriceUSD.String() != "42" {
		t.Fatalf("poolC price = %v, want per-pool 42 (no spot)", u.PriceUSD)
	}
}

// TestToStorePoolStatsConversion pins the float->decimal (nil-preserving) mapping.
func TestToStorePoolStatsConversion(t *testing.T) {
	ms := discover.PoolMarketStats{
		PoolAddr: "0xa", TVLUSD: f64(100.25), PriceChange24h: f64(-1.5),
		BaseToken: "USDe", SourceURL: "u",
	}
	got := toStorePoolStats(ms)
	if got.TVLUSD == nil || got.TVLUSD.String() != "100.25" {
		t.Fatalf("tvl = %v", got.TVLUSD)
	}
	if got.PriceChange24h == nil || got.PriceChange24h.String() != "-1.5" {
		t.Fatalf("change = %v", got.PriceChange24h)
	}
	if got.Vol24hUSD != nil || got.PriceUSD != nil {
		t.Fatalf("absent fields should be nil: %+v", got)
	}
}
