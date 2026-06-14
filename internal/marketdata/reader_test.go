package marketdata

import (
	"context"
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"

	"ocr/internal/store"
	"ocr/internal/tokens"
)

// The reader is exercised end-to-end against a mock chain (balanceOf + slot0) and a mock
// bucket volume source, so the price derivation, TVL/volume valuation, and every
// degradation path (slot0 down, reserve read down, volume down, unpriced token, ctx
// cancel) are verified with no RPC or DB.

// --- test fixtures (verified anchor addresses + the real registry pool addresses) ---

var (
	agniUSDeWMNT = store.Pool{Address: "0xeafc4d6d4c3391cd4fc10c85d2f5f972d58c0dd5", Dex: "agni", Token0: tokens.USDe, Token1: tokens.WMNT, Dec0: 18, Dec1: 18}
	moeUSDeWMNT  = store.Pool{Address: "0x5d54d430d1fd9425976147318e6080479bffc16d", Dex: "merchant_moe", Token0: tokens.USDe, Token1: tokens.WMNT, Dec0: 18, Dec1: 18}
	moeWMNTUSDT  = store.Pool{Address: "0x365722f12ceb2063286a268b03c654df81b7c00f", Dex: "merchant_moe", Token0: tokens.WMNT, Token1: tokens.USDT, Dec0: 18, Dec1: 6}
	agniUSDCUSDe = store.Pool{Address: "0xbcf99c834e65e8a58090e20edc058279317865bd", Dex: "agni", Token0: tokens.USDC, Token1: tokens.USDe, Dec0: 6, Dec1: 18}
)

// sqrtOne is the sqrtPriceX96 for a 1:1 raw price (2^96), giving WMNT = $1 in the 18/18
// USDe/WMNT pool, so the happy-path arithmetic stays exact and readable.
var sqrtOne = new(big.Int).Lsh(big.NewInt(1), 96)

// Real Mantle addresses for the Liquidity Book test: cmETH (priced from a stable via Agni)
// and FBTC (priced only from cmETH in a Merchant Moe LB pool).
const (
	cmeth = "0xe6829d9a7ee3040e1276fa75293bde931859e8fa"
	fbtc  = "0xc96de26018a54d51c097160568752c4e3bd6c364"
)

type mockChain struct {
	balances  map[string]*big.Int  // "pool|token" -> raw balance
	sqrt      map[string]*big.Int  // pool -> sqrtPriceX96 at the head
	sqrtOld   map[string]*big.Int  // pool -> sqrtPriceX96 ~24h ago (historical read)
	bins      map[string][2]uint64 // pool -> {activeId, binStep} for Liquidity Book pools
	balErr    map[string]error     // "pool|token" -> injected error
	sqrtErr   map[string]error     // pool -> injected error (head and historical)
	binErr    map[string]error     // pool -> injected getActiveId error
	latest    uint64               // chain head; 0 -> a default well above blocksPerDay
	latestErr error                // when set, LatestBlock fails (24h change degrades to nil)
}

func (m *mockChain) LatestBlock(_ context.Context) (uint64, error) {
	if m.latestErr != nil {
		return 0, m.latestErr
	}
	if m.latest == 0 {
		return 50_000_000, nil
	}
	return m.latest, nil
}

func (m *mockChain) TokenBalance(_ context.Context, token, holder common.Address) (*big.Int, error) {
	key := lower(holder.Hex()) + "|" + lower(token.Hex())
	if err := m.balErr[key]; err != nil {
		return nil, err
	}
	if b, ok := m.balances[key]; ok {
		return b, nil
	}
	return big.NewInt(0), nil
}

func (m *mockChain) PoolSqrtPriceX96At(_ context.Context, pool common.Address, block *big.Int) (*big.Int, error) {
	key := lower(pool.Hex())
	if err := m.sqrtErr[key]; err != nil {
		return nil, err
	}
	if block != nil { // historical read for the 24h change
		if s, ok := m.sqrtOld[key]; ok {
			return s, nil
		}
		return nil, errors.New("no historical slot0")
	}
	if s, ok := m.sqrt[key]; ok {
		return s, nil
	}
	return nil, errors.New("no slot0 (not a V3 pool)")
}

func (m *mockChain) PoolActiveBinAt(_ context.Context, pool common.Address, _ *big.Int) (uint32, uint16, error) {
	key := lower(pool.Hex())
	if err := m.binErr[key]; err != nil {
		return 0, 0, err
	}
	if v, ok := m.bins[key]; ok {
		return uint32(v[0]), uint16(v[1]), nil
	}
	return 0, 0, errors.New("no active bin (not an LB pool)")
}

