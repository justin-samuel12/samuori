package workers

import (
	"context"
	"database/sql"
	"errors"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

var (
	// ErrUnassigned means the worker belongs to no pool yet, so it has no profile to self-test or build for.
	ErrUnassigned = errors.New("the worker is not assigned to a pool")
	ErrWorkerBusy = errors.New("the worker has builds in progress or is being removed; drain it first")
)

// AssignWorker puts an enrolled worker in a pool, or moves it to another (ADR 0045). Trust never carries over:
// eligibility counts only self-tests recorded for the current pool, and a test still running for the old pool is
// closed as failed. A worker with builds in progress, or being removed, cannot move.
func (s *Store) AssignWorker(ctx context.Context, actor authz.Principal, workerID, poolID, reason, requestID string) (Worker, error) {
	if !actor.IsOrgAdmin() {
		return Worker{}, ErrForbidden
	}
	pool, err := s.Pool(ctx, actor.OrgID, poolID)
	if err != nil {
		return Worker{}, err
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var state string
		var current sql.NullString
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT enrollment_state, pool_id FROM workers WHERE id = ? AND org_id = ?`), workerID, actor.OrgID).Scan(&state, &current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state != Enrolled || current.String == poolID {
			return ErrConflict
		}
		var busy, removing int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT (`+BusySlotsQuery+`), (SELECT COUNT(*) FROM worker_removals WHERE worker_id = ?)`), workerID, workerID, workerID).Scan(&busy, &removing); err != nil {
			return err
		}
		if busy > 0 || removing > 0 {
			return ErrWorkerBusy
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE workers SET pool_id = ? WHERE id = ? AND org_id = ?`), poolID, workerID, actor.OrgID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE pool_self_tests SET status = 'failed', failed_checks = ?, finished_at = ? WHERE worker_id = ? AND status = 'running'`),
			`["reassigned"]`, s.now().UTC(), workerID); err != nil {
			return err
		}
		from := "unassigned"
		if current.Valid {
			from = "pool " + current.String
		}
		why := from + " to pool " + pool.Name
		if reason != "" {
			why += ": " + reason
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "worker.assign", ResourceType: "worker", ResourceID: workerID, Outcome: "succeeded", Reason: why, RequestID: requestID})
	})
	if err != nil {
		return Worker{}, err
	}
	return s.Get(ctx, actor.OrgID, workerID)
}
