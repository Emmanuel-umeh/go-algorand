// Copyright (C) 2019-2021 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package data

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/pools"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/verify"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/network"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/execpool"
	"github.com/algorand/go-algorand/util/metrics"
)

// The size txBacklogSize used to determine the size of the backlog that is used to store incoming transaction messages before starting dropping them.
// It should be configured to be higher then the number of CPU cores, so that the execution pool get saturated, but not too high to avoid lockout of the
// execution pool for a long duration of time.
const txBacklogSize = 1000

var transactionMessagesHandled = metrics.MakeCounter(metrics.TransactionMessagesHandled)
var transactionMessagesDroppedFromBacklog = metrics.MakeCounter(metrics.TransactionMessagesDroppedFromBacklog)
var transactionMessagesDroppedFromPool = metrics.MakeCounter(metrics.TransactionMessagesDroppedFromPool)

// The txBacklogMsg structure used to track a single incoming transaction from the gossip network,
type txBacklogMsg struct {
	rawmsg            *network.IncomingMessage // the raw message from the network
	unverifiedTxGroup []transactions.SignedTxn // the unverified ( and signed ) transaction group
	verificationErr   error                    // The verification error generated by the verification function, if any.
}

// TxHandler handles transaction messages
type TxHandler struct {
	txPool                *pools.TransactionPool
	ledger                *Ledger
	genesisID             string
	genesisHash           crypto.Digest
	txVerificationPool    execpool.BacklogPool
	backlogQueue          chan *txBacklogMsg
	postVerificationQueue chan *txBacklogMsg
	backlogWg             sync.WaitGroup
	net                   network.GossipNode
	ctx                   context.Context
	ctxCancel             context.CancelFunc
}

// MakeTxHandler makes a new handler for transaction messages
func MakeTxHandler(txPool *pools.TransactionPool, ledger *Ledger, net network.GossipNode, genesisID string, genesisHash crypto.Digest, executionPool execpool.BacklogPool) *TxHandler {

	if txPool == nil {
		logging.Base().Fatal("MakeTxHandler: txPool is nil on initialization")
		return nil
	}

	if ledger == nil {
		logging.Base().Fatal("MakeTxHandler: ledger is nil on initialization")
		return nil
	}

	handler := &TxHandler{
		txPool:                txPool,
		genesisID:             genesisID,
		genesisHash:           genesisHash,
		ledger:                ledger,
		txVerificationPool:    executionPool,
		backlogQueue:          make(chan *txBacklogMsg, txBacklogSize),
		postVerificationQueue: make(chan *txBacklogMsg, txBacklogSize),
		net:                   net,
	}

	handler.ctx, handler.ctxCancel = context.WithCancel(context.Background())
	return handler
}

// Start enables the processing of incoming messages at the transaction handler
func (handler *TxHandler) Start() {
	handler.net.RegisterHandlers([]network.TaggedMessageHandler{
		{Tag: protocol.TxnTag, MessageHandler: network.HandlerFunc(handler.processIncomingTxn)},
	})
	handler.backlogWg.Add(1)
	go handler.backlogWorker()
}

// Stop suspends the processing of incoming messages at the transaction handler
func (handler *TxHandler) Stop() {
	handler.ctxCancel()
	handler.backlogWg.Wait()
}

func reencode(stxns []transactions.SignedTxn) []byte {
	var result [][]byte
	for _, stxn := range stxns {
		result = append(result, protocol.Encode(&stxn))
	}
	return bytes.Join(result, nil)
}

// backlogWorker is the worker go routine that process the incoming messages from the postVerificationQueue and backlogQueue channels
// and dispatches them further.
func (handler *TxHandler) backlogWorker() {
	defer handler.backlogWg.Done()
	for {
		// prioritize the postVerificationQueue
		select {
		case wi, ok := <-handler.postVerificationQueue:
			if !ok {
				return
			}
			handler.postprocessCheckedTxn(wi)

			// restart the loop so that we could empty out the post verification queue.
			continue
		default:
		}

		// we have no more post verification items. wait for either backlog queue item or post verification item.
		select {
		case wi, ok := <-handler.backlogQueue:
			if !ok {
				return
			}
			if handler.checkAlreadyCommitted(wi) {
				continue
			}

			// enqueue the task to the verification pool.
			handler.txVerificationPool.EnqueueBacklog(handler.ctx, handler.asyncVerifySignature, wi, nil)

		case wi, ok := <-handler.postVerificationQueue:
			if !ok {
				return
			}
			handler.postprocessCheckedTxn(wi)

		case <-handler.ctx.Done():
			return
		}
	}
}

