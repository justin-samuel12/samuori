package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/auth"
)

const nodeSpec = `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"orders-api"},
"spec":{"application":{"type":"http-service","runtime":{"language":"nodejs","versionConstraint":">=22"},"port":3000},
"build":{"method":"recipe","recipe":{"id":"node-service","version":"1.0.0","install":"npm ci","start":["node","server.js"]}},
"target":{"platforms":["linux/amd64","linux/arm64"]}}}`

func dockerSpec(secrets string) string {
	return `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"from-dockerfile"},
"spec":{"build":{"method":"dockerfile","dockerfile":{"context":".","path":"Dockerfile","target":"final"}}` + secrets + `}}`
}

type body map[string]any

func (l *live) json(t *testing.T, m member, method, path, payload string, extra map[string]string) (int, http.Header, body) {
	t.Helper()
	h := map[string]string{auth.CSRFHeader: m.csrf[auth.CSRFHeader]}
	for k, v := range extra {
		h[k] = v
	}
	if payload != "" && !strings.HasPrefix(payload, "{") {
		payload = `{"document":` + payload + `}`
	}
	r := l.call(t, m.c, method, path, payload, h)
	raw, _ := io.ReadAll(r.Body)
	var b body
	_ = json.Unmarshal(raw, &b)
	if r.StatusCode < 300 && method == "GET" || r.StatusCode == 200 || r.StatusCode == 201 {
		assertMatchesContract(t, method, path, r.StatusCode, r.Header, raw)
	}
	return r.StatusCode, r.Header, b
}

func doc(spec string) string { return `{"document":` + spec + `}` }

func codes(b body, key string) string {
	var out []string
	list, _ := b[key].([]any)
	for _, x := range list {
		m := x.(map[string]any)
		c, _ := m["code"].(string)
		if c == "" {
			c, _ = m["outcome"].(string)
		}
		out = append(out, c)
	}
	return strings.Join(out, ",")
}

func (l *live) adminMember(t *testing.T) member {
	t.Helper()
	c, csrf := l.login(t)
	return member{c, map[string]string{auth.CSRFHeader: csrf}}
}

func (l *live) snapshotOf(t *testing.T, m member, project string, files map[string]string) string {
	t.Helper()
	s := l.upload(t, m, project, tarOf(t, files), 1024)
	_, op := l.commit(t, m, s.Id, fmt.Sprint(len(files), project))
	return *l.await(t, m, op.Id).ResourceId
}

