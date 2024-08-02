package beacon

import (
	"runtime/debug"
	"sync"
	"time"

	v1 "github.com/attestantio/go-eth2-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/jmoiron/sqlx"
	dynssz "github.com/pk910/dynamic-ssz"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/dora/clients/consensus"
	"github.com/ethpandaops/dora/db"
	"github.com/ethpandaops/dora/dbtypes"
	"github.com/ethpandaops/dora/utils"
	"github.com/ethpandaops/ethwallclock"
)

const EtherGweiFactor = 1_000_000_000

// Indexer is responsible for indexing the ethereum beacon chain.
type Indexer struct {
	logger        logrus.FieldLogger
	consensusPool *consensus.Pool
	dynSsz        *dynssz.DynSsz
	synchronizer  *synchronizer

	// configuration
	writeDb               bool
	disableSync           bool
	blockCompression      bool
	inMemoryEpochs        uint16
	maxParallelStateCalls uint16
	cachePersistenceDelay uint16

	// caches
	blockCache *blockCache
	epochCache *epochCache
	forkCache  *forkCache

	// indexer state
	clients               []*Client
	dbWriter              *dbWriter
	running               bool
	lastFinalizedEpoch    phase0.Epoch
	lastPrunedEpoch       phase0.Epoch
	lastPruneRunEpoch     phase0.Epoch
	finalitySubscription  *consensus.Subscription[*v1.Finality]
	wallclockSubscription *consensus.Subscription[*ethwallclock.Slot]
}

// NewIndexer creates a new instance of the Indexer.
func NewIndexer(logger logrus.FieldLogger, consensusPool *consensus.Pool) *Indexer {
	// Initialize the indexer with default values from the configuration.
	inMemoryEpochs := utils.Config.Indexer.InMemoryEpochs
	if inMemoryEpochs < 2 {
		inMemoryEpochs = 2
	}
	cachePersistenceDelay := utils.Config.Indexer.CachePersistenceDelay
	if cachePersistenceDelay < 2 {
		cachePersistenceDelay = 2
	}
	maxParallelStateCalls := uint16(utils.Config.Indexer.MaxParallelValidatorSetRequests)
	if maxParallelStateCalls < 2 {
		maxParallelStateCalls = 2
	}
	blockCompression := true
	if utils.Config.KillSwitch.DisableBlockCompression {
		blockCompression = false
	}

	// initialize dynamic SSZ encoder
	staticSpec := map[string]any{}
	specYaml, err := yaml.Marshal(consensusPool.GetChainState().GetSpecs())
	if err != nil {
		yaml.Unmarshal(specYaml, staticSpec)
	}

	// Create the indexer instance.
	indexer := &Indexer{
		logger:        logger,
		consensusPool: consensusPool,
		dynSsz:        dynssz.NewDynSsz(staticSpec),

		writeDb:               !utils.Config.Indexer.DisableIndexWriter,
		disableSync:           utils.Config.Indexer.DisableSynchronizer,
		blockCompression:      blockCompression,
		inMemoryEpochs:        inMemoryEpochs,
		maxParallelStateCalls: maxParallelStateCalls,
		cachePersistenceDelay: cachePersistenceDelay,

		clients: make([]*Client, 0),
	}

	indexer.blockCache = newBlockCache(indexer)
	indexer.epochCache = newEpochCache(indexer)
	indexer.forkCache = newForkCache(indexer)
	indexer.dbWriter = newDbWriter(indexer)

	return indexer
}

func (indexer *Indexer) getMinInMemoryEpoch() phase0.Epoch {
	minInMemoryEpoch := phase0.Epoch(0)
	if indexer.lastFinalizedEpoch > 0 {
		minInMemoryEpoch = indexer.lastFinalizedEpoch - 1
	}
	if indexer.lastPrunedEpoch > 0 && indexer.lastPrunedEpoch > minInMemoryEpoch {
		minInMemoryEpoch = indexer.lastPrunedEpoch - 1
	}

	return minInMemoryEpoch
}

// getMinInMemorySlot returns the minimum in-memory slot based on the indexer's configuration.
func (indexer *Indexer) getMinInMemorySlot() phase0.Slot {
	chainState := indexer.consensusPool.GetChainState()
	return chainState.EpochToSlot(indexer.getMinInMemoryEpoch() + 1)
}

// AddClient adds a new consensus pool client to the indexer.
func (indexer *Indexer) AddClient(index uint16, client *consensus.Client, priority int, archive bool, skipValidators bool) *Client {
	logger := indexer.logger.WithField("client", client.GetName())
	indexerClient := newClient(index, client, priority, archive, skipValidators, indexer, logger)
	indexer.clients = append(indexer.clients, indexerClient)

	return indexerClient
}

