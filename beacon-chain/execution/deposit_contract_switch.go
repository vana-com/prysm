package execution

import "github.com/ethereum/go-ethereum/common"

// depositContractForBlock returns the deposit contract that is authoritative for blkNum, along with
// the first block beyond that contract's range (zero when the range extends to the chain head).
//
// A chain that has switched deposit contracts has exactly one authoritative contract per block:
// below the switch block only the retired contract may emit deposits, and at or above it only the
// current one may. Callers use the returned boundary to keep a scanned block range on a single side
// of the switch, so that a range is never queried against the wrong contract.
func (s *Service) depositContractForBlock(blkNum uint64) (common.Address, uint64) {
	if s.cfg.depositContractSwitchBlock == 0 || blkNum >= s.cfg.depositContractSwitchBlock {
		return s.cfg.depositContractAddr, 0
	}
	return s.cfg.retiredDepositContractAddr, s.cfg.depositContractSwitchBlock
}
