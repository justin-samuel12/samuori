package builds

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"samuori.invalid/samuori/internal/authz"
	"samuori.invalid/samuori/internal/buildspec"
	"samuori.invalid/samuori/internal/db"
	"samuori.invalid/samuori/internal/egress"
	"samuori.invalid/samuori/internal/engine/render"
	"samuori.invalid/samuori/internal/policy"
	"samuori.invalid/samuori/internal/recipes"
	"samuori.invalid/samuori/internal/sources"
)

// TestOrganizationRuntimePlan: an approved organization runtime is planned like a shipped one; the plan records its
// images by digest and its ID and source, and renders to the same Dockerfile apart from the images (ADR 0051).
func TestOrganizationRuntimePlan(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "samuori.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, `INSERT INTO organizations (id, name, created_at) VALUES ('o1', 'org', ?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	cat, err := recipes.LoadBuiltin()
	if err != nil {
		t.Fatal(err)
	}
	rec := recipes.NewStatuses(d, cat)
	rec.UseImagePolicy(func(context.Context, string, string) (string, error) { return "", nil })
	admin := authz.Principal{ID: "admin", OrgID: "o1", Kind: authz.User, Bindings: []authz.Binding{{Role: authz.Administrator, ScopeType: authz.ScopeOrganization, ScopeID: "o1"}}}
	build := "docker.io/library/node:25-bookworm@sha256:" + strings.Repeat("d", 64)
	run := "docker.io/library/node:25-bookworm-slim@sha256:" + strings.Repeat("e", 64)
	o, err := rec.CreateRuntime(ctx, admin, "node-service", "1.0.0", recipes.RuntimeInput{Label: "25", BuildImage: build, RunImage: run}, "")
	if err != nil {
		t.Fatal(err)
	}
	presets, err := egress.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	eff := policy.Effective{Egress: presets.DefaultPolicy()}
	plan := func(constraint string) (Plan, error) {
		doc := &buildspec.Document{Spec: buildspec.Spec{
			Application: &buildspec.Application{Type: "http-service", Port: 3000, Runtime: &buildspec.Runtime{Language: "nodejs", VersionConstraint: constraint}},
			Build:       buildspec.Build{Method: "recipe", Recipe: &buildspec.Recipe{ID: "node-service", Version: "1.0.0", Start: buildspec.Command{Argv: []string{"node", "server.js"}}}},
		}}
		return resolvePlan(ctx, doc, "sha256:spec", sources.Snapshot{Digest: "sha256:snap"}, sources.Manifest{}, rec, "o1", eff, nil)
	}
	if _, err := plan("25"); err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("a draft runtime was planned: %v", err)
	}
	if _, err := rec.DecideRuntime(ctx, admin, "node-service", "1.0.0", o.ID, "approve", "reviewed", ""); err != nil {
		t.Fatal(err)
	}
	org, err := plan("25")
	if err != nil {
		t.Fatal(err)
	}
	r := org.Recipe
	if r.Runtime != "25" || r.BuildImage != build || r.RunImage != run || r.RuntimeID != o.ID || r.RuntimeSource != recipes.SourceOrganization {
		t.Fatalf("plan recipe = %+v", r)
	}
	shipped, err := plan("24")
	if err != nil || shipped.Recipe.RuntimeID != "" || shipped.Recipe.RuntimeSource != "" {
		t.Fatalf("a shipped runtime's plan changed shape: %+v, %v", shipped.Recipe, err)
	}
	_, withID := org.Encode()
	org.Recipe.RuntimeID = "another"
	if _, other := org.Encode(); other == withID {
		t.Error("the plan digest does not cover the runtime ID")
	}
	org.Recipe.RuntimeID = o.ID

	got, err := render.Dockerfile(org)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("FROM "+build)) || !bytes.Contains(got, []byte("FROM "+run)) {
		t.Fatalf("the organization runtime's images are not the bases:\n%s", got)
	}
	swapped := shipped
	sr := *shipped.Recipe
	sr.BuildImage, sr.RunImage = build, run
	swapped.Recipe = &sr
	want, err := render.Dockerfile(swapped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the Dockerfile differs from a shipped runtime's with the same images:\n--- shipped\n%s\n--- organization\n%s", want, got)
	}
}
