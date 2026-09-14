package execution

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/OffchainLabs/prysm/v7/beacon-chain/cache/depositsnapshot"
	"github.com/OffchainLabs/prysm/v7/beacon-chain/core/transition"
	"github.com/OffchainLabs/prysm/v7/beacon-chain/db"
	dbtest "github.com/OffchainLabs/prysm/v7/beacon-chain/db/testing"
	"github.com/OffchainLabs/prysm/v7/beacon-chain/execution/types"
	ethpb "github.com/OffchainLabs/prysm/v7/proto/prysm/v1alpha1"
	"github.com/OffchainLabs/prysm/v7/testing/assert"
	"github.com/OffchainLabs/prysm/v7/testing/require"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	gethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/pkg/errors"
)

var (
	currentContract = common.HexToAddress("0x1111111111111111111111111111111111111111")
	retiredContract = common.HexToAddress("0x2222222222222222222222222222222222222222")
)

// capturingLogger records the last filter query it was asked to serve and returns no logs, so that
// tests can assert on which contract and block range the service decided to scan.
type capturingLogger struct {
	queries []ethereum.FilterQuery
}

func (c *capturingLogger) FilterLogs(_ context.Context, q ethereum.FilterQuery) ([]gethTypes.Log, error) {
	c.queries = append(c.queries, q)
	return nil, nil
}

func (*capturingLogger) SubscribeFilterLogs(_ context.Context, _ ethereum.FilterQuery, ch chan<- gethTypes.Log) (ethereum.Subscription, error) {
	return new(event.Feed).Subscribe(ch), nil
}

// switchService builds the smallest service able to run the log scanning paths. Chainstart is marked
// done so that header lookups are skipped and only the filter query behaviour is under test.
func switchService(logger *capturingLogger, switchBlock uint64) *Service {
	depositCache, err := depositsnapshot.New()
	if err != nil {
		panic(err)
	}
	genState, err := transition.EmptyGenesisState()
	if err != nil {
		panic(err)
	}
	return &Service{
		cfg: &config{
			depositContractAddr:        currentContract,
			retiredDepositContractAddr: retiredContract,
			depositContractSwitchBlock: switchBlock,
			eth1HeaderReqLimit:         1000,
			depositCache:               depositCache,
		},
		preGenesisState:         genState,
		chainStartData:          &ethpb.ChainStartData{Chainstarted: true},
		latestEth1Data:          &ethpb.LatestETH1Data{},
		lastReceivedMerkleIndex: -1,
		httpLogger:              logger,
	}
}

func TestDepositContractForBlock(t *testing.T) {
	tests := []struct {
		name         string
		switchBlock  uint64
		blkNum       uint64
		wantAddr     common.Address
		wantBoundary uint64
	}{
		{"below switch uses retired", 100, 99, retiredContract, 100},
		{"exactly at switch uses current", 100, 100, currentContract, 0},
		{"above switch uses current", 100, 5000, currentContract, 0},
		{"genesis block below switch uses retired", 100, 0, retiredContract, 100},
		{"switch disabled always uses current", 0, 0, currentContract, 0},
		{"switch disabled ignores high blocks", 0, 999999, currentContract, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := switchService(&capturingLogger{}, tt.switchBlock)
			addr, boundary := s.depositContractForBlock(tt.blkNum)
			assert.Equal(t, tt.wantAddr, addr, "unexpected contract")
			assert.Equal(t, tt.wantBoundary, boundary, "unexpected boundary")
		})
	}
}

// TestDepositContractForBlock_BoundaryIsAboveBlock pins the invariant depositLogQueries relies on
// when it splits a range at the switch: a reported boundary is always strictly above the block it
// was asked about, so [start, boundary-1] can never invert or underflow. This is the assumption to
// re-examine first if the switch ever grows more than one boundary.
func TestDepositContractForBlock_BoundaryIsAboveBlock(t *testing.T) {
	for _, switchBlock := range []uint64{0, 1, 2, 100, 1_000_000} {
		s := switchService(&capturingLogger{}, switchBlock)
		for _, blkNum := range []uint64{0, 1, 2, 99, 100, 101, 999_999, 1_000_000, 1_000_001} {
			_, boundary := s.depositContractForBlock(blkNum)
			if boundary == 0 {
				continue
			}
			require.Equal(t, true, boundary > blkNum,
				"switchBlock=%d blkNum=%d produced boundary=%d, which would invert the split range",
				switchBlock, blkNum, boundary)
		}
	}
}

