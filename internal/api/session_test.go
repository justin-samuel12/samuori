package api

import (
	"encoding/json"
	"testing"

	"samuori.invalid/samuori/internal/api/apigen"
)

func TestSessionEndpoints(t *testing.T) {
	t.Run("SEC-AUTH-11", func(t *testing.T) {
		l := newLive(t, false)
		l.setup(t)
		c, csrf := l.login(t)
		h := map[string]string{"X-CSRF-Token": csrf}
		var st apigen.SessionStatus
		r := l.call(t, c, "GET", "/auth/session", "", nil)
		if r.StatusCode != 200 || json.NewDecoder(r.Body).Decode(&st) != nil || !st.IdleExpiresAt.Before(st.AbsoluteExpiresAt) || st.WarningSeconds != 120 {
			t.Fatalf("session status = %d %+v", r.StatusCode, st)
		}
		if r := l.call(t, c, "POST", "/auth/session/extend", "", nil); r.StatusCode != 403 {
			t.Fatalf("extend without CSRF = %d", r.StatusCode)
		}
		if r := l.call(t, c, "POST", "/auth/session/extend", "", h); r.StatusCode != 200 {
			t.Fatalf("extend = %d", r.StatusCode)
		}
		for _, body := range []string{
			`{"session":{"idleTimeoutMinutes":14,"maxSessionHours":12,"warningSeconds":120}}`,
			`{"session":{"idleTimeoutMinutes":30,"maxSessionHours":25,"warningSeconds":120}}`,
			`{"session":{"idleTimeoutMinutes":30,"maxSessionHours":12,"warningSeconds":601}}`,
			`{"session":{"idleTimeoutMinutes":120,"maxSessionHours":1,"warningSeconds":120}}`,
		} {
			if r := l.call(t, c, "PUT", "/identity-settings", body, h); r.StatusCode < 400 || r.StatusCode >= 500 {
				t.Errorf("out-of-bounds %s = %d", body, r.StatusCode)
			}
		}
		var got apigen.IdentitySettings
		r = l.call(t, c, "PUT", "/identity-settings", `{"session":{"idleTimeoutMinutes":20,"maxSessionHours":8,"warningSeconds":60}}`, h)
		if r.StatusCode != 200 || json.NewDecoder(r.Body).Decode(&got) != nil || got.Session.IdleTimeoutMinutes != 20 || got.Session.MaxSessionHours != 8 || got.LocalLogin != "all" {
			t.Fatalf("saving session timeouts = %d %+v", r.StatusCode, got)
		}
		r = l.call(t, c, "GET", "/auth/session", "", nil)
		if r.StatusCode != 200 || json.NewDecoder(r.Body).Decode(&st) != nil || st.IdleTimeoutSeconds != 1200 || st.MaxSessionSeconds != 8*3600 {
			t.Fatalf("session status after the change = %d %+v", r.StatusCode, st)
		}
		if r := l.call(t, c, "POST", "/auth/logout", "", h); r.StatusCode != 204 {
			t.Fatalf("logout = %d", r.StatusCode)
		}
		if r := l.call(t, c, "GET", "/auth/session", "", nil); r.StatusCode != 401 {
			t.Fatalf("status after sign-out = %d", r.StatusCode)
		}
	})
}
