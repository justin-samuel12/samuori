package workers

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// ErrTokenSpent refuses revoking a token that already enrolled a worker or was revoked; revoke the worker instead.
var ErrTokenSpent = errors.New("the token was already used or revoked; revoke the worker it enrolled instead")

// IssuedToken describes an enrollment token without its value, which is never stored.
type IssuedToken struct {
	ID, PoolID, CreatedBy, WorkerID, WorkerName, RevokedBy string
	CreatedAt, ExpiresAt                                   time.Time
	UsedAt, RevokedAt                                      *time.Time
}

// ListTokens returns the organization's newest enrollment tokens.
func (s *Store) ListTokens(ctx context.Context, actor authz.Principal, limit int) ([]IssuedToken, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT t.id, COALESCE(t.pool_id, ''), COALESCE(cu.username, t.created_by), t.created_at, t.expires_at, t.used_at,
		COALESCE(t.worker_id, ''), COALESCE(w.name, ''), t.revoked_at, COALESCE(ru.username, t.revoked_by, '')
		FROM enrollment_tokens t LEFT JOIN users cu ON cu.id = t.created_by LEFT JOIN users ru ON ru.id = t.revoked_by LEFT JOIN workers w ON w.id = t.worker_id
		WHERE t.org_id = ? ORDER BY t.created_at DESC LIMIT ?`), actor.OrgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IssuedToken{}
	for rows.Next() {
		var t IssuedToken
		var used, revoked sql.NullTime
		if err := rows.Scan(&t.ID, &t.PoolID, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &used, &t.WorkerID, &t.WorkerName, &revoked, &t.RevokedBy); err != nil {
			return nil, err
		}
		t.CreatedAt, t.ExpiresAt = t.CreatedAt.UTC(), t.ExpiresAt.UTC()
		if used.Valid {
			u := used.Time.UTC()
			t.UsedAt = &u
		}
		if revoked.Valid {
			r := revoked.Time.UTC()
			t.RevokedAt = &r
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken ends an unused enrollment token before it expires. Enroll checks revocation in the same
// transaction that consumes a token, so a revoked token can never enroll a worker.
func (s *Store) RevokeToken(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE enrollment_tokens SET revoked_at = ?, revoked_by = ? WHERE id = ? AND org_id = ? AND used_at IS NULL AND revoked_at IS NULL`),
			s.now().UTC(), actor.ID, id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			var exists int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM enrollment_tokens WHERE id = ? AND org_id = ?`), id, actor.OrgID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return ErrNotFound
			}
			return ErrTokenSpent
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "enrollment_token.revoke", ResourceType: "enrollment_token", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}
