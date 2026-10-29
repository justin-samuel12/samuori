// Package tenancy stores organizations, teams, projects, users and role bindings. Every read is scoped to the
// calling principal; objects outside its scope are reported as not found (docs/architecture/domain-model.md).
package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("conflict")
	// ErrArchived refuses new work on a project that is archived or purged (ADR 0058).
	ErrArchived = errors.New("this project is archived; restore it before adding uploads, specs, builds, publications or requests")
)

const (
	ProjectActive   = "active"
	ProjectArchived = "archived"
	ProjectPurged   = "purged"
)

type Team struct {
	ID, OrgID, Name string
	CreatedAt       time.Time
}

type Project struct {
	ID, OrgID, TeamID, Name, PublicationMode string
	// TeamName is set when the project is read; anyone who may see a project may see its team's name.
	TeamName             string
	CreatedAt, UpdatedAt time.Time
	// State is active, archived or purged; ArchivedBy and PurgedBy are user IDs (ADR 0058).
	State                string
	ArchivedBy, PurgedBy string
	ArchivedAt, PurgedAt *time.Time
}

type Store struct{ db *db.DB }

func NewStore(d *db.DB) *Store { return &Store{db: d} }

func (s *Store) CreateOrganization(ctx context.Context, name string) (string, error) {
	id := ids.New()
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`INSERT INTO organizations (id, name, created_at) VALUES (?, ?, ?)`), id, name, time.Now().UTC())
	return id, conflict(err)
}

func (s *Store) CreateUser(ctx context.Context, orgID, username, displayName string) (string, error) {
	id := ids.New()
	_, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`INSERT INTO users (id, org_id, username, display_name, disabled, created_at) VALUES (?, ?, ?, ?, ?, ?)`),
		id, orgID, username, displayName, false, time.Now().UTC())
	return id, conflict(err)
}

