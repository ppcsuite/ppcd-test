// Copyright (c) 2013-2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"fmt"

	"github.com/ppcsuite/btcutil"
	"github.com/ppcsuite/btcwire"
)

// BehaviorFlags is a bitmask defining tweaks to the normal behavior when
// performing chain processing and consensus rules checks.
type BehaviorFlags uint32

const (
	// BFFastAdd may be set to indicate that several checks can be avoided
	// for the block since it is already known to fit into the chain due to
	// already proving it correct links into the chain up to a known
	// checkpoint.  This is primarily used for headers-first mode.
	BFFastAdd BehaviorFlags = 1 << iota

	// BFNoPoWCheck may be set to indicate the proof of work check which
	// ensures a block hashes to a value less than the required target will
	// not be performed.
	BFNoPoWCheck

	// BFDryRun may be set to indicate the block should not modify the chain
	// or memory chain index.  This is useful to test that a block is valid
	// without modifying the current state.
	BFDryRun

	// BFNone is a convenience value to specifically indicate no flags.
	BFNone BehaviorFlags = 0
)

// blockExists determines whether a block with the given hash exists either in
// the main chain or any side chains.
func (b *BlockChain) blockExists(hash *btcwire.ShaHash) (bool, error) {
	// Check memory chain first (could be main chain or side chain blocks).
	if _, ok := b.index[*hash]; ok {
		return true, nil
	}

	// Check in database (rest of main chain not in memory).
	return b.db.ExistsSha(hash)
}

// processOrphans determines if there are any orphans which depend on the passed
// block hash (they are no longer orphans if true) and potentially accepts them.
// It repeats the process for the newly accepted blocks (to detect further
// orphans which may no longer be orphans) until there are no more.
//
// The flags do not modify the behavior of this function directly, however they
// are needed to pass along to maybeAcceptBlock.
func (b *BlockChain) processOrphans(hash *btcwire.ShaHash, timeSource MedianTimeSource, flags BehaviorFlags) error {

	defer timeTrack(now(), fmt.Sprintf("processOrphans(%v)", hash))

	// Start with processing at least the passed hash.  Leave a little room
	// for additional orphan blocks that need to be processed without
	// needing to grow the array in the common case.
	processHashes := make([]*btcwire.ShaHash, 0, 10)
	processHashes = append(processHashes, hash)
	for len(processHashes) > 0 {
		// Pop the first hash to process from the slice.
		processHash := processHashes[0]
		processHashes[0] = nil // Prevent GC leak.
		processHashes = processHashes[1:]

		// Look up all orphans that are parented by the block we just
		// accepted.  This will typically only be one, but it could
		// be multiple if multiple blocks are mined and broadcast
		// around the same time.  The one with the most proof of work
		// will eventually win out.  An indexing for loop is
		// intentionally used over a range here as range does not
		// reevaluate the slice on each iteration nor does it adjust the
		// index for the modified slice.
		for i := 0; i < len(b.prevOrphans[*processHash]); i++ {
			orphan := b.prevOrphans[*processHash][i]
			if orphan == nil {
				log.Warnf("Found a nil entry at index %d in the "+
					"orphan dependency list for block %v", i,
					processHash)
				continue
			}

			// Remove the orphan from the orphan pool.
			// It's safe to ignore the error on Sha since the hash
			// is already cached.
			orphanHash, _ := orphan.block.Sha()
			b.removeOrphanBlock(orphan)
			i--

			// ppc: processing
			b.ppcOrphanBlockRemoved(orphan.block)

			// Potentially accept the block into the block chain.
			err := b.maybeAcceptBlock(orphan.block, timeSource, flags)
			if err != nil {
				return err
			}

			// Add this block to the list of blocks to process so
			// any orphan blocks that depend on this block are
			// handled too.
			processHashes = append(processHashes, orphanHash)
		}
	}
	return nil
}

