// Package publication publishes validated images from internal storage to admin-defined destinations. Everything
// here runs on the controller: workers never see destinations or registry credentials (ADRs 0024 and 0030,
// docs/architecture/publication.md).
package publication

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"samuori.invalid/samuori/internal/cluster"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/approvals"
	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/secrets"
	"samuori.invalid/samuori/internal/signing"
	"samuori.invalid/samuori/internal/storage"
	"samuori.invalid/samuori/internal/tenancy"
	"samuori.invalid/samuori/internal/validation"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrConflict  = errors.New("already exists")
)

type Invalid struct{ Reason string }

func (e *Invalid) Error() string { return e.Reason }

type Registry struct {
	ID, Name, Kind, Endpoint string
	// AccountID and Region identify an ECR registry.
	AccountID, Region       string
	PlainHTTP, AllowPrivate bool
	CreatedAt               time.Time
}

type ConnectionTest struct {
	Status   string     `json:"status"`
	TestedAt time.Time  `json:"testedAt"`
	Detail   string     `json:"detail,omitempty"`
	Steps    []TestStep `json:"steps,omitempty"`
}

type Profile struct {
	ID, RegistryID, Name, Type, Purpose, Username, SecretID string
	Scopes                                                  []string
	Status                                                  string
	LastTest                                                *ConnectionTest
	ECR                                                     ECRConfig
}

type Destination struct {
	ID, Name, RegistryID, Repository, PushProfileID string
	AcceptsRestricted                               bool
	AllowedProjects                                 []string
	ProtectedTags                                   []string
	ImmutableTags                                   bool
	RequiresApproval                                bool
	// SignImages signs every image published here with the organization's active key; AttachProvenance adds a
	// signed SLSA provenance attestation. PromotionOnly accepts only promotions of already published images.
	SignImages, AttachProvenance, PromotionOnly bool
}

type Service struct {
	db    *db.DB
	blobs *storage.Blobs
	// developerDownloads reads the organization's publication policy; nil refuses developers.
	developerDownloads func(ctx context.Context, orgID string) (bool, error)
	tenancy            *tenancy.Store
	secrets            *secrets.Service
	validations        *validation.Service
	approvals          *approvals.Service
	pusher             Pusher
	ecr                ECR
	signing            *signing.Store
	log                *slog.Logger
	wake               func()
	now                func() time.Time
	owner              cluster.Ownership
}

// UseOwnership records which controller holds each destination lock, so a lock held by a controller that is gone is
// released and its publication resumed.
func (s *Service) UseOwnership(o cluster.Ownership) { s.owner = o }

func NewService(d *db.DB, blobs *storage.Blobs, t *tenancy.Store, sec *secrets.Service, v *validation.Service, p Pusher, log *slog.Logger) *Service {
	return &Service{db: d, blobs: blobs, tenancy: t, secrets: sec, validations: v, pusher: p, log: log, wake: func() {}, now: time.Now, owner: &cluster.Solo{}}
}

func (s *Service) OnCommit(f func()) { s.wake = f }

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}

var name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (s *Service) CreateRegistry(ctx context.Context, actor authz.Principal, r Registry, requestID string) (Registry, error) {
	if !actor.IsOrgAdmin() {
		return Registry{}, ErrForbidden
	}
	if err := validRegistry(&r); err != nil {
		return Registry{}, err
	}
	r.ID, r.CreatedAt = ids.New(), s.now().UTC()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO registries (id, org_id, name, kind, endpoint, plain_http, allow_private_network, created_by, created_at, account_id, region) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			r.ID, actor.OrgID, r.Name, r.Kind, r.Endpoint, r.PlainHTTP, r.AllowPrivate, actor.ID, r.CreatedAt, r.AccountID, r.Region); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "registry.create", ResourceType: "registry", ResourceID: r.ID, Outcome: "succeeded",
			Reason: fmt.Sprintf("%s (plain HTTP %t, private network %t)", r.Endpoint, r.PlainHTTP, r.AllowPrivate), RequestID: requestID})
	})
	return r, err
}

