package raft

import (
	"context"
	"fmt"

	hraft "github.com/hashicorp/raft"
)

// awaitReady returns the leadership term of this node once the inner store
// holds every write committed before the transaction that called it, which
// holds txMu. Readiness is the term of the last barrier that succeeded while
// this node led (readyTerm): the first transaction of each term takes a
// barrier, and the later ones of the term take none.
//
//  1. At the start of a term the leader's FSM can lag the committed log. The
//     term's first barrier returns once the FSM applied every entry before it.
//  2. For the rest of the term only this node commits entries. Each
//     transaction holds txMu, and its apply returns once the FSM applied its
//     command, so a later transaction reads every write committed before it.
//     The other proposers of a leader run outside txMu, and write nothing that
//     a transaction reads, so none needs txMu nor keeps the barrier: the
//     EventWriter proposes commands of event operations only, which the FSM
//     applies to the hub, never to the inner store, and no transaction reads
//     the hub; a membership change of Admin is a configuration entry, which
//     the FSM is never given (it is no hashicorp/raft ConfigurationStore);
//     barriers, WaitReady's included, and the no-op that hashicorp/raft
//     appends at the start of a term write nothing. No other code of podium
//     proposes.
//  3. A deposed leader, one that a later term replaced and that has not heard
//     of it yet, still leads in its own view and reads its own, stale state.
//     A write it proposes cannot commit in its old term, so no write computed
//     from that state lands. A transaction that writes nothing proposes
//     nothing, and returns what that state holds.
//  4. A node that loses the leadership of a term never leads that term again:
//     hashicorp/raft makes a node leader only once it raised its term to that
//     of the election it won. So a later term never matches readyTerm, and its
//     first transaction takes a barrier again. The term is recorded only if the
//     node leads both before and after the barrier, and its term did not change
//     across it: a leadership lost and regained meanwhile is in a later term,
//     which the barrier does not cover, and the transaction fails with
//     ErrNotLeader, having proposed nothing.
//
// The state is read before the term. Terms only grow, so a node seen leading,
// and then seen in a term that it was in already before, led that term when it
// was seen leading: readyTerm, for the check that skips the barrier, and the
// term read before the barrier, for the check after it. A barrier that
// succeeded was committed in the term this node led when raft took it, no
// earlier than that term read before, and no later than the one read after.
func (s *Store) awaitReady(ctx context.Context) (uint64, error) {
	leading := s.raft.State() == hraft.Leader
	term := s.raft.CurrentTerm()
	if leading && term == s.readyTerm {
		return term, nil
	}
	// A node that does not lead fails the barrier with ErrNotLeader, or with
	// storage.ErrUnavailable once it shut down (barrierError).
	if err := s.barrier(ctx); err != nil {
		return 0, err
	}
	leadingAfter := s.raft.State() == hraft.Leader
	after := s.raft.CurrentTerm()
	if !leading || !leadingAfter || after != term {
		return 0, fmt.Errorf("raft: WithTx: the leadership changed while the barrier ran: %w", ErrNotLeader)
	}
	s.readyTerm = after
	return after, nil
}

// Barrier's timeout only bounds enqueueing, not Future.Error. Bound the entire
// readiness operation and let Raft finish its future after caller cancellation.
// A barrier writes nothing, so its failure never leaves a write in doubt (see
// barrierError); a node that is not the leader fails with ErrNotLeader. A
// Store takes one at the first transaction of each leadership term
// (awaitReady), and WhenLeader at the start of each term of each runner.
func awaitApplied(ctx context.Context, r *hraft.Raft) error {
	return awaitBarrier(ctx, func() error { return r.Barrier(applyTimeout).Error() })
}

// awaitBarrier is awaitApplied over the barrier operation it waits for.
func awaitBarrier(ctx context.Context, barrier func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()
	if err := awaitOperation(readyCtx, barrier); err != nil {
		return fmt.Errorf("raft: wait for applied state: %w", barrierError(err))
	}
	return nil
}

func awaitOperation(ctx context.Context, operation func() error) error {
	completed := make(chan error, 1)
	go func() { completed <- operation() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-completed:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
}
