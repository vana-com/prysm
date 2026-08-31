package execution

import (
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
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
