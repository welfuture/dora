package beacon

import (
	"fmt"
	"math"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethpandaops/dora/db"
	"github.com/ethpandaops/dora/dbtypes"
	"github.com/ethpandaops/dora/utils"
	"github.com/jmoiron/sqlx"
	"github.com/juliangruber/go-intersect"
)

type dbWriter struct {
	indexer *Indexer
}

func newDbWriter(indexer *Indexer) *dbWriter {
	return &dbWriter{
		indexer: indexer,
	}
}

func (dbw *dbWriter) persistMissedSlots(tx *sqlx.Tx, epoch phase0.Epoch, blocks []*Block, epochStats *EpochStats) error {
	chainState := dbw.indexer.consensusPool.GetChainState()
	epochStatsValues := epochStats.GetValues(chainState, true)

	// insert missed slots
	firstSlot := chainState.EpochStartSlot(epoch)
	lastSlot := firstSlot + phase0.Slot(chainState.GetSpecs().SlotsPerEpoch)
	blockIdx := 0

	for slot := firstSlot; slot < lastSlot; slot++ {
		if blockIdx < len(blocks) && blocks[blockIdx].Slot == slot {
			blockIdx++
			continue
		}

		proposer := phase0.ValidatorIndex(math.MaxInt64)
		if epochStatsValues != nil {
			proposer = epochStatsValues.ProposerDuties[int(slot-firstSlot)]
		}

		missedSlot := &dbtypes.SlotHeader{
			Slot:     uint64(slot),
			Proposer: uint64(proposer),
			Status:   dbtypes.Missing,
		}

		err := db.InsertMissingSlot(missedSlot, tx)
		if err != nil {
			return fmt.Errorf("error while adding missed slot to db: %w", err)
		}
	}
	return nil
}

func (dbw *dbWriter) persistBlockData(tx *sqlx.Tx, block *Block, epochStats *EpochStats, depositIndex *uint64, orphaned bool, overrideForkId *ForkKey) error {
	// insert block
	dbBlock := dbw.buildDbBlock(block, epochStats, overrideForkId)
	if orphaned {
		dbBlock.Status = dbtypes.Orphaned
	}

	err := db.InsertSlot(dbBlock, tx)
	if err != nil {
		return fmt.Errorf("error inserting slot: %v", err)
	}

	block.isInFinalizedDb = true

	// insert child objects
	err = dbw.persistBlockChildObjects(tx, block, depositIndex, orphaned, overrideForkId)
	if err != nil {
		return err
	}

	return nil
}

func (dbw *dbWriter) persistBlockChildObjects(tx *sqlx.Tx, block *Block, depositIndex *uint64, orphaned bool, overrideForkId *ForkKey) error {
	var err error

	// insert deposits
	err = dbw.persistBlockDeposits(tx, block, depositIndex, orphaned, overrideForkId)
	if err != nil {
		return err
	}

	// insert voluntary exits
	err = dbw.persistBlockVoluntaryExits(tx, block, orphaned, overrideForkId)
	if err != nil {
		return err
	}

	// insert slashings
	err = dbw.persistBlockSlashings(tx, block, orphaned, overrideForkId)
	if err != nil {
		return err
	}

	// insert consolidations
	err = dbw.persistBlockConsolidations(tx, block, orphaned)
	if err != nil {
		return err
	}

	// insert el requests
	err = dbw.persistBlockElRequests(tx, block, orphaned)
	if err != nil {
		return err
	}

	return nil
}

