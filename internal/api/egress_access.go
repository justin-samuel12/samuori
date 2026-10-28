package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"samuori.invalid/samuori/internal/admission"
	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/approvals"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/builds"
	"samuori.invalid/samuori/internal/discovery"
	"samuori.invalid/samuori/internal/discovery/jobs"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/sources"
	"samuori.invalid/samuori/internal/specs"
)

type egressRequestView struct {
	ID            string                   `json:"id"`
	ProjectID     string                   `json:"projectId"`
	Method        string                   `json:"method"`
	Presets       []string                 `json:"presets"`
	Destinations  []string                 `json:"destinations"`
	Internal      []string                 `json:"internalDestinations"`
	Justification string                   `json:"justification"`
	Evidence      approvals.EgressEvidence `json:"evidence"`
	ApprovalID    string                   `json:"approvalId"`
	State         string                   `json:"state"`
	Reason        string                   `json:"reason,omitempty"`
	CanWithdraw   bool                     `json:"canWithdraw"`
	CreatedAt     time.Time                `json:"createdAt"`
}

func (a *API) toEgressRequest(r *http.Request, x approvals.EgressAccessRequest) egressRequestView {
	v := egressRequestView{CanWithdraw: a.approvals.CanWithdrawRequest(r.Context(), principal(r), approvals.SubjectEgressAccess, x.OrgID, x.ProjectID, x.RequestedBy, x.State), ID: x.ID, ProjectID: x.ProjectID, Method: x.Method, Presets: x.Presets, Destinations: x.Destinations, Internal: x.InternalDestinations, Justification: x.Justification,
		Evidence: x.Evidence, ApprovalID: x.ApprovalID, State: x.State, Reason: x.Reason, CreatedAt: x.CreatedAt}
	if v.Presets == nil {
		v.Presets = []string{}
	}
	if v.Destinations == nil {
		v.Destinations = []string{}
	}
	if v.Internal == nil {
		v.Internal = []string{}
	}
	return v
}

type egressAllowanceView struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"projectId"`
	Kind         string     `json:"kind"`
	Preset       string     `json:"preset,omitempty"`
	Destination  string     `json:"destination,omitempty"`
	Network      string     `json:"network,omitempty"`
	Methods      []string   `json:"methods"`
	Reason       string     `json:"reason"`
	RequestID    string     `json:"requestId"`
	Status       string     `json:"status"`
	RequestedBy  string     `json:"requestedBy"`
	ApprovedBy   string     `json:"approvedBy"`
	CreatedAt    time.Time  `json:"createdAt"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	RevokedBy    string     `json:"revokedBy,omitempty"`
	RevokedAt    *time.Time `json:"revokedAt,omitempty"`
	RevokeReason string     `json:"revokeReason,omitempty"`
}

func toEgressAllowance(x approvals.EgressAllowance) egressAllowanceView {
	v := egressAllowanceView{ID: x.ID, ProjectID: x.ProjectID, Kind: "preset", Preset: x.Preset, Methods: x.Methods, Reason: x.Reason, RequestID: x.RequestID, Status: x.Status,
		RequestedBy: x.RequestedBy, ApprovedBy: x.ApprovedBy, CreatedAt: x.CreatedAt, ExpiresAt: x.ExpiresAt, RevokedBy: x.RevokedBy, RevokedAt: x.RevokedAt, RevokeReason: x.RevokeReason}
	if x.Host != "" {
		v.Kind, v.Destination, v.Network = "destination", x.Entry(), "public"
		if x.Internal {
			v.Network = "internal"
		}
	}
	return v
}

