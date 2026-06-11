// Package chain provides a thin polling EVM client for OCR ingest on Mantle.
//
// It polls the chain head and reads blocks/headers/logs over HTTP rather than
// subscribing to WebSocket newHeads (simpler, restart-safe). Polling deliberately lags
// the tip by a confirmation depth, so there is no reorg detector: a block is already
// several confirmations deep, effectively final on Mantle, by the time it is read.
package chain

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/rs/zerolog/log"
)

// getLogsMaxElapsed bounds the total retry window for a single GetLogs call.
const getLogsMaxElapsed = 10 * time.Second

// Function selectors for the read-only view calls the pool-discovery verifier makes,
// derived from the signature string (not a magic literal) so they cannot be mistyped.
// Discovery re-derives candidates on-chain via these; the API's claimed
// tokens/decimals/ordering are not trusted.
var (
	// selDecimals = decimals() -> uint8  (ERC20).
	selDecimals = selector("decimals()")
	// selToken0 = token0() -> address  (Uniswap-V3-shape pools, e.g. Agni).
	selToken0 = selector("token0()")
	// selToken1 = token1() -> address  (Uniswap-V3-shape pools, e.g. Agni).
	selToken1 = selector("token1()")
	// selGetTokenX = getTokenX() -> address  (Liquidity Book, e.g. Merchant Moe).
	selGetTokenX = selector("getTokenX()")
	// selGetTokenY = getTokenY() -> address  (Liquidity Book, e.g. Merchant Moe).
	selGetTokenY = selector("getTokenY()")
	// selGetBinStep = getBinStep() -> uint16  (Liquidity Book, e.g. Merchant Moe): the
	// pool's bin step (= 0x17f11ecc). Used only for the API's add-liquidity deep-link.
	selGetBinStep = selector("getBinStep()")
)

// selector returns the 4-byte function selector for an ABI signature string (the first
// 4 bytes of keccak256(sig)).
func selector(sig string) []byte {
	return crypto.Keccak256([]byte(sig))[:4]
}

// Client is a polling EVM JSON-RPC client wrapping go-ethereum's ethclient.
type Client struct {
	eth     *ethclient.Client
	chainID int64
}

// Dial connects to rpcURL over HTTP and returns a Client. The provider's reported chain
// id is checked against the expected chainID (see the mismatch handling below).
func Dial(ctx context.Context, rpcURL string, chainID int64) (*Client, error) {
	eth, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("chain: dial %q: %w", rpcURL, err)
	}

	got, err := eth.ChainID(ctx)
	if err != nil {
		// Endpoint reachable but not answering eth_chainId. Warn and continue; later
		// calls surface real errors.
		log.Warn().
			Err(err).
			Str("rpc_url", rpcURL).
			Int64("expected_chain_id", chainID).
			Msg("chain: eth_chainId check failed; continuing")
	} else if chainID != 0 && got.Int64() != chainID {
		// Hard-fail: continuing would let the attestor sign txs for the wrong network
		// and corrupt the on-chain record. Fix CHAIN_ID / MANTLE_RPC_URL first.
		return nil, fmt.Errorf(
			"chain: chain_id mismatch: expected %d but RPC %q reports %d; fix CHAIN_ID or MANTLE_RPC_URL",
			chainID, rpcURL, got.Int64(),
		)
	}

	return &Client{eth: eth, chainID: chainID}, nil
}

// Eth returns the underlying ethclient.Client for callers (e.g. the attestor)
// that need direct RPC access.
func (c *Client) Eth() *ethclient.Client {
	return c.eth
}

// TxSender returns the signing EOA (tx.origin) of txHash: the real wallet that
// submitted the trade, not the router/aggregator that appears as a swap log's sender or
// recipient. It recovers the sender from the signature using the client's chain id, so
// it works for legacy and typed transactions. A still-pending tx is an error (it should
// never happen for a confirmed log); every fetch/recovery failure is wrapped so the
// caller can skip that swap.
func (c *Client) TxSender(ctx context.Context, txHash common.Hash) (common.Address, error) {
	tx, pending, err := c.eth.TransactionByHash(ctx, txHash)
	if err != nil {
		return common.Address{}, fmt.Errorf("chain: tx by hash %s: %w", txHash.Hex(), err)
	}
	if pending {
		return common.Address{}, fmt.Errorf("chain: tx %s still pending; cannot resolve sender", txHash.Hex())
	}

	// LatestSignerForChainID handles every signature scheme (legacy, EIP-155, 2930,
	// 1559), so we recover the sender without knowing the tx type.
	signer := types.LatestSignerForChainID(big.NewInt(c.chainID))
	from, err := types.Sender(signer, tx)
	if err != nil {
		return common.Address{}, fmt.Errorf("chain: recover sender for tx %s: %w", txHash.Hex(), err)
	}
	return from, nil
}

// callReturnWord executes an eth_call to `to` with a nullary 4-byte selector at the
// latest block and returns the first 32-byte ABI word. It is the shared primitive
// behind TokenDecimals and PoolTokens; hand-rolled (not abi.Pack/Unpack) because the
// calls are nullary and return one word, so a full ABI round-trip buys nothing.
func (c *Client) callReturnWord(ctx context.Context, to common.Address, sel []byte) ([]byte, error) {
	out, err := c.eth.CallContract(ctx, ethereum.CallMsg{To: &to, Data: sel}, nil)
	if err != nil {
		return nil, fmt.Errorf("chain: eth_call %s sel=0x%x: %w", to.Hex(), sel, err)
	}
	if len(out) < 32 {
		// Short return = not a contract, or it lacks the function. Hard error so the
		// caller skips an unverifiable candidate rather than guessing.
		return nil, fmt.Errorf("chain: eth_call %s sel=0x%x returned %d bytes, want >= 32",
			to.Hex(), sel, len(out))
	}
	return out[:32], nil
}

