package discover

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"ocr/internal/config"
)

// These tests cover the propose-then-verify pipeline (Discover) end-to-end with a mock
// discovery source and a mock on-chain verifier (no network, RPC, or DB). They prove the
// safety invariants:
//   - only anchor/anchor pools on a decodable dex above the liquidity floor are proposed;
//   - the on-chain tokens and decimals override whatever the API reports (a lying API
//     cannot get a non-anchor pool proposed);
//   - a per-candidate verify error skips that candidate without aborting the run;
//   - every proposal is enabled=false and labeled/decimaled from on-chain values.

// mockGecko is a hermetic GeckoClient: it returns a fixed candidate set (and an
// optional error) so the pipeline runs without any HTTP.
type mockGecko struct {
	cands []Candidate
	err   error
}

func (m *mockGecko) TopPools(_ context.Context, _ int) ([]Candidate, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.cands, nil
}

// mockVerifier is a hermetic ChainVerifier. It maps a pool address to the
// canonical on-chain tokens it should report (poolTokens), and a token address to
// its on-chain decimals (decimals). Missing entries return an error, so an
// unconfigured pool/token models an unverifiable candidate (a real eth_call that
// fails). It records calls so a test can assert the API's values were not used.
type mockVerifier struct {
	poolTokens map[string][2]common.Address // pool(lower) -> {token0, token1}
	decimals   map[string]uint8             // token(lower) -> decimals
	binSteps   map[string]uint16            // pool(lower) -> bin step (merchant_moe)
	tokenErr   map[string]error             // token(lower) -> forced decimals error
	poolErr    map[string]error             // pool(lower) -> forced tokens error
	binStepErr map[string]error             // pool(lower) -> forced bin-step error

	tokenCalls   map[string]int
	poolCalls    map[string]int
	binStepCalls map[string]int
}

func newMockVerifier() *mockVerifier {
	return &mockVerifier{
		poolTokens:   map[string][2]common.Address{},
		decimals:     map[string]uint8{},
		binSteps:     map[string]uint16{},
		tokenErr:     map[string]error{},
		poolErr:      map[string]error{},
		binStepErr:   map[string]error{},
		tokenCalls:   map[string]int{},
		poolCalls:    map[string]int{},
		binStepCalls: map[string]int{},
	}
}

func (m *mockVerifier) PoolTokens(_ context.Context, pool common.Address, dex string) (common.Address, common.Address, error) {
	key := lower(pool)
	m.poolCalls[key]++
	if err, ok := m.poolErr[key]; ok {
		return common.Address{}, common.Address{}, err
	}
	// Defensive: the pipeline must only call this for a supported dex.
	if dex != "agni" && dex != "merchant_moe" && dex != "fusionx" {
		return common.Address{}, common.Address{}, errors.New("mock: unsupported dex reached verifier")
	}
	t, ok := m.poolTokens[key]
	if !ok {
		return common.Address{}, common.Address{}, errors.New("mock: unknown pool tokens")
	}
	return t[0], t[1], nil
}

func (m *mockVerifier) TokenDecimals(_ context.Context, token common.Address) (uint8, error) {
	key := lower(token)
	m.tokenCalls[key]++
	if err, ok := m.tokenErr[key]; ok {
		return 0, err
	}
	d, ok := m.decimals[key]
	if !ok {
		return 0, errors.New("mock: unknown token decimals")
	}
	return d, nil
}

// PoolBinStep models the merchant_moe getBinStep() read. A configured forced error,
// or an unconfigured pool, returns an error, modeling a non-LB pool / RPC failure
// that Discover must tolerate (proposing with bin_step nil). It records calls so a
// test can assert Discover never calls it for an Agni pool.
func (m *mockVerifier) PoolBinStep(_ context.Context, pool common.Address) (uint16, error) {
	key := lower(pool)
	m.binStepCalls[key]++
	if err, ok := m.binStepErr[key]; ok {
		return 0, err
	}
	bs, ok := m.binSteps[key]
	if !ok {
		return 0, errors.New("mock: unknown bin step")
	}
	return bs, nil
}

