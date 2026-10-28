package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

// ServiceIdentity is a non-human account for automation. It has no password and no provider, so it cannot sign in;
// administrators issue it expiring API tokens, and its roles come from ordinary role bindings.
type ServiceIdentity struct {
	ID, Username, DisplayName string
	Disabled                  bool
	CreatedAt                 time.Time
}

// CreateService adds a service identity with no roles.
func (s *Store) CreateService(ctx context.Context, actor authz.Principal, username, display, requestID string) (ServiceIdentity, error) {
	if !actor.IsOrgAdmin() {
		return ServiceIdentity{}, ErrForbidden
	}
	if !name.MatchString(username) {
		return ServiceIdentity{}, &Invalid{"names are lower-case letters, digits and dashes"}
	}
	if display == "" {
		display = username
	}
	si := ServiceIdentity{ID: ids.New(), Username: username, DisplayName: clipN(display, 128), CreatedAt: s.now().UTC()}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO users (id, org_id, username, display_name, disabled, created_at, kind) VALUES (?, ?, ?, ?, ?, ?, 'service')`),
			si.ID, actor.OrgID, si.Username, si.DisplayName, false, si.CreatedAt); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "service-identity.create", ResourceType: "user", ResourceID: si.ID, Outcome: "succeeded", Reason: username, RequestID: requestID})
	})
	return si, err
}

// Services lists the organization's service identities.
func (s *Store) Services(ctx context.Context, actor authz.Principal) ([]ServiceIdentity, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, username, display_name, disabled, created_at FROM users WHERE org_id = ? AND kind = 'service' ORDER BY username`), actor.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceIdentity{}
	for rows.Next() {
		var si ServiceIdentity
		if err := rows.Scan(&si.ID, &si.Username, &si.DisplayName, &si.Disabled, &si.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// Service returns one service identity of the actor's organization.
func (s *Store) Service(ctx context.Context, actor authz.Principal, id string) (ServiceIdentity, error) {
	if !actor.IsOrgAdmin() {
		return ServiceIdentity{}, ErrForbidden
	}
	var si ServiceIdentity
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id, username, display_name, disabled, created_at FROM users WHERE id = ? AND org_id = ? AND kind = 'service'`), id, actor.OrgID).
		Scan(&si.ID, &si.Username, &si.DisplayName, &si.Disabled, &si.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceIdentity{}, ErrNotFound
	}
	return si, err
}

// SetServiceEnabled disables or re-enables a service identity; a disabled one's tokens stop working at once.
func (s *Store) SetServiceEnabled(ctx context.Context, actor authz.Principal, id string, enabled bool, requestID string) error {
	if _, err := s.Service(ctx, actor, id); err != nil {
		return err
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE users SET disabled = ? WHERE id = ? AND org_id = ? AND kind = 'service'`), !enabled, id, actor.OrgID); err != nil {
			return err
		}
		action := "service-identity.disable"
		if enabled {
			action = "service-identity.enable"
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: action, ResourceType: "user", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}
