package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/ids"
)

// SubjectEgressAccess asks organization administrators to open egress for one project (ADR 0052).
const SubjectEgressAccess = "egress-access-request"

// EgressRequestTTL is how long an egress access request stays open.
const EgressRequestTTL = 14 * 24 * time.Hour

const maxEgressEntries = 20

var (
	// ErrNotEvidenced refuses entries that the stored evidence does not show the build needs.
	ErrNotEvidenced     = errors.New("the evidence does not show that this project needs")
	ErrEgressPending    = errors.New("a request for the same access is already waiting for an administrator")
	ErrAllowanceRevoked = errors.New("the allowance is already revoked")
	ErrRevokeReason     = errors.New("the reason must be 10 to 2048 characters")
)

// EgressEvidence is what the server found in stored data that shows a project needs egress: a readiness item
// predicted from an upload, or the destinations a build's egress proxy denied. Clients never supply its text.
type EgressEvidence struct {
	Kind         string   `json:"kind"`
	BuildID      string   `json:"buildId,omitempty"`
	DiscoveryID  string   `json:"discoveryId,omitempty"`
	SpecID       string   `json:"specId,omitempty"`
	SpecRevision int      `json:"specRevision,omitempty"`
	SnapshotID   string   `json:"snapshotId,omitempty"`
	Code         string   `json:"code,omitempty"`
	Message      string   `json:"message,omitempty"`
	Path         string   `json:"path,omitempty"`
	Line         int      `json:"line,omitempty"`
	Denied       []string `json:"denied,omitempty"`

	// Methods the evidence applies to; empty means any. Presets and Destinations are what it lets a member request.
	Methods      []string `json:"-"`
	Presets      []string `json:"-"`
	Destinations []string `json:"-"`
}

// EgressAccessRequest asks to open presets and public or internal destinations for one project's builds of one method.
type EgressAccessRequest struct {
	ID, OrgID, ProjectID, Method string
	Presets, Destinations        []string
	InternalDestinations         []string
	Justification, RequestedBy   string
	Evidence                     EgressEvidence
	ApprovalID, State, Reason    string
	CreatedAt                    time.Time
}

// EgressAllowance is one opened preset or destination; Status is active, revoked or expired.
type EgressAllowance struct {
	egress.Allowance
	OrgID, ProjectID, RequestID, Reason string
	Status, RequestedBy, ApprovedBy     string
	RevokedBy, RevokeReason             string
	CreatedAt                           time.Time
	ExpiresAt, RevokedAt                *time.Time
}

type egressEntries struct {
	Presets      []string `json:"presets"`
	Destinations []string `json:"destinations"`
	Internal     []string `json:"internal,omitempty"`
}

