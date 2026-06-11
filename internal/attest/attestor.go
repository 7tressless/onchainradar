// Package attest records detector signals as on-chain attestations on Mantle.
//
// Each signal is reduced to a canonical hash (SignalHash) and submitted to the
// SignalAttestor contract's attest(bytes32,address,uint8,uint16); the emitted
// SignalAttested log is the timestamped record of an agent decision. This package owns
// the deterministic hashing, the z-score-to-score conversion, and the signed send.
package attest

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog/log"
)

// attestGasLimit caps an attest() call. It emits one indexed event with no storage
// writes, so this is well above real cost yet cheap at Mantle gas prices.
const attestGasLimit uint64 = 200000

// gasPriceBumpNum / gasPriceBumpDen apply a 20% margin over the suggested gas price so a
// tx is less likely to stick or drop during a fee spike (there is no resubmit path).
const gasPriceBumpNum = 120
const gasPriceBumpDen = 100

// lowBalanceWarnMNT is the agent-wallet balance (wei) below which New warns. 0.01 MNT
// covers 100+ attest txs at normal gas, an early top-up signal.
var lowBalanceWarnMNT = new(big.Int).Mul(
	big.NewInt(1e16), // 0.01 MNT in wei
	big.NewInt(1),
)

// MaxScoreZ is the largest abs z-score representable after the *100 scale (uint16
// ceiling / 100); beyond it the on-chain score saturates at math.MaxUint16. Exported
// so the pre-clamping per-event detectors (lending, lst_flow, depeg) share this ceiling.
const MaxScoreZ = 655.35

// attestABIJSON is the minimal ABI for SignalAttestor.attest.
// Source: contracts/SignalAttestor.sol.
const attestABIJSON = `[{"type":"function","name":"attest","stateMutability":"nonpayable",` +
	`"inputs":[{"name":"signalHash","type":"bytes32"},{"name":"subject","type":"address"},` +
	`{"name":"signalType","type":"uint8"},{"name":"score","type":"uint16"}],"outputs":[]}]`

// SignalHash returns keccak256 over the canonical compact JSON encoding of a
// signal:
//
//	{"pool":<lowercase>,"type":<n>,"metric":<s>,"score":<n>,"ts":<n>}
//
// Fixed key order, no insignificant whitespace, pool lowercased, metric JSON-escaped:
// byte-for-byte reproducible. This hash is committed on-chain, so its stability is
// part of the contract.
func SignalHash(pool string, signalType uint8, metric string, score uint16, ts int64) [32]byte {
	// json.Marshal a Go string for the only free-form field (metric) so we never emit
	// malformed JSON, keeping manual control over key order.
	metricJSON, err := json.Marshal(metric)
	if err != nil {
		// Marshaling a string cannot fail in practice; fall back to quoting, not panic.
		log.Error().Err(err).Str("metric", metric).Msg("attest: metric JSON-encode failed; using fallback quoting")
		metricJSON = []byte(`"` + strings.ReplaceAll(metric, `"`, `\"`) + `"`)
	}

	canonical := fmt.Sprintf(
		`{"pool":"%s","type":%d,"metric":%s,"score":%d,"ts":%d}`,
		strings.ToLower(pool), signalType, metricJSON, score, ts,
	)
	return crypto.Keccak256Hash([]byte(canonical))
}

// SignalHashEvent returns keccak256 over the canonical compact JSON encoding of a
// per-event (e.g. whale) signal:
//
//	{"pool":<lowercase>,"type":<n>,"ref":<s>,"score":<n>,"ts":<n>}
//
// It mirrors SignalHash but with the event reference ("txhash:logindex") in place of
// metric, so an event-keyed signal hashes reproducibly from its stored row. The two
// use disjoint key sets ("metric" vs "ref"), so the families cannot collide on a hash.
func SignalHashEvent(pool string, signalType uint8, ref string, score uint16, ts int64) [32]byte {
	// json.Marshal a Go string for the only free-form field (ref) so we never emit
	// malformed JSON, keeping manual control over key order.
	refJSON, err := json.Marshal(ref)
	if err != nil {
		// Marshaling a string cannot fail in practice; fall back to quoting, not panic.
		log.Error().Err(err).Str("ref", ref).Msg("attest: ref JSON-encode failed; using fallback quoting")
		refJSON = []byte(`"` + strings.ReplaceAll(ref, `"`, `\"`) + `"`)
	}

	canonical := fmt.Sprintf(
		`{"pool":"%s","type":%d,"ref":%s,"score":%d,"ts":%d}`,
		strings.ToLower(pool), signalType, refJSON, score, ts,
	)
	return crypto.Keccak256Hash([]byte(canonical))
}

