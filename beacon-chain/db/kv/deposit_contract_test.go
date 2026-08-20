package kv

import (
	"testing"

	"github.com/OffchainLabs/prysm/v7/testing/assert"
	"github.com/OffchainLabs/prysm/v7/testing/require"
	"github.com/ethereum/go-ethereum/common"
)

func TestStore_DepositContract(t *testing.T) {
	db := setupDB(t)
	ctx := t.Context()
	contractAddress := common.Address{1, 2, 3}
	retrieved, err := db.DepositContractAddress(ctx)
	require.NoError(t, err)
	assert.DeepEqual(t, []uint8(nil), retrieved, "Expected nil contract address")
	require.NoError(t, db.SaveDepositContractAddress(ctx, contractAddress))
	retrieved, err = db.DepositContractAddress(ctx)
	require.NoError(t, err)
	assert.Equal(t, contractAddress, common.BytesToAddress(retrieved), "Unexpected address")
	otherAddress := common.Address{4, 5, 6}
	err = db.SaveDepositContractAddress(ctx, otherAddress)
	want := "cannot override deposit contract address"
	assert.ErrorContains(t, want, err, "Should not have been able to override old deposit contract address")
}

func TestStore_ClearDepositContractAddress(t *testing.T) {
	db := setupDB(t)
	ctx := t.Context()

	// Clearing when nothing is stored is a no-op rather than an error, so that the flag can be
	// left set across restarts without breaking a fresh data directory.
	require.NoError(t, db.ClearDepositContractAddress(ctx))

	contractAddress := common.Address{1, 2, 3}
	require.NoError(t, db.SaveDepositContractAddress(ctx, contractAddress))

	require.NoError(t, db.ClearDepositContractAddress(ctx))
	retrieved, err := db.DepositContractAddress(ctx)
	require.NoError(t, err)
	assert.DeepEqual(t, []uint8(nil), retrieved, "Expected the deposit contract address to be cleared")

	// Clearing lifts the write-once restriction, letting a different address be recorded.
	otherAddress := common.Address{4, 5, 6}
	require.NoError(t, db.SaveDepositContractAddress(ctx, otherAddress))
	retrieved, err = db.DepositContractAddress(ctx)
	require.NoError(t, err)
	assert.Equal(t, otherAddress, common.BytesToAddress(retrieved), "Unexpected address after clearing")
}
