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

package agreement

import (
	"fmt"
	"math"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
)

// pipelinePlayer manages an ensemble of players and implements the actor interface.
// It tracks the current (first uncommitted) agreement round, and manages additional speculative agreement rounds.
type pipelinePlayer struct {
	FirstUncommittedRound   round
	FirstUncommittedVersion protocol.ConsensusVersion
	Players                 map[round]*player
}

func makePipelinePlayer(nextRound round, nextVersion protocol.ConsensusVersion) *pipelinePlayer {
	ret := &pipelinePlayer{
		FirstUncommittedRound:   nextRound,
		FirstUncommittedVersion: nextVersion,
		Players:                 make(map[round]*player),
	}
	// XXX call adjustPlayers
	return ret
}

func (p *pipelinePlayer) T() stateMachineTag { return playerMachine } // XXX different tag?
func (p *pipelinePlayer) underlying() actor  { return p }

func (p *pipelinePlayer) firstUncommittedRound() round {
	return p.FirstUncommittedRound
}

// decode implements serializableActor
func (*pipelinePlayer) decode(buf []byte) (serializableActor, error) {
	ret := pipelinePlayer{}
	err := protocol.DecodeReflect(buf, &ret)
	if err != nil {
		return nil, err
	}
	return &ret, nil
}

// encode implements serializableActor
func (p *pipelinePlayer) encode() []byte {
	return protocol.EncodeReflect(p)
}

// handle an event, usually by delegating to a child player implementation.
func (p *pipelinePlayer) handle(r routerHandle, e event) []action {
	if e.t() == none { // ignore emptyEvent
		return nil
	}

	ee, ok := e.(externalEvent)
	if !ok {
		panic("pipelinePlayer.handle didn't receive externalEvent")
	}

	switch e := e.(type) {
	case messageEvent, timeoutEvent:
		return p.handleRoundEvent(r, ee, ee.ConsensusRound())
	case checkpointEvent:
		// checkpointEvent.ConsensusRound() returns zero
		return p.handleRoundEvent(r, ee, e.Round) // XXX make checkpointAction in pipelinePlayer?
	case roundInterruptionEvent:
		p.FirstUncommittedRound = e.Round
		return p.adjustPlayers(r)
	default:
		panic("bad event")
	}
}

// protoForEvent returns the consensus version of an event, or error
func protoForEvent(e event) (protocol.ConsensusVersion, error) {
	switch e := e.(type) {
	case messageEvent:
		if e.Proto.Err != nil {
			return "", e.Proto.Err
		}
		return e.Proto.Version, nil
	case timeoutEvent:
		if e.Proto.Err != nil {
			return "", e.Proto.Err
		}
		return e.Proto.Version, nil
	case roundInterruptionEvent:
		if e.Proto.Err != nil {
			return "", e.Proto.Err
		}
		return e.Proto.Version, nil
	case thresholdEvent:
		return e.Proto, nil
	default:
		return "", fmt.Errorf("protoForEvent unsupported event")
	}
}

func (p *pipelinePlayer) newPlayerForEvent(e externalEvent, rnd round) (*player, error) {
	switch e := e.(type) {
	// for now, only create new players for messageEvents
	case messageEvent:
		cv, err := protoForEvent(e)
		if err != nil {
			return nil, err
		}
		// XXX check when ConsensusVersionView.Err is set by LedgerReader
		return &player{
			Round:        rnd,
			Step:         soft,
			Deadline:     FilterTimeout(0, cv),
			pipelined:    true,
			roundEnterer: &pipelineRoundEnterer{pp: p},
		}, nil
	default:
		return nil, fmt.Errorf("can't make player for event %+v", e)
	}
}

// handleRoundEvent looks up a player for a given round to handle an event.
func (p *pipelinePlayer) handleRoundEvent(r routerHandle, e externalEvent, rnd round) []action {
	var actions []action

	if rnd.Number < p.FirstUncommittedRound.Number {
		// stale event: give it to the oldest player
		rnd = p.FirstUncommittedRound
	}

	state, ok := p.Players[rnd]
	if !ok {
		// See if we can find the parent player; otherwise, drop.
		for prnd, rp := range p.Players {
			if rnd.Number == prnd.Number+1 {
				re := readLowestEvent{T: readLowestValue, Round: prnd}
				re = r.dispatch(*rp, re, proposalMachineRound, prnd, 0, 0).(readLowestEvent)
				if bookkeeping.BlockHash(re.Proposal.BlockDigest) == rnd.Branch {
					state = rp
					break
				}
			}
		}
		if state == nil {
			logging.Base().Debugf("couldn't find player for rnd %+v, dropping event", rnd)
			return nil
		}
	}

	// TODO move cadaver calls to somewhere cleanerxtern
	r.t.traceInput(state.Round, state.Period, *state, e) // cadaver
	r.t.ainTop(demultiplexer, playerMachine, *state, e, roundZero, 0, 0)

	// pass event to corresponding child player for this round
	a := state.handle(r, e)
	actions = append(actions, a...)

	r.t.aoutTop(demultiplexer, playerMachine, a, roundZero, 0, 0)
	r.t.traceOutput(state.Round, state.Period, *state, a) // cadaver

	return actions
}

