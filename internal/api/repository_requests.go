package api

import (
	"errors"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/approvals"
	"samuori.invalid/samuori/internal/publication"
)

type repositoryRequestView struct {
	ID              string    `json:"id"`
	ProjectID       string    `json:"projectId"`
	RegistryID      string    `json:"registryId,omitempty"`
	Repository      string    `json:"repository"`
	Justification   string    `json:"justification"`
	ApprovalID      string    `json:"approvalId"`
	State           string    `json:"state"`
	DestinationID   string    `json:"destinationId,omitempty"`
	DestinationName string    `json:"destinationName,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
}

func (a *API) toRepositoryRequest(r *http.Request, x approvals.RepositoryRequest) repositoryRequestView {
	v := repositoryRequestView{ID: x.ID, ProjectID: x.ProjectID, RegistryID: x.RegistryID, Repository: x.Repository, Justification: x.Justification, ApprovalID: x.ApprovalID, State: x.State, CreatedAt: x.CreatedAt}
	if x.DestinationID != "" && x.State == approvals.Approved {
		v.DestinationID, v.DestinationName = x.DestinationID, a.publications.DestinationName(r.Context(), principal(r).OrgID, x.DestinationID)
	}
	return v
}

func (a *API) ListRepositoryRequests(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId) {
	list, err := a.approvals.RepositoryRequests(r.Context(), principal(r), operationRoles(r.Context()), projectID)
	if err != nil {
		a.approvalError(w, r, err)
		return
	}
	out := struct {
		Items []repositoryRequestView `json:"items"`
	}{Items: []repositoryRequestView{}}
	for _, x := range list {
		out.Items = append(out.Items, a.toRepositoryRequest(r, x))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) RequestRepository(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, _ apigen.RequestRepositoryParams) {
	var in struct {
		Repository    string `json:"repository"`
		RegistryID    string `json:"registryId"`
		Justification string `json:"justification"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	x, err := a.approvals.RequestRepository(r.Context(), principal(r), operationRoles(r.Context()), projectID, in.RegistryID, in.Repository, in.Justification)
	if errors.Is(err, approvals.ErrNotFound) {
		a.approvalError(w, r, err)
		return
	}
	if err != nil {
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, a.toRepositoryRequest(r, x))
}

func (a *API) FulfillRepositoryRequest(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.FulfillRepositoryRequestParams) {
	var in struct {
		DestinationID string `json:"destinationId"`
		Reason        string `json:"reason"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	ap, err := a.publications.FulfillRepositoryRequest(r.Context(), principal(r), operationRoles(r.Context()), id, in.DestinationID, in.Reason, requestID(w))
	var inv *publication.Invalid
	switch {
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", inv.Reason)
		return
	case errors.Is(err, publication.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only administrators fulfill repository requests")
		return
	case err != nil:
		a.approvalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.toApproval(r, ap))
}
