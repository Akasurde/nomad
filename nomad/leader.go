package nomad

import (
	"fmt"
	"time"

	"github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/raft"
	"github.com/hashicorp/serf/serf"
)

// monitorLeadership is used to monitor if we acquire or lose our role
// as the leader in the Raft cluster. There is some work the leader is
// expected to do, so we must react to changes
func (s *Server) monitorLeadership() {
	leaderCh := s.raft.LeaderCh()
	var stopCh chan struct{}
	for {
		select {
		case isLeader := <-leaderCh:
			if isLeader {
				stopCh = make(chan struct{})
				go s.leaderLoop(stopCh)
				s.logger.Printf("[INFO] nomad: cluster leadership acquired")
			} else if stopCh != nil {
				close(stopCh)
				stopCh = nil
				s.logger.Printf("[INFO] nomad: cluster leadership lost")
			}
		case <-s.shutdownCh:
			return
		}
	}
}

// leaderLoop runs as long as we are the leader to run various
// maintence activities
func (s *Server) leaderLoop(stopCh chan struct{}) {
	// Ensure we revoke leadership on stepdown
	defer s.revokeLeadership()

	var reconcileCh chan serf.Member
	establishedLeader := false

RECONCILE:
	// Setup a reconciliation timer
	reconcileCh = nil
	interval := time.After(s.config.ReconcileInterval)

	// Apply a raft barrier to ensure our FSM is caught up
	start := time.Now()
	barrier := s.raft.Barrier(0)
	if err := barrier.Error(); err != nil {
		s.logger.Printf("[ERR] nomad: failed to wait for barrier: %v", err)
		goto WAIT
	}
	metrics.MeasureSince([]string{"nomad", "leader", "barrier"}, start)

	// Check if we need to handle initial leadership actions
	if !establishedLeader {
		if err := s.establishLeadership(stopCh); err != nil {
			s.logger.Printf("[ERR] nomad: failed to establish leadership: %v",
				err)
			goto WAIT
		}
		establishedLeader = true
	}

	// Reconcile any missing data
	if err := s.reconcile(); err != nil {
		s.logger.Printf("[ERR] nomad: failed to reconcile: %v", err)
		goto WAIT
	}

	// Initial reconcile worked, now we can process the channel
	// updates
	reconcileCh = s.reconcileCh

WAIT:
	// Wait until leadership is lost
	for {
		select {
		case <-stopCh:
			return
		case <-s.shutdownCh:
			return
		case <-interval:
			goto RECONCILE
		case member := <-reconcileCh:
			s.reconcileMember(member)
		}
	}
}

// establishLeadership is invoked once we become leader and are able
// to invoke an initial barrier. The barrier is used to ensure any
// previously inflight transactions have been commited and that our
// state is up-to-date.
func (s *Server) establishLeadership(stopCh chan struct{}) error {
	// If we have multiple workers, disable one to free processing
	// for the plan queue and evaluation broker
	if len(s.workers) > 1 {
		s.workers[0].SetPause(true)
	}

	// Enable the plan queue, since we are now the leader
	s.planQueue.SetEnabled(true)

	// Start the plan evaluator
	go s.planApply()

	// Enable the eval broker, since we are now the leader
	s.evalBroker.SetEnabled(true)

	// Restore the eval broker state
	if err := s.restoreEvalBroker(); err != nil {
		return err
	}

	// Scheduler periodic jobs
	go s.schedulePeriodic(stopCh)

	// Reap any failed evaluations
	go s.reapFailedEvaluations(stopCh)

	// Setup the heartbeat timers. This is done both when starting up or when
	// a leader fail over happens. Since the timers are maintained by the leader
	// node, effectively this means all the timers are renewed at the time of failover.
	// The TTL contract is that the session will not be expired before the TTL,
	// so expiring it later is allowable.
	//
	// This MUST be done after the initial barrier to ensure the latest Nodes
	// are available to be initialized. Otherwise initialization may use stale
	// data.
	if err := s.initializeHeartbeatTimers(); err != nil {
		s.logger.Printf("[ERR] nomad: heartbeat timer setup failed: %v", err)
		return err
	}
	return nil
}

// restoreEvalBroker is used to restore all pending evaluations
// into the eval broker. The broker is maintained only by the leader,
// so it must be restored anytime a leadership transition takes place.
func (s *Server) restoreEvalBroker() error {
	// Get an iterator over every evaluation
	iter, err := s.fsm.State().Evals()
	if err != nil {
		return fmt.Errorf("failed to get evaluations: %v", err)
	}

	for {
		raw := iter.Next()
		if raw == nil {
			break
		}
		eval := raw.(*structs.Evaluation)

		if !eval.ShouldEnqueue() {
			continue
		}

		if err := s.evalBroker.Enqueue(eval); err != nil {
			return fmt.Errorf("failed to enqueue evaluation %s: %v", eval.ID, err)
		}
	}
	return nil
}

// schedulePeriodic is used to do periodic job dispatch while we are leader
func (s *Server) schedulePeriodic(stopCh chan struct{}) {
	evalGC := time.NewTicker(s.config.EvalGCInterval)
	defer evalGC.Stop()

	for {
		select {
		case <-evalGC.C:
			s.evalBroker.Enqueue(s.coreJobEval(structs.CoreJobEvalGC))
		case <-stopCh:
			return
		}
	}
}