// lower returns the lowercase hex of an address, the canonical mock-map key, so a
// pool/token configured via addrKey is found by a pipeline call passing that address.
func lower(a common.Address) string { return strings.ToLower(a.Hex()) }

// addr is a tiny helper to build a common.Address from a hex string.
func addr(h string) common.Address { return common.HexToAddress(h) }

// Anchor addresses reused across cases (must match the package anchorSymbols set).
const (
	wmnt = "0x78c1b0c915c4faa5fffa6cabf0219da63d7f4cb8"
	usde = "0x5d3a1ff2b6bab83b63cd9ad0787074081a52ef34"
	usdc = "0x09bc4e0d864854c6afb6eb9a9cdf58ac190d0df9"
	usdt = "0x201eba5cc46d216ce6dc03f6a759e8e766e956ae"
)

// testCfg returns a config whose reserve and volume floors are the same value, so a
// candidate that leaves Vol24hUSD at 0 is gated by reserve alone (a 0 volume floor would
// always pass the volume side of the OR-gate). The volume-side tests use testCfgVol.
func testCfg(floorUSD float64) *config.Config {
	return &config.Config{DiscoverMinLiquidityUSD: floorUSD, DiscoverMinVol24USD: floorUSD}
}

// testCfgVol returns a config with independent reserve and 24h-volume floors, for
// the tests that prove the volume-OR-reserve discovery gate (a sub-floor-reserve
// pool with high volume passes; a pool below both is skipped).
func testCfgVol(reserveFloorUSD, volFloorUSD float64) *config.Config {
	return &config.Config{DiscoverMinLiquidityUSD: reserveFloorUSD, DiscoverMinVol24USD: volFloorUSD}
}

// The happy path: a valid anchor/anchor pool on a supported dex that clears the
// floor and verifies on-chain is proposed exactly once, enabled=false, with the
// label/decimals taken from the on-chain values (not the API).
func TestDiscover_ProposesVerifiedAnchorPool(t *testing.T) {
	const pool = "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5"

	gt := &mockGecko{cands: []Candidate{{
		PoolAddr:   pool,
		DexID:      "agni",
		Token0Addr: usde, // API hint (correct here, but ignored)
		Token1Addr: wmnt,
		ReserveUSD: 3_210_000,
	}}}

	ch := newMockVerifier()
	// On-chain ordering: USDe, WMNT. Decimals 18/18.
	ch.poolTokens[addrKey(pool)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(wmnt)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 proposal, got %d", len(got))
	}
	p := got[0]
	if p.Enabled {
		t.Fatalf("proposal must be enabled=false, got enabled=true")
	}
	if p.Address != pool {
		t.Fatalf("address = %q, want %q", p.Address, pool)
	}
	if p.Dex != "agni" {
		t.Fatalf("dex = %q, want agni", p.Dex)
	}
	if p.Token0 != usde || p.Token1 != wmnt {
		t.Fatalf("tokens = %q/%q, want %q/%q (on-chain order)", p.Token0, p.Token1, usde, wmnt)
	}
	if p.Dec0 != 18 || p.Dec1 != 18 {
		t.Fatalf("decimals = %d/%d, want 18/18 (on-chain)", p.Dec0, p.Dec1)
	}
	if p.Label != "USDe/WMNT" {
		t.Fatalf("label = %q, want USDe/WMNT (from anchor symbols)", p.Label)
	}
	if p.SourceURL == "" {
		t.Fatalf("source_url must be set for a reviewer cross-reference")
	}
	// The on-chain accessors must have been consulted (proves verification ran).
	if ch.poolCalls[addrKey(pool)] == 0 {
		t.Fatalf("PoolTokens was never called; verification did not run")
	}
}

