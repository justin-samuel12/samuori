package approvals

import (
	"context"
	"database/sql"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// CloseProject expires every pending approval of a project inside the caller's transaction, applying each subject's
// expiry handler, and audits each one (ADR 0058).
func (s *Service) CloseProject(ctx context.Context, tx *sql.Tx, actor authz.Principal, projectID, reason, requestID string) (int, error) {
	rows, err := tx.QueryContext(ctx, s.db.Rebind(`SELECT id FROM approvals WHERE project_id = ? AND state = 'pending'`), projectID)
	if err != nil {
		return 0, err
	}
	var pending []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range pending {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE approvals SET state = 'expired', decided_by = ?, reason = ?, decided_at = ? WHERE id = ? AND state = 'pending'`),
			actor.ID, reason, s.now().UTC(), id); err != nil {
			return 0, err
		}
		a, err := s.load(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		if h := s.handlers[a.SubjectType]; h != nil {
			if err := h(ctx, tx, a); err != nil {
				return 0, err
			}
		}
		if err := audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "approval.decide", ResourceType: a.SubjectType, ResourceID: a.SubjectID, Outcome: Expired, Reason: reason, RequestID: requestID}); err != nil {
			return 0, err
		}
	}
	return len(pending), nil
}