func (handler *TxHandler) postprocessCheckedTxn(wi *txBacklogMsg) {
	if wi.verificationErr != nil {
		// disconnect from peer.
		logging.Base().Warnf("Received a malformed tx group %v: %v", wi.unverifiedTxGroup, wi.verificationErr)
		handler.net.Disconnect(wi.rawmsg.Sender)
		return
	}

	// we've processed this message, so increase the counter.
	transactionMessagesHandled.Inc(nil)

	// at this point, we've verified the transaction, so we can safely treat the transaction as a verified transaction.
	verifiedTxGroup := wi.unverifiedTxGroup

	// save the transaction, if it has high enough fee and not already in the cache
	err := handler.txPool.Remember(transactions.SignedTxGroup{Transactions: verifiedTxGroup})
	if err != nil {
		logging.Base().Debugf("could not remember tx: %v", err)
		return
	}

	// if we remembered without any error ( i.e. txpool wasn't full ), then we should pin these transactions.
	err = handler.ledger.VerifiedTransactionCache().Pin(verifiedTxGroup)
	if err != nil {
		logging.Base().Infof("unable to pin transaction: %v", err)
	}

	// We reencode here instead of using rawmsg.Data to avoid broadcasting non-canonical encodings
	handler.net.Relay(handler.ctx, protocol.TxnTag, reencode(verifiedTxGroup), false, wi.rawmsg.Sender)
}

// asyncVerifySignature verifies that the given transaction group is valid, and update the txBacklogMsg data structure accordingly.
func (handler *TxHandler) asyncVerifySignature(arg interface{}) interface{} {
	tx := arg.(*txBacklogMsg)

	// build the transaction verification context
	latest := handler.ledger.Latest()
	latestHdr, err := handler.ledger.BlockHdr(latest)
	if err != nil {
		tx.verificationErr = fmt.Errorf("Could not get header for previous block %d: %w", latest, err)
		logging.Base().Warnf("Could not get header for previous block %d: %v", latest, err)
	} else {
		// we can't use PaysetGroups here since it's using a execpool like this go-routine and we don't want to deadlock.
		_, tx.verificationErr = verify.TxnGroup(tx.unverifiedTxGroup, latestHdr, handler.ledger.VerifiedTransactionCache())
	}

	select {
	case handler.postVerificationQueue <- tx:
	default:
		// we failed to write to the output queue, since the queue was full.
		// adding the metric here allows us to monitor how frequently it happens.
		transactionMessagesDroppedFromPool.Inc(nil)
	}
	return nil
}

func (handler *TxHandler) processIncomingTxn(rawmsg network.IncomingMessage) network.OutgoingMessage {
	dec := protocol.NewDecoderBytes(rawmsg.Data)
	ntx := 0
	unverifiedTxGroup := make([]transactions.SignedTxn, 1)
	for {
		if len(unverifiedTxGroup) == ntx {
			n := make([]transactions.SignedTxn, len(unverifiedTxGroup)*2)
			copy(n, unverifiedTxGroup)
			unverifiedTxGroup = n
		}

		err := dec.Decode(&unverifiedTxGroup[ntx])
		if err == io.EOF {
			break
		}
		if err != nil {
			logging.Base().Warnf("Received a non-decodable txn: %v", err)
			return network.OutgoingMessage{Action: network.Disconnect}
		}
		ntx++
	}
	if ntx == 0 {
		logging.Base().Warnf("Received empty tx group")
		return network.OutgoingMessage{Action: network.Disconnect}
	}
	unverifiedTxGroup = unverifiedTxGroup[:ntx]

	select {
	case handler.backlogQueue <- &txBacklogMsg{
		rawmsg:            &rawmsg,
		unverifiedTxGroup: unverifiedTxGroup,
	}:
	default:
		// if we failed here we want to increase the corresponding metric. It might suggest that we
		// want to increase the queue size.
		transactionMessagesDroppedFromBacklog.Inc(nil)
	}

	return network.OutgoingMessage{Action: network.Ignore}
}

// checkAlreadyCommitted test to see if the given transaction ( in the txBacklogMsg ) was already committed, and
// whether it would qualify as a candidate for the transaction pool.
//
// Note that this also checks the consistency of the transaction's group hash,
// which is required for safe transaction signature caching behavior.
func (handler *TxHandler) checkAlreadyCommitted(tx *txBacklogMsg) (processingDone bool) {
	// do a quick test to check that this transaction could potentially be committed, to reject dup pending transactions
	err := handler.txPool.Test(tx.unverifiedTxGroup)
	if err != nil {
		logging.Base().Debugf("txPool rejected transaction: %v", err)
		return true
	}
	return false
}

