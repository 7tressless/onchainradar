package attest

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// mockEthClient is a deterministic in-memory EthClient for exercising the nonce
// path without any network. It records every broadcast nonce and can be told to
// return a fixed "pending" nonce (the chain's view) and to fail the next send.
type mockEthClient struct {
	mu sync.Mutex

	// pending is what PendingNonceAt returns (the node's mempool view). The
	// manager takes max(localNonce, pending), so holding this at 0 lets the shared
	// local counter drive the assigned nonces; bumping it simulates the chain
	// advancing (used to prove a post-error resync reads the chain, not stale local).
	pending uint64

	// sentNonces is every nonce that reached SendTransaction, in arrival order.
	sentNonces []uint64

	// failNext, when > 0, makes the next N SendTransaction calls fail (decrementing
	// each time), to exercise the manager's reset-on-send-error -> resync path.
	failNext int

	// gasCalls / pendingCalls count RPCs for optional assertions.
	gasCalls     int64
	pendingCalls int64
}

func (m *mockEthClient) PendingNonceAt(_ context.Context, _ common.Address) (uint64, error) {
	atomic.AddInt64(&m.pendingCalls, 1)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending, nil
}

func (m *mockEthClient) SuggestGasPrice(_ context.Context) (*big.Int, error) {
	atomic.AddInt64(&m.gasCalls, 1)
	return big.NewInt(1_000_000), nil
}

func (m *mockEthClient) SendTransaction(_ context.Context, tx *types.Transaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext > 0 {
		m.failNext--
		// A real broadcast failure: the manager must reset and resync next time.
		return fmt.Errorf("mock: send failed")
	}
	m.sentNonces = append(m.sentNonces, tx.Nonce())
	return nil
}

func (m *mockEthClient) BalanceAt(_ context.Context, _ common.Address, _ *big.Int) (*big.Int, error) {
	// Healthy balance so New() does not emit the low-gas warning.
	return new(big.Int).Mul(big.NewInt(1), big.NewInt(1e18)), nil
}

func (m *mockEthClient) recorded() []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]uint64, len(m.sentNonces))
	copy(out, m.sentNonces)
	return out
}

// The two test keys are unrelated to any real wallet; they only need to be valid
// secp256k1 scalars so HexToECDSA / SignTx succeed.
const (
	testKeyA = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	testKeyB = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
)

// TestNonceManager_SharedAcrossAttestors_NoDuplicateNonce is the core regression
// test for the money path: two attestors that share one NonceManager (the
// signal+outcome supervisor topology) must never broadcast two txs with the same
// nonce, even under heavy concurrency. With PendingNonceAt pinned at 0 the assigned
// nonces are driven by the shared local counter, so a correct manager yields the
// contiguous set {0..N-1} with no duplicate and no gap. A per-attestor counter
// would instead produce colliding nonces.
func TestNonceManager_SharedAcrossAttestors_NoDuplicateNonce(t *testing.T) {
	ctx := context.Background()
	mock := &mockEthClient{pending: 0}
	nm := NewNonceManager()

	const addr = "0x1111111111111111111111111111111111111111"
	a1, err := New(ctx, mock, testKeyA, addr, 5000, nm)
	if err != nil {
		t.Fatalf("New(a1): %v", err)
	}
	// Second attestor: a different key but the same shared NonceManager, mirroring
	// the signal+outcome attestors sharing one wallet's nonce source. (Different keys
	// keep the test self-contained; the manager, not the key, owns serialisation.)
	a2, err := New(ctx, mock, testKeyB, addr, 5000, nm)
	if err != nil {
		t.Fatalf("New(a2): %v", err)
	}

	const perAttestor = 200
	const total = perAttestor * 2

	var wg sync.WaitGroup
	errs := make(chan error, total)
	fire := func(a *Attestor) {
		defer wg.Done()
		// signalHash/subject/type/score are irrelevant to nonce selection; use fixed
		// values. A non-nil error here is a real failure (the mock never errors in
		// this test), so surface it.
		if _, e := a.Attest(ctx, [32]byte{0xAB}, common.HexToAddress(addr), 1, 100); e != nil {
			errs <- e
		}
	}
	for i := 0; i < perAttestor; i++ {
		wg.Add(2)
		go fire(a1)
		go fire(a2)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent Attest returned error: %v", e)
	}

	got := mock.recorded()
	if len(got) != total {
		t.Fatalf("recorded %d sends, want %d", len(got), total)
	}

	// Every nonce must be distinct (no collision).
	seen := make(map[uint64]int, len(got))
	for _, n := range got {
		seen[n]++
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("nonce %d broadcast %d times (duplicate nonce -> collision)", n, c)
		}
	}

	// Distinct + contiguous {0..total-1} == monotonic coverage with no gap, which is
	// exactly what a single serialised counter produces.
	sorted := append([]uint64(nil), got...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for i, n := range sorted {
		if n != uint64(i) {
			t.Fatalf("nonces not contiguous/monotonic at index %d: got %d, want %d (full sorted set: %v)", i, n, i, sorted)
		}
	}
}