// The core safety test: an API that misreports a pool as anchor/anchor cannot get
// a non-anchor pool proposed. The API reports two anchors (USDe/WMNT), but the
// pool's real on-chain token0 is a non-anchor, so the pool is rejected. This proves
// anchor membership is decided on the on-chain tokens, never the API's.
func TestDiscover_OnChainTokensOverrideLyingAPI(t *testing.T) {
	const pool = "0x1111111111111111111111111111111111111111"
	const evilToken = "0x000000000000000000000000000000000000dead" // not an anchor

	gt := &mockGecko{cands: []Candidate{{
		PoolAddr:   pool,
		DexID:      "agni",
		Token0Addr: usde, // API claims anchor/anchor …
		Token1Addr: wmnt,
		ReserveUSD: 10_000_000,
	}}}

	ch := newMockVerifier()
	// … but on-chain, token0 is a non-anchor. The pool must be rejected.
	ch.poolTokens[addrKey(pool)] = [2]common.Address{addr(evilToken), addr(wmnt)}
	// Decimals are configured so that, if the pipeline wrongly trusted the API and
	// skipped the anchor check, it would still need these; their presence makes the
	// test fail for the right reason (anchor rejection), not a decimals miss.
	ch.decimals[addrKey(evilToken)] = 18
	ch.decimals[addrKey(wmnt)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a pool whose on-chain tokens are not both anchors must be rejected, got %d proposals: %+v", len(got), got)
	}
}

// A candidate on an unsupported dex (no decoder) is skipped before any on-chain
// call; proposing an undecodable pool would corrupt its signals.
func TestDiscover_SkipsUnsupportedDex(t *testing.T) {
	const pool = "0x2222222222222222222222222222222222222222"
	gt := &mockGecko{cands: []Candidate{{
		PoolAddr:   pool,
		DexID:      "fusionx-v3", // no decoder
		Token0Addr: usde,
		Token1Addr: wmnt,
		ReserveUSD: 10_000_000,
	}}}
	ch := newMockVerifier()
	// Configure on-chain anchors anyway: the dex gate must skip before verifying.
	ch.poolTokens[addrKey(pool)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(wmnt)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unsupported dex must be skipped, got %d proposals", len(got))
	}
	if ch.poolCalls[addrKey(pool)] != 0 {
		t.Fatalf("unsupported dex must be skipped before any on-chain call, but PoolTokens was called")
	}
}

// A candidate below the liquidity floor is skipped (coarse gate), and before any
// on-chain call (cheap filter first).
func TestDiscover_SkipsBelowLiquidityFloor(t *testing.T) {
	const pool = "0x3333333333333333333333333333333333333333"
	gt := &mockGecko{cands: []Candidate{{
		PoolAddr:   pool,
		DexID:      "agni",
		Token0Addr: usde,
		Token1Addr: wmnt,
		ReserveUSD: 50_000, // below the 100k reserve floor
		Vol24hUSD:  10_000, // and below the 100k volume floor -> skipped (both below)
	}}}
	ch := newMockVerifier()
	ch.poolTokens[addrKey(pool)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(wmnt)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("below-floor pool must be skipped, got %d proposals", len(got))
	}
	if ch.poolCalls[addrKey(pool)] != 0 {
		t.Fatalf("below-floor pool must be skipped before any on-chain call")
	}
}

// The volume-gate test: a candidate whose reserve is below the liquidity floor but
// whose 24h volume clears the volume floor passes the coarse discovery gate (the
// OR), proceeds to on-chain verification, and is proposed, so a high-volume,
// lower-TVL pool is not dropped for thin reserves.
func TestDiscover_HighVolumeSubFloorReservePasses(t *testing.T) {
	const pool = "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	gt := &mockGecko{cands: []Candidate{{
		PoolAddr:   pool,
		DexID:      "agni",
		Token0Addr: usde,
		Token1Addr: wmnt,
		ReserveUSD: 50_000,    // below the 100k reserve floor …
		Vol24hUSD:  1_000_000, // … but above the 250k volume floor -> eligible
	}}}
	ch := newMockVerifier()
	// On-chain anchors so it verifies once the gate lets it through.
	ch.poolTokens[addrKey(pool)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(wmnt)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfgVol(100_000, 250_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a sub-floor-reserve pool with high volume must PASS the gate and be proposed, got %d proposals", len(got))
	}
	if got[0].Address != pool {
		t.Fatalf("proposed wrong pool: %q, want %q", got[0].Address, pool)
	}
	// It must have proceeded to on-chain verification (the gate did not short-circuit it).
	if ch.poolCalls[addrKey(pool)] == 0 {
		t.Fatalf("a volume-eligible pool must reach on-chain verification (PoolTokens), but it was never called")
	}
}

