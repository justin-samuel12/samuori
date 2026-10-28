package kubeconvert

import (
	"context"
	"database/sql"
	"fmt"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/tenancy"
)

// DeleteRoles may delete any conversion of a project; the developer who created a conversion may delete it too.
var DeleteRoles = []authz.Role{authz.Maintainer, authz.Administrator}

// Rename changes a conversion's display name. Stored revisions keep the names already written into their files; the
// next run or edit uses the new name.
func (s *Service) Rename(ctx context.Context, p authz.Principal, id, name, requestID string) (Conversion, error) {
	c, err := s.load(ctx, p, id, true)
	if err != nil {
		return Conversion{}, err
	}
	if !dnsLabel.MatchString(name) {
		return Conversion{}, invalid("the name %q must be a DNS label (lowercase letters, digits and dashes, at most 63)", name)
	}
	if name == c.Name {
		return c, nil
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tenancy.RequireActive(ctx, s.db, tx, c.ProjectID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE kube_conversions SET name = ?, updated_at = ? WHERE id = ?`), name, s.now().UTC(), id); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "kube-conversion.update", ResourceType: "kube-conversion", ResourceID: id, Outcome: "succeeded",
			Reason: fmt.Sprintf("renamed %s to %s", c.Name, name), RequestID: requestID})
	})
	if err != nil {
		return Conversion{}, err
	}
	return s.Get(ctx, p, id)
}

// Delete removes a conversion with its runs, revisions and stored files; the audit trail keeps what happened. It is
// refused with ErrConflict while a run is queued, assigned or running.
func (s *Service) Delete(ctx context.Context, p authz.Principal, id, requestID string) error {
	c, err := s.load(ctx, p, id, true)
	if err != nil {
		return err
	}
	pr, err := s.project(ctx, p, c.ProjectID, true)
	if err != nil {
		return err
	}
	if c.CreatedBy != p.ID && !authz.Can(p, DeleteRoles, authz.Resource{OrgID: pr.OrgID, TeamID: pr.TeamID, ProjectID: pr.ID}) {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tenancy.RequireActive(ctx, s.db, tx, c.ProjectID); err != nil {
			return err
		}
		if s.db.Dialect == db.Postgres {
			// Blocks a run being queued for this conversion until the delete decides.
			if _, err := tx.ExecContext(ctx, `SELECT id FROM kube_conversions WHERE id = $1 FOR UPDATE`, id); err != nil {
				return err
			}
		}
		var active int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM kube_conversion_runs WHERE conversion_id = ? AND state IN (?, ?, ?)`), id, StateQueued, StateAssigned, StateRunning).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return ErrConflict
		}
		var files int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM kube_conversion_files WHERE conversion_id = ?`), id).Scan(&files); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM kube_conversion_files WHERE conversion_id = ?`,
			`DELETE FROM kube_conversion_revisions WHERE conversion_id = ?`,
			`DELETE FROM kube_conversion_runs WHERE conversion_id = ?`,
			`DELETE FROM kube_conversions WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(q), id); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "kube-conversion.delete", ResourceType: "kube-conversion", ResourceID: id, Outcome: "succeeded",
			Reason: fmt.Sprintf("%s in project %s: %d revisions, %d stored files", c.Name, c.ProjectID, c.CurrentRevision, files), RequestID: requestID})
	})
}