func TestProcessETH1Block_SelectsContractForBlock(t *testing.T) {
	tests := []struct {
		name     string
		blkNum   uint64
		wantAddr common.Address
	}{
		{"below switch scans retired", 99, retiredContract},
		{"at switch scans current", 100, currentContract},
		{"above switch scans current", 101, currentContract},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := &capturingLogger{}
			s := switchService(logger, 100)
			require.NoError(t, s.ProcessETH1Block(t.Context(), new(big.Int).SetUint64(tt.blkNum)))
			require.Equal(t, 1, len(logger.queries))
			assert.DeepEqual(t, []common.Address{tt.wantAddr}, logger.queries[0].Addresses)
		})
	}
}

func TestProcessBlockInBatch_SplitsAtSwitchBlock(t *testing.T) {
	const switchBlock = 100

	t.Run("a batch spanning the switch queries each contract over its own range", func(t *testing.T) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		next, _, err := s.processBlockInBatch(t.Context(), 90, 500, 50, 10, 0, map[uint64]*types.HeaderInfo{})
		require.NoError(t, err)
		require.Equal(t, 2, len(logger.queries))

		assert.DeepEqual(t, []common.Address{retiredContract}, logger.queries[0].Addresses)
		assert.Equal(t, uint64(90), logger.queries[0].FromBlock.Uint64())
		assert.Equal(t, uint64(switchBlock-1), logger.queries[0].ToBlock.Uint64())

		assert.DeepEqual(t, []common.Address{currentContract}, logger.queries[1].Addresses)
		assert.Equal(t, uint64(switchBlock), logger.queries[1].FromBlock.Uint64())
		assert.Equal(t, uint64(140), logger.queries[1].ToBlock.Uint64())

		// ToBlock is inclusive, so the two ranges must be adjacent: no block missed at the seam and
		// none scanned twice.
		assert.Equal(t, logger.queries[0].ToBlock.Uint64()+1, logger.queries[1].FromBlock.Uint64())
		assert.Equal(t, uint64(140), next, "the batch is not shortened by the split")
	})

	t.Run("the split survives the extension to the follow height", func(t *testing.T) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		// logCount 0 with lastReceivedMerkleIndex -1 puts the batch within the log request limit,
		// which is the path that extends the range all the way to the follow height.
		next, _, err := s.processBlockInBatch(t.Context(), 90, 200, 500, 10, 0, map[uint64]*types.HeaderInfo{})
		require.NoError(t, err)
		require.Equal(t, 2, len(logger.queries))
		assert.Equal(t, uint64(switchBlock-1), logger.queries[0].ToBlock.Uint64())
		assert.Equal(t, uint64(200), logger.queries[1].ToBlock.Uint64())
		assert.Equal(t, uint64(200), next)
	})

	t.Run("a batch entirely on one side issues a single query", func(t *testing.T) {
		for _, tt := range []struct {
			name     string
			start    uint64
			wantAddr common.Address
		}{
			{"wholly below the switch", 40, retiredContract},
			{"starting at the switch", switchBlock, currentContract},
		} {
			t.Run(tt.name, func(t *testing.T) {
				logger := &capturingLogger{}
				s := switchService(logger, switchBlock)
				_, _, err := s.processBlockInBatch(
					t.Context(), tt.start, 500, 50, 10, 1000, map[uint64]*types.HeaderInfo{})
				require.NoError(t, err)
				require.Equal(t, 1, len(logger.queries))
				assert.DeepEqual(t, []common.Address{tt.wantAddr}, logger.queries[0].Addresses)
			})
		}
	})

	// Starting on the last block below the switch used to be a fixed point: the range was shortened
	// to end where it began, and the caller resumes from the returned block, so the scan spun on one
	// block forever with no error and no log line.
	t.Run("the block immediately before the switch makes progress", func(t *testing.T) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		start := uint64(switchBlock - 1)
		next, _, err := s.processBlockInBatch(t.Context(), start, 500, 50, 10, 1000, map[uint64]*types.HeaderInfo{})
		require.NoError(t, err)
		require.Equal(t, true, next > start, "returned its own input, the scan would spin")
	})

	// The whole scan must terminate, not just each individual batch behave sensibly.
	t.Run("a scan across the switch terminates", func(t *testing.T) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		const followHeight = 300
		current := uint64(90)
		batchSize := uint64(50)
		var err error
		for i := 0; current < followHeight; i++ {
			require.Equal(t, true, i < 100, "loop failed to terminate, stuck at block %d", current)
			prev := current
			current, batchSize, err = s.processBlockInBatch(
				t.Context(), current, followHeight, batchSize, 10, 1000, map[uint64]*types.HeaderInfo{})
			require.NoError(t, err)
			require.Equal(t, true, current > prev, "no progress: block %d returned itself", prev)
		}
	})
}