// registerEgressAccess turns an approved request into allowances in the deciding transaction.
func (s *Service) registerEgressAccess() {
	s.OnDecision(SubjectEgressAccess, func(ctx context.Context, tx *sql.Tx, a Approval) error {
		if a.State == Approved {
			var method, entries, justification, requester string
			if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT method, entries, justification, requested_by FROM egress_access_requests WHERE id = ?`), a.SubjectID).
				Scan(&method, &entries, &justification, &requester); err != nil {
				return err
			}
			var e egressEntries
			if err := json.Unmarshal([]byte(entries), &e); err != nil {
				return err
			}
			methods, _ := json.Marshal([]string{method})
			actor := authz.Principal{ID: a.DecidedBy, OrgID: a.OrgID, Kind: authz.User}
			add := func(kind string, preset, host, port any, internal bool, entry string) error {
				id := ids.New()
				if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO project_egress_allowances (id, org_id, project_id, kind, preset, host, port, methods, reason, request_id, status, requested_by, approved_by, created_at, expires_at, internal)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?)`),
					id, a.OrgID, a.ProjectID, kind, preset, host, port, string(methods), justification, a.SubjectID, requester, a.DecidedBy, s.now().UTC(), utc(a.ValidUntil), internal); err != nil {
					return err
				}
				return audit.Append(ctx, s.db, tx, audit.Event{Actor: actor, Action: "egress.allowance.create", ResourceType: "project-egress-allowance", ResourceID: id, Outcome: "active",
					Reason: fmt.Sprintf("project %s, %s builds: %s (request %s)", a.ProjectID, method, entry, a.SubjectID)})
			}
			for _, p := range e.Presets {
				if err := add("preset", p, nil, nil, false, "preset "+p); err != nil {
					return err
				}
			}
			for _, d := range e.Destinations {
				host, port, _ := net.SplitHostPort(d)
				n, _ := strconv.Atoi(port)
				if err := add("destination", nil, host, n, false, d); err != nil {
					return err
				}
			}
			for _, d := range e.Internal {
				host, port, _ := net.SplitHostPort(d)
				n, _ := strconv.Atoi(port)
				if err := add("destination", nil, host, n, true, "internal "+d); err != nil {
					return err
				}
			}
		}
		if a.State == Pending {
			return nil
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE egress_access_requests SET pending_key = NULL, closed_at = ? WHERE id = ?`), s.now().UTC(), a.SubjectID)
		return err
	})
}

// RequestEgressAccess records a project member's request, after checking every entry against the evidence the
// server found, and a pending approval that only organization administrators other than the requester decide.
func (s *Service) RequestEgressAccess(ctx context.Context, p authz.Principal, roles []authz.Role, projectID, method string, presets, destinations, internal []string, justification string, ev EgressEvidence) (EgressAccessRequest, error) {
	project, err := s.tenancy.GetProject(ctx, p, roles, projectID)
	if err != nil {
		return EgressAccessRequest{}, ErrNotFound
	}
	if !slices.Contains(egress.Methods, method) {
		return EgressAccessRequest{}, invalidf("the build method must be one of %s", strings.Join(egress.Methods, ", "))
	}
	if len(ev.Methods) > 0 && !slices.Contains(ev.Methods, method) {
		return EgressAccessRequest{}, fmt.Errorf("%w access for %s builds", ErrNotEvidenced, method)
	}
	if l := len(strings.TrimSpace(justification)); l < 20 || len(justification) > 2048 {
		return EgressAccessRequest{}, invalidf("the justification must be 20 to 2048 characters")
	}
	if n := len(presets) + len(destinations) + len(internal); n == 0 || n > maxEgressEntries {
		return EgressAccessRequest{}, invalidf("request 1 to %d presets or destinations", maxEgressEntries)
	}
	cat, err := egress.Builtin()
	if err != nil {
		return EgressAccessRequest{}, err
	}
	e := egressEntries{Presets: []string{}, Destinations: []string{}}
	for _, id := range presets {
		if _, ok := cat.Get(id); !ok {
			return EgressAccessRequest{}, invalidf("unknown egress preset %q", id)
		}
		if !slices.Contains(ev.Presets, id) {
			return EgressAccessRequest{}, fmt.Errorf("%w the %s preset", ErrNotEvidenced, id)
		}
		e.Presets = append(e.Presets, id)
	}
	for _, d := range destinations {
		r, err := egress.RequestableDestination(d)
		if err != nil {
			return EgressAccessRequest{}, Invalid(err.Error())
		}
		d = net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
		if !slices.Contains(ev.Destinations, d) {
			return EgressAccessRequest{}, fmt.Errorf("%w %s", ErrNotEvidenced, d)
		}
		e.Destinations = append(e.Destinations, d)
	}
	for _, d := range internal {
		r, err := egress.RequestableInternal(d)
		if err != nil {
			return EgressAccessRequest{}, Invalid(err.Error())
		}
		d = net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
		if !slices.Contains(ev.Destinations, d) {
			return EgressAccessRequest{}, fmt.Errorf("%w %s", ErrNotEvidenced, d)
		}
		if slices.Contains(e.Destinations, d) {
			return EgressAccessRequest{}, invalidf("%s is requested both as public and as internal", d)
		}
		e.Internal = append(e.Internal, d)
	}
	sort.Strings(e.Presets)
	sort.Strings(e.Destinations)
	sort.Strings(e.Internal)
	e.Presets, e.Destinations, e.Internal = slices.Compact(e.Presets), slices.Compact(e.Destinations), slices.Compact(e.Internal)
	active, err := s.ProjectAllowances(ctx, p.OrgID, projectID)
	if err != nil {
		return EgressAccessRequest{}, err
	}
	for _, a := range active {
		if slices.Contains(a.Methods, method) && (slices.Contains(e.Presets, a.Preset) || slices.Contains(e.Destinations, a.Entry()) || slices.Contains(e.Internal, a.Entry())) {
			return EgressAccessRequest{}, fmt.Errorf("%w: %s is already allowed for this project's %s builds", ErrAlreadyAllowed, strings.TrimPrefix(a.Entry(), "preset:"), method)
		}
	}
	entries, _ := json.Marshal(e)
	evidence, _ := json.Marshal(ev)
	key := method + "|" + string(entries)
	r := EgressAccessRequest{ID: ids.New(), OrgID: p.OrgID, ProjectID: projectID, Method: method, Presets: e.Presets, Destinations: e.Destinations, InternalDestinations: e.Internal, Justification: justification,
		RequestedBy: p.ID, Evidence: ev, State: Pending, CreatedAt: s.now().UTC()}
	what := slices.Concat(prefixed("preset ", e.Presets), e.Destinations, prefixed("INTERNAL NETWORK ", e.Internal))
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO egress_access_requests (id, org_id, project_id, method, entries, pending_key, justification, evidence, requested_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			r.ID, r.OrgID, projectID, method, string(entries), key, justification, string(evidence), p.ID, r.CreatedAt); err != nil {
			if isUnique(err) {
				return ErrEgressPending
			}
			return err
		}
		a, err := s.Create(ctx, tx, p.OrgID, projectID, SubjectEgressAccess, r.ID,
			fmt.Sprintf("Open %s for %s builds of project %s only: %s", strings.Join(what, ", "), method, project.Name, justification), p.ID, EgressRequestTTL)
		r.ApprovalID = a.ID
		return err
	})
	if err != nil {
		return EgressAccessRequest{}, err
	}
	return r, nil
}

// Invalid is a request the caller can correct.
type Invalid string

func (e Invalid) Error() string { return string(e) }

func invalidf(format string, args ...any) error { return Invalid(fmt.Sprintf(format, args...)) }

func prefixed(p string, in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, p+s)
	}
	return out
}

// EgressAccessRequests lists a project's egress access requests with the state of their approvals.
func (s *Service) EgressAccessRequests(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string) ([]EgressAccessRequest, error) {
	if !p.IsOrgAdmin() {
		if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
			return nil, ErrNotFound
		}
	}
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT r.id, r.org_id, r.project_id, r.method, r.entries, r.justification, r.evidence, r.requested_by, a.id, a.state, COALESCE(a.reason, ''), r.created_at
		FROM egress_access_requests r JOIN approvals a ON a.subject_type = ? AND a.subject_id = r.id WHERE r.org_id = ? AND r.project_id = ? ORDER BY r.created_at DESC`), SubjectEgressAccess, p.OrgID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EgressAccessRequest{}
	for rows.Next() {
		var r EgressAccessRequest
		var entries, evidence string
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.Method, &entries, &r.Justification, &evidence, &r.RequestedBy, &r.ApprovalID, &r.State, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		var e egressEntries
		_ = json.Unmarshal([]byte(entries), &e)
		_ = json.Unmarshal([]byte(evidence), &r.Evidence)
		r.Presets, r.Destinations, r.InternalDestinations = e.Presets, e.Destinations, e.Internal
		out = append(out, r)
	}
	return out, rows.Err()
}

