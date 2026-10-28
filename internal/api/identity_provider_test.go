package api

import (
	"strings"
	"testing"
)

// TestIdentityProviderUpdate edits an LDAP provider: the stored bind secret is kept, server URLs are normalized,
// bad settings are refused and non-administrators cannot edit it.
func TestIdentityProviderUpdate(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, _, _ := twoTeams(t, l)
	admin := l.adminMember(t)

	_, _, provs := l.json(t, admin, "GET", "/secret-providers", "", nil)
	var local string
	for _, x := range provs["items"].([]any) {
		if m := x.(map[string]any); m["type"] == "local" {
			local = m["id"].(string)
		}
	}
	st, _, sec := l.json(t, admin, "POST", "/secret-providers/"+local+"/local-secrets", `{"name":"identity-ad-bind-password","type":"text","value":"s3cret"}`, map[string]string{"Idempotency-Key": "idp-update-secret-0001"})
	if st != 201 {
		t.Fatalf("store secret = %d %v", st, sec)
	}
	ldap := `"ldap":{"url":%s,"bindDn":"cn=svc,dc=example,dc=com","userBaseDn":"dc=example,dc=com","userFilter":"(uid={username})"}`
	st, _, p := l.json(t, admin, "POST", "/identity-providers", `{"kind":"ldap","name":"ad","secretId":"`+sec["id"].(string)+`",`+strings.Replace(ldap, "%s", `"ldaps://dc1.example.com"`, 1)+`}`, nil)
	if st != 201 {
		t.Fatalf("create = %d %v", st, p)
	}
	path := "/identity-providers/" + p["id"].(string)

	st, _, u := l.json(t, admin, "PUT", path, `{"displayName":"Company AD",`+strings.Replace(ldap, "%s", `" ldaps://dc1.example.com,ldaps://dc2.example.com:636 "`, 1)+`}`, nil)
	if st != 200 {
		t.Fatalf("update = %d %v", st, u)
	}
	if got := u["ldap"].(map[string]any)["url"]; got != "ldaps://dc1.example.com ldaps://dc2.example.com:636" {
		t.Errorf("url = %q", got)
	}
	if u["displayName"] != "Company AD" || u["hasSecret"] != true || u["name"] != "ad" {
		t.Errorf("update kept the wrong fields: %v", u)
	}
	if st, _, b := l.json(t, admin, "PUT", path, `{`+strings.Replace(ldap, "%s", `"https://dc1.example.com"`, 1)+`}`, nil); st != 422 {
		t.Errorf("https URL = %d %v, want 422", st, b)
	}
	if st, _, _ := l.json(t, alice, "PUT", path, `{"displayName":"mine"}`, nil); st != 403 {
		t.Errorf("developer update = %d, want 403", st)
	}
}