func validRegistry(r *Registry) error {
	if !name.MatchString(r.Name) {
		return &Invalid{"names are lower-case letters, digits and dashes"}
	}
	switch r.Kind {
	case "oci":
		if err := ValidEndpoint(r.Endpoint); err != nil {
			return &Invalid{err.Error()}
		}
		r.AccountID, r.Region = "", ""
	case "ecr":
		return ecrRegistry(r)
	default:
		return &Invalid{"the kind must be oci or ecr; use oci for Harbor and other registries"}
	}
	return nil
}

func (s *Service) registry(ctx context.Context, orgID, id string) (Registry, error) {
	var r Registry
	err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id, name, kind, endpoint, plain_http, allow_private_network, created_at, account_id, region FROM registries WHERE id = ? AND org_id = ?`), id, orgID).
		Scan(&r.ID, &r.Name, &r.Kind, &r.Endpoint, &r.PlainHTTP, &r.AllowPrivate, &r.CreatedAt, &r.AccountID, &r.Region)
	if errors.Is(err, sql.ErrNoRows) {
		return Registry{}, ErrNotFound
	}
	return r, err
}

func (s *Service) Registries(ctx context.Context, actor authz.Principal) ([]Registry, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, name, kind, endpoint, plain_http, allow_private_network, created_at, account_id, region FROM registries WHERE org_id = ? ORDER BY name`), actor.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Registry{}
	for rows.Next() {
		var r Registry
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.Endpoint, &r.PlainHTTP, &r.AllowPrivate, &r.CreatedAt, &r.AccountID, &r.Region); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateProfile records registry credentials. The referenced secret becomes controller-only, so it can never be
// bound to a project or released to a worker.
func (s *Service) CreateProfile(ctx context.Context, actor authz.Principal, p Profile, requestID string) (Profile, error) {
	if !actor.IsOrgAdmin() {
		return Profile{}, ErrForbidden
	}
	reg, err := s.registry(ctx, actor.OrgID, p.RegistryID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Profile{}, &Invalid{"the registry does not exist"}
		}
		return Profile{}, err
	}
	if p.Purpose != "push" && p.Purpose != "pull" {
		return Profile{}, &Invalid{"purpose must be push or pull"}
	}
	if err := validProfile(&p, reg); err != nil {
		return Profile{}, err
	}
	p.ID, p.Status = ids.New(), "active"
	scopes, _ := json.Marshal(p.Scopes)
	ecrConfig := "{}"
	if p.Type == "ecr" {
		raw, _ := json.Marshal(p.ECR)
		ecrConfig = string(raw)
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		var secretRef any
		if p.SecretID != "" {
			if err := s.secrets.MarkControllerOnly(ctx, tx, actor.OrgID, p.SecretID); err != nil {
				return err
			}
			secretRef = p.SecretID
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO registry_profiles (id, org_id, registry_id, name, type, purpose, username, secret_id, repository_scopes, status, created_by, created_at, config) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			p.ID, actor.OrgID, p.RegistryID, p.Name, p.Type, p.Purpose, p.Username, secretRef, string(scopes), p.Status, actor.ID, s.now().UTC(), ecrConfig); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "registry-profile.create", ResourceType: "registry-profile", ResourceID: p.ID, Outcome: "succeeded", Reason: p.Type + " " + p.Purpose, RequestID: requestID})
	})
	if err != nil {
		var inv *secrets.Invalid
		if errors.As(err, &inv) {
			return Profile{}, &Invalid{inv.Reason}
		}
		return Profile{}, err
	}
	return p, nil
}

func validProfile(p *Profile, reg Registry) error {
	if !name.MatchString(p.Name) {
		return &Invalid{"names are lower-case letters, digits and dashes"}
	}
	if (reg.Kind == "ecr") != (p.Type == "ecr") {
		return &Invalid{"ECR registries take ECR profiles, and only they do"}
	}
	switch p.Type {
	case "anonymous":
		if p.SecretID != "" || p.Username != "" {
			return &Invalid{"anonymous profiles take no credentials"}
		}
	case "oci-basic":
		if p.Username == "" || p.SecretID == "" {
			return &Invalid{"basic profiles need a username and a password secret"}
		}
	case "oci-token":
		if p.SecretID == "" || p.Username != "" {
			return &Invalid{"token profiles need a token secret and no username"}
		}
	case "ecr":
		if p.Username != "" {
			return &Invalid{"ECR profiles take no username"}
		}
		if err := p.ECR.validate(p.SecretID != ""); err != nil {
			return err
		}
	default:
		return &Invalid{"type must be anonymous, oci-basic, oci-token or ecr"}
	}
	if p.Type != "ecr" {
		p.ECR = ECRConfig{}
	}
	if p.Scopes == nil {
		p.Scopes = []string{}
	}
	for _, sc := range p.Scopes {
		if _, err := path.Match(sc, ""); err != nil || sc == "" {
			return &Invalid{"repository scopes are glob patterns such as team/*"}
		}
	}
	return nil
}

