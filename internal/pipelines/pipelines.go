// Package pipelines turns Git pushes into builds (D20, ADR 0033, docs/architecture/pipelines.md). A pipeline fetches
// one commit into a snapshot and starts a build of its spec; checks, approvals and publication then follow the
// project and spec as for an upload. A run builds with its owner's current roles.
package pipelines

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"path"
	"regexp"
	"samuori.invalid/samuori/internal/cluster"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
	"samuori.invalid/samuori/internal/tenancy"
)

var (
	ErrForbidden = errors.New("forbidden")
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("conflict")
	ErrBusy      = errors.New("too many queued runs")
)

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

// BuildRoles are the roles a run's owner needs on the project, the same as for starting a build by hand.
var BuildRoles = []authz.Role{authz.Developer, authz.Maintainer}

// Hook schemes: an HMAC-SHA256 signature of the body (GitHub, Gitea, Forgejo) or GitLab's shared token.
const (
	SchemeHMAC   = "hmac-sha256"
	SchemeGitLab = "gitlab-token"
)

type Pipeline struct {
	ID, OrgID, ProjectID, Name string
	SpecID                     string
	// SpecRevision 0 builds the spec's latest revision.
	SpecRevision        int
	RepoURL             string
	Refs                []string
	Username            string
	HasToken            bool
	AllowPrivateNetwork bool
	HookScheme          string
	OwnerID             string
	Enabled             bool
	CreatedBy           string
	CreatedAt           time.Time
	UpdatedAt           time.Time

	tokenSecretID, hookSecretID string
}

type Secrets interface {
	CreateControllerSecret(ctx context.Context, actor authz.Principal, name, description string, value []byte, requestID string) (string, error)
	ControllerValue(ctx context.Context, orgID, secretID string) ([]byte, error)
}

type Snapshots interface {
	Ingest(ctx context.Context, p authz.Principal, roles []authz.Role, projectID, archive, requestID string) (sources.Snapshot, error)
	Staging() string
	Limits() sources.Limits
}

type Builds interface {
	Create(ctx context.Context, p authz.Principal, roles []authz.Role, specID string, revision int, snapshotID string, opts builds.CreateOptions, key, requestID string) (builds.Run, error)
}

