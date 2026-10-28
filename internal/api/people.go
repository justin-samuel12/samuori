package api

import (
	"errors"
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/identity"
	"samuori.invalid/samuori/internal/tenancy"
)

// peopleError maps team, user and password refusals; anything else goes to tenancyError.
func (a *API) peopleError(w http.ResponseWriter, r *http.Request, err error) {
	var inUse *tenancy.InUse
	switch {
	case errors.As(err, &inUse):
		apigen.WriteProblem(w, http.StatusConflict, "team_in_use", "the team cannot be deleted while it has "+joinList(inUse.What)+"; move or remove them first")
	case errors.Is(err, tenancy.ErrInvalidName), errors.Is(err, tenancy.ErrInvalidRole):
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, tenancy.ErrInvalidDisplayName):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	case errors.Is(err, tenancy.ErrSelf):
		apigen.WriteProblem(w, http.StatusConflict, "own_account", err.Error())
	case errors.Is(err, tenancy.ErrLastAdmin):
		apigen.WriteProblem(w, http.StatusConflict, "last_administrator", err.Error())
	case errors.Is(err, tenancy.ErrLastLocalAdmin):
		apigen.WriteProblem(w, http.StatusConflict, "last_local_administrator", err.Error())
	case errors.Is(err, tenancy.ErrFromMapping):
		apigen.WriteProblem(w, http.StatusConflict, "from_group_mapping", err.Error())
	case errors.Is(err, auth.ErrResetSelf):
		apigen.WriteProblem(w, http.StatusConflict, "own_account", err.Error())
	case errors.Is(err, auth.ErrNotLocalUser):
		apigen.WriteProblem(w, http.StatusConflict, "not_local", err.Error())
	case errors.Is(err, auth.ErrWrongPassword):
		apigen.WriteProblem(w, http.StatusBadRequest, "wrong_password", err.Error())
	case errors.Is(err, auth.ErrSamePassword):
		apigen.WriteProblem(w, http.StatusBadRequest, "same_password", err.Error())
	case errors.Is(err, auth.ErrWeakPassword):
		apigen.WriteProblem(w, http.StatusBadRequest, "weak_password", err.Error())
	case errors.Is(err, auth.ErrLocked), errors.Is(err, auth.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		apigen.WriteProblem(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	default:
		a.tenancyError(w, r, err)
	}
}

func joinList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	out := items[0]
	for _, s := range items[1 : len(items)-1] {
		out += ", " + s
	}
	return out + " and " + items[len(items)-1]
}

func (a *API) UpdateTeam(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateTeamParams) {
	var body apigen.TeamCreate
	if !decodeBody(w, r, &body) {
		return
	}
	t, err := a.tenancy.RenameTeam(r.Context(), principal(r), id, body.Name, requestID(w))
	if err != nil {
		a.peopleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.Team{Id: t.ID, Name: t.Name})
}

func (a *API) DeleteTeam(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeleteTeamParams) {
	if err := a.tenancy.DeleteTeam(r.Context(), principal(r), id, requestID(w)); err != nil {
		a.peopleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) setDisplayName(w http.ResponseWriter, r *http.Request, userID string) {
	var body apigen.DisplayNameUpdate
	if !decodeBody(w, r, &body) {
		return
	}
	if err := a.tenancy.SetUserDisplayName(r.Context(), principal(r), userID, body.DisplayName, requestID(w)); err != nil {
		a.peopleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) UpdateUser(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateUserParams) {
	a.setDisplayName(w, r, id)
}

func (a *API) UpdateMe(w http.ResponseWriter, r *http.Request, _ apigen.UpdateMeParams) {
	p := principal(r)
	if p.Kind != authz.User {
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only people change their own display name")
		return
	}
	a.setDisplayName(w, r, p.ID)
}

func (a *API) SetUserState(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.SetUserStateParams) {
	var in apigen.EnabledState
	if !decodeBody(w, r, &in) {
		return
	}
	if err := a.tenancy.SetUserEnabled(r.Context(), principal(r), id, in.Enabled, requestID(w)); err != nil {
		a.peopleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) ResetUserPassword(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.ResetUserPasswordParams) {
	t, err := a.auth.ResetPassword(r.Context(), principal(r), id, requestID(w))
	if err != nil {
		a.peopleError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, apigen.TemporaryPassword{TemporaryPassword: t.Value, ExpiresAt: t.ExpiresAt})
}

func (a *API) ChangePassword(w http.ResponseWriter, r *http.Request, _ apigen.ChangePasswordParams) {
	session, ok := browserSession(r)
	if !ok {
		apigen.WriteProblem(w, http.StatusForbidden, "browser_session_required", "change your password from a signed-in browser, not with an API token")
		return
	}
	var body apigen.PasswordChange
	if !decodeBody(w, r, &body) {
		return
	}
	if body.CurrentPassword == nil || body.NewPassword == nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "currentPassword and newPassword are required")
		return
	}
	if err := a.auth.ChangePassword(r.Context(), principal(r), session, *body.CurrentPassword, *body.NewPassword, auth.ClientIP(r), requestID(w)); err != nil {
		a.peopleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) ChangeRoleBinding(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.ChangeRoleBindingParams) {
	var body apigen.RoleBindingChange
	if !decodeBody(w, r, &body) {
		return
	}
	rb, err := a.tenancy.ChangeRole(r.Context(), principal(r), id, authz.Role(body.Role), requestID(w))
	if err != nil {
		a.peopleError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRoleBinding(rb))
}

func (a *API) UpdateServiceIdentity(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateServiceIdentityParams) {
	var body apigen.DisplayNameUpdate
	if !decodeBody(w, r, &body) {
		return
	}
	si, err := a.idStore.SetServiceDisplayName(r.Context(), principal(r), id, body.DisplayName, requestID(w))
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	v, err := a.serviceView(r, si)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) DeleteIdentityProvider(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.DeleteIdentityProviderParams) {
	err := a.idStore.Delete(r.Context(), principal(r), id, requestID(w))
	if errors.Is(err, identity.ErrInUse) {
		apigen.WriteProblem(w, http.StatusConflict, "provider_in_use", err.Error())
		return
	}
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
