// Package imagesources manages approved image sources: private upstream registries reached through an authorizing
// mirror (ADR 0030, OD-14). The controller is the mirror's token issuer: it grants pull access, per build attempt, to
// exactly the repositories the build's team may use (docs/architecture/registry-auth.md#build-time-pulls).
package imagesources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/distribution/reference"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("already exists")
)

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

// Source is an approved private image source. Builds pull Upstream images through Mirror, and only repositories
// matching Patterns, for the listed teams.
type Source struct {
	ID, Name, Upstream, Mirror string
	Patterns, TeamIDs          []string
	PullProfileID              string
	Status                     string
	CreatedAt, UpdatedAt       time.Time
}

// Secrets keeps the token-signing key as a controller-only secret.
type Secrets interface {
	CreateControllerSecret(ctx context.Context, actor authz.Principal, name, description string, value []byte, requestID string) (string, error)
	ControllerValue(ctx context.Context, orgID, secretID string) ([]byte, error)
}

type Store struct {
	db      *db.DB
	secrets Secrets
	// Shared allows one mirror to serve more than one team. OD-14 keeps it off until SEC-REG-04, 07 and 08 pass in CI.
	Shared bool
	now    func() time.Time
}

func NewStore(d *db.DB, sec Secrets) *Store { return &Store{db: d, secrets: sec, now: time.Now} }

var (
	name     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	hostPort = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)
	pattern  = regexp.MustCompile(`^[a-z0-9*?._/-]{1,255}$`)
)

func (s *Store) validate(ctx context.Context, orgID string, src *Source) error {
	if !name.MatchString(src.Name) {
		return &Invalid{"names are lower-case letters, digits and dashes"}
	}
	src.Upstream, src.Mirror = strings.ToLower(src.Upstream), strings.ToLower(src.Mirror)
	if !hostPort.MatchString(src.Upstream) || !hostPort.MatchString(src.Mirror) || src.Upstream == src.Mirror {
		return &Invalid{"the upstream and the mirror are different hosts, such as registry.example and mirror.samuori.internal:5000"}
	}
	if len(src.Patterns) == 0 {
		return &Invalid{"at least one repository pattern is required, such as payments/*"}
	}
	for _, p := range src.Patterns {
		if _, err := path.Match(p, ""); err != nil || !pattern.MatchString(p) || p == "*" || strings.Contains(p, "..") {
			return &Invalid{"repository patterns are paths with * and ?, such as payments/*; a bare * is too broad"}
		}
	}
	if len(src.TeamIDs) == 0 {
		return &Invalid{"at least one team must be allowed to use the source"}
	}
	if len(src.TeamIDs) > 1 && !s.Shared {
		return &Invalid{"one mirror may serve one team until shared mirrors are enabled (OD-14); add a mirror per team"}
	}
	for _, t := range src.TeamIDs {
		var n int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM teams WHERE id = ? AND org_id = ?`), t, orgID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return &Invalid{"unknown team " + t}
		}
	}
	if src.PullProfileID != "" {
		var purpose string
		err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT purpose FROM registry_profiles WHERE id = ? AND org_id = ?`), src.PullProfileID, orgID).Scan(&purpose)
		if err != nil || purpose != "pull" {
			return &Invalid{"the profile must be a pull profile of this organization; push credentials never serve builds"}
		}
	}
	if !s.Shared {
		var others int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM image_sources WHERE org_id = ? AND mirror = ? AND id != ? AND team_ids != ?`), orgID, src.Mirror, src.ID, teamsJSON(src.TeamIDs)).Scan(&others); err != nil {
			return err
		}
		if others > 0 {
			return &Invalid{"this mirror already serves another team; shared mirrors are not enabled (OD-14)"}
		}
	}
	return nil
}

func teamsJSON(t []string) string {
	c := slices.Clone(t)
	slices.Sort(c)
	raw, _ := json.Marshal(c)
	return string(raw)
}

// Create approves an image source; administrators only, audited.
func (s *Store) Create(ctx context.Context, actor authz.Principal, src Source, requestID string) (Source, error) {
	if !actor.IsOrgAdmin() {
		return Source{}, ErrForbidden
	}
	src.ID = ids.New()
	if err := s.validate(ctx, actor.OrgID, &src); err != nil {
		return Source{}, err
	}
	src.Status = "active"
	src.CreatedAt = s.now().UTC()
	src.UpdatedAt = src.CreatedAt
	patterns, _ := json.Marshal(src.Patterns)
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO image_sources (id, org_id, name, upstream, mirror, repository_patterns, team_ids, pull_profile_id, status, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			src.ID, actor.OrgID, src.Name, src.Upstream, src.Mirror, string(patterns), teamsJSON(src.TeamIDs), nullable(src.PullProfileID), src.Status, actor.ID, src.CreatedAt, src.UpdatedAt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(err.Error(), "duplicate key") {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "image-source.create", ResourceType: "image-source", ResourceID: src.ID, Outcome: "succeeded",
			Reason: fmt.Sprintf("%s via %s: %s", src.Upstream, src.Mirror, strings.Join(src.Patterns, ", ")), RequestID: requestID})
	})
	return src, err
}

// SetStatus disables or re-enables a source. Disabling stops new tokens at once; issued tokens expire within minutes.
func (s *Store) SetStatus(ctx context.Context, actor authz.Principal, id string, active bool, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	status := "disabled"
	if active {
		status = "active"
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE image_sources SET status = ?, updated_at = ? WHERE id = ? AND org_id = ?`), status, s.now().UTC(), id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "image-source." + map[bool]string{true: "enable", false: "disable"}[active], ResourceType: "image-source", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}

