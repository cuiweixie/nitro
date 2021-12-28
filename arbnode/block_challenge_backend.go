//
// Copyright 2021, Offchain Labs, Inc. All rights reserved.
//

package arbnode

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/offchainlabs/arbstate/solgen/go/challengegen"
	"github.com/offchainlabs/arbstate/validator"
	"github.com/pkg/errors"
)

type GoGlobalState struct {
	BlockHash  common.Hash
	Batch      uint64
	PosInBatch uint64
}

func u64ToBe(x uint64) []byte {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, x)
	return data
}

func (s GoGlobalState) Hash() common.Hash {
	data := []byte("Global state:")
	data = append(data, s.BlockHash.Bytes()...)
	data = append(data, u64ToBe(s.Batch)...)
	data = append(data, u64ToBe(s.PosInBatch)...)
	return crypto.Keccak256Hash(data)
}

func GoGlobalStateFromSolidity(gs challengegen.GlobalState) GoGlobalState {
	return GoGlobalState{
		BlockHash:  gs.Bytes32Vals[0],
		Batch:      gs.U64Vals[0],
		PosInBatch: gs.U64Vals[1],
	}
}

func (s GoGlobalState) AsSolidityStruct() challengegen.GlobalState {
	return challengegen.GlobalState{
		Bytes32Vals: [1][32]byte{s.BlockHash},
		U64Vals:     [2]uint64{s.Batch, s.PosInBatch},
	}
}

type BlockChallengeBackend struct {
	bc                     *core.BlockChain
	startBlock             uint64
	startPosition          uint64
	endPosition            uint64
	startGs                GoGlobalState
	endGs                  GoGlobalState
	inboxTracker           *InboxTracker
	tooFarStartsAtPosition uint64
}

// Assert that BlockChallengeBackend implements ChallengeBackend
var _ validator.ChallengeBackend = (*BlockChallengeBackend)(nil)

func NewBlockChallengeBackend(ctx context.Context, bc *core.BlockChain, inboxTracker *InboxTracker, client bind.ContractBackend, challengeAddr common.Address) (*BlockChallengeBackend, error) {
	callOpts := &bind.CallOpts{Context: ctx}
	challengeCon, err := challengegen.NewBlockChallenge(challengeAddr, client)
	if err != nil {
		return nil, err
	}

	solStartGs, err := challengeCon.GetStartGlobalState(callOpts)
	if err != nil {
		return nil, err
	}
	startGs := GoGlobalStateFromSolidity(solStartGs)
	if startGs.PosInBatch != 0 {
		return nil, errors.New("challenge started misaligned with batch boundary")
	}
	startBlock := bc.GetBlockByHash(startGs.BlockHash)
	if startBlock == nil {
		return nil, errors.New("failed to find start block")
	}
	startBlockNum := startBlock.NumberU64()

	var startBatchMeta BatchMetadata
	if startGs.Batch > 0 {
		startBatchMeta, err = inboxTracker.GetBatchMetadata(startGs.Batch - 1)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get challenge start batch metadata")
		}
	}
	if startBatchMeta.MessageCount != startBlockNum {
		return nil, errors.New("start block and start message count are not 1:1")
	}

	solEndGs, err := challengeCon.GetEndGlobalState(callOpts)
	if err != nil {
		return nil, err
	}
	endGs := GoGlobalStateFromSolidity(solEndGs)
	if endGs.PosInBatch != 0 {
		return nil, errors.New("challenge ended misaligned with batch boundary")
	}
	if endGs.Batch <= startGs.Batch {
		return nil, errors.New("challenge didn't advance batch")
	}
	lastBatchMeta, err := inboxTracker.GetBatchMetadata(endGs.Batch - 1)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get challenge end batch metadata")
	}
	endMsgCount := lastBatchMeta.MessageCount
	endBatchBlock := bc.GetBlockByNumber(endMsgCount)
	if endBatchBlock == nil {
		return nil, errors.New("missing block at end of last challenge batch")
	}

	return &BlockChallengeBackend{
		bc:                     bc,
		startBlock:             startBlockNum,
		startGs:                startGs,
		startPosition:          0,
		endPosition:            math.MaxUint64,
		endGs:                  endGs,
		inboxTracker:           inboxTracker,
		tooFarStartsAtPosition: endMsgCount - startBlockNum + 1,
	}, nil
}

