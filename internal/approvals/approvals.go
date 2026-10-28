// Package approvals records approval requests and their decisions. A decision is made once, by someone other than
// the requester, and never changes afterwards (docs/architecture/approvals.md).
package approvals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/tenancy"
)

const (
	Pending  = "pending"
	Approved = "approved"
	Rejected = "rejected"
	Expired  = "expired"

	SubjectPublication   = "publication"
	SubjectSecretBinding = "secret-binding"
	SubjectRepository    = "repository-request"
	SubjectBaseImage     = "base-image-request"
)

var (
	ErrNotFound  = errors.New("approval not found")
	ErrForbidden = errors.New("forbidden")
	ErrDecided   = errors.New("the approval was already decided or has expired")
	ErrOwn       = errors.New("you cannot decide your own request")
	// ErrUnfulfilled refuses approving a repository request that has no destination; fulfil it instead.
	ErrUnfulfilled = errors.New("approve a repository request by fulfilling it with a destination")
)

type Approval struct {
	ID, OrgID, ProjectID   string
	SubjectType, SubjectID string
	State, Summary         string
	RequestedBy, DecidedBy string
	Reason                 string
	CreatedAt, ExpiresAt   time.Time
	DecidedAt, ValidUntil  *time.Time
}

// Usable reports whether an approved request may still be acted on.
func (a Approval) Usable(now time.Time) bool {
	return a.State == Approved && (a.ValidUntil == nil || now.Before(*a.ValidUntil))
}

// deciders lists the roles that may decide each kind of request.
var deciders = map[string][]authz.Role{
	SubjectPublication:   {authz.Publisher, authz.SecurityReviewer, authz.Administrator},
	SubjectSecretBinding: {authz.Administrator},
	SubjectRepository:    {authz.Administrator},
	SubjectBaseImage:     {authz.Administrator},
	SubjectEgressAccess:  {authz.Administrator},
}

// orgScoped kinds change organization-wide policy or widen a project beyond it, so only organization-scoped
// bindings may decide them.
var orgScoped = map[string]bool{SubjectBaseImage: true, SubjectEgressAccess: true}

type Handler func(ctx context.Context, tx *sql.Tx, a Approval) error

type Service struct {
	db       *db.DB
	tenancy  *tenancy.Store
	handlers map[string]Handler
	now      func() time.Time
	baseImgs BaseImagePolicy
	egress   EgressPolicy
}

func NewService(d *db.DB, t *tenancy.Store) *Service {
	s := &Service{db: d, tenancy: t, handlers: map[string]Handler{}, now: time.Now}
	s.OnDecision(SubjectRepository, func(ctx context.Context, tx *sql.Tx, a Approval) error {
		if a.State != Approved {
			return nil
		}
		var dest sql.NullString
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT destination_id FROM repository_requests WHERE id = ?`), a.SubjectID).Scan(&dest); err != nil {
			return err
		}
		if !dest.Valid {
			return ErrUnfulfilled
		}
		return nil
	})
	s.registerEgressAccess()
	return s
}

// SetClock replaces the clock, for tests.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// OnDecision registers the function that applies decisions and expiries to a subject, in the same transaction.
func (s *Service) OnDecision(subject string, h Handler) { s.handlers[subject] = h }

// Create records a pending approval inside the caller's transaction.
func (s *Service) Create(ctx context.Context, tx *sql.Tx, orgID, projectID, subjectType, subjectID, summary, requestedBy string, ttl time.Duration) (Approval, error) {
	now := s.now().UTC()
	a := Approval{ID: ids.New(), OrgID: orgID, ProjectID: projectID, SubjectType: subjectType, SubjectID: subjectID, State: Pending, Summary: summary, RequestedBy: requestedBy, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	_, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO approvals (id, org_id, project_id, subject_type, subject_id, state, summary, requested_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?)`),
		a.ID, orgID, projectID, subjectType, subjectID, summary, requestedBy, now, a.ExpiresAt)
	if err != nil {
		return Approval{}, err
	}
	return a, audit.Append(ctx, s.db, tx, audit.Event{Actor: authz.Principal{ID: requestedBy, OrgID: orgID, Kind: authz.User}, Action: "approval.request", ResourceType: subjectType, ResourceID: subjectID, Outcome: "pending", Reason: summary})
}

