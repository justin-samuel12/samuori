package api

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	cnbBuilder = "docker.io/paketobuildpacks/ubuntu-noble-builder:0.0.205@sha256:39dd864355d104e1f1b64a4238f6871bf0514ec3fc1427b13544c68452793dd9"
	cnbRun     = "docker.io/paketobuildpacks/ubuntu-noble-run:0.0.150@sha256:2b7308aea56815c1f376ab84cb1df5338797d480e32d899753e168cd367ab060"
	cnbSpec    = `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"api"},"spec":{"application":{"type":"http-service","port":3000},"build":{"method":"buildpacks","buildpacks":{"env":{"BP_NODE_VERSION":"22.*"}}},"target":{"platforms":["linux/arm64"]}}}`
)

// TestBuildpacksAdmission: the builder and run image must be pinned by digest to enable buildpacks; admission
// refuses buildpacks outside the provider's project scope, sends both images through the base-image policy, needs a
// verified BuildKit worker, and developers see availability but no pins (SEC-CNB-01, SEC-CNB-05, ADR 0047).
func TestBuildpacksAdmission(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)

	projects := "[]"
	cfg := func(builder, run string) string {
		return `{"enabled":true,"version":"v0.21.26","artifacts":{"builder":"` + builder + `","run-image":"` + run + `"},"pools":[],"projects":` + projects + `,"egressPresets":["nodejs-dist","npm"],"timeoutSeconds":0}`
	}
	if st, _, b := l.json(t, admin, "PUT", "/providers/buildpacks", cfg("docker.io/paketobuildpacks/ubuntu-noble-builder:latest", cnbRun), nil); st != 422 {
		t.Errorf("a builder pinned by tag only = %d %v", st, b)
	}
	if st, _, b := l.json(t, admin, "PUT", "/providers/buildpacks", `{"enabled":true,"version":"v0.21.26","artifacts":{"builder":"`+cnbBuilder+`"},"pools":[],"projects":[],"egressPresets":[],"timeoutSeconds":0}`, nil); st != 422 {
		t.Errorf("enabling without a run image = %d %v", st, b)
	}
	if st, _, b := l.json(t, admin, "PUT", "/providers/buildpacks", cfg(cnbBuilder, cnbRun), nil); st != 200 {
		t.Fatalf("enable buildpacks = %d %v", st, b)
	}
	snap := l.snapshotOf(t, alice, projA, map[string]string{"package.json": `{"name":"api","scripts":{"start":"node server.js"}}`, "server.js": "x"})
	_, _, sp := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(cnbSpec), nil)
	check := func() body {
		_, _, rep := l.json(t, alice, "POST", "/specs/"+sp["specId"].(string)+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		return rep
	}
	if !strings.Contains(codes(check(), "semanticErrors"), "build-method-not-allowed") {
		t.Error("a buildpacks build is admitted although the method policy does not allow it")
	}
	if st, _, b := l.json(t, admin, "PUT", "/build-method-policy", `{"allowed":["dockerfile","buildpacks"],"default":"buildpacks"}`, nil); st != 200 {
		t.Fatalf("set policy = %d %v", st, b)
	}
	rep := check()
	if c := codes(rep, "semanticErrors"); c != "" {
		t.Errorf("an enabled buildpacks provider is refused: %v", rep["semanticErrors"])
	}
	if !strings.Contains(codes(rep, "policyDecisions"), "block") || rep["admissible"] != false {
		t.Errorf("the builder and run image bypass the base-image policy: %v", rep["policyDecisions"])
	}
	if !strings.Contains(codes(rep, "platformIssues"), "no-verified-pool") {
		t.Errorf("a buildpacks build is admitted without a verified worker: %v", rep["platformIssues"])
	}
	st, _, b := l.json(t, alice, "GET", "/projects/"+projA+"/build-methods", "", nil)
	rawb, _ := json.Marshal(b)
	raw := string(rawb)
	if st != 200 || b["default"] != "buildpacks" || !strings.Contains(raw, `"available":true,"method":"buildpacks"`) {
		t.Errorf("build methods = %d %s", st, raw)
	}
	if strings.Contains(raw, "sha256:") || strings.Contains(raw, "paketobuildpacks") {
		t.Errorf("a developer sees provider pins: %s", raw)
	}

	projects = `["01ARZ3NDEKTSV4RRFFQ69G5FAV"]`
	if st, _, b := l.json(t, admin, "PUT", "/providers/buildpacks", cfg(cnbBuilder, cnbRun), nil); st != 200 {
		t.Fatalf("restrict buildpacks = %d %v", st, b)
	}
	if !strings.Contains(codes(check(), "semanticErrors"), "build-method-not-allowed") {
		t.Error("a buildpacks build is admitted for a project the provider is not enabled for")
	}
}