// The mirror of the volume-gate test: a candidate below both the reserve floor and
// the volume floor is skipped, before any on-chain call (cheap filter first).
func TestDiscover_BelowBothFloorsSkipped(t *testing.T) {
	const pool = "0xffffffffffffffffffffffffffffffffffffffff"
	gt := &mockGecko{cands: []Candidate{{
		PoolAddr:   pool,
		DexID:      "agni",
		Token0Addr: usde,
		Token1Addr: wmnt,
		ReserveUSD: 50_000, // below the 100k reserve floor …
		Vol24hUSD:  10_000, // … and below the 250k volume floor -> skipped
	}}}
	ch := newMockVerifier()
	ch.poolTokens[addrKey(pool)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(wmnt)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfgVol(100_000, 250_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a pool below BOTH floors must be skipped, got %d proposals", len(got))
	}
	if ch.poolCalls[addrKey(pool)] != 0 {
		t.Fatalf("a below-both-floors pool must be skipped before any on-chain call")
	}
}

// A per-candidate verify error (PoolTokens fails, or TokenDecimals fails) skips
// just that candidate and does not abort the run: a healthy candidate later in the
// same batch is still proposed. This proves one unverifiable pool never loses the
// rest of the run.
func TestDiscover_VerifyErrorSkipsCandidateNotRun(t *testing.T) {
	const poolBadTokens = "0x4444444444444444444444444444444444444444"
	const poolBadDecimals = "0x5555555555555555555555555555555555555555"
	const poolGood = "0x6666666666666666666666666666666666666666"

	gt := &mockGecko{cands: []Candidate{
		{PoolAddr: poolBadTokens, DexID: "agni", Token0Addr: usde, Token1Addr: wmnt, ReserveUSD: 1_000_000},
		{PoolAddr: poolBadDecimals, DexID: "merchant_moe", Token0Addr: usde, Token1Addr: usdc, ReserveUSD: 1_000_000},
		{PoolAddr: poolGood, DexID: "agni", Token0Addr: usdc, Token1Addr: usde, ReserveUSD: 1_000_000},
	}}

	ch := newMockVerifier()
	// poolBadTokens: PoolTokens errors (RPC failure) -> skip.
	ch.poolErr[addrKey(poolBadTokens)] = errors.New("rpc: token0() reverted")
	// poolBadDecimals: tokens resolve to anchors, but its token1's decimals() call
	// errors -> skip. Use USDT as the failing token so the forced error does not
	// collide with poolGood's tokens (each pool must verify independently).
	ch.poolTokens[addrKey(poolBadDecimals)] = [2]common.Address{addr(usde), addr(usdt)}
	ch.tokenErr[addrKey(usdt)] = errors.New("rpc: decimals() reverted")
	// poolGood: fully resolvable (USDC/USDe) -> proposed.
	ch.poolTokens[addrKey(poolGood)] = [2]common.Address{addr(usdc), addr(usde)}
	ch.decimals[addrKey(usdc)] = 6
	ch.decimals[addrKey(usde)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("a per-candidate verify error must not abort the run, got error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("only the fully-verified pool should be proposed, got %d", len(got))
	}
	if got[0].Address != poolGood {
		t.Fatalf("proposed wrong pool: %q, want %q", got[0].Address, poolGood)
	}
	// The good pool's decimals come from on-chain (6/18), in on-chain order.
	if got[0].Dec0 != 6 || got[0].Dec1 != 18 {
		t.Fatalf("on-chain decimals wrong: %d/%d, want 6/18", got[0].Dec0, got[0].Dec1)
	}
	if got[0].Label != "USDC/USDe" {
		t.Fatalf("label = %q, want USDC/USDe", got[0].Label)
	}
}

// A mixed batch with one of each rejection reason plus one valid pool yields
// exactly the one valid proposal.
func TestDiscover_MixedBatchProposesOnlyValid(t *testing.T) {
	const (
		poolUnsupported = "0x7777777777777777777777777777777777777777"
		poolNonAnchor   = "0x8888888888888888888888888888888888888888"
		poolValid       = "0x9999999999999999999999999999999999999999"
		nonAnchor       = "0x000000000000000000000000000000000000beef"
	)

	gt := &mockGecko{cands: []Candidate{
		// unsupported dex
		{PoolAddr: poolUnsupported, DexID: "butter", Token0Addr: usde, Token1Addr: wmnt, ReserveUSD: 9_000_000},
		// API claims anchors, on-chain says non-anchor token1
		{PoolAddr: poolNonAnchor, DexID: "agni", Token0Addr: usde, Token1Addr: wmnt, ReserveUSD: 9_000_000},
		// valid
		{PoolAddr: poolValid, DexID: "merchant_moe", Token0Addr: usde, Token1Addr: wmnt, ReserveUSD: 9_000_000},
	}}

	ch := newMockVerifier()
	ch.poolTokens[addrKey(poolNonAnchor)] = [2]common.Address{addr(usde), addr(nonAnchor)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(nonAnchor)] = 18
	ch.poolTokens[addrKey(poolValid)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(wmnt)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 1 || got[0].Address != poolValid {
		t.Fatalf("expected only the valid pool proposed, got %+v", got)
	}
}

// A duplicate pool address within one candidate batch is proposed at most once.
func TestDiscover_DeduplicatesWithinBatch(t *testing.T) {
	const pool = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	gt := &mockGecko{cands: []Candidate{
		{PoolAddr: pool, DexID: "agni", Token0Addr: usde, Token1Addr: wmnt, ReserveUSD: 1_000_000},
		{PoolAddr: pool, DexID: "agni", Token0Addr: usde, Token1Addr: wmnt, ReserveUSD: 1_000_000},
	}}
	ch := newMockVerifier()
	ch.poolTokens[addrKey(pool)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(wmnt)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("duplicate pool must be proposed once, got %d", len(got))
	}
}

// A merchant_moe (Liquidity Book) proposal carries the on-chain bin step (read via
// PoolBinStep) so the API can build the add-liquidity deep-link; an Agni proposal
// carries bin_step nil and never triggers a bin-step read (Agni has no bin step).
func TestDiscover_BinStepForMoeOnly(t *testing.T) {
	const (
		moePool  = "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		agniPool = "0xcccccccccccccccccccccccccccccccccccccccc"
	)
	gt := &mockGecko{cands: []Candidate{
		{PoolAddr: moePool, DexID: "merchant-moe-v2-2", Token0Addr: usde, Token1Addr: wmnt, ReserveUSD: 1_000_000},
		{PoolAddr: agniPool, DexID: "agni", Token0Addr: usdc, Token1Addr: usde, ReserveUSD: 1_000_000},
	}}

	ch := newMockVerifier()
	// moe pool: USDe/WMNT on-chain, bin step 25 (one of the verified real values).
	ch.poolTokens[addrKey(moePool)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(wmnt)] = 18
	ch.binSteps[addrKey(moePool)] = 25
	// agni pool: USDC/USDe on-chain (no bin step).
	ch.poolTokens[addrKey(agniPool)] = [2]common.Address{addr(usdc), addr(usde)}
	ch.decimals[addrKey(usdc)] = 6

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 proposals, got %d: %+v", len(got), got)
	}

	byAddr := map[string]int{} // address -> bin step (or -1 for nil)
	for _, p := range got {
		if p.BinStep == nil {
			byAddr[p.Address] = -1
		} else {
			byAddr[p.Address] = *p.BinStep
		}
	}
	if byAddr[moePool] != 25 {
		t.Fatalf("merchant_moe proposal bin_step = %d, want 25", byAddr[moePool])
	}
	if byAddr[agniPool] != -1 {
		t.Fatalf("agni proposal bin_step = %d, want nil (-1)", byAddr[agniPool])
	}
	// The bin-step read must have run for the moe pool and never for the agni pool.
	if ch.binStepCalls[addrKey(moePool)] == 0 {
		t.Fatalf("PoolBinStep was never called for the merchant_moe pool")
	}
	if ch.binStepCalls[addrKey(agniPool)] != 0 {
		t.Fatalf("PoolBinStep must not be called for an Agni pool, got %d calls", ch.binStepCalls[addrKey(agniPool)])
	}
}

// A merchant_moe pool whose bin-step read fails (a non-LB pool / transient RPC error)
// is still proposed, with bin_step nil, never blocked. The backfill / a later
// `ocr poolmeta` run can fill it in.
func TestDiscover_BinStepReadFailureStillProposes(t *testing.T) {
	const moePool = "0xdddddddddddddddddddddddddddddddddddddddd"
	gt := &mockGecko{cands: []Candidate{
		{PoolAddr: moePool, DexID: "merchant_moe", Token0Addr: usde, Token1Addr: wmnt, ReserveUSD: 1_000_000},
	}}
	ch := newMockVerifier()
	ch.poolTokens[addrKey(moePool)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(wmnt)] = 18
	ch.binStepErr[addrKey(moePool)] = errors.New("rpc: getBinStep() reverted")

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("a bin-step read failure must not abort the run, got error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the pool to still be proposed, got %d", len(got))
	}
	if got[0].BinStep != nil {
		t.Fatalf("bin_step should be nil after a read failure, got %d", *got[0].BinStep)
	}
}

// A fetch error from the discovery source aborts the run with a wrapped error
// (never a silent empty result).
func TestDiscover_FetchErrorAborts(t *testing.T) {
	gt := &mockGecko{err: errors.New("gecko: 429 rate limited")}
	ch := newMockVerifier()
	_, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err == nil {
		t.Fatalf("expected a fetch error to abort the run")
	}
}

// Nil dependencies are rejected up front rather than panicking.
func TestDiscover_NilDeps(t *testing.T) {
	if _, err := Discover(context.Background(), nil, newMockVerifier(), testCfg(1)); err == nil {
		t.Fatalf("nil GeckoClient should error")
	}
	if _, err := Discover(context.Background(), &mockGecko{}, nil, testCfg(1)); err == nil {
		t.Fatalf("nil ChainVerifier should error")
	}
	if _, err := Discover(context.Background(), &mockGecko{}, newMockVerifier(), nil); err == nil {
		t.Fatalf("nil config should error")
	}
}

// addrKey returns the lowercase hex key used to index the mock maps, matching the
// `lower` helper the mock verifier uses, so a candidate's common.Address resolves
// to the configured tokens/decimals.
func addrKey(hexAddr string) string { return strings.ToLower(common.HexToAddress(hexAddr).Hex()) }

// canonicalDex must map only the DEX families OCR can decode (Agni / Merchant Moe)
// onto their canonical names (tolerating the API's versioned slugs) and reject
// everything else, since a pool on an undecodable DEX would corrupt signals. The
// canonical names returned here must match chain.PoolTokens's dex switch.
func TestCanonicalDex(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"agni", "agni", true},
		{"AGNI", "agni", true},
		{"agni-v3", "agni", true},
		{"merchant-moe-v2-2", "merchant_moe", true},
		{"merchant_moe", "merchant_moe", true},
		{"Merchant Moe", "merchant_moe", true},
		// FusionX: only the V2 (decodable UniV2) variant maps; V3 is undecodable.
		{"fusionx-v2", "fusionx", true},
		{"FusionX-V2", "fusionx", true}, // case-insensitive
		{"fusionx", "fusionx", true},    // bare "fusionx" defaults to the V2 family
		{"fusionx-v3", "", false},       // V3 swap event is non-standard -> unsupported
		{"butter", "", false},
		{"uniswap-v3", "", false},
		{"", "", false},
		{"moe", "", false},      // "moe" without "merchant" must not match
		{"merchant", "", false}, // "merchant" without "moe" must not match
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, ok := canonicalDex(tc.in)
			if name != tc.wantName || ok != tc.wantOK {
				t.Fatalf("canonicalDex(%q) = (%q,%v), want (%q,%v)", tc.in, name, ok, tc.wantName, tc.wantOK)
			}
			// A supported canonical name must be one chain.PoolTokens accepts.
			if ok && name != "agni" && name != "merchant_moe" && name != "fusionx" {
				t.Fatalf("canonicalDex(%q) returned unsupported canonical name %q", tc.in, name)
			}
		})
	}
}

