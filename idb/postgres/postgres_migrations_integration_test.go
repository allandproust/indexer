package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	sdk_types "github.com/algorand/go-algorand-sdk/types"

	"github.com/algorand/indexer/idb"
	"github.com/algorand/indexer/idb/postgres/internal/encoding"
	"github.com/algorand/indexer/types"
	"github.com/algorand/indexer/util/test"
)

func nextMigrationNum(t *testing.T, db *IndexerDb) int {
	j, err := db.getMetastate(migrationMetastateKey)
	assert.NoError(t, err)

	assert.True(t, len(j) > 0)

	var state MigrationState
	err = encoding.DecodeJSON([]byte(j), &state)
	assert.NoError(t, err)

	return state.NextMigration
}

func TestFixFreezeLookupMigration(t *testing.T) {
	db, shutdownFunc := setupIdb(t)
	defer shutdownFunc()

	var sender types.Address
	var faddr types.Address

	sender[0] = 0x01
	faddr[0] = 0x02

	///////////
	// Given // A block containing an asset freeze txn has been imported.
	///////////
	freeze, _ := test.MakeAssetFreezeOrPanic(test.Round, 1234, true, sender, faddr)
	importTxns(t, db, test.Round, freeze)

	//////////
	// When // We truncate the txn_participation table and run our migration
	//////////
	db.db.Exec("TRUNCATE txn_participation")
	FixFreezeLookupMigration(db, &MigrationState{NextMigration: 12})

	//////////
	// Then // The sender is still deleted, but the freeze addr should be back.
	//////////
	senderCount := queryInt(db.db, "SELECT COUNT(*) FROM txn_participation WHERE addr = $1", sender[:])
	faddrCount := queryInt(db.db, "SELECT COUNT(*) FROM txn_participation WHERE addr = $1", faddr[:])
	assert.Equal(t, 0, senderCount)
	assert.Equal(t, 1, faddrCount)
}

// Test that ClearAccountDataMigration() clears account data for closed accounts.
func TestClearAccountDataMigrationClosedAccounts(t *testing.T) {
	db, shutdownFunc := setupIdb(t)
	defer shutdownFunc()

	// Rekey account A.
	{
		stxn, txnRow := test.MakePayTxnRowOrPanic(
			test.Round, 0, 0, 0, 0, 0, 0, test.AccountA, test.AccountA, sdk_types.ZeroAddress,
			test.AccountB)

		importTxns(t, db, test.Round, stxn)
		accountTxns(t, db, test.Round, txnRow)
	}

	// Close account A without deleting account data.
	{
		query := "UPDATE account SET deleted = true, closed_at = $1 WHERE addr = $2"
		_, err := db.db.Exec(query, test.Round+1, test.AccountA[:])
		assert.NoError(t, err)
	}

	// Run migration.
	err := ClearAccountDataMigration(db, &MigrationState{})
	assert.NoError(t, err)

	// Check that account A has no account data.
	opts := idb.AccountQueryOptions{
		EqualToAddress: test.AccountA[:],
		IncludeDeleted: true,
	}
	ch, _ := db.GetAccounts(context.Background(), opts)
	accountRow, ok := <-ch
	assert.True(t, ok)
	assert.NoError(t, accountRow.Error)
	account := accountRow.Account
	assert.Nil(t, account.AuthAddr)
}