// coreJobEval returns an evaluation for a core job
func (s *Server) coreJobEval(job string) *structs.Evaluation {
	return &structs.Evaluation{
		ID:          generateUUID(),
		Priority:    structs.CoreJobPriority,
		Type:        structs.JobTypeCore,
		TriggeredBy: structs.EvalTriggerScheduled,
		JobID:       job,
		Status:      structs.EvalStatusPending,
		ModifyIndex: s.raft.AppliedIndex(),
	}
}

// reapFailedEvaluations is used to reap evaluations that
// have reached their delivery limit and should be failed
func (s *Server) reapFailedEvaluations(stopCh chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		default:
			// Scan for a failed evaluation
			eval, token, err := s.evalBroker.Dequeue([]string{failedQueue}, time.Second)
			if err != nil {
				return
			}
			if eval == nil {
				continue
			}

			// Update the status to failed
			newEval := eval.Copy()
			newEval.Status = structs.EvalStatusFailed
			newEval.StatusDescription = fmt.Sprintf("evaluation reached delivery limit (%d)", s.config.EvalDeliveryLimit)
			s.logger.Printf("[WARN] nomad: eval %#v reached delivery limit, marking as failed", newEval)

			// Update via Raft
			req := structs.EvalUpdateRequest{
				Evals: []*structs.Evaluation{newEval},
			}
			if _, _, err := s.raftApply(structs.EvalUpdateRequestType, &req); err != nil {
				s.logger.Printf("[ERR] nomad: failed to update failed eval %#v: %v", newEval, err)
				continue
			}

			// Ack completion
			s.evalBroker.Ack(eval.ID, token)
		}
	}
}

// revokeLeadership is invoked once we step down as leader.
// This is used to cleanup any state that may be specific to a leader.
func (s *Server) revokeLeadership() error {
	// Disable the plan queue, since we are no longer leader
	s.planQueue.SetEnabled(false)

	// Disable the eval broker, since it is only useful as a leader
	s.evalBroker.SetEnabled(false)

	// Clear the heartbeat timers on either shutdown or step down,
	// since we are no longer responsible for TTL expirations.
	if err := s.clearAllHeartbeatTimers(); err != nil {
		s.logger.Printf("[ERR] nomad: clearing heartbeat timers failed: %v", err)
		return err
	}

	// Unpause our worker if we paused previously
	if len(s.workers) > 1 {
		s.workers[0].SetPause(false)
	}
	return nil
}

// reconcile is used to reconcile the differences between Serf
// membership and what is reflected in our strongly consistent store.
func (s *Server) reconcile() error {
	defer metrics.MeasureSince([]string{"nomad", "leader", "reconcile"}, time.Now())
	members := s.serf.Members()
	for _, member := range members {
		if err := s.reconcileMember(member); err != nil {
			return err
		}
	}
	return nil
}

// reconcileMember is used to do an async reconcile of a single serf member
func (s *Server) reconcileMember(member serf.Member) error {
	// Check if this is a member we should handle
	valid, parts := isNomadServer(member)
	if !valid || parts.Region != s.config.Region {
		return nil
	}
	defer metrics.MeasureSince([]string{"nomad", "leader", "reconcileMember"}, time.Now())

	// Do not reconcile ourself
	if member.Name == fmt.Sprintf("%s.%s", s.config.NodeName, s.config.Region) {
		return nil
	}

	var err error
	switch member.Status {
	case serf.StatusAlive:
		err = s.addRaftPeer(member, parts)
	case serf.StatusLeft, StatusReap:
		err = s.removeRaftPeer(member, parts)
	}
	if err != nil {
		s.logger.Printf("[ERR] nomad: failed to reconcile member: %v: %v",
			member, err)
		return err
	}
	return nil
}

// addRaftPeer is used to add a new Raft peer when a Nomad server joins
func (s *Server) addRaftPeer(m serf.Member, parts *serverParts) error {
	// Check for possibility of multiple bootstrap nodes
	if parts.Bootstrap {
		members := s.serf.Members()
		for _, member := range members {
			valid, p := isNomadServer(member)
			if valid && member.Name != m.Name && p.Bootstrap {
				s.logger.Printf("[ERR] nomad: '%v' and '%v' are both in bootstrap mode. Only one node should be in bootstrap mode, not adding Raft peer.", m.Name, member.Name)
				return nil
			}
		}
	}

	// Attempt to add as a peer
	future := s.raft.AddPeer(parts.Addr.String())
	if err := future.Error(); err != nil && err != raft.ErrKnownPeer {
		s.logger.Printf("[ERR] nomad: failed to add raft peer: %v", err)
		return err
	} else if err == nil {
		s.logger.Printf("[INFO] nomad: added raft peer: %v", parts)
	}
	return nil
}

// removeRaftPeer is used to remove a Raft peer when a Nomad server leaves
// or is reaped
func (s *Server) removeRaftPeer(m serf.Member, parts *serverParts) error {
	// Attempt to remove as peer
	future := s.raft.RemovePeer(parts.Addr.String())
	if err := future.Error(); err != nil && err != raft.ErrUnknownPeer {
		s.logger.Printf("[ERR] nomad: failed to remove raft peer '%v': %v",
			parts, err)
		return err
	} else if err == nil {
		s.logger.Printf("[INFO] nomad: removed server '%s' as peer", m.Name)
	}
	return nil
}