func TestSpecsRevisionsAndIsolation(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, _ := twoTeams(t, l)

	st, h, rev := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(nodeSpec), nil)
	if st != 201 || h.Get("ETag") != `"1"` || rev["revision"].(float64) != 1 {
		t.Fatalf("create = %d %v %v", st, h.Get("ETag"), rev)
	}
	specID := rev["specId"].(string)
	if st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(nodeSpec), nil); st != 409 || b["code"] != "spec_exists" {
		t.Fatalf("duplicate name = %d %v", st, b)
	}
	t.Run("wizard finds the spec it collided with", func(t *testing.T) {
		_, _, found := l.json(t, alice, "GET", "/projects/"+projA+"/specs?name=orders-api", "", nil)
		items, _ := found["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["id"] != specID {
			t.Fatalf("lookup by name = %v", found)
		}
		_, _, none := l.json(t, alice, "GET", "/projects/"+projA+"/specs?name=other", "", nil)
		if items, _ := none["items"].([]any); len(items) != 0 {
			t.Fatalf("lookup of an unknown name = %v", none)
		}
		if st, _, _ := l.json(t, bob, "GET", "/projects/"+projA+"/specs?name=orders-api", "", nil); st != 404 {
			t.Fatalf("bob looks up a spec in team A = %d", st)
		}
	})

	t.Run("SEC-RBAC-02", func(t *testing.T) {
		for _, path := range []string{"/specs/" + specID, "/specs/" + specID + "/revisions", "/specs/" + specID + "/revisions/1", "/projects/" + projA + "/specs"} {
			if st, _, _ := l.json(t, bob, "GET", path, "", nil); st != 404 {
				t.Errorf("bob GET %s = %d", path, st)
			}
		}
		if st, _, _ := l.json(t, bob, "POST", "/specs/"+specID+"/revisions", doc(nodeSpec), map[string]string{"If-Match": `"1"`}); st != 404 {
			t.Errorf("bob adds revision = %d", st)
		}
		if st, _, _ := l.json(t, bob, "POST", "/projects/"+projA+"/specs", doc(strings.Replace(nodeSpec, "orders-api", "x", 1)), nil); st != 404 {
			t.Errorf("bob creates spec in team A = %d", st)
		}
	})

	invalid := strings.Replace(nodeSpec, `"method":"recipe"`, `"method":"magic"`, 1)
	if st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(strings.Replace(invalid, "orders-api", "bad", 1)), nil); st != 422 || len(b["details"].([]any)) == 0 {
		t.Fatalf("invalid spec = %d %v", st, b)
	}

	t.Run("SEC-SPC-02", func(t *testing.T) {
		leaky := strings.Replace(nodeSpec, `"install":"npm ci"`, `"install":"npm ci --registry https://ci:hunter2secret@registry.example.invalid/"`, 1)
		st, _, b := l.json(t, alice, "POST", "/specs/"+specID+"/revisions", doc(leaky), map[string]string{"If-Match": `"1"`})
		if st != 422 || !strings.Contains(fmt.Sprint(b["details"]), "secret-literal") || strings.Contains(fmt.Sprint(b), "hunter2secret") {
			t.Fatalf("secret literal = %d %v", st, b)
		}
	})

	if st, _, _ := l.json(t, alice, "POST", "/specs/"+specID+"/revisions", doc(nodeSpec), map[string]string{"If-Match": `"7"`}); st != 412 {
		t.Fatalf("stale If-Match = %d", st)
	}
	changed := strings.Replace(nodeSpec, `"port":3000`, `"port":8080`, 1)
	st, h, rev2 := l.json(t, alice, "POST", "/specs/"+specID+"/revisions", doc(changed), map[string]string{"If-Match": `"1"`})
	if st != 201 || h.Get("ETag") != `"2"` || rev2["digest"] == rev["digest"] {
		t.Fatalf("revision 2 = %d %v", st, rev2)
	}
	if st, _, _ := l.json(t, alice, "POST", "/specs/"+specID+"/revisions", doc(changed), map[string]string{"If-Match": `"1"`}); st != 412 {
		t.Fatalf("lost update not prevented = %d", st)
	}
	_, _, list := l.json(t, alice, "GET", "/specs/"+specID+"/revisions", "", nil)
	if len(list["items"].([]any)) != 2 {
		t.Fatalf("revisions = %v", list)
	}
	if _, err := l.db.SQL.Exec(`UPDATE spec_revisions SET digest = 'x' WHERE spec_id = ?`, specID); err == nil {
		t.Fatal("spec revisions are mutable")
	}
}

