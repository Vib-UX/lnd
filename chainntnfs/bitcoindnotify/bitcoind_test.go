//go:build dev
// +build dev

package bitcoindnotify

import (
	"bytes"

	"io/ioutil"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"
)

var (
	testScript = []byte{
		// OP_HASH160
		0xA9,
		// OP_DATA_20
		0x14,
		// <20-byte hash>
		0xec, 0x6f, 0x7a, 0x5a, 0xa8, 0xf2, 0xb1, 0x0c, 0xa5, 0x15,
		0x04, 0x52, 0x3a, 0x60, 0xd4, 0x03, 0x06, 0xf6, 0x96, 0xcd,
		// OP_EQUAL
		0x87,
	}
)

func initHintCache(t *testing.T) *chainntnfs.HeightHintCache {
	t.Helper()

	tempDir, err := ioutil.TempDir("", "kek")
	require.NoError(t, err, "unable to create temp dir")
	db, err := channeldb.Open(tempDir)
	require.NoError(t, err, "unable to create db")
	testCfg := chainntnfs.CacheConfig{
		QueryDisable: false,
	}
	hintCache, err := chainntnfs.NewHeightHintCache(testCfg, db.Backend)
	require.NoError(t, err, "unable to create hint cache")

	return hintCache
}

// setUpNotifier is a helper function to start a new notifier backed by a
// bitcoind driver.
func setUpNotifier(t *testing.T, bitcoindConn *chain.BitcoindConn,
	spendHintCache chainntnfs.SpendHintCache,
	confirmHintCache chainntnfs.ConfirmHintCache,
	blockCache *blockcache.BlockCache) *BitcoindNotifier {

	t.Helper()

	notifier := New(
		bitcoindConn, chainntnfs.NetParams, spendHintCache,
		confirmHintCache, blockCache,
	)
	if err := notifier.Start(); err != nil {
		t.Fatalf("unable to start notifier: %v", err)
	}

	return notifier
}

// syncNotifierWithMiner is a helper method that attempts to wait until the
// notifier is synced (in terms of the chain) with the miner.
func syncNotifierWithMiner(t *testing.T, notifier *BitcoindNotifier,
	miner *rpctest.Harness) uint32 {

	t.Helper()

	_, minerHeight, err := miner.Client.GetBestBlock()
	require.NoError(t, err, "unable to retrieve miner's current height")

	timeout := time.After(10 * time.Second)
	for {
		_, bitcoindHeight, err := notifier.chainConn.GetBestBlock()
		if err != nil {
			t.Fatalf("unable to retrieve bitcoind's current "+
				"height: %v", err)
		}

		if bitcoindHeight == minerHeight {
			return uint32(bitcoindHeight)
		}

		select {
		case <-time.After(100 * time.Millisecond):
		case <-timeout:
			t.Fatalf("timed out waiting to sync notifier")
		}
	}
}

// TestHistoricalConfDetailsTxIndex ensures that we correctly retrieve
// historical confirmation details using the backend node's txindex.
func TestHistoricalConfDetailsTxIndex(t *testing.T) {
	testHistoricalConfDetailsTxIndex(t, true)
	testHistoricalConfDetailsTxIndex(t, false)
}

func testHistoricalConfDetailsTxIndex(t *testing.T, rpcPolling bool) {
	miner, tearDown := chainntnfs.NewMiner(
		t, []string{"--txindex"}, true, 25,
	)
	defer tearDown()

	bitcoindConn, cleanUp := chainntnfs.NewBitcoindBackend(
		t, miner.P2PAddress(), true, rpcPolling,
	)
	defer cleanUp()

	hintCache := initHintCache(t)
	blockCache := blockcache.NewBlockCache(10000)

	notifier := setUpNotifier(
		t, bitcoindConn, hintCache, hintCache, blockCache,
	)
	defer notifier.Stop()

	syncNotifierWithMiner(t, notifier, miner)

	// A transaction unknown to the node should not be found within the
	// txindex even if it is enabled, so we should not proceed with any
	// fallback methods.
	var unknownHash chainhash.Hash
	copy(unknownHash[:], bytes.Repeat([]byte{0x10}, 32))
	unknownConfReq, err := chainntnfs.NewConfRequest(&unknownHash, testScript)
	require.NoError(t, err, "unable to create conf request")
	_, txStatus, err := notifier.historicalConfDetails(unknownConfReq, 0, 0)
	require.NoError(t, err, "unable to retrieve historical conf details")

	switch txStatus {
	case chainntnfs.TxNotFoundIndex:
	case chainntnfs.TxNotFoundManually:
		t.Fatal("should not have proceeded with fallback method, but did")
	default:
		t.Fatal("should not have found non-existent transaction, but did")
	}

	// Now, we'll create a test transaction, confirm it, and attempt to
	// retrieve its confirmation details.
	txid, pkScript, err := chainntnfs.GetTestTxidAndScript(miner)
	require.NoError(t, err, "unable to create tx")
	if err := chainntnfs.WaitForMempoolTx(miner, txid); err != nil {
		t.Fatal(err)
	}
	confReq, err := chainntnfs.NewConfRequest(txid, pkScript)
	require.NoError(t, err, "unable to create conf request")

	// The transaction should be found in the mempool at this point.
	_, txStatus, err = notifier.historicalConfDetails(confReq, 0, 0)
	require.NoError(t, err, "unable to retrieve historical conf details")

	// Since it has yet to be included in a block, it should have been found
	// within the mempool.
	switch txStatus {
	case chainntnfs.TxFoundMempool:
	default:
		t.Fatalf("should have found the transaction within the "+
			"mempool, but did not: %v", txStatus)
	}

	if _, err := miner.Client.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Ensure the notifier and miner are synced to the same height to ensure
	// the txindex includes the transaction just mined.
	syncNotifierWithMiner(t, notifier, miner)

	_, txStatus, err = notifier.historicalConfDetails(confReq, 0, 0)
	require.NoError(t, err, "unable to retrieve historical conf details")

	// Since the backend node's txindex is enabled and the transaction has
	// confirmed, we should be able to retrieve it using the txindex.
	switch txStatus {
	case chainntnfs.TxFoundIndex:
	default:
		t.Fatal("should have found the transaction within the " +
			"txindex, but did not")
	}
}

