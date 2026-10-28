package secrets

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// redirects reports whether a stored credential would go somewhere new or be used differently under cfg.
func redirects(old, cfg ExternalConfig) bool {
	return old.Address != cfg.Address || old.Endpoint != cfg.Endpoint || old.AuthMethod != cfg.AuthMethod || old.RoleID != cfg.RoleID
}

// UpdateProvider changes an external store's name and connection. A stored credential is never sent to a new
// address, endpoint, sign-in method or AppRole: changing those needs the credential again. An empty credential keeps
// the stored one.
func (s *Service) UpdateProvider(ctx context.Context, actor authz.Principal, id, name string, cfg ExternalConfig, credential []byte, requestID string) (Provider, error) {
	if !actor.IsOrgAdmin() {
		return Provider{}, ErrForbidden
	}
	cur, err := s.provider(ctx, actor.OrgID, id)
	if err != nil {
		return Provider{}, err
	}
	if cur.Type == ProviderLocal {
		return Provider{}, ErrNotFound
	}
	if !secretName.MatchString(name) || len(name) > 63 {
		return Provider{}, &Invalid{"the name must be lower-case letters, digits, '.', '_' or '-'"}
	}
	replace := len(credential) > 0
	keep := !replace && cur.CredentialSecretID != "" && cfg.AuthMethod != "workload-identity"
	if keep && redirects(cur.Config, cfg) {
		return Provider{}, &Invalid{"enter the credential again: Samuori never sends a stored credential to a new address, endpoint, sign-in method or AppRole"}
	}
	if err := cfg.validate(cur.Type, replace || keep); err != nil {
		return Provider{}, err
	}
	var n int
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM secret_providers WHERE org_id = ? AND name = ? AND id <> ?`), actor.OrgID, name, id).Scan(&n); err != nil {
		return Provider{}, err
	}
	if n > 0 {
		return Provider{}, &Invalid{"a provider with that name exists"}
	}
	credID, dropped := cur.CredentialSecretID, ""
	switch {
	case replace && credID != "":
		sec, err := s.secret(ctx, actor.OrgID, credID)
		if err != nil {
			return Provider{}, err
		}
		if _, err := s.Rotate(ctx, actor, credID, sec.Version, credential, requestID); err != nil {
			return Provider{}, err
		}
	case replace:
		if credID, err = s.CreateControllerSecret(ctx, actor, "secret-provider-"+name, "credential of secret provider "+name, credential, requestID); err != nil {
			return Provider{}, err
		}
	case !keep:
		credID, dropped = "", credID
	}
	raw, _ := json.Marshal(cfg)
	var changes []string
	if name != cur.Name {
		changes = append(changes, "renamed from "+cur.Name)
	}
	if replace {
		changes = append(changes, "credential replaced")
	}
	if dropped != "" {
		changes = append(changes, "credential removed")
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE secret_providers SET name = ?, config = ?, credential_secret_id = ?, last_check = NULL WHERE id = ? AND org_id = ?`),
			name, string(raw), nullString(credID), id, actor.OrgID); err != nil {
			if isUnique(err) {
				return &Invalid{"a provider with that name exists"}
			}
			return err
		}
		if dropped != "" {
			if err := deleteSecretRows(ctx, s, tx, actor.OrgID, dropped); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret-provider.update", ResourceType: "secret-provider", ResourceID: id, Outcome: "succeeded",
			Reason: strings.TrimSuffix(fmt.Sprintf("%s %s (%s); %s", cur.Type, name, cfg.AuthMethod, strings.Join(changes, ", ")), "; "), RequestID: requestID})
	})
	if err != nil {
		return Provider{}, err
	}
	return s.provider(ctx, actor.OrgID, id)
}

// DeleteProvider removes an external store and its stored credential. Grants, bindings and signing keys keep their
// store whatever their status, so a store that was ever used stays.
func (s *Service) DeleteProvider(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	cur, err := s.provider(ctx, actor.OrgID, id)
	if err != nil {
		return err
	}
	if cur.Type == ProviderLocal {
		return ErrNotFound
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var uses []string
		for _, ref := range []struct{ query, active, history string }{
			{`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END), 0) FROM secret_grants WHERE provider_id = ?`, "%d active grant(s) to projects", "the history of %d revoked grant(s)"},
			{`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END), 0) FROM secret_bindings WHERE provider_id = ?`, "%d active binding(s)", "the history of %d disabled or replaced binding(s)"},
			{`SELECT COUNT(*), COALESCE(SUM(CASE WHEN state = 'active' THEN 1 ELSE 0 END), 0) FROM signing_keys WHERE provider_id = ?`, "%d active signing key(s)", "%d retired signing key(s)"},
			{`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END), 0) FROM allowed_secrets WHERE provider_id = ?`, "%d allowed secret(s)", "the history of %d removed allowed secret(s)"},
		} {
			var all, active int
			if err := tx.QueryRowContext(ctx, s.db.Rebind(ref.query), id).Scan(&all, &active); err != nil {
				return err
			}
			if active > 0 {
				uses = append(uses, fmt.Sprintf(ref.active, active))
			}
			if all > active {
				uses = append(uses, fmt.Sprintf(ref.history, all-active))
			}
		}
		if len(uses) > 0 {
			return &InUse{Uses: uses, Store: true}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM secret_providers WHERE id = ? AND org_id = ?`), id, actor.OrgID); err != nil {
			return err
		}
		if cur.CredentialSecretID != "" {
			if err := deleteSecretRows(ctx, s, tx, actor.OrgID, cur.CredentialSecretID); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret-provider.delete", ResourceType: "secret-provider", ResourceID: id, Outcome: "succeeded",
			Reason: cur.Type + " " + cur.Name, RequestID: requestID})
	})
}

func deleteSecretRows(ctx context.Context, s *Service, tx *sql.Tx, orgID, secretID string) error {
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM secret_versions WHERE secret_id = ?`), secretID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM secrets WHERE id = ? AND org_id = ?`), secretID, orgID)
	return err
}
