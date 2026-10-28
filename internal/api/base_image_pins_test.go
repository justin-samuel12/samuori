package api

import (
	"fmt"
	"strings"
	"testing"
)

func dockerfileSpec(name, path, extra string) string {
	return `{"apiVersion":"samuori.invalid/v1alpha1","kind":"ImageBuild","metadata":{"name":"` + name + `"},
"spec":{"build":{"method":"dockerfile","dockerfile":{"context":".","path":"` + path + `"` + extra + `}}}}`
}

// TestBaseImageTagPinning: with pinTags, admission pins each FROM tag to the digest it resolves to and leaves stages,
// scratch and names a named context replaces alone; an unresolvable tag blocks. With pinTags off, tags are refused
// and the reason points to the setting (ADR 0049).
func TestBaseImageTagPinning(t *testing.T) {
	l := newLiveWith(t, false, smallChunks(), nil)
	alice, _, projA, _ := twoTeams(t, l)
	admin := l.adminMember(t)
	snap := l.snapshotOf(t, alice, projA, map[string]string{
		"Dockerfile":          "FROM node:18-alpine AS builder\nRUN true\nFROM builder AS mid\nFROM scratch AS final\nCOPY --from=builder /etc/hostname /x\n",
		"shadowed.Dockerfile": "FROM node\n",
		"missing.Dockerfile":  "FROM node:missing\n",
		"vendor/readme":       "x",
	})
	specs := map[string]string{}
	check := func(name, path, extra string) body {
		t.Helper()
		if specs[name] == "" {
			st, _, created := l.json(t, alice, "POST", "/projects/"+projA+"/specs", doc(dockerfileSpec(name, path, extra)), nil)
			if st != 201 {
				t.Fatalf("create spec %s = %d %v", name, st, created)
			}
			specs[name] = created["specId"].(string)
		}
		_, _, rep := l.json(t, alice, "POST", "/specs/"+specs[name]+"/revisions/1/admission-checks", `{"snapshotId":"`+snap+`"}`, nil)
		return rep
	}

	rep := check("pinned", "Dockerfile", "")
	pins, _ := rep["baseImagePins"].([]any)
	if len(pins) != 1 || fmt.Sprint(pins[0]) != fmt.Sprint(map[string]any{"ref": "node:18-alpine", "image": "docker.io/library/node@" + mustResolve(t, "node:18-alpine")}) {
		t.Fatalf("pins = %v; want node:18-alpine only, not the builder stage or scratch", rep["baseImagePins"])
	}
	if !strings.Contains(fmt.Sprint(rep["policyDecisions"]), "Pinned at admission: node:18-alpine") {
		t.Errorf("decisions = %v", rep["policyDecisions"])
	}

	rep = check("shadowed", "shadowed.Dockerfile", `,"namedContexts":{"node":{"path":"vendor"}}`)
	if rep["baseImagePins"] != nil || !strings.Contains(fmt.Sprint(rep["policyDecisions"]), "named build context or stage called node would replace it") {
		t.Errorf("a FROM a named context replaces was pinned: %v", rep)
	}
	rep = check("missing", "missing.Dockerfile", "")
	if rep["admissible"] != false || !strings.Contains(fmt.Sprint(rep["policyDecisions"]), "could not resolve the tag") {
		t.Errorf("an unresolvable tag was not blocked: %v", rep)
	}

	_, _, pols := l.json(t, admin, "GET", "/policies", "", nil)
	var id string
	for _, p := range pols["items"].([]any) {
		if m := p.(map[string]any); m["kind"] == "base-images" {
			id = m["id"].(string)
			if d := m["document"].(map[string]any); d["pinTags"] != true || d["requireDigest"] != true {
				t.Fatalf("default base-images policy = %v", d)
			}
		}
	}
	repos := `["docker.io/library/node"]`
	if st, _, _ := l.json(t, admin, "PUT", "/policies/"+id, `{"kind":"base-images","document":{"allowedRepositories":`+repos+`,"requireDigest":false,"pinTags":true}}`, map[string]string{"If-Match": `"1"`}); st != 422 {
		t.Errorf("pinTags without requireDigest = %d", st)
	}
	strict := `{"kind":"base-images","document":{"allowedRepositories":` + repos + `,"requireDigest":true,"pinTags":false}}`
	_, _, impact := l.json(t, admin, "POST", "/policies/"+id+"/dry-run", strict, nil)
	token, _ := impact["confirmation"].(string)
	if token == "" {
		t.Fatalf("turning pinning off blocks the pinned spec, so it needs confirmation: %v", impact)
	}
	withReason := strings.Replace(strict, `"kind":"base-images",`, `"kind":"base-images","reason":"only digest references from now on",`, 1)
	if st, _, b := l.json(t, admin, "PUT", "/policies/"+id, withReason, map[string]string{"If-Match": `"1"`, "X-Samuori-Confirm": token}); st != 200 {
		t.Fatalf("strict policy = %d %v", st, b)
	}
	rep = check("pinned", "Dockerfile", "")
	reason := fmt.Sprint(rep["policyDecisions"])
	if rep["baseImagePins"] != nil || !strings.Contains(reason, "node:18-alpine (it is not pinned by digest)") || !strings.Contains(reason, "pin tags to their current digest at admission") {
		t.Errorf("strict policy report = %v", rep)
	}
}

func mustResolve(t *testing.T, ref string) string {
	t.Helper()
	d, err := testTags{}.ResolveTag(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