func (s *Store) CreateTeam(ctx context.Context, actor authz.Principal, name, requestID string) (Team, error) {
	if !authz.Can(actor, []authz.Role{authz.Administrator}, authz.Resource{OrgID: actor.OrgID}) {
		return Team{}, ErrForbidden
	}
	t := Team{ID: ids.New(), OrgID: actor.OrgID, Name: name, CreatedAt: time.Now().UTC()}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO teams (id, org_id, name, created_at) VALUES (?, ?, ?, ?)`), t.ID, t.OrgID, t.Name, t.CreatedAt); err != nil {
			return conflict(err)
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "team.create", ResourceType: "team", ResourceID: t.ID, Outcome: "succeeded", RequestID: requestID})
	})
	return t, err
}

// User is a person in the organization. Source names the identity provider that created them; empty for local.
type User struct {
	ID, Username, DisplayName, Source, Kind string
	Disabled                                bool
	Roles                                   []RoleBinding
}

// RoleBinding is a granted role. Source names the identity provider whose group mapping granted it; such bindings
// are replaced at every sign-in.
type RoleBinding struct {
	ID, Source string
	authz.Binding
}

// ErrLastAdmin protects the organization from losing its final administrator binding.
var ErrLastAdmin = errors.New("cannot remove the last organization administrator")

// ErrLastLocalAdmin keeps one local (password) administrator so the organization stays reachable when every
// identity provider fails.
var ErrLastLocalAdmin = errors.New("cannot remove the last local administrator; it is the way in if the identity provider fails")

// Bind grants a role on a scope inside the actor's organization; only organization administrators may change bindings.
func (s *Store) Bind(ctx context.Context, actor authz.Principal, principalID string, b authz.Binding, requestID string) (RoleBinding, error) {
	if !actor.IsOrgAdmin() {
		return RoleBinding{}, ErrForbidden
	}
	rb := RoleBinding{ID: ids.New(), Binding: b}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.inOrg(ctx, tx, actor.OrgID, "users", principalID); err != nil {
			return err
		}
		switch b.ScopeType {
		case authz.ScopeOrganization:
			if b.ScopeID != actor.OrgID {
				return ErrNotFound
			}
		case authz.ScopeTeam:
			if err := s.inOrg(ctx, tx, actor.OrgID, "teams", b.ScopeID); err != nil {
				return err
			}
		case authz.ScopeProject:
			if err := s.inOrg(ctx, tx, actor.OrgID, "projects", b.ScopeID); err != nil {
				return err
			}
		default:
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO role_bindings (id, org_id, principal_id, role, scope_type, scope_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			rb.ID, actor.OrgID, principalID, string(b.Role), string(b.ScopeType), b.ScopeID, time.Now().UTC()); err != nil {
			return conflict(err)
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "role.bind", ResourceType: "role_binding", ResourceID: rb.ID, Outcome: "succeeded", Reason: string(b.Role) + "@" + string(b.ScopeType), RequestID: requestID})
	})
	return rb, err
}

// Unbind removes a role binding, refusing to remove the organization's last administrator or last local administrator.
func (s *Store) Unbind(ctx context.Context, actor authz.Principal, bindingID, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockOrg(ctx, tx, actor.OrgID); err != nil {
			return err
		}
		var role, scope, principalID string
		var local bool
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT rb.role, rb.scope_type, rb.principal_id, u.provider_id IS NULL AND u.kind = 'user' FROM role_bindings rb JOIN users u ON u.id = rb.principal_id WHERE rb.id = ? AND rb.org_id = ?`), bindingID, actor.OrgID).Scan(&role, &scope, &principalID, &local)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if role == string(authz.Administrator) && scope == string(authz.ScopeOrganization) {
			if err := s.keepAdmins(ctx, tx, actor.OrgID, principalID, local); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM role_bindings WHERE id = ?`), bindingID); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "role.unbind", ResourceType: "role_binding", ResourceID: bindingID, Outcome: "succeeded", Reason: role + "@" + scope, RequestID: requestID})
	})
}

// ListUsers returns every user in the actor's organization with their bindings; administrators only.
func (s *Store) ListUsers(ctx context.Context, actor authz.Principal) ([]User, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT u.id, u.username, u.display_name, u.disabled, u.kind, COALESCE(up.display_name, ''), rb.id, rb.role, rb.scope_type, rb.scope_id, COALESCE(bp.display_name, '')
		FROM users u
		LEFT JOIN identity_providers up ON up.id = u.provider_id
		LEFT JOIN role_bindings rb ON rb.principal_id = u.id AND rb.org_id = u.org_id
		LEFT JOIN identity_providers bp ON bp.id = rb.source_provider_id
		WHERE u.org_id = ? ORDER BY u.username, rb.id`), actor.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var bid, role, scope, scopeID sql.NullString
		var bindingSource string
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Disabled, &u.Kind, &u.Source, &bid, &role, &scope, &scopeID, &bindingSource); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].ID != u.ID {
			out = append(out, u)
		}
		if bid.Valid {
			last := &out[len(out)-1]
			last.Roles = append(last.Roles, RoleBinding{ID: bid.String, Source: bindingSource, Binding: authz.Binding{Role: authz.Role(role.String), ScopeType: authz.ScopeType(scope.String), ScopeID: scopeID.String}})
		}
	}
	return out, rows.Err()
}

func (s *Store) inOrg(ctx context.Context, tx *sql.Tx, orgID, table, id string) error {
	var n int
	if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE id = ? AND org_id = ?`), id, orgID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) LoadPrincipal(ctx context.Context, userID string) (authz.Principal, string, error) {
	var orgID, display, kind string
	var disabled bool
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT org_id, display_name, disabled, kind FROM users WHERE id = ?`), userID).Scan(&orgID, &display, &disabled, &kind)
	if errors.Is(err, sql.ErrNoRows) || disabled {
		return authz.Principal{}, "", ErrNotFound
	}
	if err != nil {
		return authz.Principal{}, "", err
	}
	p := authz.Principal{ID: userID, OrgID: orgID, Kind: authz.User}
	if kind == "service" {
		p.Kind = authz.Service
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT role, scope_type, scope_id FROM role_bindings WHERE principal_id = ? AND org_id = ?`), userID, orgID)
	if err != nil {
		return authz.Principal{}, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var b authz.Binding
		var role, scope string
		if err := rows.Scan(&role, &scope, &b.ScopeID); err != nil {
			return authz.Principal{}, "", err
		}
		b.Role, b.ScopeType = authz.Role(role), authz.ScopeType(scope)
		p.Bindings = append(p.Bindings, b)
	}
	return p, display, rows.Err()
}

func (s *Store) OrganizationName(ctx context.Context, orgID string) (string, error) {
	var name string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT name FROM organizations WHERE id = ?`), orgID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

