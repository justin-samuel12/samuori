package api

import (
	"fmt"
	"strings"
	"testing"
)

// TestShippedRuntimeStatusAPI: only organization administrators deprecate, withdraw and approve again a shipped
// runtime; developers stop seeing a withdrawn one, admission refuses it by name with the reason and resolves ranges to
// the next runtime, a deprecated one builds with a warning, and approving again restores it (ADR 0060).
func TestShippedRuntimeStatusAPI(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	var teamA string
	_ = l.db.SQL.QueryRow(l.db.Rebind(`SELECT team_id FROM projects WHERE id = ?`), projA).Scan(&teamA)
	carol := l.memberWith(t, admin, "carol", "maintainer", teamA)
	set := func(m member, label, status string) int {
		st, _, _ := l.json(t, m, "PUT", "/recipes/node-service/versions/1.0.0/shipped-runtimes/"+label+"/status", fmt.Sprintf(`{"status":%q,"reason":"Node.js 22 reaches end of life"}`, status), nil)
		return st
	}
	listed := func(m member) map[string]map[string]any {
		_, _, list := l.json(t, m, "GET", "/recipes", "", nil)
		out := map[string]map[string]any{}
		for _, it := range list["items"].([]any) {
			if r := it.(map[string]any); r["id"] == "node-service" {
				for _, rt := range r["runtimes"].([]any) {
					out[rt.(map[string]any)["version"].(string)] = rt.(map[string]any)
				}
			}
		}
		return out
	}

	for name, m := range map[string]member{"developer": alice, "maintainer": carol} {
		if st := set(m, "22", "withdrawn"); st != 403 {
			t.Fatalf("a %s withdrew a shipped runtime: %d", name, st)
		}
	}
	if st := set(admin, "21", "withdrawn"); st != 404 {
		t.Fatalf("an unshipped label = %d", st)
	}
	if st := set(admin, "22", "withdrawn"); st != 204 {
		t.Fatalf("withdraw = %d", st)
	}
	if _, ok := listed(alice)["22"]; ok {
		t.Fatal("a developer still sees the withdrawn runtime")
	}
	if rt := listed(admin)["22"]; rt["status"] != "withdrawn" || rt["statusBy"] == nil || rt["statusReason"] != "Node.js 22 reaches end of life" {
		t.Fatalf("administrators see %v", rt)
	}

	snap := l.snapshotOf(t, alice, projA, map[string]string{"package.json": `{"name":"api","scripts":{"start":"node server.js"}}`, "server.js": "x"})
	specs := 0
	admission := func(constraint string) body {
		specs++
		spec := strings.Replace(strings.Replace(nodeSpec, `">=22"`, `"`+constraint+`"`, 1), "orders-api", fmt.Sprint("orders-api-", specs), 1)
		st, _, created := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(spec), nil)
		if st != 201 {
			t.Fatalf("create spec = %d %v", st, created)
		}
		_, _, rep := l.json(t, alice, "POST", "/specs/"+created["specId"].(string)+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		return rep
	}
	if rep := admission("22"); rep["admissible"] != false || !strings.Contains(fmt.Sprint(rep["semanticErrors"]), "runtime 22 of Node.js application has been withdrawn by an administrator: Node.js 22 reaches end of life") {
		t.Fatalf("admission of a withdrawn runtime = %v", rep)
	}
	if rep := admission(">=22"); codes(rep, "semanticErrors") != "" {
		t.Fatalf(">=22 with 22 withdrawn = %v", rep)
	}

	if st := set(admin, "24", "deprecated"); st != 204 {
		t.Fatalf("deprecate = %d", st)
	}
	if rt := listed(alice)["24"]; rt["status"] != "deprecated" || rt["statusBy"] != nil {
		t.Fatalf("a developer sees %v", rt)
	}
	if rep := admission("24"); codes(rep, "semanticErrors") != "" || !strings.Contains(codes(rep, "warnings"), "runtime-deprecated") {
		t.Fatalf("admission of a deprecated runtime = %v", rep)
	}

	if st := set(admin, "22", "approved"); st != 204 {
		t.Fatalf("approve again = %d", st)
	}
	if rt := listed(alice)["22"]; rt["status"] != "approved" {
		t.Fatalf("approving again did not restore 22: %v", rt)
	}
	if rep := admission("22"); codes(rep, "semanticErrors") != "" {
		t.Fatalf("admission after approving again = %v", rep)
	}
}

