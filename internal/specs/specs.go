// Package specs stores BuildSpecs as immutable, validated revisions (docs/architecture/buildspec.md).
package specs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/tenancy"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrPrecondition = errors.New("spec changed since it was read")
	ErrConflict     = errors.New("a spec with that name already exists in this project")
)

// Invalid carries every schema and semantic issue so editors can show them all at once.
type Invalid struct{ Result buildspec.Result }

func (e *Invalid) Error() string { return "the BuildSpec is not valid" }

type Spec struct {
	ID, ProjectID, Name string
	LatestRevision      int
	CreatedAt           time.Time
}

type Revision struct {
	SpecID     string
	Revision   int
	Digest     string
	Document   json.RawMessage
	EditorMode string
	Note       string
	CreatedBy  string
	CreatedAt  time.Time
}

func ETag(revision int) string { return `"` + strconv.Itoa(revision) + `"` }

type Service struct {
	db       *db.DB
	tenancy  *tenancy.Store
	policies *policy.Store
}

func NewService(d *db.DB, t *tenancy.Store, p *policy.Store) *Service {
	return &Service{db: d, tenancy: t, policies: p}
}

// Validate runs schema, semantic and argument-policy checks for an organization.
func (s *Service) Validate(ctx context.Context, orgID string, doc []byte) (buildspec.Result, policy.Effective, error) {
	eff, err := s.policies.Effective(ctx, orgID)
	if err != nil {
		return buildspec.Result{}, eff, err
	}
	opts := buildspec.DefaultOptions()
	opts.ArgRules = eff.ArgRules()
	res, err := buildspec.Validate(doc, opts)
	return res, eff, err
}

func (s *Service) prepare(ctx context.Context, orgID string, doc []byte) (canonical []byte, digest, apiVersion, name string, err error) {
	res, _, err := s.Validate(ctx, orgID, doc)
	if err != nil {
		return nil, "", "", "", err
	}
	if !res.Valid() {
		return nil, "", "", "", &Invalid{Result: res}
	}
	canonical, err = json.Marshal(res.Document)
	if err != nil {
		return nil, "", "", "", err
	}
	digest, err = buildspec.Digest(res.Document)
	return canonical, digest, res.Document.APIVersion, res.Document.Metadata.Name, err
}

func (s *Service) Create(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string, doc []byte, mode, note, requestID string) (Spec, Revision, error) {
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		return Spec{}, Revision{}, err
	}
	canonical, digest, apiVersion, name, err := s.prepare(ctx, p.OrgID, doc)
	if err != nil {
		return Spec{}, Revision{}, err
	}
	now := time.Now().UTC()
	sp := Spec{ID: ids.New(), ProjectID: projectID, Name: name, LatestRevision: 1, CreatedAt: now}
	rev := Revision{SpecID: sp.ID, Revision: 1, Digest: digest, Document: canonical, EditorMode: mode, Note: note, CreatedBy: p.ID, CreatedAt: now}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tenancy.RequireActive(ctx, s.db, tx, projectID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO build_specs (id, org_id, project_id, name, latest_revision, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?, ?)`),
			sp.ID, p.OrgID, projectID, name, p.ID, now, now); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return s.insertRevision(ctx, tx, p, rev, apiVersion, requestID)
	})
	return sp, rev, err
}

// AddRevision appends a revision when ifMatch is the ETag of the latest one.
func (s *Service) AddRevision(ctx context.Context, p authz.Principal, roles []authz.Role, specID, ifMatch string, doc []byte, mode, note, requestID string) (Revision, error) {
	sp, err := s.Get(ctx, p, roles, specID)
	if err != nil {
		return Revision{}, err
	}
	if strings.TrimPrefix(ifMatch, "W/") != ETag(sp.LatestRevision) {
		return Revision{}, ErrPrecondition
	}
	canonical, digest, apiVersion, _, err := s.prepare(ctx, p.OrgID, doc)
	if err != nil {
		return Revision{}, err
	}
	now := time.Now().UTC()
	rev := Revision{SpecID: specID, Revision: sp.LatestRevision + 1, Digest: digest, Document: canonical, EditorMode: mode, Note: note, CreatedBy: p.ID, CreatedAt: now}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := tenancy.RequireActive(ctx, s.db, tx, sp.ProjectID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE build_specs SET latest_revision = ?, updated_at = ? WHERE id = ? AND latest_revision = ?`), rev.Revision, now, specID, sp.LatestRevision)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrPrecondition
		}
		return s.insertRevision(ctx, tx, p, rev, apiVersion, requestID)
	})
	return rev, err
}