func (s *Store) ListTeams(ctx context.Context, p authz.Principal) ([]Team, error) {
	where, args := s.visibility(p, nil, "id", "")
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, org_id, name, created_at FROM teams WHERE org_id = ? AND (`+where+`) ORDER BY name`), append([]any{p.OrgID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListProjects returns projects visible to p through roles, ordered by ID, starting after cursor. Archived and
// purged projects are left out unless includeArchived is set.
func (s *Store) ListProjects(ctx context.Context, p authz.Principal, roles []authz.Role, cursor string, limit int, includeArchived bool) ([]Project, error) {
	where, args := s.visibility(p, roles, "team_id", "id")
	if !includeArchived {
		where = "(" + where + ") AND state = 'active'"
	}
	q := `SELECT ` + projectCols + ` FROM projects WHERE org_id = ? AND id > ? AND (` + where + `) ORDER BY id LIMIT ?`
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(q), append(append([]any{p.OrgID, cursor}, args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		pr, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

func (s *Store) GetProject(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (Project, error) {
	row := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+projectCols+` FROM projects WHERE id = ? AND org_id = ?`), id, p.OrgID)
	pr, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	if !authz.Can(p, roles, authz.Resource{OrgID: pr.OrgID, TeamID: pr.TeamID, ProjectID: pr.ID}) {
		return Project{}, ErrNotFound
	}
	return pr, nil
}

func (s *Store) CreateProject(ctx context.Context, p authz.Principal, teamID, name, mode, requestID string) (Project, error) {
	var orgID string
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT org_id FROM teams WHERE id = ?`), teamID).Scan(&orgID)
	if errors.Is(err, sql.ErrNoRows) || orgID != p.OrgID {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	team := authz.Resource{OrgID: orgID, TeamID: teamID}
	if !authz.Can(p, []authz.Role{authz.Maintainer, authz.Administrator}, team) {
		if authz.Can(p, []authz.Role{authz.Developer, authz.Publisher, authz.SecurityReviewer, authz.Operator}, team) {
			return Project{}, ErrForbidden
		}
		return Project{}, ErrNotFound
	}
	if mode == "" {
		mode = "manual"
	}
	now := time.Now().UTC()
	pr := Project{ID: ids.New(), OrgID: orgID, TeamID: teamID, Name: name, PublicationMode: mode, CreatedAt: now, UpdatedAt: now, State: ProjectActive}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO projects (id, org_id, team_id, name, publication_mode, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			pr.ID, pr.OrgID, pr.TeamID, pr.Name, pr.PublicationMode, pr.CreatedAt, pr.UpdatedAt); err != nil {
			return conflict(err)
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "project.create", ResourceType: "project", ResourceID: pr.ID, Outcome: "succeeded", RequestID: requestID})
	})
	if err != nil {
		return Project{}, err
	}
	return pr, nil
}

// visibility builds a SQL predicate matching rows whose team column (and optional project column) is covered by p.
func (s *Store) visibility(p authz.Principal, roles []authz.Role, teamCol, projectCol string) (string, []any) {
	matches := func(r authz.Role) bool {
		if roles == nil {
			return true
		}
		for _, x := range roles {
			if x == r {
				return true
			}
		}
		return false
	}
	var conds []string
	var args []any
	for _, b := range p.Bindings {
		if !matches(b.Role) {
			continue
		}
		switch b.ScopeType {
		case authz.ScopeOrganization:
			if b.ScopeID == p.OrgID {
				return "1 = 1", nil
			}
		case authz.ScopeTeam:
			conds, args = append(conds, teamCol+" = ?"), append(args, b.ScopeID)
		case authz.ScopeProject:
			if projectCol != "" {
				conds, args = append(conds, projectCol+" = ?"), append(args, b.ScopeID)
			} else {
				conds, args = append(conds, teamCol+" IN (SELECT team_id FROM projects WHERE id = ?)"), append(args, b.ScopeID)
			}
		}
	}
	if len(conds) == 0 {
		return "1 = 0", nil
	}
	return strings.Join(conds, " OR "), args
}

type scanner interface{ Scan(...any) error }

const projectCols = `id, org_id, team_id, name, publication_mode, created_at, updated_at, COALESCE((SELECT t.name FROM teams t WHERE t.id = projects.team_id), ''), state, COALESCE(archived_by, ''), archived_at, COALESCE(purged_by, ''), purged_at`

