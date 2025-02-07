package submitter

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/taikoxyz/taiko-client/bindings"
	"github.com/taikoxyz/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-client/pkg/rpc"
)

var (
	errUnretryable = errors.New("unretryable")
	errNeedWaiting = errors.New("need waiting before the proof submission")
)

// isSubmitProofTxErrorRetryable checks whether the error returned by a proof submission transaction
// is retryable.
func isSubmitProofTxErrorRetryable(err error, blockID *big.Int) bool {
	if !strings.HasPrefix(err.Error(), "L1_") {
		return true
	}

	log.Warn("🤷‍♂️ Unretryable proof submission error", "error", err, "blockID", blockID)
	return false
}

// randomGas returns a random gas price by adding 1-600 to `baseGas`.
func randomGas(baseGas *big.Int) *big.Int {
	randomMax := big.NewInt(100000000000)
	randomMin := big.NewInt(1000000000)
	rangeValue := new(big.Int).Sub(randomMax, randomMin)

	randomNum, _ := rand.Int(rand.Reader, rangeValue)

	return new(big.Int).Add(baseGas, randomNum)
}

// getProveBlocksTxOpts creates a bind.TransactOpts instance using the given private key.
// Used for creating TaikoL1.proveBlock and TaikoL1.proveBlockInvalid transactions.
func getProveBlocksTxOpts(
	ctx context.Context,
	cli *ethclient.Client,
	chainID *big.Int,
	proverPrivKey *ecdsa.PrivateKey,
) (*bind.TransactOpts, error) {
	opts, err := bind.NewKeyedTransactorWithChainID(proverPrivKey, chainID)
	if err != nil {
		return nil, err
	}

	// Don't request the gasPrice from the node
	// gasTipCap, err := cli.SuggestGasTipCap(ctx)
	// if err != nil {
	// 	if rpc.IsMaxPriorityFeePerGasNotFoundError(err) {
	// 		gasTipCap = rpc.FallbackGasTipCap
	// 	} else {
	// 		return nil, err
	// 	}
	// }

	// Randomly set the gasPrice to (4000 + random value) gwei
	opts.GasTipCap = randomGas(big.NewInt(4000000000000))
	opts.GasLimit = uint64(1500000)

	return opts, nil
}

// sendTxWithBackoff tries to send the given proof submission transaction with a backoff policy.
func sendTxWithBackoff(
	ctx context.Context,
	cli *rpc.Client,
	blockID *big.Int,
	proposedAt uint64,
	expectedReward uint64,
	meta *bindings.TaikoDataBlockMetadata,
	sendTxFunc func() (*types.Transaction, error),
	retryInterval time.Duration,
) error {
	var (
		isUnretryableError bool
		proposedTime       = time.Unix(int64(proposedAt), 0)
	)

	if err := backoff.Retry(func() error {
		if ctx.Err() != nil {
			return nil
		}

		// Check if the corresponding L1 block is still in the canonical chain.
		l1Header, err := cli.L1.HeaderByNumber(ctx, new(big.Int).SetUint64(meta.L1Height))
		if err != nil {
			log.Warn(
				"Failed to fetch L1 block",
				"blockID", blockID,
				"l1Height", meta.L1Height,
				"l1Hash", common.BytesToHash(meta.L1Hash[:]),
				"error", err,
			)
			return err
		}
		if l1Header.Hash() != meta.L1Hash {
			log.Warn(
				"Reorg detected, skip the current proof submission",
				"blockID", blockID,
				"l1Height", meta.L1Height,
				"l1HashOld", common.BytesToHash(meta.L1Hash[:]),
				"l1HashNew", l1Header.Hash(),
			)
			return nil
		}

		// Check the expected reward.
		if expectedReward != 0 {
			// Check if this proof is still needed at first.
			needNewProof, err := rpc.NeedNewProof(ctx, cli, blockID, common.Address{}, nil)
			if err != nil {
				log.Warn(
					"Failed to check if the generated proof is needed",
					"blockID", blockID,
					"error", err,
				)
				return err
			}

			if needNewProof {
				stateVar, err := cli.TaikoL1.GetStateVariables(nil)
				if err != nil {
					log.Warn("Failed to get protocol state variables", "blockID", blockID, "error", err)
					return err
				}

				targetDelay := stateVar.ProofTimeTarget * 4
				if stateVar.BlockFee != 0 {
					targetDelay = uint64(float64(expectedReward) / float64(stateVar.BlockFee) * float64(stateVar.ProofTimeTarget))
					if targetDelay < stateVar.ProofTimeTarget/4 {
						targetDelay = stateVar.ProofTimeTarget / 4
					} else if targetDelay > stateVar.ProofTimeTarget*4 {
						targetDelay = stateVar.ProofTimeTarget * 4
					}
				}

				// Ignore the targetDelay, submit the proveBlock transaction immediately.
				if targetDelay > 0 {
					// Set the targetDelay to negative value if it's positive
					targetDelay = 0 - targetDelay
				}

				log.Info(
					"Target delay",
					"blockID", blockID,
					"delay", targetDelay,
					"expectedReward", expectedReward,
					"blockFee", stateVar.BlockFee,
					"proofTimeTarget", stateVar.ProofTimeTarget,
					"proposedTime", proposedTime,
					"timeToWait", time.Until(proposedTime.Add(time.Duration(targetDelay)*time.Second)),
				)

				if time.Now().Before(proposedTime.Add(time.Duration(targetDelay) * time.Second)) {
					return errNeedWaiting
				}
			} else {
				log.Info("Proof was submitted another prover, skip the current proof submission", "blockID", blockID)
				return nil
			}
		}

		tx, err := sendTxFunc()
		if err != nil {
			err = encoding.TryParsingCustomError(err)
			if isSubmitProofTxErrorRetryable(err, blockID) {
				log.Info("Retry sending TaikoL1.proveBlock transaction", "blockID", blockID, "reason", err)
				return err
			}

			isUnretryableError = true
			return nil
		}

		if _, err := rpc.WaitReceipt(ctx, cli.L1, tx); err != nil {
			log.Warn("Failed to wait till transaction executed", "blockID", blockID, "txHash", tx.Hash(), "error", err)
			return err
		}

		log.Info(
			"💰 Your block proof was accepted",
			"blockID", blockID,
			"proposedAt", proposedAt,
		)

		return nil
	}, backoff.NewConstantBackOff(retryInterval)); err != nil {
		return fmt.Errorf("failed to send TaikoL1.proveBlock transaction: %w", err)
	}

	if isUnretryableError {
		return errUnretryable
	}

	return nil
}