const profileCols = `id, registry_id, name, type, purpose, username, COALESCE(secret_id, ''), repository_scopes, status, last_test, config`

func scanProfile(sc interface{ Scan(...any) error }) (Profile, error) {
	var p Profile
	var scopes, cfg string
	var last sql.NullString
	if err := sc.Scan(&p.ID, &p.RegistryID, &p.Name, &p.Type, &p.Purpose, &p.Username, &p.SecretID, &scopes, &p.Status, &last, &cfg); err != nil {
		return Profile{}, err
	}
	if p.Type == "ecr" {
		_ = json.Unmarshal([]byte(cfg), &p.ECR)
	}
	p.Scopes = []string{}
	_ = json.Unmarshal([]byte(scopes), &p.Scopes)
	if last.Valid {
		var t ConnectionTest
		if json.Unmarshal([]byte(last.String), &t) == nil {
			p.LastTest = &t
		}
	}
	return p, nil
}

func (s *Service) profile(ctx context.Context, orgID, id string) (Profile, error) {
	p, err := scanProfile(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+profileCols+` FROM registry_profiles WHERE id = ? AND org_id = ?`), id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	return p, err
}

func (s *Service) Profiles(ctx context.Context, actor authz.Principal) ([]Profile, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+profileCols+` FROM registry_profiles WHERE org_id = ? ORDER BY name`), actor.OrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Profile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// target resolves a destination's repository and credentials for the controller, just in time.
func (s *Service) target(ctx context.Context, orgID string, d Destination) (Target, Profile, error) {
	p, err := s.profile(ctx, orgID, d.PushProfileID)
	if err != nil {
		return Target{}, Profile{}, err
	}
	r, err := s.registry(ctx, orgID, p.RegistryID)
	if err != nil {
		return Target{}, Profile{}, err
	}
	t := Target{Endpoint: r.Endpoint, Repository: d.Repository, PlainHTTP: r.PlainHTTP, AllowPrivate: r.AllowPrivate}
	if p.Type == "ecr" {
		c, err := s.ecrConnection(ctx, orgID, r, p)
		if err != nil {
			return Target{}, Profile{}, err
		}
		if t.Username, t.Password, err = s.ecr.Token(ctx, c); err != nil {
			return Target{}, Profile{}, fmt.Errorf("the ECR token could not be obtained: %w", err)
		}
		return t, p, nil
	}
	if p.SecretID != "" {
		v, err := s.secrets.ControllerValue(ctx, orgID, p.SecretID)
		if err != nil {
			return Target{}, Profile{}, fmt.Errorf("the profile's credential is unavailable")
		}
		if p.Type == "oci-token" {
			t.Token = string(v)
		} else {
			t.Username, t.Password = p.Username, string(v)
		}
	}
	return t, p, nil
}

// TestProfile proves connectivity and authentication against the registry without publishing anything.
func (s *Service) TestProfile(ctx context.Context, actor authz.Principal, id string) (ConnectionTest, error) {
	if !actor.IsOrgAdmin() {
		return ConnectionTest{}, ErrForbidden
	}
	p, err := s.profile(ctx, actor.OrgID, id)
	if err != nil {
		return ConnectionTest{}, err
	}
	scope := "samuori-test"
	if len(p.Scopes) > 0 {
		scope = p.Scopes[0]
	}
	res := ConnectionTest{Status: "ok", TestedAt: s.now().UTC()}
	if p.Type == "ecr" {
		res = s.testECR(ctx, actor.OrgID, p, res)
		raw, _ := json.Marshal(res)
		_, err = s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE registry_profiles SET last_test = ? WHERE id = ?`), string(raw), id)
		return res, err
	}
	t, _, err := s.target(ctx, actor.OrgID, Destination{PushProfileID: id, Repository: scope})
	if err == nil {
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = s.pusher.Ping(tctx, t)
		cancel()
	}
	if err != nil {
		res.Status, res.Detail = "failed", redact(err, t)
	}
	raw, _ := json.Marshal(res)
	_, err = s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE registry_profiles SET last_test = ? WHERE id = ?`), string(raw), id)
	return res, err
}