// StartIndexer starts the indexing process.
func (indexer *Indexer) StartIndexer() {
	if indexer.running {
		return
	}

	indexer.running = true
	indexer.synchronizer = newSynchronizer(indexer, indexer.logger.WithField("service", "synchronizer"))
	chainState := indexer.consensusPool.GetChainState()
	finalizedSlot := chainState.GetFinalizedSlot()
	finalizedEpoch, _ := chainState.GetFinalizedCheckpoint()
	indexer.lastFinalizedEpoch = finalizedEpoch

	pruneState := dbtypes.IndexerPruneState{}
	db.GetExplorerState("indexer.prunestate", &pruneState)
	indexer.lastPrunedEpoch = phase0.Epoch(pruneState.Epoch)

	if indexer.lastPrunedEpoch < finalizedEpoch {
		indexer.lastPrunedEpoch = finalizedEpoch
		err := db.RunDBTransaction(func(tx *sqlx.Tx) error {
			return indexer.forkCache.updatePruningState(tx, indexer.lastPrunedEpoch)
		})
		if err != nil {
			indexer.logger.WithError(err).Errorf("error while updating prune state")
		}
	}

	indexer.lastPruneRunEpoch = chainState.CurrentEpoch()

	// restore unfinalized forks from db
	for _, dbFork := range db.GetUnfinalizedForks(uint64(finalizedSlot)) {
		fork := newForkFromDb(dbFork)
		indexer.forkCache.addFork(fork)
	}

	if err := indexer.forkCache.loadForkState(); err != nil {
		indexer.logger.WithError(err).Errorf("failed loading fork state")
	}

	// restore unfinalized epoch stats from db
	restoredEpochStats := 0
	t1 := time.Now()
	processingLimiter := make(chan bool, 10)
	processingWaitGroup := sync.WaitGroup{}
	err := db.StreamUnfinalizedDuties(func(dbDuty *dbtypes.UnfinalizedDuty) {
		if dbDuty.Epoch < uint64(finalizedEpoch) {
			return
		}

		// restoring epoch stats can be slow as all duties are recomputed
		// parallelize the processing to speed up the restore
		processingWaitGroup.Add(1)
		processingLimiter <- true

		go func() {
			defer func() {
				<-processingLimiter
				processingWaitGroup.Done()
			}()

			epochStats := indexer.epochCache.createOrGetEpochStats(phase0.Epoch(dbDuty.Epoch), phase0.Root(dbDuty.DependentRoot), false)

			err := epochStats.restoreFromDb(dbDuty, indexer.dynSsz, chainState)
			if err != nil {
				indexer.logger.WithError(err).Errorf("failed restoring epoch stats for epoch %v (%x) from db", dbDuty.Epoch, dbDuty.DependentRoot)
				return
			}

			epochStats.isInDb = true

			restoredEpochStats++
			if dbDuty.Epoch < uint64(indexer.lastPrunedEpoch) {
				epochStats.pruneValues()
			}
		}()
	})
	processingWaitGroup.Wait()
	if err != nil {
		indexer.logger.WithError(err).Errorf("failed restoring unfinalized epoch stats from DB")
	} else {
		indexer.logger.Infof("restored %v unfinalized epoch stats from DB (%.3f sec)", restoredEpochStats, time.Since(t1).Seconds())
	}

	// restore unfinalized epoch aggregations from db
	restoredEpochAggregations := 0
	t1 = time.Now()
	err = db.StreamUnfinalizedEpochs(func(unfinalizedEpoch *dbtypes.UnfinalizedEpoch) {
		epochStats := indexer.epochCache.getEpochStats(phase0.Epoch(unfinalizedEpoch.Epoch), phase0.Root(unfinalizedEpoch.DependentRoot))
		if epochStats == nil {
			indexer.logger.Warnf("failed restoring epoch aggregations for epoch %v [%x] from db: epoch stats not found", unfinalizedEpoch.Epoch, unfinalizedEpoch.DependentRoot)
			return
		}

		if epochStats.prunedEpochAggregations == nil {
			epochStats.prunedEpochAggregations = []*dbtypes.UnfinalizedEpoch{}
		}
		epochStats.prunedEpochAggregations = append(epochStats.prunedEpochAggregations, unfinalizedEpoch)
	})
	if err != nil {
		indexer.logger.WithError(err).Errorf("failed restoring unfinalized epoch aggregations from DB")
	} else {
		indexer.logger.Infof("restored %v unfinalized epoch aggregations from DB (%.3f sec)", restoredEpochAggregations, time.Since(t1).Seconds())
	}

	// restore unfinalized blocks from db
	restoredBlockCount := 0
	restoredBodyCount := 0
	t1 = time.Now()
	err = db.StreamUnfinalizedBlocks(func(dbBlock *dbtypes.UnfinalizedBlock) {
		if dbBlock.Slot < uint64(finalizedSlot) {
			return
		}

		block, _ := indexer.blockCache.createOrGetBlock(phase0.Root(dbBlock.Root), phase0.Slot(dbBlock.Slot))
		block.forkId = ForkKey(dbBlock.ForkId)
		block.processingStatus = dbBlock.Status
		block.isInUnfinalizedDb = true

		if dbBlock.HeaderVer != 1 {
			indexer.logger.Warnf("failed unmarshal unfinalized block header %v [%x] from db: unsupported header version", dbBlock.Slot, dbBlock.Root)
			return
		}

		header := &phase0.SignedBeaconBlockHeader{}
		err := header.UnmarshalSSZ(dbBlock.HeaderSSZ)
		if err != nil {
			indexer.logger.Warnf("failed unmarshal unfinalized block header %v [%x] from db: %v", dbBlock.Slot, dbBlock.Root, err)
			return
		}

		block.SetHeader(header)

		blockBody, err := unmarshalVersionedSignedBeaconBlockSSZ(indexer.dynSsz, dbBlock.BlockVer, dbBlock.BlockSSZ)
		if err != nil {
			indexer.logger.Warnf("could not restore unfinalized block body %v [%x] from db: %v", dbBlock.Slot, dbBlock.Root, err)
		} else if block.processingStatus == 0 {
			block.SetBlock(blockBody)
			restoredBodyCount++
		} else {
			block.setBlockIndex(blockBody)
			block.isInFinalizedDb = true
		}

		restoredBlockCount++

		if time.Since(t1) > 5*time.Second {
			indexer.logger.Infof("restoring unfinalized blocks from DB... (%v done)", restoredBlockCount)
			t1 = time.Now()
		}
	})
	if err != nil {
		indexer.logger.WithError(err).Errorf("failed restoring unfinalized blocks from DB")
	} else {
		indexer.logger.Infof("restored %v unfinalized blocks from DB (%v with bodies, %.3f sec)", restoredBlockCount, restoredBodyCount, time.Since(t1).Seconds())
	}

	// start indexing for all clients
	for _, client := range indexer.clients {
		client.startIndexing()
	}

	// add indexer event handlers
	indexer.finalitySubscription = indexer.consensusPool.SubscribeFinalizedEvent(10)
	indexer.wallclockSubscription = indexer.consensusPool.SubscribeWallclockSlotEvent(1)

	go indexer.runIndexerLoop()

	// start synchronizer
	indexer.startSynchronizer(indexer.lastFinalizedEpoch)
}

