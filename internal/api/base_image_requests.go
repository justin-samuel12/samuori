package api

import (
	"errors"
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/approvals"
)

func (a *API) toBaseImageRequest(r *http.Request, x approvals.BaseImageRequest) apigen.BaseImageRequest {
	can := a.approvals.CanWithdrawRequest(r.Context(), principal(r), approvals.SubjectBaseImage, x.OrgID, x.ProjectID, x.RequestedBy, x.State)
	v := apigen.BaseImageRequest{CanWithdraw: &can, Id: x.ID, ProjectId: x.ProjectID, Repository: x.Repository, Justification: x.Justification, ApprovalId: x.ApprovalID, State: apigen.BaseImageRequestState(x.State), CreatedAt: x.CreatedAt}
	if x.Reason != "" {
		v.Reason = &x.Reason
	}
	if x.PolicyVersion > 0 {
		v.PolicyVersion = &x.PolicyVersion
	}
	return v
}

func (a *API) ListBaseImageRequests(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId) {
	list, err := a.approvals.BaseImageRequests(r.Context(), principal(r), operationRoles(r.Context()), projectID)
	if err != nil {
		a.approvalError(w, r, err)
		return
	}
	out := apigen.BaseImageRequestList{Items: []apigen.BaseImageRequest{}}
	for _, x := range list {
		out.Items = append(out.Items, a.toBaseImageRequest(r, x))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) RequestBaseImage(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, _ apigen.RequestBaseImageParams) {
	var in apigen.BaseImageRequestCreate
	if !decodeBody(w, r, &in) {
		return
	}
	x, err := a.approvals.RequestBaseImage(r.Context(), principal(r), operationRoles(r.Context()), projectID, in.Repository, in.Justification)
	switch {
	case refuseArchived(w, err):
	case errors.Is(err, approvals.ErrNotFound):
		a.approvalError(w, r, err)
	case errors.Is(err, approvals.ErrAlreadyAllowed):
		apigen.WriteProblem(w, http.StatusConflict, "already_allowed", err.Error()+"; builds can use it now")
	case errors.Is(err, approvals.ErrRequestPending):
		apigen.WriteProblem(w, http.StatusConflict, "request_pending", err.Error())
	case err != nil:
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	default:
		writeJSON(w, http.StatusAccepted, a.toBaseImageRequest(r, x))
	}
}
