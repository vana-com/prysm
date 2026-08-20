package execution

import (
	"context"
	"math/big"
	"testing"

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

func TestProcessBlockInBatch_NeverSpansSwitchBlock(t *testing.T) {
	const switchBlock = 100

	t.Run("batch below the switch is clamped to the last retired block", func(t *testing.T) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		// A batch size that would otherwise reach well past the switch.
		next, _, err := s.processBlockInBatch(t.Context(), 90, 500, 50, 10, 0, map[uint64]*types.HeaderInfo{})
		require.NoError(t, err)
		require.Equal(t, 1, len(logger.queries))
		assert.DeepEqual(t, []common.Address{retiredContract}, logger.queries[0].Addresses)
		assert.Equal(t, uint64(90), logger.queries[0].FromBlock.Uint64())
		assert.Equal(t, uint64(switchBlock-1), logger.queries[0].ToBlock.Uint64(), "batch crossed the switch block")
		assert.Equal(t, uint64(switchBlock-1), next, "should resume at the boundary")
	})

	t.Run("clamp still applies when the range is extended to the follow height", func(t *testing.T) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		// logCount 0 with lastReceivedMerkleIndex -1 puts the batch within the log request limit, which
		// is the path that extends ToBlock all the way to the follow height.
		_, _, err := s.processBlockInBatch(t.Context(), 90, 200, 500, 10, 0, map[uint64]*types.HeaderInfo{})
		require.NoError(t, err)
		require.Equal(t, 1, len(logger.queries))
		assert.Equal(t, uint64(switchBlock-1), logger.queries[0].ToBlock.Uint64(), "extension escaped the clamp")
	})

	t.Run("batch starting at the switch uses the current contract and is not clamped", func(t *testing.T) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		_, _, err := s.processBlockInBatch(t.Context(), switchBlock, 500, 50, 10, 1000, map[uint64]*types.HeaderInfo{})
		require.NoError(t, err)
		require.Equal(t, 1, len(logger.queries))
		assert.DeepEqual(t, []common.Address{currentContract}, logger.queries[0].Addresses)
		assert.Equal(t, uint64(150), logger.queries[0].ToBlock.Uint64())
	})

	// Starting exactly on the last block below the switch is the case that can deadlock
	// processPastLogs: the clamp pins end to start, and processBlockInBatch returns end, so without
	// crossing the boundary the caller would loop on the same block forever with no error or log.
	t.Run("the block immediately before the switch advances past it", func(t *testing.T) {
		logger := &capturingLogger{}
		s := switchService(logger, switchBlock)
		start := uint64(switchBlock - 1)
		next, _, err := s.processBlockInBatch(t.Context(), start, 500, 50, 10, 1000, map[uint64]*types.HeaderInfo{})
		require.NoError(t, err)
		assert.Equal(t, uint64(switchBlock-1), logger.queries[0].ToBlock.Uint64(), "must not scan past the switch")
		assert.Equal(t, uint64(switchBlock), next, "must cross the boundary once its last block is scanned")
		require.Equal(t, true, next > start, "processBlockInBatch returned its own input, processPastLogs would spin")
	})

	// The whole scan must terminate, not just each individual batch behave sensibly.
	t.Run("processPastLogs-style loop terminates across the switch", func(t *testing.T) {
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
