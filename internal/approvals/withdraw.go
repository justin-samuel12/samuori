package approvals

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
)

// Withdrawn closes a pending request at its requester's wish; it is a closing state like expired, never a decision
// (ADR 0062).
const Withdrawn = "withdrawn"

var (
	// ErrNotWithdrawable refuses withdrawing a kind of request that has its own lifecycle, such as a publication.
	ErrNotWithdrawable = errors.New("only repository, base-image, egress-access and secret-binding requests can be withdrawn")
	ErrWithdrawReason  = errors.New("the reason is at most 2048 characters")
)

var withdrawable = map[string]bool{SubjectSecretBinding: true, SubjectRepository: true, SubjectBaseImage: true, SubjectEgressAccess: true}

// requesterRoles may withdraw a request they made; managerRoles may withdraw anyone's request on the project.
var (
	requesterRoles = []authz.Role{authz.Developer, authz.Maintainer, authz.Administrator}
	managerRoles   = []authz.Role{authz.Maintainer, authz.Administrator}
)

func (s *Service) canWithdraw(ctx context.Context, p authz.Principal, a Approval) bool {
	if !withdrawable[a.SubjectType] || a.OrgID != p.OrgID {
		return false
	}
	res := authz.Resource{OrgID: a.OrgID, TeamID: s.team(ctx, a.ProjectID), ProjectID: a.ProjectID}
	if a.RequestedBy == p.ID && authz.Can(p, requesterRoles, res) {
		return true
	}
	return authz.Can(p, managerRoles, res)
}

// CanWithdraw reports whether the caller could withdraw the approval now, for the UI.
func (s *Service) CanWithdraw(ctx context.Context, p authz.Principal, a Approval) bool {
	return a.State == Pending && s.now().Before(a.ExpiresAt) && s.canWithdraw(ctx, p, a)
}

// CanWithdrawRequest is CanWithdraw for a listed request, from its subject type, project, requester and state.
func (s *Service) CanWithdrawRequest(ctx context.Context, p authz.Principal, subjectType, orgID, projectID, requestedBy, state string) bool {
	return state == Pending && s.canWithdraw(ctx, p, Approval{SubjectType: subjectType, OrgID: orgID, ProjectID: projectID, RequestedBy: requestedBy})
}

// Withdraw closes a pending request before anyone decides it. The requester, or a maintainer or administrator of the
// project, may withdraw it; a decided or expired request is immutable and is refused with ErrDecided. The subject's
// closing handler runs in the same transaction, so a withdrawn base-image or egress request no longer blocks a new one.
func (s *Service) Withdraw(ctx context.Context, p authz.Principal, roles []authz.Role, id, reason, requestID string) (Approval, error) {
	a, err := s.load(ctx, s.db.SQL, id)
	if err != nil {
		return Approval{}, err
	}
	if !s.visible(ctx, p, roles, a) {
		return Approval{}, ErrNotFound
	}
	if !withdrawable[a.SubjectType] {
		return Approval{}, ErrNotWithdrawable
	}
	if !s.canWithdraw(ctx, p, a) {
		return Approval{}, ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 2048 {
		return Approval{}, ErrWithdrawReason
	}
	now := s.now().UTC()
	if a.State != Pending || !now.Before(a.ExpiresAt) {
		return Approval{}, ErrDecided
	}
	why := "withdrawn"
	if reason != "" {
		why += ": " + reason
	}
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE approvals SET state = ?, decided_by = ?, reason = ?, decided_at = ? WHERE id = ? AND state = 'pending'`),
			Withdrawn, p.ID, why, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrDecided
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
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "approval.withdraw", ResourceType: a.SubjectType, ResourceID: a.SubjectID, Outcome: Withdrawn, Reason: why, RequestID: requestID})
	})
	return a, err
}
