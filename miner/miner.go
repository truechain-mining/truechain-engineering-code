// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package miner implements Ethereum block creation and mining.
package miner

import (
	"fmt"
	"math/big"
	"sync/atomic"

	"github.com/truechain/truechain-engineering-code/accounts"
	"github.com/truechain/truechain-engineering-code/common"
	"github.com/truechain/truechain-engineering-code/consensus"
	"github.com/truechain/truechain-engineering-code/core"
	"github.com/truechain/truechain-engineering-code/core/snailchain"
	"github.com/truechain/truechain-engineering-code/core/state"
	"github.com/truechain/truechain-engineering-code/core/types"
	"github.com/truechain/truechain-engineering-code/ethdb"
	"github.com/truechain/truechain-engineering-code/etrue/downloader"
	"github.com/truechain/truechain-engineering-code/event"
	"github.com/truechain/truechain-engineering-code/log"
	"github.com/truechain/truechain-engineering-code/params"
)

// Backend wraps all methods required for mining.

type Backend interface {
	AccountManager() *accounts.Manager
	SnailBlockChain() *snailchain.SnailBlockChain
	BlockChain() *core.BlockChain
	TxPool() *core.TxPool
	SnailPool() *snailchain.SnailPool
	ChainDb() ethdb.Database
	//Election() *etrue.Election
}

//Election module implementation committee interface
type CommitteeElection interface {
	//VerifySigns verify the fast chain committee signatures in batches
	VerifySigns(pvs []*types.PbftSign) ([]*types.CommitteeMember, []error)

	//Get a list of committee members
	//GetCommittee(FastNumber *big.Int, FastHash common.Hash) (*big.Int, []*types.CommitteeMember)
	GetCommittee(fastNumber *big.Int) []*types.CommitteeMember

	SubscribeElectionEvent(ch chan<- types.ElectionEvent) event.Subscription

	IsCommitteeMember(members []*types.CommitteeMember, publickey []byte) bool
}

// Miner creates blocks and searches for proof-of-work values.
type Miner struct {
	mux *event.TypeMux

	worker *worker

	toElect    bool   // for elect
	publickey  []byte // for publickey
	FruitOnly  bool   // only for miner fruit
	singleNode bool   // for single node mode

	coinbase  common.Address
	mining    int32
	truechain Backend
	engine    consensus.Engine
	election  CommitteeElection

	//election
	electionCh  chan types.ElectionEvent
	electionSub event.Subscription

	canStart    int32 // can start indicates whether we can start the mining operation
	shouldStart int32 // should start indicates whether we should start after sync
	commitFlag  int32

}

func New(truechain Backend, config *params.ChainConfig, mux *event.TypeMux, engine consensus.Engine,
	election CommitteeElection, mineFruit bool, singleNode bool) *Miner {
	miner := &Miner{
		truechain:  truechain,
		mux:        mux,
		engine:     engine,
		election:   election,
		FruitOnly:  mineFruit, // set fruit only
		singleNode: singleNode,
		electionCh: make(chan types.ElectionEvent, txChanSize),
		worker:     newWorker(config, engine, common.Address{}, truechain, mux),
		canStart:   1,
		commitFlag: 1,
	}

	miner.Register(NewCpuAgent(truechain.SnailBlockChain(), engine))
	miner.electionSub = miner.election.SubscribeElectionEvent(miner.electionCh)

	go miner.SetFruitOnly(mineFruit)

	// single node not need care about the election
	if !miner.singleNode {
		go miner.loop()
	}

	go miner.update()
	return miner
}

func (self *Miner) loop() {

	defer self.electionSub.Unsubscribe()
	for {
		select {
		case ch := <-self.electionCh:
			switch ch.Option {
			case types.CommitteeStart:
				// alread to start mining need stop

				if self.election.IsCommitteeMember(ch.CommitteeMembers, self.publickey) {
					// i am committee
					if self.Mining() {
						atomic.StoreInt32(&self.commitFlag, 0)
						self.Stop()
					}
					atomic.StoreInt32(&self.commitFlag, 0)
				} else {
					log.Debug("not in commiteer munber so start to miner")
					atomic.StoreInt32(&self.commitFlag, 1)
					self.Start(self.coinbase)

				}
				log.Debug("==================get  election  msg  1 CommitteeStart", "canStart", self.canStart, "shoutstart", self.shouldStart, "mining", self.mining)
			case types.CommitteeStop:

				log.Debug("==================get  election  msg  3 CommitteeStop", "canStart", self.canStart, "shoutstart", self.shouldStart, "mining", self.mining)
				atomic.StoreInt32(&self.commitFlag, 1)
				self.Start(self.coinbase)
			}
		case <-self.electionSub.Err():
			return

		}
	}

}

