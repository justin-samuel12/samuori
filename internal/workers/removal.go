package workers

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	workerv1 "samuori.invalid/samuori/internal/workerproto/workerv1"
)

// ActionRemove drains a worker, has its host wipe Samuori, then revokes it (ADR 0045).
const ActionRemove = "remove"

const (
	removalDrainTimeout = 2 * time.Hour
	removalWipeTimeout  = 2 * time.Minute
	wipeProtocolMinor   = 2
)

// Removal is where a worker's removal stands.
type Removal struct {
	State, Outcome, Detail, Reason string
	RequestedAt, UpdatedAt         time.Time
}

// RequestRemoval starts removing a worker: it is drained now, and ProcessRemovals wipes and revokes it.
func (s *Server) RequestRemoval(ctx context.Context, actor authz.Principal, id, reason, requestID string) (Worker, error) {
	if !actor.IsOrgAdmin() {
		return Worker{}, ErrForbidden
	}
	w, err := s.store.Get(ctx, actor.OrgID, id)
	if err != nil {
		return Worker{}, err
	}
	if w.EnrollmentState != Enrolled {
		return Worker{}, ErrConflict
	}
	now := s.store.now().UTC()
	err = s.store.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.store.db.Rebind(`DELETE FROM worker_removals WHERE worker_id = ?`), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.store.db.Rebind(`INSERT INTO worker_removals (worker_id, org_id, reason, requested_by, requested_at, state, updated_at) VALUES (?, ?, ?, ?, ?, 'draining', ?)`),
			id, actor.OrgID, reason, actor.ID, now, now); err != nil {
			return err
		}
		return audit.Append(ctx, s.store.db, tx, audit.Event{Actor: actor, Action: "worker.remove", ResourceType: "worker", ResourceID: id, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Worker{}, err
	}
	return s.ApplyAction(ctx, actor, id, ActionDrain, "removing: "+reason, nil, requestID)
}

// RemovalOf returns a worker's removal, if one was requested.
func (s *Store) RemovalOf(ctx context.Context, workerID string) (Removal, bool) {
	var r Removal
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT state, outcome, detail, reason, requested_at, updated_at FROM worker_removals WHERE worker_id = ?`), workerID).
		Scan(&r.State, &r.Outcome, &r.Detail, &r.Reason, &r.RequestedAt, &r.UpdatedAt)
	return r, err == nil
}

type pendingRemoval struct {
	workerID, orgID, state string
	minor                  int
	updated, requested     time.Time
}

// ProcessRemovals advances every removal; the leader runs it.
func (s *Server) ProcessRemovals(ctx context.Context) error {
	rows, err := s.store.db.SQL.QueryContext(ctx, `SELECT r.worker_id, r.org_id, r.state, w.protocol_minor, r.updated_at, r.requested_at
		FROM worker_removals r JOIN workers w ON w.id = r.worker_id WHERE r.state <> 'removed'`)
	if err != nil {
		return err
	}
	var list []pendingRemoval
	for rows.Next() {
		var p pendingRemoval
		if err := rows.Scan(&p.workerID, &p.orgID, &p.state, &p.minor, &p.updated, &p.requested); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	now := s.store.now()
	for _, p := range list {
		switch p.state {
		case "draining":
			var busy int
			if err := s.store.db.SQL.QueryRowContext(ctx, s.store.db.Rebind(BusySlotsQuery), p.workerID, p.workerID).Scan(&busy); err != nil {
				return err
			}
			if busy > 0 && now.Sub(p.requested) < removalDrainTimeout {
				continue
			}
			if !s.Connected(p.workerID) || p.minor < wipeProtocolMinor {
				detail := "the worker was not connected, so its host was not wiped"
				if s.Connected(p.workerID) {
					detail = "the worker predates remote wipe, so its host was not wiped"
				}
				s.finishRemoval(ctx, p, "not-confirmed", detail)
				continue
			}
			if err := s.setRemoval(ctx, p.workerID, "wiping", "", "asked the host to wipe Samuori"); err != nil {
				return err
			}
			if err := s.Send(p.workerID, &workerv1.ControllerMessage{Kind: &workerv1.ControllerMessage_Remove{Remove: &workerv1.RemoveRequest{Reason: "removed by an administrator"}}}); err != nil {
				_ = s.setRemoval(ctx, p.workerID, "draining", "", "the wipe request did not reach the worker; retrying")
			}
		case "wiping":
			if now.Sub(p.updated) > removalWipeTimeout {
				s.finishRemoval(ctx, p, "not-confirmed", "the worker did not confirm the wipe")
			}
		}
	}
	return nil
}

func (s *Server) setRemoval(ctx context.Context, workerID, state, outcome, detail string) error {
	_, err := s.store.db.SQL.ExecContext(ctx, s.store.db.Rebind(`UPDATE worker_removals SET state = ?, outcome = ?, detail = ?, updated_at = ? WHERE worker_id = ?`),
		state, outcome, detail, s.store.now().UTC(), workerID)
	return err
}

// finishRemoval revokes the worker and records how its host ended up.
func (s *Server) finishRemoval(ctx context.Context, p pendingRemoval, outcome, detail string) {
	actor := authz.Principal{ID: "system:worker-removal", OrgID: p.orgID, Kind: authz.System}
	if _, err := s.ApplyAction(ctx, actor, p.workerID, ActionRevoke, "removed: "+detail, nil, ""); err != nil && !errors.Is(err, ErrConflict) {
		s.log.Error("worker removal: revoke", "worker", p.workerID, "error", err)
		return
	}
	if err := s.setRemoval(ctx, p.workerID, "removed", outcome, detail); err != nil {
		s.log.Error("worker removal: record", "worker", p.workerID, "error", err)
	}
}

// ReportRemoval is the worker confirming it handed the wipe to its host; the worker is revoked straight away.
func (s *Server) ReportRemoval(ctx context.Context, req *workerv1.RemovalReport) (*workerv1.RemovalAck, error) {
	w := ctx.Value(workerKey{}).(Worker)
	var p pendingRemoval
	err := s.store.db.SQL.QueryRowContext(ctx, s.store.db.Rebind(`SELECT worker_id, org_id, state, updated_at, requested_at FROM worker_removals WHERE worker_id = ? AND state = 'wiping'`), w.ID).
		Scan(&p.workerID, &p.orgID, &p.state, &p.updated, &p.requested)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(codes.FailedPrecondition, "no removal is waiting for this worker")
	}
	if err != nil {
		return nil, s.callError("ReportRemoval", err)
	}
	if req.GetStaged() {
		s.finishRemoval(context.WithoutCancel(ctx), p, "wiped", "the host started removing Samuori")
	} else {
		s.finishRemoval(context.WithoutCancel(ctx), p, "not-confirmed", "the host could not start the wipe: "+req.GetDetail())
	}
	return &workerv1.RemovalAck{}, nil
}