// TestOrganizationRuntimesAPI: only organization administrators add and decide runtimes; images must be pinned
// and allowed; developers see approved runtimes only, and admission refuses draft and withdrawn ones (ADR 0051).
func TestOrganizationRuntimesAPI(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	const base = "/recipes/node-service/versions/1.0.0/runtimes"
	node25 := "docker.io/library/node:25-bookworm-slim@sha256:" + strings.Repeat("b", 64)
	add := func(m member, label, build, run string) (int, body) {
		st, _, b := l.json(t, m, "POST", base, fmt.Sprintf(`{"label":%q,"buildImage":%q,"runImage":%q,"note":"Node 25 for early adopters"}`, label, build, run), nil)
		return st, b
	}

	if st, _ := add(alice, "25", node25, node25); st != 403 {
		t.Fatalf("a developer added a runtime: %d", st)
	}
	for name, c := range map[string]struct {
		label, image string
		status       int
		says         string
	}{
		"unpinned":       {"25", "docker.io/library/node:25-bookworm-slim", 422, "pinned by digest"},
		"short name":     {"25", "node:25@sha256:" + strings.Repeat("b", 64), 422, "pinned by digest"},
		"not allowed":    {"25", "quay.io/example/node:25@sha256:" + strings.Repeat("b", 64), 422, "not on the approved list"},
		"bad label":      {"25-beta", node25, 422, "runtime label"},
		"shipped label":  {"24", node25, 409, "ships runtime 24"},
		"unknown recipe": {"25", node25, 0, ""},
	} {
		path := base
		if name == "unknown recipe" {
			path = "/recipes/no-such-recipe/versions/1.0.0/runtimes"
		}
		st, _, b := l.json(t, admin, "POST", path, fmt.Sprintf(`{"label":%q,"buildImage":%q,"runImage":%q}`, c.label, c.image, c.image), nil)
		if c.status == 0 {
			if st != 404 {
				t.Errorf("%s = %d", name, st)
			}
			continue
		}
		if st != c.status || !strings.Contains(fmt.Sprint(b["message"]), c.says) {
			t.Errorf("%s = %d %v", name, st, b)
		}
	}

	st, draft := add(admin, "25", node25, node25)
	if st != 201 || draft["status"] != "draft" {
		t.Fatalf("add = %d %v", st, draft)
	}
	id := draft["id"].(string)
	if st, b := add(admin, "25", node25, node25); st != 409 {
		t.Fatalf("a duplicate label = %d %v", st, b)
	}
	offered := func(m member) bool {
		_, _, list := l.json(t, m, "GET", "/recipes", "", nil)
		for _, it := range list["items"].([]any) {
			r := it.(map[string]any)
			if r["id"] != "node-service" {
				continue
			}
			for _, rt := range r["runtimes"].([]any) {
				if x := rt.(map[string]any); x["version"] == "25" {
					return x["source"] == "organization" && x["status"] == "approved" && x["id"] == id
				}
			}
		}
		return false
	}
	if offered(alice) || offered(admin) {
		t.Fatal("/recipes offers a draft runtime")
	}
	if _, _, rts := l.json(t, alice, "GET", base, "", nil); len(rts["organization"].([]any)) != 0 || len(rts["shipped"].([]any)) != 2 {
		t.Fatalf("a developer sees draft runtimes: %v", rts)
	}

	snap := l.snapshotOf(t, alice, projA, map[string]string{"package.json": `{"name":"api","scripts":{"start":"node server.js"}}`, "server.js": "x"})
	_, _, spec := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(strings.Replace(nodeSpec, `">=22"`, `"25"`, 1)), nil)
	admission := func() body {
		_, _, rep := l.json(t, alice, "POST", "/specs/"+spec["specId"].(string)+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		return rep
	}
	if rep := admission(); !strings.Contains(fmt.Sprint(rep["semanticErrors"]), "draft that an administrator has not approved") || rep["admissible"] != false {
		t.Fatalf("admission of a draft runtime = %v", rep)
	}

	decide := func(m member, action string) (int, body) {
		st, _, b := l.json(t, m, "POST", base+"/"+id+"/actions", `{"action":"`+action+`","reason":"reviewed the release notes"}`, nil)
		return st, b
	}
	if st, _ := decide(alice, "approve"); st != 403 {
		t.Fatalf("a developer approved: %d", st)
	}
	if st, b := decide(admin, "approve"); st != 200 || b["status"] != "approved" || b["approvedBy"] == nil {
		t.Fatalf("approve = %d %v", st, b)
	}
	if !offered(alice) {
		t.Fatal("/recipes does not offer the approved runtime to developers")
	}
	if rep := admission(); codes(rep, "semanticErrors") != "" || codes(rep, "policyDecisions") != "allow,allow" {
		t.Fatalf("admission of an approved runtime = %v", rep)
	}

	if st, b := decide(admin, "withdraw"); st != 200 || b["status"] != "withdrawn" || b["withdrawnAt"] == nil {
		t.Fatalf("withdraw = %d %v", st, b)
	}
	if st, _ := decide(admin, "approve"); st != 422 {
		t.Fatalf("a withdrawn runtime was approved again: %d", st)
	}
	if offered(alice) {
		t.Fatal("/recipes offers a withdrawn runtime")
	}
	if rep := admission(); !strings.Contains(fmt.Sprint(rep["semanticErrors"]), "withdrawn by an administrator: reviewed the release notes") {
		t.Fatalf("admission of a withdrawn runtime = %v", rep)
	}
	if st, b := add(admin, "25", node25, node25); st != 201 {
		t.Fatalf("re-adding a withdrawn label = %d %v", st, b)
	}
	_, _, rts := l.json(t, admin, "GET", base, "", nil)
	if n := len(rts["organization"].([]any)); n != 2 {
		t.Fatalf("administrators see %d organization runtimes, want the withdrawn one kept as history and the new draft", n)
	}
	var audited int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action LIKE 'recipe.runtime.%'`).Scan(&audited)
	if audited != 4 {
		t.Errorf("audited runtime changes = %d, want 4", audited)
	}
}