// TestProcessBlockInBatch_BookmarkIsAlwaysScanned guards the meaning of the returned block. The
// caller records it as LastRequestedBlock and later resumes at LastRequestedBlock+1, so a block
// reported without having been queried has every deposit in it dropped silently, and the next
// deposit then fails the merkle index check for good.
//
// The follow height is the exposure: it climbs one block at a time and so rests on the switch block
// for about one block's time. A scan running in that window ends exactly at the switch, which is
// where an implementation that shortens a batch rather than splitting it stops covering the block it
// claims to have reached. Both cases below therefore put the follow height on the switch block.
func TestProcessBlockInBatch_BookmarkIsAlwaysScanned(t *testing.T) {
	const switchBlock = 100

	queriedBlocks := func(l *capturingLogger) map[uint64]bool {
		seen := map[uint64]bool{}
		for _, q := range l.queries {
			for b := q.FromBlock.Uint64(); b <= q.ToBlock.Uint64(); b++ {
				seen[b] = true
			}
		}
		return seen
	}

	t.Run("follow height on the switch block still scans it", func(t *testing.T) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		const followHeight = switchBlock

		current := uint64(90)
		batchSize := uint64(50)
		var err error
		for i := 0; current < followHeight; i++ {
			require.Equal(t, true, i < 100, "loop failed to terminate, stuck at block %d", current)
			current, batchSize, err = s.processBlockInBatch(
				t.Context(), current, followHeight, batchSize, 10, 0, map[uint64]*types.HeaderInfo{})
			require.NoError(t, err)
		}

		// The walk ended here, so this value is persisted as LastRequestedBlock and the incremental
		// path will resume above it. Every block up to and including it must have been queried.
		seen := queriedBlocks(logger)
		for b := uint64(90); b <= current; b++ {
			require.Equal(t, true, seen[b], "block %d reported as scanned but never queried (bookmark=%d)", b, current)
		}
	})

	t.Run("a batch ending on the switch reports only a queried block", func(t *testing.T) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		next, _, err := s.processBlockInBatch(
			t.Context(), switchBlock-1, switchBlock, 50, 10, 0, map[uint64]*types.HeaderInfo{})
		require.NoError(t, err)
		seen := queriedBlocks(logger)
		require.Equal(t, true, seen[next], "returned block %d was never queried", next)
	})
}

// TestDepositSwitchHint checks the operator-facing half of a switch failure. Without this text the
// only thing surfacing is initPOWService's rate-limited retry message, which blames the execution
// client, so a node stopped by its own switch configuration looks like one that is merely behind.
func TestDepositSwitchHint(t *testing.T) {
	t.Run("names the switch for a block at or above it", func(t *testing.T) {
		s := switchService(&capturingLogger{}, 100)
		for _, blk := range []uint64{100, 5000} {
			hint := s.depositSwitchHint(blk)
			require.NotEqual(t, "", hint, "no hint for block %d", blk)
			assert.StringContains(t, "deposit contract switch block 100", hint)
			assert.StringContains(t, retiredContract.Hex(), hint)
			assert.StringContains(t, currentContract.Hex(), hint)
			assert.StringContains(t, "before the execution client", hint)
		}
	})

	t.Run("stays silent below the switch and when unconfigured", func(t *testing.T) {
		s := switchService(&capturingLogger{}, 100)
		assert.Equal(t, "", s.depositSwitchHint(99))
		assert.Equal(t, "", switchService(&capturingLogger{}, 0).depositSwitchHint(5000))
	})

	t.Run("reaches the errors an operator actually sees", func(t *testing.T) {
		s := switchService(&capturingLogger{}, 100)
		err := s.ProcessLog(t.Context(), &gethTypes.Log{
			Address:     retiredContract,
			BlockNumber: 150,
			Topics:      []common.Hash{depositEventSignature},
		})
		require.NotNil(t, err)
		assert.StringContains(t, "deposit contract switch block 100", err.Error())
	})
}