type mockVol struct {
	vols map[string][2]decimal.Decimal // pool -> {vol0, vol1}
	errs map[string]error              // pool -> injected error
}

func (m *mockVol) SumBucketVolumeSince(_ context.Context, pool string, _ time.Time) (decimal.Decimal, decimal.Decimal, error) {
	key := lower(pool)
	if err := m.errs[key]; err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	if v, ok := m.vols[key]; ok {
		return v[0], v[1], nil
	}
	return decimal.Zero, decimal.Zero, nil
}

func raw(whole int64, dec int) *big.Int {
	return new(big.Int).Mul(big.NewInt(whole), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil))
}

func rawDec(whole int64, dec int) decimal.Decimal {
	return decimal.NewFromBigInt(raw(whole, dec), 0)
}

func bk(p store.Pool, token string) string { return lower(p.Address) + "|" + lower(token) }

func wantF(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("got nil, want %v", want)
	}
	if math.Abs(*got-want) > 1e-6*math.Max(1, math.Abs(want)) {
		t.Fatalf("got %v, want %v", *got, want)
	}
}

func wantNil(t *testing.T, got *float64) {
	t.Helper()
	if got != nil {
		t.Fatalf("want nil, got %v", *got)
	}
}

// fullChain returns a mock chain wired with the four-pool reserves used across tests,
// optionally overriding the WMNT-source slot0.
func fullChain() *mockChain {
	return &mockChain{
		sqrt: map[string]*big.Int{lower(agniUSDeWMNT.Address): sqrtOne},
		// 24h ago: price0in1 = 4 (sqrt = 2*2^96), so WMNT was 1/4 = $0.25, and WMNT's $1
		// now is a +300% change.
		sqrtOld: map[string]*big.Int{lower(agniUSDeWMNT.Address): new(big.Int).Lsh(big.NewInt(1), 97)},
		balances: map[string]*big.Int{
			bk(agniUSDeWMNT, tokens.USDe): raw(1_000_000, 18),
			bk(agniUSDeWMNT, tokens.WMNT): raw(2_000_000, 18),
			bk(moeUSDeWMNT, tokens.USDe):  raw(900_000, 18),
			bk(moeUSDeWMNT, tokens.WMNT):  raw(1_000_000, 18),
			bk(moeWMNTUSDT, tokens.WMNT):  raw(1_000_000, 18),
			bk(moeWMNTUSDT, tokens.USDT):  raw(500_000, 6),
			bk(agniUSDCUSDe, tokens.USDC): raw(700_000, 6),
			bk(agniUSDCUSDe, tokens.USDe): raw(1_000_000, 18),
		},
		balErr:  map[string]error{},
		sqrtErr: map[string]error{},
	}
}

func fullVol() *mockVol {
	return &mockVol{
		vols: map[string][2]decimal.Decimal{
			lower(agniUSDeWMNT.Address): {rawDec(500_000, 18), decimal.Zero},
			lower(moeWMNTUSDT.Address):  {decimal.Zero, rawDec(300_000, 6)},
			lower(agniUSDCUSDe.Address): {rawDec(400_000, 6), decimal.Zero},
			lower(moeUSDeWMNT.Address):  {rawDec(250_000, 18), decimal.Zero},
		},
		errs: map[string]error{},
	}
}

var allPools = []store.Pool{agniUSDeWMNT, moeUSDeWMNT, moeWMNTUSDT, agniUSDCUSDe}

func TestSnapshotHappyPath(t *testing.T) {
	snap, err := NewReader(fullChain(), fullVol()).Snapshot(context.Background(), allPools)
	if err != nil {
		t.Fatal(err)
	}

	a := snap[lower(agniUSDeWMNT.Address)] // USDe(stable)/WMNT, WMNT derived to $1
	wantF(t, a.TVLUSD, 3_000_000)          // 1e6 USDe + 2e6 WMNT
	wantF(t, a.PriceUSD, 1)                // token0 USDe
	wantF(t, a.Vol24hUSD, 500_000)         // USDe stable side
	wantF(t, a.PriceChange24h, 0)          // token0 USDe is a flat $1 anchor
	if a.SourceURL != sourceOnchain {
		t.Fatalf("source = %q, want %q", a.SourceURL, sourceOnchain)
	}

	w := snap[lower(moeWMNTUSDT.Address)] // WMNT(derived)/USDT(stable), unequal decimals
	wantF(t, w.TVLUSD, 1_500_000)         // 1e6 WMNT + 5e5 USDT
	wantF(t, w.PriceUSD, 1)               // token0 WMNT priced from the Agni pool
	wantF(t, w.Vol24hUSD, 300_000)        // USDT stable side
	wantF(t, w.PriceChange24h, 300)       // WMNT $0.25 -> $1 over 24h

	c := snap[lower(agniUSDCUSDe.Address)] // both stable, no slot0 read needed
	wantF(t, c.TVLUSD, 1_700_000)          // 7e5 USDC + 1e6 USDe
	wantF(t, c.PriceUSD, 1)
	wantF(t, c.Vol24hUSD, 400_000)
	wantF(t, c.PriceChange24h, 0)

	m := snap[lower(moeUSDeWMNT.Address)] // LB pool valued off the derived WMNT price
	wantF(t, m.TVLUSD, 1_900_000)         // 9e5 USDe + 1e6 WMNT
	wantF(t, m.Vol24hUSD, 250_000)
}