// ProcessBlock is the main workhorse for handling insertion of new blocks into
// the block chain.  It includes functionality such as rejecting duplicate
// blocks, ensuring blocks follow all rules, orphan handling, and insertion into
// the block chain along with best chain selection and reorganization.
//
// It returns a bool which indicates whether or not the block is an orphan and
// any errors that occurred during processing.  The returned bool is only valid
// when the error is nil.
func (b *BlockChain) ProcessBlock(block *btcutil.Block, timeSource MedianTimeSource, flags BehaviorFlags) (bool, error) {

	defer timeTrack(now(), fmt.Sprintf("ProcessBlock(%v)", slice(block.Sha())[0]))

	fastAdd := flags&BFFastAdd == BFFastAdd
	dryRun := flags&BFDryRun == BFDryRun

	blockHash, err := block.Sha()
	if err != nil {
		return false, err
	}
	log.Tracef("Processing block %v", blockHash)

	// The block must not already exist in the main chain or side chains.
	exists, err := b.blockExists(blockHash)
	if err != nil {
		return false, err
	}
	if exists {
		str := fmt.Sprintf("already have block %v", blockHash)
		return false, ruleError(ErrDuplicateBlock, str)
	}

	// The block must not already exist as an orphan.
	if _, exists := b.orphans[*blockHash]; exists {
		str := fmt.Sprintf("already have block (orphan) %v", blockHash)
		return false, ruleError(ErrDuplicateBlock, str)
	}

	// ppc: processing
	ppcErr := b.ppcProcessBlock(block, phasePreSanity)
	if ppcErr != nil {
		return false, ppcErr
	}

	// Perform preliminary sanity checks on the block and its transactions.
	err = checkBlockSanity(b.netParams, block, b.netParams.PowLimit, timeSource, flags)
	if err != nil {
		return false, err
	}

	// Find the previous checkpoint and perform some additional checks based
	// on the checkpoint.  This provides a few nice properties such as
	// preventing old side chain blocks before the last checkpoint,
	// rejecting easy to mine, but otherwise bogus, blocks that could be
	// used to eat memory, and ensuring expected (versus claimed) proof of
	// work requirements since the previous checkpoint are met.
	blockHeader := &block.MsgBlock().Header
	checkpointBlock, err := b.findPreviousCheckpoint()
	if err != nil {
		return false, err
	}
	if checkpointBlock != nil {
		// Ensure the block timestamp is after the checkpoint timestamp.
		checkpointHeader := &checkpointBlock.MsgBlock().Header
		checkpointTime := checkpointHeader.Timestamp
		if blockHeader.Timestamp.Before(checkpointTime) {
			str := fmt.Sprintf("block %v has timestamp %v before "+
				"last checkpoint timestamp %v", blockHash,
				blockHeader.Timestamp, checkpointTime)
			return false, ruleError(ErrCheckpointTimeTooOld, str)
		}
		if !fastAdd {
			// Even though the checks prior to now have already ensured the
			// proof of work exceeds the claimed amount, the claimed amount
			// is a field in the block header which could be forged.  This
			// check ensures the proof of work is at least the minimum
			// expected based on elapsed time since the last checkpoint and
			// maximum adjustment allowed by the retarget rules.
			/* TODO peercoin : disabled
			duration := blockHeader.Timestamp.Sub(checkpointTime)
			requiredTarget := CompactToBig(b.calcEasiestDifficulty(
				checkpointHeader.Bits, duration))
			currentTarget := CompactToBig(blockHeader.Bits)
			if currentTarget.Cmp(requiredTarget) > 0 {
				str := fmt.Sprintf("block target difficulty of %064x "+
					"is too low when compared to the previous "+
					"checkpoint", currentTarget)
				return false, ruleError(ErrDifficultyTooLow, str)
			}*/
		}
	}

	// Handle orphan blocks.
	prevHash := &blockHeader.PrevBlock
	if !prevHash.IsEqual(zeroHash) {
		prevHashExists, err := b.blockExists(prevHash)
		if err != nil {
			return false, err
		}
		if !prevHashExists {
			if !dryRun {
				// ppc: processing
				ppcErr := b.ppcProcessOrphan(block)
				if ppcErr != nil {
					return false, ppcErr
				}
				log.Infof("Adding orphan block %v with parent %v",
					blockHash, prevHash)
				b.addOrphanBlock(block)
			}
			return true, nil
		}
	}

	// The block has passed all context independent checks and appears sane
	// enough to potentially accept it into the block chain.
	err = b.maybeAcceptBlock(block, timeSource, flags)
	if err != nil {
		return false, err
	}

	// Don't process any orphans or log when the dry run flag is set.
	if !dryRun {
		// Accept any orphan blocks that depend on this block (they are
		// no longer orphans) and repeat for those accepted blocks until
		// there are no more.
		err := b.processOrphans(blockHash, timeSource, flags)
		if err != nil {
			return false, err
		}

		log.Debugf("Accepted block %v", blockHash)
	}

	return false, nil
}