func (a *API) ListEgressAccessRequests(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.ListEgressAccessRequestsParams) {
	list, err := a.approvals.EgressAccessRequests(r.Context(), principal(r), operationRoles(r.Context()), projectID)
	if err != nil {
		a.approvalError(w, r, err)
		return
	}
	out := struct {
		Items      []egressRequestView `json:"items"`
		NextCursor *string             `json:"nextCursor,omitempty"`
	}{Items: []egressRequestView{}}
	for _, x := range list {
		out.Items = append(out.Items, a.toEgressRequest(r, x))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x egressRequestView) string { return x.ID }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) ListEgressAllowances(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.ListEgressAllowancesParams) {
	list, err := a.approvals.EgressAllowances(r.Context(), principal(r), operationRoles(r.Context()), projectID)
	if err != nil {
		a.approvalError(w, r, err)
		return
	}
	out := struct {
		Items      []egressAllowanceView `json:"items"`
		NextCursor *string               `json:"nextCursor,omitempty"`
	}{Items: []egressAllowanceView{}}
	for _, x := range list {
		out.Items = append(out.Items, toEgressAllowance(x))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x egressAllowanceView) string { return x.ID }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) RequestEgressAccess(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, _ apigen.RequestEgressAccessParams) {
	var in apigen.EgressAccessRequestCreate
	if !decodeBody(w, r, &in) {
		return
	}
	p, roles := principal(r), operationRoles(r.Context())
	ev, err := a.egressEvidence(r.Context(), p, roles, projectID, in.Evidence)
	if err == nil {
		method := ""
		if in.Method != nil {
			method = string(*in.Method)
		} else if len(ev.Methods) == 1 {
			method = ev.Methods[0]
		}
		var internal []string
		if in.InternalDestinations != nil {
			internal = *in.InternalDestinations
		}
		var x approvals.EgressAccessRequest
		if x, err = a.approvals.RequestEgressAccess(r.Context(), p, roles, projectID, method, in.Presets, in.Destinations, internal, in.Justification, ev); err == nil {
			writeJSON(w, http.StatusAccepted, a.toEgressRequest(r, x))
			return
		}
	}
	var invalid approvals.Invalid
	switch {
	case errors.Is(err, approvals.ErrNotFound), errors.Is(err, errEvidenceNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, approvals.ErrAlreadyAllowed):
		apigen.WriteProblem(w, http.StatusConflict, "already_allowed", err.Error())
	case errors.Is(err, approvals.ErrEgressPending):
		apigen.WriteProblem(w, http.StatusConflict, "request_pending", err.Error())
	case errors.Is(err, approvals.ErrNotEvidenced):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "not_evidenced", err.Error())
	case errors.As(err, &invalid):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) RevokeEgressAllowance(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RevokeEgressAllowanceParams) {
	var in apigen.EgressAllowanceRevocation
	if !decodeBody(w, r, &in) {
		return
	}
	x, err := a.approvals.RevokeEgressAllowance(r.Context(), principal(r), id, in.Reason, requestID(w))
	switch {
	case errors.Is(err, approvals.ErrAllowanceRevoked):
		apigen.WriteProblem(w, http.StatusConflict, "revoked", err.Error())
	case errors.Is(err, approvals.ErrRevokeReason):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	case errors.Is(err, approvals.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators revoke egress allowances")
	case err != nil:
		a.approvalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, toEgressAllowance(x))
	}
}

type grantedDestinationView struct {
	Destination string                `json:"destination"`
	Host        string                `json:"host"`
	Port        int                   `json:"port"`
	Network     string                `json:"network"`
	Allowances  []egressAllowanceView `json:"allowances"`
}

func networkName(internal bool) string {
	if internal {
		return egress.NetworkInternal
	}
	return egress.NetworkPublic
}

