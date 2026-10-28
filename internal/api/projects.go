package api

import (
	"errors"
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/projects"
	"samuori.invalid/samuori/internal/tenancy"
)

// refuseArchived answers 409 for work refused because the project is archived or purged (ADR 0058).
func refuseArchived(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, tenancy.ErrArchived) {
		return false
	}
	apigen.WriteProblem(w, http.StatusConflict, "project_archived", tenancy.ErrArchived.Error())
	return true
}

const externalCopiesNotice = "Images already pushed to external registries were not deleted; remove them in each registry if they must go."

func (a *API) projectLifecycleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrConfirm):
		apigen.WriteProblem(w, http.StatusPreconditionRequired, "confirmation_required", err.Error()+" in the X-Samuori-Confirm header")
	case errors.Is(err, projects.ErrNotActive), errors.Is(err, projects.ErrNotArchived), errors.Is(err, projects.ErrPurged):
		apigen.WriteProblem(w, http.StatusConflict, "project_state", err.Error())
	case errors.Is(err, projects.ErrBusy):
		apigen.WriteProblem(w, http.StatusConflict, "busy", err.Error())
	default:
		a.tenancyError(w, r, err)
	}
}

// ArchiveProject stops new work for a project and revokes its grants (ADR 0058).
func (a *API) ArchiveProject(w http.ResponseWriter, r *http.Request, id apigen.ProjectId, _ apigen.ArchiveProjectParams) {
	var in apigen.ProjectArchiveRequest
	if r.ContentLength != 0 && !decodeBody(w, r, &in) {
		return
	}
	reason := ""
	if in.Reason != nil {
		reason = *in.Reason
	}
	res, err := a.projects.Archive(r.Context(), principal(r), id, reason, requestID(w))
	if err != nil {
		if errors.Is(err, projects.ErrReason) {
			apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
			return
		}
		a.projectLifecycleError(w, r, err)
		return
	}
	out := apigen.ProjectArchive{Project: a.toProject(r.Context(), res.Project), CancelledBuilds: res.CancelledBuilds, ClosedUploads: res.ClosedUploads, Notes: res.CancelErrors}
	if out.Notes == nil {
		out.Notes = []string{}
	}
	rv := res.Revoked
	out.Revoked.SecretGrants, out.Revoked.SecretBindings, out.Revoked.EgressAllowances = rv.SecretGrants, rv.SecretBindings, rv.EgressAllowances
	out.Revoked.Destinations, out.Revoked.FederationGrants, out.Revoked.Pipelines = rv.Destinations, rv.FederationGrants, rv.Pipelines
	out.Revoked.OutputExceptions, out.Revoked.Requests = rv.OutputExceptions, rv.Requests
	writeJSON(w, http.StatusOK, out)
}

// RestoreProject makes an archived project usable again, without its former grants.
func (a *API) RestoreProject(w http.ResponseWriter, r *http.Request, id apigen.ProjectId, _ apigen.RestoreProjectParams) {
	p, err := a.projects.Restore(r.Context(), principal(r), id, requestID(w))
	if err != nil {
		a.projectLifecycleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.toProject(r.Context(), p))
}

// PurgeProject deletes an archived project's stored content once the caller types its name (ADR 0058).
func (a *API) PurgeProject(w http.ResponseWriter, r *http.Request, id apigen.ProjectId, params apigen.PurgeProjectParams) {
	confirm := ""
	if params.XSamuoriConfirm != nil {
		confirm = *params.XSamuoriConfirm
	}
	res, err := a.projects.Purge(r.Context(), principal(r), id, confirm, requestID(w))
	if err != nil {
		if errors.Is(err, projects.ErrIncomplete) {
			a.log.Error("project purge incomplete", "project", id, "error", err)
			apigen.WriteProblem(w, http.StatusInternalServerError, "purge_incomplete", err.Error()+". "+externalCopiesNotice)
			return
		}
		a.projectLifecycleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.ProjectPurge{Project: a.toProject(r.Context(), res.Project), Snapshots: res.Snapshots, Builds: res.Builds,
		RemovedBlobs: res.RemovedBlobs, KeptBlobs: res.KeptBlobs, ExternalCopies: externalCopiesNotice})
}
