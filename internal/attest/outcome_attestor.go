package attest

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog/log"
)

// attestOutcomeABIJSON is the minimal ABI for OutcomeAttestor.attestOutcome.
// Source: contracts/src/OutcomeAttestor.sol.
const attestOutcomeABIJSON = `[{"type":"function","name":"attestOutcome","stateMutability":"nonpayable",` +
	`"inputs":[{"name":"signalHash","type":"bytes32"},{"name":"grade","type":"uint8"},` +
	`{"name":"reportHash","type":"bytes32"}],"outputs":[]}]`

// OutcomeAttestor signs and submits attestOutcome() transactions from a single
// gas-only agent wallet. It is the sidecar twin of Attestor: a separate contract for
// matured outcome report cards, so the live signal-attestation path is never touched.
//
// Nonce tracking is delegated to a shared NonceManager. When the same key signs for
// both contracts (the normal deployment), pass both the same instance so a concurrent
// signal attest and outcome attest serialise through one counter instead of colliding.
type OutcomeAttestor struct {
	eth      EthClient
	key      *ecdsa.PrivateKey
	from     common.Address
	contract common.Address
	chainID  *big.Int
	abi      abi.ABI
	nonces   *NonceManager
}

// NewOutcome builds an OutcomeAttestor. Mirrors attest.New: privKeyHex is the agent
// key (0x prefix optional), contractAddr the deployed OutcomeAttestor address, chainID
// the EVM chain id for signing; keys required, contractAddr must be valid hex. Pass the
// same NonceManager as the signal Attestor whenever both sign from one key (a nil one is
// rejected). ctx drives the startup balance preflight; a low balance warns but succeeds.
func NewOutcome(ctx context.Context, eth EthClient, privKeyHex, contractAddr string, chainID int64, nonces *NonceManager) (*OutcomeAttestor, error) {
	if eth == nil {
		return nil, fmt.Errorf("attest: nil ethclient")
	}
	if nonces == nil {
		return nil, fmt.Errorf("attest: nil nonce manager")
	}
	if strings.TrimSpace(privKeyHex) == "" {
		return nil, fmt.Errorf("attest: empty private key")
	}
	if strings.TrimSpace(contractAddr) == "" {
		return nil, fmt.Errorf("attest: empty outcome contract address")
	}
	if !common.IsHexAddress(contractAddr) {
		return nil, fmt.Errorf("attest: invalid outcome contract address %q", contractAddr)
	}

	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimPrefix(privKeyHex, "0x"), "0X"))
	if err != nil {
		// Do not include the key material in the error.
		return nil, fmt.Errorf("attest: parse private key: %w", err)
	}

	parsedABI, err := abi.JSON(strings.NewReader(attestOutcomeABIJSON))
	if err != nil {
		return nil, fmt.Errorf("attest: parse attestOutcome ABI: %w", err)
	}

	from := crypto.PubkeyToAddress(key.PublicKey)
	a := &OutcomeAttestor{
		eth:      eth,
		key:      key,
		from:     from,
		contract: common.HexToAddress(contractAddr),
		chainID:  big.NewInt(chainID),
		abi:      parsedABI,
		nonces:   nonces,
	}

	// Balance preflight: warn if the wallet is low before attestations fail. Short
	// timeout off the caller's ctx so a hung RPC cannot block startup or ignore a signal.
	bctx, bcancel := context.WithTimeout(ctx, 10*time.Second)
	defer bcancel()
	bal, balErr := eth.BalanceAt(bctx, from, nil)
	if balErr != nil {
		log.Warn().
			Err(balErr).
			Str("agent", from.Hex()).
			Msg("attest: could not fetch agent balance (outcome attestor); proceeding but monitor manually")
	} else if bal.Cmp(lowBalanceWarnMNT) < 0 {
		log.Warn().
			Str("agent", from.Hex()).
			Str("balance_wei", bal.String()).
			Msg("attest: agent wallet low on gas (outcome attestor); attestations will fail when balance hits 0, top up now")
	}

	log.Info().
		Str("agent", from.Hex()).
		Str("contract", a.contract.Hex()).
		Int64("chain_id", chainID).
		Msg("attest: outcome attestor initialized")

	return a, nil
}

// AttestOutcome packs and sends an attestOutcome() transaction for one matured outcome
// and returns the submitted tx hash. Like Attest, it does not wait for the receipt and
// wraps every RPC and signing error.
//
// signalHash must be the same hash the signal was attested with on the SignalAttestor
// (outcome.signalHashHex / cmd/ocr's attestHash), so a consumer can join the outcome back
// to the call. reportHash is the keccak256 of the canonical report card
// (outcome.ReportHash); grade is the composite 0..100 score. The nonce comes from the
// shared NonceManager, so a concurrent signal+outcome attest from this key is safe.
func (a *OutcomeAttestor) AttestOutcome(ctx context.Context, signalHash [32]byte, grade uint8, reportHash [32]byte) (string, error) {
	// Pack calldata before the nonce lock (pure CPU, nonce-independent); a pack failure
	// returns without touching the counter.
	data, err := a.abi.Pack("attestOutcome", signalHash, grade, reportHash)
	if err != nil {
		return "", fmt.Errorf("attest: pack attestOutcome calldata: %w", err)
	}

	var txHash string
	var usedNonce uint64
	err = a.nonces.Send(ctx, a.from, a.eth.PendingNonceAt, func(nonce uint64) (bool, error) {
		gasPrice, gerr := a.eth.SuggestGasPrice(ctx)
		if gerr != nil {
			// Failure before broadcast: do not resync the nonce counter.
			return false, fmt.Errorf("attest: suggest gas price: %w", gerr)
		}

		// Bump the suggested price by 20% (see gasPriceBumpNum).
		bumpedPrice := new(big.Int).Mul(gasPrice, big.NewInt(gasPriceBumpNum))
		bumpedPrice.Div(bumpedPrice, big.NewInt(gasPriceBumpDen))

		tx := types.NewTransaction(nonce, a.contract, big.NewInt(0), attestGasLimit, bumpedPrice, data)

		signed, serr := types.SignTx(tx, types.LatestSignerForChainID(a.chainID), a.key)
		if serr != nil {
			// Failure before broadcast: do not resync the nonce counter.
			return false, fmt.Errorf("attest: sign tx: %w", serr)
		}

		if serr := a.eth.SendTransaction(ctx, signed); serr != nil {
			// A real broadcast failed: tell the manager to resync from chain.
			return true, fmt.Errorf("attest: send tx: %w", serr)
		}

		txHash = signed.Hash().Hex()
		usedNonce = nonce
		return false, nil
	})
	if err != nil {
		return "", err
	}

	log.Info().
		Str("tx", txHash).
		Uint8("grade", grade).
		Uint64("nonce", usedNonce).
		Msg("attest: outcome attestation submitted (mempool-accepted; not yet confirmed)")

	return txHash, nil
}
