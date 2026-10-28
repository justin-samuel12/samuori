package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
)

// TestCrossTeamAccess creates resources in team B's project and checks that a developer of team A can neither
// read nor change any of them, while their owner can (SEC-RBAC-01, 03 to 07).
func TestCrossTeamAccess(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, _, projB := twoTeams(t, l)
	admin := l.adminMember(t)

	snapB := l.snapshotOf(t, bob, projB, map[string]string{"package.json": `{"name":"b"}`})
	st, _, up := l.json(t, bob, "POST", "/projects/"+projB+"/uploads", `{"fileCount":1,"totalBytes":10}`, nil)
	if st != 201 {
		t.Fatalf("upload session = %d %v", st, up)
	}
	uploadB := up["id"].(string)
	_, _, rev := l.json(t, bob, "POST", "/projects/"+projB+"/specs", doc(nodeSpec), nil)
	specB := rev["specId"].(string)
	st, _, run := l.json(t, bob, "POST", "/builds", fmt.Sprintf(`{"specId":%q,"specRevision":1,"snapshotId":%q}`, specB, snapB), map[string]string{"Idempotency-Key": "rbac-build-000000001"})
	if st != 201 {
		t.Fatalf("build = %d %v", st, run)
	}
	buildB := run["id"].(string)
	var org string
	_ = l.db.SQL.QueryRow(`SELECT org_id FROM projects WHERE id = ?`, projB).Scan(&org)
	artifactB := "01JRBACART0000000000000001"
	_, _ = l.db.SQL.Exec(`PRAGMA foreign_keys = OFF`)
	defer l.db.SQL.Exec(`PRAGMA foreign_keys = ON`)
	if _, err := l.db.SQL.Exec(`INSERT INTO build_artifacts (id, org_id, run_id, attempt_id, generation, kind, digest, size, created_at) VALUES (?, ?, ?, 'a-b', 1, 'image', ?, 10, ?)`,
		artifactB, org, buildB, "sha256:"+strings.Repeat("e", 64), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	l.json(t, bob, "POST", "/projects/"+projB+"/repository-requests", `{"repository":"team-b/app","justification":"team B needs a repository"}`, nil)

	idem := func(k string) map[string]string {
		return map[string]string{"Idempotency-Key": "rbac-" + k + "-0000000001"}
	}
	cases := []struct {
		id, method, path, body string
		headers                map[string]string
		ownerOK                bool
	}{
		{"SEC-RBAC-01", "GET", "/projects/" + projB + "/snapshots", "", nil, true},
		{"SEC-RBAC-01", "GET", "/snapshots/" + snapB, "", nil, true},
		{"SEC-RBAC-01", "GET", "/snapshots/" + snapB + "/discoveries", "", nil, true},
		{"SEC-RBAC-01", "POST", "/snapshots/" + snapB + "/discoveries", "", idem("disc"), false},
		{"SEC-RBAC-01", "GET", "/uploads/" + uploadB, "", nil, true},
		{"SEC-RBAC-01", "POST", "/uploads/" + uploadB + "/commit", "", idem("commit"), false},
		{"SEC-RBAC-01", "POST", "/projects/" + projB + "/uploads", `{"fileCount":1,"totalBytes":10}`, nil, false},
		{"SEC-RBAC-03", "GET", "/builds/" + buildB, "", nil, true},
		{"SEC-RBAC-03", "GET", "/builds/" + buildB + "/attempts", "", nil, true},
		{"SEC-RBAC-03", "GET", "/builds/" + buildB + "/logs", "", nil, false},
		{"SEC-RBAC-03", "POST", "/builds/" + buildB + "/cancel", "", idem("cancel"), false},
		{"SEC-RBAC-03", "POST", "/builds", fmt.Sprintf(`{"specId":%q,"specRevision":1,"snapshotId":%q}`, specB, snapB), idem("build"), false},
		{"SEC-RBAC-04", "GET", "/artifacts/" + artifactB, "", nil, true},
		{"SEC-RBAC-04", "GET", "/artifacts/" + artifactB + "/validations", "", nil, true},
		{"SEC-RBAC-04", "POST", "/artifacts/" + artifactB + "/validations", "", idem("reval"), false},
		{"SEC-RBAC-04", "GET", "/projects/" + projB + "/images", "", nil, true},
		{"SEC-RBAC-04", "GET", "/publications?projectId=" + projB, "", nil, true},
		{"SEC-RBAC-05", "GET", "/projects/" + projB + "/secret-bindings", "", nil, false},
		{"SEC-RBAC-05", "POST", "/projects/" + projB + "/secret-requests", `{"logicalId":"npm-token","justification":"needed for private packages"}`, idem("secreq"), false},
		{"SEC-RBAC-06", "GET", "/build-caches", "", nil, false},
		{"SEC-RBAC-07", "GET", "/projects/" + projB + "/destinations", "", nil, true},
		{"SEC-RBAC-07", "GET", "/projects/" + projB + "/repository-requests", "", nil, true},
		{"SEC-RBAC-07", "POST", "/projects/" + projB + "/repository-requests", `{"repository":"team-a/steal","justification":"asking in another team's name"}`, nil, false},
		{"SEC-RBAC-07", "GET", "/destinations", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.id+" "+c.method+" "+c.path, func(t *testing.T) {
			st, _, b := l.json(t, alice, c.method, c.path, c.body, c.headers)
			// A filtered list may answer with nothing instead of refusing; it must not show anything.
			if items, ok := b["items"].([]any); st == 200 && c.method == "GET" && ok && len(items) == 0 && strings.Contains(c.path, "?") {
				return
			}
			if st != 403 && st != 404 && st != 400 {
				raw, _ := json.Marshal(b)
				t.Fatalf("team A got %d: %s", st, raw)
			}
			if c.ownerOK {
				if st, _, _ := l.json(t, bob, c.method, c.path, c.body, c.headers); st != 200 {
					t.Fatalf("the owner got %d", st)
				}
			}
		})
	}
	t.Run("lists never include another team's resources", func(t *testing.T) {
		for _, path := range []string{"/builds", "/approvals", "/publications"} {
			_, _, b := l.json(t, alice, "GET", path, "", nil)
			raw, _ := json.Marshal(b)
			for _, id := range []string{buildB, snapB, specB, artifactB, projB} {
				if strings.Contains(string(raw), id) {
					t.Errorf("%s shows %s to team A", path, id)
				}
			}
		}
		if _, _, b := l.json(t, admin, "GET", "/builds", "", nil); !strings.Contains(fmt.Sprint(b), buildB) {
			t.Error("the administrator does not see team B's build")
		}
	})
}

// TestScopedAdministratorCannotPreviewPolicies checks that an administrator bound to one project can neither dry-run
// nor replace an organization policy: the dry run reports specs across every team.
func TestScopedAdministratorCannotPreviewPolicies(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	_, _, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	var u apigen.User
	_ = json.NewDecoder(l.call(t, admin.c, "POST", "/users", `{"username":"pat","password":"a long secret phrase"}`, admin.csrf).Body).Decode(&u)
	if r := l.call(t, admin.c, "POST", "/role-bindings", fmt.Sprintf(`{"principalId":%q,"role":"administrator","scopeType":"project","scopeId":%q}`, u.Id, projA), admin.csrf); r.StatusCode != 201 {
		t.Fatalf("bind = %d", r.StatusCode)
	}
	c := l.client()
	var pat member
	for _, ck := range l.call(t, c, "POST", "/auth/login", `{"username":"pat","password":"a long secret phrase"}`, nil).Cookies() {
		if ck.Name == auth.CSRFCookie {
			pat = member{c, map[string]string{auth.CSRFHeader: ck.Value}}
		}
	}
	_, _, list := l.json(t, admin, "GET", "/policies", "", nil)
	var bid string
	for _, p := range list["items"].([]any) {
		if m := p.(map[string]any); m["kind"] == "base-images" {
			bid = m["id"].(string)
		}
	}
	doc := `{"kind":"base-images","document":{"allowedRepositories":["docker.io/library/python"],"requireDigest":true}}`
	if st, _, b := l.json(t, pat, "POST", "/policies/"+bid+"/dry-run", doc, nil); st != 403 {
		t.Fatalf("dry run = %d %v", st, b)
	}
	if st, _, _ := l.json(t, pat, "PUT", "/policies/"+bid, doc, map[string]string{"If-Match": `"1"`}); st != 403 {
		t.Fatalf("replace = %d", st)
	}
	if st, _, _ := l.json(t, admin, "POST", "/policies/"+bid+"/dry-run", doc, nil); st != 200 {
		t.Fatalf("the organization administrator's dry run = %d", st)
	}
}