// update keeps track of the downloader events. Please be aware that this is a one shot type of update loop.
// It's entered once and as soon as `Done` or `Failed` has been broadcasted the events are unregistered and
// the loop is exited. This to prevent a major security vuln where external parties can DOS you with blocks
// and halt your mining operation for as long as the DOS continues.
func (self *Miner) update() {
	//defer self.electionSub.Unsubscribe()
	events := self.mux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{}, types.ElectionEvent{})
out:
	for ev := range events.Chan() {
		switch ev.Data.(type) {
		case downloader.StartEvent:
			log.Info("-----------------get download info startEvent")
			atomic.StoreInt32(&self.canStart, 0)
			if self.Mining() {
				self.Stop()
				atomic.StoreInt32(&self.shouldStart, 1)
				log.Info("Mining aborted due to sync")
			}
		case downloader.DoneEvent, downloader.FailedEvent:
			log.Info("-----------------get download info DoneEvent,FailedEvent")
			shouldStart := atomic.LoadInt32(&self.shouldStart) == 1

			atomic.StoreInt32(&self.canStart, 1)
			atomic.StoreInt32(&self.shouldStart, 0)
			if shouldStart {
				self.Start(self.coinbase)
			}
			// unsubscribe. we're only interested in this event once
			events.Unsubscribe()
			// stop immediately and ignore all further pending events
			break out
		}
	}
}

func (self *Miner) Start(coinbase common.Address) {
	log.Debug("start miner --miner start function")
	atomic.StoreInt32(&self.shouldStart, 1)
	self.SetEtherbase(coinbase)

	if atomic.LoadInt32(&self.canStart) == 0 || atomic.LoadInt32(&self.commitFlag) == 0{
		log.Info("start to miner","canstart",self.canStart,"commitflag",self.commitFlag)
		return
	}
	atomic.StoreInt32(&self.mining, 1)

	self.worker.start()
	self.worker.commitNewWork()
}

func (self *Miner) Stop() {
	log.Debug(" miner   ---stop miner funtion")
	self.worker.stop()
	atomic.StoreInt32(&self.mining, 0)
	atomic.StoreInt32(&self.shouldStart, 0)

}

func (self *Miner) Register(agent Agent) {
	if self.Mining() {
		agent.Start()
	}
	self.worker.register(agent)
}

func (self *Miner) Unregister(agent Agent) {
	self.worker.unregister(agent)
}

func (self *Miner) Mining() bool {
	return atomic.LoadInt32(&self.mining) > 0
}

func (self *Miner) HashRate() (tot int64) {
	if pow, ok := self.engine.(consensus.PoW); ok {
		tot += int64(pow.Hashrate())

	}
	// do we care this might race? is it worth we're rewriting some
	// aspects of the worker/locking up agents so we can get an accurate
	// hashrate?
	for agent := range self.worker.agents {
		if _, ok := agent.(*CpuAgent); !ok {
			tot += agent.GetHashRate()
		}
	}
	return tot
}

func (self *Miner) SetExtra(extra []byte) error {
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("Extra exceeds max length. %d > %v", len(extra), params.MaximumExtraDataSize)
	}
	self.worker.setExtra(extra)
	return nil
}

// Pending returns the currently pending block and associated state.
func (self *Miner) Pending() (*types.Block, *state.StateDB) {
	return self.worker.pending()
}

func (self *Miner) PendingSnail() (*types.SnailBlock, *state.StateDB) {
	return self.worker.pendingSnail()
}

// PendingBlock returns the currently pending block.
//
// Note, to access both the pending block and the pending state
// simultaneously, please use Pending(), as the pending state can
// change between multiple method calls
func (self *Miner) PendingBlock() *types.Block {
	return self.worker.pendingBlock()
}
func (self *Miner) PendingSnailBlock() *types.SnailBlock {
	return self.worker.pendingSnailBlock()
}

func (self *Miner) SetEtherbase(addr common.Address) {
	self.coinbase = addr
	self.worker.setEtherbase(addr)
}

func (self *Miner) SetElection(toElect bool, pubkey []byte) {

	if len(pubkey)<= 0{
		log.Info("Set election failed, pubkey is nil")
		return
	}
	self.toElect = toElect
	self.publickey = make([]byte, len(pubkey))

	copy(self.publickey, pubkey)
	self.worker.setElection(toElect, pubkey)
	log.Info("Set election success")
}

func (self *Miner) SetFruitOnly(FruitOnly bool) {
	self.FruitOnly = FruitOnly
	self.worker.SetFruitOnly(FruitOnly)
}
