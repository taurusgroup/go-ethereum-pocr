// This file is part of the go-ethereum library.
// Copyright 2017 The go-ethereum Authors
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package clique implements the proof-of-authority consensus engine.
package cliquepocr

import (
	"bytes"
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru"
)

// We put new constants to be able to override default clique values
const (
	checkpointInterval = 1024 // Number of blocks after which to save the vote snapshot to the database
	inmemorySnapshots  = 128  // Number of recent vote snapshots to keep in memory
	inmemorySignatures = 4096 // Number of recent block signatures to keep in memory

	wiggleTime  = 500 * time.Millisecond // Random delay (per signer) to allow concurrent signers
	extraVanity = 32
)

// Clique proof-of-authority protocol constants.
var (
	epochLength = uint64(30000) // Default number of blocks after which to checkpoint and reset the pending votes, that could be overrided from default Clique
)

var zeroAddress = common.HexToAddress("0x0000000000000000000000000000000000000000")

// Use a separate address for collecting the total crypto generated because the smart contract also needs to hold auditor pledge
var sessionVariablesContractAddress = "0x0000000000000000000000000000000000000101"

var sessionVariableTotalPocRCoins = "GeneratedPocRTotal"
var zero = big.NewInt(0)
var CTCUnit = big.NewInt(1e+18)
var MinBlockBetweenAudit = big.NewInt((3600 / 4) * 24 * 365) // 1 year
// var MinBlockBetweenAudit = big.NewInt(10) // 10 block for debugging
var PenaltyOnOldFootprint int64 = 5 // In percent of the footprint to be added to the footprint value

// var raceRankComputation = NewRaceRankComputation()

type CliquePoCR struct {
	config *params.CliqueConfig // Consensus engine configuration parameters
	db     ethdb.Database       // Database to store and retrieve snapshot checkpoints

	recents    *lru.ARCCache // Snapshots for recent block to speed up reorgs
	signatures *lru.ARCCache // Signatures of recent blocks to speed up mining

	proposals map[common.Address]bool // Current list of proposals we are pushing

	// signer common.Address  // Ethereum address of the signing key
	// signFn clique.SignerFn // Signer function to authorize hashes with
	// lock   sync.RWMutex    // Protects the signer fields

	// The fields below are for testing only
	// fakeDiff             bool // Skip difficulty verifications
	EngineInstance *clique.Clique
	// signersList          []common.Address
	// signersListLastBlock uint64
	computation IRewardComputation
}

func New(config *params.CliqueConfig, db ethdb.Database) *CliquePoCR {
	conf := *config
	if conf.Epoch == 0 {
		conf.Epoch = epochLength
	}
	// Allocate the snapshot caches and create the engine
	recents, _ := lru.NewARC(inmemorySnapshots)
	signatures, _ := lru.NewARC(inmemorySignatures)
	return &CliquePoCR{
		config:         &conf,
		db:             db,
		recents:        recents,
		signatures:     signatures,
		proposals:      make(map[common.Address]bool),
		EngineInstance: clique.New(config, db),
		computation:    NewRaceRankComputation(),
	}
}

func SetSessionVariable(key string, value *big.Int, state *state.StateDB) {
	state.SetState(common.HexToAddress(sessionVariablesContractAddress), common.BytesToHash(crypto.Keccak256([]byte(key))), common.BigToHash(value))
}
func ReadSessionVariable(key string, state *state.StateDB) *big.Int {
	return state.GetState(common.HexToAddress(sessionVariablesContractAddress), common.BytesToHash(crypto.Keccak256([]byte(key)))).Big()
}

// ########################################################################################################################
// ## IMPLEMENT THE consensus.Engine INTERFACE
// ########################################################################################################################

func (c *CliquePoCR) Author(header *types.Header) (common.Address, error) {
	return c.EngineInstance.Author(header)
}

// VerifyHeader checks whether a header conforms to the consensus rules of a
// given EngineInstance. Verifying the seal may be done optionally here, or explicitly
// via the VerifySeal method.
func (c *CliquePoCR) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header, seal bool) error {
	// log.Info("VerifyHeader", "number", header.Number)
	return c.EngineInstance.VerifyHeader(chain, header, seal)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications (the order is that of
// the input slice).

func (c *CliquePoCR) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	// log.Info("VerifyHeaders", "number[0]", headers[0].Number, "nb", len(headers))
	return c.EngineInstance.VerifyHeaders(chain, headers, seals)
}

// VerifyUncles verifies that the given block's uncles conform to the consensus
// rules of a given EngineInstance.

func (c *CliquePoCR) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	return c.EngineInstance.VerifyUncles(chain, block)
}

