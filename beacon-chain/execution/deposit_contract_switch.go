package execution

import (
	"context"
	"fmt"
	"math/big"

	"github.com/OffchainLabs/prysm/v7/cmd"
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
// rewind happens once. A database already migrated for a different switch block is refused rather
// than migrated again, since rewinding cannot reconcile one boundary with another.
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
	// A database scanned under one switch block cannot be reconciled with another by rewinding.
	// Blocks between the two keep the contract attribution they were read under, and rewinding to a
	// raised switch never revisits them: the deposits the retired contract emitted there are never
	// read, while the current contract's deposits already recorded there sit below the new switch
	// and so pass the height check above unnoticed. The configuration expresses a single retired
	// address and a single switch block, so a second switch is not representable and any mismatch is
	// either a misconfiguration or a correction of one. Both want a person, not a silent remigration.
	if found {
		return errors.Errorf(
			"deposit log scan was already migrated for switch block %d but %d is configured; a database "+
				"scanned under one switch block cannot be corrected by rewinding to another. Restore the "+
				"previous value, or resync this node's deposit history and start once with --%s to clear "+
				"the recorded switch",
			applied, switchBlock, cmd.ClearDepositContract.Name)
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
	rewound := cursor > switchBlock
	if rewound {
		s.latestEth1Data.LastRequestedBlock = switchBlock
	}
	s.latestEth1DataLock.Unlock()

	if rewound {
		log.WithFields(logrus.Fields{
			"previousBlock": cursor,
			"switchBlock":   switchBlock,
		}).Warn("Rewinding deposit log scan to the deposit contract switch block, as the scan " +
			"reached past it before the switch was configured and so never read the current contract there")

		// Persist the rewound cursor before recording the switch as applied, because the marker is
		// what stops this from running again. Nothing else writes the cursor on a useful schedule --
		// processPastLogs only sets it in memory, and savePowchainData is otherwise reached at every
		// thousandth deposit index or at chain start -- so leaving it unsaved means a restart
		// restores the stale cursor while the marker suppresses the rewind that would fix it, and
		// the skipped deposits are lost for good.
		//
		// This order is the safe one: a crash between the two leaves the marker absent, so the
		// migration simply runs again. The reverse order is the bug.
		if err := s.savePowchainData(ctx); err != nil {
			return errors.Wrap(err, "could not persist the rewound deposit log scan cursor")
		}
	}
	return s.cfg.beaconDB.SaveAppliedDepositContractSwitch(ctx, switchBlock)
}

// errUnexpectedDepositContract marks a deposit log rejected for coming from a contract that is not
// authoritative for its block. It is a sentinel rather than an ad-hoc error so that the retry loop in
// initPOWService can tell this apart from the execution-client failures it shares a path with, and
// report it as the configuration problem it is instead of blaming the execution client.
var errUnexpectedDepositContract = errors.New(
	"deposit log from a contract that is not authoritative for its block")

// depositSwitchHint describes the configured switch when blkNum sits at or above it, for appending
// to errors a misconfigured switch is a plausible cause of. It is empty when no switch is configured
// or the block predates it.
//
// Without it these failures reach the operator as the generic retry message from initPOWService,
// which blames the execution client, and a node that stopped following deposits because of its own
// switch configuration looks indistinguishable from one whose execution client is behind.
func (s *Service) depositSwitchHint(blkNum uint64) string {
	if s.cfg.depositContractSwitchBlock == 0 || blkNum < s.cfg.depositContractSwitchBlock {
		return ""
	}
	return fmt.Sprintf(
		". Block %d is at or above the configured deposit contract switch block %d, where deposits "+
			"move from %s to %s, so check that configuration before the execution client",
		blkNum, s.cfg.depositContractSwitchBlock,
		s.cfg.retiredDepositContractAddr.Hex(), s.cfg.depositContractAddr.Hex())
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