func (s *Service) team(ctx context.Context, projectID string) string {
	var team string
	_ = s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), projectID).Scan(&team)
	return team
}

func (s *Service) canDecide(ctx context.Context, p authz.Principal, a Approval) bool {
	if orgScoped[a.SubjectType] {
		return authz.Can(p, deciders[a.SubjectType], authz.Resource{OrgID: a.OrgID})
	}
	return authz.Can(p, deciders[a.SubjectType], authz.Resource{OrgID: a.OrgID, TeamID: s.team(ctx, a.ProjectID), ProjectID: a.ProjectID})
}

// visible: deciders see the request, and so does anyone who can read its project.
func (s *Service) visible(ctx context.Context, p authz.Principal, roles []authz.Role, a Approval) bool {
	if a.OrgID != p.OrgID {
		return false
	}
	if s.canDecide(ctx, p, a) {
		return true
	}
	_, err := s.tenancy.GetProject(ctx, p, roles, a.ProjectID)
	return err == nil
}

// Decide records the decision once. validUntil bounds how long an approval may be acted on.
func (s *Service) Decide(ctx context.Context, p authz.Principal, roles []authz.Role, id string, approve bool, reason string, validUntil *time.Time, requestID string) (Approval, error) {
	return s.decide(ctx, p, roles, id, approve, reason, validUntil, requestID, nil)
}

// decide runs within, when set, in the transaction that wins the decision, so that only the decider's own changes
// to the subject are kept.
func (s *Service) decide(ctx context.Context, p authz.Principal, roles []authz.Role, id string, approve bool, reason string, validUntil *time.Time, requestID string, within func(*sql.Tx) error) (Approval, error) {
	a, err := s.load(ctx, s.db.SQL, id)
	if err != nil {
		return Approval{}, err
	}
	if !s.visible(ctx, p, roles, a) {
		return Approval{}, ErrNotFound
	}
	if !s.canDecide(ctx, p, a) {
		return Approval{}, ErrForbidden
	}
	if a.RequestedBy == p.ID {
		return Approval{}, ErrOwn
	}
	now := s.now().UTC()
	if a.State != Pending || !now.Before(a.ExpiresAt) {
		return Approval{}, ErrDecided
	}
	state := Rejected
	if approve {
		state = Approved
	}
	if validUntil != nil && !validUntil.After(now) {
		return Approval{}, fmt.Errorf("the approval must stay valid after now")
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE approvals SET state = ?, decided_by = ?, reason = ?, decided_at = ?, valid_until = ? WHERE id = ? AND state = 'pending'`),
			state, p.ID, reason, now, utc(validUntil), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrDecided
		}
		if within != nil {
			if err := within(tx); err != nil {
				return err
			}
		}
		a, err = s.load(ctx, tx, id)
		if err != nil {
			return err
		}
		if h := s.handlers[a.SubjectType]; h != nil {
			if err := h(ctx, tx, a); err != nil {
				return err
			}
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "approval.decide", ResourceType: a.SubjectType, ResourceID: a.SubjectID, Outcome: state, Reason: reason, RequestID: requestID})
	})
	return a, err
}

// Expire closes pending approvals whose time ran out.
func (s *Service) Expire(ctx context.Context) error {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id FROM approvals WHERE state = 'pending' AND expires_at <= ?`), s.now().UTC())
	if err != nil {
		return err
	}
	var due []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		due = append(due, id)
	}
	rows.Close()
	for _, id := range due {
		err := s.db.InTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE approvals SET state = 'expired', decided_at = ? WHERE id = ? AND state = 'pending'`), s.now().UTC(), id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil
			}
			a, err := s.load(ctx, tx, id)
			if err != nil {
				return err
			}
			if h := s.handlers[a.SubjectType]; h != nil {
				return h(ctx, tx, a)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

const cols = `id, org_id, project_id, subject_type, subject_id, state, summary, requested_by, COALESCE(decided_by, ''), COALESCE(reason, ''), created_at, expires_at, decided_at, valid_until`

func scan(sc interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	var decided, valid sql.NullTime
	if err := sc.Scan(&a.ID, &a.OrgID, &a.ProjectID, &a.SubjectType, &a.SubjectID, &a.State, &a.Summary, &a.RequestedBy, &a.DecidedBy, &a.Reason, &a.CreatedAt, &a.ExpiresAt, &decided, &valid); err != nil {
		return Approval{}, err
	}
	if decided.Valid {
		a.DecidedAt = &decided.Time
	}
	if valid.Valid {
		a.ValidUntil = &valid.Time
	}
	return a, nil
}

func (s *Service) load(ctx context.Context, q queryer, id string) (Approval, error) {
	a, err := scan(q.QueryRowContext(ctx, s.db.Rebind(`SELECT `+cols+` FROM approvals WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return a, err
}

