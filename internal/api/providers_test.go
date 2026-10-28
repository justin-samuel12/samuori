package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProvidersAndMethodPolicy: providers start disabled, enabling one needs a pinned version, and admission refuses
// a build method the policy does not allow; a Railpack build is refused outside the provider's scope, its images go
// through the base-image policy and developers see availability but no pins (SEC-RPK-06, ADR 0047).
func TestProvidersAndMethodPolicy(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)

	st, _, b := l.json(t, admin, "GET", "/providers", "", nil)
	if st != 200 || len(b["items"].([]any)) != 4 {
		t.Fatalf("providers = %d %v", st, b)
	}
	for _, x := range b["items"].([]any) {
		if p := x.(map[string]any); p["config"].(map[string]any)["enabled"] != false {
			t.Errorf("%v is enabled by default", p["name"])
		}
	}
	if got := strings.Join(toStrings(b["policy"].(map[string]any)["allowed"]), ","); got != "recipe,dockerfile" {
		t.Errorf("default allowed methods = %s", got)
	}

	projects := "[]"
	cfg := func(enabled bool, version, frontend string) string {
		return `{"enabled":` + map[bool]string{true: "true", false: "false"}[enabled] + `,"version":"` + version + `","artifacts":{"binary-amd64":"sha256:` + strings.Repeat("a", 64) + `","binary-arm64":"sha256:` + strings.Repeat("b", 64) + `","frontend":"` + frontend + `","builder":"` + railpackBuilder + `","runtime":"` + railpackRuntime + `"},"pools":[],"projects":` + projects + `,"egressPresets":["npm"],"timeoutSeconds":0}`
	}
	if st, _, b := l.json(t, admin, "PUT", "/providers/railpack", cfg(true, "", "ghcr.io/railwayapp/railpack-frontend@sha256:"+strings.Repeat("c", 64)), nil); st != 422 {
		t.Errorf("enabling without a version = %d %v", st, b)
	}
	if st, _, b := l.json(t, admin, "PUT", "/providers/railpack", cfg(true, "v0.43.0", "ghcr.io/railwayapp/railpack-frontend:v0.43.0"), nil); st != 422 {
		t.Errorf("a frontend not pinned by digest = %d %v", st, b)
	}
	if st, _, b := l.json(t, admin, "PUT", "/providers/railpack", cfg(true, "v0.43.0", "ghcr.io/railwayapp/railpack-frontend@sha256:"+strings.Repeat("c", 64)), nil); st != 200 {
		t.Fatalf("enable railpack = %d %v", st, b)
	}
	if st, _, _ := l.json(t, alice, "PUT", "/providers/railpack", cfg(false, "", ""), nil); st != 403 {
		t.Errorf("developer configure = %d, want 403", st)
	}
	if st, _, b := l.json(t, admin, "PUT", "/build-method-policy", `{"allowed":["recipe"],"default":"dockerfile"}`, nil); st != 422 {
		t.Errorf("a default that is not allowed = %d %v", st, b)
	}

	snap := l.snapshotOf(t, alice, projA, map[string]string{"package.json": `{"name":"api","scripts":{"start":"node server.js"}}`, "server.js": "x"})
	_, _, rec := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(nodeSpec), nil)
	check := func() string {
		_, _, rep := l.json(t, alice, "POST", "/specs/"+rec["specId"].(string)+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		return codes(rep, "semanticErrors")
	}
	if strings.Contains(check(), "build-method-not-allowed") {
		t.Fatal("a recipe build is refused under the default policy")
	}
	if st, _, b := l.json(t, admin, "PUT", "/build-method-policy", `{"allowed":["dockerfile","railpack"],"default":"railpack"}`, nil); st != 200 {
		t.Fatalf("set policy = %d %v", st, b)
	}
	if !strings.Contains(check(), "build-method-not-allowed") {
		t.Error("a recipe build is admitted although the policy no longer allows recipes")
	}
	var n int
	_ = l.db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('provider.configure', 'build-method-policy.set')`).Scan(&n)
	if n != 2 {
		t.Errorf("audited provider changes = %d, want 2", n)
	}

	_, _, rp := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(railpackSpec), nil)
	railpackCheck := func() body {
		_, _, rep := l.json(t, alice, "POST", "/specs/"+rp["specId"].(string)+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		return rep
	}
	rep := railpackCheck()
	if c := codes(rep, "semanticErrors"); c != "" {
		t.Errorf("an enabled Railpack is refused: %v", rep["semanticErrors"])
	}
	if !strings.Contains(codes(rep, "policyDecisions"), "block") || rep["admissible"] != false {
		t.Errorf("Railpack's builder and runtime images bypass the base-image policy: %v", rep["policyDecisions"])
	}
	if !strings.Contains(codes(rep, "platformIssues"), "no-verified-pool") {
		t.Errorf("a Railpack build is admitted without a verified worker: %v", rep["platformIssues"])
	}
	methods := func() (string, body) {
		st, _, b := l.json(t, alice, "GET", "/projects/"+projA+"/build-methods", "", nil)
		if st != 200 {
			t.Fatalf("build methods = %d %v", st, b)
		}
		raw, _ := json.Marshal(b)
		return string(raw), b
	}
	raw, b := methods()
	if b["default"] != "railpack" || !strings.Contains(raw, `"available":true,"method":"railpack"`) || !strings.Contains(raw, `"available":false,"method":"buildpacks"`) {
		t.Errorf("build methods = %s", raw)
	}
	if strings.Contains(raw, "sha256:") || strings.Contains(raw, "pools") || strings.Contains(raw, "v0.43.0") {
		t.Errorf("a developer sees provider configuration: %s", raw)
	}
	if st, _, _ := l.json(t, alice, "GET", "/providers", "", nil); st != 403 {
		t.Errorf("developer list providers = %d, want 403", st)
	}

	projects = `["01ARZ3NDEKTSV4RRFFQ69G5FAV"]`
	if st, _, b := l.json(t, admin, "PUT", "/providers/railpack", cfg(true, "v0.43.0", "ghcr.io/railwayapp/railpack-frontend@sha256:"+strings.Repeat("c", 64)), nil); st != 200 {
		t.Fatalf("restrict railpack = %d %v", st, b)
	}
	if !strings.Contains(codes(railpackCheck(), "semanticErrors"), "build-method-not-allowed") {
		t.Error("a Railpack build is admitted for a project the provider is not enabled for")
	}
	if raw, _ := methods(); !strings.Contains(raw, `"available":false,"method":"railpack","reason":"Railpack is not enabled for this project`) {
		t.Errorf("build methods for a restricted project = %s", raw)
	}
}

const (
	railpackBuilder = "ghcr.io/railwayapp/railpack-builder:mise-2026.10.14@sha256:03df9e9aa1e786c2e0dd12c5409acad3bbd5a0fa8a75c1117c7ddab5c0c3fde4"
	railpackRuntime = "ghcr.io/railwayapp/railpack-runtime:mise-2026.10.14@sha256:02820533713338569ae4d4f0df986c3e7cc6e93457fd06fbb0d2cfa86fba9cdb"
	railpackSpec    = `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"api"},"spec":{"application":{"type":"http-service","port":3000},"build":{"method":"railpack"},"target":{"platforms":["linux/arm64"]}}}`
)

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