func TestAdmissionDryRun(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	snap := l.snapshotOf(t, alice, projA, map[string]string{
		"package.json": `{"name":"api","scripts":{"start":"node server.js"},"dependencies":{"express":"5"}}`,
		"server.js":    "x",
		"Dockerfile":   "# syntax=docker/dockerfile:1.12@sha256:" + strings.Repeat("a", 64) + "\nFROM node:22-slim AS final\nRUN --mount=type=secret,id=npm-token npm ci\n",
	})

	_, _, rec := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(nodeSpec), nil)
	recipeSpec := rec["specId"].(string)
	st, _, rep := l.json(t, alice, "POST", "/specs/"+recipeSpec+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
	if st != 200 || rep["admissible"] != false || codes(rep, "policyDecisions") != "allow,allow" || codes(rep, "semanticErrors") != "" || codes(rep, "platformIssues") != "no-verified-pool" {
		t.Fatalf("recipe admission = %d %v", st, rep)
	}
	if st, _, _ := l.json(t, bob, "POST", "/specs/"+recipeSpec+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil); st != 404 {
		t.Fatalf("bob admission = %d", st)
	}

	_, _, dk := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(dockerSpec("")), nil)
	dockerID := dk["specId"].(string)
	t.Run("SEC-SPC-03", func(t *testing.T) {
		_, _, rep := l.json(t, alice, "POST", "/specs/"+dockerID+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		if rep["admissible"] != false || !strings.Contains(codes(rep, "policyDecisions"), "block") {
			t.Fatalf("unapproved frontend admitted: %v", rep)
		}
	})
	_, _, rep = l.json(t, alice, "POST", "/specs/"+dockerID+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
	if !strings.Contains(codes(rep, "semanticErrors"), "secret-undeclared") || !strings.Contains(fmt.Sprint(rep["policyDecisions"]), "Pinned at admission: node:22-slim → docker.io/library/node@sha256:") {
		t.Fatalf("dockerfile report = %v", rep)
	}

	_, _, pols := l.json(t, admin, "GET", "/policies", "", nil)
	var frontends, baseImages map[string]any
	for _, p := range pols["items"].([]any) {
		m := p.(map[string]any)
		switch m["kind"] {
		case "frontends":
			frontends = m
		case "base-images":
			baseImages = m
		}
	}
	fid := frontends["id"].(string)
	approve := `{"kind":"frontends","document":{"approved":["docker/dockerfile:1.12@sha256:` + strings.Repeat("a", 64) + `"]}}`
	if st, _, _ := l.json(t, admin, "PUT", "/policies/"+fid, `{"kind":"frontends","document":{"approved":["docker/dockerfile:1"]}}`, map[string]string{"If-Match": `"1"`}); st != 422 {
		t.Fatalf("unpinned frontend accepted = %d", st)
	}
	if st, _, _ := l.json(t, alice, "PUT", "/policies/"+fid, approve, map[string]string{"If-Match": `"1"`}); st != 403 {
		t.Fatalf("developer edits policy = %d", st)
	}
	if st, h, _ := l.json(t, admin, "PUT", "/policies/"+fid, approve, map[string]string{"If-Match": `"1"`}); st != 200 || h.Get("ETag") != `"2"` {
		t.Fatalf("approve frontend = %d", st)
	}
	if st, _, _ := l.json(t, admin, "PUT", "/policies/"+fid, approve, map[string]string{"If-Match": `"1"`}); st != 412 {
		t.Fatalf("stale policy write = %d", st)
	}
	_, _, rep = l.json(t, alice, "POST", "/specs/"+dockerID+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
	if strings.Contains(fmt.Sprint(rep["policyDecisions"]), "syntax") {
		t.Fatalf("approved frontend still refused: %v", rep)
	}

	if st, _, _ := l.json(t, alice, "POST", "/specs/"+dockerID+"/revisions", doc(dockerSpec(`,"secrets":[{"id":"npm-token"}]`)), map[string]string{"If-Match": `"1"`}); st != 201 {
		t.Fatalf("revision with secret = %d", st)
	}
	_, _, rep = l.json(t, alice, "POST", "/specs/"+dockerID+"/revisions/2/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
	sr := rep["secretReadiness"].([]any)
	if rep["secretBearing"] != true || len(sr) != 1 || sr[0].(map[string]any)["status"] != "missing" || rep["admissible"] != false || strings.Contains(codes(rep, "semanticErrors"), "secret-undeclared") {
		t.Fatalf("secret readiness = %v", rep)
	}

	bid := baseImages["id"].(string)
	restrict := `{"kind":"base-images","document":{"allowedRepositories":["docker.io/library/python"],"requireDigest":true}}`
	_, _, impact := l.json(t, admin, "POST", "/policies/"+bid+"/dry-run", restrict, nil)
	if !strings.Contains(fmt.Sprint(impact["newlyBlocked"]), recipeSpec) || impact["affectedProjects"].(float64) != 1 {
		t.Fatalf("impact = %v", impact)
	}
	_, _, again := l.json(t, admin, "GET", "/policies", "", nil)
	for _, p := range again["items"].([]any) {
		if m := p.(map[string]any); m["kind"] == "base-images" && m["version"].(float64) != 1 {
			t.Fatal("dry run changed the policy")
		}
	}

	t.Run("egress preset not allowed", func(t *testing.T) {
		var eid string
		for _, p := range again["items"].([]any) {
			if m := p.(map[string]any); m["kind"] == "egress" {
				eid = m["id"].(string)
			}
		}
		noNpm := `{"kind":"egress","document":{"allowedPresets":["pypi"],"dockerfilePresets":[],"destinations":[]}}`
		_, _, impact := l.json(t, admin, "POST", "/policies/"+eid+"/dry-run", noNpm, nil)
		token, _ := impact["confirmation"].(string)
		if !strings.Contains(fmt.Sprint(impact["newlyBlocked"]), recipeSpec) || token == "" {
			t.Fatalf("egress impact = %v", impact)
		}
		t.Run("INT-ADM-01 a change that blocks specs needs its dry run and a reason", func(t *testing.T) {
			if st, _, b := l.json(t, admin, "PUT", "/policies/"+eid, noNpm, map[string]string{"If-Match": `"1"`}); st != 428 || b["code"] != "confirmation_required" {
				t.Fatalf("unconfirmed = %d %v", st, b)
			}
			other := `{"kind":"egress","document":{"allowedPresets":[],"dockerfilePresets":[],"destinations":[]}}`
			if st, _, _ := l.json(t, admin, "PUT", "/policies/"+eid, other, map[string]string{"If-Match": `"1"`, "X-Samuori-Confirm": token}); st != 428 {
				t.Fatalf("a confirmation for another document = %d", st)
			}
			if st, _, _ := l.json(t, admin, "PUT", "/policies/"+eid, noNpm, map[string]string{"If-Match": `"1"`, "X-Samuori-Confirm": token + "x"}); st != 428 {
				t.Fatalf("a forged confirmation = %d", st)
			}
			if st, _, b := l.json(t, admin, "PUT", "/policies/"+eid, noNpm, map[string]string{"If-Match": `"1"`, "X-Samuori-Confirm": token}); st != 422 || b["code"] != "reason_required" {
				t.Fatalf("no reason = %d %v", st, b)
			}
		})
		withReason := strings.Replace(noNpm, `"kind":"egress",`, `"kind":"egress","reason":"npm moves behind the internal mirror",`, 1)
		if st, _, _ := l.json(t, admin, "PUT", "/policies/"+eid, withReason, map[string]string{"If-Match": `"1"`, "X-Samuori-Confirm": token}); st != 200 {
			t.Fatalf("egress policy update = %d", st)
		}
		_, _, events := l.json(t, admin, "GET", "/audit-events?action=policy.update", "", nil)
		if !strings.Contains(fmt.Sprint(events), "npm moves behind the internal mirror") {
			t.Fatalf("the reason was not audited: %v", events)
		}
		_, _, rep := l.json(t, alice, "POST", "/specs/"+recipeSpec+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		if rep["admissible"] != false || !strings.Contains(fmt.Sprint(rep["policyDecisions"]), "network access to npm") {
			t.Fatalf("recipe admitted without its egress preset: %v", rep)
		}
	})

	t.Run("recipe withdrawal", func(t *testing.T) {
		if st, _, _ := l.json(t, alice, "PUT", "/recipes/node-service/versions/1.0.0/status", `{"status":"withdrawn","reason":"x"}`, nil); st != 403 {
			t.Fatalf("developer withdraws recipe = %d", st)
		}
		if r := l.call(t, admin.c, "PUT", "/recipes/node-service/versions/1.0.0/status", `{"status":"withdrawn","reason":"Superseded by 2.0"}`, admin.csrf); r.StatusCode != 204 {
			t.Fatalf("withdraw = %d", r.StatusCode)
		}
		_, _, rep := l.json(t, alice, "POST", "/specs/"+recipeSpec+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		if !strings.Contains(codes(rep, "semanticErrors"), "recipe-withdrawn") || rep["admissible"] != false {
			t.Fatalf("withdrawn recipe admitted: %v", rep)
		}
		_, _, list := l.json(t, alice, "GET", "/recipes", "", nil)
		if strings.Contains(fmt.Sprint(list), "node-service") {
			t.Fatal("developers still see a withdrawn recipe")
		}
		_, _, adminList := l.json(t, admin, "GET", "/recipes", "", nil)
		if !strings.Contains(fmt.Sprint(adminList), "withdrawn") {
			t.Fatal("administrators cannot see withdrawn recipes")
		}
	})
}

func TestEditorRenderingAndDrafts(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, _ := twoTeams(t, l)

	withExt := strings.Replace(nodeSpec, `"target":`, `"x-team-notes":{"owner":"payments"},"target":`, 1)
	_, _, r1 := l.json(t, alice, "POST", "/spec-documents/validate", `{"document":`+withExt+`}`, nil)
	yml, _ := r1["canonicalYaml"].(string)
	if r1["valid"] != true || !strings.HasPrefix(yml, "apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\n") || !strings.Contains(yml, "x-team-notes:") {
		t.Fatalf("render = %v", r1)
	}
	ymlJSON, _ := json.Marshal(yml)
	_, _, r2 := l.json(t, alice, "POST", "/spec-documents/validate", `{"yaml":`+string(ymlJSON)+`}`, nil)
	if r2["digest"] != r1["digest"] || r2["canonicalYaml"] != yml {
		t.Fatalf("YAML round trip changed the document: %v vs %v", r2["digest"], r1["digest"])
	}
	_, _, partial := l.json(t, alice, "POST", "/spec-documents/validate", `{"yaml":"apiVersion: samuori.invalid/v1alpha1\nkind: ImageBuild\nspec: {}\n"}`, nil)
	if partial["valid"] != false || partial["canonicalYaml"] == "" || len(partial["schemaErrors"].([]any)) == 0 {
		t.Fatalf("partial = %v", partial)
	}
	if st, _, _ := l.json(t, alice, "POST", "/spec-documents/validate", `{"yaml":"a: 1","document":{}}`, nil); st != 400 {
		t.Fatalf("both inputs = %d", st)
	}

	path := "/projects/" + projA + "/wizard-draft"
	if st, _, _ := l.json(t, alice, "GET", path, "", nil); st != 404 {
		t.Fatalf("no draft = %d", st)
	}
	st, _, d1 := l.json(t, alice, "PUT", path, `{"version":0,"data":{"step":3,"mode":"guided"}}`, nil)
	if st != 200 || d1["version"].(float64) != 1 {
		t.Fatalf("save = %d %v", st, d1)
	}
	if st, _, _ := l.json(t, alice, "PUT", path, `{"version":0,"data":{"step":1}}`, nil); st != 409 {
		t.Fatalf("conflicting create = %d", st)
	}
	if st, _, _ := l.json(t, alice, "PUT", path, `{"version":1,"data":{"note":"token ghp_`+strings.Repeat("b", 36)+`"}}`, nil); st != 422 {
		t.Fatalf("secret in draft = %d", st)
	}
	if st, _, _ := l.json(t, bob, "GET", path, "", nil); st != 404 {
		t.Fatalf("bob reads alice's draft = %d", st)
	}
	_, _, got := l.json(t, alice, "GET", path, "", nil)
	if got["data"].(map[string]any)["step"].(float64) != 3 {
		t.Fatalf("draft = %v", got)
	}
	if r := l.call(t, alice.c, "DELETE", path, "", alice.csrf); r.StatusCode != 204 {
		t.Fatalf("delete = %d", r.StatusCode)
	}
}
