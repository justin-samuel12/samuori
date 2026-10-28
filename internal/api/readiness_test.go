package api

import (
	"fmt"
	"strings"
	"testing"
)

func readinessCodes(items any) map[string]map[string]any {
	out := map[string]map[string]any{}
	list, _ := items.([]any)
	for _, x := range list {
		m := x.(map[string]any)
		out[m["code"].(string)] = m
	}
	return out
}

// TestBuildReadiness: the admission dry run and the analysis page predict, from the upload alone, that npm ci and
// sharp 0.32's libvips download need network access Dockerfile builds do not get, that the spec's port is not the
// one the Dockerfile exposes, and that Node.js 18 is past end of life. Readiness never changes admissibility
// (ADR 0050).
func TestBuildReadiness(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	snap := l.snapshotOf(t, alice, projA, map[string]string{
		"Dockerfile":        "FROM node:18-alpine\nWORKDIR /app\nCOPY package*.json ./\nRUN npm ci --only=production=false\nCOPY . .\nEXPOSE 3000\nCMD [\"node\", \"server.js\"]\n",
		"package.json":      `{"name":"shop","scripts":{"start":"node server.js"},"dependencies":{"express":"^4.21.0","sharp":"^0.32.6"}}`,
		"package-lock.json": `{"lockfileVersion":3,"packages":{"":{},"node_modules/express":{"version":"4.21.2"},"node_modules/sharp":{"version":"0.32.6","hasInstallScript":true}}}`,
		"server.js":         "require('express')().listen(3000)\n",
	})
	spec := `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"shop"},
"spec":{"application":{"type":"http-service","port":8080},"build":{"method":"dockerfile","dockerfile":{"context":".","path":"Dockerfile"}}}}`
	st, _, created := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(spec), nil)
	if st != 201 {
		t.Fatalf("create spec = %d %v", st, created)
	}
	_, _, rep := l.json(t, alice, "POST", "/specs/"+created["specId"].(string)+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
	got := readinessCodes(rep["readiness"])
	for _, code := range []string{"egress-package-manager", "egress-downloader", "port-mismatch", "runtime-end-of-life", "npm-only-flag"} {
		if got[code] == nil {
			t.Errorf("admission readiness lacks %s: %v", code, rep["readiness"])
		}
	}
	if m := got["egress-downloader"]; m != nil && (!strings.Contains(fmt.Sprint(m["message"]), "github.com:443") || m["fixer"] != "developer" || m["severity"] != "will-fail") {
		t.Errorf("sharp item = %v", m)
	}
	for code, m := range got {
		if m["blocking"] == true {
			t.Errorf("%s is marked blocking, but no admission rule refuses it", code)
		}
	}
	if strings.Contains(fmt.Sprint(rep["semanticErrors"], rep["policyDecisions"]), "sharp") {
		t.Errorf("readiness leaked into admission's blocking reasons: %v", rep)
	}

	_, _, op := l.json(t, alice, "POST", "/snapshots/"+snap+"/discoveries", "", map[string]string{"Idempotency-Key": "readiness-disc-0001"})
	opID, _ := op["id"].(string)
	done := l.await(t, alice, opID)
	if done.ResourceId == nil {
		t.Fatalf("discovery = %+v", done)
	}
	_, _, disc := l.json(t, alice, "GET", "/discoveries/"+*done.ResourceId, "", nil)
	dfs, _ := disc["dockerfiles"].([]any)
	if len(dfs) != 1 {
		t.Fatalf("dockerfiles = %v", disc["dockerfiles"])
	}
	df := readinessCodes(dfs[0].(map[string]any)["readiness"])
	if df["egress-package-manager"] == nil || df["egress-downloader"] == nil {
		t.Errorf("analysis page readiness = %v", dfs[0].(map[string]any)["readiness"])
	}
}