func (dbw *dbWriter) persistEpochData(tx *sqlx.Tx, epoch phase0.Epoch, blocks []*Block, epochStats *EpochStats, epochVotes *EpochVotes) error {
	if tx == nil {
		return db.RunDBTransaction(func(tx *sqlx.Tx) error {
			return dbw.persistEpochData(tx, epoch, blocks, epochStats, epochVotes)
		})
	}
	canonicalForkId := ForkKey(0)

	dbEpoch := dbw.buildDbEpoch(epoch, blocks, epochStats, epochVotes, func(block *Block, depositIndex *uint64) {
		err := dbw.persistBlockData(tx, block, epochStats, depositIndex, false, &canonicalForkId)
		if err != nil {
			dbw.indexer.logger.Errorf("error persisting slot: %v", err)
		}
	})

	// insert missing slots
	err := dbw.persistMissedSlots(tx, epoch, blocks, epochStats)
	if err != nil {
		return err
	}

	// insert epoch
	err = db.InsertEpoch(dbEpoch, tx)
	if err != nil {
		return fmt.Errorf("error while saving epoch to db: %w", err)
	}

	return nil
}

func (dbw *dbWriter) persistSyncAssignments(tx *sqlx.Tx, epoch phase0.Epoch, epochStats *EpochStats) error {
	chainState := dbw.indexer.consensusPool.GetChainState()
	specs := chainState.GetSpecs()

	if epoch < phase0.Epoch(specs.AltairForkEpoch) {
		// no sync committees before altair
		return nil
	}

	var epochStatsValues *EpochStatsValues
	if epochStats != nil {
		epochStatsValues = epochStats.GetValues(chainState, true)
	}
	if epochStatsValues == nil {
		return nil
	}

	period := epoch / phase0.Epoch(specs.EpochsPerSyncCommitteePeriod)
	isStartOfPeriod := epoch == period*phase0.Epoch(specs.EpochsPerSyncCommitteePeriod)
	if !isStartOfPeriod && db.IsSyncCommitteeSynchronized(uint64(period)) {
		// already synchronized
		return nil
	}

	syncAssignments := make([]*dbtypes.SyncAssignment, 0)
	for idx, val := range epochStatsValues.SyncCommitteeDuties {
		syncAssignments = append(syncAssignments, &dbtypes.SyncAssignment{
			Period:    uint64(period),
			Index:     uint32(idx),
			Validator: uint64(val),
		})
	}
	return db.InsertSyncAssignments(syncAssignments, tx)
}

func (dbw *dbWriter) buildDbBlock(block *Block, epochStats *EpochStats, overrideForkId *ForkKey) *dbtypes.Slot {
	chainState := dbw.indexer.consensusPool.GetChainState()
	blockBody := block.GetBlock()
	if blockBody == nil {
		dbw.indexer.logger.Errorf("error while building db blocks: block body not found: %v", block.Slot)
		return nil
	}

	var epochStatsValues *EpochStatsValues
	if epochStats != nil {
		epochStatsValues = epochStats.GetValues(chainState, true)
	}

	graffiti, _ := blockBody.Graffiti()
	attestations, _ := blockBody.Attestations()
	deposits, _ := blockBody.Deposits()
	voluntaryExits, _ := blockBody.VoluntaryExits()
	attesterSlashings, _ := blockBody.AttesterSlashings()
	proposerSlashings, _ := blockBody.ProposerSlashings()
	blsToExecChanges, _ := blockBody.BLSToExecutionChanges()
	syncAggregate, _ := blockBody.SyncAggregate()
	executionBlockNumber, _ := blockBody.ExecutionBlockNumber()
	executionBlockHash, _ := blockBody.ExecutionBlockHash()
	executionExtraData, _ := getBlockExecutionExtraData(blockBody)
	executionTransactions, _ := blockBody.ExecutionTransactions()
	executionWithdrawals, _ := blockBody.Withdrawals()

	dbBlock := dbtypes.Slot{
		Slot:                  uint64(block.header.Message.Slot),
		Proposer:              uint64(block.header.Message.ProposerIndex),
		Status:                dbtypes.Canonical,
		ForkId:                uint64(block.forkId),
		Root:                  block.Root[:],
		ParentRoot:            block.header.Message.ParentRoot[:],
		StateRoot:             block.header.Message.StateRoot[:],
		Graffiti:              graffiti[:],
		GraffitiText:          utils.GraffitiToString(graffiti[:]),
		AttestationCount:      uint64(len(attestations)),
		DepositCount:          uint64(len(deposits)),
		ExitCount:             uint64(len(voluntaryExits)),
		AttesterSlashingCount: uint64(len(attesterSlashings)),
		ProposerSlashingCount: uint64(len(proposerSlashings)),
		BLSChangeCount:        uint64(len(blsToExecChanges)),
	}

	if overrideForkId != nil {
		dbBlock.ForkId = uint64(*overrideForkId)
	}

	if syncAggregate != nil {
		var assignedCount int
		if epochStatsValues != nil {
			assignedCount = len(epochStatsValues.SyncCommitteeDuties)
		} else {
			// this is not accurate, but best we can get without epoch assignments
			assignedCount = len(syncAggregate.SyncCommitteeBits) * 8
		}

		votedCount := 0
		for i := 0; i < assignedCount; i++ {
			if utils.BitAtVector(syncAggregate.SyncCommitteeBits, i) {
				votedCount++
			}
		}
		dbBlock.SyncParticipation = float32(votedCount) / float32(assignedCount)
	}

	if executionBlockNumber > 0 {
		dbBlock.EthTransactionCount = uint64(len(executionTransactions))
		dbBlock.EthBlockNumber = &executionBlockNumber
		dbBlock.EthBlockHash = executionBlockHash[:]
		dbBlock.EthBlockExtra = executionExtraData
		dbBlock.EthBlockExtraText = utils.GraffitiToString(executionExtraData[:])
		dbBlock.WithdrawCount = uint64(len(executionWithdrawals))
		for _, withdrawal := range executionWithdrawals {
			dbBlock.WithdrawAmount += uint64(withdrawal.Amount)
		}
	}

	return &dbBlock
}

