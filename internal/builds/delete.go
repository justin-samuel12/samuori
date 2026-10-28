package builds

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

var (
	ErrDeleted     = errors.New("this build was deleted")
	ErrInUse       = errors.New("the build's image is in use")
	ErrDeleteRoles = errors.New("only project maintainers and administrators delete builds")
	ErrNotFinished = errors.New("only a finished build can be deleted; cancel it first")
	ErrReason      = errors.New("the reason is at most 1024 characters")
)

// Deletion records who deleted a build and when (ADR 0053).
type Deletion struct {
	By     string
	Reason string
	At     time.Time
}

// Deleted returns the run's deletion, or nil.
func (s *Service) Deleted(ctx context.Context, runID string) (*Deletion, error) {
	var d Deletion
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT deleted_by, reason, deleted_at FROM build_run_deletions WHERE run_id = ?`), runID).Scan(&d.By, &d.Reason, &d.At)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &d, err
}

// Delete removes a finished build's image, logs and cache from storage while keeping the run, its attempts,
// validations and audit trail (ADR 0053). It is refused while a publication, a hold or a validation still uses the
// image. Garbage collection then frees the space, unless another build holds the same content.
func (s *Service) Delete(ctx context.Context, p authz.Principal, roles []authz.Role, id, reason, requestID string) (Deletion, error) {
	r, err := s.Get(ctx, p, roles, id)
	if err != nil {
		return Deletion{}, err
	}
	if !authz.Can(p, []authz.Role{authz.Maintainer, authz.Administrator}, authz.Resource{OrgID: r.OrgID, TeamID: r.TeamID, ProjectID: r.ProjectID}) {
		return Deletion{}, ErrDeleteRoles
	}
	if !Terminal(r.State) {
		return Deletion{}, ErrNotFinished
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 1024 {
		return Deletion{}, ErrReason
	}
	d := Deletion{By: p.ID, Reason: reason, At: s.now().UTC()}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM build_run_deletions WHERE run_id = ?`), id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrDeleted
		}
		for what, q := range map[string]string{
			"a publication":              `SELECT COUNT(*) FROM publications WHERE artifact_id IN (SELECT id FROM build_artifacts WHERE run_id = ?) AND state NOT IN ('failed', 'blocked', 'rejected', 'expired', 'cancelled')`,
			"a legal or audit hold":      `SELECT COUNT(*) FROM artifact_holds WHERE released_at IS NULL AND digest IN (SELECT digest FROM build_artifacts WHERE run_id = ?)`,
			"a validation still running": `SELECT COUNT(*) FROM validation_runs WHERE build_run_id = ? AND state IN ('pending', 'running')`,
		} {
			if err := tx.QueryRowContext(ctx, s.db.Rebind(q), id).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return errors.Join(ErrInUse, errors.New(what+" uses it"))
			}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO build_run_deletions (run_id, org_id, deleted_by, reason, deleted_at) VALUES (?, ?, ?, ?, ?)`), id, r.OrgID, p.ID, reason, d.At); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM cache_entries WHERE run_id = ?`), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM build_live_logs WHERE attempt_id IN (SELECT id FROM build_attempts WHERE run_id = ?)`), id); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "build.delete", ResourceType: "build-run", ResourceID: id, Outcome: "deleted", Reason: reason, RequestID: requestID})
	})
	return d, err
}
