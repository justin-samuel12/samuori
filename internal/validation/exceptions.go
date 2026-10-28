package validation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/ids"
	"samuori.invalid/samuori/internal/imagescan"
)

// Exception lets require-exception findings of a project pass, for paths matching its patterns, until it expires
// (ADR 0031). An image that relies on one is classified restricted.
type Exception struct {
	ID, OrgID, ProjectID   string
	Classes, PathPatterns  []string
	Justification, State   string
	RequestedBy, DecidedBy string
	DecisionReason         string
	ExpiresAt, CreatedAt   time.Time
	DecidedAt              *time.Time
}

// MaxExceptionLifetime bounds how long an exception can stay in force.
const MaxExceptionLifetime = 90 * 24 * time.Hour

var (
	ErrExceptionInvalid = errors.New("an exception needs path patterns, a justification of at least 20 characters and an expiry within 90 days")
	ErrSelfApproval     = errors.New("an exception must be decided by someone other than the requester")
	ErrDecided          = errors.New("the exception was already decided")
)

// exceptionClasses are the classes whose findings can be excepted: everything that defaults to or may be set to
// require-exception. A block outcome is never excepted, whatever its class.
var exceptionClasses = []string{imagescan.ClassDatabaseSecrets, imagescan.ClassDatabaseData, imagescan.ClassCredentialFile, imagescan.ClassCredentialContent, imagescan.ClassMetadata, imagescan.ClassPrivateKey}

func (e Exception) covers(f Finding, now time.Time) bool {
	if e.State != "approved" || !now.Before(e.ExpiresAt) {
		return false
	}
	if len(e.Classes) > 0 && !slices.Contains(e.Classes, f.Class) {
		return false
	}
	for _, p := range e.PathPatterns {
		if imagescan.Glob(p, f.Path) {
			return true
		}
	}
	return false
}

// RequestException records a request by a maintainer of the project; an administrator decides it.
func (s *Service) RequestException(ctx context.Context, actor authz.Principal, roles []authz.Role, projectID string, classes, patterns []string, justification string, lifetime time.Duration, requestID string) (Exception, error) {
	proj, err := s.tenancy.GetProject(ctx, actor, roles, projectID)
	if err != nil {
		return Exception{}, err
	}
	if !authz.Can(actor, []authz.Role{authz.Maintainer, authz.Administrator}, authz.Resource{OrgID: actor.OrgID, TeamID: proj.TeamID, ProjectID: proj.ID}) {
		return Exception{}, ErrForbidden
	}
	if len(patterns) == 0 || len(patterns) > 32 || len(strings.TrimSpace(justification)) < 20 || lifetime <= 0 || lifetime > MaxExceptionLifetime {
		return Exception{}, ErrExceptionInvalid
	}
	// Patterns are image paths such as var/opt/mssql/secrets/**; one that matches everything is refused.
	for _, p := range patterns {
		if p == "" || p == "**" || p == "*" || strings.HasPrefix(p, "/") || slices.Contains(strings.Split(p, "/"), "..") {
			return Exception{}, ErrExceptionInvalid
		}
	}
	for _, c := range classes {
		if !slices.Contains(exceptionClasses, c) {
			return Exception{}, ErrExceptionInvalid
		}
	}
	now := s.now().UTC()
	e := Exception{ID: ids.New(), OrgID: actor.OrgID, ProjectID: projectID, Classes: append([]string{}, classes...), PathPatterns: patterns, Justification: justification,
		State: "requested", RequestedBy: actor.ID, ExpiresAt: now.Add(lifetime), CreatedAt: now}
	cl, _ := json.Marshal(e.Classes)
	pp, _ := json.Marshal(e.PathPatterns)
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO output_exceptions (id, org_id, project_id, classes, path_patterns, justification, state, requested_by, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			e.ID, e.OrgID, projectID, string(cl), string(pp), justification, e.State, actor.ID, e.ExpiresAt, now); err != nil {
			return err
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "exception.request", ResourceType: "output_exception", ResourceID: e.ID, Outcome: "succeeded", Reason: strings.Join(patterns, ", "), RequestID: requestID})
	})
	return e, err
}

