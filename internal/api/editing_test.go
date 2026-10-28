package api

import (
	"strings"
	"testing"

	"samuori.invalid/samuori/internal/ids"
)

func auditCount(t *testing.T, l *live, action, resourceID string) int {
	t.Helper()
	var n int
	if err := l.db.SQL.QueryRow(l.db.Rebind(`SELECT COUNT(*) FROM audit_events WHERE action = ? AND resource_id = ?`), action, resourceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestProjectRename: maintainers and administrators of the project's team rename it with the creation name rule and
// per-team uniqueness; developers are refused, other teams see nothing, and the project never changes team.
func TestProjectRename(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, projB := twoTeams(t, l)
	admin := l.adminMember(t)
	var teamA, teamB string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), projA).Scan(&teamA)
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), projB).Scan(&teamB)
	maint := l.memberWith(t, admin, "mona", "maintainer", teamA)
	other := l.memberWith(t, admin, "omar", "maintainer", teamB)
	if st, _, _ := l.json(t, admin, "POST", "/projects", `{"name":"billing","teamId":"`+teamA+`"}`, nil); st != 201 {
		t.Fatalf("second project = %d", st)
	}

	if st, _, _ := l.json(t, alice, "PATCH", "/projects/"+projA, `{"name":"orders"}`, nil); st != 403 {
		t.Errorf("developer rename = %d", st)
	}
	if st, _, _ := l.json(t, other, "PATCH", "/projects/"+projA, `{"name":"orders"}`, nil); st != 404 {
		t.Errorf("other team rename = %d", st)
	}
	for what, payload := range map[string]string{"upper case": `{"name":"Orders"}`, "move team": `{"teamId":"` + ids.New() + `"}`, "empty": `{}`} {
		if st, _, _ := l.json(t, maint, "PATCH", "/projects/"+projA, payload, nil); st != 400 {
			t.Errorf("%s = %d", what, st)
		}
	}
	if st, _, _ := l.json(t, maint, "PATCH", "/projects/"+projA, `{"name":"billing"}`, nil); st != 409 {
		t.Errorf("duplicate name = %d", st)
	}
	st, _, p := l.json(t, maint, "PATCH", "/projects/"+projA, `{"name":"orders"}`, nil)
	if st != 200 || p["name"] != "orders" || p["teamId"] != teamA {
		t.Fatalf("rename = %d %v", st, p)
	}
	var reason string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT reason FROM audit_events WHERE action = 'project.update' AND resource_id = ?`), projA).Scan(&reason)
	if reason != "renamed app to orders" {
		t.Errorf("audit reason = %q", reason)
	}
	if st, _, p := l.json(t, admin, "PATCH", "/projects/"+projA, `{"publicationMode":"automatic"}`, nil); st != 200 || p["name"] != "orders" || p["publicationMode"] != "automatic" {
		t.Errorf("mode change = %d %v", st, p)
	}
}

// TestKubeConversionRenameDelete: a conversion is renamed by anyone who may edit it and deleted by its creator or a
// maintainer; a delete is refused while a run is in progress and removes runs, revisions and files but not the audit.
func TestKubeConversionRenameDelete(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	var teamA string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), projA).Scan(&teamA)
	carol := l.memberWith(t, admin, "carol", "developer", teamA)
	sum := "sha256:" + strings.Repeat("ab", 32)
	if st, _, b := l.json(t, admin, "PUT", "/providers/kompose", `{"enabled":true,"version":"v1.38.0","artifacts":{"binary-amd64":"`+sum+`","binary-arm64":"`+sum+`"},"pools":[],"projects":[],"egressPresets":[],"timeoutSeconds":120}`, nil); st != 200 {
		t.Fatalf("enable kompose = %d %v", st, b)
	}
	snap := l.snapshotOf(t, alice, projA, map[string]string{"compose.yml": "services:\n  web:\n    image: nginx:1.27\n"})
	create := `{"name":"shop","provider":"kompose","input":{"kind":"compose","snapshotId":"` + snap + `","composePath":"compose.yml"},"options":{"format":"manifests"}}`
	st, _, c := l.json(t, alice, "POST", "/projects/"+projA+"/kube-conversions", create, map[string]string{"Idempotency-Key": "kube-" + ids.New()})
	if st != 202 {
		t.Fatalf("create = %d %v", st, c)
	}
	id := c["id"].(string)
	path := "/kube-conversions/" + id

	if st, _, _ := l.json(t, alice, "DELETE", path, "", nil); st != 409 {
		t.Fatalf("delete while queued = %d", st)
	}
	_, _ = l.db.SQL.Exec(l.db.Rebind(`UPDATE kube_conversion_runs SET state = 'failed' WHERE conversion_id = ?`), id)
	if st, _, _ := l.json(t, bob, "PATCH", path, `{"name":"store"}`, nil); st != 404 {
		t.Errorf("other team rename = %d", st)
	}
	if st, _, _ := l.json(t, carol, "PATCH", path, `{"name":"Store"}`, nil); st != 400 {
		t.Errorf("invalid name = %d", st)
	}
	if st, _, b := l.json(t, carol, "PATCH", path, `{"name":"store"}`, nil); st != 200 || b["name"] != "store" {
		t.Fatalf("rename = %d %v", st, b)
	}
	if st, _, _ := l.json(t, bob, "DELETE", path, "", nil); st != 404 {
		t.Errorf("other team delete = %d", st)
	}
	if st, _, _ := l.json(t, carol, "DELETE", path, "", nil); st != 403 {
		t.Errorf("a developer deleted another developer's conversion: %d", st)
	}
	if st, _, _ := l.json(t, alice, "DELETE", path, "", nil); st != 204 {
		t.Fatalf("creator delete = %d", st)
	}
	if st, _, _ := l.json(t, alice, "GET", path, "", nil); st != 404 {
		t.Errorf("get after delete = %d", st)
	}
	var left int
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT (SELECT COUNT(*) FROM kube_conversion_runs WHERE conversion_id = ?) + (SELECT COUNT(*) FROM kube_conversion_files WHERE conversion_id = ?)`), id, id).Scan(&left)
	if left != 0 || auditCount(t, l, "kube-conversion.update", id) != 1 || auditCount(t, l, "kube-conversion.delete", id) != 1 || auditCount(t, l, "kube-conversion.create", id) != 1 {
		t.Errorf("rows left = %d, or the audit trail is incomplete", left)
	}
}

