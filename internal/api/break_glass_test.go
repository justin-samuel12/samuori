package api

import (
	"fmt"
	"testing"
	"time"
)

// TestLastLocalAdministratorKept refuses to remove the last local administrator while directory administrators
// remain, and shows which provider a user and a role came from.
func TestLastLocalAdministratorKept(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	l.setup(t)
	admin := l.adminMember(t)

	_, _, me := l.json(t, admin, "GET", "/users", "", nil)
	first := me["items"].([]any)[0].(map[string]any)
	localBinding := first["roles"].([]any)[0].(map[string]any)["id"].(string)
	var org string
	if err := l.db.SQL.QueryRow(`SELECT org_id FROM users WHERE id = ?`, first["id"]).Scan(&org); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, ins := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO identity_providers (id, org_id, kind, name, display_name, enabled, config, created_by, created_at, updated_at) VALUES ('01JX0000000000000000000P01', ?, 'ldap', 'ad', 'Company AD', 1, '{}', 'system', ?, ?)`, []any{org, now, now}},
		{`INSERT INTO users (id, org_id, username, display_name, disabled, created_at, kind, provider_id, external_id) VALUES ('01JX0000000000000000000A01', ?, 'dir-admin', 'Directory Admin', 0, ?, 'user', '01JX0000000000000000000P01', 'dn:x')`, []any{org, now}},
		{`INSERT INTO role_bindings (id, org_id, principal_id, role, scope_type, scope_id, created_at, source_provider_id) VALUES ('01JX0000000000000000000B01', ?, '01JX0000000000000000000A01', 'administrator', 'organization', ?, ?, '01JX0000000000000000000P01')`, []any{org, org, now}},
	} {
		if _, err := l.db.SQL.Exec(ins.q, ins.args...); err != nil {
			t.Fatalf("%v", err)
		}
	}

	if st, _, b := l.json(t, admin, "POST", "/role-bindings/"+localBinding+"/revoke", "", nil); st != 409 || b["code"] != "last_local_administrator" {
		t.Fatalf("removing the only local administrator = %d %v, want 409 last_local_administrator", st, b)
	}

	_, _, users := l.json(t, admin, "GET", "/users", "", nil)
	for _, x := range users["items"].([]any) {
		u := x.(map[string]any)
		if u["username"] == "dir-admin" {
			role := u["roles"].([]any)[0].(map[string]any)
			if u["source"] != "Company AD" || role["source"] != "Company AD" {
				t.Errorf("directory user source = %v, role source = %v", u["source"], role["source"])
			}
		} else if _, ok := u["source"]; ok {
			t.Errorf("local user %v has a source", u["username"])
		}
	}

	st, _, second := l.json(t, admin, "POST", "/users", `{"username":"backup-admin","password":"correct-horse-battery-staple-1"}`, nil)
	if st != 201 {
		t.Fatalf("create user = %d %v", st, second)
	}
	if st, _, b := l.json(t, admin, "POST", "/role-bindings", fmt.Sprintf(`{"principalId":%q,"role":"administrator","scopeType":"organization","scopeId":%q}`, second["id"], org), nil); st != 201 {
		t.Fatalf("bind = %d %v", st, b)
	}
	if st, _, b := l.json(t, admin, "POST", "/role-bindings/"+localBinding+"/revoke", "", nil); st != 204 {
		t.Errorf("removing one of two local administrators = %d %v, want 204", st, b)
	}
}
