package identity

import "testing"

func TestMemberOfCase(t *testing.T) {
	groups := []string{"CN=Samuori-Admins,OU=Groups,DC=Ad,DC=Example,DC=com", "Samuori-Admins"}
	if !memberOf(LDAP, groups, "samuori-admins") {
		t.Error("an LDAP group name should match regardless of case")
	}
	if !memberOf(LDAP, groups, "cn=samuori-admins,ou=groups,dc=ad,dc=example,dc=com") {
		t.Error("an LDAP group DN should match regardless of case")
	}
	if memberOf(OIDC, groups, "samuori-admins") {
		t.Error("OIDC group claims must match exactly")
	}
}