// isAnchor matches strictly by address (any case), and only for the known set.
func TestIsAnchor(t *testing.T) {
	if !isAnchor(wmnt) || !isAnchor("0x78C1B0C915C4FAA5FFFA6CABF0219DA63D7F4CB8") {
		t.Fatalf("WMNT (any case) must be an anchor")
	}
	if isAnchor("0x000000000000000000000000000000000000dead") {
		t.Fatalf("an unknown token must not be an anchor")
	}
	if anchorSymbol(usdc) != "USDC" {
		t.Fatalf("anchorSymbol(USDC) = %q, want USDC", anchorSymbol(usdc))
	}
}

// A FusionX V2 anchor/anchor candidate is proposed end-to-end: canonicalDex maps
// "fusionx-v2" -> "fusionx" and the pipeline verifies it via the same token0()/
// token1() (agni-shape) accessor path. The mock verifier is reached with dex
// "fusionx" (its guard now admits it) and returns the on-chain pair.
func TestDiscover_ProposesFusionxV2Pool(t *testing.T) {
	const pool = "0x1212121212121212121212121212121212121212"
	gt := &mockGecko{cands: []Candidate{{
		PoolAddr:   pool,
		DexID:      "fusionx-v2",
		Token0Addr: usde,
		Token1Addr: wmnt,
		ReserveUSD: 1_000_000,
	}}}
	ch := newMockVerifier()
	ch.poolTokens[addrKey(pool)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(wmnt)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a verified FusionX V2 anchor pool must be proposed, got %d", len(got))
	}
	if got[0].Dex != "fusionx" {
		t.Fatalf("proposal dex = %q, want fusionx (canonical)", got[0].Dex)
	}
	if got[0].Token0 != usde || got[0].Token1 != wmnt {
		t.Fatalf("tokens = %q/%q, want %q/%q (on-chain order)", got[0].Token0, got[0].Token1, usde, wmnt)
	}
	if got[0].Enabled {
		t.Fatalf("proposal must be enabled=false")
	}
	// A FusionX (UniV2-shape) pool must not trigger a Liquidity Book bin-step read.
	if ch.binStepCalls[addrKey(pool)] != 0 {
		t.Fatalf("PoolBinStep must not be called for a fusionx pool, got %d calls", ch.binStepCalls[addrKey(pool)])
	}
}