type Specs interface {
	Get(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (specs.Spec, error)
}

type Projects interface {
	GetProject(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (tenancy.Project, error)
	LoadPrincipal(ctx context.Context, userID string) (authz.Principal, string, error)
}

type Service struct {
	db        *db.DB
	secrets   Secrets
	snapshots Snapshots
	builds    Builds
	specs     Specs
	projects  Projects
	fetcher   Fetcher
	// MaxQueued bounds queued webhook runs per pipeline.
	MaxQueued int
	now       func() time.Time
	owner     cluster.Ownership
}

// UseOwnership records which controller fetches each run, so a fetch interrupted by a departed controller is taken
// over without waiting for its claim to expire.
func (s *Service) UseOwnership(o cluster.Ownership) { s.owner = o }

func NewService(d *db.DB, sec Secrets, snaps Snapshots, b Builds, sp Specs, pr Projects, f Fetcher) *Service {
	return &Service{db: d, secrets: sec, snapshots: snaps, builds: b, specs: sp, projects: pr, fetcher: f, MaxQueued: 20, now: time.Now, owner: &cluster.Solo{}}
}

var (
	pipelineName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	username     = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,100}$`)
)

// validRepoURL accepts https URLs without credentials, query or fragment; http only to a loopback host.
func validRepoURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || u.Path == "" || len(raw) > 500 {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		ip := net.ParseIP(u.Hostname())
		return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return false
}

// validPattern accepts full ref globs under refs/heads/ or refs/tags/.
func validPattern(p string) bool {
	if !strings.HasPrefix(p, "refs/heads/") && !strings.HasPrefix(p, "refs/tags/") {
		return false
	}
	if _, err := path.Match(p, "refs/heads/x"); err != nil {
		return false
	}
	return ValidRef(strings.ReplaceAll(strings.ReplaceAll(p, "*", "x"), "?", "x"))
}

// Matches reports whether ref is one the pipeline builds.
func (p Pipeline) Matches(ref string) bool {
	if !ValidRef(ref) {
		return false
	}
	for _, pat := range p.Refs {
		if ok, _ := path.Match(pat, ref); ok {
			return true
		}
	}
	return false
}

func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Create adds a pipeline and returns its webhook secret, shown once. Only organization administrators manage
// pipelines; the owner is the administrator or one of the organization's service identities.
func (s *Service) Create(ctx context.Context, actor authz.Principal, p Pipeline, token, requestID string) (Pipeline, string, error) {
	if !actor.IsOrgAdmin() {
		return Pipeline{}, "", ErrForbidden
	}
	switch {
	case !pipelineName.MatchString(p.Name):
		return Pipeline{}, "", &Invalid{"names are lower-case letters, digits and dashes"}
	case !validRepoURL(p.RepoURL):
		return Pipeline{}, "", &Invalid{"the repository URL must be https, without credentials, query or fragment (http only on loopback)"}
	case len(p.Refs) == 0 || len(p.Refs) > 20:
		return Pipeline{}, "", &Invalid{"give 1 to 20 ref patterns, such as refs/heads/main or refs/tags/v*"}
	case p.HookScheme != SchemeHMAC && p.HookScheme != SchemeGitLab:
		return Pipeline{}, "", &Invalid{"the webhook scheme must be hmac-sha256 or gitlab-token"}
	case p.Username != "" && !username.MatchString(p.Username):
		return Pipeline{}, "", &Invalid{"the username has characters that are not allowed"}
	case p.Username != "" && token == "":
		return Pipeline{}, "", &Invalid{"a username needs a token"}
	case len(token) > 4096 || strings.ContainsAny(token, "\r\n"):
		return Pipeline{}, "", &Invalid{"the token is not valid"}
	case p.SpecRevision < 0:
		return Pipeline{}, "", &Invalid{"the spec revision must be 0 (latest) or a revision number"}
	}
	for _, r := range p.Refs {
		if !validPattern(r) {
			return Pipeline{}, "", &Invalid{"ref pattern " + r + " is not valid; use refs/heads/<branch> or refs/tags/<tag>, with * and ? as wildcards"}
		}
	}
	if p.OwnerID == "" {
		p.OwnerID = actor.ID
	}
	owner, _, err := s.projects.LoadPrincipal(ctx, p.OwnerID)
	if err != nil || owner.OrgID != actor.OrgID || (p.OwnerID != actor.ID && owner.Kind != authz.Service) {
		return Pipeline{}, "", &Invalid{"the owner must be you or one of the organization's service identities"}
	}
	if _, err := s.projects.GetProject(ctx, owner, BuildRoles, p.ProjectID); err != nil {
		return Pipeline{}, "", &Invalid{"the owner needs the developer or maintainer role on the project; a service identity with that role is recommended"}
	}
	var specProject string
	err = s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT project_id FROM build_specs WHERE id = ? AND org_id = ?`), p.SpecID, actor.OrgID).Scan(&specProject)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && specProject != p.ProjectID) {
		return Pipeline{}, "", &Invalid{"the spec does not exist in this project"}
	}
	if err != nil {
		return Pipeline{}, "", err
	}
	if p.SpecRevision > 0 {
		var n int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM spec_revisions WHERE spec_id = ? AND revision = ?`), p.SpecID, p.SpecRevision).Scan(&n); err != nil {
			return Pipeline{}, "", err
		}
		if n == 0 {
			return Pipeline{}, "", &Invalid{"the spec has no such revision"}
		}
	}
	now := s.now().UTC()
	p.ID, p.OrgID, p.Enabled, p.CreatedBy, p.CreatedAt, p.UpdatedAt = ids.New(), actor.OrgID, true, actor.ID, now, now
	suffix := strings.ToLower(p.ID[len(p.ID)-6:])
	hook := newSecret()
	if p.hookSecretID, err = s.secrets.CreateControllerSecret(ctx, actor, "pipeline-"+p.Name+"-hook-"+suffix, "webhook secret of pipeline "+p.Name, []byte(hook), requestID); err != nil {
		return Pipeline{}, "", err
	}
	if token != "" {
		if p.tokenSecretID, err = s.secrets.CreateControllerSecret(ctx, actor, "pipeline-"+p.Name+"-token-"+suffix, "repository token of pipeline "+p.Name, []byte(token), requestID); err != nil {
			return Pipeline{}, "", err
		}
		p.HasToken = true
	}
	refs, _ := json.Marshal(p.Refs)
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO pipelines (id, org_id, project_id, name, spec_id, spec_revision, repo_url, refs, username, token_secret_id, allow_private_network, hook_scheme, hook_secret_id, owner_id, enabled, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			p.ID, p.OrgID, p.ProjectID, p.Name, p.SpecID, p.SpecRevision, p.RepoURL, string(refs), nullable(p.Username), nullable(p.tokenSecretID), p.AllowPrivateNetwork, p.HookScheme, p.hookSecretID, p.OwnerID, true, actor.ID, now, now); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pipeline.create", ResourceType: "pipeline", ResourceID: p.ID, Outcome: "succeeded", Reason: p.Name + " " + p.RepoURL, RequestID: requestID})
	})
	if err != nil {
		return Pipeline{}, "", err
	}
	return p, hook, nil
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

const pipelineCols = `id, org_id, project_id, name, spec_id, spec_revision, repo_url, refs, COALESCE(username, ''), COALESCE(token_secret_id, ''), allow_private_network, hook_scheme, hook_secret_id, owner_id, enabled, created_by, created_at, updated_at`

func scanPipeline(sc interface{ Scan(...any) error }) (Pipeline, error) {
	var p Pipeline
	var refs string
	if err := sc.Scan(&p.ID, &p.OrgID, &p.ProjectID, &p.Name, &p.SpecID, &p.SpecRevision, &p.RepoURL, &refs, &p.Username, &p.tokenSecretID, &p.AllowPrivateNetwork, &p.HookScheme, &p.hookSecretID, &p.OwnerID, &p.Enabled, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Pipeline{}, err
	}
	_ = json.Unmarshal([]byte(refs), &p.Refs)
	p.HasToken = p.tokenSecretID != ""
	return p, nil
}

func (s *Service) load(ctx context.Context, id string) (Pipeline, error) {
	p, err := scanPipeline(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+pipelineCols+` FROM pipelines WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Pipeline{}, ErrNotFound
	}
	return p, err
}