func (dbw *dbWriter) buildDbEpoch(epoch phase0.Epoch, blocks []*Block, epochStats *EpochStats, epochVotes *EpochVotes, blockFn func(block *Block, depositIndex *uint64)) *dbtypes.Epoch {
	chainState := dbw.indexer.consensusPool.GetChainState()

	var epochStatsValues *EpochStatsValues
	if epochStats != nil {
		epochStatsValues = epochStats.GetValues(chainState, true)
	}

	// insert missed slots
	firstSlot := chainState.EpochStartSlot(epoch)
	lastSlot := firstSlot + phase0.Slot(chainState.GetSpecs().SlotsPerEpoch) - 1

	totalSyncAssigned := 0
	totalSyncVoted := 0
	var depositIndex *uint64
	dbEpoch := dbtypes.Epoch{
		Epoch: uint64(epoch),
	}
	if epochVotes != nil {
		dbEpoch.VotedTarget = uint64(epochVotes.CurrentEpoch.TargetVoteAmount + epochVotes.NextEpoch.TargetVoteAmount)
		dbEpoch.VotedHead = uint64(epochVotes.CurrentEpoch.HeadVoteAmount + epochVotes.NextEpoch.HeadVoteAmount)
		dbEpoch.VotedTotal = uint64(epochVotes.CurrentEpoch.TotalVoteAmount + epochVotes.NextEpoch.TotalVoteAmount)
	}
	if epochStatsValues != nil {
		dbEpoch.ValidatorCount = epochStatsValues.ActiveValidators
		dbEpoch.ValidatorBalance = uint64(epochStatsValues.ActiveBalance)
		dbEpoch.Eligible = uint64(epochStatsValues.EffectiveBalance)
		depositIndexField := epochStatsValues.FirstDepositIndex
		depositIndex = &depositIndexField
	}

	// aggregate blocks
	blockIdx := 0
	for slot := firstSlot; slot <= lastSlot; slot++ {
		var block *Block

		if blockIdx < len(blocks) && blocks[blockIdx].Slot == slot {
			block = blocks[blockIdx]
			blockIdx++
		}

		if block != nil {
			dbEpoch.BlockCount++
			blockBody := block.GetBlock()
			if blockBody == nil {
				dbw.indexer.logger.Errorf("error while building db epoch: block body not found for aggregation: %v", block.Slot)
				continue
			}
			if blockFn != nil {
				blockFn(block, depositIndex)
			}

			attestations, _ := blockBody.Attestations()
			deposits, _ := blockBody.Deposits()
			voluntaryExits, _ := blockBody.VoluntaryExits()
			attesterSlashings, _ := blockBody.AttesterSlashings()
			proposerSlashings, _ := blockBody.ProposerSlashings()
			blsToExecChanges, _ := blockBody.BLSToExecutionChanges()
			syncAggregate, _ := blockBody.SyncAggregate()
			executionTransactions, _ := blockBody.ExecutionTransactions()
			executionWithdrawals, _ := blockBody.Withdrawals()

			dbEpoch.AttestationCount += uint64(len(attestations))
			dbEpoch.DepositCount += uint64(len(deposits))
			dbEpoch.ExitCount += uint64(len(voluntaryExits))
			dbEpoch.AttesterSlashingCount += uint64(len(attesterSlashings))
			dbEpoch.ProposerSlashingCount += uint64(len(proposerSlashings))
			dbEpoch.BLSChangeCount += uint64(len(blsToExecChanges))

			if syncAggregate != nil && epochStatsValues != nil {
				votedCount := 0
				assignedCount := len(epochStatsValues.SyncCommitteeDuties)
				for i := 0; i < assignedCount; i++ {
					if utils.BitAtVector(syncAggregate.SyncCommitteeBits, i) {
						votedCount++
					}
				}
				totalSyncAssigned += assignedCount
				totalSyncVoted += votedCount
			}

			dbEpoch.EthTransactionCount += uint64(len(executionTransactions))
			dbEpoch.WithdrawCount += uint64(len(executionWithdrawals))
			for _, withdrawal := range executionWithdrawals {
				dbEpoch.WithdrawAmount += uint64(withdrawal.Amount)
			}
		}
	}

	if totalSyncAssigned > 0 {
		dbEpoch.SyncParticipation = float32(totalSyncVoted) / float32(totalSyncAssigned)
	}

	return &dbEpoch
}

