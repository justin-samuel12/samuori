package api

import (
	"strings"
	"testing"
)

// TestRegistryEditAndDelete: registries and credentials can be edited and deleted by administrators only, deletes are
// refused while anything refers to them, and an edit never sends a stored credential somewhere new.
func TestRegistryEditAndDelete(t *testing.T) {
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
	secret := func(name string) string {
		st, _, sec := l.json(t, admin, "POST", "/secret-providers/"+local+"/local-secrets", `{"name":"`+name+`","type":"text","value":"hunter22"}`, map[string]string{"Idempotency-Key": "reg-edit-" + name})
		if st != 201 {
			t.Fatalf("store secret = %d %v", st, sec)
		}
		return sec["id"].(string)
	}
	pw, pw2 := secret("lab-password"), secret("lab-password-2")

	st, _, reg := l.json(t, admin, "POST", "/registries", `{"name":"lab","kind":"oci","endpoint":"registry.example:5000"}`, nil)
	if st != 201 {
		t.Fatalf("create registry = %d %v", st, reg)
	}
	regID := reg["id"].(string)
	st, _, prof := l.json(t, admin, "POST", "/registry-credential-profiles", `{"name":"lab-push","registryId":"`+regID+`","type":"oci-basic","purpose":"push","username":"ci","secretRef":"`+pw+`"}`, nil)
	if st != 201 {
		t.Fatalf("create profile = %d %v", st, prof)
	}
	profID := prof["id"].(string)

	for who, m := range map[string]member{"developer": alice} {
		if st, _, _ := l.json(t, m, "PUT", "/registries/"+regID, `{"name":"lab2","endpoint":"registry.example:5000"}`, nil); st != 403 {
			t.Errorf("%s edit registry = %d, want 403", who, st)
		}
		if st, _, _ := l.json(t, m, "DELETE", "/registries/"+regID, "", nil); st != 403 {
			t.Errorf("%s delete registry = %d, want 403", who, st)
		}
		if st, _, _ := l.json(t, m, "PUT", "/registry-credential-profiles/"+profID, `{"name":"x","type":"anonymous"}`, nil); st != 403 {
			t.Errorf("%s edit profile = %d, want 403", who, st)
		}
		if st, _, _ := l.json(t, m, "DELETE", "/registry-credential-profiles/"+profID, "", nil); st != 403 {
			t.Errorf("%s delete profile = %d, want 403", who, st)
		}
	}

	t.Run("registry edits are validated like create and keep the kind", func(t *testing.T) {
		for name, body := range map[string]string{
			"bad name":     `{"name":"Lab","endpoint":"registry.example:5000"}`,
			"bad endpoint": `{"name":"lab","endpoint":"https://registry.example/path"}`,
			"metadata":     `{"name":"lab","endpoint":"169.254.169.254"}`,
		} {
			if st, _, b := l.json(t, admin, "PUT", "/registries/"+regID, body, nil); st != 422 {
				t.Errorf("%s = %d %v", name, st, b)
			}
		}
		st, _, b := l.json(t, admin, "PUT", "/registries/"+regID, `{"name":"lab-renamed","endpoint":"registry.example:5000","kind":"ecr","allowPrivateNetwork":true}`, nil)
		if st != 200 || b["name"] != "lab-renamed" || b["kind"] != "oci" || b["allowPrivateNetwork"] != true {
			t.Fatalf("rename = %d %v", st, b)
		}
	})

	t.Run("a stored credential is never redirected", func(t *testing.T) {
		for name, body := range map[string]string{
			"endpoint":   `{"name":"lab-renamed","endpoint":"attacker.example:5000"}`,
			"plain HTTP": `{"name":"lab-renamed","endpoint":"registry.example:5000","plainHttp":true}`,
		} {
			st, _, b := l.json(t, admin, "PUT", "/registries/"+regID, body, nil)
			if st != 422 || !strings.Contains(b["message"].(string), "lab-push") {
				t.Errorf("%s change with a stored secret = %d %v", name, st, b)
			}
		}
		_, _, regs := l.json(t, admin, "GET", "/registries", "", nil)
		if ep := regs["items"].([]any)[0].(map[string]any)["endpoint"]; ep != "registry.example:5000" {
			t.Fatalf("endpoint after refused edits = %v", ep)
		}
		if st, _, b := l.json(t, admin, "PUT", "/registry-credential-profiles/"+profID, `{"name":"lab-push","type":"oci-basic","username":"ci"}`, nil); st != 422 {
			t.Errorf("basic profile without its secret = %d %v", st, b)
		}
	})

	t.Run("profile edits", func(t *testing.T) {
		st, _, b := l.json(t, admin, "PUT", "/registry-credential-profiles/"+profID, `{"name":"lab-publisher","type":"oci-basic","username":"robot","secretRef":"`+pw2+`","repositoryScopes":["payments/*"],"purpose":"pull","registryId":"other"}`, nil)
		if st != 200 || b["name"] != "lab-publisher" || b["username"] != "robot" || b["secretRef"] != pw2 || b["purpose"] != "push" || b["registryId"] != regID {
			t.Fatalf("edit profile = %d %v", st, b)
		}
		var co bool
		_ = l.db.SQL.QueryRow(`SELECT controller_only FROM secrets WHERE id = ?`, pw2).Scan(&co)
		if !co {
			t.Error("the new secret is not reserved for the controller")
		}
	})

	st, _, dest := l.json(t, admin, "POST", "/destinations", `{"name":"orders","registryId":"`+regID+`","repository":"payments/orders","pushProfileId":"`+profID+`","allowedProjectIds":["`+projA+`"]}`, nil)
	if st != 201 {
		t.Fatalf("destination = %d %v", st, dest)
	}
	_, _, teams := l.json(t, admin, "GET", "/teams", "", nil)
	team := teams["items"].([]any)[0].(map[string]any)["id"].(string)
	st, _, pull := l.json(t, admin, "POST", "/registry-credential-profiles", `{"name":"lab-pull","registryId":"`+regID+`","type":"anonymous","purpose":"pull"}`, nil)
	if st != 201 {
		t.Fatalf("pull profile = %d %v", st, pull)
	}
	st, _, src := l.json(t, admin, "POST", "/image-sources", `{"name":"lab-mirror","upstream":"registry.example:5000","mirror":"mirror.example:5000","repositoryPatterns":["base/*"],"teamIds":["`+team+`"],"pullProfileId":"`+pull["id"].(string)+`"}`, nil)
	if st != 201 {
		t.Fatalf("image source = %d %v", st, src)
	}

	t.Run("scopes must keep covering destinations", func(t *testing.T) {
		if st, _, b := l.json(t, admin, "PUT", "/registry-credential-profiles/"+profID, `{"name":"lab-publisher","type":"oci-basic","username":"robot","secretRef":"`+pw2+`","repositoryScopes":["billing/*"]}`, nil); st != 422 {
			t.Errorf("scopes excluding a destination = %d %v", st, b)
		}
	})

	t.Run("destination edits", func(t *testing.T) {
		st, _, other := l.json(t, admin, "POST", "/registries", `{"name":"other","kind":"oci","endpoint":"other.example:5000"}`, nil)
		if st != 201 {
			t.Fatalf("second registry = %d %v", st, other)
		}
		otherID := other["id"].(string)
		st, _, op := l.json(t, admin, "POST", "/registry-credential-profiles", `{"name":"other-push","registryId":"`+otherID+`","type":"anonymous","purpose":"push","repositoryScopes":["team/*"]}`, nil)
		if st != 201 {
			t.Fatalf("second push profile = %d %v", st, op)
		}
		opID := op["id"].(string)
		st, _, d := l.json(t, admin, "POST", "/destinations", `{"name":"edit-me","registryId":"`+otherID+`","repository":"team/one","pushProfileId":"`+opID+`"}`, nil)
		if st != 201 {
			t.Fatalf("destination = %d %v", st, d)
		}
		dPath := "/destinations/" + d["id"].(string)
		body := func(name, registry, repo, profile, extra string) string {
			return `{"name":"` + name + `","registryId":"` + registry + `","repository":"` + repo + `","pushProfileId":"` + profile + `"` + extra + `}`
		}
		if st, _, _ := l.json(t, alice, "PUT", dPath, body("edit-me", otherID, "team/one", opID, ""), nil); st != 403 {
			t.Errorf("a developer edited a destination: %d", st)
		}
		for name, b := range map[string]string{
			"another registry's credentials": body("edit-me", otherID, "team/one", profID, ""),
			"pull credentials":               body("edit-me", otherID, "team/one", pull["id"].(string), ""),
			"outside the scopes":             body("edit-me", otherID, "billing/one", opID, ""),
			"another registry":               body("edit-me", regID, "payments/orders", profID, ""),
			"bad tag pattern":                body("edit-me", otherID, "team/one", opID, `,"protectedTags":["["]`),
			"unknown project":                body("edit-me", otherID, "team/one", opID, `,"allowedProjectIds":["01ARZ3NDEKTSV4RRFFQ69G5FAV"]`),
			"duplicate name":                 body("orders", otherID, "team/one", opID, ""),
		} {
			if st, _, res := l.json(t, admin, "PUT", dPath, b, nil); st != 422 && st != 409 {
				t.Errorf("%s = %d %v", name, st, res)
			}
		}
		st, _, res := l.json(t, admin, "PUT", dPath, body("edited", otherID, "team/two", opID, `,"allowedProjectIds":["`+projA+`"],"protectedTags":["latest"],"requiresApproval":true,"signImages":true`), nil)
		if st != 200 || res["name"] != "edited" || res["repository"] != "team/two" || res["registryId"] != otherID || res["requiresApproval"] != true || res["signImages"] != true {
			t.Fatalf("edit destination = %d %v", st, res)
		}
		var repo string
		_ = l.db.SQL.QueryRow(`SELECT repository FROM destinations WHERE id = ?`, d["id"]).Scan(&repo)
		if repo != "team/two" {
			t.Fatalf("stored repository = %q", repo)
		}

		conn, err := l.db.SQL.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`PRAGMA foreign_keys = OFF`,
			`INSERT INTO publications (id, org_id, project_id, kind, artifact_id, artifact_digest, destination_id, tags, state, gate_decisions, requested_by, idempotency_key, created_at, updated_at)
			SELECT 'pub-edit', org_id, '` + projA + `', 'publish', 'artifact-x', 'sha256:00', id, '["v1"]', 'published', '[]', 'someone', 'edit-key', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP FROM destinations WHERE id = '` + d["id"].(string) + `'`,
			`PRAGMA foreign_keys = ON`} {
			if _, err := conn.ExecContext(t.Context(), q); err != nil {
				t.Fatal(err)
			}
		}
		_ = conn.Close()
		st, _, res = l.json(t, admin, "PUT", dPath, body("edited", otherID, "team/three", opID, ""), nil)
		if st != 422 || !strings.Contains(res["message"].(string), "1 publication(s)") {
			t.Errorf("repository change after a publication = %d %v", st, res)
		}
		st, _, res = l.json(t, admin, "PUT", dPath, body("edited-again", otherID, "team/two", opID, `,"immutableTags":true,"promotionOnly":true`), nil)
		if st != 200 || res["immutableTags"] != true || res["promotionOnly"] != true || res["requiresApproval"] != false {
			t.Fatalf("policy edit after a publication = %d %v", st, res)
		}
		var audited int
		_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'destination.update' AND resource_id = ?`, d["id"]).Scan(&audited)
		if audited != 2 {
			t.Errorf("audited destination edits = %d, want 2", audited)
		}
	})

	t.Run("leftover controller-only secrets", func(t *testing.T) {
		secrets := func() map[string]map[string]any {
			_, _, list := l.json(t, admin, "GET", "/secret-providers/"+local+"/local-secrets", "", nil)
			out := map[string]map[string]any{}
			for _, x := range list["items"].([]any) {
				m := x.(map[string]any)
				out[m["id"].(string)] = m
			}
			return out
		}
		got := secrets()
		if got[pw]["controllerOnly"] != true || len(got[pw]["samuoriUses"].([]any)) != 0 {
			t.Errorf("the replaced credential = %v, want a controller-only secret Samuori no longer uses", got[pw])
		}
		if uses := got[pw2]["samuoriUses"].([]any); len(uses) != 1 || uses[0] != "a registry credential" {
			t.Errorf("the current credential = %v", got[pw2])
		}
		for _, id := range []string{pw, pw2} {
			bind := `{"logicalId":"reg_pw","providerId":"` + local + `","providerReference":"` + id + `","usage":["build"],"reason":"try it"}`
			if st, _, b := l.json(t, admin, "POST", "/projects/"+projA+"/secret-bindings", bind, map[string]string{"Idempotency-Key": "bind-" + id}); st != 422 {
				t.Errorf("binding a controller-only secret = %d %v", st, b)
			}
		}
		if st, _, b := l.json(t, admin, "DELETE", "/secret-providers/"+local+"/local-secrets/"+pw2, "", nil); st != 409 || !strings.Contains(b["message"].(string), "a registry credential") {
			t.Errorf("delete a credential in use = %d %v", st, b)
		}
		if st, _, _ := l.json(t, alice, "DELETE", "/secret-providers/"+local+"/local-secrets/"+pw, "", nil); st != 403 {
			t.Errorf("a developer deleted a leftover secret: %d", st)
		}
		if st, _, b := l.json(t, admin, "DELETE", "/secret-providers/"+local+"/local-secrets/"+pw, "", nil); st != 204 {
			t.Fatalf("delete a leftover secret = %d %v", st, b)
		}
		var audited int
		_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'secret.delete' AND resource_id = ? AND reason LIKE '%no longer uses%'`, pw).Scan(&audited)
		if audited != 1 {
			t.Errorf("audited leftover deletes = %d", audited)
		}
	})

	t.Run("deletes are refused while in use", func(t *testing.T) {
		st, _, b := l.json(t, admin, "DELETE", "/registries/"+regID, "", nil)
		if st != 409 {
			t.Fatalf("delete registry in use = %d %v", st, b)
		}
		for _, want := range []string{"credentials lab-publisher", "credentials lab-pull", "destination orders", "image source lab-mirror"} {
			if !strings.Contains(b["message"].(string), want) {
				t.Errorf("registry uses %q miss %q", b["message"], want)
			}
		}
		if st, _, b := l.json(t, admin, "DELETE", "/registry-credential-profiles/"+profID, "", nil); st != 409 || !strings.Contains(b["message"].(string), "destination orders") {
			t.Errorf("delete push profile in use = %d %v", st, b)
		}
		if st, _, b := l.json(t, admin, "DELETE", "/registry-credential-profiles/"+pull["id"].(string), "", nil); st != 409 || !strings.Contains(b["message"].(string), "image source lab-mirror") {
			t.Errorf("delete pull profile in use = %d %v", st, b)
		}
	})

	t.Run("deletes succeed once nothing refers", func(t *testing.T) {
		destPath := "/destinations/" + dest["id"].(string)
		if st, _, _ := l.json(t, alice, "DELETE", destPath, "", nil); st != 403 {
			t.Errorf("a developer deleted a destination: %d", st)
		}
		if st, _, b := l.json(t, admin, "DELETE", destPath, "", nil); st != 204 {
			t.Fatalf("delete destination = %d %v", st, b)
		}
		if st, _, _ := l.json(t, admin, "DELETE", destPath, "", nil); st != 404 {
			t.Errorf("deleting it again = %d, want 404", st)
		}
		if _, err := l.db.SQL.Exec(`DELETE FROM image_sources WHERE id = ?`, src["id"]); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{profID, pull["id"].(string)} {
			if st, _, b := l.json(t, admin, "DELETE", "/registry-credential-profiles/"+id, "", nil); st != 204 {
				t.Fatalf("delete profile = %d %v", st, b)
			}
		}
		if st, _, b := l.json(t, admin, "PUT", "/registries/"+regID, `{"name":"lab-renamed","endpoint":"registry2.example:5000"}`, nil); st != 200 || b["endpoint"] != "registry2.example:5000" {
			t.Fatalf("endpoint change without stored credentials = %d %v", st, b)
		}
		if st, _, b := l.json(t, admin, "DELETE", "/registries/"+regID, "", nil); st != 204 {
			t.Fatalf("delete registry = %d %v", st, b)
		}
		if st, _, _ := l.json(t, admin, "DELETE", "/registries/"+regID, "", nil); st != 404 {
			t.Errorf("second delete = %d", st)
		}
	})

	t.Run("ECR identity edits", func(t *testing.T) {
		_, _, ecr := l.json(t, admin, "POST", "/registries", `{"name":"aws","kind":"ecr","awsAccountId":"123456789012","region":"eu-west-1"}`, nil)
		ecrID := ecr["id"].(string)
		key := secret("aws-key")
		st, _, p := l.json(t, admin, "POST", "/registry-credential-profiles", `{"name":"aws-push","registryId":"`+ecrID+`","type":"ecr","purpose":"push","ecr":{"authMethod":"access-key"},"secretRef":"`+key+`"}`, nil)
		if st != 201 {
			t.Fatalf("ECR profile = %d %v", st, p)
		}
		pid := p["id"].(string)
		if st, _, b := l.json(t, admin, "PUT", "/registry-credential-profiles/"+pid, `{"name":"aws-push","type":"ecr","ecr":{"authMethod":"access-key","apiEndpoint":"https://attacker.example"},"secretRef":"`+key+`"}`, nil); st != 422 {
			t.Errorf("API endpoint change keeping the access key = %d %v", st, b)
		}
		if st, _, b := l.json(t, admin, "PUT", "/registries/"+ecrID, `{"name":"aws","awsAccountId":"210987654321","region":"eu-west-1"}`, nil); st != 422 {
			t.Errorf("account change with a stored access key = %d %v", st, b)
		}
		if st, _, b := l.json(t, admin, "PUT", "/registry-credential-profiles/"+pid, `{"name":"aws-push","type":"ecr","ecr":{"authMethod":"assume-role","roleArn":"arn:aws:iam::123456789012:role/samuori-push"},"secretRef":"`+key+`"}`, nil); st != 422 {
			t.Errorf("assume-role keeping a stored key = %d %v", st, b)
		}
		st, _, b := l.json(t, admin, "PUT", "/registry-credential-profiles/"+pid, `{"name":"aws-push","type":"ecr","ecr":{"authMethod":"workload-identity"}}`, nil)
		if st != 200 || b["secretRef"] != nil || b["ecr"].(map[string]any)["authMethod"] != "workload-identity" {
			t.Fatalf("switch to the controller's role = %d %v", st, b)
		}
		st, _, b = l.json(t, admin, "PUT", "/registries/"+ecrID, `{"name":"aws","awsAccountId":"210987654321","region":"us-east-1"}`, nil)
		if st != 200 || b["endpoint"] != "210987654321.dkr.ecr.us-east-1.amazonaws.com" {
			t.Fatalf("account change without stored secrets = %d %v", st, b)
		}
		if st, _, b := l.json(t, admin, "PUT", "/registry-credential-profiles/"+pid, `{"name":"aws-push","type":"oci-basic","username":"x","secretRef":"`+key+`"}`, nil); st != 422 {
			t.Errorf("non-ECR type on an ECR registry = %d %v", st, b)
		}
	})

	var n int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('registry.update', 'registry.delete', 'registry-profile.update', 'registry-profile.delete')`).Scan(&n)
	if n != 8 {
		t.Errorf("audited registry edits and deletes = %d, want 8", n)
	}
}
