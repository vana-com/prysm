package execution

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// depositContractForBlock returns the deposit contract that is authoritative for blkNum, along with
// the first block beyond that contract's range (zero when the range extends to the chain head).
//
// A chain that has switched deposit contracts has exactly one authoritative contract per block:
// below the switch block only the retired contract may emit deposits, and at or above it only the
// current one may. Callers use the returned boundary to keep a scanned block range on a single side
// of the switch, so that a range is never queried against the wrong contract.
//
// A non-zero boundary is always strictly above blkNum, because a boundary is only reported for a
// block below the switch. depositLogQueries depends on that to split a range into two non-empty
// halves, so preserve it if this ever grows more than one switch point.
func (s *Service) depositContractForBlock(blkNum uint64) (common.Address, uint64) {
	if s.cfg.depositContractSwitchBlock == 0 || blkNum >= s.cfg.depositContractSwitchBlock {
		return s.cfg.depositContractAddr, 0
	}
	return s.cfg.retiredDepositContractAddr, s.cfg.depositContractSwitchBlock
}

// applyDepositContractSwitchMigration rewinds the deposit log scan to the switch block the first
// time a given switch is configured on this database.
//
// The scan cursor only ever moves forward, and it advances whether or not a block held any deposit.
// A node that synced before the switch was configured therefore has a cursor built while only the
// retired contract was being read, and that cursor may already sit above the switch block. Resuming
// from it would query the current contract from there on and never request the deposits it emitted
// between the switch block and the cursor. Those gaps do not announce themselves: the next deposit
// simply fails the sequential merkle index check and the scan wedges for good.
//
// Rewinding is safe to repeat -- already-known deposits are skipped by index -- but rescanning from
// the switch on every start would grow without bound, so the applied switch is recorded and the
// rewind happens once. Reconfiguring a different switch block records anew and migrates again.
func (s *Service) applyDepositContractSwitchMigration(ctx context.Context) error {
	switchBlock := s.cfg.depositContractSwitchBlock
	if switchBlock == 0 {
		return nil
	}

	applied, found, err := s.cfg.beaconDB.AppliedDepositContractSwitch(ctx)
	if err != nil {
		return errors.Wrap(err, "could not read applied deposit contract switch")
	}
	if found && applied == switchBlock {
		return nil
	}

	// Rewinding can only append: the rescan reads the current contract from the switch block on, and
	// deposits already known are skipped by index. That holds only while every stored deposit sits
	// below the switch. One at or above it can only have come from the retired contract still
	// emitting after the switch, and no rewind repairs that -- the rescan would find the current
	// contract's deposit at the same index and drop it as already seen, leaving the wrong leaf in the
	// tree for good. Refuse to start rather than diverge quietly.
	for _, ctr := range s.cfg.depositCache.AllDepositContainers(ctx) {
		if ctr.Eth1BlockHeight >= switchBlock {
			return errors.Errorf(
				"deposit %d was recorded at block %d, at or above the deposit contract switch block %d, "+
					"so it came from the retired contract after the switch and the deposit tree cannot be "+
					"corrected by rescanning; resync this node's deposit history",
				ctr.Index, ctr.Eth1BlockHeight, switchBlock)
		}
	}

	s.latestEth1DataLock.Lock()
	cursor := s.latestEth1Data.LastRequestedBlock
	if cursor > switchBlock {
		s.latestEth1Data.LastRequestedBlock = switchBlock
	}
	s.latestEth1DataLock.Unlock()

	if cursor > switchBlock {
		log.WithFields(logrus.Fields{
			"previousBlock": cursor,
			"switchBlock":   switchBlock,
		}).Warn("Rewinding deposit log scan to the deposit contract switch block, as the scan " +
			"reached past it before the switch was configured and so never read the current contract there")
	}
	return s.cfg.beaconDB.SaveAppliedDepositContractSwitch(ctx, switchBlock)
}

// depositLogQueries returns the filter queries needed to read every deposit log in [start, end],
// which is inclusive at both ends as eth_getLogs treats it.
//
// A range spanning the deposit contract switch needs one query per contract, since only one contract
// is authoritative on each side of it. Splitting the query rather than shortening the range keeps the
// caller's block accounting honest: it records the last block of the range as scanned and later
// resumes above it, so a range must never be reported as covered unless it was queried.
//
// The two ranges are adjacent and disjoint -- [start, boundary-1] and [boundary, end] -- and ascend,
// so their results concatenate into a correctly ordered log stream with no gap or repeat at the seam.
//
// Both halves are non-empty because depositContractForBlock only reports a boundary above the block
// it was asked about, so boundary-1 is never below start. Were that to change, the first range would
// invert and the execution client would reject the query outright (geth answers fromBlock > toBlock
// with "invalid block range params"), stalling the scan rather than quietly skipping blocks.
func (s *Service) depositLogQueries(start, end uint64) []ethereum.FilterQuery {
	query := func(addr common.Address, from, to uint64) ethereum.FilterQuery {
		return ethereum.FilterQuery{
			Addresses: []common.Address{addr},
			FromBlock: new(big.Int).SetUint64(from),
			ToBlock:   new(big.Int).SetUint64(to),
		}
	}

	addr, boundary := s.depositContractForBlock(start)
	if boundary == 0 || end < boundary {
		return []ethereum.FilterQuery{query(addr, start, end)}
	}
	currentAddr, _ := s.depositContractForBlock(boundary)
	return []ethereum.FilterQuery{
		query(addr, start, boundary-1),
		query(currentAddr, boundary, end),
	}
}