const allowanceCols = `id, org_id, project_id, COALESCE(preset, ''), COALESCE(host, ''), COALESCE(port, 0), methods, reason, request_id, status, requested_by, approved_by,
	COALESCE(revoked_by, ''), COALESCE(revoke_reason, ''), created_at, expires_at, revoked_at, internal`

func (s *Service) allowances(ctx context.Context, where string, args ...any) ([]EgressAllowance, error) {
	rows, err := s.db.SQL.QueryContext(ctx, s.db.Rebind(`SELECT `+allowanceCols+` FROM project_egress_allowances WHERE `+where+` ORDER BY created_at DESC`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.now()
	out := []EgressAllowance{}
	for rows.Next() {
		var a EgressAllowance
		var methods string
		var expires, revoked sql.NullTime
		if err := rows.Scan(&a.ID, &a.OrgID, &a.ProjectID, &a.Preset, &a.Host, &a.Port, &methods, &a.Reason, &a.RequestID, &a.Status, &a.RequestedBy, &a.ApprovedBy,
			&a.RevokedBy, &a.RevokeReason, &a.CreatedAt, &expires, &revoked, &a.Internal); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(methods), &a.Methods); err != nil {
			return nil, err
		}
		if expires.Valid {
			a.ExpiresAt = &expires.Time
			if a.Status == "active" && !now.Before(expires.Time) {
				a.Status = Expired
			}
		}
		if revoked.Valid {
			a.RevokedAt = &revoked.Time
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ProjectAllowances returns a project's active, unexpired allowances for build planning and admission, without
// authorization.
func (s *Service) ProjectAllowances(ctx context.Context, orgID, projectID string) ([]egress.Allowance, error) {
	all, err := s.allowances(ctx, `org_id = ? AND project_id = ? AND status = 'active'`, orgID, projectID)
	if err != nil {
		return nil, err
	}
	var out []egress.Allowance
	for _, a := range all {
		if a.Status == "active" {
			out = append(out, a.Allowance)
		}
	}
	return out, nil
}

// EgressAllowances lists every allowance of a project, including revoked and expired ones.
func (s *Service) EgressAllowances(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string) ([]EgressAllowance, error) {
	if !p.IsOrgAdmin() {
		if _, err := s.tenancy.GetProject(ctx, p, roles, projectID); err != nil {
			return nil, ErrNotFound
		}
	}
	return s.allowances(ctx, `org_id = ? AND project_id = ?`, p.OrgID, projectID)
}

// RevokeEgressAllowance closes an allowance; builds planned afterwards no longer get it. Only organization
// administrators revoke.
func (s *Service) RevokeEgressAllowance(ctx context.Context, p authz.Principal, id, reason, requestID string) (EgressAllowance, error) {
	list, err := s.allowances(ctx, `id = ? AND org_id = ?`, id, p.OrgID)
	if err != nil {
		return EgressAllowance{}, err
	}
	if len(list) == 0 {
		return EgressAllowance{}, ErrNotFound
	}
	if !p.IsOrgAdmin() {
		return EgressAllowance{}, ErrForbidden
	}
	if l := len(strings.TrimSpace(reason)); l < 10 || len(reason) > 2048 {
		return EgressAllowance{}, ErrRevokeReason
	}
	a := list[0]
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE project_egress_allowances SET status = 'revoked', revoked_by = ?, revoked_at = ?, revoke_reason = ? WHERE id = ? AND status = 'active'`),
			p.ID, s.now().UTC(), reason, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrAllowanceRevoked
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "egress.allowance.revoke", ResourceType: "project-egress-allowance", ResourceID: id, Outcome: "revoked",
			Reason: fmt.Sprintf("project %s: %s: %s", a.ProjectID, strings.TrimPrefix(a.Entry(), "preset:"), reason), RequestID: requestID})
	})
	if err != nil {
		return EgressAllowance{}, err
	}
	list, err = s.allowances(ctx, `id = ?`, id)
	if err != nil || len(list) == 0 {
		return EgressAllowance{}, err
	}
	return list[0], nil
}
