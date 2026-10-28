package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
)

// InUse refuses deleting an object that something still refers to; What lists those references.
type InUse struct{ What []string }

func (e *InUse) Error() string { return "still in use: " + strings.Join(e.What, "; ") }

var (
	ErrInvalidName        = errors.New("team names are lower-case letters, digits and dashes, starting and ending with a letter or digit, at most 63 characters")
	ErrSelf               = errors.New("you cannot disable your own account")
	ErrInvalidDisplayName = errors.New("the display name is 1 to 128 characters")
	ErrFromMapping        = errors.New("this role comes from an identity provider group mapping; change the mapping instead")
	ErrInvalidRole        = errors.New("unknown role")
	ErrNotLocal           = errors.New("this is not a user account")
)

var teamName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

var roles = []authz.Role{authz.Developer, authz.Maintainer, authz.Publisher, authz.Operator, authz.SecurityReviewer, authz.Administrator}

// RenameTeam changes a team's name under the same rules as creation; administrators only.
func (s *Store) RenameTeam(ctx context.Context, actor authz.Principal, id, name, requestID string) (Team, error) {
	if !actor.IsOrgAdmin() {
		return Team{}, ErrForbidden
	}
	if !teamName.MatchString(name) {
		return Team{}, ErrInvalidName
	}
	var t Team
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT id, org_id, name, created_at FROM teams WHERE id = ? AND org_id = ?`), id, actor.OrgID).Scan(&t.ID, &t.OrgID, &t.Name, &t.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		old := t.Name
		if old == name {
			return nil
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE teams SET name = ? WHERE id = ?`), name, id); err != nil {
			return conflict(err)
		}
		t.Name = name
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "team.rename", ResourceType: "team", ResourceID: id, Outcome: "succeeded", Reason: old + " -> " + name, RequestID: requestID})
	})
	return t, err
}

// DeleteTeam removes a team nothing refers to. Projects in any state count, because purged projects keep their
// records (ADR 0058), as do role bindings, group mappings, notification rules and image sources.
func (s *Store) DeleteTeam(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		lock := `SELECT name FROM teams WHERE id = ? AND org_id = ?`
		if s.db.Dialect == db.Postgres {
			lock += ` FOR UPDATE`
		}
		var name string
		err := tx.QueryRowContext(ctx, s.db.Rebind(lock), id, actor.OrgID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		refs := []struct{ label, q string }{
			{"project", `SELECT COUNT(*) FROM projects WHERE team_id = ?`},
			{"role binding", `SELECT COUNT(*) FROM role_bindings WHERE scope_type = 'team' AND scope_id = ?`},
			{"identity provider group mapping", `SELECT COUNT(*) FROM identity_group_mappings WHERE scope_type = 'team' AND scope_id = ?`},
			{"notification rule", `SELECT COUNT(*) FROM notification_rules WHERE team_id = ?`},
		}
		var what []string
		for _, ref := range refs {
			var n int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(ref.q), id).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				what = append(what, plural(n, ref.label))
			}
		}
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM image_sources WHERE org_id = ? AND team_ids LIKE ?`), actor.OrgID, `%"`+id+`"%`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			what = append(what, plural(n, "image source"))
		}
		if len(what) > 0 {
			return &InUse{What: what}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM teams WHERE id = ?`), id); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "team.delete", ResourceType: "team", ResourceID: id, Outcome: "succeeded", Reason: name, RequestID: requestID})
	})
}

func plural(n int, label string) string {
	if n == 1 {
		return "1 " + label
	}
	return fmt.Sprintf("%d %ss", n, label)
}