// DecideException approves or rejects a request. Only administrators other than the requester decide.
func (s *Service) DecideException(ctx context.Context, actor authz.Principal, id string, approve bool, reason, requestID string) (Exception, error) {
	if !actor.IsOrgAdmin() {
		return Exception{}, ErrForbidden
	}
	e, err := s.exception(ctx, actor.OrgID, id)
	if err != nil {
		return Exception{}, err
	}
	if e.State != "requested" {
		return Exception{}, ErrDecided
	}
	if e.RequestedBy == actor.ID {
		return Exception{}, ErrSelfApproval
	}
	state := "rejected"
	if approve {
		state = "approved"
	}
	now := s.now().UTC()
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE output_exceptions SET state = ?, decided_by = ?, decision_reason = ?, decided_at = ? WHERE id = ? AND state = 'requested'`), state, actor.ID, reason, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrDecided
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "exception." + state, ResourceType: "output_exception", ResourceID: id, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Exception{}, err
	}
	return s.exception(ctx, actor.OrgID, id)
}

// RevokeException ends an exception early. Images that relied on it fail their next validation, including the
// revalidation at promotion.
func (s *Service) RevokeException(ctx context.Context, actor authz.Principal, id, reason, requestID string) (Exception, error) {
	if !actor.IsOrgAdmin() {
		return Exception{}, ErrForbidden
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE output_exceptions SET state = 'revoked', decided_by = ?, decision_reason = ?, decided_at = ? WHERE id = ? AND org_id = ? AND state IN ('requested', 'approved')`), actor.ID, reason, s.now().UTC(), id, actor.OrgID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "exception.revoke", ResourceType: "output_exception", ResourceID: id, Outcome: "succeeded", Reason: reason, RequestID: requestID})
	})
	if err != nil {
		return Exception{}, err
	}
	return s.exception(ctx, actor.OrgID, id)
}

// Exceptions lists a project's exceptions for anyone who can see the project.
func (s *Service) Exceptions(ctx context.Context, actor authz.Principal, roles []authz.Role, projectID string) ([]Exception, error) {
	if _, err := s.tenancy.GetProject(ctx, actor, roles, projectID); err != nil {
		return nil, err
	}
	return s.exceptions(ctx, `project_id = ?`, projectID)
}

// PendingExceptions lists requests waiting for an administrator.
func (s *Service) PendingExceptions(ctx context.Context, actor authz.Principal) ([]Exception, error) {
	if !actor.IsOrgAdmin() {
		return nil, ErrForbidden
	}
	return s.exceptions(ctx, `org_id = ? AND state = 'requested'`, actor.OrgID)
}

func (s *Service) exception(ctx context.Context, orgID, id string) (Exception, error) {
	list, err := s.exceptions(ctx, `org_id = ? AND id = ?`, orgID, id)
	if err != nil {
		return Exception{}, err
	}
	if len(list) == 0 {
		return Exception{}, ErrNotFound
	}
	return list[0], nil
}

func (s *Service) exceptions(ctx context.Context, where string, args ...any) ([]Exception, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT id, org_id, project_id, classes, path_patterns, justification, state, requested_by, decided_by, decision_reason, expires_at, created_at, decided_at FROM output_exceptions WHERE `+where+` ORDER BY created_at DESC`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Exception
	for rows.Next() {
		var e Exception
		var cl, pp string
		var decided sql.NullTime
		if err := rows.Scan(&e.ID, &e.OrgID, &e.ProjectID, &cl, &pp, &e.Justification, &e.State, &e.RequestedBy, &e.DecidedBy, &e.DecisionReason, &e.ExpiresAt, &e.CreatedAt, &decided); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(cl), &e.Classes)
		_ = json.Unmarshal([]byte(pp), &e.PathPatterns)
		if decided.Valid {
			e.DecidedAt = &decided.Time
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