func (handler *TxHandler) processDecoded(unverifiedTxGroup []transactions.SignedTxn) (disconnect bool) {
	tx := &txBacklogMsg{
		unverifiedTxGroup: unverifiedTxGroup,
	}
	if handler.checkAlreadyCommitted(tx) {
		return false
	}

	// build the transaction verification context
	latest := handler.ledger.Latest()
	latestHdr, err := handler.ledger.BlockHdr(latest)
	if err != nil {
		logging.Base().Warnf("Could not get header for previous block %v: %v", latest, err)
		return false
	}

	unverifiedTxnGroups := bookkeeping.SignedTxnsToGroups(unverifiedTxGroup)
	err = verify.PaysetGroups(context.Background(), unverifiedTxnGroups, latestHdr, handler.txVerificationPool, handler.ledger.VerifiedTransactionCache())
	if err != nil {
		// transaction is invalid
		logging.Base().Warnf("One or more transactions were malformed: %v", err)
		return true
	}

	// at this point, we've verified the transaction group,
	// so we can safely treat the transaction as a verified transaction.
	verifiedTxGroup := unverifiedTxGroup

	// save the transaction, if it has high enough fee and not already in the cache
	err = handler.txPool.Remember(transactions.SignedTxGroup{Transactions: verifiedTxGroup})
	if err != nil {
		logging.Base().Debugf("could not remember tx: %v", err)
		return false
	}

	// if we remembered without any error ( i.e. txpool wasn't full ), then we should pin these transactions.
	err = handler.ledger.VerifiedTransactionCache().Pin(verifiedTxGroup)
	if err != nil {
		logging.Base().Warnf("unable to pin transaction: %v", err)
	}

	return false
}

// filterAlreadyCommitted scan the list of signed transaction groups, and filter out the ones that have already been included,
// or that should not be added to the transaction pool.
// the resulting slice is using the *same* underlying array as the input slice, and the caller must ensure that this would not
// cause issue on the caller side. The nonDuplicatedFilteredGroups describe whether any of the removed transacation groups was
// removed for a reason *other* than being duplicate ( for instance, malformed transaction )
func (handler *TxHandler) filterAlreadyCommitted(unverifiedTxGroups []transactions.SignedTxGroup) (filteredGroups []transactions.SignedTxGroup, nonDuplicatedFilteredGroups bool) {
	remainedTxnsGroupOffset := 0
	for idx, utxng := range unverifiedTxGroups {
		err := handler.txPool.Test(utxng.Transactions)
		switch err.(type) {
		case nil:
			// no error was generated.
		case *ledgercore.TransactionInLedgerError:
			// this is a duplicate transaction group.
			continue
		default:
			// some non-duplicate error was reported on this group.
			nonDuplicatedFilteredGroups = true
			continue
		}

		if remainedTxnsGroupOffset != idx {
			unverifiedTxGroups[remainedTxnsGroupOffset] = utxng
		}
		remainedTxnsGroupOffset++
	}
	return unverifiedTxGroups[:remainedTxnsGroupOffset], nonDuplicatedFilteredGroups
}

// processDecodedArray receives a slice of transaction groups and attempt to add them to the transaction pool.
// The processDecodedArray returns whether the node should be disconnecting from the source of these transactions ( in case a malicious transaction is found )
// as well as whether all the provided transactions were included in the transaction pool by the time the method returns.
func (handler *TxHandler) processDecodedArray(unverifiedTxGroups []transactions.SignedTxGroup) (disconnect, allTransactionIncluded bool) {
	var nonDuplicatedFilteredGroups bool
	unverifiedTxGroups, nonDuplicatedFilteredGroups = handler.filterAlreadyCommitted(unverifiedTxGroups)

	if len(unverifiedTxGroups) == 0 {
		return false, !nonDuplicatedFilteredGroups
	}

	// build the transaction verification context
	latest := handler.ledger.Latest()
	latestHdr, err := handler.ledger.BlockHdr(latest)
	if err != nil {
		logging.Base().Warnf("Could not get header for previous block %v: %v", latest, err)
		return false, false
	}

	unverifiedTxnGroups := make([][]transactions.SignedTxn, len(unverifiedTxGroups))
	for i, unverifiedGroup := range unverifiedTxGroups {
		unverifiedTxnGroups[i] = unverifiedGroup.Transactions
	}

	err = verify.PaysetGroups(context.Background(), unverifiedTxnGroups, latestHdr, handler.txVerificationPool, handler.ledger.VerifiedTransactionCache())
	if err != nil {
		// transaction is invalid
		logging.Base().Warnf("One or more transactions were malformed: %v", err)
		return true, false
	}

	// at this point, we've verified the transaction group,
	// so we can safely treat the transaction as a verified transaction.
	verifiedTxGroup := unverifiedTxGroups

	// save the transaction, if it has high enough fee and not already in the cache
	err = handler.txPool.RememberArray(verifiedTxGroup)
	if err != nil {
		logging.Base().Debugf("could not remember tx: %v", err)
		return false, false
	}

	// if we remembered without any error ( i.e. txpool wasn't full ), then we should pin these transactions.
	err = handler.ledger.VerifiedTransactionCache().PinGroups(verifiedTxGroup)
	if err != nil {
		logging.Base().Warnf("unable to pin transaction: %v", err)
	}

	return false, !nonDuplicatedFilteredGroups
}