// testECR runs the ECR adapter's step-by-step test against the profile's scoped repositories.
func (s *Service) testECR(ctx context.Context, orgID string, p Profile, res ConnectionTest) ConnectionTest {
	r, err := s.registry(ctx, orgID, p.RegistryID)
	if err == nil {
		var c ECRConnection
		if c, err = s.ecrConnection(ctx, orgID, r, p); err == nil {
			tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			res.Steps, err = s.ecr.Test(tctx, c, p.Scopes)
			cancel()
		}
	}
	for _, st := range res.Steps {
		if st.Status == "failed" && err == nil {
			err = fmt.Errorf("%s: %s", st.Step, st.Detail)
		}
	}
	if err != nil {
		res.Status, res.Detail = "failed", clipText(err.Error(), 500)
	}
	return res
}

// redact removes credentials from an error before it is stored or shown.
func redact(err error, t Target) string {
	msg := err.Error()
	for _, secret := range []string{t.Password, t.Token} {
		if len(secret) >= 4 {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

func (s *Service) CreateDestination(ctx context.Context, actor authz.Principal, d Destination, requestID string) (Destination, error) {
	if !actor.IsOrgAdmin() {
		return Destination{}, ErrForbidden
	}
	if err := s.validDestination(ctx, actor.OrgID, &d); err != nil {
		return Destination{}, err
	}
	d.ID = ids.New()
	projects, _ := json.Marshal(d.AllowedProjects)
	tags, _ := json.Marshal(d.ProtectedTags)
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO destinations (id, org_id, name, registry_id, repository, push_profile_id, accepts_restricted, allowed_project_ids, protected_tags, immutable_tags, requires_approval, created_by, created_at, sign_images, attach_provenance, promotion_only) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			d.ID, actor.OrgID, d.Name, d.RegistryID, d.Repository, d.PushProfileID, d.AcceptsRestricted, string(projects), string(tags), d.ImmutableTags, d.RequiresApproval, actor.ID, s.now().UTC(), d.SignImages, d.AttachProvenance, d.PromotionOnly); err != nil {
			if isUnique(err) {
				return ErrConflict
			}
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "destination.create", ResourceType: "destination", ResourceID: d.ID, Outcome: "succeeded", Reason: d.Repository, RequestID: requestID})
	})
	return d, err
}