// adjustPlayers creates and garbage-collects players as needed
func (p *pipelinePlayer) adjustPlayers(r routerHandle) []action {
	var actions []action
	maxDepth := config.Consensus[p.FirstUncommittedVersion].AgreementPipelineDepth

	// First, GC any players that are no longer relevant.  We also might
	// have players that appear to be mis-speculations (we have a better
	// payload proposal for their parent player), but we don't know yet
	// if that better proposal will be agreed on..
	for rnd := range p.Players {
		if rnd.Number < p.FirstUncommittedRound.Number {
			delete(p.Players, rnd)
		}

		if rnd.Number == p.FirstUncommittedRound.Number && rnd.Branch != p.FirstUncommittedRound.Branch {
			delete(p.Players, rnd)
		}
	}

	// If we don't have a player for the first uncommitted round, create it.
	// This could happen right at startup, or right after the player was GCed
	// because it reached consensus.
	actions = append(actions, p.ensurePlayer(r, p.FirstUncommittedRound, p.FirstUncommittedVersion)...)

	for rnd, rp := range p.Players {
		if rnd.Number >= p.FirstUncommittedRound.Number + basics.Round(maxDepth) {
			continue
		}

		// If some player has moved on beyond period 0, something
		// is not on the fast path, and we should not speculate.
		if rp.Period > 0 {
			// XXX optimization: consider pausing speculation
			// for any "child" rounds of rnd, if already present
			// in p.Players.
			continue
		}

		re := readLowestEvent{T: readLowestPayload, Round: rp.Round, Period: rp.Period}
		re = r.dispatch(*rp, re, proposalMachineRound, rp.Round, rp.Period, 0).(readLowestEvent)
		if !re.PayloadOK {
			continue
		}

		nextrnd := round{Number: rnd.Number+1, Branch: bookkeeping.BlockHash(re.Proposal.BlockDigest)}
		actions = append(actions, p.ensurePlayer(r, nextrnd, re.Payload.prevVersion)...)
	}

	return actions
}

// ensurePlayer creates a player for a particular round, if not already present.
func (p *pipelinePlayer) ensurePlayer(r routerHandle, nextrnd round, ver protocol.ConsensusVersion) []action {
	var actions []action

	_, ok := p.Players[nextrnd]
	if ok {
		return nil
	}

	newPlayer := &player{
		Round:        nextrnd,
		Step:         soft,
		Deadline:     FilterTimeout(0, ver),
		pipelined:    true,
		roundEnterer: &pipelineRoundEnterer{pp: p},
	}

	p.Players[nextrnd] = newPlayer
	actions = append(actions, pseudonodeAction{T: assemble, Round: nextrnd}, rezeroAction{Round: nextrnd})

	e := r.dispatch(*newPlayer, roundInterruptionEvent{Round: nextrnd}, proposalMachine, nextrnd, 0, 0)
	if e.t() == payloadPipelined {
		e := e.(payloadProcessedEvent)
		msg := message{MessageHandle: 0, Tag: protocol.ProposalPayloadTag, UnauthenticatedProposal: e.UnauthenticatedPayload}
		a := verifyPayloadAction(messageEvent{T: payloadPresent, Input: msg}, newPlayer.Round, e.Period, e.Pinned)
		actions = append(actions, a)
	}

	// we might need to handle a pipelined threshold event
	res := r.dispatch(*newPlayer, freshestBundleRequestEvent{}, voteMachineRound, newPlayer.Round, 0, 0)
	freshestRes := res.(freshestBundleEvent) // panic if violate postcondition
	if freshestRes.Ok {
		a4 := newPlayer.handle(r, freshestRes.Event)
		actions = append(actions, a4...)
	}

	return actions
}

// externalDemuxSignals returns a list of per-player signals allowing demux.next to wait for
// multiple pipelined per-round deadlines, as well as the last committed round.
func (p *pipelinePlayer) externalDemuxSignals() pipelineExternalDemuxSignals {
	s := make([]externalDemuxSignals, len(p.Players))
	i := 0
	for _, p := range p.Players {
		s[i] = externalDemuxSignals{
			Deadline:             p.Deadline,
			FastRecoveryDeadline: p.FastRecoveryDeadline,
			CurrentRound:         p.Round,
		}
		i++
	}
	return pipelineExternalDemuxSignals{signals: s, currentRound: p.FirstUncommittedRound.Number}
}

// allPlayersRPS returns a list of per-player (round, period, step) tuples reflecting the current
// state of the pipelinePlayer's child players.
func (p *pipelinePlayer) allPlayersRPS() []RPS {
	ret := make([]RPS, len(p.Players))
	i := 0
	for _, p := range p.Players {
		ret[i] = RPS{Round: p.Round, Period: p.Period, Step: p.Step}
		i++
	}
	return ret
}

type pipelineRoundEnterer struct {
	pp *pipelinePlayer
}

func (re *pipelineRoundEnterer) enter(p *player, r routerHandle, source event, target round) []action {
	// XXX mark player as done:
	// - do not report in demux signals
	// - advance FirstUncommitted if the first guy is done

	// XXX 
	prevRound := p.Round

	a := enterRound(p, r, source, target)
	if p.Round != target {
		panic("enterRound did not transition player to target")
	}

	// confirmed prevRound, player wants to move to target
	// XXX bad
	delete(re.pp.Players, prevRound)

	// update player's entry in map to new round
	re.pp.Players[target] = p

	// XXX check if we are speculating on the same round, different leaf

	// update FirstUncommittedRound
	var minRound round
	minRound.Number = basics.Round(math.MaxUint64)
	for rnd := range re.pp.Players {
		// XXX ensure rnd.Branch matches the hash of the block that just committed
		if rnd.Number < minRound.Number {
			minRound = rnd
		}
	}
	re.pp.FirstUncommittedRound = minRound

	return a
}