// Lookup returns an approval for internal checks, without authorization.
func (s *Service) Lookup(ctx context.Context, id string) (Approval, error) {
	return s.load(ctx, s.db.SQL, id)
}

func (s *Service) Get(ctx context.Context, p authz.Principal, roles []authz.Role, id string) (Approval, error) {
	a, err := s.load(ctx, s.db.SQL, id)
	if err != nil {
		return Approval{}, err
	}
	if !s.visible(ctx, p, roles, a) {
		return Approval{}, ErrNotFound
	}
	return a, nil
}

// List returns the approvals the caller may see, newest first; state filters when set.
func (s *Service) List(ctx context.Context, p authz.Principal, roles []authz.Role, state string, limit int) ([]Approval, error) {
	q, args := `SELECT `+cols+` FROM approvals WHERE org_id = ?`, []any{p.OrgID}
	if state != "" {
		q, args = q+` AND state = ?`, append(args, state)
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(q+` ORDER BY created_at DESC LIMIT 500`), args...)
	if err != nil {
		return nil, err
	}
	var all []Approval
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, a)
	}
	rows.Close()
	out := []Approval{}
	for _, a := range all {
		if s.visible(ctx, p, roles, a) {
			out = append(out, a)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// CanDecide reports whether the caller could decide the approval, for the UI.
func (s *Service) CanDecide(ctx context.Context, p authz.Principal, a Approval) bool {
	return a.State == Pending && a.RequestedBy != p.ID && s.canDecide(ctx, p, a)
}

func utc(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// BindingRequestTTL is how long a developer's request for a secret binding stays open.
const BindingRequestTTL = 7 * 24 * time.Hour

var logicalID = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// RequestBinding asks administrators to bind a logical secret for a project. It records the request only; an
// administrator binds the secret and then approves the request, or rejects it.
func (s *Service) RequestBinding(ctx context.Context, p authz.Principal, roles []authz.Role, projectID, logical, justification string) (Approval, error) {
	project, err := s.tenancy.GetProject(ctx, p, roles, projectID)
	if err != nil {
		return Approval{}, ErrNotFound
	}
	if !logicalID.MatchString(logical) {
		return Approval{}, fmt.Errorf("the secret name must match the spec's secret ID pattern")
	}
	if l := len(justification); l < 10 || l > 2048 {
		return Approval{}, fmt.Errorf("the justification must be 10 to 2048 characters")
	}
	id := ids.New()
	var a Approval
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO secret_binding_requests (id, org_id, project_id, logical_id, justification, requested_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			id, p.OrgID, projectID, logical, justification, p.ID, s.now().UTC()); err != nil {
			return err
		}
		var err error
		a, err = s.Create(ctx, tx, p.OrgID, projectID, SubjectSecretBinding, id, fmt.Sprintf("Bind secret %s for project %s: %s", logical, project.Name, justification), p.ID, BindingRequestTTL)
		return err
	})
	return a, err
}

// RepositoryRequestTTL is how long a request for a new repository stays open.
const RepositoryRequestTTL = 7 * 24 * time.Hour

// RepositoryRequest asks administrators for a publication repository for a project (D03).
type RepositoryRequest struct {
	ID, OrgID, ProjectID, RegistryID, Repository string
	Justification, RequestedBy, DestinationID    string
	ApprovalID, State                            string
	CreatedAt                                    time.Time
}

var repositoryPath = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)