// TestUnexpectedDepositContractIsIdentifiable pins that the address guard stays distinguishable
// after wrapping. initPOWService matches on the sentinel to replace its default message, which
// blames the execution client, so losing it here silently restores the misleading report.
func TestUnexpectedDepositContractIsIdentifiable(t *testing.T) {
	s := switchService(&capturingLogger{}, 100)
	err := s.ProcessLog(t.Context(), &gethTypes.Log{
		Address:     retiredContract,
		BlockNumber: 150,
		Topics:      []common.Hash{depositEventSignature},
	})
	require.NotNil(t, err)

	// The chain the error actually travels: ProcessLog, then the batch, then the historical scan.
	wrapped := errors.Wrap(errors.Wrap(err, "could not process log"), "processPastLogs")
	require.Equal(t, true, errors.Is(wrapped, errUnexpectedDepositContract),
		"sentinel lost through wrapping, so the retry loop falls back to blaming the execution client")

	// An unrelated failure must not be mistaken for it.
	require.Equal(t, false, errors.Is(errors.New("some execution client failure"), errUnexpectedDepositContract))
}

// TestProcessBlockInBatch_UnknownDepositCount covers the deposit count being unreadable, which
// happens legitimately when a switch is configured before the current contract is deployed. The
// count reaches only a batch-widening branch and nothing that decides which blocks are queried, so
// the scan must behave identically with or without it.
func TestProcessBlockInBatch_UnknownDepositCount(t *testing.T) {
	const (
		switchBlock  = 100
		followHeight = 200
	)

	run := func(t *testing.T, count uint64) (*capturingLogger, uint64) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		next, _, err := s.processBlockInBatch(
			t.Context(), 90, followHeight, 50, 10, count, map[uint64]*types.HeaderInfo{})
		require.NoError(t, err)
		return logger, next
	}

	known, knownNext := run(t, 1000)
	unknown, unknownNext := run(t, unknownDepositCount)

	require.Equal(t, true, unknownNext > 90, "the scan must advance without a deposit count")
	assert.Equal(t, knownNext, unknownNext, "an unreadable count changed how far the batch reached")
	require.Equal(t, len(known.queries), len(unknown.queries))
	for i := range known.queries {
		assert.Equal(t, known.queries[i].FromBlock.Uint64(), unknown.queries[i].FromBlock.Uint64())
		assert.Equal(t, known.queries[i].ToBlock.Uint64(), unknown.queries[i].ToBlock.Uint64())
	}
}

// TestProcessBlockInBatch_NoSwitchIsUnchanged pins the behaviour of a chain that never switched
// deposit contracts, so the switch support cannot regress existing deployments.
func TestProcessBlockInBatch_NoSwitchIsUnchanged(t *testing.T) {
	logger := &capturingLogger{}
	s := switchService(logger, 0)
	_, _, err := s.processBlockInBatch(t.Context(), 90, 500, 50, 10, 1000, map[uint64]*types.HeaderInfo{})
	require.NoError(t, err)
	require.Equal(t, 1, len(logger.queries))
	assert.DeepEqual(t, []common.Address{currentContract}, logger.queries[0].Addresses)
	assert.Equal(t, uint64(90), logger.queries[0].FromBlock.Uint64())
	assert.Equal(t, uint64(140), logger.queries[0].ToBlock.Uint64())
}

func TestProcessLog_RejectsDepositFromUnexpectedContract(t *testing.T) {
	depositLog := func(addr common.Address, blkNum uint64) *gethTypes.Log {
		return &gethTypes.Log{
			Address:     addr,
			BlockNumber: blkNum,
			Topics:      []common.Hash{depositEventSignature},
		}
	}

	t.Run("retired contract emitting after the switch is rejected", func(t *testing.T) {
		s := switchService(&capturingLogger{}, 100)
		err := s.ProcessLog(t.Context(), depositLog(retiredContract, 150))
		require.NotNil(t, err, "expected a deposit from the retired contract to be rejected")
		require.Equal(t, true, errors.Is(err, errUnexpectedDepositContract))
	})

	t.Run("current contract emitting before the switch is rejected", func(t *testing.T) {
		s := switchService(&capturingLogger{}, 100)
		err := s.ProcessLog(t.Context(), depositLog(currentContract, 50))
		require.NotNil(t, err, "expected a deposit from the current contract below the switch to be rejected")
		require.Equal(t, true, errors.Is(err, errUnexpectedDepositContract))
	})

	t.Run("non deposit events are not address checked", func(t *testing.T) {
		s := switchService(&capturingLogger{}, 100)
		other := &gethTypes.Log{
			Address:     retiredContract,
			BlockNumber: 150,
			Topics:      []common.Hash{{'n', 'o', 'p', 'e'}},
		}
		require.NoError(t, s.ProcessLog(t.Context(), other))
	})
}

