package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
)

func TestBaseImageRequests(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	var org, teamA string
	if err := l.db.SQL.QueryRow(`SELECT org_id, team_id FROM projects WHERE id = ?`, projA).Scan(&org, &teamA); err != nil {
		t.Fatal(err)
	}
	userWith := func(name string, bindings ...string) member {
		var u apigen.User
		_ = json.NewDecoder(l.call(t, admin.c, "POST", "/users", fmt.Sprintf(`{"username":%q,"password":"a long secret phrase"}`, name), admin.csrf).Body).Decode(&u)
		for _, b := range bindings {
			if r := l.call(t, admin.c, "POST", "/role-bindings", fmt.Sprintf(`{"principalId":%q,%s}`, u.Id, b), admin.csrf); r.StatusCode != 201 {
				t.Fatalf("bind %s = %d", b, r.StatusCode)
			}
		}
		c := l.client()
		for _, ck := range l.call(t, c, "POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"a long secret phrase"}`, name), nil).Cookies() {
			if ck.Name == auth.CSRFCookie {
				return member{c, map[string]string{auth.CSRFHeader: ck.Value}}
			}
		}
		t.Fatalf("login %s failed", name)
		return member{}
	}
	carol := userWith("carol", `"role":"administrator","scopeType":"organization","scopeId":"`+org+`"`, `"role":"developer","scopeType":"team","scopeId":"`+teamA+`"`)
	pat := userWith("pat", `"role":"administrator","scopeType":"project","scopeId":"`+projA+`"`)
	allowed := func() []string {
		_, _, list := l.json(t, admin, "GET", "/policies", "", nil)
		for _, p := range list["items"].([]any) {
			if m := p.(map[string]any); m["kind"] == "base-images" {
				var out []string
				for _, r := range m["document"].(map[string]any)["allowedRepositories"].([]any) {
					out = append(out, r.(string))
				}
				return out
			}
		}
		t.Fatal("no base-images policy")
		return nil
	}
	decide := func(m member, approval, decision, key string) (int, body) {
		st, _, b := l.json(t, m, "POST", "/approvals/"+approval+"/decision", `{"decision":"`+decision+`","reason":"reviewed the vendor image and its licence"}`, map[string]string{"Idempotency-Key": key})
		return st, b
	}
	const why = `"justification":"the database integration tests need the vendor server image"`

	for name, repo := range map[string]string{"tag": "mcr.microsoft.com/mssql/server:2022", "digest": "mcr.microsoft.com/mssql/server@sha256:" + fmt.Sprintf("%064d", 0), "upper case": "MCR.microsoft.com/mssql/server", "port": "localhost:5000/x", "empty": ""} {
		if st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/base-image-requests", `{"repository":"`+repo+`",`+why+`}`, nil); st != 422 && st != 400 {
			t.Errorf("%s = %d %v", name, st, b)
		}
	}
	if st, _, _ := l.json(t, alice, "POST", "/projects/"+projA+"/base-image-requests", `{"repository":"mcr.microsoft.com/mssql/server","justification":"too short"}`, nil); st != 422 && st != 400 {
		t.Errorf("short justification = %d", st)
	}
	if st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/base-image-requests", `{"repository":"`+allowed()[0]+`",`+why+`}`, nil); st != 409 || b["code"] != "already_allowed" {
		t.Errorf("already allowed = %d %v", st, b)
	}
	if st, _, _ := l.json(t, bob, "POST", "/projects/"+projA+"/base-image-requests", `{"repository":"mcr.microsoft.com/mssql/server",`+why+`}`, nil); st != 404 {
		t.Fatalf("another team requested for project A: %d", st)
	}
	if st, _, _ := l.json(t, bob, "GET", "/projects/"+projA+"/base-image-requests", "", nil); st != 404 {
		t.Fatalf("another team listed project A's requests: %d", st)
	}

	snap := l.snapshotOf(t, alice, projA, map[string]string{"Dockerfile": "FROM mcr.microsoft.com/mssql/server@sha256:" + fmt.Sprintf("%064d", 1) + " AS final\n"})
	_, _, spec := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(dockerSpec("")), nil)
	blocked := func() []any {
		_, _, rep := l.json(t, alice, "POST", "/specs/"+spec["specId"].(string)+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		var out []any
		for _, d := range rep["policyDecisions"].([]any) {
			if m := d.(map[string]any); m["outcome"] == "block" {
				out = append(out, m)
			}
		}
		return out
	}
	if b := blocked(); len(b) != 1 || fmt.Sprint(b[0].(map[string]any)["requestableRepositories"]) != "[mcr.microsoft.com/mssql/server]" {
		t.Fatalf("admission does not offer the unlisted repository for request: %v", b)
	}

	st, _, req := l.json(t, alice, "POST", "/projects/"+projA+"/base-image-requests", `{"repository":"mcr.microsoft.com/mssql/server",`+why+`}`, nil)
	if st != 202 || req["state"] != "pending" || req["repository"] != "mcr.microsoft.com/mssql/server" {
		t.Fatalf("request = %d %v", st, req)
	}
	approval := req["approvalId"].(string)
	if st, _, b := l.json(t, carol, "POST", "/projects/"+projA+"/base-image-requests", `{"repository":"mcr.microsoft.com/mssql/server",`+why+`}`, nil); st != 409 || b["code"] != "request_pending" {
		t.Fatalf("duplicate = %d %v", st, b)
	}
	if st, b := decide(alice, approval, "approve", "bir-decide-000000001"); st != 403 {
		t.Fatalf("the requester approved: %d %v", st, b)
	}
	if st, b := decide(pat, approval, "approve", "bir-decide-000000002"); st != 403 && st != 404 {
		t.Fatalf("a project administrator approved an organization policy change: %d %v", st, b)
	}
	if slices.Contains(allowed(), "mcr.microsoft.com/mssql/server") {
		t.Fatal("the repository was allowed before approval")
	}
	var policyID string
	_ = l.db.SQL.QueryRow(`SELECT id FROM policies WHERE org_id = ? AND kind = 'base-images'`, org).Scan(&policyID)
	edited, _ := json.Marshal(map[string]any{"kind": "base-images", "document": map[string]any{"allowedRepositories": append(allowed(), "quay.io/example/tool"), "requireDigest": true}})
	if st, _, b := l.json(t, admin, "PUT", "/policies/"+policyID, string(edited), map[string]string{"If-Match": `"1"`}); st != 200 {
		t.Fatalf("administrator's own policy edit = %d %v", st, b)
	}
	st, ap := decide(admin, approval, "approve", "bir-decide-000000003")
	if st != 200 || ap["state"] != "approved" {
		t.Fatalf("approve = %d %v", st, ap)
	}
	if now := allowed(); !slices.Contains(now, "mcr.microsoft.com/mssql/server") || !slices.Contains(now, "quay.io/example/tool") {
		t.Fatalf("approval did not extend the current policy version: %v", now)
	}
	if b := blocked(); len(b) != 0 {
		t.Fatalf("admission still blocks the approved repository: %v", b)
	}
	stale, _ := json.Marshal(map[string]any{"kind": "base-images", "document": map[string]any{"allowedRepositories": allowed(), "requireDigest": true}})
	if st, _, _ := l.json(t, admin, "PUT", "/policies/"+policyID, string(stale), map[string]string{"If-Match": `"2"`}); st != 409 && st != 412 {
		t.Fatalf("an edit based on the version before approval = %d", st)
	}
	_, _, list := l.json(t, alice, "GET", "/projects/"+projA+"/base-image-requests", "", nil)
	got := list["items"].([]any)[0].(map[string]any)
	if got["state"] != "approved" || got["policyVersion"] == nil {
		t.Fatalf("request after approval = %v", got)
	}
	var audited int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'policy.update' AND reason LIKE ?`, "%base-image request "+req["id"].(string)+"%").Scan(&audited)
	if audited != 1 {
		t.Errorf("policy change audit records naming the request = %d", audited)
	}

	st, _, own := l.json(t, carol, "POST", "/projects/"+projA+"/base-image-requests", `{"repository":"node",`+why+`}`, nil)
	if st != 409 {
		t.Fatalf("short Docker Hub name that the defaults allow = %d %v", st, own)
	}
	st, _, own = l.json(t, carol, "POST", "/projects/"+projA+"/base-image-requests", `{"repository":"gcr.io/distroless/static",`+why+`}`, nil)
	if st != 202 {
		t.Fatalf("carol's request = %d %v", st, own)
	}
	if st, b := decide(carol, own["approvalId"].(string), "approve", "bir-decide-000000004"); st != 403 || b["code"] != "own_request" {
		t.Fatalf("an administrator approved their own request: %d %v", st, b)
	}
	if st, _ := decide(admin, own["approvalId"].(string), "reject", "bir-decide-000000005"); st != 200 {
		t.Fatalf("reject = %d", st)
	}
	if slices.Contains(allowed(), "gcr.io/distroless/static") {
		t.Fatal("a rejected request changed the policy")
	}
	if st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/base-image-requests", `{"repository":"gcr.io/distroless/static",`+why+`}`, nil); st != 202 {
		t.Fatalf("a new request after rejection = %d %v", st, b)
	}
}