func (dbw *dbWriter) persistBlockDeposits(tx *sqlx.Tx, block *Block, depositIndex *uint64, orphaned bool, overrideForkId *ForkKey) error {
	// insert deposits
	dbDeposits := dbw.buildDbDeposits(block, depositIndex, orphaned, overrideForkId)
	if orphaned {
		for idx := range dbDeposits {
			dbDeposits[idx].Orphaned = true
		}
	}

	if len(dbDeposits) > 0 {
		err := db.InsertDeposits(dbDeposits, tx)
		if err != nil {
			return fmt.Errorf("error inserting deposits: %v", err)
		}
	}

	return nil
}

func (dbw *dbWriter) buildDbDeposits(block *Block, depositIndex *uint64, orphaned bool, overrideForkId *ForkKey) []*dbtypes.Deposit {
	blockBody := block.GetBlock()
	if blockBody == nil {
		return nil
	}

	deposits, err := blockBody.Deposits()
	if err != nil {
		return nil
	}

	dbDeposits := make([]*dbtypes.Deposit, len(deposits))
	for idx, deposit := range deposits {
		dbDeposit := &dbtypes.Deposit{
			SlotNumber:            uint64(block.Slot),
			SlotIndex:             uint64(idx),
			SlotRoot:              block.Root[:],
			Orphaned:              orphaned,
			ForkId:                uint64(block.forkId),
			PublicKey:             deposit.Data.PublicKey[:],
			WithdrawalCredentials: deposit.Data.WithdrawalCredentials,
			Amount:                uint64(deposit.Data.Amount),
		}
		if depositIndex != nil {
			cDepIdx := *depositIndex
			dbDeposit.Index = &cDepIdx
			*depositIndex++
		}
		if overrideForkId != nil {
			dbDeposit.ForkId = uint64(*overrideForkId)
		}

		dbDeposits[idx] = dbDeposit
	}

	return dbDeposits
}

