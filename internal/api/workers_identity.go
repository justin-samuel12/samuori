package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/artifacts"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/pki"
	"samuori.invalid/samuori/internal/workers"
)

// Identity is the part of the worker channel that manages worker identities and the CA (ADR 0026).
type Identity interface {
	ApplyAction(ctx context.Context, actor authz.Principal, id, action, reason string, compromisedSince *time.Time, requestID string) (workers.Worker, error)
	Rotation(ctx context.Context) (workers.Rotation, error)
	BeginRotation(ctx context.Context, actor authz.Principal, requestID string) (workers.Rotation, error)
	SwitchRotation(ctx context.Context, actor authz.Principal, requestID string) (workers.Rotation, error)
	EmergencyRotation(ctx context.Context, actor authz.Principal, reason, requestID string) (workers.Rotation, error)
}

func (a *API) identity(w http.ResponseWriter) (Identity, bool) {
	id, ok := a.channel.(Identity)
	if !ok || a.channel == nil {
		apigen.WriteProblem(w, http.StatusConflict, "no_worker_channel", "this controller has no worker endpoint")
	}
	return id, ok
}

func (a *API) ApplyWorkerAction(w http.ResponseWriter, r *http.Request, id apigen.WorkerId, _ apigen.ApplyWorkerActionParams) {
	var in apigen.WorkerAction
	if !decodeBody(w, r, &in) {
		return
	}
	if in.CompromisedSince != nil && in.Action != "revoke" {
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", "compromisedSince applies to revoke only")
		return
	}
	ident, ok := a.identity(w)
	if !ok {
		return
	}
	if (in.Action == "revoke" || in.Action == "remove") && !principal(r).IsOrgAdmin() {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators revoke or remove workers")
		return
	}
	wk, err := ident.ApplyAction(r.Context(), principal(r), id, string(in.Action), in.Reason, in.CompromisedSince, requestID(w))
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	pools, err := a.poolIndex(r)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.toWorker(r, wk, pools))
}

func (a *API) UpdateWorkerPool(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateWorkerPoolParams) {
	var in apigen.WorkerPoolUpdate
	if !decodeBody(w, r, &in) {
		return
	}
	u := workers.PoolUpdate{Name: in.Name, Location: in.Location, TrustDomain: in.TrustDomain, TrustedWorkloadsOnly: in.TrustedWorkloadsOnly}
	if in.IsolationProfile != nil {
		p := string(*in.IsolationProfile)
		u.IsolationProfile = &p
	}
	before, err := a.workers.Pool(r.Context(), principal(r).OrgID, id)
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	p, err := a.workers.UpdatePool(r.Context(), principal(r), id, u, requestID(w))
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	if p.ProfileApproved && p.IsolationProfile != before.IsolationProfile && a.channel != nil {
		if _, err := a.channel.RequestSelfTest(r.Context(), principal(r), id, requestID(w)); err != nil && !errors.Is(err, workers.ErrNoSelfTestWorker) && !errors.Is(err, workers.ErrSelfTestRunning) {
			a.log.Warn("self-tests after a profile change", "pool", id, "error", err)
		}
	}
	list, err := a.workers.List(r.Context(), principal(r).OrgID, "", 10_000)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toPool(p, list))
}

