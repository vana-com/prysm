package execution

import (
	"context"
	"math/big"
	"testing"

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
	return &Service{
		cfg: &config{
			depositContractAddr:        currentContract,
			retiredDepositContractAddr: retiredContract,
			depositContractSwitchBlock: switchBlock,
			eth1HeaderReqLimit:         1000,
		},
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
		assert.StringContains(t, "unexpected contract", err.Error())
	})

	t.Run("current contract emitting before the switch is rejected", func(t *testing.T) {
		s := switchService(&capturingLogger{}, 100)
		err := s.ProcessLog(t.Context(), depositLog(currentContract, 50))
		require.NotNil(t, err, "expected a deposit from the current contract below the switch to be rejected")
		assert.StringContains(t, "unexpected contract", err.Error())
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

	t.Run("a different switch block migrates again", func(t *testing.T) {
		s, beaconDB := newService(t, switchBlock, 5000)
		require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))

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

	t.Run("does nothing when no switch is configured", func(t *testing.T) {
		s, beaconDB := newService(t, 0, 5000)
		require.NoError(t, s.applyDepositContractSwitchMigration(t.Context()))
		assert.Equal(t, uint64(5000), s.latestEth1Data.LastRequestedBlock)
		_, found, err := beaconDB.AppliedDepositContractSwitch(t.Context())
		require.NoError(t, err)
		assert.Equal(t, false, found, "recorded a migration for a chain that never switched")
	})
}
