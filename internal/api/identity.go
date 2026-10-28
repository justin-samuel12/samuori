package api

import (
	"errors"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/identity"
	"samuori.invalid/samuori/internal/tenancy"
)

// oidcCookie binds a sign-in to the browser that started it. It must be Lax: the provider's redirect back is a
// cross-site top-level navigation.
const oidcCookie = "samuori_oidc"

func (a *API) identityError(w http.ResponseWriter, r *http.Request, err error) {
	var inv *identity.Invalid
	switch {
	case errors.Is(err, identity.ErrForbidden):
		apigen.WriteProblem(w, http.StatusForbidden, "forbidden", "only organization administrators manage identity")
	case errors.Is(err, identity.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, identity.ErrConflict):
		apigen.WriteProblem(w, http.StatusConflict, "conflict", "that name is already used")
	case errors.As(err, &inv):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", inv.Reason)
	case errors.Is(err, auth.ErrSessionTimeouts):
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", err.Error())
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) ListSignInProviders(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	out := struct {
		Items []item `json:"items"`
	}{Items: []item{}}
	if a.idStore != nil {
		list, err := a.idStore.Enabled(r.Context())
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		for _, p := range list {
			if p.Kind == identity.OIDC && a.signIn.RedirectURL() == "" {
				continue
			}
			out.Items = append(out.Items, item{p.ID, p.DisplayName, p.Kind})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) StartOidcSignIn(w http.ResponseWriter, r *http.Request, providerID apigen.Id, params apigen.StartOidcSignInParams) {
	if a.signIn == nil || !a.auth.AllowOIDCStart(auth.ClientIP(r)) {
		w.Header().Set("Retry-After", "60")
		apigen.WriteProblem(w, http.StatusTooManyRequests, "rate_limited", "too many sign-in attempts; try again later")
		return
	}
	returnTo := "/"
	if params.ReturnTo != nil {
		returnTo, _ = auth.SafeReturnTo(*params.ReturnTo)
	}
	url, browser, err := a.signIn.StartOIDC(r.Context(), providerID, identity.OIDCStart{ReturnTo: returnTo})
	if errors.Is(err, identity.ErrNotFound) || errors.Is(err, identity.ErrDisabled) {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "that sign-in option is not available")
		return
	}
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: browser, Path: "/api/v1/auth/oidc/", MaxAge: 600, HttpOnly: true, Secure: a.secureCookies, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, url, http.StatusFound)
}

func (a *API) FinishOidcSignIn(w http.ResponseWriter, r *http.Request, params apigen.FinishOidcSignInParams) {
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: "", Path: "/api/v1/auth/oidc/", MaxAge: -1, HttpOnly: true, Secure: a.secureCookies, SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	fail := func() { http.Redirect(w, r, "/?signin=failed", http.StatusFound) }
	if a.signIn == nil || params.State == nil || params.Code == nil || params.Error != nil {
		fail()
		return
	}
	browser := ""
	if c, err := r.Cookie(oidcCookie); err == nil {
		browser = c.Value
	}
	res, err := a.signIn.FinishOIDC(r.Context(), *params.State, *params.Code, browser, requestID(w))
	if err != nil {
		if !errors.Is(err, identity.ErrAuthFailed) {
			a.log.Error("OIDC sign-in", "error", err)
		}
		fail()
		return
	}
	var s auth.Session
	if res.RenewSessionHash != "" {
		s, _, err = a.auth.ReplaceSession(r.Context(), res.RenewSessionHash, res.UserID, requestID(w), "OIDC")
		if errors.Is(err, auth.ErrNotFound) {
			// The renewed session ended during the round trip; the person has just signed in, so start a session.
			s, err = a.auth.IssueSession(r.Context(), res.UserID, res.OrgID, requestID(w), "OIDC sign-in after the renewed session ended")
		}
	} else {
		s, err = a.auth.IssueSession(r.Context(), res.UserID, res.OrgID, requestID(w), "OIDC sign-in")
	}
	if err != nil {
		if !errors.Is(err, auth.ErrDifferentUser) {
			a.log.Error("OIDC session", "error", err)
		}
		fail()
		return
	}
	a.setCookie(w, auth.SessionCookie, s.Token, true)
	a.setCookie(w, auth.CSRFCookie, s.CSRF, false)
	back, _ := auth.SafeReturnTo(res.ReturnTo)
	http.Redirect(w, r, back, http.StatusFound)
}

type providerView struct {
	ID          string               `json:"id"`
	Kind        string               `json:"kind"`
	Name        string               `json:"name"`
	DisplayName string               `json:"displayName"`
	Enabled     bool                 `json:"enabled"`
	HasSecret   bool                 `json:"hasSecret"`
	OIDC        *identity.OIDCConfig `json:"oidc,omitempty"`
	LDAP        *identity.LDAPConfig `json:"ldap,omitempty"`
	CreatedAt   time.Time            `json:"createdAt"`
}

func toProvider(p identity.Provider) providerView {
	return providerView{ID: p.ID, Kind: p.Kind, Name: p.Name, DisplayName: p.DisplayName, Enabled: p.Enabled, HasSecret: p.SecretID != "", OIDC: p.OIDC, LDAP: p.LDAP, CreatedAt: p.CreatedAt}
}

func (a *API) ListIdentityProviders(w http.ResponseWriter, r *http.Request, params apigen.ListIdentityProvidersParams) {
	list, err := a.idStore.List(r.Context(), principal(r))
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	out := struct {
		Items      []providerView `json:"items"`
		NextCursor *string        `json:"nextCursor,omitempty"`
	}{Items: []providerView{}}
	for _, p := range list {
		out.Items = append(out.Items, toProvider(p))
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x providerView) string { return x.ID }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateIdentityProvider(w http.ResponseWriter, r *http.Request, _ apigen.CreateIdentityProviderParams) {
	var in struct {
		Kind        string               `json:"kind"`
		Name        string               `json:"name"`
		DisplayName string               `json:"displayName"`
		SecretID    string               `json:"secretId"`
		OIDC        *identity.OIDCConfig `json:"oidc"`
		LDAP        *identity.LDAPConfig `json:"ldap"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Kind == identity.OIDC && a.signIn.RedirectURL() == "" {
		apigen.WriteProblem(w, http.StatusUnprocessableEntity, "invalid", "configure the controller's external URL (server.externalUrl) before adding an OIDC provider")
		return
	}
	p, err := a.idStore.Create(r.Context(), principal(r), identity.Provider{Kind: in.Kind, Name: in.Name, DisplayName: in.DisplayName, SecretID: in.SecretID, OIDC: in.OIDC, LDAP: in.LDAP}, requestID(w))
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProvider(p))
}

func (a *API) UpdateIdentityProvider(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.UpdateIdentityProviderParams) {
	var in struct {
		DisplayName string               `json:"displayName"`
		SecretID    string               `json:"secretId"`
		OIDC        *identity.OIDCConfig `json:"oidc"`
		LDAP        *identity.LDAPConfig `json:"ldap"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	p, err := a.idStore.Update(r.Context(), principal(r), id, identity.Provider{DisplayName: in.DisplayName, SecretID: in.SecretID, OIDC: in.OIDC, LDAP: in.LDAP}, requestID(w))
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toProvider(p))
}

func (a *API) SetIdentityProviderState(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.SetIdentityProviderStateParams) {
	var in apigen.EnabledState
	if !decodeBody(w, r, &in) {
		return
	}
	p, err := a.idStore.SetEnabled(r.Context(), principal(r), id, in.Enabled, requestID(w))
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toProvider(p))
}

func (a *API) TestIdentityProvider(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.TestIdentityProviderParams) {
	err := a.signIn.Test(r.Context(), principal(r), id)
	if errors.Is(err, identity.ErrForbidden) || errors.Is(err, identity.ErrNotFound) {
		a.identityError(w, r, err)
		return
	}
	res := map[string]any{"status": "ok", "testedAt": time.Now().UTC()}
	if err != nil {
		msg := err.Error()
		if len(msg) > 1000 {
			msg = msg[:1000]
		}
		res["status"], res["detail"] = "failed", msg
	}
	writeJSON(w, http.StatusOK, res)
}

type mappingView struct {
	ID        string `json:"id"`
	Group     string `json:"group"`
	Role      string `json:"role"`
	ScopeType string `json:"scopeType"`
	ScopeID   string `json:"scopeId"`
}

func (a *API) ListGroupMappings(w http.ResponseWriter, r *http.Request, id apigen.Id, params apigen.ListGroupMappingsParams) {
	list, err := a.idStore.Mappings(r.Context(), principal(r), id)
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	out := struct {
		Items      []mappingView `json:"items"`
		NextCursor *string       `json:"nextCursor,omitempty"`
	}{Items: []mappingView{}}
	for _, m := range list {
		out.Items = append(out.Items, mappingView{m.ID, m.Group, string(m.Binding.Role), string(m.Binding.ScopeType), m.Binding.ScopeID})
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x mappingView) string { return x.ID }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateGroupMapping(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.CreateGroupMappingParams) {
	var in apigen.GroupMappingCreate
	if !decodeBody(w, r, &in) {
		return
	}
	m, err := a.idStore.AddMapping(r.Context(), principal(r), id, identity.Mapping{Group: in.Group,
		Binding: authz.Binding{Role: authz.Role(in.Role), ScopeType: authz.ScopeType(in.ScopeType), ScopeID: in.ScopeId}}, requestID(w))
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, mappingView{m.ID, m.Group, string(m.Binding.Role), string(m.Binding.ScopeType), m.Binding.ScopeID})
}

func (a *API) DeleteGroupMapping(w http.ResponseWriter, r *http.Request, providerID, mappingID apigen.Id, _ apigen.DeleteGroupMappingParams) {
	if err := a.idStore.RemoveMapping(r.Context(), principal(r), providerID, mappingID, requestID(w)); err != nil {
		a.identityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) identitySettings(w http.ResponseWriter, r *http.Request) {
	orgID := principal(r).OrgID
	policy, err := a.auth.LocalLogin(r.Context(), orgID)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	t, err := a.auth.SessionTimeouts(r.Context(), orgID)
	if errors.Is(err, auth.ErrNotFound) {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := apigen.IdentitySettings{LocalLogin: apigen.IdentitySettingsLocalLogin(policy), Session: apigen.SessionTimeouts{
		IdleTimeoutMinutes: int(t.Idle / time.Minute), MaxSessionHours: int(t.Max / time.Hour), WarningSeconds: int(t.Warning / time.Second)}}
	if u := a.signIn.RedirectURL(); u != "" {
		out.OidcRedirectUrl = &u
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) GetIdentitySettings(w http.ResponseWriter, r *http.Request) {
	if !principal(r).IsOrgAdmin() {
		a.identityError(w, r, identity.ErrForbidden)
		return
	}
	a.identitySettings(w, r)
}

func (a *API) SetIdentitySettings(w http.ResponseWriter, r *http.Request, _ apigen.SetIdentitySettingsParams) {
	var in apigen.IdentitySettingsUpdate
	if !decodeBody(w, r, &in) {
		return
	}
	fail := func(err error) {
		if errors.Is(err, tenancy.ErrForbidden) {
			err = identity.ErrForbidden
		}
		a.identityError(w, r, err)
	}
	if in.Session != nil {
		t := auth.SessionTimeouts{Idle: time.Duration(in.Session.IdleTimeoutMinutes) * time.Minute,
			Max: time.Duration(in.Session.MaxSessionHours) * time.Hour, Warning: time.Duration(in.Session.WarningSeconds) * time.Second}
		if err := a.auth.SetSessionTimeouts(r.Context(), principal(r), t, requestID(w)); err != nil {
			fail(err)
			return
		}
	}
	if in.LocalLogin != nil {
		if err := a.auth.SetLocalLogin(r.Context(), principal(r), string(*in.LocalLogin), requestID(w)); err != nil {
			fail(err)
			return
		}
	}
	a.identitySettings(w, r)
}

type tokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

type serviceView struct {
	ID          string      `json:"id"`
	Username    string      `json:"username"`
	DisplayName string      `json:"displayName"`
	Disabled    bool        `json:"disabled"`
	CreatedAt   time.Time   `json:"createdAt"`
	Tokens      []tokenView `json:"tokens"`
}

func (a *API) serviceView(r *http.Request, si identity.ServiceIdentity) (serviceView, error) {
	v := serviceView{ID: si.ID, Username: si.Username, DisplayName: si.DisplayName, Disabled: si.Disabled, CreatedAt: si.CreatedAt, Tokens: []tokenView{}}
	tokens, err := a.auth.TokensOf(r.Context(), si.ID)
	for _, t := range tokens {
		v.Tokens = append(v.Tokens, tokenView{t.ID, t.Name, t.Scopes, t.ExpiresAt, t.LastUsedAt})
	}
	return v, err
}

func (a *API) ListServiceIdentities(w http.ResponseWriter, r *http.Request, params apigen.ListServiceIdentitiesParams) {
	list, err := a.idStore.Services(r.Context(), principal(r))
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	out := struct {
		Items      []serviceView `json:"items"`
		NextCursor *string       `json:"nextCursor,omitempty"`
	}{Items: []serviceView{}}
	for _, si := range list {
		v, err := a.serviceView(r, si)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		out.Items = append(out.Items, v)
	}
	var ok bool
	if out.Items, out.NextCursor, ok = pageOf(w, out.Items, params.Limit, params.Cursor, func(x serviceView) string { return x.ID }); !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateServiceIdentity(w http.ResponseWriter, r *http.Request, _ apigen.CreateServiceIdentityParams) {
	var in apigen.ServiceIdentityCreate
	if !decodeBody(w, r, &in) {
		return
	}
	display := ""
	if in.DisplayName != nil {
		display = *in.DisplayName
	}
	si, err := a.idStore.CreateService(r.Context(), principal(r), in.Username, display, requestID(w))
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	v, _ := a.serviceView(r, si)
	writeJSON(w, http.StatusCreated, v)
}

func (a *API) SetServiceIdentityState(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.SetServiceIdentityStateParams) {
	var in apigen.EnabledState
	if !decodeBody(w, r, &in) {
		return
	}
	if err := a.idStore.SetServiceEnabled(r.Context(), principal(r), id, in.Enabled, requestID(w)); err != nil {
		a.identityError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) CreateServiceToken(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.CreateServiceTokenParams) {
	var body apigen.ApiTokenRequest
	if !decodeBody(w, r, &body) {
		return
	}
	si, err := a.idStore.Service(r.Context(), principal(r), id)
	if err != nil {
		a.identityError(w, r, err)
		return
	}
	if !body.ExpiresAt.After(time.Now()) || body.ExpiresAt.After(time.Now().AddDate(1, 0, 0)) {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_expiry", "expiresAt must be in the future and within one year")
		return
	}
	scopes := make([]string, 0, len(body.Scopes))
	for _, s := range body.Scopes {
		scopes = append(scopes, string(s))
	}
	t, err := a.auth.CreateTokenFor(r.Context(), principal(r), si.ID, body.Name, scopes, body.ExpiresAt, requestID(w))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, apigen.ApiTokenIssued{Id: t.ID, Name: t.Name, Scopes: &scopes, ExpiresAt: t.ExpiresAt, Token: t.Value})
}

func (a *API) RevokeServiceToken(w http.ResponseWriter, r *http.Request, id, tokenID apigen.Id, _ apigen.RevokeServiceTokenParams) {
	if _, err := a.idStore.Service(r.Context(), principal(r), id); err != nil {
		a.identityError(w, r, err)
		return
	}
	err := a.auth.RevokeTokenFor(r.Context(), principal(r), id, tokenID, requestID(w))
	if errors.Is(err, auth.ErrNotFound) {
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