// viewRoles may see a project's pipelines and their runs.
var viewRoles = []authz.Role{authz.Developer, authz.Maintainer, authz.Publisher, authz.Operator, authz.SecurityReviewer, authz.Administrator}

// Get returns a pipeline to administrators and to people who can see its project.
func (s *Service) Get(ctx context.Context, actor authz.Principal, id string) (Pipeline, error) {
	p, err := s.load(ctx, id)
	if err != nil || p.OrgID != actor.OrgID {
		return Pipeline{}, ErrNotFound
	}
	if !actor.IsOrgAdmin() {
		if _, err := s.projects.GetProject(ctx, actor, viewRoles, p.ProjectID); err != nil {
			return Pipeline{}, ErrNotFound
		}
	}
	return p, nil
}

// List returns a project's pipelines, or every pipeline of the organization for administrators.
func (s *Service) List(ctx context.Context, actor authz.Principal, projectID string) ([]Pipeline, error) {
	q, args := `SELECT `+pipelineCols+` FROM pipelines WHERE org_id = ?`, []any{actor.OrgID}
	if projectID != "" {
		if !actor.IsOrgAdmin() {
			if _, err := s.projects.GetProject(ctx, actor, viewRoles, projectID); err != nil {
				return nil, ErrNotFound
			}
		}
		q, args = q+` AND project_id = ?`, append(args, projectID)
	} else if !actor.IsOrgAdmin() {
		return nil, &Invalid{"choose a project"}
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(q+` ORDER BY name`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pipeline{}
	for rows.Next() {
		p, err := scanPipeline(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) SetEnabled(ctx context.Context, actor authz.Principal, id string, enabled bool, requestID string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE pipelines SET enabled = ?, updated_at = ? WHERE id = ? AND org_id = ?`), enabled, s.now().UTC(), id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pipeline." + map[bool]string{true: "enable", false: "disable"}[enabled], ResourceType: "pipeline", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}

// RotateHookSecret replaces the webhook secret; the old one stops working at once.
func (s *Service) RotateHookSecret(ctx context.Context, actor authz.Principal, id, requestID string) (string, error) {
	if !actor.IsOrgAdmin() {
		return "", ErrForbidden
	}
	p, err := s.load(ctx, id)
	if err != nil || p.OrgID != actor.OrgID {
		return "", ErrNotFound
	}
	hook := newSecret()
	secretID, err := s.secrets.CreateControllerSecret(ctx, actor, "pipeline-"+p.Name+"-hook-"+strings.ToLower(ids.New()[20:]), "webhook secret of pipeline "+p.Name, []byte(hook), requestID)
	if err != nil {
		return "", err
	}
	return hook, s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE pipelines SET hook_secret_id = ?, updated_at = ? WHERE id = ?`), secretID, s.now().UTC(), id); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "pipeline.rotate-hook-secret", ResourceType: "pipeline", ResourceID: id, Outcome: "succeeded", RequestID: requestID})
	})
}
