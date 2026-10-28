package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/tenancy"
)

func (a *API) CreateTeam(w http.ResponseWriter, r *http.Request, _ apigen.CreateTeamParams) {
	var body apigen.TeamCreate
	if !decodeBody(w, r, &body) {
		return
	}
	t, err := a.tenancy.CreateTeam(r.Context(), principal(r), body.Name, requestID(w))
	if err != nil {
		a.tenancyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, apigen.Team{Id: t.ID, Name: t.Name})
}

func (a *API) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.tenancy.ListUsers(r.Context(), principal(r))
	if err != nil {
		a.tenancyError(w, r, err)
		return
	}
	out := apigen.UserList{Items: make([]apigen.User, 0, len(users))}
	for _, u := range users {
		out.Items = append(out.Items, toUser(u))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateUser(w http.ResponseWriter, r *http.Request, _ apigen.CreateUserParams) {
	var body apigen.UserCreate
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Password == nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "password is required")
		return
	}
	display := ""
	if body.DisplayName != nil {
		display = *body.DisplayName
	}
	id, err := a.auth.CreateLocalUser(r.Context(), principal(r), body.Username, display, *body.Password, requestID(w))
	if errors.Is(err, auth.ErrWeakPassword) {
		apigen.WriteProblem(w, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	if err != nil {
		a.tenancyError(w, r, err)
		return
	}
	if display == "" {
		display = body.Username
	}
	writeJSON(w, http.StatusCreated, apigen.User{Id: id, Username: body.Username, DisplayName: display, Roles: []apigen.RoleBinding{}})
}

func (a *API) CreateRoleBinding(w http.ResponseWriter, r *http.Request, _ apigen.CreateRoleBindingParams) {
	var body apigen.RoleBindingCreate
	if !decodeBody(w, r, &body) {
		return
	}
	b := authz.Binding{Role: authz.Role(body.Role), ScopeType: authz.ScopeType(body.ScopeType), ScopeID: body.ScopeId}
	rb, err := a.tenancy.Bind(r.Context(), principal(r), body.PrincipalId, b, requestID(w))
	if err != nil {
		a.tenancyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRoleBinding(rb))
}

func (a *API) RevokeRoleBinding(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RevokeRoleBindingParams) {
	err := a.tenancy.Unbind(r.Context(), principal(r), id, requestID(w))
	if errors.Is(err, tenancy.ErrLastAdmin) {
		apigen.WriteProblem(w, http.StatusConflict, "last_administrator", err.Error())
		return
	}
	if errors.Is(err, tenancy.ErrLastLocalAdmin) {
		apigen.WriteProblem(w, http.StatusConflict, "last_local_administrator", err.Error())
		return
	}
	if err != nil {
		a.tenancyError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toUser(u tenancy.User) apigen.User {
	out := apigen.User{Id: u.ID, Username: u.Username, DisplayName: u.DisplayName, Disabled: u.Disabled, Roles: make([]apigen.RoleBinding, 0, len(u.Roles))}
	if u.Source != "" {
		out.Source = &u.Source
	}
	for _, rb := range u.Roles {
		out.Roles = append(out.Roles, toRoleBinding(rb))
	}
	return out
}

func toRoleBinding(rb tenancy.RoleBinding) apigen.RoleBinding {
	out := apigen.RoleBinding{Id: rb.ID, Role: apigen.Role(rb.Role), ScopeType: apigen.RoleBindingScopeType(rb.ScopeType), ScopeId: rb.ScopeID}
	if rb.Source != "" {
		out.Source = &rb.Source
	}
	return out
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
		return false
	}
	return true
}

func requestID(w http.ResponseWriter) string { return w.Header().Get("X-Request-ID") }