func TestSnapshotSlot0FailureLeavesWMNTUnpriced(t *testing.T) {
	chain := fullChain()
	chain.sqrtErr[lower(agniUSDeWMNT.Address)] = errors.New("rpc down")

	snap, err := NewReader(chain, fullVol()).Snapshot(context.Background(), allPools)
	if err != nil {
		t.Fatal(err)
	}

	a := snap[lower(agniUSDeWMNT.Address)]
	wantNil(t, a.TVLUSD)           // WMNT leg unpriced -> no whole TVL
	wantF(t, a.PriceUSD, 1)        // token0 USDe still $1
	wantF(t, a.Vol24hUSD, 500_000) // USDe stable side still values volume

	w := snap[lower(moeWMNTUSDT.Address)]
	wantNil(t, w.TVLUSD)
	wantNil(t, w.PriceUSD)         // token0 WMNT has no price
	wantF(t, w.Vol24hUSD, 300_000) // USDT stable side unaffected

	c := snap[lower(agniUSDCUSDe.Address)] // pure-stable pool fully unaffected
	wantF(t, c.TVLUSD, 1_700_000)
	wantF(t, c.PriceUSD, 1)
	wantF(t, c.Vol24hUSD, 400_000)
}

func TestSnapshotReserveReadFailureOmitsTVLOnly(t *testing.T) {
	chain := fullChain()
	chain.balErr[bk(agniUSDCUSDe, tokens.USDC)] = errors.New("rpc down")

	snap, err := NewReader(chain, fullVol()).Snapshot(context.Background(), allPools)
	if err != nil {
		t.Fatal(err)
	}

	c := snap[lower(agniUSDCUSDe.Address)]
	wantNil(t, c.TVLUSD)           // one reserve read failed
	wantF(t, c.PriceUSD, 1)        // price independent of reserves
	wantF(t, c.Vol24hUSD, 400_000) // volume independent of reserves

	wantF(t, snap[lower(agniUSDeWMNT.Address)].TVLUSD, 3_000_000) // other pools intact
}

func TestSnapshotVolumeFailureOmitsVolumeOnly(t *testing.T) {
	vol := fullVol()
	vol.errs[lower(agniUSDeWMNT.Address)] = errors.New("db down")

	snap, err := NewReader(fullChain(), vol).Snapshot(context.Background(), allPools)
	if err != nil {
		t.Fatal(err)
	}

	a := snap[lower(agniUSDeWMNT.Address)]
	wantF(t, a.TVLUSD, 3_000_000)
	wantF(t, a.PriceUSD, 1)
	wantNil(t, a.Vol24hUSD) // only volume omitted
}

func TestSnapshotEmptyVolumeIsZeroNotNil(t *testing.T) {
	vol := fullVol()
	vol.vols[lower(agniUSDeWMNT.Address)] = [2]decimal.Decimal{decimal.Zero, decimal.Zero}

	snap, err := NewReader(fullChain(), vol).Snapshot(context.Background(), allPools)
	if err != nil {
		t.Fatal(err)
	}
	wantF(t, snap[lower(agniUSDeWMNT.Address)].Vol24hUSD, 0) // a real zero, present
}