// ScoreFromZ converts a z-score into the uint16 on-chain score:
// round(min(|z|, 655.35) * 100), saturating at 65535. NaN maps to 0.
func ScoreFromZ(z float64) uint16 {
	if math.IsNaN(z) {
		return 0
	}
	scaled := math.Round(math.Min(math.Abs(z), MaxScoreZ) * 100)
	if scaled >= math.MaxUint16 {
		return math.MaxUint16
	}
	if scaled < 0 {
		return 0
	}
	return uint16(scaled)
}

// Attestor signs and submits attest() transactions to the SignalAttestor
// contract from a single gas-only agent wallet.
//
// Nonce tracking is delegated to a shared per-wallet NonceManager (see nonces.Send):
// the Attestor, OutcomeAttestor, and recovery loop share one instance so two
// concurrent sends from the same key cannot pick the same nonce.
type Attestor struct {
	eth      EthClient
	key      *ecdsa.PrivateKey
	from     common.Address
	contract common.Address
	chainID  *big.Int
	abi      abi.ABI
	nonces   *NonceManager
}

// New builds an Attestor. privKeyHex is the agent wallet key (0x prefix optional);
// contractAddr is the deployed SignalAttestor address; chainID is the EVM chain id
// for replay-protected signing. Both keys are required; contractAddr must be valid hex.
//
// nonces is the shared per-wallet NonceManager: pass the same instance to every
// component that signs from this key (OutcomeAttestor, recovery loop) so all sends
// serialise through one counter. A nil manager is rejected. ctx drives the startup
// balance preflight so a shutdown signal can interrupt a hung RPC. A balance below
// lowBalanceWarnMNT still succeeds but warns so the deployer can top up.
func New(ctx context.Context, eth EthClient, privKeyHex, contractAddr string, chainID int64, nonces *NonceManager) (*Attestor, error) {
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
		return nil, fmt.Errorf("attest: empty contract address")
	}
	if !common.IsHexAddress(contractAddr) {
		return nil, fmt.Errorf("attest: invalid contract address %q", contractAddr)
	}

	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimPrefix(privKeyHex, "0x"), "0X"))
	if err != nil {
		// Do not include the key material in the error.
		return nil, fmt.Errorf("attest: parse private key: %w", err)
	}

	parsedABI, err := abi.JSON(strings.NewReader(attestABIJSON))
	if err != nil {
		return nil, fmt.Errorf("attest: parse attest ABI: %w", err)
	}

	from := crypto.PubkeyToAddress(key.PublicKey)
	a := &Attestor{
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
			Msg("attest: could not fetch agent balance; proceeding but monitor manually")
	} else if bal.Cmp(lowBalanceWarnMNT) < 0 {
		log.Warn().
			Str("agent", from.Hex()).
			Str("balance_wei", bal.String()).
			Msg("attest: agent wallet low on gas; attestations will fail when balance hits 0, top up now")
	}

	log.Info().
		Str("agent", from.Hex()).
		Str("contract", a.contract.Hex()).
		Int64("chain_id", chainID).
		Msg("attest: attestor initialized")

	return a, nil
}

// Attest packs and sends an attest() transaction for one signal and returns the
// submitted tx hash. It does not wait for the receipt: the caller persists the hash
// immediately and treats the signal as "submitted" until mined. Every RPC and signing
// error is wrapped and returned. The nonce comes from the shared NonceManager, so rapid
// or concurrent sends from this key cannot collide.
func (a *Attestor) Attest(ctx context.Context, signalHash [32]byte, subject common.Address, signalType uint8, score uint16) (string, error) {
	// Pack calldata before the nonce lock (pure CPU, nonce-independent); a pack failure
	// returns without touching the counter.
	data, err := a.abi.Pack("attest", signalHash, subject, signalType, score)
	if err != nil {
		return "", fmt.Errorf("attest: pack attest calldata: %w", err)
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
		Str("subject", subject.Hex()).
		Uint8("signal_type", signalType).
		Uint16("score", score).
		Uint64("nonce", usedNonce).
		Msg("attest: attestation submitted (mempool-accepted; not yet confirmed)")

	return txHash, nil
}