func (indexer *Indexer) runIndexerLoop() {
	defer func() {
		if err := recover(); err != nil {
			indexer.logger.WithError(err.(error)).Errorf("uncaught panic in indexer.beacon.Indexer.runIndexerLoop subroutine: %v, stack: %v", err, string(debug.Stack()))
			time.Sleep(10 * time.Second)

			go indexer.runIndexerLoop()
		}
	}()

	chainState := indexer.consensusPool.GetChainState()

	for {
		select {
		case finalityEvent := <-indexer.finalitySubscription.Channel():
			err := indexer.processFinalityEvent(finalityEvent)
			if err != nil {
				indexer.logger.WithError(err).Errorf("error processing finality event (epoch: %v, root: %v)", finalityEvent.Finalized.Epoch, finalityEvent.Finalized.Root.String())
			}

			if indexer.lastFinalizedEpoch > indexer.lastPrunedEpoch {
				indexer.lastPrunedEpoch = indexer.lastFinalizedEpoch
				err := db.RunDBTransaction(func(tx *sqlx.Tx) error {
					return indexer.forkCache.updatePruningState(tx, indexer.lastPrunedEpoch)
				})
				if err != nil {
					indexer.logger.WithError(err).Errorf("error while updating prune state")
				}
			}

			err = indexer.runCachePruning()
			if err != nil {
				indexer.logger.WithError(err).Errorf("failed pruning cache")
			}

			indexer.lastPruneRunEpoch = chainState.CurrentEpoch()

		case slotEvent := <-indexer.wallclockSubscription.Channel():
			epoch := chainState.EpochOfSlot(phase0.Slot(slotEvent.Number()))
			slotIndex := chainState.SlotToSlotIndex(phase0.Slot(slotEvent.Number()))
			slotProgress := uint8(100 / chainState.GetSpecs().SlotsPerEpoch * uint64(slotIndex))

			// prune cache if last pruning epoch is outdated and we are at least 50% into the current
			if epoch > indexer.lastPruneRunEpoch && slotProgress >= 50 {
				err := indexer.runCachePruning()
				if err != nil {
					indexer.logger.WithError(err).Errorf("failed pruning cache")
				}

				indexer.lastPruneRunEpoch = epoch
			}

		}
	}
}