func (a *API) DeleteWorkerPool(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeleteWorkerPoolParams) {
	if err := a.workers.DeletePool(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.workersError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type locationView struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
}

func (a *API) ListLocations(w http.ResponseWriter, r *http.Request) {
	list, err := a.workers.Locations(r.Context(), principal(r).OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Items []locationView `json:"items"`
	}{Items: []locationView{}}
	for _, l := range list {
		out.Items = append(out.Items, locationView{l.Name, l.Description, l.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateLocation(w http.ResponseWriter, r *http.Request, _ apigen.CreateLocationParams) {
	var in apigen.LocationCreate
	if !decodeBody(w, r, &in) {
		return
	}
	desc := ""
	if in.Description != nil {
		desc = *in.Description
	}
	l, err := a.workers.CreateLocation(r.Context(), principal(r), in.Name, desc, requestID(w))
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, locationView{l.Name, l.Description, l.CreatedAt})
}

func (a *API) DeleteLocation(w http.ResponseWriter, r *http.Request, name string, _ apigen.DeleteLocationParams) {
	list, err := a.workers.Locations(r.Context(), principal(r).OrgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	for _, l := range list {
		if l.Name == name {
			if err := a.workers.DeleteLocation(r.Context(), principal(r), l.ID, requestID(w)); err != nil {
				a.workersError(w, r, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	a.workersError(w, r, workers.ErrNotFound)
}

// GetProjectLocations is visible to anyone who can see the project; other projects stay hidden (404).
func (a *API) GetProjectLocations(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	if _, err := a.tenancy.GetProject(r.Context(), principal(r), operationRoles(r.Context()), id); err != nil {
		a.tenancyError(w, r, err)
		return
	}
	list, err := a.workers.ProjectLocations(r.Context(), id)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.ProjectLocations{Locations: nonNil(list)})
}

func (a *API) SetProjectLocations(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.SetProjectLocationsParams) {
	var in apigen.ProjectLocations
	if !decodeBody(w, r, &in) {
		return
	}
	list, err := a.workers.SetProjectLocations(r.Context(), principal(r), id, in.Locations, requestID(w))
	if err != nil {
		a.workersError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.ProjectLocations{Locations: nonNil(list)})
}

type anchorView struct {
	Pin         string     `json:"pin"`
	State       string     `json:"state"`
	NotBefore   time.Time  `json:"notBefore"`
	NotAfter    time.Time  `json:"notAfter"`
	RetireAfter *time.Time `json:"retireAfter,omitempty"`
}

type pendingView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	BundleVersion uint64 `json:"bundleVersion"`
}

type bundleView struct {
	BundleVersion      uint64        `json:"bundleVersion"`
	IssuingPin         string        `json:"issuingPin"`
	RotationInProgress bool          `json:"rotationInProgress"`
	Anchors            []anchorView  `json:"anchors"`
	PendingWorkers     []pendingView `json:"pendingWorkers"`
}

func toBundle(rot workers.Rotation, orgID string) bundleView {
	v := bundleView{BundleVersion: rot.BundleVersion, RotationInProgress: rot.InProgress, Anchors: []anchorView{}, PendingWorkers: []pendingView{}}
	for _, an := range rot.Anchors {
		if an.State == pki.StateIssuing {
			v.IssuingPin = an.Pin
		}
		v.Anchors = append(v.Anchors, anchorView{an.Pin, an.State, an.NotBefore, an.NotAfter, an.RetireAfter})
	}
	for _, p := range rot.Pending {
		if p.OrgID == orgID {
			v.PendingWorkers = append(v.PendingWorkers, pendingView{p.ID, p.Name, p.BundleVersion})
		}
	}
	return v
}

func (a *API) GetTrustBundle(w http.ResponseWriter, r *http.Request) {
	ident, ok := a.identity(w)
	if !ok {
		return
	}
	rot, err := ident.Rotation(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBundle(rot, principal(r).OrgID))
}

func (a *API) StartCaRotation(w http.ResponseWriter, r *http.Request, _ apigen.StartCaRotationParams) {
	var in apigen.CaRotation
	if !decodeBody(w, r, &in) {
		return
	}
	ident, ok := a.identity(w)
	if !ok {
		return
	}
	var rot workers.Rotation
	var err error
	if in.Mode == "emergency" {
		rot, err = ident.EmergencyRotation(r.Context(), principal(r), in.Reason, requestID(w))
	} else {
		rot, err = ident.BeginRotation(r.Context(), principal(r), requestID(w))
	}
	if a.rotationError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, toBundle(rot, principal(r).OrgID))
}

func (a *API) SwitchCaRotation(w http.ResponseWriter, r *http.Request, _ apigen.SwitchCaRotationParams) {
	ident, ok := a.identity(w)
	if !ok {
		return
	}
	rot, err := ident.SwitchRotation(r.Context(), principal(r), requestID(w))
	if errors.Is(err, workers.ErrRotationBlocked) {
		apigen.WriteProblem(w, http.StatusConflict, "rotation_blocked", "workers have not acknowledged the new trust bundle; see pendingWorkers in GET /pki")
		return
	}
	if a.rotationError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, toBundle(rot, principal(r).OrgID))
}

func (a *API) rotationError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, pki.ErrRotationInProgress), errors.Is(err, pki.ErrNoRotation):
		apigen.WriteProblem(w, http.StatusConflict, "rotation_state", err.Error())
	default:
		a.workersError(w, r, err)
	}
	return true
}

type quarantineView struct {
	ID            string     `json:"id"`
	Digest        string     `json:"digest"`
	WorkerID      string     `json:"workerId"`
	Reason        string     `json:"reason"`
	CreatedAt     time.Time  `json:"createdAt"`
	ReleasedAt    *time.Time `json:"releasedAt,omitempty"`
	ReleasedBy    string     `json:"releasedBy,omitempty"`
	ReleaseReason string     `json:"releaseReason,omitempty"`
}

func toQuarantine(q artifacts.Quarantine) quarantineView {
	return quarantineView{q.ID, q.Digest, q.WorkerID, q.Reason, q.CreatedAt, q.ReleasedAt, q.ReleasedBy, q.ReleaseReason}
}

func (a *API) ListArtifactQuarantines(w http.ResponseWriter, r *http.Request, params apigen.ListArtifactQuarantinesParams) {
	list, err := a.artifacts.Quarantines(r.Context(), principal(r).OrgID, params.Active != nil && *params.Active)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := struct {
		Items []quarantineView `json:"items"`
	}{Items: []quarantineView{}}
	for _, q := range list {
		out.Items = append(out.Items, toQuarantine(q))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) ReleaseArtifactQuarantine(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.ReleaseArtifactQuarantineParams) {
	var in apigen.ReleaseArtifactQuarantineJSONBody
	if !decodeBody(w, r, &in) {
		return
	}
	q, err := a.artifacts.ReleaseQuarantine(r.Context(), principal(r), id, in.Reason, requestID(w))
	switch {
	case errors.Is(err, artifacts.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "no active quarantine with this ID")
	case errors.Is(err, artifacts.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only security reviewers and administrators release quarantines")
	case err != nil:
		a.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, toQuarantine(q))
	}
}