// SolicitedTxHandler handles messages received through channels other than the gossip network.
// It therefore circumvents the notion of incoming/outgoing messages
type SolicitedTxHandler interface {
	Handle(txgroup []transactions.SignedTxn) error
}

type solicitedTxHandler struct {
	txHandler *TxHandler
}

// SolicitedTxHandler converts a transaction handler to a SolicitedTxHandler
func (handler *TxHandler) SolicitedTxHandler() SolicitedTxHandler {
	return &solicitedTxHandler{txHandler: handler}
}

func (handler *solicitedTxHandler) Handle(txgroup []transactions.SignedTxn) error {
	disconnect := handler.txHandler.processDecoded(txgroup)
	if disconnect {
		return fmt.Errorf("invalid transaction")
	}
	return nil
}

// SolicitedAsyncTxHandler handles messages received through channels other than the gossip network.
// It therefore circumvents the notion of incoming/outgoing messages
type SolicitedAsyncTxHandler interface {
	HandleTransactionGroups(networkPeer interface{}, ackCh chan uint64, messageSeq uint64, groups []transactions.SignedTxGroup)
	Start()
	Stop()
}

type solicitedAyncTxHandler struct {
	txHandler     *TxHandler
	backlogGroups chan txGroups
	stopped       sync.WaitGroup
	stopCtxFunc   context.CancelFunc
}

type txGroups struct {
	// the network package opaque network peer
	networkPeer interface{}
	// the feedback channel, in case we've successfully added the transaction groups to the transaction pool.
	ackCh chan uint64
	// the message sequence number, which would be written back to the feedback channel
	messageSeq uint64
	// the transactions groups slice
	txGroups []transactions.SignedTxGroup
}

// SolicitedAsyncTxHandler converts a transaction handler to a SolicitedTxHandler
func (handler *TxHandler) SolicitedAsyncTxHandler() SolicitedAsyncTxHandler {
	return &solicitedAyncTxHandler{
		txHandler:     handler,
		backlogGroups: make(chan txGroups, txBacklogSize),
	}
}

func (handler *solicitedAyncTxHandler) HandleTransactionGroups(networkPeer interface{}, ackCh chan uint64, messageSeq uint64, groups []transactions.SignedTxGroup) {
	select {
	case handler.backlogGroups <- txGroups{networkPeer: networkPeer, txGroups: groups, ackCh: ackCh, messageSeq: messageSeq}:
	default:
		logging.Base().Warnf("solicitedAyncTxHandler exhusted groups backlog")
	}
}

func (handler *solicitedAyncTxHandler) Start() {
	if handler.stopCtxFunc == nil {
		var ctx context.Context
		ctx, handler.stopCtxFunc = context.WithCancel(context.Background())
		handler.stopped.Add(1)
		go handler.loop(ctx)
	}
}

func (handler *solicitedAyncTxHandler) Stop() {
	if handler.stopCtxFunc != nil {
		handler.stopCtxFunc()
		handler.stopped.Wait()
		handler.stopCtxFunc = nil
	}
}
func (handler *solicitedAyncTxHandler) loop(ctx context.Context) {
	defer handler.stopped.Done()
	var groups txGroups
	for {
		select {
		case <-ctx.Done():
			return
		case groups = <-handler.backlogGroups:
		}
		disconnect, allTransactionsIncluded := handler.txHandler.processDecodedArray(groups.txGroups)
		if disconnect {
			handler.txHandler.net.Disconnect(groups.networkPeer)
			handler.txHandler.net.RequestConnectOutgoing(false, make(chan struct{}))
		} else if allTransactionsIncluded {
			select {
			case groups.ackCh <- groups.messageSeq:
				// all good, write was successful.
			default:
				// unable to write since channel was full - log this:
				logging.Base().Warnf("solicitedAyncTxHandler was unable to ack transaction groups inclusion since the acknowledgement channel was full")
			}
		}
	}
}