// RequestRepository records the request and a pending approval that only administrators decide. An administrator
// fulfils it by linking a destination open to the project, or rejects it.
func (s *Service) RequestRepository(ctx context.Context, p authz.Principal, roles []authz.Role, projectID, registryID, repository, justification string) (RepositoryRequest, error) {
	project, err := s.tenancy.GetProject(ctx, p, roles, projectID)
	if err != nil {
		return RepositoryRequest{}, ErrNotFound
	}
	if !repositoryPath.MatchString(repository) || len(repository) > 255 {
		return RepositoryRequest{}, fmt.Errorf("the repository must be a lower-case path such as team/app")
	}
	if l := len(justification); l < 10 || l > 2048 {
		return RepositoryRequest{}, fmt.Errorf("the justification must be 10 to 2048 characters")
	}
	if registryID != "" {
		var n int
		if err := s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*) FROM registries WHERE id = ? AND org_id = ?`), registryID, p.OrgID).Scan(&n); err != nil {
			return RepositoryRequest{}, err
		}
		if n == 0 {
			return RepositoryRequest{}, fmt.Errorf("the registry does not exist")
		}
	}
	r := RepositoryRequest{ID: ids.New(), OrgID: p.OrgID, ProjectID: projectID, RegistryID: registryID, Repository: repository, Justification: justification, RequestedBy: p.ID, State: Pending, CreatedAt: s.now().UTC()}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO repository_requests (id, org_id, project_id, registry_id, repository, justification, requested_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			r.ID, r.OrgID, r.ProjectID, nullString(registryID), repository, justification, p.ID, r.CreatedAt); err != nil {
			return err
		}
		a, err := s.Create(ctx, tx, p.OrgID, projectID, SubjectRepository, r.ID, fmt.Sprintf("Repository %s for project %s: %s", repository, project.Name, justification), p.ID, RepositoryRequestTTL)
		r.ApprovalID = a.ID
		return err
	})
	return r, err
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// RepositoryRequests lists a project's requests with the state of their approvals.
func (s *Service) RepositoryRequests(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string) ([]RepositoryRequest, error) {
	if !p.IsOrgAdmin() {
		if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
			return nil, ErrNotFound
		}
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT r.id, r.org_id, r.project_id, COALESCE(r.registry_id, ''), r.repository, r.justification, r.requested_by, COALESCE(r.destination_id, ''), a.id, a.state, r.created_at
		FROM repository_requests r JOIN approvals a ON a.subject_type = ? AND a.subject_id = r.id WHERE r.org_id = ? AND r.project_id = ? ORDER BY r.created_at DESC`), SubjectRepository, p.OrgID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RepositoryRequest{}
	for rows.Next() {
		var r RepositoryRequest
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.RegistryID, &r.Repository, &r.Justification, &r.RequestedBy, &r.DestinationID, &r.ApprovalID, &r.State, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FulfillRepository approves a pending repository request with the destination that fulfils it, once check accepts
// that destination for the request. The link is written in the approving transaction, so concurrent fulfilments
// cannot leave the request pointing at a destination its approver did not choose.
func (s *Service) FulfillRepository(ctx context.Context, p authz.Principal, roles []authz.Role, approvalID, destinationID, reason, requestID string, check func(RepositoryRequest) error) (Approval, error) {
	a, err := s.load(ctx, s.db.SQL, approvalID)
	if err != nil || a.OrgID != p.OrgID || a.SubjectType != SubjectRepository {
		return Approval{}, ErrNotFound
	}
	if !s.CanDecide(ctx, p, a) {
		if a.RequestedBy == p.ID {
			return Approval{}, ErrOwn
		}
		if a.State != Pending {
			return Approval{}, ErrDecided
		}
		return Approval{}, ErrForbidden
	}
	var r RepositoryRequest
	err = s.db.SQL.QueryRowContext(ctx, s.db.Rebind(`SELECT id, project_id, COALESCE(registry_id, ''), repository FROM repository_requests WHERE id = ?`), a.SubjectID).Scan(&r.ID, &r.ProjectID, &r.RegistryID, &r.Repository)
	if err != nil {
		return Approval{}, err
	}
	if err := check(r); err != nil {
		return Approval{}, err
	}
	return s.decide(ctx, p, roles, a.ID, true, reason, nil, requestID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE repository_requests SET destination_id = ? WHERE id = ?`), destinationID, r.ID)
		return err
	})
}