// TestNonceManager_ErrorResetsAndResyncs proves the reset-on-send-error semantics:
// after a SendTransaction failure the manager drops its local counter, so the next
// call resyncs the nonce from the chain (PendingNonceAt) instead of reusing a stale
// local value.
func TestNonceManager_ErrorResetsAndResyncs(t *testing.T) {
	ctx := context.Background()
	mock := &mockEthClient{pending: 0}
	nm := NewNonceManager()

	const addr = "0x2222222222222222222222222222222222222222"
	a, err := New(ctx, mock, testKeyA, addr, 5000, nm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 1) Five clean sends with pending=0 advance the local counter to 5 (nonces 0..4).
	for i := 0; i < 5; i++ {
		if _, e := a.Attest(ctx, [32]byte{byte(i)}, common.HexToAddress(addr), 1, 100); e != nil {
			t.Fatalf("clean send %d: %v", i, e)
		}
	}
	if got := mock.recorded(); len(got) != 5 || got[4] != 4 {
		t.Fatalf("after 5 clean sends, recorded=%v (want last nonce 4)", got)
	}

	// 2) Force the next broadcast to fail. The manager must reset (known=false). At
	//    this point pending is still 0, but local is 5; the failing attempt would
	//    have used max(5,0)=5.
	mock.mu.Lock()
	mock.failNext = 1
	mock.mu.Unlock()
	if _, e := a.Attest(ctx, [32]byte{0xFF}, common.HexToAddress(addr), 1, 100); e == nil {
		t.Fatalf("expected send error, got nil")
	}

	// 3) Simulate the chain having advanced to nonce 9 (e.g. the failed tx, or other
	//    activity, changed the pending view). Because the error reset the local
	//    counter, the next send must take PendingNonceAt (9), not the stale local 5.
	mock.mu.Lock()
	mock.pending = 9
	mock.mu.Unlock()
	if _, e := a.Attest(ctx, [32]byte{0x01}, common.HexToAddress(addr), 1, 100); e != nil {
		t.Fatalf("post-reset send: %v", e)
	}

	got := mock.recorded()
	last := got[len(got)-1]
	if last != 9 {
		t.Fatalf("post-error nonce = %d, want 9 (resynced from chain, not stale local 5); recorded=%v", last, got)
	}
}

// TestNonceManager_Send_PreSendErrorDoesNotReset checks the asymmetry: a failure
// before the broadcast (resync=false) must leave the counter intact, while a send
// failure (resync=true) clears it. Driven directly against the manager so the two
// branches are unambiguous.
func TestNonceManager_Send_PreSendErrorDoesNotReset(t *testing.T) {
	ctx := context.Background()
	nm := NewNonceManager()
	from := common.HexToAddress("0x3333333333333333333333333333333333333333")
	pending := func(context.Context, common.Address) (uint64, error) { return 0, nil }

	// One success -> local advances to 1.
	if err := nm.Send(ctx, from, pending, func(uint64) (bool, error) { return false, nil }); err != nil {
		t.Fatalf("first send: %v", err)
	}

	// A pre-send failure (resync=false) must not reset: the next call should still
	// prefer the local counter (1) over pending (0).
	wantErr := fmt.Errorf("gas boom")
	if err := nm.Send(ctx, from, pending, func(uint64) (bool, error) { return false, wantErr }); err == nil {
		t.Fatalf("expected pre-send error")
	}
	var seen uint64
	if err := nm.Send(ctx, from, pending, func(n uint64) (bool, error) { seen = n; return false, nil }); err != nil {
		t.Fatalf("send after pre-send error: %v", err)
	}
	if seen != 1 {
		t.Fatalf("pre-send error wrongly reset the counter: next nonce = %d, want 1", seen)
	}

	// A send failure (resync=true) must reset: with pending still 0 the next call
	// falls back to the chain value 0, not the stale local 2.
	if err := nm.Send(ctx, from, pending, func(uint64) (bool, error) { return true, fmt.Errorf("send boom") }); err == nil {
		t.Fatalf("expected send error")
	}
	if err := nm.Send(ctx, from, pending, func(n uint64) (bool, error) { seen = n; return false, nil }); err != nil {
		t.Fatalf("send after reset: %v", err)
	}
	if seen != 0 {
		t.Fatalf("send error did not reset the counter: next nonce = %d, want 0 (resync from pending)", seen)
	}
}
