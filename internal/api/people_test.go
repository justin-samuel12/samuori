package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
)

type browser struct {
	l    *live
	c    *http.Client
	csrf string
}

func (l *live) signIn(t *testing.T, username, password string) *browser {
	t.Helper()
	c := l.client()
	resp := l.call(t, c, "POST", "/auth/login", `{"username":"`+username+`","password":"`+password+`"}`, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("login %s = %d", username, resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == auth.CSRFCookie {
			return &browser{l, c, ck.Value}
		}
	}
	t.Fatal("no CSRF cookie")
	return nil
}

func (b *browser) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	resp := b.l.call(t, b.c, method, path, body, map[string]string{auth.CSRFHeader: b.csrf})
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (b *browser) want(t *testing.T, status int, method, path, body string) []byte {
	t.Helper()
	got, out := b.do(t, method, path, body)
	if got != status {
		t.Fatalf("%s %s = %d %s, want %d", method, path, got, out, status)
	}
	return out
}

const bobPassword = "bob's long passphrase"

func peopleSetup(t *testing.T) (*live, *browser, string) {
	t.Helper()
	l := newLive(t, false)
	l.setup(t)
	admin := l.signIn(t, "admin", "correct horse battery")
	u := decode[apigen.User](t, admin.want(t, 201, "POST", "/users", `{"username":"dana","password":"`+bobPassword+`"}`))
	return l, admin, u.Id
}

func loginStatus(t *testing.T, l *live, body string) int {
	t.Helper()
	return l.call(t, l.client(), "POST", "/auth/login", body, nil).StatusCode
}

func TestTeamRenameAndDelete(t *testing.T) {
	l, admin, bob := peopleSetup(t)
	a := decode[apigen.Team](t, admin.want(t, 201, "POST", "/teams", `{"name":"alpha"}`))
	admin.want(t, 201, "POST", "/teams", `{"name":"beta"}`)
	if got := decode[apigen.Team](t, admin.want(t, 200, "PATCH", "/teams/"+a.Id, `{"name":"gamma"}`)); got.Name != "gamma" {
		t.Fatalf("renamed = %+v", got)
	}
	admin.want(t, 409, "PATCH", "/teams/"+a.Id, `{"name":"beta"}`)
	admin.want(t, 400, "PATCH", "/teams/"+a.Id, `{"name":"Not Valid"}`)

	rb := decode[apigen.RoleBinding](t, admin.want(t, 201, "POST", "/role-bindings", `{"principalId":"`+bob+`","role":"developer","scopeType":"team","scopeId":"`+a.Id+`"}`))
	if out := admin.want(t, 409, "DELETE", "/teams/"+a.Id, ""); !strings.Contains(string(out), "1 role binding") {
		t.Fatalf("in-use refusal = %s", out)
	}
	admin.want(t, 204, "POST", "/role-bindings/"+rb.Id+"/revoke", "")

	var org string
	_ = l.db.SQL.QueryRow(`SELECT id FROM organizations`).Scan(&org)
	now := time.Now().UTC()
	if _, err := l.db.SQL.Exec(`INSERT INTO projects (id, org_id, team_id, name, publication_mode, created_at, updated_at, state) VALUES ('01JX00000000000000000PRJ01', ?, ?, 'old', 'manual', ?, ?, 'purged')`, org, a.Id, now, now); err != nil {
		t.Fatal(err)
	}
	if out := admin.want(t, 409, "DELETE", "/teams/"+a.Id, ""); !strings.Contains(string(out), "1 project") {
		t.Fatalf("purged project did not keep the team: %s", out)
	}
	if _, err := l.db.SQL.Exec(`DELETE FROM projects`); err != nil {
		t.Fatal(err)
	}
	admin.want(t, 204, "DELETE", "/teams/"+a.Id, "")
	admin.want(t, 404, "DELETE", "/teams/"+a.Id, "")
	var audited int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('team.rename', 'team.delete')`).Scan(&audited)
	if audited != 2 {
		t.Fatalf("audited team changes = %d", audited)
	}
}

func TestDisableUser(t *testing.T) {
	l, admin, bob := peopleSetup(t)
	session := l.signIn(t, "dana", bobPassword)
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	tok := decode[apigen.ApiTokenIssued](t, session.want(t, 201, "POST", "/me/api-tokens", `{"name":"ci","expiresAt":"`+exp+`","scopes":["read"]}`))
	bearer := map[string]string{"Authorization": "Bearer " + tok.Token}

	admin.want(t, 204, "POST", "/users/"+bob+"/state", `{"enabled":false}`)
	if got, _ := session.do(t, "GET", "/me", ""); got != 401 {
		t.Fatalf("disabled user's session = %d", got)
	}
	if r := l.call(t, &http.Client{}, "GET", "/me", "", bearer); r.StatusCode != 401 {
		t.Fatalf("disabled user's token = %d", r.StatusCode)
	}
	if got := loginStatus(t, l, `{"username":"dana","password":"`+bobPassword+`"}`); got != 401 {
		t.Fatalf("disabled login = %d", got)
	}
	admin.want(t, 204, "POST", "/users/"+bob+"/state", `{"enabled":true}`)
	l.signIn(t, "dana", bobPassword)
	if r := l.call(t, &http.Client{}, "GET", "/me", "", bearer); r.StatusCode != 401 {
		t.Fatalf("token revived after re-enable = %d", r.StatusCode)
	}

	var adminID string
	_ = l.db.SQL.QueryRow(`SELECT id FROM users WHERE username = 'admin'`).Scan(&adminID)
	admin.want(t, 409, "POST", "/users/"+adminID+"/state", `{"enabled":false}`)

	// A directory administrator may not disable the last local administrator.
	var org string
	_ = l.db.SQL.QueryRow(`SELECT id FROM organizations`).Scan(&org)
	admin.want(t, 201, "POST", "/role-bindings", `{"principalId":"`+bob+`","role":"administrator","scopeType":"organization","scopeId":"`+org+`"}`)
	bobAdmin := l.signIn(t, "dana", bobPassword)
	if _, err := l.db.SQL.Exec(`UPDATE users SET provider_id = 'dir', external_id = 'b' WHERE id = ?`, bob); err != nil {
		t.Fatal(err)
	}
	if out := bobAdmin.want(t, 409, "POST", "/users/"+adminID+"/state", `{"enabled":false}`); !strings.Contains(string(out), "last_local_administrator") {
		t.Fatalf("last local admin refusal = %s", out)
	}
}

func TestAdminPasswordReset(t *testing.T) {
	l, admin, bob := peopleSetup(t)
	session := l.signIn(t, "dana", bobPassword)
	resp := l.call(t, admin.c, "POST", "/users/"+bob+"/password-reset", "", map[string]string{auth.CSRFHeader: admin.csrf})
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("reset = %d, cache %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var tmp apigen.TemporaryPassword
	_ = json.NewDecoder(resp.Body).Decode(&tmp)
	if len(tmp.TemporaryPassword) < 12 || !tmp.ExpiresAt.After(time.Now().Add(23*time.Hour)) {
		t.Fatalf("temporary password = %+v", tmp)
	}
	if got, _ := session.do(t, "GET", "/me", ""); got != 401 {
		t.Fatalf("session after reset = %d", got)
	}
	if got := loginStatus(t, l, `{"username":"dana","password":"`+bobPassword+`"}`); got != 401 {
		t.Fatalf("old password after reset = %d", got)
	}
	if got := loginStatus(t, l, `{"username":"dana","password":"`+tmp.TemporaryPassword+`"}`); got != 403 {
		t.Fatalf("temporary password without a new one = %d", got)
	}
	if got := loginStatus(t, l, `{"username":"dana","password":"`+tmp.TemporaryPassword+`","newPassword":"short"}`); got != 400 {
		t.Fatalf("weak replacement = %d", got)
	}
	if got := loginStatus(t, l, `{"username":"dana","password":"`+tmp.TemporaryPassword+`","newPassword":"bob's brand new phrase"}`); got != 204 {
		t.Fatalf("replacing the temporary password = %d", got)
	}
	if got := loginStatus(t, l, `{"username":"dana","password":"`+tmp.TemporaryPassword+`"}`); got != 401 {
		t.Fatalf("temporary password reused = %d", got)
	}
	l.signIn(t, "dana", "bob's brand new phrase")

	tmp = decode[apigen.TemporaryPassword](t, admin.want(t, 200, "POST", "/users/"+bob+"/password-reset", ""))
	if _, err := l.db.SQL.Exec(`UPDATE user_credentials SET temporary_until = ? WHERE user_id = ?`, time.Now().Add(-time.Minute).UTC(), bob); err != nil {
		t.Fatal(err)
	}
	if got := loginStatus(t, l, `{"username":"dana","password":"`+tmp.TemporaryPassword+`","newPassword":"bob's other new phrase"}`); got != 401 {
		t.Fatalf("expired temporary password = %d", got)
	}

	var adminID string
	_ = l.db.SQL.QueryRow(`SELECT id FROM users WHERE username = 'admin'`).Scan(&adminID)
	admin.want(t, 409, "POST", "/users/"+adminID+"/password-reset", "")
	var hashes int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'user.password-reset'`).Scan(&hashes)
	if hashes != 2 {
		t.Fatalf("audited resets = %d", hashes)
	}
	list := string(admin.want(t, 200, "GET", "/users", ""))
	if strings.Contains(list, "argon2") || strings.Contains(list, "assword") {
		t.Fatalf("user list exposes credentials: %s", list)
	}
}

