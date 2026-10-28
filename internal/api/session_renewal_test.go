package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/identity"
	"samuori.invalid/samuori/internal/sources"
)

func cookieOf(r *http.Response, name string) *http.Cookie {
	for _, c := range r.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (l *live) withSession(t *testing.T, method, path, session, csrf, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, l.srv.URL+"/api/v1"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session})
	if csrf != "" {
		req.Header.Set(auth.CSRFHeader, csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestSessionRenewalEndpoint(t *testing.T) {
	t.Run("SEC-AUTH-13", func(t *testing.T) {
		l := newLive(t, false)
		l.setup(t)
		c := l.client()
		login := l.call(t, c, "POST", "/auth/login", `{"username":"admin","password":"correct horse battery"}`, nil)
		oldSession, oldCSRF := cookieOf(login, auth.SessionCookie).Value, cookieOf(login, auth.CSRFCookie).Value

		var st apigen.SessionStatus
		r := l.call(t, c, "GET", "/auth/session", "", nil)
		if json.NewDecoder(r.Body).Decode(&st) != nil || st.RenewWith == nil || *st.RenewWith != apigen.SessionStatusRenewWithPassword {
			t.Fatalf("session status does not say how to renew: %+v", st)
		}
		if r := l.call(t, c, "POST", "/auth/session/renewal", `{"password":"correct horse battery"}`, nil); r.StatusCode != 403 {
			t.Fatalf("renewal without CSRF = %d", r.StatusCode)
		}
		h := map[string]string{auth.CSRFHeader: oldCSRF}
		if r := l.call(t, c, "POST", "/auth/session/renewal", `{"password":"wrong password here"}`, h); r.StatusCode != 400 {
			t.Fatalf("renewal with a wrong password = %d, want 400 (never 401)", r.StatusCode)
		}
		if r := l.call(t, c, "POST", "/auth/session/renewal", `{"password":"correct horse battery","returnTo":"https://evil.example/"}`, h); r.StatusCode != 400 {
			t.Fatalf("renewal with an absolute returnTo = %d", r.StatusCode)
		}
		r = l.call(t, c, "POST", "/auth/session/renewal", `{"password":"correct horse battery"}`, h)
		raw, _ := io.ReadAll(r.Body)
		if r.StatusCode != 200 {
			t.Fatalf("renewal = %d %s", r.StatusCode, raw)
		}
		assertMatchesContract(t, "POST", "/auth/session/renewal", r.StatusCode, r.Header, raw)
		session, csrf := cookieOf(r, auth.SessionCookie), cookieOf(r, auth.CSRFCookie)
		if session == nil || csrf == nil || session.Value == oldSession || csrf.Value == oldCSRF {
			t.Fatal("renewal did not issue a new session and CSRF token")
		}
		if !session.HttpOnly || session.SameSite != http.SameSiteStrictMode || csrf.HttpOnly {
			t.Fatalf("renewed cookies have the wrong flags: %+v / %+v", session, csrf)
		}
		if r := l.withSession(t, "GET", "/auth/session", oldSession, "", ""); r.StatusCode != 401 {
			t.Fatalf("the replaced session still works: %d", r.StatusCode)
		}
		if r := l.withSession(t, "POST", "/auth/session/renewal", oldSession, oldCSRF, `{"password":"correct horse battery"}`); r.StatusCode != 401 {
			t.Fatalf("the replaced session was renewed again: %d", r.StatusCode)
		}
		if r := l.withSession(t, "GET", "/auth/session", session.Value, "", ""); r.StatusCode != 200 {
			t.Fatalf("the new session does not work: %d", r.StatusCode)
		}

		h = map[string]string{auth.CSRFHeader: csrf.Value}
		r = l.call(t, c, "POST", "/me/api-tokens", `{"name":"ci","scopes":["read"],"expiresAt":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`, h)
		raw, _ = io.ReadAll(r.Body)
		var tok apigen.ApiTokenIssued
		if json.Unmarshal(raw, &tok) != nil || tok.Token == "" {
			t.Fatalf("create token = %d %s", r.StatusCode, raw)
		}
		if r := l.call(t, l.client(), "POST", "/auth/session/renewal", `{"password":"correct horse battery"}`, map[string]string{"Authorization": "Bearer " + tok.Token}); r.StatusCode != 403 {
			t.Fatalf("renewal with an API token = %d", r.StatusCode)
		}
	})
}

type fakeOIDC struct{ subject string }

func (f *fakeOIDC) AuthCodeURL(_ context.Context, _ identity.Provider, _, state, _, _ string) (string, error) {
	return "https://idp.example/authorize?state=" + url.QueryEscape(state), nil
}

func (f *fakeOIDC) Exchange(context.Context, identity.Provider, string, string, string, string, string) (identity.Claims, error) {
	return identity.Claims{Subject: f.subject, Username: f.subject}, nil
}

func (f *fakeOIDC) Check(context.Context, identity.Provider) error { return nil }

// TestSessionRenewalWithProvider renews an OIDC account's session through the provider: the callback replaces the
// session only for the same account and returns to a validated same-origin page.
func TestSessionRenewalWithProvider(t *testing.T) {
	idp := &fakeOIDC{subject: "sso-user"}
	l := newLiveWith(t, false, sources.DefaultConfig(), func(d *Deps) {
		d.Identity = identity.NewService(d.IdentityStore, idp, nil, testRedirect)
	})
	l.setup(t)
	admin := l.adminMember(t)
	st, _, p := l.json(t, admin, "POST", "/identity-providers", `{"kind":"oidc","name":"sso","displayName":"Example SSO","oidc":{"issuer":"https://idp.example","clientId":"samuori"}}`, nil)
	if st != 201 {
		t.Fatalf("create provider = %d %v", st, p)
	}
	provider := p["id"].(string)
	if st, _, b := l.json(t, admin, "POST", "/identity-providers/"+provider+"/state", `{"enabled":true}`, nil); st != 200 {
		t.Fatalf("enable provider = %d %v", st, b)
	}

	c := l.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	finish := func(providerURL string) *http.Response {
		t.Helper()
		u, err := url.Parse(providerURL)
		if err != nil || u.Query().Get("state") == "" {
			t.Fatalf("provider URL %q", providerURL)
		}
		return l.call(t, c, "GET", "/auth/oidc/callback?code=x&state="+url.QueryEscape(u.Query().Get("state")), "", nil)
	}
	r := l.call(t, c, "GET", "/auth/oidc/"+provider+"/start?returnTo="+url.QueryEscape("https://evil.example/"), "", nil)
	if r.StatusCode != 302 {
		t.Fatalf("start = %d", r.StatusCode)
	}
	if r := finish(r.Header.Get("Location")); r.StatusCode != 302 || r.Header.Get("Location") != "/" {
		t.Fatalf("sign-in with an absolute returnTo went to %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	r = l.call(t, c, "GET", "/auth/oidc/"+provider+"/start?returnTo="+url.QueryEscape("/projects/p1?tab=builds"), "", nil)
	r = finish(r.Header.Get("Location"))
	if r.Header.Get("Location") != "/projects/p1?tab=builds" {
		t.Fatalf("sign-in returned to %q", r.Header.Get("Location"))
	}
	oldSession, oldCSRF := cookieOf(r, auth.SessionCookie).Value, cookieOf(r, auth.CSRFCookie).Value

	var status apigen.SessionStatus
	if json.NewDecoder(l.withSession(t, "GET", "/auth/session", oldSession, "", "").Body).Decode(&status) != nil || status.RenewWith == nil ||
		*status.RenewWith != apigen.SessionStatusRenewWithProvider || status.ProviderName == nil || *status.ProviderName != "Example SSO" {
		t.Fatalf("SSO session status = %+v", status)
	}
	if r := l.withSession(t, "POST", "/auth/session/renewal", oldSession, oldCSRF, `{"returnTo":"//evil.example/x"}`); r.StatusCode != 400 {
		t.Fatalf("renewal with a protocol-relative returnTo = %d", r.StatusCode)
	}

	renew := func() string {
		t.Helper()
		r := l.call(t, c, "POST", "/auth/session/renewal", `{"returnTo":"/builds/b1"}`, map[string]string{auth.CSRFHeader: oldCSRF})
		raw, _ := io.ReadAll(r.Body)
		var out apigen.SessionRenewalRedirect
		if r.StatusCode != 202 || json.Unmarshal(raw, &out) != nil {
			t.Fatalf("SSO renewal = %d %s", r.StatusCode, raw)
		}
		return out.RedirectUrl
	}
	idp.subject = "someone-else"
	if r := finish(renew()); r.Header.Get("Location") != "/?signin=failed" || cookieOf(r, auth.SessionCookie) != nil {
		t.Fatalf("a renewal finished as another account went to %q", r.Header.Get("Location"))
	}
	if r := l.withSession(t, "GET", "/auth/session", oldSession, "", ""); r.StatusCode != 200 {
		t.Fatalf("a refused renewal ended the session: %d", r.StatusCode)
	}

	idp.subject = "sso-user"
	r = finish(renew())
	if r.Header.Get("Location") != "/builds/b1" {
		t.Fatalf("renewal returned to %q", r.Header.Get("Location"))
	}
	fresh := cookieOf(r, auth.SessionCookie)
	if fresh == nil || fresh.Value == oldSession {
		t.Fatal("SSO renewal did not issue a new session")
	}
	if r := l.withSession(t, "GET", "/auth/session", oldSession, "", ""); r.StatusCode != 401 {
		t.Fatalf("the renewed session still works: %d", r.StatusCode)
	}
	if r := l.withSession(t, "GET", "/auth/session", fresh.Value, "", ""); r.StatusCode != 200 {
		t.Fatalf("the new session does not work: %d", r.StatusCode)
	}
	var renewed int
	if err := l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'session.renew' AND outcome = 'succeeded'`).Scan(&renewed); err != nil || renewed != 1 {
		t.Fatalf("SSO renewal audited %d times: %v", renewed, err)
	}
}
