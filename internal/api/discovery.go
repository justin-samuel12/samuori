package api

import (
	"errors"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/admission"
	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/discovery"
	"samuori.invalid/samuori/internal/discovery/jobs"
	"samuori.invalid/samuori/internal/discovery/sandbox"
	"samuori.invalid/samuori/internal/egress"
)

func (a *API) StartDiscovery(w http.ResponseWriter, r *http.Request, snapshotID apigen.SnapshotId, _ apigen.StartDiscoveryParams) {
	op, err := a.discovery.Start(r.Context(), principal(r), operationRoles(r.Context()), snapshotID, requestID(w))
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, toOperation(op, requestID(w)))
}

// discoveryView mirrors the Discovery schema; detector results keep their contract field names.
type discoveryView struct {
	ID          string                         `json:"id"`
	ProjectID   string                         `json:"projectId"`
	SnapshotID  string                         `json:"snapshotId"`
	Status      string                         `json:"status"`
	Findings    []discovery.Finding            `json:"findings"`
	Dockerfiles []discovery.DockerfileAnalysis `json:"dockerfiles"`
	Warnings    []discovery.Warning            `json:"warnings"`
	Sandbox     *sandbox.Sandbox               `json:"sandbox,omitempty"`
	Error       *apigen.Problem                `json:"error,omitempty"`
	CreatedAt   time.Time                      `json:"createdAt"`
}

func (a *API) ListSnapshotDiscoveries(w http.ResponseWriter, r *http.Request, snapshotID apigen.SnapshotId) {
	list, err := a.discovery.List(r.Context(), principal(r), operationRoles(r.Context()), snapshotID)
	if err != nil {
		a.sourcesError(w, r, err)
		return
	}
	out := struct {
		Items []discoveryView `json:"items"`
	}{Items: make([]discoveryView, 0, len(list))}
	for _, d := range list {
		out.Items = append(out.Items, view(d, requestID(w), false))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetDiscovery(w http.ResponseWriter, r *http.Request, id apigen.Id) {
	d, err := a.discovery.Get(r.Context(), principal(r), operationRoles(r.Context()), id)
	if errors.Is(err, jobs.ErrNotFound) {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if d.Status == jobs.Completed {
		eff, err := a.policies.Effective(r.Context(), principal(r).OrgID)
		var project []egress.Allowance
		if err == nil {
			project, err = a.approvals.ProjectAllowances(r.Context(), principal(r).OrgID, d.ProjectID)
		}
		if err == nil {
			err = admission.DiscoveryReadiness(&d.Result, eff.Egress, project)
		}
		if err != nil {
			a.internalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, view(d, requestID(w), true))
}

func view(d jobs.Discovery, reqID string, full bool) discoveryView {
	v := discoveryView{ID: d.ID, ProjectID: d.ProjectID, SnapshotID: d.SnapshotID, Status: d.Status, CreatedAt: d.CreatedAt}
	if full {
		v.Findings, v.Dockerfiles, v.Warnings = d.Result.Findings, d.Result.Dockerfiles, d.Result.Warnings
	}
	if v.Findings == nil {
		v.Findings = []discovery.Finding{}
	}
	if v.Dockerfiles == nil {
		v.Dockerfiles = []discovery.DockerfileAnalysis{}
	}
	if v.Warnings == nil {
		v.Warnings = []discovery.Warning{}
	}
	if full && d.Status != jobs.Running {
		v.Sandbox = &d.Sandbox
	}
	if d.ErrorCode != "" {
		v.Error = &apigen.Problem{Code: d.ErrorCode, Message: d.ErrorMessage, RequestId: reqID}
	}
	return v
}