// TestHistoricalConfDetailsNoTxIndex ensures that we correctly retrieve
// historical confirmation details using the set of fallback methods when the
// backend node's txindex is disabled.
func TestHistoricalConfDetailsNoTxIndex(t *testing.T) {
	testHistoricalConfDetailsNoTxIndex(t, true)
	testHistoricalConfDetailsNoTxIndex(t, false)
}

func testHistoricalConfDetailsNoTxIndex(t *testing.T, rpcpolling bool) {
	miner, tearDown := chainntnfs.NewMiner(t, nil, true, 25)
	defer tearDown()

	bitcoindConn, cleanUp := chainntnfs.NewBitcoindBackend(
		t, miner.P2PAddress(), false, rpcpolling,
	)
	defer cleanUp()

	hintCache := initHintCache(t)
	blockCache := blockcache.NewBlockCache(10000)

	notifier := setUpNotifier(
		t, bitcoindConn, hintCache, hintCache, blockCache,
	)
	defer notifier.Stop()

	// Since the node has its txindex disabled, we fall back to scanning the
	// chain manually. A transaction unknown to the network should not be
	// found.
	var unknownHash chainhash.Hash
	copy(unknownHash[:], bytes.Repeat([]byte{0x10}, 32))
	unknownConfReq, err := chainntnfs.NewConfRequest(&unknownHash, testScript)
	require.NoError(t, err, "unable to create conf request")
	broadcastHeight := syncNotifierWithMiner(t, notifier, miner)
	_, txStatus, err := notifier.historicalConfDetails(
		unknownConfReq, uint32(broadcastHeight), uint32(broadcastHeight),
	)
	require.NoError(t, err, "unable to retrieve historical conf details")

	switch txStatus {
	case chainntnfs.TxNotFoundManually:
	case chainntnfs.TxNotFoundIndex:
		t.Fatal("should have proceeded with fallback method, but did not")
	default:
		t.Fatal("should not have found non-existent transaction, but did")
	}

	// Now, we'll create a test transaction and attempt to retrieve its
	// confirmation details. In order to fall back to manually scanning the
	// chain, the transaction must be in the chain and not contain any
	// unspent outputs. To ensure this, we'll create a transaction with only
	// one output, which we will manually spend. The backend node's
	// transaction index should also be disabled, which we've already
	// ensured above.
	outpoint, output, privKey := chainntnfs.CreateSpendableOutput(t, miner)
	spendTx := chainntnfs.CreateSpendTx(t, outpoint, output, privKey)
	const noOfBlocks = 100
	spendTxHash, err := miner.Client.SendRawTransaction(spendTx, true)
	require.NoError(t, err, "unable to broadcast tx")
	broadcastHeight = syncNotifierWithMiner(t, notifier, miner)
	if err := chainntnfs.WaitForMempoolTx(miner, spendTxHash); err != nil {
		t.Fatalf("tx not relayed to miner: %v", err)
	}
	if _, err := miner.Client.Generate(noOfBlocks); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Ensure the notifier and miner are synced to the same height to ensure
	// we can find the transaction when manually scanning the chain.
	confReq, err := chainntnfs.NewConfRequest(&outpoint.Hash, output.PkScript)
	require.NoError(t, err, "unable to create conf request")
	currentHeight := syncNotifierWithMiner(t, notifier, miner)
	txConfirmation, txStatus, err := notifier.historicalConfDetails(
		confReq, uint32(broadcastHeight), uint32(currentHeight),
	)
	require.NoError(t, err, "unable to retrieve historical conf details")
	blockHash, err := notifier.chainConn.GetBlockHash(int64(broadcastHeight))
	require.NoError(t, err, "unable to get blockHash")

	// Since the backend node's txindex is disabled and the transaction has
	// confirmed, we should be able to find it by falling back to scanning
	// the chain manually.
	switch txStatus {
	case chainntnfs.TxFoundManually:
		require.Equal(
			t, txConfirmation.BlockHash, blockHash, "blockhash mismatch",
		)
		require.Equal(
			t, txConfirmation.BlockHeight, broadcastHeight, "height mismatch",
		)
	default:
		t.Fatal("should have found the transaction by manually " +
			"scanning the chain, but did not")
	}
}

