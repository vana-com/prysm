package kv

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/OffchainLabs/prysm/v7/monitoring/tracing/trace"
	"github.com/ethereum/go-ethereum/common"
	bolt "go.etcd.io/bbolt"
)

// DepositContractAddress returns contract address is the address of
// the deposit contract on the proof of work chain.
func (s *Store) DepositContractAddress(ctx context.Context) ([]byte, error) {
	_, span := trace.StartSpan(ctx, "BeaconDB.DepositContractAddress")
	defer span.End()
	var addr []byte
	if err := s.db.View(func(tx *bolt.Tx) error {
		chainInfo := tx.Bucket(chainMetadataBucket)
		stored := chainInfo.Get(depositContractAddressKey)
		if len(stored) > 0 {
			addr = slices.Clone(stored)
		}
		return nil
	}); err != nil { // This view never returns an error, but we'll handle anyway for sanity.
		panic(err) // lint:nopanic -- View never returns an error.
	}
	return addr, nil
}

// AppliedDepositContractSwitch returns the deposit contract switch block this database has already
// been migrated for, and whether any migration has been recorded at all.
//
// The deposit log scan cursor only moves forward, so a database whose cursor was built before a
// switch was configured has never read the current contract below that cursor. Recording which
// switch has been applied is what lets that be corrected exactly once instead of on every start.
func (s *Store) AppliedDepositContractSwitch(ctx context.Context) (uint64, bool, error) {
	_, span := trace.StartSpan(ctx, "BeaconDB.AppliedDepositContractSwitch")
	defer span.End()

	var (
		block uint64
		found bool
	)
	if err := s.db.View(func(tx *bolt.Tx) error {
		enc := tx.Bucket(chainMetadataBucket).Get(depositContractSwitchKey)
		if len(enc) != 8 {
			return nil
		}
		block = binary.BigEndian.Uint64(enc)
		found = true
		return nil
	}); err != nil {
		return 0, false, err
	}
	return block, found, nil
}

// SaveAppliedDepositContractSwitch records that the deposit log scan has been migrated for the given
// switch block, so that the migration is not repeated on subsequent starts.
func (s *Store) SaveAppliedDepositContractSwitch(ctx context.Context, block uint64) error {
	_, span := trace.StartSpan(ctx, "BeaconDB.SaveAppliedDepositContractSwitch")
	defer span.End()

	enc := make([]byte, 8)
	binary.BigEndian.PutUint64(enc, block)
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(chainMetadataBucket).Put(depositContractSwitchKey, enc)
	})
}

// ClearDepositContractAddress removes the stored deposit contract address, leaving every other
// bucket untouched. SaveDepositContractAddress is write-once, so clearing the key is how an
// operator intentionally migrating to a new deposit contract lets the node re-record the address
// from its current configuration without wiping the whole database.
func (s *Store) ClearDepositContractAddress(ctx context.Context) error {
	_, span := trace.StartSpan(ctx, "BeaconDB.ClearDepositContractAddress")
	defer span.End()

	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(chainMetadataBucket).Delete(depositContractAddressKey)
	})
}

// SaveDepositContractAddress to the db. It returns an error if an address has been previously saved.
func (s *Store) SaveDepositContractAddress(ctx context.Context, addr common.Address) error {
	_, span := trace.StartSpan(ctx, "BeaconDB.VerifyContractAddress")
	defer span.End()

	return s.db.Update(func(tx *bolt.Tx) error {
		chainInfo := tx.Bucket(chainMetadataBucket)
		expectedAddress := chainInfo.Get(depositContractAddressKey)
		if expectedAddress != nil {
			return fmt.Errorf("cannot override deposit contract address: %v", expectedAddress)
		}
		return chainInfo.Put(depositContractAddressKey, addr.Bytes())
	})
}
