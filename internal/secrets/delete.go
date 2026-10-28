package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// InUse refuses deleting a secret, or with Store an external store, that a project or Samuori itself still uses.
type InUse struct {
	Uses  []string
	Store bool
}

func (e *InUse) Error() string { return "in use: " + strings.Join(e.Uses, "; ") }

// systemRefs are where Samuori keeps its own credentials as local secrets. Any row counts, whatever its status, so
// history such as a retired signing key keeps its key.
var systemRefs = []struct{ table, column, what string }{
	{"registry_profiles", "secret_id", "a registry credential"},
	{"signing_keys", "secret_id", "a signing key"},
	{"identity_providers", "secret_id", "an identity provider"},
	{"mirror_token_keys", "secret_id", "the image mirror"},
	{"notification_channels", "secret_id", "a notification channel"},
	{"pipelines", "token_secret_id", "a pipeline token"},
	{"pipelines", "hook_secret_id", "a pipeline webhook"},
	{"secret_providers", "credential_secret_id", "an external secret store's credential"},
}

// DeleteSecret destroys every version of a local secret. It is refused while any project has an active grant or
// binding of it, or any Samuori record refers to it; the check and the delete share one transaction. A controller-only
// secret nothing refers to any more, such as one left by deleted or replaced credentials, may be deleted
// (docs/architecture/secrets.md#deleting-secrets).
func (s *Service) DeleteSecret(ctx context.Context, actor authz.Principal, providerID, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	prov, err := s.provider(ctx, actor.OrgID, providerID)
	if err != nil {
		return err
	}
	if prov.Type != ProviderLocal {
		return ErrNotFound
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var name string
		var controllerOnly bool
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT name, controller_only FROM secrets WHERE id = ? AND org_id = ?`), id, actor.OrgID).Scan(&name, &controllerOnly)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var uses []string
		collect := func(query, format string) error {
			rows, err := tx.QueryContext(ctx, s.db.Rebind(query), id, actor.OrgID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var project, label string
				if err := rows.Scan(&project, &label); err != nil {
					return err
				}
				uses = append(uses, fmt.Sprintf(format, project, label))
			}
			return rows.Err()
		}
		if err := collect(`SELECT pr.name, g.name FROM secret_grants g JOIN projects pr ON pr.id = g.project_id JOIN secret_providers p ON p.id = g.provider_id
			WHERE p.type = 'local' AND g.provider_ref = ? AND g.org_id = ? AND g.status = 'active' ORDER BY pr.name, g.name`, "granted to project %s as %q"); err != nil {
			return err
		}
		if err := collect(`SELECT pr.name, b.logical_id FROM secret_bindings b JOIN projects pr ON pr.id = b.project_id JOIN secret_providers p ON p.id = b.provider_id
			WHERE p.type = 'local' AND b.provider_ref = ? AND b.org_id = ? AND b.status = 'active' ORDER BY pr.name, b.logical_id`, "bound in project %s as %s"); err != nil {
			return err
		}
		samuori, err := samuoriUses(ctx, s, tx, id)
		if err != nil {
			return err
		}
		for _, what := range samuori {
			uses = append(uses, "used by Samuori as "+what)
		}
		if len(uses) > 0 {
			return &InUse{Uses: uses}
		}
		reason := "deleted " + name + " and all its versions"
		if controllerOnly {
			reason = "deleted " + name + ", a controller-only secret Samuori no longer uses, and all its versions"
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM secret_versions WHERE secret_id = ?`), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM secrets WHERE id = ? AND org_id = ?`), id, actor.OrgID); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.delete", ResourceType: "secret", ResourceID: id, Outcome: "succeeded",
			Reason: reason, RequestID: requestID})
	})
}

// samuoriUses names what Samuori itself keeps the secret for; any row counts, whatever its status.
func samuoriUses(ctx context.Context, s *Service, tx *sql.Tx, id string) ([]string, error) {
	var out []string
	for _, ref := range systemRefs {
		var n int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM `+ref.table+` WHERE `+ref.column+` = ?`), id).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, ref.what)
		}
	}
	return out, nil
}

// controllerUses maps each secret Samuori keeps for itself to what it is kept for.
func (s *Service) controllerUses(ctx context.Context) (map[string][]string, error) {
	out := map[string][]string{}
	for _, ref := range systemRefs {
		rows, err := s.db.SQL.QueryContext(ctx, `SELECT DISTINCT `+ref.column+` FROM `+ref.table+` WHERE `+ref.column+` IS NOT NULL`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if !slices.Contains(out[id], ref.what) {
				out[id] = append(out[id], ref.what)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