// SetUserDisplayName changes a person's display name. Administrators change anyone's; a user changes only their own.
func (s *Store) SetUserDisplayName(ctx context.Context, actor authz.Principal, userID, display, requestID string) error {
	if actor.ID != userID && !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	display = strings.TrimSpace(display)
	if display == "" || len([]rune(display)) > 128 {
		return ErrInvalidDisplayName
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE users SET display_name = ? WHERE id = ? AND org_id = ? AND kind = 'user'`), display, userID, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "user.update", ResourceType: "user", ResourceID: userID, Outcome: "succeeded", Reason: "display name", RequestID: requestID})
	})
}

// SetUserEnabled disables or re-enables a person (ADR 0065). Disabling revokes the user's sessions and API tokens in
// the same transaction; it refuses the caller's own account, the last enabled organization administrator and the
// last enabled local administrator.
func (s *Store) SetUserEnabled(ctx context.Context, actor authz.Principal, userID string, enabled bool, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	if !enabled && userID == actor.ID {
		return ErrSelf
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockOrg(ctx, tx, actor.OrgID); err != nil {
			return err
		}
		var local, admin bool
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT u.provider_id IS NULL,
			EXISTS (SELECT 1 FROM role_bindings rb WHERE rb.principal_id = u.id AND rb.org_id = u.org_id AND rb.role = 'administrator' AND rb.scope_type = 'organization')
			FROM users u WHERE u.id = ? AND u.org_id = ? AND u.kind = 'user'`), userID, actor.OrgID).Scan(&local, &admin)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		action := "user.enable"
		if !enabled {
			action = "user.disable"
			if admin {
				if err := s.keepAdmins(ctx, tx, actor.OrgID, userID, local); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`), now, userID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE api_tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`), now, userID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE users SET disabled = ? WHERE id = ?`), !enabled, userID); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: action, ResourceType: "user", ResourceID: userID, Outcome: "succeeded", RequestID: requestID})
	})
}

// ChangeRole replaces the role of an existing binding, keeping its scope, in one audited step. Bindings from group
// mappings are refused because the next sign-in replaces them.
func (s *Store) ChangeRole(ctx context.Context, actor authz.Principal, bindingID string, role authz.Role, requestID string) (RoleBinding, error) {
	if !actor.IsOrgAdmin() {
		return RoleBinding{}, ErrForbidden
	}
	if !slices.Contains(roles, role) {
		return RoleBinding{}, ErrInvalidRole
	}
	var rb RoleBinding
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockOrg(ctx, tx, actor.OrgID); err != nil {
			return err
		}
		var old, scope, principalID string
		var local, mapped bool
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT rb.role, rb.scope_type, rb.scope_id, rb.principal_id, u.provider_id IS NULL AND u.kind = 'user', rb.source_provider_id IS NOT NULL
			FROM role_bindings rb JOIN users u ON u.id = rb.principal_id WHERE rb.id = ? AND rb.org_id = ?`), bindingID, actor.OrgID).
			Scan(&old, &scope, &rb.ScopeID, &principalID, &local, &mapped)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rb.ID, rb.Role, rb.ScopeType = bindingID, role, authz.ScopeType(scope)
		if mapped {
			return ErrFromMapping
		}
		if old == string(role) {
			return nil
		}
		if old == string(authz.Administrator) && scope == string(authz.ScopeOrganization) {
			if err := s.keepAdmins(ctx, tx, actor.OrgID, principalID, local); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE role_bindings SET role = ? WHERE id = ?`), string(role), bindingID); err != nil {
			return conflict(err)
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "role.change", ResourceType: "role_binding", ResourceID: bindingID, Outcome: "succeeded",
			Reason: old + "@" + scope + " -> " + string(role) + "@" + scope, RequestID: requestID})
	})
	return rb, err
}

// keepAdmins refuses a change that would leave no other enabled organization administrator, or, when the user is
// local, no other enabled local administrator.
func (s *Store) keepAdmins(ctx context.Context, tx *sql.Tx, orgID, userID string, local bool) error {
	q := `SELECT COUNT(*) FROM role_bindings rb JOIN users u ON u.id = rb.principal_id
		WHERE rb.org_id = ? AND rb.role = 'administrator' AND rb.scope_type = 'organization' AND u.disabled = ? AND u.id <> ?`
	var n int
	if err := tx.QueryRowContext(ctx, s.db.Rebind(q), orgID, false, userID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	if !local {
		return nil
	}
	if err := tx.QueryRowContext(ctx, s.db.Rebind(q+` AND u.provider_id IS NULL AND u.kind = 'user'`), orgID, false, userID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastLocalAdmin
	}
	return nil
}

// lockOrg serializes administrator-count checks on PostgreSQL; SQLite transactions are already serialized.
func (s *Store) lockOrg(ctx context.Context, tx *sql.Tx, orgID string) error {
	if s.db.Dialect != db.Postgres {
		return nil
	}
	_, err := tx.ExecContext(ctx, s.db.Rebind(`SELECT id FROM organizations WHERE id = ? FOR UPDATE`), orgID)
	return err
}
