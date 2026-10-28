package approvals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
)

// BaseImagePolicy is the organization base-image policy that approved requests extend
// (docs/architecture/recipes-and-policies.md#base-image-requests).
type BaseImagePolicy interface {
	BaseImageListed(ctx context.Context, orgID, repository string) (bool, error)
	AllowBaseImageRepository(ctx context.Context, tx *sql.Tx, actor authz.Principal, repository, reason string) (int, error)
}

// BaseImageRequestTTL is how long a request for an outside base-image repository stays open.
const BaseImageRequestTTL = 14 * 24 * time.Hour

var (
	ErrAlreadyAllowed = errors.New("already allowed")
	ErrRequestPending = errors.New("a request for this repository is already waiting for an administrator")
)

// BaseImageRequest asks organization administrators to add a repository to the base-image policy.
type BaseImageRequest struct {
	ID, OrgID, ProjectID, Repository string
	Justification, RequestedBy       string
	ApprovalID, State, Reason        string
	PolicyVersion                    int
	CreatedAt                        time.Time
}

var baseImageRepository = regexp.MustCompile(`^(?:localhost|[a-z0-9]+(?:-+[a-z0-9]+)*(?:\.[a-z0-9]+(?:-+[a-z0-9]+)*)+)(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)+$`)

// NormalizeBaseImageRepository checks a requested repository the way the base-images policy does and expands short
// Docker Hub names, so "node" becomes docker.io/library/node.
func NormalizeBaseImageRepository(repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.ContainsAny(repo, "@: ") {
		return "", fmt.Errorf("the repository must be a registry host and path such as mcr.microsoft.com/mssql/server, without tag or digest")
	}
	n := repo
	if first, _, found := strings.Cut(repo, "/"); !found {
		n = "docker.io/library/" + repo
	} else if !strings.Contains(first, ".") && first != "localhost" {
		n = "docker.io/" + repo
	}
	if len(n) > 255 || !baseImageRepository.MatchString(n) {
		return "", fmt.Errorf("the repository must be a lower-case registry host and path such as mcr.microsoft.com/mssql/server")
	}
	return n, nil
}

// UseBaseImagePolicy connects approved base-image requests to the policy they extend.
func (s *Service) UseBaseImagePolicy(p BaseImagePolicy) {
	s.baseImgs = p
	s.OnDecision(SubjectBaseImage, func(ctx context.Context, tx *sql.Tx, a Approval) error {
		if a.State == Pending {
			return nil
		}
		version := sql.NullInt64{}
		if a.State == Approved {
			var repo string
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT repository FROM base_image_requests WHERE id = ?`), a.SubjectID).Scan(&repo); err != nil {
				return err
			}
			v, err := p.AllowBaseImageRepository(ctx, tx, authz.Principal{ID: a.DecidedBy, OrgID: a.OrgID, Kind: authz.User}, repo,
				fmt.Sprintf("base-image request %s approved: allow %s", a.SubjectID, repo))
			if err != nil {
				return err
			}
			version = sql.NullInt64{Int64: int64(v), Valid: true}
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE base_image_requests SET pending_repository = NULL, policy_version = ?, closed_at = ? WHERE id = ?`), version, s.now().UTC(), a.SubjectID)
		return err
	})
}

// RequestBaseImage records a project member's request and a pending approval that only organization administrators
// decide. Approving it adds the repository to the base-images policy in the deciding transaction.
func (s *Service) RequestBaseImage(ctx context.Context, p authz.Principal, roles []authz.Role, projectID, repository, justification string) (BaseImageRequest, error) {
	project, err := s.tenancy.GetProject(ctx, p, roles, projectID)
	if err != nil {
		return BaseImageRequest{}, ErrNotFound
	}
	if s.baseImgs == nil {
		return BaseImageRequest{}, fmt.Errorf("base-image requests are not available on this server")
	}
	repo, err := NormalizeBaseImageRepository(repository)
	if err != nil {
		return BaseImageRequest{}, err
	}
	if l := len(strings.TrimSpace(justification)); l < 20 || len(justification) > 2048 {
		return BaseImageRequest{}, fmt.Errorf("the justification must be 20 to 2048 characters")
	}
	listed, err := s.baseImgs.BaseImageListed(ctx, p.OrgID, repo)
	if err != nil {
		return BaseImageRequest{}, err
	}
	if listed {
		return BaseImageRequest{}, fmt.Errorf("%w: %s is already on the organization's approved base-image list", ErrAlreadyAllowed, repo)
	}
	r := BaseImageRequest{ID: ids.New(), OrgID: p.OrgID, ProjectID: projectID, Repository: repo, Justification: justification, RequestedBy: p.ID, State: Pending, CreatedAt: s.now().UTC()}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO base_image_requests (id, org_id, project_id, repository, pending_repository, justification, requested_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			r.ID, r.OrgID, r.ProjectID, repo, repo, justification, p.ID, r.CreatedAt); err != nil {
			if isUnique(err) {
				return ErrRequestPending
			}
			return err
		}
		a, err := s.Create(ctx, tx, p.OrgID, projectID, SubjectBaseImage, r.ID, fmt.Sprintf("Base image repository %s for project %s: %s", repo, project.Name, justification), p.ID, BaseImageRequestTTL)
		r.ApprovalID = a.ID
		return err
	})
	if err != nil {
		return BaseImageRequest{}, err
	}
	return r, nil
}

func isUnique(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}

// BaseImageRequests lists a project's base-image requests with the state of their approvals.
func (s *Service) BaseImageRequests(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string) ([]BaseImageRequest, error) {
	if !p.IsOrgAdmin() {
		if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
			return nil, ErrNotFound
		}
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT r.id, r.org_id, r.project_id, r.repository, r.justification, r.requested_by, COALESCE(r.policy_version, 0), a.id, a.state, COALESCE(a.reason, ''), r.created_at
		FROM base_image_requests r JOIN approvals a ON a.subject_type = ? AND a.subject_id = r.id WHERE r.org_id = ? AND r.project_id = ? ORDER BY r.created_at DESC`), SubjectBaseImage, p.OrgID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BaseImageRequest{}
	for rows.Next() {
		var r BaseImageRequest
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.Repository, &r.Justification, &r.RequestedBy, &r.PolicyVersion, &r.ApprovalID, &r.State, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