func TestSelfPasswordChange(t *testing.T) {
	l, _, _ := peopleSetup(t)
	other := l.signIn(t, "dana", bobPassword)
	me := l.signIn(t, "dana", bobPassword)
	me.want(t, 400, "POST", "/me/password", `{"currentPassword":"not my password","newPassword":"bob's next long phrase"}`)
	me.want(t, 400, "POST", "/me/password", `{"currentPassword":"`+bobPassword+`","newPassword":"short"}`)
	me.want(t, 204, "POST", "/me/password", `{"currentPassword":"`+bobPassword+`","newPassword":"bob's next long phrase"}`)
	if got, _ := other.do(t, "GET", "/me", ""); got != 401 {
		t.Fatalf("other session after change = %d", got)
	}
	me.want(t, 200, "GET", "/me", "")
	l.signIn(t, "dana", "bob's next long phrase")

	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	tok := decode[apigen.ApiTokenIssued](t, me.want(t, 201, "POST", "/me/api-tokens", `{"name":"rw","expiresAt":"`+exp+`","scopes":["read","write"]}`))
	if r := l.call(t, &http.Client{}, "POST", "/me/password", `{"currentPassword":"bob's next long phrase","newPassword":"bob's third long phrase"}`, map[string]string{"Authorization": "Bearer " + tok.Token}); r.StatusCode != 403 {
		t.Fatalf("password change with a token = %d", r.StatusCode)
	}
	for i := range 5 {
		if got, _ := me.do(t, "POST", "/me/password", `{"currentPassword":"wrong guess `+string(rune('a'+i))+`","newPassword":"bob's third long phrase"}`); got != 400 {
			t.Fatalf("wrong guess %d = %d", i, got)
		}
	}
	me.want(t, 429, "POST", "/me/password", `{"currentPassword":"bob's next long phrase","newPassword":"bob's third long phrase"}`)
}

