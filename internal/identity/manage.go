package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// ErrInUse refuses deleting a provider that has signed someone in; their accounts stay linked to it.
var ErrInUse = errors.New("this provider has signed people in, and their accounts stay linked to it; disable it instead")

// Delete removes a provider that never signed anyone in, with its group mappings and unfinished sign-ins.
func (s *Store) Delete(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var name string
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT name FROM identity_providers WHERE id = ? AND org_id = ?`), id, actor.OrgID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var users int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM users WHERE provider_id = ?`), id).Scan(&users); err != nil {
			return err
		}
		if users > 0 {
			return ErrInUse
		}
		for _, q := range []string{`DELETE FROM identity_group_mappings WHERE provider_id = ?`, `DELETE FROM oidc_logins WHERE provider_id = ?`, `DELETE FROM identity_providers WHERE id = ?`} {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(q), id); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "identity-provider.delete", ResourceType: "identity-provider", ResourceID: id, Outcome: "succeeded", Reason: name, RequestID: requestID})
	})
}

// SetServiceDisplayName relabels a service identity; its username, which pipelines name as owner, stays fixed.
func (s *Store) SetServiceDisplayName(ctx context.Context, actor authz.Principal, id, display, requestID string) (ServiceIdentity, error) {
	if _, err := s.Service(ctx, actor, id); err != nil {
		return ServiceIdentity{}, err
	}
	display = strings.TrimSpace(display)
	if display == "" || len([]rune(display)) > 128 {
		return ServiceIdentity{}, &Invalid{"the display name is 1 to 128 characters"}
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE users SET display_name = ? WHERE id = ? AND org_id = ? AND kind = 'service'`), display, id, actor.OrgID); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "service-identity.update", ResourceType: "user", ResourceID: id, Outcome: "succeeded", Reason: "display name", RequestID: requestID})
	})
	if err != nil {
		return ServiceIdentity{}, err
	}
	return s.Service(ctx, actor, id)
}