func (s *Service) insertRevision(ctx context.Context, tx *sql.Tx, p authz.Principal, r Revision, apiVersion, requestID string) error {
	if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO spec_revisions (spec_id, revision, digest, api_version, document, editor_mode, note, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		r.SpecID, r.Revision, r.Digest, apiVersion, string(r.Document), nullable(r.EditorMode), nullable(r.Note), r.CreatedBy, r.CreatedAt); err != nil {
		return err
	}
	return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "spec.revision", ResourceType: "spec", ResourceID: r.SpecID, Outcome: "succeeded", Reason: fmt.Sprintf("revision %d %s", r.Revision, r.Digest), RequestID: requestID})
}

// Get returns a spec in a project p can access with roles; other specs are not found.
func (s *Service) Get(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (Spec, error) {
	var sp Spec
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id, project_id, name, latest_revision, created_at FROM build_specs WHERE id = ? AND org_id = ?`), id, p.OrgID).
		Scan(&sp.ID, &sp.ProjectID, &sp.Name, &sp.LatestRevision, &sp.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Spec{}, ErrNotFound
	}
	if err != nil {
		return Spec{}, err
	}
	if _, err := s.tenancy.GetProject(ctx, p, roles, sp.ProjectID); err != nil {
		return Spec{}, ErrNotFound
	}
	return sp, nil
}

// List pages through a project's specs; a non-empty name returns only the spec with that metadata.name.
func (s *Service) List(ctx context.Context, p authz.Principal, roles []authz.Role, projectID, name, cursor string, limit int) ([]Spec, error) {
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		return nil, err
	}
	q, args := `SELECT id, project_id, name, latest_revision, created_at FROM build_specs WHERE project_id = ? AND org_id = ? AND id > ?`, []any{projectID, p.OrgID, cursor}
	if name != "" {
		q, args = q+` AND name = ?`, append(args, name)
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(q+` ORDER BY id LIMIT ?`), append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Spec
	for rows.Next() {
		var sp Spec
		if err := rows.Scan(&sp.ID, &sp.ProjectID, &sp.Name, &sp.LatestRevision, &sp.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

func (s *Service) Revisions(ctx context.Context, p authz.Principal, roles []authz.Role, specID string, after, limit int) ([]Revision, error) {
	if _, err := s.Get(ctx, p, roles, specID); err != nil {
		return nil, err
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(selectRevision+` WHERE spec_id = ? AND revision > ? ORDER BY revision LIMIT ?`), specID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) Revision(ctx context.Context, p authz.Principal, roles []authz.Role, specID string, n int) (Spec, Revision, error) {
	sp, err := s.Get(ctx, p, roles, specID)
	if err != nil {
		return Spec{}, Revision{}, err
	}
	r, err := scanRevision(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(selectRevision+` WHERE spec_id = ? AND revision = ?`), specID, n))
	if errors.Is(err, sql.ErrNoRows) {
		return Spec{}, Revision{}, ErrNotFound
	}
	return sp, r, err
}

// LatestRevisions returns the newest revision of every spec in an organization, for policy impact previews.
func (s *Service) LatestRevisions(ctx context.Context, orgID string) ([]Spec, []Revision, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT b.id, b.project_id, b.name, b.latest_revision, b.created_at, r.spec_id, r.revision, r.digest, r.document, r.editor_mode, r.note, r.created_by, r.created_at
		FROM build_specs b JOIN spec_revisions r ON r.spec_id = b.id AND r.revision = b.latest_revision WHERE b.org_id = ? ORDER BY b.id`), orgID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var specs []Spec
	var revs []Revision
	for rows.Next() {
		var sp Spec
		var r Revision
		var doc string
		var mode, note sql.NullString
		if err := rows.Scan(&sp.ID, &sp.ProjectID, &sp.Name, &sp.LatestRevision, &sp.CreatedAt, &r.SpecID, &r.Revision, &r.Digest, &doc, &mode, &note, &r.CreatedBy, &r.CreatedAt); err != nil {
			return nil, nil, err
		}
		r.Document, r.EditorMode, r.Note = json.RawMessage(doc), mode.String, note.String
		specs, revs = append(specs, sp), append(revs, r)
	}
	return specs, revs, rows.Err()
}

const selectRevision = `SELECT spec_id, revision, digest, document, editor_mode, note, created_by, created_at FROM spec_revisions`

func scanRevision(r interface{ Scan(...any) error }) (Revision, error) {
	var rev Revision
	var doc string
	var mode, note sql.NullString
	err := r.Scan(&rev.SpecID, &rev.Revision, &rev.Digest, &doc, &mode, &note, &rev.CreatedBy, &rev.CreatedAt)
	rev.Document, rev.EditorMode, rev.Note = json.RawMessage(doc), mode.String, note.String
	return rev, err
}

func isUnique(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Digest validates a document and returns the digest of its canonical form, as a revision of it would record.
func (s *Service) Digest(ctx context.Context, orgID string, doc []byte) (string, error) {
	_, digest, _, _, err := s.prepare(ctx, orgID, doc)
	return digest, err
}
