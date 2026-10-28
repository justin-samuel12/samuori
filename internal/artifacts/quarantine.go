package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
)

// Quarantine is a hold on publication of an image built by a worker revoked as compromised (ADR 0026).
type Quarantine struct {
	ID, OrgID, Digest, WorkerID, Reason string
	CreatedAt                           time.Time
	ReleasedAt                          *time.Time
	ReleasedBy, ReleaseReason           string
}

// QuarantineWorker holds every image the worker produced in attempts that started at or after since.
func (s *Store) QuarantineWorker(ctx context.Context, orgID, workerID string, since time.Time, reason string) (int, error) {
	now := time.Now().UTC()
	n := 0
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, s.db.Rebind(`SELECT DISTINCT a.digest FROM build_artifacts a JOIN build_attempts t ON t.id = a.attempt_id
			WHERE a.org_id = ? AND a.kind = 'image' AND t.worker_id = ? AND t.started_at >= ?`), orgID, workerID, since.UTC())
		if err != nil {
			return err
		}
		var digests []string
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				rows.Close()
				return err
			}
			digests = append(digests, d)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, d := range digests {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO artifact_quarantines (id, org_id, digest, worker_id, reason, created_at) VALUES (?, ?, ?, ?, ?, ?)`),
				ids.New(), orgID, d, workerID, reason, now); err != nil {
				return err
			}
			if err := audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{OrgID: orgID, Kind: authz.System}, Action: "artifact.quarantine", ResourceType: "artifact", ResourceID: d, Outcome: "succeeded", Reason: "built by revoked worker " + workerID}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// Quarantined returns the active quarantine of an image digest, if any.
func (s *Store) Quarantined(ctx context.Context, orgID, digest string) (Quarantine, bool, error) {
	q, err := s.scanQuarantine(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectQuarantine+` WHERE org_id = ? AND digest = ? AND released_at IS NULL ORDER BY created_at LIMIT 1`), orgID, digest))
	if errors.Is(err, sql.ErrNoRows) {
		return Quarantine{}, false, nil
	}
	return q, err == nil, err
}

// Quarantines lists an organization's quarantines, newest first.
func (s *Store) Quarantines(ctx context.Context, orgID string, active bool) ([]Quarantine, error) {
	q := selectQuarantine + ` WHERE org_id = ?`
	if active {
		q += ` AND released_at IS NULL`
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(q+` ORDER BY created_at DESC LIMIT 500`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Quarantine
	for rows.Next() {
		q, err := s.scanQuarantine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ReleaseQuarantine lets an administrator or security reviewer clear a quarantine after review; it is audited.
func (s *Store) ReleaseQuarantine(ctx context.Context, actor authz.Principal, id, reason, requestID string) (Quarantine, error) {
	if !authz.Can(actor, []authz.Role{authz.Administrator, authz.SecurityReviewer}, authz.Resource{OrgID: actor.OrgID}) {
		return Quarantine{}, ErrForbidden
	}
	now := time.Now().UTC()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE artifact_quarantines SET released_at = ?, released_by = ?, release_reason = ? WHERE id = ? AND org_id = ? AND released_at IS NULL`), now, actor.ID, reason, id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "artifact.quarantine.release", ResourceType: "artifact_quarantine", ResourceID: id, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Quarantine{}, err
	}
	return s.scanQuarantine(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectQuarantine+` WHERE id = ?`), id))
}

const selectQuarantine = `SELECT id, org_id, digest, worker_id, reason, created_at, released_at, released_by, release_reason FROM artifact_quarantines`

func (s *Store) scanQuarantine(r interface{ Scan(...any) error }) (Quarantine, error) {
	var q Quarantine
	var released sql.NullTime
	if err := r.Scan(&q.ID, &q.OrgID, &q.Digest, &q.WorkerID, &q.Reason, &q.CreatedAt, &released, &q.ReleasedBy, &q.ReleaseReason); err != nil {
		return Quarantine{}, err
	}
	if released.Valid {
		q.ReleasedAt = &released.Time
	}
	return q, nil
}
