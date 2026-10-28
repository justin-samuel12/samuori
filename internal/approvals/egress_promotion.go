package approvals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"samuori.invalid/samuori/internal/audit"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/egress"
)

// EgressPolicy is the organization egress policy that promoted allowances extend
// (docs/architecture/egress.md#promoting-project-allowances).
type EgressPolicy interface {
	EnsureOrg(ctx context.Context, orgID string) error
	AddEgressDestination(ctx context.Context, tx *sql.Tx, actor authz.Principal, d egress.Destination, reason string) (int, error)
}

// UseEgressPolicy connects allowance promotion to the policy it extends.
func (s *Service) UseEgressPolicy(p EgressPolicy) { s.egress = p }

var ErrPromoteReason = errors.New("the reason must be 10 to 1024 characters")

// DestinationGrant is one destination opened for projects by separate allowances.
type DestinationGrant struct {
	Host       string
	Port       int
	Internal   bool
	Allowances []EgressAllowance
}

// GrantedDestinations groups the organization's active destination allowances by destination and network, most
// projects first, and lists active preset allowances separately. Organization administrators only.
func (s *Service) GrantedDestinations(ctx context.Context, p authz.Principal) ([]DestinationGrant, []EgressAllowance, error) {
	if !p.IsOrgAdmin() {
		return nil, nil, ErrForbidden
	}
	all, err := s.allowances(ctx, `org_id = ? AND status = 'active'`, p.OrgID)
	if err != nil {
		return nil, nil, err
	}
	groups := map[string]*DestinationGrant{}
	var order []string
	presets := []EgressAllowance{}
	for _, a := range all {
		if a.Status != "active" {
			continue
		}
		if a.Preset != "" {
			presets = append(presets, a)
			continue
		}
		k := fmt.Sprintf("%s|%d|%t", a.Host, a.Port, a.Internal)
		if groups[k] == nil {
			groups[k] = &DestinationGrant{Host: a.Host, Port: a.Port, Internal: a.Internal}
			order = append(order, k)
		}
		groups[k].Allowances = append(groups[k].Allowances, a)
	}
	out := make([]DestinationGrant, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].Allowances) != len(out[j].Allowances) {
			return len(out[i].Allowances) > len(out[j].Allowances)
		}
		return out[i].Host < out[j].Host
	})
	return out, presets, nil
}

// Promotion is the outcome of moving allowances into the organization egress policy.
type Promotion struct {
	PolicyVersion int
	Revoked       []EgressAllowance
}

// PromoteDestination adds host:port to the organization egress policy for every build method (or every method but
// Dockerfile builds) and revokes the project allowances it replaces, in one transaction. Any organization
// administrator may do it, as they may edit the policy; it is audited.
func (s *Service) PromoteDestination(ctx context.Context, p authz.Principal, host string, port int, internal, noDockerfile bool, reason, requestID string) (Promotion, error) {
	if !p.IsOrgAdmin() {
		return Promotion{}, ErrForbidden
	}
	if s.egress == nil {
		return Promotion{}, errors.New("the egress policy is not connected")
	}
	if l := len(strings.TrimSpace(reason)); l < 10 || len(reason) > 1024 {
		return Promotion{}, ErrPromoteReason
	}
	all, err := s.allowances(ctx, `org_id = ? AND status = 'active' AND host = ? AND port = ?`, p.OrgID, host, port)
	if err != nil {
		return Promotion{}, err
	}
	var replaced []EgressAllowance
	for _, a := range all {
		if a.Status == "active" && a.Internal == internal {
			replaced = append(replaced, a)
		}
	}
	if len(replaced) == 0 {
		return Promotion{}, ErrNotFound
	}
	network := egress.NetworkPublic
	if internal {
		network = egress.NetworkInternal
	}
	if err := s.egress.EnsureOrg(ctx, p.OrgID); err != nil {
		return Promotion{}, err
	}
	dest := net.JoinHostPort(host, strconv.Itoa(port))
	var out Promotion
	err = s.db.InTx(ctx, func(tx *sql.Tx) error {
		v, err := s.egress.AddEgressDestination(ctx, tx, p, egress.Destination{Host: host, Port: port, Reason: reason, Network: network, NoDockerfile: noDockerfile}, reason)
		if err != nil {
			return err
		}
		out.PolicyVersion = v
		why := fmt.Sprintf("Moved into the organization egress policy, version %d: %s", v, reason)
		for _, a := range replaced {
			res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE project_egress_allowances SET status = 'revoked', revoked_by = ?, revoked_at = ?, revoke_reason = ? WHERE id = ? AND status = 'active'`),
				p.ID, s.now().UTC(), why, a.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrAllowanceRevoked
			}
			if err := audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "egress.allowance.revoke", ResourceType: "project-egress-allowance", ResourceID: a.ID, Outcome: "revoked",
				Reason: fmt.Sprintf("project %s: %s: %s", a.ProjectID, a.Entry(), why), RequestID: requestID}); err != nil {
				return err
			}
		}
		scope := "every build method"
		if noDockerfile {
			scope = "every build method except Dockerfile builds"
		}
		return audit.Append(ctx, s.db, tx, audit.Event{Actor: p, Action: "egress.allowance.promote", ResourceType: "egress-policy", ResourceID: dest, Outcome: "promoted",
			Reason: fmt.Sprintf("%s (%s) opened for %s in policy version %d, replacing %d project allowance(s): %s", dest, network, scope, v, len(replaced), reason), RequestID: requestID})
	})
	if err != nil {
		return Promotion{}, err
	}
	for _, a := range replaced {
		if list, err := s.allowances(ctx, `id = ?`, a.ID); err == nil && len(list) == 1 {
			out.Revoked = append(out.Revoked, list[0])
		}
	}
	return out, nil
}