func TestSnapshotUnpriceableNonStablePool(t *testing.T) {
	// WMNT/fake, no Agni pool to price either side: every USD field degrades to nil but
	// the pool is still returned with the source stamped.
	fake := "0x000000000000000000000000000000000000dead"
	orphan := store.Pool{Address: "0x1111111111111111111111111111111111111111", Dex: "merchant_moe", Token0: tokens.WMNT, Token1: fake, Dec0: 18, Dec1: 18}

	chain := &mockChain{
		sqrt:     map[string]*big.Int{},
		balances: map[string]*big.Int{bk(orphan, tokens.WMNT): raw(10, 18), bk(orphan, fake): raw(10, 18)},
		balErr:   map[string]error{},
		sqrtErr:  map[string]error{},
	}
	vol := &mockVol{vols: map[string][2]decimal.Decimal{lower(orphan.Address): {rawDec(5, 18), rawDec(5, 18)}}, errs: map[string]error{}}

	snap, err := NewReader(chain, vol).Snapshot(context.Background(), []store.Pool{orphan})
	if err != nil {
		t.Fatal(err)
	}
	o := snap[lower(orphan.Address)]
	wantNil(t, o.TVLUSD)
	wantNil(t, o.PriceUSD)
	wantNil(t, o.Vol24hUSD)
	wantNil(t, o.PriceChange24h)
	if o.SourceURL != sourceOnchain {
		t.Fatalf("source = %q, want %q", o.SourceURL, sourceOnchain)
	}
}

func TestSnapshotCtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewReader(fullChain(), fullVol()).Snapshot(ctx, allPools); err == nil {
		t.Fatal("a cancelled context must return an error")
	}
}

func TestSnapshotHistoricalUnavailableChangeNil(t *testing.T) {
	// LatestBlock failing means no ~24h-ago baseline at all: every change degrades to nil,
	// while the live TVL/price/volume fields are untouched.
	chain := fullChain()
	chain.latestErr = errors.New("head read failed")

	snap, err := NewReader(chain, fullVol()).Snapshot(context.Background(), allPools)
	if err != nil {
		t.Fatal(err)
	}
	wantF(t, snap[lower(agniUSDeWMNT.Address)].TVLUSD, 3_000_000) // live fields intact
	wantF(t, snap[lower(moeWMNTUSDT.Address)].PriceUSD, 1)
	wantNil(t, snap[lower(agniUSDeWMNT.Address)].PriceChange24h)
	wantNil(t, snap[lower(moeWMNTUSDT.Address)].PriceChange24h)
	wantNil(t, snap[lower(agniUSDCUSDe.Address)].PriceChange24h)
}

func TestSnapshotHistoricalWMNTMissingChangeNil(t *testing.T) {
	// Head slot0 present but the historical read returns nothing for the WMNT-source pool:
	// WMNT's 24h change is nil (no prior price) while its live price stays; stables, whose
	// $1 anchor holds at both blocks, still read a flat 0.
	chain := fullChain()
	chain.sqrtOld = map[string]*big.Int{}

	snap, err := NewReader(chain, fullVol()).Snapshot(context.Background(), allPools)
	if err != nil {
		t.Fatal(err)
	}
	w := snap[lower(moeWMNTUSDT.Address)]
	wantF(t, w.PriceUSD, 1)
	wantNil(t, w.PriceChange24h)
	wantF(t, snap[lower(agniUSDCUSDe.Address)].PriceChange24h, 0)
}

func TestBinPriceToPrice0in1(t *testing.T) {
	// The live cases pin the on-chain-verified formula + decimal convention against the
	// values measured from the real pools (see the empirical probe).
	cases := []struct {
		name       string
		activeID   uint32
		binStep    uint16
		dec0, dec1 int
		want, tol  float64
		ok         bool
	}{
		{"center bin is exactly 1", lbCenterBin, 25, 18, 18, 1.0, 1e-9, true},
		{"live FBTC/cmETH ratio", 8399251, 25, 8, 18, 34.7592, 1e-3, true},
		{"live WMNT/USDT ratio", 8377310, 25, 18, 6, 0.560609, 1e-5, true},
		{"zero bin step rejected", lbCenterBin, 0, 18, 18, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := binPriceToPrice0in1(c.activeID, c.binStep, c.dec0, c.dec1)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && math.Abs(got-c.want) > c.tol {
				t.Fatalf("price = %v, want %v ± %v", got, c.want, c.tol)
			}
		})
	}
}

