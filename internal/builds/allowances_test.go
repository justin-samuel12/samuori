package builds

import (
	"context"
	"slices"
	"testing"

	"samuori.invalid/samuori/internal/admission"
	"samuori.invalid/samuori/internal/buildplan"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/sources"
)

type projectAllowances map[string][]egress.Allowance

func (f projectAllowances) ProjectAllowances(_ context.Context, _, projectID string) ([]egress.Allowance, error) {
	return f[projectID], nil
}

// TestProjectAllowancesInPlan: a project's approved allowances reach its Dockerfile build plans, and so the plan
// digest, only for the methods they were approved for; another project's plan is unchanged (ADR 0052).
func TestProjectAllowancesInPlan(t *testing.T) {
	ctx := context.Background()
	cat, err := egress.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	org := cat.DefaultPolicy()
	if slices.Contains(org.DockerfilePresets, "npm") {
		t.Fatal("the default policy already gives Dockerfile builds npm")
	}
	allowances := projectAllowances{"pA": {
		{ID: "al1", Preset: "npm", Methods: []string{"dockerfile"}},
		{ID: "al2", Host: "github.com", Port: 443, Methods: []string{"dockerfile"}},
		{ID: "al3", Host: "objects.githubusercontent.com", Port: 443, Methods: []string{"recipe"}},
	}}
	doc := &buildspec.Document{Spec: buildspec.Spec{Build: buildspec.Build{Method: "dockerfile", Dockerfile: &buildspec.Dockerfile{Context: ".", Path: "Dockerfile"}}}}
	manifest := sources.Manifest{Files: []sources.File{{Path: "Dockerfile", Digest: "sha256:df"}}}
	plan := func(project string) (Plan, string) {
		eff := policy.Effective{Egress: org}
		if err := admission.ProjectEgress(ctx, allowances, &eff, "o1", project, "dockerfile"); err != nil {
			t.Fatal(err)
		}
		p, err := resolvePlan(ctx, doc, "sha256:spec", sources.Snapshot{Digest: "sha256:snap"}, manifest, nil, "o1", eff, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, digest := p.Encode()
		return p, digest
	}
	allows := func(p Plan, host string) bool {
		return slices.ContainsFunc(p.Egress.Allow, func(r buildplan.EgressRule) bool { return r.Host == host && r.Port == 443 && !r.Internal })
	}

	a, digestA := plan("pA")
	if !allows(a, "registry.npmjs.org") || !allows(a, "github.com") || !slices.Contains(a.Egress.Presets, "npm@1.0.0") {
		t.Errorf("project A's plan lacks its allowances: %+v", a.Egress)
	}
	if allows(a, "objects.githubusercontent.com") {
		t.Error("an allowance approved for recipe builds reached a Dockerfile build")
	}
	if !slices.Equal(a.Egress.Allowances, []string{"al1", "al2"}) {
		t.Errorf("plan allowances = %v", a.Egress.Allowances)
	}

	b, digestB := plan("pB")
	if allows(b, "registry.npmjs.org") || allows(b, "github.com") || len(b.Egress.Allowances) != 0 {
		t.Errorf("another project's plan got project A's allowances: %+v", b.Egress)
	}
	if digestA == digestB {
		t.Error("the allowances do not change the plan digest")
	}
	plain, err := resolvePlan(ctx, doc, "sha256:spec", sources.Snapshot{Digest: "sha256:snap"}, manifest, nil, "o1", policy.Effective{Egress: org}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, d := plain.Encode(); d != digestB {
		t.Error("a project without allowances no longer gets the plan it got before allowances existed")
	}

	delete(allowances, "pA")
	if again, _ := plan("pA"); allows(again, "registry.npmjs.org") || allows(again, "github.com") {
		t.Error("a revoked allowance still reaches new plans")
	}
}