func (dbw *dbWriter) persistBlockVoluntaryExits(tx *sqlx.Tx, block *Block, orphaned bool, overrideForkId *ForkKey) error {
	// insert voluntary exits
	dbVoluntaryExits := dbw.buildDbVoluntaryExits(block, orphaned, overrideForkId)
	if len(dbVoluntaryExits) > 0 {
		err := db.InsertVoluntaryExits(dbVoluntaryExits, tx)
		if err != nil {
			return fmt.Errorf("error inserting voluntary exits: %v", err)
		}
	}

	return nil
}

func (dbw *dbWriter) buildDbVoluntaryExits(block *Block, orphaned bool, overrideForkId *ForkKey) []*dbtypes.VoluntaryExit {
	blockBody := block.GetBlock()
	if blockBody == nil {
		return nil
	}

	voluntaryExits, err := blockBody.VoluntaryExits()
	if err != nil {
		return nil
	}

	dbVoluntaryExits := make([]*dbtypes.VoluntaryExit, len(voluntaryExits))
	for idx, voluntaryExit := range voluntaryExits {
		dbVoluntaryExit := &dbtypes.VoluntaryExit{
			SlotNumber:     uint64(block.Slot),
			SlotIndex:      uint64(idx),
			SlotRoot:       block.Root[:],
			Orphaned:       orphaned,
			ForkId:         uint64(block.forkId),
			ValidatorIndex: uint64(voluntaryExit.Message.ValidatorIndex),
		}
		if overrideForkId != nil {
			dbVoluntaryExit.ForkId = uint64(*overrideForkId)
		}

		dbVoluntaryExits[idx] = dbVoluntaryExit
	}

	return dbVoluntaryExits
}

func (dbw *dbWriter) persistBlockSlashings(tx *sqlx.Tx, block *Block, orphaned bool, overrideForkId *ForkKey) error {
	// insert slashings
	dbSlashings := dbw.buildDbSlashings(block, orphaned, overrideForkId)
	if len(dbSlashings) > 0 {
		err := db.InsertSlashings(dbSlashings, tx)
		if err != nil {
			return fmt.Errorf("error inserting slashings: %v", err)
		}
	}

	return nil
}

func (dbw *dbWriter) buildDbSlashings(block *Block, orphaned bool, overrideForkId *ForkKey) []*dbtypes.Slashing {
	blockBody := block.GetBlock()
	if blockBody == nil {
		return nil
	}

	proposerSlashings, err := blockBody.ProposerSlashings()
	if err != nil {
		return nil
	}

	attesterSlashings, err := blockBody.AttesterSlashings()
	if err != nil {
		return nil
	}

	proposerIndex, err := blockBody.ProposerIndex()
	if err != nil {
		return nil
	}

	dbSlashings := []*dbtypes.Slashing{}
	slashingIndex := 0

	for _, proposerSlashing := range proposerSlashings {
		dbSlashing := &dbtypes.Slashing{
			SlotNumber:     uint64(block.Slot),
			SlotIndex:      uint64(slashingIndex),
			SlotRoot:       block.Root[:],
			Orphaned:       orphaned,
			ForkId:         uint64(block.forkId),
			ValidatorIndex: uint64(proposerSlashing.SignedHeader1.Message.ProposerIndex),
			SlasherIndex:   uint64(proposerIndex),
			Reason:         dbtypes.ProposerSlashing,
		}
		if overrideForkId != nil {
			dbSlashing.ForkId = uint64(*overrideForkId)
		}

		slashingIndex++
		dbSlashings = append(dbSlashings, dbSlashing)
	}

	for _, attesterSlashing := range attesterSlashings {
		att1, _ := attesterSlashing.Attestation1()
		att2, _ := attesterSlashing.Attestation2()
		if att1 == nil || att2 == nil {
			continue
		}

		att1AttestingIndices, _ := att1.AttestingIndices()
		att2AttestingIndices, _ := att2.AttestingIndices()
		if att1AttestingIndices == nil || att2AttestingIndices == nil {
			continue
		}

		inter := intersect.Simple(att1AttestingIndices, att2AttestingIndices)
		for _, j := range inter {
			valIdx := j.(uint64)

			dbSlashing := &dbtypes.Slashing{
				SlotNumber:     uint64(block.Slot),
				SlotIndex:      uint64(slashingIndex),
				SlotRoot:       block.Root[:],
				Orphaned:       false,
				ValidatorIndex: uint64(valIdx),
				SlasherIndex:   uint64(proposerIndex),
				Reason:         dbtypes.AttesterSlashing,
			}
			dbSlashings = append(dbSlashings, dbSlashing)
		}

		slashingIndex++
	}

	return dbSlashings
}