func TestRoleChangeAndRenames(t *testing.T) {
	l, admin, bob := peopleSetup(t)
	team := decode[apigen.Team](t, admin.want(t, 201, "POST", "/teams", `{"name":"alpha"}`))
	rb := decode[apigen.RoleBinding](t, admin.want(t, 201, "POST", "/role-bindings", `{"principalId":"`+bob+`","role":"developer","scopeType":"team","scopeId":"`+team.Id+`"}`))
	if got := decode[apigen.RoleBinding](t, admin.want(t, 200, "PATCH", "/role-bindings/"+rb.Id, `{"role":"maintainer"}`)); got.Role != "maintainer" || got.ScopeId != team.Id {
		t.Fatalf("changed = %+v", got)
	}
	var reason string
	_ = l.db.SQL.QueryRow(`SELECT reason FROM audit_events WHERE action = 'role.change'`).Scan(&reason)
	if reason != "developer@team -> maintainer@team" {
		t.Fatalf("audit reason = %q", reason)
	}
	var adminBinding string
	_ = l.db.SQL.QueryRow(`SELECT rb.id FROM role_bindings rb JOIN users u ON u.id = rb.principal_id WHERE u.username = 'admin'`).Scan(&adminBinding)
	admin.want(t, 409, "PATCH", "/role-bindings/"+adminBinding, `{"role":"developer"}`)

	admin.want(t, 204, "PATCH", "/users/"+bob, `{"displayName":"Bob Builder"}`)
	bobSession := l.signIn(t, "dana", bobPassword)
	bobSession.want(t, 204, "PATCH", "/me", `{"displayName":"Robert"}`)
	if me := decode[apigen.Me](t, bobSession.want(t, 200, "GET", "/me", "")); me.DisplayName == nil || *me.DisplayName != "Robert" {
		t.Fatalf("display name = %v", me.DisplayName)
	}

	si := decode[serviceView](t, admin.want(t, 201, "POST", "/service-identities", `{"username":"ci"}`))
	if got := decode[serviceView](t, admin.want(t, 200, "PATCH", "/service-identities/"+si.ID, `{"displayName":"Release pipeline"}`)); got.DisplayName != "Release pipeline" || got.Username != "ci" {
		t.Fatalf("service identity = %+v", got)
	}
}