// TestHistoricalSpendDetailsNoTxIndex ensures that we correctly retrieve
// historical spend details using the set of fallback methods when the
// backend node's txindex is disabled.
func TestHistoricalSpendDetailsNoTxIndex(t *testing.T) {
	testHistoricalSpendDetailsNoTxIndex(t, true)
	testHistoricalSpendDetailsNoTxIndex(t, false)
}

func testHistoricalSpendDetailsNoTxIndex(t *testing.T, rpcPolling bool) {
	miner, tearDown := chainntnfs.NewMiner(t, nil, true, 25)
	defer tearDown()

	bitcoindConn, cleanUp := chainntnfs.NewBitcoindBackend(
		t, miner.P2PAddress(), false, rpcPolling,
	)
	defer cleanUp()

	hintCache := initHintCache(t)
	blockCache := blockcache.NewBlockCache(10000)

	notifier := setUpNotifier(
		t, bitcoindConn, hintCache, hintCache, blockCache,
	)
	defer func() {
		err := notifier.Stop()
		require.NoError(t, err)
	}()

	// Since the node has its txindex disabled, we fall back to scanning the
	// chain manually. A outpoint unknown to the network should not be
	// notified.
	var unknownHash chainhash.Hash
	copy(unknownHash[:], bytes.Repeat([]byte{0x10}, 32))
	invalidOutpoint := wire.NewOutPoint(&unknownHash, 0)
	unknownSpendReq, err := chainntnfs.NewSpendRequest(invalidOutpoint, testScript)
	require.NoError(t, err, "unable to create spend request")
	broadcastHeight := syncNotifierWithMiner(t, notifier, miner)
	spendDetails, errSpend := notifier.historicalSpendDetails(
		unknownSpendReq, broadcastHeight, broadcastHeight,
	)
	require.NoError(t, errSpend, "unable to retrieve historical spend details")
	require.Equal(t, spendDetails, (*chainntnfs.SpendDetail)(nil))

	// Now, we'll create a test transaction and attempt to retrieve its
	// spending details. In order to fall back to manually scanning the
	// chain, the transaction must be in the chain and not contain any
	// unspent outputs. To ensure this, we'll create a transaction with only
	// one output, which we will manually spend. The backend node's
	// transaction index should also be disabled, which we've already
	// ensured above.
	outpoint, output, privKey := chainntnfs.CreateSpendableOutput(t, miner)
	spendTx := chainntnfs.CreateSpendTx(t, outpoint, output, privKey)
	const noOfBlocks = 100
	spendTxHash, err := miner.Client.SendRawTransaction(spendTx, true)
	require.NoError(t, err, "unable to broadcast tx")
	broadcastHeight = syncNotifierWithMiner(t, notifier, miner)
	err = chainntnfs.WaitForMempoolTx(miner, spendTxHash)
	require.NoError(t, err, "tx not relayed to miner")
	_, err = miner.Client.Generate(noOfBlocks)
	require.NoError(t, err, "unable to generate blocks")

	// Ensure the notifier and miner are synced to the same height to ensure
	// we can find the transaction spend details when manually scanning the
	// chain.
	spendReq, err := chainntnfs.NewSpendRequest(outpoint, output.PkScript)
	require.NoError(t, err, "unable to create conf request")
	currentHeight := syncNotifierWithMiner(t, notifier, miner)
	validSpendDetails, err := notifier.historicalSpendDetails(
		spendReq, broadcastHeight, currentHeight,
	)
	require.NoError(t, err, "unable to retrieve historical spend details")

	// Since the backend node's txindex is disabled and the transaction has
	// confirmed, we should be able to find it by falling back to scanning
	// the chain manually.
	require.NotNil(t, validSpendDetails)
	require.Equal(t, validSpendDetails.SpentOutPoint, outpoint)
	require.Equal(t, validSpendDetails.SpenderTxHash, spendTxHash)
	require.Equal(t, validSpendDetails.SpendingTx, spendTx)
	require.Equal(t, validSpendDetails.SpendingHeight, int32(broadcastHeight+1))
}