// Prepare initializes the consensus fields of a block header according to the
// rules of a particular EngineInstance. The changes are executed inline.

func (c *CliquePoCR) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	// log.Info("Prepare", "number", header.Number)

	return c.EngineInstance.Prepare(chain, header)
}

// Finalize runs any post-transaction state modifications (e.g. block rewards)
// but does not assemble the block.
//
// Note: The block header and state database might be updated to reflect any
// consensus rules that happen at finalization (e.g. block rewards).
// This function is called when the block is imported from another node
// It does not receive the transaction receipt (that'a shame because it contains the gas used)
// Hence the reason for putting the extra fields in the tx
func (c *CliquePoCR) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header) {
	// log.Info("Finalize", "number", header.Number)
	blockPostProcessing(c, chain, state, header, txs, false)
	// Finalize
	c.EngineInstance.Finalize(chain, header, state, txs, uncles)
}

// FinalizeAndAssemble runs any post-transaction state modifications (e.g. block
// rewards) and assembles the final block.
//
// Note: The block header and state database might be updated to reflect any
// consensus rules that happen at finalization (e.g. block rewards).
// This function is called when the block is created by this node
// It receive the transaction receipt but since the Finalize receive the fee info from the tx , we'll do the same
func (c *CliquePoCR) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	// log.Info("FinalizeAndAssemble", "number", header.Number)
	blockPostProcessing(c, chain, state, header, txs, true)
	// Finalize block
	return c.EngineInstance.FinalizeAndAssemble(chain, header, state, txs, uncles, receipts)
}

// Seal generates a new sealing request for the given input block and pushes
// the result into the given channel.
//
// Note, the method returns immediately and will send the result async. More
// than one result may also be returned depending on the consensus algorithm.

func (c *CliquePoCR) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	// log.Info("Seal", "number", block.Number())

	return c.EngineInstance.Seal(chain, block, results, stop)
}

// SealHash returns the hash of a block prior to it being sealed.
func (c *CliquePoCR) SealHash(header *types.Header) common.Hash {
	return c.EngineInstance.SealHash(header)
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have.

func (c *CliquePoCR) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return c.EngineInstance.CalcDifficulty(chain, time, parent)
}

// APIs returns the RPC APIs this consensus engine provides.
func (c *CliquePoCR) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return c.EngineInstance.APIs(chain)
}

// Close terminates any background threads maintained by the consensus EngineInstance.
func (c *CliquePoCR) Close() error {
	return c.EngineInstance.Close()
}

// Authorize injects a private key into the consensus engine to mint new blocks
// with.

func (c *CliquePoCR) Authorize(signer common.Address, signFn clique.SignerFn) {
	c.EngineInstance.Authorize(signer, signFn)
}

// ########################################################################################################################
// ########################################################################################################################

// ########################################################################################################################
// ##  PRIVATE IMPLEMENTATION PART
// ########################################################################################################################

// blockPostProcessing will credits the coinbase of the given block with the mining
// reward. The total reward consists of the static block reward and rewards for
// included transactions. The reward will depends on the environmental footprint of the node.
// newBlock (bool) is true when called by FinalizeAndAssemble ie when the block is to be created and signed by this node
// else newBlock will be false when called by Finalize ie when called for an imported block signed by another node
func blockPostProcessing(c *CliquePoCR, chain consensus.ChainHeaderReader, state *state.StateDB, header *types.Header, txs []*types.Transaction, newBlock bool) {
	// skip block 0
	if header.Number.Int64() <= 0 {
		return
	}

	// author is the sealer address of the block being processed
	var author common.Address
	if newBlock {
		// the block is not yet signed so we are signing it
		author = c.EngineInstance.Signer
	} else {
		var err error
		// Get the block sealer when the block is signed by another node
		author, err = c.Author(header)
		if err != nil {
			// the sealer is invalid in this received block, do not even bother processing anything
			// the clique implementation VerifyHeader will cover that case
			return
		}
	}

	// blockReward is the reward for the sealer for creating that block. It does not contains the fees
	blockReward := big.NewInt(0)

	footprint, rank, nbNodes, totalCrypto, err := calcCarbonFootprintRanking(c, chain, author, state, header)
	if err != nil {
		// if it could not be calculated
		log.Warn("Fail calculating the node ranking", "node", author.String(), "error", err)

	} else {
		// ranking successfully calculated
		r, err := calcCarbonFootprintReward(c, author, header, footprint, rank, nbNodes, totalCrypto)
		// if it could not be calculated
		if err != nil {
			log.Warn("Fail calculating the block reward", "node", author.String(), "error", err)
		} else {
			blockReward = r
		}
	}

	if blockReward.Sign() > 0 {
		// Accumulate the rewards for the miner
		state.AddBalance(author, blockReward)
		// AddBalance to a non accessible account storage to just accrue the total amount of crypto created
		// and use this as a control of the monetary creation policy
		addTotalCryptoBalance(state, blockReward)
	}
	if rank == nil {
		// if the ranking was not successfully calculated, force it to a zero ranking so fees are zeroed
		rank = big.NewRat(0, 1)
	}
	feeAdjustment, burnt := calcCarbonFootprintTxFee(c, author, header, rank, txs)

	// Update the fees even if the block reward could not be calculated
	if feeAdjustment.Sign() == 1 {
		// Should not happen to add more fee to the account but let's cover this case anyway
		state.AddBalance(author, feeAdjustment)
		// add the created crypto as the fees comes from noone
		addTotalCryptoBalance(state, feeAdjustment)
	} else if feeAdjustment.Sign() == -1 {
		// remove the over received fee
		state.SubBalance(author, new(big.Int).Abs(feeAdjustment))
		// remove the un earned (burned) fees
		addTotalCryptoBalance(state, feeAdjustment)
	}

	if burnt.Sign() != 0 {
		// remove the burned fee from the EIP-1559 from the crypto counter
		addTotalCryptoBalance(state, burnt.Neg(burnt))
	}

	synchronizeSealers(c, chain, author, state, header)

	log.Info("💵 Sealer earnings", "block", header.Number, "node", author.String(), "rank", rank.FloatString(4), "blockReward", blockReward.String(), "feeAdjustment", feeAdjustment.String(), "burnt", burnt.String())
}

