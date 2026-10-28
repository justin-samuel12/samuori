package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/api/apigen"
	"samuori.invalid/samuori/internal/auth"
	"samuori.invalid/samuori/internal/buildplan"
)

// TestEgressAccessRequests: a developer requests egress that a readiness item or a build's denials show the project
// needs; an organization administrator other than the requester approves it, and it opens only for that project
// and build method until revoked. Entries without stored evidence, refused addresses and other projects' evidence
// are refused (ADR 0052).
func TestEgressAccessRequests(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, bob, projA, projB := twoTeams(t, l)
	admin := l.adminMember(t)
	var org string
	if err := l.db.SQL.QueryRow(l.db.Rebind(`SELECT org_id FROM projects WHERE id = ?`), projA).Scan(&org); err != nil {
		t.Fatal(err)
	}
	var u apigen.User
	_ = json.NewDecoder(l.call(t, admin.c, "POST", "/users", `{"username":"pat","password":"a long secret phrase"}`, admin.csrf).Body).Decode(&u)
	if r := l.call(t, admin.c, "POST", "/role-bindings", fmt.Sprintf(`{"principalId":%q,"role":"administrator","scopeType":"project","scopeId":%q}`, u.Id, projA), admin.csrf); r.StatusCode != 201 {
		t.Fatalf("bind pat = %d", r.StatusCode)
	}
	pat := member{l.client(), nil}
	for _, ck := range l.call(t, pat.c, "POST", "/auth/login", `{"username":"pat","password":"a long secret phrase"}`, nil).Cookies() {
		if ck.Name == auth.CSRFCookie {
			pat.csrf = map[string]string{auth.CSRFHeader: ck.Value}
		}
	}

	files := map[string]string{
		"Dockerfile":        "FROM node:18-alpine\nWORKDIR /app\nCOPY package*.json ./\nRUN npm ci\nCOPY . .\nEXPOSE 3000\nCMD [\"node\", \"server.js\"]\n",
		"package.json":      `{"name":"shop","scripts":{"start":"node server.js"},"dependencies":{"express":"^4.21.0","sharp":"^0.32.6"}}`,
		"package-lock.json": `{"lockfileVersion":3,"packages":{"":{},"node_modules/express":{"version":"4.21.2"},"node_modules/sharp":{"version":"0.32.6","hasInstallScript":true}}}`,
		"server.js":         "require('express')().listen(3000)\n",
	}
	spec := `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"shop"},
"spec":{"application":{"type":"http-service","port":3000},"build":{"method":"dockerfile","dockerfile":{"context":".","path":"Dockerfile"}}}}`
	project := func(m member, proj string) (snap, specID string) {
		snap = l.snapshotOf(t, m, proj, files)
		_, _, created := l.json(t, m, "POST", "/projects/"+proj+"/specs", doc(spec), nil)
		return snap, created["specId"].(string)
	}
	snapA, specA := project(alice, projA)
	snapB, specB := project(bob, projB)
	readiness := func(m member, specID, snap string) map[string]map[string]any {
		_, _, rep := l.json(t, m, "POST", "/specs/"+specID+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		return readinessCodes(rep["readiness"])
	}
	npm := readiness(alice, specA, snapA)["egress-package-manager"]
	if npm == nil || fmt.Sprint(npm["requestable"]) != "map[destinations:[] presets:[npm]]" || !strings.Contains(fmt.Sprint(npm["fix"]), "request access for this project") {
		t.Fatalf("npm readiness item = %v", npm)
	}
	sharp := readiness(alice, specA, snapA)["egress-downloader"]
	if sharp == nil || !strings.Contains(fmt.Sprint(sharp["requestable"]), "github.com:443") {
		t.Fatalf("sharp readiness item = %v", sharp)
	}
	evidence := func(item map[string]any, specID, snap string) string {
		line, _ := item["line"].(float64)
		path, _ := item["path"].(string)
		return fmt.Sprintf(`{"specId":%q,"specRevision":1,"snapshotId":%q,"code":%q,"path":%q,"line":%d}`, specID, snap, item["code"], path, int(line))
	}
	const why = `"justification":"npm ci needs the public registry to install dependencies"`
	request := func(m member, proj, method, presets, dests, ev string) (int, body) {
		st, _, b := l.json(t, m, "POST", "/projects/"+proj+"/egress-access-requests",
			fmt.Sprintf(`{"method":%q,"presets":%s,"destinations":%s,%s,"evidence":%s}`, method, presets, dests, why, ev), nil)
		return st, b
	}
	npmEv := evidence(npm, specA, snapA)

	for name, c := range map[string]struct{ presets, dests string }{
		"preset without evidence": {`["pypi"]`, `[]`},
		"loopback":                {`[]`, `["127.0.0.1:80"]`},
		"metadata":                {`[]`, `["169.254.169.254:80"]`},
		"private":                 {`[]`, `["10.0.0.5:443"]`},
		"internal name":           {`[]`, `["metadata.google.internal:80"]`},
		"destination not in item": {`[]`, `["github.com:443"]`},
	} {
		if st, b := request(alice, projA, "dockerfile", c.presets, c.dests, npmEv); st != 422 {
			t.Errorf("%s = %d %v", name, st, b)
		}
	}
	if st, b := request(alice, projA, "recipe", `["npm"]`, `[]`, npmEv); st != 422 || b["code"] != "not_evidenced" {
		t.Errorf("another method than the evidence = %d %v", st, b)
	}
	if st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/egress-access-requests", `{"method":"dockerfile","presets":["npm"],"destinations":[],"justification":"too short","evidence":`+npmEv+`}`, nil); st != 422 && st != 400 {
		t.Errorf("short justification = %d %v", st, b)
	}
	if st, b := request(bob, projA, "dockerfile", `["npm"]`, `[]`, npmEv); st != 404 {
		t.Errorf("another team requested for project A = %d %v", st, b)
	}
	if st, b := request(bob, projB, "dockerfile", `["npm"]`, `[]`, npmEv); st != 404 {
		t.Errorf("project A's evidence used for project B = %d %v", st, b)
	}

	st, req := request(alice, projA, "dockerfile", `["npm"]`, `[]`, npmEv)
	if st != 202 || req["state"] != "pending" {
		t.Fatalf("request = %d %v", st, req)
	}
	if st, b := request(alice, projA, "dockerfile", `["npm"]`, `[]`, npmEv); st != 409 || b["code"] != "request_pending" {
		t.Errorf("duplicate = %d %v", st, b)
	}
	ev := req["evidence"].(map[string]any)
	if ev["kind"] != "readiness" || ev["message"] != npm["message"] || ev["specId"] != specA {
		t.Errorf("stored evidence = %v", ev)
	}
	approval := req["approvalId"].(string)
	decide := func(m member, approval, decision, key string) (int, body) {
		st, _, b := l.json(t, m, "POST", "/approvals/"+approval+"/decision", `{"decision":"`+decision+`","reason":"the public npm registry is acceptable for this project"}`, map[string]string{"Idempotency-Key": key})
		return st, b
	}
	if st, b := decide(alice, approval, "approve", "egress-decide-00000001"); st != 403 {
		t.Fatalf("the requester approved: %d %v", st, b)
	}
	if st, b := decide(pat, approval, "approve", "egress-decide-00000002"); st != 403 && st != 404 {
		t.Fatalf("a project administrator approved: %d %v", st, b)
	}
	if readiness(alice, specA, snapA)["egress-package-manager"] == nil {
		t.Fatal("the request opened egress before approval")
	}
	if st, b := decide(admin, approval, "approve", "egress-decide-00000003"); st != 200 || b["state"] != "approved" {
		t.Fatalf("approve = %d %v", st, b)
	}
	if readiness(alice, specA, snapA)["egress-package-manager"] != nil {
		t.Error("project A still lacks npm after approval")
	}
	if readiness(bob, specB, snapB)["egress-package-manager"] == nil {
		t.Error("project B got project A's allowance")
	}
	_, _, list := l.json(t, alice, "GET", "/projects/"+projA+"/egress-allowances", "", nil)
	items := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("allowances = %v", list)
	}
	al := items[0].(map[string]any)
	if al["kind"] != "preset" || al["preset"] != "npm" || al["status"] != "active" || fmt.Sprint(al["methods"]) != "[dockerfile]" {
		t.Errorf("allowance = %v", al)
	}
	if st, _, _ := l.json(t, bob, "GET", "/projects/"+projA+"/egress-allowances", "", nil); st != 404 {
		t.Errorf("another team listed project A's allowances: %d", st)
	}
	if st, b := request(alice, projA, "dockerfile", `["npm"]`, `[]`, npmEv); st != 422 && st != 409 {
		t.Errorf("request for what is now allowed = %d %v", st, b)
	}

	sharpEv := evidence(sharp, specA, snapA)
	st, rej := request(alice, projA, "dockerfile", `[]`, `["github.com:443"]`, sharpEv)
	if st != 202 {
		t.Fatalf("sharp request = %d %v", st, rej)
	}
	if st, _ := decide(admin, rej["approvalId"].(string), "reject", "egress-decide-00000004"); st != 200 {
		t.Fatalf("reject = %d", st)
	}
	_, _, reqs := l.json(t, alice, "GET", "/projects/"+projA+"/egress-access-requests", "", nil)
	if got := reqs["items"].([]any)[0].(map[string]any); got["state"] != "rejected" || got["reason"] == nil {
		t.Errorf("rejected request = %v", got)
	}
	if _, _, list := l.json(t, alice, "GET", "/projects/"+projA+"/egress-allowances", "", nil); len(list["items"].([]any)) != 1 {
		t.Errorf("a rejected request created allowances: %v", list)
	}

	_, _, run := l.json(t, alice, "POST", "/builds", fmt.Sprintf(`{"specId":%q,"specRevision":1,"snapshotId":%q}`, specA, snapA), map[string]string{"Idempotency-Key": "egress-access-build-01"})
	runID := run["id"].(string)
	raw, digest := buildplan.Plan{Version: 1, Method: "dockerfile"}.Encode()
	now := time.Now().UTC()
	report := `{"denied":[{"host":"objects.githubusercontent.com","port":443,"count":2,"reason":"not on the build's egress allowlist"},{"host":"127.0.0.1","port":80,"count":1,"reason":"not on the build's egress allowlist"},{"host":"nexus.corp","port":8081,"count":1,"reason":"not on the build's egress allowlist"}]}`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO build_plans (digest, org_id, plan, created_at) VALUES (?, ?, ?, ?)`, []any{digest, org, string(raw), now}},
		{`UPDATE build_runs SET plan_digest = ? WHERE id = ?`, []any{digest, runID}},
		{`INSERT INTO build_attempts (id, run_id, number, generation, worker_id, pool_id, state, lease_deadline, cleanup, started_at, ended_at, failure_class, egress_report) VALUES ('01JACCESSATTEMPT0000000001', ?, 1, 1, '01JACCESSW0RKER00000000001', '01JACCESSP0000000000000001', 'failed', ?, 'destroyed', ?, ?, 'application', ?)`, []any{runID, now, now, now, report}},
	} {
		if _, err := l.db.SQL.Exec(l.db.Rebind(q.sql), q.args...); err != nil {
			t.Fatal(err)
		}
	}
	buildEv := `{"buildId":"` + runID + `"}`
	if st, b := request(alice, projA, "dockerfile", `[]`, `["127.0.0.1:80"]`, buildEv); st != 422 {
		t.Errorf("loopback from a build's denials = %d %v", st, b)
	}
	if st, b := request(bob, projB, "dockerfile", `[]`, `["objects.githubusercontent.com:443"]`, buildEv); st != 404 {
		t.Errorf("project A's build used as project B's evidence = %d %v", st, b)
	}
	st, fromBuild := request(alice, projA, "dockerfile", `[]`, `["objects.githubusercontent.com:443"]`, buildEv)
	if st != 202 || !strings.Contains(fmt.Sprint(fromBuild["evidence"]), "objects.githubusercontent.com:443") {
		t.Fatalf("build request = %d %v", st, fromBuild)
	}
	if st, _ := decide(admin, fromBuild["approvalId"].(string), "approve", "egress-decide-00000005"); st != 200 {
		t.Fatalf("approve build request = %d", st)
	}
	internalReq := func(dests string) (int, body) {
		st, _, b := l.json(t, alice, "POST", "/projects/"+projA+"/egress-access-requests",
			fmt.Sprintf(`{"method":"dockerfile","presets":[],"destinations":[],"internalDestinations":%s,%s,"evidence":%s}`, dests, why, buildEv), nil)
		return st, b
	}
	if st, b := request(alice, projA, "dockerfile", `[]`, `["nexus.corp:8081"]`, buildEv); st != 422 {
		t.Errorf("an internal name requested as public = %d %v", st, b)
	}
	for _, d := range []string{`["127.0.0.1:80"]`, `["169.254.169.254:80"]`, `["10.9.9.9:443"]`} {
		if st, b := internalReq(d); st != 422 {
			t.Errorf("internal %s = %d %v", d, st, b)
		}
	}
	st, internal := internalReq(`["nexus.corp:8081"]`)
	if st != 202 || fmt.Sprint(internal["internalDestinations"]) != "[nexus.corp:8081]" {
		t.Fatalf("internal request = %d %v", st, internal)
	}
	_, _, appr := l.json(t, admin, "GET", "/approvals?state=pending", "", nil)
	if !strings.Contains(fmt.Sprint(appr), "INTERNAL NETWORK nexus.corp:8081") {
		t.Errorf("the approval does not flag internal access: %v", appr)
	}
	if st, _ := decide(admin, internal["approvalId"].(string), "approve", "egress-decide-00000006"); st != 200 {
		t.Fatalf("approve internal request = %d", st)
	}
	_, _, allowed := l.json(t, alice, "GET", "/projects/"+projA+"/egress-allowances", "", nil)
	if !strings.Contains(fmt.Sprint(allowed), "destination:nexus.corp:8081") || !strings.Contains(fmt.Sprint(allowed), "network:internal") {
		t.Errorf("internal allowance = %v", allowed)
	}

	_, _, att := l.json(t, alice, "GET", "/builds/"+runID+"/attempts", "", nil)
	if !strings.Contains(fmt.Sprint(att), "now allowed for this project") {
		t.Errorf("the denial advice ignores the approved allowance: %v", att)
	}

	if st, _, _ := l.json(t, alice, "POST", "/egress-allowances/"+al["id"].(string)+"/revocation", `{"reason":"the project moved to a mirror"}`, nil); st != 403 && st != 404 {
		t.Errorf("a developer revoked an allowance: %d", st)
	}
	if st, _, b := l.json(t, admin, "POST", "/egress-allowances/"+al["id"].(string)+"/revocation", `{"reason":"the project moved to a mirror"}`, nil); st != 200 || b["status"] != "revoked" {
		t.Fatalf("revoke = %d %v", st, b)
	}
	if st, _, _ := l.json(t, admin, "POST", "/egress-allowances/"+al["id"].(string)+"/revocation", `{"reason":"the project moved to a mirror"}`, nil); st != 409 {
		t.Errorf("second revoke = %d", st)
	}
	if readiness(alice, specA, snapA)["egress-package-manager"] == nil {
		t.Error("a revoked allowance still opens npm")
	}
	var audited int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('egress.allowance.create', 'egress.allowance.revoke')`).Scan(&audited)
	if audited != 4 {
		t.Errorf("allowance audit events = %d", audited)
	}
}
