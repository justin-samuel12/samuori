package builds

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"time"

	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/validation"
)

// Delegated runs are executed by a federation peer; this installation owns them and waits for the peer (ADR 0037).
const Delegated = "delegated"

// PeerRoutes tells the build service which locations are federation routes.
type PeerRoutes interface {
	Federated(ctx context.Context, orgID, location string) (bool, error)
}

// UsePeerRoutes enables delegation to federation peers.
func (s *Service) UsePeerRoutes(p PeerRoutes) { s.peers = p }

// federatedQueued excludes runs bound for a federation route from local dispatch.
const federatedQueued = `EXISTS (SELECT 1 FROM federation_routes fr JOIN locations l ON l.id = fr.location_id WHERE l.org_id = build_runs.org_id AND l.name = build_runs.preferred_location)`

// QueuedForPeers lists queued runs whose location is a federation route.
func (s *Service) QueuedForPeers(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectRun+` WHERE state = ? AND `+federatedQueued+` ORDER BY priority DESC, created_at LIMIT ?`), Queued, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ErrNotQueued means the run left the queue before it could be delegated.
var ErrNotQueued = errors.New("the run is no longer queued")

// Delegate moves a queued run to delegated within tx and records a stand-in attempt for the peer, so the image the
// peer returns is stored like any other attempt's. It returns the attempt ID.
func (s *Service) Delegate(ctx context.Context, tx *sql.Tx, r Run, peerID string) (string, error) {
	now := s.now().UTC()
	gen := r.Generation + 1
	attemptID := ids.New()
	res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE build_runs SET state = ?, generation = ?, current_attempt_id = ?, lease_worker_id = NULL, lease_deadline = NULL, attempts_used = attempts_used + 1, updated_at = ? WHERE id = ? AND state = ? AND generation = ?`),
		Delegated, gen, attemptID, now, r.ID, Queued, r.Generation)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", ErrNotQueued
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at) VALUES (?, ?, ?, ?, ?, ?, 'delegated', ?, 'not-applicable', ?)`),
		attemptID, r.ID, r.AttemptsUsed+1, gen, "peer:"+peerID, "federation:"+peerID, now.Add(100*365*24*time.Hour), now); err != nil {
		return "", err
	}
	return attemptID, s.event(ctx, tx, r.ID, "state", map[string]any{"state": Delegated, "peer": peerID})
}

// CompleteDelegated stores the image a peer built for a delegated run, verified against the digest the peer reported,
// and starts this installation's own validation with the peer's worker checks. A run that is no longer delegated,
// for example cancelled or abandoned, keeps its state and the image is not recorded.
func (sc *Scheduler) CompleteDelegated(ctx context.Context, runID, attemptID, digest string, size int64, image io.Reader, checks []validation.Check) error {
	r, err := sc.s.load(ctx, sc.s.db.SQL, `id = ?`, runID)
	if err != nil {
		return err
	}
	if r.State != Delegated || r.CurrentAttemptID != attemptID {
		return ErrNotDelegated
	}
	if err := sc.artifacts.Accept(ctx, artifacts.Grant{OrgID: r.OrgID, RunID: r.ID, AttemptID: attemptID, Generation: r.Generation, Kind: artifacts.KindImage, Digest: digest, Size: size}, image); err != nil {
		return err
	}
	now := sc.s.now().UTC()
	err = sc.s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE build_runs SET state = ?, artifact_digest = ?, updated_at = ? WHERE id = ? AND state = ? AND generation = ?`), Succeeded, digest, now, r.ID, Delegated, r.Generation)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotDelegated
		}
		if _, err := tx.ExecContext(ctx, sc.s.db.Rebind(`UPDATE build_attempts SET state = 'completed', artifact_digest = ?, ended_at = ? WHERE id = ?`), digest, now, attemptID); err != nil {
			return err
		}
		if _, err := sc.s.assignVersion(ctx, tx, r.ID, r.ProjectID, now); err != nil {
			return err
		}
		if err := sc.s.event(ctx, tx, r.ID, "state", map[string]string{"state": Succeeded}); err != nil {
			return err
		}
		if sc.validations == nil {
			return nil
		}
		var plan string
		if err := tx.QueryRowContext(ctx, sc.s.db.Rebind(`SELECT plan FROM build_plans WHERE digest = ?`), r.PlanDigest).Scan(&plan); err != nil {
			return err
		}
		vid, err := sc.validations.CreateForBuild(ctx, tx, validation.BuildInput{OrgID: r.OrgID, ProjectID: r.ProjectID, BuildRunID: r.ID, AttemptID: attemptID,
			ArtifactDigest: digest, Plan: []byte(plan), Checks: checks})
		if err != nil {
			return err
		}
		return sc.s.event(ctx, tx, r.ID, "validation", map[string]string{"validationId": vid, "state": "pending"})
	})
	if err == nil {
		sc.s.wake()
	}
	return err
}

// ErrNotDelegated means the run is no longer waiting for its peer.
var ErrNotDelegated = errors.New("the run is no longer delegated")

// FailDelegated ends a delegated run that the peer refused, failed or that an administrator abandoned.
func (s *Service) FailDelegated(ctx context.Context, tx *sql.Tx, runID, class, detail string) error {
	now := s.now().UTC()
	res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE build_runs SET state = ?, failure_class = ?, blocked_reason = ?, updated_at = ? WHERE id = ? AND state = ?`), Failed, class, clip(detail), now, runID, Delegated)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotDelegated
	}
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE build_attempts SET state = 'failed', failure_class = ?, detail = ?, ended_at = ? WHERE run_id = ? AND state = 'delegated'`), class, clip(detail), now, runID); err != nil {
		return err
	}
	return s.event(ctx, tx, runID, "state", map[string]string{"state": Failed, "reason": clip(detail)})
}

// RunByID loads a run without an authorization check, for controller duties.
func (s *Service) RunByID(ctx context.Context, id string) (Run, error) {
	return s.load(ctx, s.db.SQL, `id = ?`, id)
}