func getTotalCryptoBalance(state *state.StateDB) *big.Int {
	return ReadSessionVariable(sessionVariableTotalPocRCoins, state)
}

func addTotalCryptoBalance(state *state.StateDB, value *big.Int) *big.Int {
	// state.CreateAccount(common.HexToAddress(totalCryptoGeneratedAddress))
	currentTotal := ReadSessionVariable(sessionVariableTotalPocRCoins, state)
	newTotal := big.NewInt(0).Add(currentTotal, value)
	SetSessionVariable(sessionVariableTotalPocRCoins, newTotal, state)
	// log.Info("Increasing the total crypto", "from", currentTotal.String(), "to", newTotal.String())
	return newTotal
}

func calcCarbonFootprintRanking(c *CliquePoCR, chain consensus.ChainHeaderReader, author common.Address, state *state.StateDB, header *types.Header) (footprint *big.Int, rank *big.Rat, nbNodes int, totalCrypto *big.Int, err error) {
	// log.Info("calcCarbonFootprintReward ", "header.Number", header.Number)
	contract := NewCarbonFootPrintContract(author, chain.Config(), state, header)

	signers, err := c.getSigners(chain, header, nil)
	if err != nil {
		return nil, big.NewRat(0, 1), 0, nil, err
	}

	// Define an array to store all nodes footprint
	allNodesFootprint := []*big.Int{}
	for _, signerAddress := range signers {
		// log.Debug("Signer found", "address", signerAddress)
		// retrieve the last block and the footprint
		f, block, err := contract.footprint(signerAddress)
		// apply a penalty if the age of the audit is greater than a multiple of number of blocks to incentivize redoing audits
		f = calcCarbonFootprintAuditIncentive(f, block, header.Number)
		if err == nil {
			allNodesFootprint = append(allNodesFootprint, f)
			// if the current sealer is our block author, keep its footprint
			if bytes.Equal(signerAddress.Bytes(), author.Bytes()) {
				footprint = f
			}
		}
	}

	// a Zero environmental footprint means no footprint at all
	if footprint == nil || footprint.Cmp(zero) == 0 {
		return nil, big.NewRat(0, 1), 0, nil, errors.New("sealer does not have a footprint")
	}

	// get the ranking as a value between 0 and 1
	r, N, err := c.computation.CalculateRanking(footprint, allNodesFootprint)
	if err != nil {
		return nil, big.NewRat(0, 1), 0, nil, err
	}
	log.Debug("Node ranking result", "signer", author, "rank", r)

	M := getTotalCryptoBalance(state)

	return footprint, r, N, M, nil
}

