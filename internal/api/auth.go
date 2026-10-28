package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
)

func (a *API) GetSetupStatus(w http.ResponseWriter, r *http.Request) {
	done, err := a.auth.SetupCompleted(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.SetupStatus{Completed: done})
}

func (a *API) BootstrapAdministrator(w http.ResponseWriter, r *http.Request) {
	var body apigen.BootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == nil || body.Password == nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "token and password are required")
		return
	}
	in := auth.BootstrapInput{Token: *body.Token, OrganizationName: body.OrganizationName, Username: body.Username, Password: *body.Password}
	if body.DisplayName != nil {
		in.DisplayName = *body.DisplayName
	}
	err := a.auth.Bootstrap(r.Context(), in, a.dataDir, w.Header().Get("X-Request-ID"), true)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrWeakPassword):
		apigen.WriteProblem(w, http.StatusBadRequest, "weak_password", err.Error())
	case errors.Is(err, auth.ErrBootstrapToken):
		apigen.WriteProblem(w, http.StatusForbidden, "invalid_bootstrap_token", err.Error())
	case errors.Is(err, auth.ErrBootstrapDone):
		apigen.WriteProblem(w, http.StatusConflict, "setup_complete", err.Error())
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) Login(w http.ResponseWriter, r *http.Request) {
	var body apigen.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "username and password are required")
		return
	}
	provider := ""
	if body.ProviderId != nil {
		provider = *body.ProviderId
	}
	var s auth.Session
	var err error
	if body.NewPassword != nil && *body.NewPassword != "" && provider == "" {
		s, err = a.auth.LoginReplacingTemporary(r.Context(), body.Username, *body.Password, *body.NewPassword, auth.ClientIP(r), w.Header().Get("X-Request-ID"))
	} else {
		s, err = a.auth.Login(r.Context(), provider, body.Username, *body.Password, auth.ClientIP(r), w.Header().Get("X-Request-ID"))
	}
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrPasswordChangeRequired):
		apigen.WriteProblem(w, http.StatusForbidden, "password_change_required", err.Error())
		return
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrSamePassword):
		apigen.WriteProblem(w, http.StatusBadRequest, "weak_password", err.Error())
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		apigen.WriteProblem(w, http.StatusUnauthorized, "invalid_credentials", err.Error())
		return
	case errors.Is(err, auth.ErrLocked), errors.Is(err, auth.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		apigen.WriteProblem(w, http.StatusTooManyRequests, "rate_limited", err.Error())
		return
	default:
		a.internalError(w, r, err)
		return
	}
	a.setCookie(w, auth.SessionCookie, s.Token, true)
	a.setCookie(w, auth.CSRFCookie, s.CSRF, false)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) Logout(w http.ResponseWriter, r *http.Request, _ apigen.LogoutParams) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		if err := a.auth.Logout(r.Context(), c.Value); err != nil {
			a.internalError(w, r, err)
			return
		}
	}
	a.clearCookie(w, auth.SessionCookie, true)
	a.clearCookie(w, auth.CSRFCookie, false)
	w.WriteHeader(http.StatusNoContent)
}

// browserSession returns the session cookie of a cookie-authenticated request; bearer-token callers have no session.
func browserSession(r *http.Request) (string, bool) {
	if r.Header.Get("Authorization") != "" {
		return "", false
	}
	c, err := r.Cookie(auth.SessionCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

func (a *API) GetSession(w http.ResponseWriter, r *http.Request) {
	a.sessionStatus(w, r, a.auth.Session)
}

func (a *API) ExtendSession(w http.ResponseWriter, r *http.Request, _ apigen.ExtendSessionParams) {
	a.sessionStatus(w, r, a.auth.ExtendSession)
}

func (a *API) sessionStatus(w http.ResponseWriter, r *http.Request, get func(context.Context, string) (auth.SessionStatus, error)) {
	token, ok := browserSession(r)
	if !ok {
		apigen.WriteProblem(w, http.StatusNotFound, "no_session", "this request has no browser session")
		return
	}
	s, err := get(r.Context(), token)
	if errors.Is(err, auth.ErrNotFound) {
		apigen.WriteProblem(w, http.StatusNotFound, "no_session", "the session has ended")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, apigen.SessionStatus{IdleExpiresAt: s.IdleExpiresAt, AbsoluteExpiresAt: s.AbsoluteExpiresAt, ServerTime: s.ServerTime,
		IdleTimeoutSeconds: int(s.Timeouts.Idle / time.Second), MaxSessionSeconds: int(s.Timeouts.Max / time.Second), WarningSeconds: int(s.Timeouts.Warning / time.Second)})
}

func (a *API) ListApiTokens(w http.ResponseWriter, r *http.Request, _ apigen.ListApiTokensParams) {
	tokens, err := a.auth.ListTokens(r.Context(), principal(r))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := apigen.ApiTokenList{Items: make([]apigen.ApiToken, 0, len(tokens))}
	for _, t := range tokens {
		scopes := t.Scopes
		out.Items = append(out.Items, apigen.ApiToken{Id: t.ID, Name: t.Name, Scopes: &scopes, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) CreateApiToken(w http.ResponseWriter, r *http.Request, _ apigen.CreateApiTokenParams) {
	var body apigen.ApiTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apigen.WriteProblem(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
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
	t, err := a.auth.CreateToken(r.Context(), principal(r), body.Name, scopes, body.ExpiresAt, w.Header().Get("X-Request-ID"))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, apigen.ApiTokenIssued{Id: t.ID, Name: t.Name, Scopes: &scopes, ExpiresAt: t.ExpiresAt, Token: t.Value})
}

func (a *API) RevokeApiToken(w http.ResponseWriter, r *http.Request, id apigen.Id, _ apigen.RevokeApiTokenParams) {
	err := a.auth.RevokeToken(r.Context(), principal(r), id, w.Header().Get("X-Request-ID"))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrNotFound):
		apigen.WriteProblem(w, http.StatusNotFound, "not_found", "not found")
	default:
		a.internalError(w, r, err)
	}
}

func (a *API) setCookie(w http.ResponseWriter, name, value string, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: httpOnly, Secure: a.secureCookies, SameSite: http.SameSiteStrictMode})
}

func (a *API) clearCookie(w http.ResponseWriter, name string, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: httpOnly, Secure: a.secureCookies, SameSite: http.SameSiteStrictMode})
}
