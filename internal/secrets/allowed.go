package secrets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

// AllowedSecret is a secret of an external store that administrators may grant to projects. A store connection
// alone allows nothing (docs/architecture/secrets.md#allowed-secrets).
type AllowedSecret struct {
	ID           string
	ProviderID   string
	ProviderName string
	ProviderType string
	Reference    string
	Label        string
	Note         string
	// Fields are the key names of a whole KV v2 secret; empty for a single key.
	Fields    []string
	CreatedBy string
	CreatedAt time.Time
}

// AllowSecret adds a secret of an external store to the allowed list. It is read once to prove it exists (a whole
// KV v2 secret records its key names); values are discarded at once.
func (s *Service) AllowSecret(ctx context.Context, actor authz.Principal, providerID, reference, label, note, requestID string) (AllowedSecret, error) {
	if !actor.IsOrgAdmin() {
		return AllowedSecret{}, ErrForbidden
	}
	prov, err := s.provider(ctx, actor.OrgID, providerID)
	if errors.Is(err, ErrNotFound) {
		return AllowedSecret{}, &Invalid{"the secret store does not exist in this organization"}
	}
	if err != nil {
		return AllowedSecret{}, err
	}
	if prov.Type == ProviderLocal {
		return AllowedSecret{}, &Invalid{"stored secrets are granted directly; the allowed list is for external stores"}
	}
	if err := validReference(prov.Type, prov.Config, reference); err != nil {
		return AllowedSecret{}, err
	}
	if label == "" {
		label = reference
	}
	if len(label) > 128 || len(note) > 1024 {
		return AllowedSecret{}, &Invalid{"the label must be at most 128 characters and the note at most 1024"}
	}
	fields := []string{}
	if isKeySet(prov.Type, reference) {
		if fields, err = s.keyNames(ctx, actor.OrgID, prov.ID, reference, "latest"); err != nil {
			return AllowedSecret{}, &Invalid{"the store could not list that secret's keys: " + clipText(err.Error(), 300)}
		}
	} else {
		v, err := s.readExternal(ctx, actor.OrgID, prov.ID, reference, "latest")
		if err != nil {
			return AllowedSecret{}, &Invalid{"the store could not read that reference: " + clipText(err.Error(), 300)}
		}
		clear(v)
	}
	keys, _ := json.Marshal(fields)
	a := AllowedSecret{ID: ids.New(), ProviderID: prov.ID, ProviderName: prov.Name, ProviderType: prov.Type, Reference: reference, Label: label, Note: note, Fields: fields, CreatedBy: actor.ID, CreatedAt: s.now().UTC()}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO allowed_secrets (id, org_id, provider_id, reference, label, note, fields, status, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			a.ID, actor.OrgID, prov.ID, reference, label, note, string(keys), Active, actor.ID, a.CreatedAt); err != nil {
			if isUnique(err) {
				return &Invalid{"that secret is already allowed"}
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.allow", ResourceType: "allowed-secret", ResourceID: a.ID, Outcome: "succeeded",
			Reason: fmt.Sprintf("%s %s as %q (%d keys)", prov.Name, reference, label, len(fields)), RequestID: requestID})
	})
	if err != nil {
		return AllowedSecret{}, err
	}
	return a, nil
}

// RemoveAllowedSecret stops new grants of a secret. Grants already made stay until revoked.
func (s *Service) RemoveAllowedSecret(ctx context.Context, actor authz.Principal, id, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE allowed_secrets SET status = 'removed', removed_by = ?, removed_at = ? WHERE id = ? AND org_id = ? AND status = ?`),
			actor.ID, s.now().UTC(), id, actor.OrgID, Active)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "secret.allow.remove", ResourceType: "allowed-secret", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}

const allowedCols = `a.id, a.provider_id, p.name, p.type, a.reference, a.label, a.note, a.fields, a.created_by, a.created_at FROM allowed_secrets a JOIN secret_providers p ON p.id = a.provider_id`

func scanAllowed(sc interface{ Scan(...any) error }) (AllowedSecret, error) {
	var a AllowedSecret
	var fields string
	if err := sc.Scan(&a.ID, &a.ProviderID, &a.ProviderName, &a.ProviderType, &a.Reference, &a.Label, &a.Note, &fields, &a.CreatedBy, &a.CreatedAt); err != nil {
		return AllowedSecret{}, err
	}
	a.Fields = []string{}
	_ = json.Unmarshal([]byte(fields), &a.Fields)
	return a, nil
}

// AllowedSecrets lists the organization's active allowed secrets for administrators.
func (s *Service) AllowedSecrets(ctx context.Context, actor authz.Principal) ([]AllowedSecret, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+allowedCols+` WHERE a.org_id = ? AND a.status = ? ORDER BY p.name, a.label`), actor.OrgID, Active)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AllowedSecret{}
	for rows.Next() {
		a, err := scanAllowed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) allowedSecret(ctx context.Context, orgID, id string) (AllowedSecret, error) {
	a, err := scanAllowed(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+allowedCols+` WHERE a.id = ? AND a.org_id = ? AND a.status = ?`), id, orgID, Active))
	if errors.Is(err, sql.ErrNoRows) {
		return AllowedSecret{}, ErrNotFound
	}
	return a, err
}

// allowing finds the active entry that allows a reference: the same reference, or one key of an allowed whole secret.
func (s *Service) allowing(ctx context.Context, orgID, providerID, ref string) (AllowedSecret, bool, error) {
	path, key, hasKey := strings.Cut(ref, "#")
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+allowedCols+` WHERE a.org_id = ? AND a.provider_id = ? AND a.status = ? AND a.reference IN (?, ?)`), orgID, providerID, Active, ref, path)
	if err != nil {
		return AllowedSecret{}, false, err
	}
	defer rows.Close()
	var found []AllowedSecret
	for rows.Next() {
		a, err := scanAllowed(rows)
		if err != nil {
			return AllowedSecret{}, false, err
		}
		found = append(found, a)
	}
	if err := rows.Err(); err != nil {
		return AllowedSecret{}, false, err
	}
	for _, a := range found {
		if a.Reference == ref {
			return a, true, nil
		}
	}
	for _, a := range found {
		if hasKey && a.Reference == path && slices.Contains(a.Fields, key) {
			return a, true, nil
		}
	}
	return AllowedSecret{}, false, nil
}