func (s *Service) validDestination(ctx context.Context, orgID string, d *Destination) error {
	if !name.MatchString(d.Name) {
		return &Invalid{"names are lower-case letters, digits and dashes"}
	}
	if !repository.MatchString(d.Repository) || len(d.Repository) > 255 {
		return &Invalid{"the repository must be a lower-case OCI repository path such as team/app"}
	}
	p, err := s.profile(ctx, orgID, d.PushProfileID)
	if err != nil {
		return &Invalid{"the push profile does not exist"}
	}
	if p.Purpose != "push" || p.RegistryID != d.RegistryID {
		return &Invalid{"the profile must be a push profile for the same registry"}
	}
	if len(p.Scopes) > 0 && !slices.ContainsFunc(p.Scopes, func(sc string) bool { ok, _ := path.Match(sc, d.Repository); return ok }) {
		return &Invalid{"the profile's repository scopes do not include this repository"}
	}
	for _, tag := range d.ProtectedTags {
		if _, err := path.Match(tag, ""); err != nil || tag == "" {
			return &Invalid{"protected tags are patterns such as latest or v*"}
		}
	}
	for _, pid := range d.AllowedProjects {
		var n int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM projects WHERE id = ? AND org_id = ?`), pid, orgID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return &Invalid{"unknown project " + pid}
		}
	}
	if d.AllowedProjects == nil {
		d.AllowedProjects = []string{}
	}
	if d.ProtectedTags == nil {
		d.ProtectedTags = []string{}
	}
	return nil
}

// SetAllowedProjects replaces the projects that may publish to a destination.
func (s *Service) SetAllowedProjects(ctx context.Context, actor authz.Principal, id string, projects []string) error {
	if !actor.IsOrgAdmin() {
		return ErrForbidden
	}
	raw, _ := json.Marshal(projects)
	res, err := s.db.SQL.ExecContext(ctx, s.db.Rebind(`UPDATE destinations SET allowed_project_ids = ? WHERE id = ? AND org_id = ?`), string(raw), id, actor.OrgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

const destinationCols = `id, name, registry_id, repository, push_profile_id, accepts_restricted, allowed_project_ids, protected_tags, immutable_tags, requires_approval, sign_images, attach_provenance, promotion_only`

func scanDestination(sc interface{ Scan(...any) error }) (Destination, error) {
	var d Destination
	var projects, tags string
	if err := sc.Scan(&d.ID, &d.Name, &d.RegistryID, &d.Repository, &d.PushProfileID, &d.AcceptsRestricted, &projects, &tags, &d.ImmutableTags, &d.RequiresApproval, &d.SignImages, &d.AttachProvenance, &d.PromotionOnly); err != nil {
		return Destination{}, err
	}
	d.AllowedProjects, d.ProtectedTags = []string{}, []string{}
	_ = json.Unmarshal([]byte(projects), &d.AllowedProjects)
	_ = json.Unmarshal([]byte(tags), &d.ProtectedTags)
	return d, nil
}

func (s *Service) destination(ctx context.Context, orgID, id string) (Destination, error) {
	d, err := scanDestination(s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT `+destinationCols+` FROM destinations WHERE id = ? AND org_id = ?`), id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Destination{}, ErrNotFound
	}
	return d, err
}

func (s *Service) Destinations(ctx context.Context, actor authz.Principal) ([]Destination, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	return s.destinations(ctx, actor.OrgID)
}

func (s *Service) destinations(ctx context.Context, orgID string) ([]Destination, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+destinationCols+` FROM destinations WHERE org_id = ? ORDER BY name`), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Destination{}
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ProjectDestinations lists the destinations a project may publish to; registry details and credentials are
// never included.
func (s *Service) ProjectDestinations(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string) ([]Destination, error) {
	if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
		return nil, ErrNotFound
	}
	all, err := s.destinations(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	out := []Destination{}
	for _, d := range all {
		if slices.Contains(d.AllowedProjects, projectID) {
			out = append(out, d)
		}
	}
	return out, nil
}

// FulfillRepositoryRequest approves a developer's repository request by linking a destination open to the request's
// project, in the requested registry when one was named (D03). Create the repository and destination first.
func (s *Service) FulfillRepositoryRequest(ctx context.Context, actor authz.Principal, roles []authz.Role, approvalID, destinationID, reason, requestID string) (approvals.Approval, error) {
	if !actor.IsOrgAdmin() {
		return approvals.Approval{}, ErrForbidden
	}
	d, err := s.destination(ctx, actor.OrgID, destinationID)
	if err != nil {
		return approvals.Approval{}, &Invalid{"the destination does not exist"}
	}
	return s.approvals.FulfillRepository(ctx, actor, roles, approvalID, d.ID, reason, requestID, func(r approvals.RepositoryRequest) error {
		if !slices.Contains(d.AllowedProjects, r.ProjectID) {
			return &Invalid{"the destination is not open to the request's project"}
		}
		if r.RegistryID != "" && d.RegistryID != r.RegistryID {
			return &Invalid{"the destination is in another registry than the one requested"}
		}
		return nil
	})
}

// DestinationName names a destination for display; it reveals no settings.
func (s *Service) DestinationName(ctx context.Context, orgID, id string) string {
	var n string
	_ = s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT name FROM destinations WHERE id = ? AND org_id = ?`), id, orgID).Scan(&n)
	return n
}
