package attest

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// EthClient is the minimal subset of *ethclient.Client the attestors and the startup
// balance preflight depend on, so tests can drive the nonce path with a mock.
// *ethclient.Client satisfies it as-is.
type EthClient interface {
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SuggestGasPrice(ctx context.Context) (*big.Int, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	BalanceAt(ctx context.Context, account common.Address, blockNumber *big.Int) (*big.Int, error)
}

// PendingNonceFunc fetches the next pending nonce for an account. It matches
// EthClient.PendingNonceAt exactly, so the real client's method value passes verbatim
// and a test can substitute a deterministic stand-in.
type PendingNonceFunc func(ctx context.Context, account common.Address) (uint64, error)

// SendFunc broadcasts the caller's transaction for the nonce the NonceManager assigned.
// It owns the per-contract calldata work (gas-price fetch, build, sign, SendTransaction);
// the manager owns only nonce selection and post-send bookkeeping.
//
// The resync flag tells the manager how to treat a failure: true means a broadcast was
// attempted and failed (the tx may have propagated), so drop the local counter and
// resync from chain next call; false means the failure was before broadcast (the nonce
// never hit the wire), so leave the counter untouched. On success resync is ignored.
type SendFunc func(nonce uint64) (resync bool, err error)

// NonceManager serialises every transaction from a single wallet through one mutex so
// two concurrent senders can never pick the same nonce. The signal Attestor, the
// OutcomeAttestor, and the recovery loop share one instance per wallet, so concurrent
// detect-stage and outcome-stage attests off the same key take distinct, monotonic
// nonces. Each send takes nonce = max(localNonce, PendingNonceAt) to tolerate restarts
// and propagation lag, advances past a broadcast nonce, and resyncs only after a failure.
type NonceManager struct {
	mu         sync.Mutex
	localNonce uint64
	known      bool // false until the first successfully broadcast nonce
}

// NewNonceManager returns a NonceManager with no cached nonce yet; the first Send
// resyncs from the chain via the supplied PendingNonceFunc.
func NewNonceManager() *NonceManager {
	return &NonceManager{}
}

// Send assigns nonce = max(localNonce, pending(from)) and invokes send under the
// manager's lock, so the whole "read pending → pick max → broadcast → commit" sequence
// is atomic against every other send from the same wallet. The local counter is used
// only once known and strictly ahead of the node's pending value. The send closure owns
// its own error wrapping (gas/sign/send), returned verbatim. No key material is touched.
func (m *NonceManager) Send(ctx context.Context, from common.Address, pending PendingNonceFunc, send SendFunc) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	pendingNonce, err := pending(ctx, from)
	if err != nil {
		return fmt.Errorf("attest: pending nonce for %s: %w", from.Hex(), err)
	}

	nonce := pendingNonce
	if m.known && m.localNonce > pendingNonce {
		// Our in-process counter is ahead of what the node's mempool reports
		// (propagation lag). Use our counter to avoid a nonce collision.
		nonce = m.localNonce
	}

	resync, err := send(nonce)
	if err != nil {
		if resync {
			// A real broadcast failed: resync from chain on the next call.
			m.known = false
		}
		return err
	}

	// Advance the local counter past the successfully submitted nonce.
	m.localNonce = nonce + 1
	m.known = true
	return nil
}