// TokenDecimals returns an ERC20 token's decimals() via eth_call. A value above 255 is
// rejected as malformed so a junk contract's wide integer is never silently truncated
// into a plausible decimal. Used by discovery to re-derive decimals on-chain.
func (c *Client) TokenDecimals(ctx context.Context, token common.Address) (uint8, error) {
	word, err := c.callReturnWord(ctx, token, selDecimals)
	if err != nil {
		return 0, fmt.Errorf("chain: decimals %s: %w", token.Hex(), err)
	}
	n := new(big.Int).SetBytes(word)
	if !n.IsUint64() || n.Uint64() > 255 {
		return 0, fmt.Errorf("chain: decimals %s out of uint8 range: %s", token.Hex(), n.String())
	}
	return uint8(n.Uint64()), nil
}

// PoolTokens returns a pool's canonical on-chain token ordering (token0, token1) via
// eth_call, selecting the accessor pair by DEX family:
//
//   - "agni" / "fusionx" (any Uniswap-V2/V3-shape pair): token0() / token1().
//   - "merchant_moe" (Liquidity Book): getTokenX() / getTokenY().
//
// This is the authoritative ordering the rest of OCR assumes (aggregator decoders,
// netflow sign), not whatever a discovery API reports. An unsupported dex, or a contract
// that does not answer the accessor, returns an error and the caller skips the candidate.
func (c *Client) PoolTokens(ctx context.Context, pool common.Address, dex string) (token0, token1 common.Address, err error) {
	var sel0, sel1 []byte
	switch dex {
	case "agni", "fusionx":
		sel0, sel1 = selToken0, selToken1
	case "merchant_moe":
		sel0, sel1 = selGetTokenX, selGetTokenY
	default:
		return common.Address{}, common.Address{}, fmt.Errorf("chain: pool tokens: unsupported dex %q", dex)
	}

	w0, err := c.callReturnWord(ctx, pool, sel0)
	if err != nil {
		return common.Address{}, common.Address{}, fmt.Errorf("chain: pool %s token0/X: %w", pool.Hex(), err)
	}
	w1, err := c.callReturnWord(ctx, pool, sel1)
	if err != nil {
		return common.Address{}, common.Address{}, fmt.Errorf("chain: pool %s token1/Y: %w", pool.Hex(), err)
	}
	// An ABI address is the low 20 bytes of the word; BytesToAddress takes them.
	return common.BytesToAddress(w0), common.BytesToAddress(w1), nil
}

// PoolBinStep returns a Merchant Moe (Liquidity Book) pool's bin step via eth_call
// getBinStep(). A value above 65535 is rejected as malformed so a junk contract's wide
// integer is never truncated into a plausible bin step. A non-LB pool does not implement
// getBinStep(), so the call errors and the caller leaves bin_step NULL. Read only by
// discovery / the `ocr poolmeta` backfill for the add-liquidity deep-link, never hot.
func (c *Client) PoolBinStep(ctx context.Context, pool common.Address) (uint16, error) {
	word, err := c.callReturnWord(ctx, pool, selGetBinStep)
	if err != nil {
		return 0, fmt.Errorf("chain: bin step %s: %w", pool.Hex(), err)
	}
	n := new(big.Int).SetBytes(word)
	if !n.IsUint64() || n.Uint64() > 65535 {
		return 0, fmt.Errorf("chain: bin step %s out of uint16 range: %s", pool.Hex(), n.String())
	}
	return uint16(n.Uint64()), nil
}

// LatestBlock returns the most recent block number known to the RPC endpoint.
func (c *Client) LatestBlock(ctx context.Context) (uint64, error) {
	n, err := c.eth.BlockNumber(ctx)
	if err != nil {
		return 0, fmt.Errorf("chain: latest block: %w", err)
	}
	return n, nil
}

// HeaderByNumber returns the header for block n.
func (c *Client) HeaderByNumber(ctx context.Context, n uint64) (*types.Header, error) {
	h, err := c.eth.HeaderByNumber(ctx, new(big.Int).SetUint64(n))
	if err != nil {
		return nil, fmt.Errorf("chain: header %d: %w", n, err)
	}
	return h, nil
}

// GetLogs returns logs in the inclusive block range [from, to], optionally
// filtered to the given contract addresses (nil/empty = all addresses). The
// call is retried with exponential backoff bounded by getLogsMaxElapsed, since
// eth_getLogs is the most rate-limit-prone endpoint we hit.
func (c *Client) GetLogs(ctx context.Context, from, to uint64, addresses []common.Address) ([]types.Log, error) {
	q := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: addresses,
	}

	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = getLogsMaxElapsed

	var out []types.Log
	op := func() error {
		logs, err := c.eth.FilterLogs(ctx, q)
		if err != nil {
			return err
		}
		out = logs
		return nil
	}
	if err := backoff.Retry(op, backoff.WithContext(bo, ctx)); err != nil {
		return nil, fmt.Errorf("chain: get logs [%d,%d]: %w", from, to, err)
	}
	return out, nil
}

// Close releases the underlying RPC connection.
func (c *Client) Close() {
	c.eth.Close()
}