// TestPipelineEditAPI: pipeline edits and deletes are administrator-only and an edit never returns the token.
func TestPipelineEditAPI(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	pid := ids.New()
	if st, _, _ := l.json(t, alice, "PATCH", "/pipelines/"+pid, `{"name":"x"}`, nil); st != 403 {
		t.Errorf("developer edit = %d", st)
	}
	if st, _, _ := l.json(t, alice, "DELETE", "/pipelines/"+pid, "", nil); st != 403 {
		t.Errorf("developer delete = %d", st)
	}
	admin := l.adminMember(t)
	if st, _, _ := l.json(t, admin, "PATCH", "/pipelines/"+pid, `{"token":"t"}`, nil); st != 404 {
		t.Errorf("edit of an unknown pipeline = %d", st)
	}
	if st, _, _ := l.json(t, admin, "PATCH", "/pipelines/"+pid, `{"projectId":"`+projA+`"}`, nil); st != 400 {
		t.Errorf("moving a pipeline = %d", st)
	}
}

// TestWithdrawRequests: a requester, or a maintainer of the project, withdraws a pending request; others are refused,
// decided requests stay immutable, and a withdrawn base-image request no longer blocks a new one (ADR 0062).
func TestWithdrawRequests(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	var teamA string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), projA).Scan(&teamA)
	carol := l.memberWith(t, admin, "carol", "developer", teamA)
	maint := l.memberWith(t, admin, "mona", "maintainer", teamA)
	withdraw := func(m member, approval string) (int, body) {
		st, _, b := l.json(t, m, "POST", "/approvals/"+approval+"/withdrawal", `{"reason":"no longer needed"}`, nil)
		return st, b
	}

	_, _, req := l.json(t, alice, "POST", "/projects/"+projA+"/repository-requests", `{"repository":"payments/orders-api","justification":"the orders service needs its own repository"}`, nil)
	approval := req["approvalId"].(string)
	if req["canWithdraw"] != true {
		t.Errorf("the requester is not offered a withdrawal: %v", req)
	}
	_, _, seen := l.json(t, carol, "GET", "/projects/"+projA+"/repository-requests", "", nil)
	if seen["items"].([]any)[0].(map[string]any)["canWithdraw"] != false {
		t.Error("another developer is offered a withdrawal")
	}
	if st, _ := withdraw(bob, approval); st != 404 {
		t.Errorf("other team withdraw = %d", st)
	}
	if st, _ := withdraw(carol, approval); st != 403 {
		t.Errorf("another developer withdrew the request: %d", st)
	}
	if st, _, b := l.json(t, alice, "POST", "/approvals/"+approval+"/withdrawal", `{"reason":"`+strings.Repeat("x", 2049)+`"}`, nil); st != 400 && st != 422 {
		t.Errorf("long reason = %d %v", st, b)
	}
	st, ap := withdraw(alice, approval)
	if st != 200 || ap["state"] != "withdrawn" || ap["canWithdraw"] != false {
		t.Fatalf("withdraw = %d %v", st, ap)
	}
	if st, _ := withdraw(alice, approval); st != 409 {
		t.Errorf("second withdraw = %d", st)
	}
	if st, _, _ := l.json(t, admin, "POST", "/approvals/"+approval+"/decision", `{"decision":"approve","reason":"approving after the requester withdrew it"}`, map[string]string{"Idempotency-Key": "decide-0000000000009"}); st != 409 {
		t.Errorf("decision after withdraw = %d", st)
	}
	_, _, list := l.json(t, alice, "GET", "/projects/"+projA+"/repository-requests", "", nil)
	if got := list["items"].([]any)[0].(map[string]any); got["state"] != "withdrawn" {
		t.Errorf("listed request = %v", got)
	}
	if auditCount(t, l, "approval.withdraw", req["id"].(string)) != 1 {
		t.Error("the withdrawal is not audited")
	}

	_, _, rejected := l.json(t, alice, "POST", "/projects/"+projA+"/repository-requests", `{"repository":"payments/other","justification":"a request an administrator rejects"}`, nil)
	if st, _, _ := l.json(t, admin, "POST", "/approvals/"+rejected["approvalId"].(string)+"/decision", `{"decision":"reject","reason":"use the shared repository"}`, map[string]string{"Idempotency-Key": "decide-0000000000010"}); st != 200 {
		t.Fatalf("reject = %d", st)
	}
	if st, _ := withdraw(alice, rejected["approvalId"].(string)); st != 409 {
		t.Errorf("withdraw of a decided request = %d", st)
	}

	st, _, bind := l.json(t, alice, "POST", "/projects/"+projA+"/secret-requests", `{"logicalId":"npm-token","justification":"needed for private packages"}`, map[string]string{"Idempotency-Key": "secreq-000000000001"})
	if st != 202 {
		t.Fatalf("secret request = %d %v", st, bind)
	}
	if st, b := withdraw(maint, bind["id"].(string)); st != 200 || b["state"] != "withdrawn" {
		t.Errorf("maintainer withdraw = %d %v", st, b)
	}

	const image = `{"repository":"mcr.microsoft.com/mssql/server","justification":"the database integration tests need the vendor server image"}`
	_, _, bi := l.json(t, alice, "POST", "/projects/"+projA+"/base-image-requests", image, nil)
	if st, _, _ := l.json(t, alice, "POST", "/projects/"+projA+"/base-image-requests", image, nil); st != 409 {
		t.Fatalf("second pending base-image request = %d", st)
	}
	if st, _ := withdraw(alice, bi["approvalId"].(string)); st != 200 {
		t.Fatalf("base-image withdraw = %d", st)
	}
	if st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/base-image-requests", image, nil); st != 202 {
		t.Errorf("a new request after the withdrawal = %d %v", st, b)
	}
}
