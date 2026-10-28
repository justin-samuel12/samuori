package specs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"go.yaml.in/yaml/v3"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/secretscan"
)

// Rendering is what the editor needs to switch views: the normalized document, its YAML and any issues.
type Rendering struct {
	Valid     bool
	Document  json.RawMessage
	YAML      string
	Digest    string
	Schema    []buildspec.Issue
	Semantics []buildspec.Issue
}

// Render parses YAML or JSON and returns canonical forms. Documents that fail the schema are still rendered
// (keys sorted, not canonical) so an incomplete draft can be edited as YAML.
func (s *Service) Render(ctx context.Context, orgID string, input []byte) (Rendering, error) {
	res, _, err := s.Validate(ctx, orgID, input)
	if err != nil {
		return Rendering{}, err
	}
	r := Rendering{Valid: res.Valid(), Schema: res.SchemaErrors, Semantics: res.SemanticErrors}
	if res.Document != nil {
		y, err := buildspec.Canonical(res.Document)
		if err != nil {
			return Rendering{}, err
		}
		r.YAML = string(y)
		r.Document, _ = json.Marshal(res.Document)
		r.Digest, _ = buildspec.Digest(res.Document)
		return r, nil
	}
	var generic any
	if yaml.Unmarshal(input, &generic) == nil && generic != nil {
		if js, err := json.Marshal(generic); err == nil {
			r.Document = js
			var buf bytes.Buffer
			enc := yaml.NewEncoder(&buf)
			enc.SetIndent(2)
			if enc.Encode(generic) == nil && enc.Close() == nil {
				r.YAML = buf.String()
			}
		}
	}
	return r, nil
}

const maxDraftBytes = 256 << 10

var (
	ErrDraftConflict = errors.New("the draft changed in another window")
	ErrDraftTooLarge = errors.New("the draft is larger than 256 KiB")
)

// SecretInDraft rejects drafts containing credential-like text; drafts must never store secret values.
type SecretInDraft struct{ Message string }

func (e *SecretInDraft) Error() string { return e.Message }

type Draft struct {
	Version   int
	Data      json.RawMessage
	UpdatedAt time.Time
}

func (s *Service) GetDraft(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string) (Draft, error) {
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		return Draft{}, err
	}
	var d Draft
	var data string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT version, data, updated_at FROM wizard_drafts WHERE user_id = ? AND project_id = ?`), p.ID, projectID).Scan(&d.Version, &data, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	d.Data = json.RawMessage(data)
	return d, err
}

// SaveDraft writes the caller's draft when version matches the stored one (0 creates it).
func (s *Service) SaveDraft(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string, version int, data json.RawMessage) (Draft, error) {
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		return Draft{}, err
	}
	if len(data) > maxDraftBytes {
		return Draft{}, ErrDraftTooLarge
	}
	if hits := secretscan.Match(data); len(hits) > 0 {
		return Draft{}, &SecretInDraft{Message: hits[0].Message}
	}
	d := Draft{Version: version + 1, Data: data, UpdatedAt: time.Now().UTC()}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		var res sql.Result
		var err error
		if version == 0 {
			res, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO wizard_drafts (user_id, project_id, org_id, version, data, updated_at) VALUES (?, ?, ?, 1, ?, ?)`), p.ID, projectID, p.OrgID, string(data), d.UpdatedAt)
			if err != nil && isUnique(err) {
				return ErrDraftConflict
			}
		} else {
			res, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE wizard_drafts SET version = ?, data = ?, updated_at = ? WHERE user_id = ? AND project_id = ? AND version = ?`), d.Version, string(data), d.UpdatedAt, p.ID, projectID, version)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrDraftConflict
		}
		return nil
	})
	return d, err
}

func (s *Service) DeleteDraft(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string) error {
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		return err
	}
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`DELETE FROM wizard_drafts WHERE user_id = ? AND project_id = ?`), p.ID, projectID)
	return err
}