func (b *BlockChallengeBackend) findBatchFromMessageCount(ctx context.Context, msgCount uint64) (uint64, error) {
	if msgCount == 0 {
		return 0, nil
	}
	low := b.startGs.Batch
	high := b.endGs.Batch
	if b.endGs.PosInBatch == 0 {
		if high == 0 {
			return 0, errors.New("end global state at inbox position (0, 0)")
		}
		high--
	}
	for {
		// Binary search invariants:
		//   - messageCount(high) >= msgCount
		//   - messageCount(low-1) < msgCount
		mid := (low + high) / 2
		batchMeta, err := b.inboxTracker.GetBatchMetadata(mid)
		if err != nil {
			return 0, errors.Wrap(err, "failed to get batch metadata while binary searching")
		}
		if batchMeta.MessageCount < msgCount {
			low = mid + 1
		} else if batchMeta.MessageCount == msgCount {
			return mid, nil
		} else if mid == low { // batchMeta.MessageCount > msgCount
			return mid, nil
		} else { // batchMeta.MessageCount > msgCount
			high = mid
		}
	}
}

const STATUS_FINISHED uint8 = 1
const STATUS_TOO_FAR uint8 = 3

func (b *BlockChallengeBackend) getInfoAtStep(ctx context.Context, position uint64) (GoGlobalState, uint8, error) {
	if position >= b.tooFarStartsAtPosition {
		return GoGlobalState{}, STATUS_TOO_FAR, nil
	}
	block := b.bc.GetBlockByNumber(b.startBlock + position)
	if block == nil {
		return GoGlobalState{}, 0, errors.New("failed to get block in block challenge")
	}
	msgCount := b.startBlock + position
	batch, err := b.findBatchFromMessageCount(ctx, msgCount)
	if err != nil {
		return GoGlobalState{}, 0, err
	}
	var prevBatchMeta BatchMetadata
	if batch > 0 {
		prevBatchMeta, err = b.inboxTracker.GetBatchMetadata(batch - 1)
		if err != nil {
			return GoGlobalState{}, 0, err
		}
		if prevBatchMeta.MessageCount >= msgCount {
			return GoGlobalState{}, 0, errors.New("findBatchFromMessageCount returned bad block")
		}
	}
	globalState := GoGlobalState{
		Batch:      batch,
		PosInBatch: msgCount - prevBatchMeta.MessageCount,
		BlockHash:  block.Hash(),
	}
	return globalState, STATUS_FINISHED, nil
}

func (b *BlockChallengeBackend) SetRange(ctx context.Context, start uint64, end uint64) error {
	if b.startPosition == start && b.endPosition == end {
		return nil
	}
	newStartGs, _, err := b.getInfoAtStep(ctx, start)
	if err != nil {
		return err
	}
	newEndGs, endStatus, err := b.getInfoAtStep(ctx, end)
	if err != nil {
		return err
	}
	b.startGs = newStartGs
	if endStatus == STATUS_FINISHED {
		b.endGs = newEndGs
	}
	return nil
}

func (b *BlockChallengeBackend) GetHashAtStep(ctx context.Context, position uint64) (common.Hash, error) {
	gs, status, err := b.getInfoAtStep(ctx, position)
	if err != nil {
		return common.Hash{}, err
	}
	if status == STATUS_FINISHED {
		data := []byte("Block state:")
		data = append(data, gs.Hash().Bytes()...)
		return crypto.Keccak256Hash(data), nil
	} else if status == STATUS_TOO_FAR {
		return crypto.Keccak256Hash([]byte("Block state, too far:")), nil
	} else {
		panic(fmt.Sprintf("Unknown block status: %v", status))
	}
}

func (b *BlockChallengeBackend) IssueOneStepProof(ctx context.Context, client bind.ContractBackend, auth *bind.TransactOpts, challenge common.Address, oldState validator.ChallengeState, startSegment int) (*types.Transaction, error) {
	con, err := challengegen.NewBlockChallenge(challenge, client)
	if err != nil {
		return nil, err
	}
	position := oldState.Segments[startSegment].Position
	machineStatuses := [2]uint8{}
	globalStates := [2]GoGlobalState{}
	globalStates[0], machineStatuses[0], err = b.getInfoAtStep(ctx, position)
	if err != nil {
		return nil, err
	}
	globalStates[1], machineStatuses[1], err = b.getInfoAtStep(ctx, position+1)
	if err != nil {
		return nil, err
	}
	globalStateHashes := [2][32]byte{
		globalStates[0].Hash(),
		globalStates[1].Hash(),
	}
	return con.ChallengeExecution(
		auth,
		oldState.Start,
		new(big.Int).Sub(oldState.End, oldState.Start),
		oldState.RawSegments,
		big.NewInt(int64(startSegment)),
		machineStatuses,
		globalStateHashes,
	)
}