// A FusionX V3 candidate is skipped before any on-chain call: its swap event is
// non-standard (no decoder), so canonicalDex returns ok=false and the unsupported-dex
// gate drops it. This guards the precision of the v2-only mapping: a v3 pool must
// never be proposed.
func TestDiscover_SkipsFusionxV3(t *testing.T) {
	const pool = "0x3434343434343434343434343434343434343434"
	gt := &mockGecko{cands: []Candidate{{
		PoolAddr:   pool,
		DexID:      "fusionx-v3",
		Token0Addr: usde,
		Token1Addr: wmnt,
		ReserveUSD: 10_000_000,
	}}}
	ch := newMockVerifier()
	// Configure on-chain anchors anyway: the dex gate must skip before verifying.
	ch.poolTokens[addrKey(pool)] = [2]common.Address{addr(usde), addr(wmnt)}
	ch.decimals[addrKey(usde)] = 18
	ch.decimals[addrKey(wmnt)] = 18

	got, err := Discover(context.Background(), gt, ch, testCfg(100_000))
	if err != nil {
		t.Fatalf("Discover error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a FusionX V3 pool (undecodable) must be skipped, got %d proposals", len(got))
	}
	if ch.poolCalls[addrKey(pool)] != 0 {
		t.Fatalf("an unsupported (v3) dex must be skipped before any on-chain call")
	}
}