func (dbw *dbWriter) persistBlockConsolidations(tx *sqlx.Tx, block *Block, orphaned bool) error {
	// insert deposits
	dbConsolidations := dbw.buildDbConsolidations(block)
	if orphaned {
		for idx := range dbConsolidations {
			dbConsolidations[idx].Orphaned = true
		}
	}

	if len(dbConsolidations) > 0 {
		err := db.InsertConsolidations(dbConsolidations, tx)
		if err != nil {
			return fmt.Errorf("error inserting consolidations: %v", err)
		}
	}

	return nil
}

func (dbw *dbWriter) buildDbConsolidations(block *Block) []*dbtypes.Consolidation {
	blockBody := block.GetBlock()
	if blockBody == nil {
		return nil
	}

	/*
		consolidations, err := blockBody.Consolidations()
		if err != nil {
			return nil
		}

		dbConsolidations := make([]*dbtypes.Consolidation, len(consolidations))
		for idx, consolidation := range consolidations {
			dbConsolidation := &dbtypes.Consolidation{
				SlotNumber:  block.Slot,
				SlotIndex:   uint64(idx),
				SlotRoot:    block.Root,
				Orphaned:    false,
				SourceIndex: uint64(consolidation.Message.SourceIndex),
				TargetIndex: uint64(consolidation.Message.TargetIndex),
				Epoch:       uint64(consolidation.Message.Epoch),
			}

			dbConsolidations[idx] = dbConsolidation
		}

		return dbConsolidations
	*/

	return []*dbtypes.Consolidation{}
}

func (dbw *dbWriter) persistBlockElRequests(tx *sqlx.Tx, block *Block, orphaned bool) error {
	// insert deposits
	dbElRequests := dbw.buildDbElRequests(block)
	if orphaned {
		for idx := range dbElRequests {
			dbElRequests[idx].Orphaned = true
		}
	}

	if len(dbElRequests) > 0 {
		err := db.InsertElRequests(dbElRequests, tx)
		if err != nil {
			return fmt.Errorf("error inserting el requests: %v", err)
		}
	}

	return nil
}

func (dbw *dbWriter) buildDbElRequests(block *Block) []*dbtypes.ElRequest {
	blockBody := block.GetBlock()
	if blockBody == nil {
		return nil
	}

	/*
		elRequests, err := blockBody.ExecutionRequests()
		if err != nil {
			return nil
		}

		dbElRequests := make([]*dbtypes.ElRequest, len(elRequests))
		for idx, elRequest := range elRequests {
			dbElRequest := &dbtypes.ElRequest{
				SlotNumber:  block.Slot,
				SlotIndex:   uint64(idx),
				SlotRoot:    block.Root,
				Orphaned:    false,
			}

			dbElRequests[idx] = dbElRequest
		}

		return dbElRequests
	*/

	return []*dbtypes.ElRequest{}
}
