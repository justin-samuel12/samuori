package api

import (
	"errors"
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/approvals"
	"samuori.invalid/samuori/internal/publication"
	"samuori.invalid/samuori/internal/tenancy"
)

func (a *API) approvalError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, approvals.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, approvals.ErrOwn):
		apigen.WriteProblem(w, http.StatusForbidden, "own_request", "you cannot decide your own request; ask another approver")
	case errors.Is(err, approvals.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "you may not decide this request")
	case errors.Is(err, approvals.ErrDecided):
		apigen.WriteProblem(w, http.StatusConflict, "decided", err.Error())
	case errors.Is(err, approvals.ErrUnfulfilled):
		apigen.WriteProblem(w, http.StatusConflict, "unfulfilled", err.Error())
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) toApproval(r *http.Request, ap approvals.Approval) apigen.Approval {
	out := apigen.Approval{Id: ap.ID, ProjectId: ap.ProjectID, SubjectType: apigen.ApprovalSubjectType(ap.SubjectType), SubjectId: ap.SubjectID, State: apigen.ApprovalState(ap.State),
		Summary: ap.Summary, RequestedBy: apigen.Principal{Id: ap.RequestedBy, Type: apigen.PrincipalTypeUser}, CanDecide: a.approvals.CanDecide(r.Context(), principal(r), ap),
		CanWithdraw: a.approvals.CanWithdraw(r.Context(), principal(r), ap), CreatedAt: ap.CreatedAt, ExpiresAt: ap.ExpiresAt, DecidedAt: ap.DecidedAt, ValidUntil: ap.ValidUntil}
	if ap.RequestedBy == "system" {
		out.RequestedBy.Type = apigen.PrincipalTypeService
	}
	if ap.DecidedBy != "" {
		out.DecidedBy = &apigen.Principal{Id: ap.DecidedBy, Type: apigen.PrincipalTypeUser}
	}
	if ap.Reason != "" {
		out.Reason = &ap.Reason
	}
	return out
}

func (a *API) ListApprovals(w http.ResponseWriter, r *http.Request, params apigen.ListApprovalsParams) {
	limit, _ := pageParams(params.Limit, params.Cursor)
	state := ""
	if params.State != nil {
		state = string(*params.State)
	}
	list, err := a.approvals.List(r.Context(), principal(r), operationRoles(r.Context()), state, limit)
	if err != nil {
		a.approvalError(w, r, err)
		return
	}
	out := apigen.ApprovalList{Items: []apigen.Approval{}}
	for _, ap := range list {
		out.Items = append(out.Items, a.toApproval(r, ap))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) DecideApproval(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DecideApprovalParams) {
	var in apigen.ApprovalDecision
	if !decodeBody(w, r, &in) {
		return
	}
	ap, err := a.approvals.Decide(r.Context(), principal(r), operationRoles(r.Context()), id, in.Decision == apigen.ApprovalDecisionDecisionApprove, in.Reason, in.ValidUntil, requestID(w))
	if err != nil {
		a.approvalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.toApproval(r, ap))
}

// WithdrawApproval closes a pending request before it is decided (ADR 0062).
func (a *API) WithdrawApproval(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.WithdrawApprovalParams) {
	var in apigen.ApprovalWithdrawal
	if r.ContentLength != 0 && !decodeBody(w, r, &in) {
		return
	}
	reason := ""
	if in.Reason != nil {
		reason = *in.Reason
	}
	ap, err := a.approvals.Withdraw(r.Context(), principal(r), operationRoles(r.Context()), id, reason, requestID(w))
	switch {
	case errors.Is(err, approvals.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only the requester or a maintainer or administrator of the project may withdraw this request")
	case errors.Is(err, approvals.ErrNotWithdrawable):
		apigen.WriteProblem(w, http.StatusConflict, "not_withdrawable", err.Error())
	case errors.Is(err, approvals.ErrWithdrawReason):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	case err != nil:
		a.approvalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, a.toApproval(r, ap))
	}
}

func (a *API) RequestSecretBinding(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, _ apigen.RequestSecretBindingParams) {
	var in apigen.SecretBindingRequest
	if !decodeBody(w, r, &in) {
		return
	}
	ap, err := a.approvals.RequestBinding(r.Context(), principal(r), operationRoles(r.Context()), projectID, in.LogicalId, in.Justification)
	if refuseArchived(w, err) {
		return
	}
	if errors.Is(err, approvals.ErrNotFound) {
		a.approvalError(w, r, err)
		return
	}
	if err != nil {
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, a.toApproval(r, ap))
}

func (a *API) UpdateProject(w http.ResponseWriter, r *http.Request, id apigen.ProjectId, _ apigen.UpdateProjectParams) {
	var in apigen.ProjectUpdate
	if !decodeBody(w, r, &in) {
		return
	}
	var c tenancy.ProjectChanges
	c.Name = in.Name
	if in.PublicationMode != nil {
		mode := string(*in.PublicationMode)
		c.PublicationMode = &mode
	}
	p, err := a.tenancy.UpdateProject(r.Context(), principal(r), id, c, requestID(w))
	var inv *tenancy.ErrInvalid
	if errors.As(err, &inv) {
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", inv.Reason)
		return
	}
	if err != nil {
		a.tenancyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.toProject(r.Context(), p))
}

func (a *API) ListProjectImages(w http.ResponseWriter, r *http.Request, projectID apigen.ProjectId, params apigen.ListProjectImagesParams) {
	limit, cursor := pageParams(params.Limit, params.Cursor)
	list, err := a.publications.Images(r.Context(), principal(r), operationRoles(r.Context()), projectID, cursor, limit+1)
	if err != nil {
		a.publicationError(w, r, err)
		return
	}
	type pubView struct {
		ID              string   `json:"id"`
		DestinationID   string   `json:"destinationId"`
		DestinationName string   `json:"destinationName,omitempty"`
		State           string   `json:"state"`
		Tags            []string `json:"tags"`
		DestinationRef  string   `json:"destinationRef,omitempty"`
	}
	type imageView struct {
		ArtifactID   string           `json:"artifactId"`
		Digest       string           `json:"digest"`
		BuildRunID   string           `json:"buildRunId"`
		Version      string           `json:"version,omitempty"`
		App          string           `json:"app,omitempty"`
		SizeBytes    int64            `json:"sizeBytes"`
		CreatedAt    any              `json:"createdAt"`
		Validation   *validationBrief `json:"validation,omitempty"`
		Publications []pubView        `json:"publications"`
	}
	out := struct {
		Items      []imageView `json:"items"`
		NextCursor *string     `json:"nextCursor,omitempty"`
	}{Items: []imageView{}}
	list, out.NextCursor = pageLimited(list, limit, func(x publication.Image) string { return x.ArtifactID })
	for _, img := range list {
		v := imageView{ArtifactID: img.ArtifactID, Digest: img.Digest, BuildRunID: img.BuildRunID, Version: img.Version, App: img.App, SizeBytes: img.Size, CreatedAt: img.CreatedAt, Publications: []pubView{}}
		if img.Validation != nil {
			v.Validation = &validationBrief{ID: img.Validation.ID, State: img.Validation.State}
			if img.Validation.Verdict != "" {
				v.Validation.Verdict = &img.Validation.Verdict
			}
		}
		for _, p := range img.Publications {
			v.Publications = append(v.Publications, pubView{ID: p.ID, DestinationID: p.DestinationID, DestinationName: p.DestinationName, State: p.State, Tags: p.Tags, DestinationRef: p.DestinationRef})
		}
		out.Items = append(out.Items, v)
	}
	writeJSON(w, http.StatusOK, out)
}