func scanProject(r scanner) (Project, error) {
	var p Project
	var archived, purged sql.NullTime
	err := r.Scan(&p.ID, &p.OrgID, &p.TeamID, &p.Name, &p.PublicationMode, &p.CreatedAt, &p.UpdatedAt, &p.TeamName, &p.State, &p.ArchivedBy, &archived, &p.PurgedBy, &purged)
	if archived.Valid {
		p.ArchivedAt = &archived.Time
	}
	if purged.Valid {
		p.PurgedAt = &purged.Time
	}
	return p, err
}

// Querier is a *sql.DB or *sql.Tx.
type Querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// RequireActive is the one guard every path that creates work for a project calls inside its transaction; it
// returns ErrArchived unless the project is active (ADR 0058). On PostgreSQL it share-locks the row so an archive
// that commits first is seen.
func RequireActive(ctx context.Context, d *db.DB, q Querier, projectID string) error {
	query := `SELECT state FROM projects WHERE id = ?`
	if d.Dialect == db.Postgres {
		query += ` FOR SHARE`
	}
	var state string
	err := q.QueryRowContext(ctx, d.Rebind(query), projectID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != ProjectActive {
		return ErrArchived
	}
	return nil
}

func conflict(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate key") {
		return ErrConflict
	}
	return err
}

// ProjectName is the name rule for projects, the same as the API contract's.
var ProjectName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ErrInvalid reports a rejected project setting.
type ErrInvalid struct{ Reason string }

func (e *ErrInvalid) Error() string { return e.Reason }

// ProjectChanges are the settings UpdateProject may change; nil leaves a setting as it is. A project never moves to
// another team.
type ProjectChanges struct {
	Name, PublicationMode *string
}

// UpdateProject renames the project or changes whether it publishes automatically after a passing validation. Only
// maintainers and administrators of the project's team may, with the same name rule and per-team uniqueness as
// creation; each change is audited.
func (s *Store) UpdateProject(ctx context.Context, p authz.Principal, id string, c ProjectChanges, requestID string) (Project, error) {
	if c.PublicationMode != nil && *c.PublicationMode != "manual" && *c.PublicationMode != "automatic" {
		return Project{}, &ErrInvalid{"the publication mode must be manual or automatic"}
	}
	if c.Name != nil && !ProjectName.MatchString(*c.Name) {
		return Project{}, &ErrInvalid{"the name must be lower-case letters, digits and dashes, at most 63 characters"}
	}
	if c.Name == nil && c.PublicationMode == nil {
		return Project{}, &ErrInvalid{"change the name or the publication mode"}
	}
	readers := []authz.Role{authz.Developer, authz.Maintainer, authz.Publisher, authz.SecurityReviewer, authz.Administrator}
	pr, err := s.GetProject(ctx, p, readers, id)
	if err != nil {
		return Project{}, err
	}
	if !authz.Can(p, []authz.Role{authz.Maintainer, authz.Administrator}, authz.Resource{OrgID: pr.OrgID, TeamID: pr.TeamID, ProjectID: pr.ID}) {
		return Project{}, ErrForbidden
	}
	now := time.Now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := RequireActive(ctx, s.db, tx, id); err != nil {
			return err
		}
		if c.Name != nil && *c.Name != pr.Name {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE projects SET name = ?, updated_at = ? WHERE id = ?`), *c.Name, now, id); err != nil {
				return conflict(err)
			}
			if err := audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "project.update", ResourceType: "project", ResourceID: id, Outcome: "succeeded",
				Reason: "renamed " + pr.Name + " to " + *c.Name, RequestID: requestID}); err != nil {
				return err
			}
			pr.Name = *c.Name
		}
		if c.PublicationMode != nil && *c.PublicationMode != pr.PublicationMode {
			if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE projects SET publication_mode = ?, updated_at = ? WHERE id = ?`), *c.PublicationMode, now, id); err != nil {
				return err
			}
			if err := audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "project.update", ResourceType: "project", ResourceID: id, Outcome: "succeeded",
				Reason: "publication mode " + *c.PublicationMode, RequestID: requestID}); err != nil {
				return err
			}
			pr.PublicationMode = *c.PublicationMode
		}
		return nil
	})
	if err != nil {
		return Project{}, err
	}
	pr.UpdatedAt = now
	return pr, nil
}