/*
	Returns the penalized footprint based on the age of the last footprint
	Per full year of age, apply a % of increase on the footprint

	result = footprint x (1 + nb * penalty%)
	Calculated as footprint x (100 + penalty x nb) / 100, so the integer division happens last
*/
func calcCarbonFootprintAuditIncentive(footprint *big.Int, lastBlock *big.Int, currentBlock *big.Int) *big.Int {
	// calulate the blocks elapsed since the audit
	delta := new(big.Int).Sub(currentBlock, lastBlock)
	if delta.Sign() <= 0 || footprint.Sign() <= 0 {
		return footprint
	}
	// calculate the number of full years since the last audit. Result is zero
	factor := new(big.Int).Div(delta, MinBlockBetweenAudit)
	// calculate the ratio to apply : PenaltyOnOldFootprint% per years
	factor.Mul(factor, big.NewInt(PenaltyOnOldFootprint))
	factor.Add(factor, big.NewInt(100))
	newFootprint := new(big.Int).Mul(footprint, factor)
	newFootprint.Div(newFootprint, big.NewInt(100))
	log.Debug("Result of the footprint penality", "initial", footprint, "factor", factor, "new footprint", newFootprint)
	return newFootprint
}

// Calculate the fees that needs to be rmoved from the sealer because of the ranking ie has and the fees that have been burt in the EIP 1559
func calcCarbonFootprintTxFee(c *CliquePoCR, address common.Address, header *types.Header, rank *big.Rat, txs []*types.Transaction) (*big.Int, *big.Int) {
	received := big.NewInt(0)
	burnt := big.NewInt(0)
	for _, tx := range txs {
		received = received.Add(received, tx.FeeTransferred)
		burnt = burnt.Add(burnt, tx.FeeBurnt)
	}

	expected := new(big.Rat).SetInt(received)
	expected = expected.Mul(expected, rank)

	adjustment := new(big.Rat).Sub(expected, new(big.Rat).SetInt(received))

	return new(big.Int).Div(adjustment.Num(), adjustment.Denom()), burnt
}

func calcCarbonFootprintReward(c *CliquePoCR, address common.Address, header *types.Header, footprint *big.Int, rank *big.Rat, nbNodes int, totalCrypto *big.Int) (*big.Int, error) {

	reward, err := c.computation.CalculateCarbonFootprintReward(rank, nbNodes, totalCrypto)
	if err != nil {
		return nil, err
	}

	// log.Info("Calculated reward based on footprint", "block", header.Number, "node", address.String(), "total", totalCrypto, "nb", nbNodes, "rank", rank.FloatString(5), "reward", reward)
	return reward, nil
}

func (c *CliquePoCR) getSigners(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) ([]common.Address, error) {
	number := header.Number.Uint64()

	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := c.EngineInstance.Snapshot(chain, number-1, header.ParentHash, parents)
	if err != nil {
		return nil, err
	}
	signersArray := snap.GetSigners()
	return signersArray, nil
}

func contains(array []common.Address, value common.Address) bool {
	for _, v := range array {
		if v == value {
			return true
		}
	}
	return false
}

func synchronizeSealers(c *CliquePoCR, chain consensus.ChainHeaderReader, author common.Address, state *state.StateDB, header *types.Header) error {
	signers, err := c.getSigners(chain, header, nil)
	if err != nil {
		return err
	}
	/*
		- pseudo code
		// start by removing missing sealers
		for i = 0 to nbNodes-1
				s = sealers[i]
				e = isSealer[s]
				if s not in snapshot.sealers then
						isSealer[s] = false
						sealers[i] = zero

		// now force the replication of the snapshot
		for i = 0 to snapshot.sealers.length-1
				s = sealers[i]
				e = isSealer[snapshot.sealers[i]]
				if s != snapshot.sealers[i] then
						 sealers[i] = snapshot.sealers[i]
				if not e then
						 isSealer[snapshot.sealers[i]] = true

		// finally update the number of nodes
		nbNodes = snapshot.sealers.length
	*/

	contract := NewCarbonFootPrintContractForUpdate(author, chain.Config(), state, header)
	nbNodes := contract.getNbNodes()

	// log.Info("Synchronizing the sealers", "sc count", nbNodes, "actual", len(signers))

	for i := uint64(0); i < nbNodes; i++ {
		s := contract.getSealerAt(int64(i))
		if !contains(signers, s) {
			log.Info("Synchronizing the sealers", "deleting", s, "at", i)
			contract.setIsSealerOf(s, false)
			contract.setSealerAt(int64(i), zeroAddress)
		}
	}

	for i, signer := range signers {
		s := contract.getSealerAt(int64(i))
		e := contract.getIsSealerOf(signer)
		if s != signer {
			log.Info("Synchronizing the sealers", "setting", signer, "at", i)
			contract.setSealerAt(int64(i), signer)
		}
		if !e {
			log.Info("Synchronizing the sealers", "enabling", signer)
			contract.setIsSealerOf(signer, true)
		}
	}

	if nbNodes != uint64(len(signers)) {
		log.Info("Synchronizing the sealers", "nbNodes", len(signers))
		contract.setNbNodes(int64(len(signers)))
	}

	return nil
}