func (a *API) ListGrantedDestinations(w http.ResponseWriter, r *http.Request) {
	groups, presets, err := a.approvals.GrantedDestinations(r.Context(), principal(r))
	if errors.Is(err, approvals.ErrForbidden) {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators see every project's egress allowances")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Destinations []grantedDestinationView `json:"destinations"`
		Presets      []egressAllowanceView    `json:"presets"`
	}{Destinations: []grantedDestinationView{}, Presets: []egressAllowanceView{}}
	for _, g := range groups {
		v := grantedDestinationView{Destination: net.JoinHostPort(g.Host, strconv.Itoa(g.Port)), Host: g.Host, Port: g.Port, Network: networkName(g.Internal), Allowances: []egressAllowanceView{}}
		for _, x := range g.Allowances {
			v.Allowances = append(v.Allowances, toEgressAllowance(x))
		}
		out.Destinations = append(out.Destinations, v)
	}
	for _, x := range presets {
		out.Presets = append(out.Presets, toEgressAllowance(x))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) PromoteEgressDestination(w http.ResponseWriter, r *http.Request, _ apigen.PromoteEgressDestinationParams) {
	var in apigen.EgressPromotionRequest
	if !decodeBody(w, r, &in) {
		return
	}
	noDockerfile := in.NoDockerfile != nil && *in.NoDockerfile
	x, err := a.approvals.PromoteDestination(r.Context(), principal(r), in.Host, in.Port, in.Network == apigen.EgressPromotionRequestNetworkInternal, noDockerfile, in.Reason, requestID(w))
	switch {
	case errors.Is(err, approvals.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators change the egress policy")
	case errors.Is(err, approvals.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "no active project allowance opens this destination")
	case errors.Is(err, approvals.ErrPromoteReason):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	case errors.Is(err, policy.ErrDestinationConflict), errors.Is(err, approvals.ErrAllowanceRevoked):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", err.Error())
	case err != nil:
		var invalid *policy.Invalid
		if errors.As(err, &invalid) {
			apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
			return
		}
		a.internalError(w, r, err)
	default:
		out := struct {
			PolicyVersion int                   `json:"policyVersion"`
			Revoked       []egressAllowanceView `json:"revoked"`
		}{PolicyVersion: x.PolicyVersion, Revoked: []egressAllowanceView{}}
		for _, y := range x.Revoked {
			out.Revoked = append(out.Revoked, toEgressAllowance(y))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

var errEvidenceNotFound = errors.New("evidence not found")

// egressEvidence recomputes the evidence a request names from stored data: the denials a build's egress proxy
// reported, or a readiness item of a discovery or admission check. Only what it shows can be requested (ADR 0052).
func (a *API) egressEvidence(ctx context.Context, p authz.Principal, roles []authz.Role, projectID string, ref apigen.EgressEvidenceRef) (approvals.EgressEvidence, error) {
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	notFound := func(err error) error {
		for _, nf := range []error{builds.ErrNotFound, jobs.ErrNotFound, specs.ErrNotFound, sources.ErrNotFound, sources.ErrDeleted} {
			if errors.Is(err, nf) {
				return errEvidenceNotFound
			}
		}
		return err
	}
	foreign := approvals.Invalid("the evidence belongs to another project")
	if id := str(ref.BuildId); id != "" {
		run, method, denied, err := a.builds.EgressEvidence(ctx, p, roles, id)
		if err != nil {
			return approvals.EgressEvidence{}, notFound(err)
		}
		if run.ProjectID != projectID {
			return approvals.EgressEvidence{}, foreign
		}
		if method == "" {
			return approvals.EgressEvidence{}, approvals.Invalid("the build has no plan, so it reached no network")
		}
		ev := approvals.EgressEvidence{Kind: "build", BuildID: run.ID, Methods: []string{method}, Denied: []string{}}
		for _, d := range denied {
			dest := net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
			ev.Denied = append(ev.Denied, fmt.Sprintf("%s (%s)", dest, d.Reason))
			if d.Requestable {
				ev.Destinations = append(ev.Destinations, dest)
			}
		}
		return ev, nil
	}
	code, path, line := str(ref.Code), str(ref.Path), 0
	if ref.Line != nil {
		line = *ref.Line
	}
	if code == "" {
		return approvals.EgressEvidence{}, approvals.Invalid("name a build, or a readiness item by its code, path and line")
	}
	type candidate struct {
		item    discovery.Readiness
		methods []string
	}
	var items []candidate
	ev := approvals.EgressEvidence{Kind: "readiness", Code: code, Path: path, Line: line}
	switch {
	case str(ref.DiscoveryId) != "":
		d, err := a.discovery.Get(ctx, p, roles, *ref.DiscoveryId)
		if err != nil {
			return approvals.EgressEvidence{}, notFound(err)
		}
		if d.ProjectID != projectID {
			return approvals.EgressEvidence{}, foreign
		}
		if d.Status != jobs.Completed {
			return approvals.EgressEvidence{}, approvals.Invalid("the discovery has not completed")
		}
		eff, err := a.policies.Effective(ctx, p.OrgID)
		if err != nil {
			return approvals.EgressEvidence{}, err
		}
		project, err := a.approvals.ProjectAllowances(ctx, p.OrgID, projectID)
		if err != nil {
			return approvals.EgressEvidence{}, err
		}
		if err := admission.DiscoveryReadiness(&d.Result, eff.Egress, project); err != nil {
			return approvals.EgressEvidence{}, err
		}
		for _, df := range d.Result.Dockerfiles {
			for _, it := range df.Readiness {
				items = append(items, candidate{it, []string{"dockerfile"}})
			}
		}
		for _, f := range d.Result.Findings {
			for _, it := range f.Readiness {
				items = append(items, candidate{it, nil})
			}
		}
		ev.DiscoveryID, ev.SnapshotID = d.ID, d.SnapshotID
	case str(ref.SpecId) != "" && ref.SpecRevision != nil && str(ref.SnapshotId) != "":
		sp, rev, err := a.specs.Revision(ctx, p, roles, *ref.SpecId, *ref.SpecRevision)
		if err != nil {
			return approvals.EgressEvidence{}, notFound(err)
		}
		if sp.ProjectID != projectID {
			return approvals.EgressEvidence{}, foreign
		}
		var doc struct {
			Spec struct {
				Build struct {
					Method string `json:"method"`
				} `json:"build"`
			} `json:"spec"`
		}
		_ = json.Unmarshal(rev.Document, &doc)
		rep, err := a.admission.Check(ctx, p, roles, *ref.SpecId, *ref.SpecRevision, *ref.SnapshotId)
		if err != nil {
			return approvals.EgressEvidence{}, notFound(err)
		}
		for _, it := range rep.Readiness {
			items = append(items, candidate{it, []string{doc.Spec.Build.Method}})
		}
		ev.SpecID, ev.SpecRevision, ev.SnapshotID = sp.ID, *ref.SpecRevision, *ref.SnapshotId
	default:
		return approvals.EgressEvidence{}, approvals.Invalid("name the discovery, or the spec revision and upload, whose readiness item shows the need")
	}
	found := false
	for _, c := range items {
		it := c.item
		if it.Code != code || it.Path != path || it.Line != line || it.Requestable == nil {
			continue
		}
		if !found {
			ev.Message, ev.Methods = it.Message, c.methods
		} else if c.methods == nil {
			ev.Methods = nil
		}
		found = true
		ev.Presets = append(ev.Presets, it.Requestable.Presets...)
		ev.Destinations = append(ev.Destinations, it.Requestable.Destinations...)
	}
	if !found {
		return approvals.EgressEvidence{}, approvals.Invalid("no current readiness item with that code, path and line asks for egress a project member can request")
	}
	return ev, nil
}
