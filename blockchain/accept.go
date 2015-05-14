// Copyright (c) 2013-2015 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"fmt"

	"github.com/ppcsuite/btcutil"
)

// maybeAcceptBlock potentially accepts a block into the memory block chain.
// It performs several validation checks which depend on its position within
// the block chain before adding it.  The block is expected to have already gone
// through ProcessBlock before calling this function with it.
//
// The flags modify the behavior of this function as follows:
//  - BFDryRun: The memory chain index will not be pruned and no accept
//    notification will be sent since the block is not being accepted.
//
// The flags are also passed to checkBlockContext and connectBestChain.  See
// their documentation for how the flags modify their behavior.
func (b *BlockChain) maybeAcceptBlock(block *btcutil.Block, timeSource MedianTimeSource, flags BehaviorFlags) error {
	dryRun := flags&BFDryRun == BFDryRun

	// Get a block node for the block previous to this one.  Will be nil
	// if this is the genesis block.
	prevNode, err := b.getPrevNodeFromBlock(block)
	if err != nil {
		log.Errorf("getPrevNodeFromBlock: %v", err)
		return err
	}

	// The height of this block is one more than the referenced previous
	// block.
	blockHeight := int64(0)
	if prevNode != nil {
		blockHeight = prevNode.height + 1
	}
	block.SetHeight(blockHeight)

	// The block must pass all of the validation rules which depend on the
	// position of the block within the block chain.
	err = b.checkBlockContext(block, prevNode, flags)
	if err != nil {
		return err
	}

	// ppc: verify hash target and signature of coinstake tx
	// TODO(mably) is it the best place to do that?
	// TODO(mably) a timeSource param is needed to get the AdjustedTime
	err = b.checkBlockProofOfStake(block, timeSource)
	if err != nil {
		str := fmt.Sprintf("Proof of stake check failed for block %v : %v", block.Sha(), err)
		return ruleError(ErrProofOfStakeCheck, str)
	}

	// ppc: populate all ppcoin specific block meta data
	err = b.addToBlockIndex(block)
	if err != nil {
		return err
	}

	// Prune block nodes which are no longer needed before creating
	// a new node.
	if !dryRun {
		err = b.pruneBlockNodes()
		if err != nil {
			return err
		}
	}

	// Create a new block node for the block and add it to the in-memory
	// block chain (could be either a side chain or the main chain).
	blockHeader := &block.MsgBlock().Header
	newNode := ppcNewBlockNode(
		blockHeader, block.Sha(), blockHeight, block.Meta()) // ppc: peercoin
	if prevNode != nil { // Not genesis block
		newNode.parent = prevNode
		newNode.height = blockHeight
		// ppc: newNode.workSum has been initialied to block trust in ppcNewBlockNode
		newNode.workSum.Add(prevNode.workSum, newNode.workSum)
	}

	// Connect the passed block to the chain while respecting proper chain
	// selection according to the chain with the most proof of work.  This
	// also handles validation of the transaction scripts.
	err = b.connectBestChain(newNode, block, flags)
	if err != nil {
		return err
	}

	// Notify the caller that the new block was accepted into the block
	// chain.  The caller would typically want to react by relaying the
	// inventory to other peers.
	if !dryRun {
		b.sendNotification(NTBlockAccepted, block)
	}

	return nil
}