func TestSnapshotLBBridgesToken(t *testing.T) {
	// FBTC lives only in a Merchant Moe LB pool, paired with cmETH; cmETH is priced from a
	// stable on Agni. The LB spot read must bridge cmETH -> FBTC so the LB pool gets full
	// TVL/price, with real decimals (8/18) and the live active bin.
	usdeCmeth := store.Pool{Address: "0xa1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", Dex: "agni", Token0: tokens.USDe, Token1: cmeth, Dec0: 18, Dec1: 18}
	fbtcLB := store.Pool{Address: "0xb2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2", Dex: "merchant_moe", Token0: fbtc, Token1: cmeth, Dec0: 8, Dec1: 18}

	chain := &mockChain{
		sqrt: map[string]*big.Int{lower(usdeCmeth.Address): sqrtOne}, // cmETH = 1/1 = $1
		bins: map[string][2]uint64{lower(fbtcLB.Address): {8399251, 25}},
		balances: map[string]*big.Int{
			bk(usdeCmeth, tokens.USDe): raw(1000, 18), bk(usdeCmeth, cmeth): raw(1000, 18),
			bk(fbtcLB, fbtc): raw(10, 8), bk(fbtcLB, cmeth): raw(100, 18),
		},
	}
	vol := &mockVol{vols: map[string][2]decimal.Decimal{lower(fbtcLB.Address): {rawDec(2, 8), rawDec(50, 18)}}}

	snap, err := NewReader(chain, vol).Snapshot(context.Background(), []store.Pool{usdeCmeth, fbtcLB})
	if err != nil {
		t.Fatal(err)
	}

	// Expected FBTC price = cmETH($1) * (cmETH per FBTC), recomputed independently here so
	// the assertion checks the read->dispatch->bridge wiring, not the formula's own output.
	price0in1 := math.Pow(1+25.0/10000, float64(8399251-8388608)) * math.Pow(10, float64(8-18))

	f := snap[lower(fbtcLB.Address)]
	wantF(t, f.PriceUSD, price0in1)                      // FBTC (token0), bridged from cmETH
	wantF(t, f.TVLUSD, 10*price0in1+100)                 // 10 FBTC * price + 100 cmETH * $1
	wantF(t, f.Vol24hUSD, 2*price0in1)                   // vol0 (FBTC) valued at FBTC's price
	wantF(t, snap[lower(usdeCmeth.Address)].PriceUSD, 1) // token0 USDe
}

func TestSnapshotLBReadFailureLeavesTokenUnpriced(t *testing.T) {
	usdeCmeth := store.Pool{Address: "0xa1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", Dex: "agni", Token0: tokens.USDe, Token1: cmeth, Dec0: 18, Dec1: 18}
	fbtcLB := store.Pool{Address: "0xb2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2", Dex: "merchant_moe", Token0: fbtc, Token1: cmeth, Dec0: 8, Dec1: 18}
	chain := &mockChain{
		sqrt:   map[string]*big.Int{lower(usdeCmeth.Address): sqrtOne},
		binErr: map[string]error{lower(fbtcLB.Address): errors.New("getActiveId reverted")},
		balances: map[string]*big.Int{
			bk(fbtcLB, fbtc): raw(10, 8), bk(fbtcLB, cmeth): raw(100, 18),
		},
	}
	snap, err := NewReader(chain, &mockVol{}).Snapshot(context.Background(), []store.Pool{usdeCmeth, fbtcLB})
	if err != nil {
		t.Fatal(err)
	}
	f := snap[lower(fbtcLB.Address)]
	wantNil(t, f.PriceUSD) // FBTC could not be priced (LB read failed)
	wantNil(t, f.TVLUSD)
}

func TestSqrtPriceToPrice0in1(t *testing.T) {
	cases := []struct {
		name       string
		sqrtP      *big.Int
		dec0, dec1 int
		want       float64
		ok         bool
	}{
		{"unit price equal decimals", sqrtOne, 18, 18, 1, true},
		{"token0 fewer decimals", sqrtOne, 6, 18, 1e-12, true},
		{"token0 more decimals", sqrtOne, 18, 6, 1e12, true},
		{"sqrt doubles squares to four", new(big.Int).Lsh(big.NewInt(1), 97), 18, 18, 4, true},
		{"zero is not a price", big.NewInt(0), 18, 18, 0, false},
		{"nil is not a price", nil, 18, 18, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := sqrtPriceToPrice0in1(c.sqrtP, c.dec0, c.dec1)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && math.Abs(got-c.want) > 1e-9*math.Max(1, math.Abs(c.want)) {
				t.Fatalf("price = %v, want %v", got, c.want)
			}
		})
	}
}

// ensure the package's lower() and the mock key scheme agree with go-ethereum address
// normalization, so a checksummed address from the reader still hits the mock map.
func TestAddressKeyNormalization(t *testing.T) {
	if got := lower(common.HexToAddress(tokens.WMNT).Hex()); got != tokens.WMNT {
		t.Fatalf("normalized %q, want %q", got, tokens.WMNT)
	}
	if !strings.HasPrefix(tokens.WMNT, "0x") {
		t.Fatal("anchor address must be 0x-prefixed lowercase hex")
	}
}