const cols = `id, name, upstream, mirror, repository_patterns, team_ids, COALESCE(pull_profile_id, ''), status, created_at, updated_at`

func scan(sc interface{ Scan(...any) error }) (Source, error) {
	var src Source
	var patterns, teams string
	if err := sc.Scan(&src.ID, &src.Name, &src.Upstream, &src.Mirror, &patterns, &teams, &src.PullProfileID, &src.Status, &src.CreatedAt, &src.UpdatedAt); err != nil {
		return Source{}, err
	}
	_ = json.Unmarshal([]byte(patterns), &src.Patterns)
	_ = json.Unmarshal([]byte(teams), &src.TeamIDs)
	return src, nil
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Source, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+cols+` FROM image_sources WHERE `+q+` ORDER BY name`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Source{}
	for rows.Next() {
		src, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// List returns every source of the organization for administrators, and only those a developer's teams may use
// otherwise, so developers see names, not other teams' sources.
func (s *Store) List(ctx context.Context, actor authz.Principal) ([]Source, error) {
	all, err := s.query(ctx, `org_id = ?`, actor.OrgID)
	if err != nil || actor.IsOrgAdmin() {
		return all, err
	}
	out := []Source{}
	for _, src := range all {
		if slices.ContainsFunc(src.TeamIDs, func(t string) bool {
			return authz.Can(actor, []authz.Role{authz.Developer, authz.Maintainer, authz.Publisher}, authz.Resource{OrgID: actor.OrgID, TeamID: t})
		}) {
			src.PullProfileID = ""
			out = append(out, src)
		}
	}
	return out, nil
}

// Get returns one of the organization's sources.
func (s *Store) Get(ctx context.Context, orgID, id string) (Source, error) {
	src, err := scan(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+cols+` FROM image_sources WHERE id = ? AND org_id = ?`), id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	return src, err
}

// ForUpstream returns the active sources for an upstream registry host.
func (s *Store) ForUpstream(ctx context.Context, orgID, upstream string) ([]Source, error) {
	return s.query(ctx, `org_id = ? AND upstream = ? AND status = 'active'`, orgID, strings.ToLower(upstream))
}

// Allows reports whether a team may pull repository through the source.
func (src Source) Allows(teamID, repository string) bool {
	if src.Status != "active" || !slices.Contains(src.TeamIDs, teamID) {
		return false
	}
	return slices.ContainsFunc(src.Patterns, func(p string) bool { ok, _ := path.Match(p, repository); return ok })
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// Admit reports whether image comes from an upstream with approved sources and, if so, whether the project's team
// may pull it. Images from other registries are left to the base image policy.
func (s *Store) Admit(ctx context.Context, orgID, projectID, image string) (private, allowed bool, err error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return false, false, nil
	}
	sources, err := s.ForUpstream(ctx, orgID, reference.Domain(named))
	if err != nil || len(sources) == 0 {
		return false, false, err
	}
	var team string
	if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT team_id FROM projects WHERE id = ? AND org_id = ?`), projectID, orgID).Scan(&team); err != nil {
		return true, false, err
	}
	for _, src := range sources {
		if src.Allows(team, reference.Path(named)) {
			return true, true, nil
		}
	}
	return true, false, nil
}
