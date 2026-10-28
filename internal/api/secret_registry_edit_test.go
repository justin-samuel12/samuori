package api

import (
	"strings"
	"testing"
)

// TestSecretAndImageSourceEdits: grant, allowed-secret and image-source edits and deletes, and signing-key retirement,
// are administrator-only over HTTP; a grant edit reports the bindings it disabled.
func TestSecretAndImageSourceEdits(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)

	_, _, provs := l.json(t, admin, "GET", "/secret-providers", "", nil)
	var local string
	for _, x := range provs["items"].([]any) {
		if m := x.(map[string]any); m["type"] == "local" {
			local = m["id"].(string)
		}
	}
	st, _, sec := l.json(t, admin, "POST", "/secret-providers/"+local+"/local-secrets", `{"name":"npm-token","type":"text","value":"hunter22"}`, map[string]string{"Idempotency-Key": "edit-secret-0000001"})
	if st != 201 {
		t.Fatalf("store = %d %v", st, sec)
	}
	st, _, g := l.json(t, admin, "POST", "/projects/"+projA+"/secret-grants", `{"providerId":"`+local+`","providerReference":"`+sec["id"].(string)+`","usage":["build","test"],"reason":"packages"}`, map[string]string{"Idempotency-Key": "edit-grant-00000001"})
	if st != 201 {
		t.Fatalf("grant = %d %v", st, g)
	}
	grant := "/projects/" + projA + "/secret-grants/" + g["id"].(string)
	if st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/secret-grant-bindings", `{"logicalId":"npm-token","grantId":"`+g["id"].(string)+`"}`, map[string]string{"Idempotency-Key": "edit-bind-000000001"}); st != 201 {
		t.Fatalf("bind = %d %v", st, b)
	}
	narrow := `{"name":"npm","usage":["build"],"eligiblePoolIds":[],"reason":"builds only"}`
	if st, _, _ := l.json(t, alice, "PUT", grant, narrow, nil); st != 403 {
		t.Errorf("developer grant edit = %d, want 403", st)
	}
	st, _, u := l.json(t, admin, "PUT", grant, narrow, nil)
	if st != 200 || u["grant"].(map[string]any)["name"] != "npm" || len(u["disabled"].([]any)) != 1 {
		t.Fatalf("narrowing edit = %d %v", st, u)
	}
	_, _, list := l.json(t, alice, "GET", "/projects/"+projA+"/secret-grants", "", nil)
	if items := list["items"].([]any); len(items) != 1 || items[0].(map[string]any)["providerReference"] != nil || len(list["uses"].([]any)) != 0 {
		t.Errorf("member view after narrowing = %v", list)
	}

	if st, _, _ := l.json(t, alice, "PUT", "/allowed-secrets/01JAB3K0000000000000000099", `{"label":"x"}`, nil); st != 403 {
		t.Errorf("developer allowed-secret edit = %d, want 403", st)
	}

	_, _, teams := l.json(t, admin, "GET", "/teams", "", nil)
	team := teams["items"].([]any)[0].(map[string]any)["id"].(string)
	st, _, src := l.json(t, admin, "POST", "/image-sources", `{"name":"corp","upstream":"registry.corp.example","mirror":"mirror.example:5000","repositoryPatterns":["base/*"],"teamIds":["`+team+`"]}`, nil)
	if st != 201 {
		t.Fatalf("image source = %d %v", st, src)
	}
	path := "/image-sources/" + src["id"].(string)
	body := `{"name":"corp-base","repositoryPatterns":["base/node/*"],"teamIds":["` + team + `"]}`
	if st, _, _ := l.json(t, alice, "PUT", path, body, nil); st != 403 {
		t.Errorf("developer image source edit = %d, want 403", st)
	}
	if st, _, _ := l.json(t, alice, "DELETE", path, "", nil); st != 403 {
		t.Errorf("developer image source delete = %d, want 403", st)
	}
	if st, _, b := l.json(t, admin, "PUT", path, `{"name":"corp","repositoryPatterns":["*"],"teamIds":["`+team+`"]}`, nil); st != 422 {
		t.Errorf("bare * = %d %v", st, b)
	}
	if st, _, b := l.json(t, admin, "PUT", path, body, nil); st != 200 || b["name"] != "corp-base" || b["upstream"] != "registry.corp.example" {
		t.Errorf("image source edit = %d %v", st, b)
	}
	if st, _, _ := l.json(t, admin, "DELETE", path, "", nil); st != 204 {
		t.Errorf("image source delete = %d", st)
	}

	st, _, key := l.json(t, admin, "POST", "/signing-keys", `{"name":"release"}`, nil)
	if st != 201 {
		t.Fatalf("signing key = %d %v", st, key)
	}
	retire := "/signing-keys/" + key["id"].(string) + "/retire"
	if st, _, _ := l.json(t, alice, "POST", retire, "", nil); st != 403 {
		t.Errorf("developer retire = %d, want 403", st)
	}
	if st, _, _ := l.json(t, admin, "POST", retire, "", nil); st != 204 {
		t.Fatalf("retire = %d", st)
	}
	if st, _, _ := l.json(t, admin, "POST", retire, "", nil); st != 404 {
		t.Errorf("retiring twice = %d, want 404", st)
	}
	_, _, keys := l.json(t, alice, "GET", "/signing-keys", "", nil)
	k := keys["items"].([]any)[0].(map[string]any)
	if k["state"] != "retired" || !strings.Contains(k["publicKey"].(string), "PUBLIC KEY") {
		t.Errorf("retired key = %v", k)
	}
}