func TestDeleteIdentityProvider(t *testing.T) {
	l, admin, bob := peopleSetup(t)
	var org string
	_ = l.db.SQL.QueryRow(`SELECT id FROM organizations`).Scan(&org)
	now := time.Now().UTC()
	for _, id := range []string{"01JX0000000000000000000P01", "01JX0000000000000000000P02"} {
		if _, err := l.db.SQL.Exec(`INSERT INTO identity_providers (id, org_id, kind, name, display_name, enabled, config, created_by, created_at, updated_at) VALUES (?, ?, 'ldap', ?, 'Directory', 0, '{}', 'system', ?, ?)`, id, org, "p"+id[len(id)-1:], now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.db.SQL.Exec(`UPDATE users SET provider_id = '01JX0000000000000000000P02', external_id = 'b' WHERE id = ?`, bob); err != nil {
		t.Fatal(err)
	}
	admin.want(t, 204, "DELETE", "/identity-providers/01JX0000000000000000000P01", "")
	if out := admin.want(t, 409, "DELETE", "/identity-providers/01JX0000000000000000000P02", ""); !strings.Contains(string(out), "provider_in_use") {
		t.Fatalf("used provider = %s", out)
	}
}

func TestPeopleManagementNeedsOrganizationAdministrator(t *testing.T) {
	l, admin, bob := peopleSetup(t)
	team := decode[apigen.Team](t, admin.want(t, 201, "POST", "/teams", `{"name":"alpha"}`))
	rb := decode[apigen.RoleBinding](t, admin.want(t, 201, "POST", "/role-bindings", `{"principalId":"`+bob+`","role":"administrator","scopeType":"team","scopeId":"`+team.Id+`"}`))
	si := decode[serviceView](t, admin.want(t, 201, "POST", "/service-identities", `{"username":"ci"}`))
	var adminID string
	_ = l.db.SQL.QueryRow(`SELECT id FROM users WHERE username = 'admin'`).Scan(&adminID)
	teamAdmin := l.signIn(t, "dana", bobPassword)
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", "/teams/" + team.Id, `{"name":"beta"}`},
		{"DELETE", "/teams/" + team.Id, ""},
		{"PATCH", "/users/" + adminID, `{"displayName":"x"}`},
		{"POST", "/users/" + adminID + "/state", `{"enabled":false}`},
		{"POST", "/users/" + adminID + "/password-reset", ""},
		{"PATCH", "/role-bindings/" + rb.Id, `{"role":"developer"}`},
		{"PATCH", "/service-identities/" + si.ID, `{"displayName":"x"}`},
		{"DELETE", "/identity-providers/01JX0000000000000000000P01", ""},
	} {
		teamAdmin.want(t, 403, c.method, c.path, c.body)
	}
}