// Test that ClearAccountDataMigration() clears account data that was set before account was closed.
func TestClearAccountDataMigrationClearsReopenedAccounts(t *testing.T) {
	db, shutdownFunc := setupIdb(t)
	defer shutdownFunc()

	// Create account A.
	{
		stxn, txnRow := test.MakePayTxnRowOrPanic(
			test.Round, 0, 0, 0, 0, 0, 0, test.AccountA, test.AccountA, sdk_types.ZeroAddress,
			sdk_types.ZeroAddress)

		importTxns(t, db, test.Round, stxn)
		accountTxns(t, db, test.Round, txnRow)
	}

	// Make rekey and keyreg transactions for account A.
	{
		stxn0, txnRow0 := test.MakePayTxnRowOrPanic(
			test.Round+1, 0, 0, 0, 0, 0, 0, test.AccountA, test.AccountA, sdk_types.ZeroAddress,
			test.AccountB)
		stxn1, txnRow1 := test.MakeSimpleKeyregOnlineTxn(test.Round+1, test.AccountA)

		importTxns(t, db, test.Round+1, stxn0, stxn1)
		accountTxns(t, db, test.Round+1, txnRow0, txnRow1)
	}

	// Check that account A is online, has auth-addr and participation info.
	opts := idb.AccountQueryOptions{
		EqualToAddress: test.AccountA[:],
	}
	{
		ch, _ := db.GetAccounts(context.Background(), opts)
		accountRow, ok := <-ch
		assert.True(t, ok)
		assert.NoError(t, accountRow.Error)
		account := accountRow.Account
		assert.NotNil(t, account.AuthAddr)
		assert.NotNil(t, account.Participation)
		assert.Equal(t, "Online", account.Status)
	}

	// Simulate closing and reopening of account A.
	{
		query := "UPDATE account SET deleted = false, closed_at = $1 WHERE addr = $2"
		_, err := db.db.Exec(query, test.Round+2, test.AccountA[:])
		assert.NoError(t, err)
	}

	// Run migration.
	err := ClearAccountDataMigration(db, &MigrationState{})
	assert.NoError(t, err)

	// Check that account A is offline and has no account data.
	{
		ch, _ := db.GetAccounts(context.Background(), opts)
		accountRow, ok := <-ch
		assert.True(t, ok)
		assert.NoError(t, accountRow.Error)
		account := accountRow.Account
		assert.Nil(t, account.AuthAddr)
		assert.Nil(t, account.Participation)
		assert.Equal(t, "Offline", account.Status)
	}
}

// Test that ClearAccountDataMigration() does not clear account data because is was updated after
// account was closed.
func TestClearAccountDataMigrationDoesNotClear(t *testing.T) {
	db, shutdownFunc := setupIdb(t)
	defer shutdownFunc()

	// Create account A.
	{
		stxn, txnRow := test.MakePayTxnRowOrPanic(
			test.Round, 0, 0, 0, 0, 0, 0, test.AccountA, test.AccountA, sdk_types.ZeroAddress,
			sdk_types.ZeroAddress)

		importTxns(t, db, test.Round, stxn)
		accountTxns(t, db, test.Round, txnRow)
	}

	// Simulate closing and reopening of account A.
	{
		query := "UPDATE account SET deleted = false, closed_at = $1 WHERE addr = $2"
		_, err := db.db.Exec(query, test.Round+1, test.AccountA[:])
		assert.NoError(t, err)
	}

	// Account A rekey and keyreg.
	{
		stxn0, txnRow0 := test.MakePayTxnRowOrPanic(
			test.Round+2, 0, 0, 0, 0, 0, 0, test.AccountA, test.AccountA, sdk_types.ZeroAddress,
			test.AccountB)
		stxn1, txnRow1 := test.MakeSimpleKeyregOnlineTxn(test.Round+2, test.AccountA)

		importTxns(t, db, test.Round+2, stxn0, stxn1)
		accountTxns(t, db, test.Round+2, txnRow0, txnRow1)
	}

	// A safety check.
	{
		accounts, err := getAccounts(db.db)
		assert.NoError(t, err)
		assert.Equal(t, 1, len(accounts))
	}

	// Run migration.
	err := ClearAccountDataMigration(db, &MigrationState{})
	assert.NoError(t, err)

	// Check that account A is online and has auth addr and keyreg data.
	opts := idb.AccountQueryOptions{
		EqualToAddress: test.AccountA[:],
	}
	ch, _ := db.GetAccounts(context.Background(), opts)
	accountRow, ok := <-ch
	assert.True(t, ok)
	assert.NoError(t, accountRow.Error)
	account := accountRow.Account
	assert.NotNil(t, account.AuthAddr)
	assert.NotNil(t, account.Participation)
	assert.Equal(t, "Online", account.Status)
}

// Test that ClearAccountDataMigration() increments the next migration number.
func TestClearAccountDataMigrationIncMigrationNum(t *testing.T) {
	db, shutdownFunc := setupIdb(t)
	defer shutdownFunc()

	// Run migration.
	state := MigrationState{NextMigration: 13}
	err := ClearAccountDataMigration(db, &state)
	assert.NoError(t, err)

	assert.Equal(t, 14, state.NextMigration)
	newNum := nextMigrationNum(t, db)
	assert.Equal(t, 14, newNum)
}