// TestApplyDepositContractSwitchMigration covers the upgrade path a node takes when a switch is
// configured on a database that was built without one. The scan cursor only moves forward and
// advances whether or not a block held a deposit, so such a cursor can already sit above the switch
// block while the current contract was never read below it. Resuming there drops those deposits
// silently, and the next one then fails the sequential index check for good.
// depositContainer builds the minimum a deposit cache will accept: it indexes containers by public
// key, so Deposit.Data must be populated even when the test only cares about the block height.
func depositContainer(index int64, blockHeight uint64) *ethpb.DepositContainer {
	pubkey := make([]byte, 48)
	pubkey[0] = byte(index + 1)
	return &ethpb.DepositContainer{
		Index:           index,
		Eth1BlockHeight: blockHeight,
		Deposit:         &ethpb.Deposit{Data: &ethpb.Deposit_Data{PublicKey: pubkey}},
	}
}

func TestApplyDepositContractSwitchMigration(t *testing.T) {
	const switchBlock = 100

	newService := func(t *testing.T, switchBlk, cursor uint64) (*Service, db.HeadAccessDatabase) {
		beaconDB := dbtest.SetupDB(t)
		s := switchService(&capturingLogger{}, switchBlk)
		s.cfg.beaconDB = beaconDB
		s.latestEth1Data = &ethpb.LatestETH1Data{LastRequestedBlock: cursor}
		return s, beaconDB
	}

	t.Run("rewinds a cursor that ran past the switch before it was configured", func(t *testing.T) {
		s, beaconDB := newService(t, switchBlock, 5000)
		require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))
		assert.Equal(t, uint64(switchBlock), s.latestEth1Data.LastRequestedBlock,
			"the range between the switch and the cursor would never be scanned")

		applied, found, err := beaconDB.AppliedDepositContractSwitch(t.Context())
		require.NoError(t, err)
		assert.Equal(t, true, found)
		assert.Equal(t, uint64(switchBlock), applied)
	})

	// The marker is what stops the migration running again, so the rewound cursor has to reach disk
	// before it. Leaving it in memory means a restart restores the stale cursor while the marker
	// suppresses the rewind, and the skipped deposits are lost with the one-shot protection spent.
	t.Run("persists the rewound cursor, not just the marker", func(t *testing.T) {
		s, beaconDB := newService(t, switchBlock, 5000)
		require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))

		// This is what a restart restores, so it is what actually has to be correct.
		stored, err := beaconDB.ExecutionChainData(t.Context())
		require.NoError(t, err)
		require.NotNil(t, stored)
		assert.Equal(t, uint64(switchBlock), stored.CurrentEth1Data.LastRequestedBlock,
			"the rewind never reached disk, so a restart would resume from the stale cursor")
	})

	t.Run("a restart after the migration resumes from the switch", func(t *testing.T) {
		s, beaconDB := newService(t, switchBlock, 5000)
		require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))

		// Restart: the cursor comes back from disk and the marker suppresses a second rewind, so the
		// persisted value is the only thing standing between the node and the skipped range.
		stored, err := beaconDB.ExecutionChainData(t.Context())
		require.NoError(t, err)
		restarted := switchService(&capturingLogger{}, switchBlock)
		restarted.cfg.beaconDB = beaconDB
		restarted.latestEth1Data = stored.CurrentEth1Data
		require.NoError(t, restarted.applyDepositContractSwitchMigration(t.Context()))
		assert.Equal(t, uint64(switchBlock), restarted.latestEth1Data.LastRequestedBlock,
			"the restarted node resumed above the switch with the migration already marked applied")
	})

	t.Run("runs once, so a later start does not rescan", func(t *testing.T) {
		s, beaconDB := newService(t, switchBlock, 5000)
		require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))
		require.Equal(t, uint64(switchBlock), s.latestEth1Data.LastRequestedBlock)

		// Simulate the node having scanned onwards, then restarting against the same database.
		s2 := switchService(&capturingLogger{}, switchBlock)
		s2.cfg.beaconDB = beaconDB
		s2.latestEth1Data = &ethpb.LatestETH1Data{LastRequestedBlock: 9000}
		require.NoError(t, s2.applyDepositContractSwitchMigration(t.Context()))
		assert.Equal(t, uint64(9000), s2.latestEth1Data.LastRequestedBlock, "rewound a second time")
	})

	// Rewinding cannot reconcile one boundary with another. Blocks between the two keep the contract
	// attribution they were read under, and for a raised switch those deposits sit below the new
	// value, so the height check above passes them unnoticed. A second switch is not representable in
	// the configuration either, so a mismatch is always a misconfiguration or a correction of one.
	for _, tt := range []struct {
		name         string
		reconfigured uint64
	}{
		{"raised", 3000},
		{"lowered", 50},
	} {
		t.Run("refuses a switch block reconfigured "+tt.name, func(t *testing.T) {
			s, beaconDB := newService(t, switchBlock, 5000)
			require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))

			s2 := switchService(&capturingLogger{}, tt.reconfigured)
			s2.cfg.beaconDB = beaconDB
			s2.latestEth1Data = &ethpb.LatestETH1Data{LastRequestedBlock: 9000}
			err := s2.applyDepositContractSwitchMigration(t.Context())
			require.NotNil(t, err, "silently remigrated to a different switch block")
			assert.StringContains(t, "already migrated for switch block 100", err.Error())
			assert.StringContains(t, "clear-deposit-contract", err.Error())
			assert.Equal(t, uint64(9000), s2.latestEth1Data.LastRequestedBlock, "rewound despite refusing")
		})
	}

	// Without an escape hatch a mistyped switch block would brick the data directory permanently.
	t.Run("clearing the recorded switch lets a new one be applied", func(t *testing.T) {
		s, beaconDB := newService(t, switchBlock, 5000)
		require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))
		require.NoError(t, beaconDB.ClearAppliedDepositContractSwitch(t.Context()))

		s2 := switchService(&capturingLogger{}, 3000)
		s2.cfg.beaconDB = beaconDB
		s2.latestEth1Data = &ethpb.LatestETH1Data{LastRequestedBlock: 9000}
		require.NoError(t, s2.applyDepositContractSwitchMigration(t.Context()))
		assert.Equal(t, uint64(3000), s2.latestEth1Data.LastRequestedBlock)
	})

	t.Run("leaves a cursor below the switch alone", func(t *testing.T) {
		s, _ := newService(t, switchBlock, 40)
		require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))
		assert.Equal(t, uint64(40), s.latestEth1Data.LastRequestedBlock)
	})

	// A deposit recorded at or above the switch can only have come from the retired contract still
	// emitting after the switch. Rewinding cannot repair that: the rescan would find the current
	// contract's deposit at the same index and drop it as already seen, leaving the wrong leaf in the
	// tree permanently. Refusing to start is the only outcome that is not silent divergence.
	for _, height := range []uint64{switchBlock, switchBlock + 1} {
		t.Run(fmt.Sprintf("refuses to start with a stored deposit at block %d", height), func(t *testing.T) {
			s, _ := newService(t, switchBlock, 5000)
			s.cfg.depositCache.InsertDepositContainers(t.Context(), []*ethpb.DepositContainer{
				depositContainer(0, 40),
				depositContainer(1, height),
			})
			err := s.applyDepositContractSwitchMigration(t.Context())
			require.NotNil(t, err, "a deposit at block %d must not be silently rescanned over", height)
			assert.StringContains(t, "resync this node's deposit history", err.Error())
			assert.Equal(t, uint64(5000), s.latestEth1Data.LastRequestedBlock, "rewound despite refusing")
		})
	}

	t.Run("accepts deposits that all sit below the switch", func(t *testing.T) {
		s, _ := newService(t, switchBlock, 5000)
		s.cfg.depositCache.InsertDepositContainers(t.Context(), []*ethpb.DepositContainer{
			depositContainer(0, 10),
			depositContainer(1, switchBlock-1),
		})
		require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))
		assert.Equal(t, uint64(switchBlock), s.latestEth1Data.LastRequestedBlock)
	})

	t.Run("does nothing when no switch is configured", func(t *testing.T) {
		s, beaconDB := newService(t, 0, 5000)
		require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))
		assert.Equal(t, uint64(5000), s.latestEth1Data.LastRequestedBlock)
		_, found, err := beaconDB.AppliedDepositContractSwitch(t.Context())
		require.NoError(t, err)
		assert.Equal(t, false, found, "recorded a migration for a chain that never switched")
	})
}
